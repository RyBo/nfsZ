/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nfszv1alpha1 "github.com/rybo/nfsZ/api/v1alpha1"
)

const (
	finalizerName = "nfsz.dev/teardown"

	// AnnotationRetained marks a backing PVC orphaned by reclaimPolicy Retain.
	AnnotationRetained = "nfsz.dev/retained-from"
)

// SharedVolumeReconciler reconciles a SharedVolume object
type SharedVolumeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// OperatorNamespace hosts the per-volume NFS server stacks.
	OperatorNamespace string
	// GaneshaImage is the NFS server image to run.
	GaneshaImage string
	// AllowCIDRs are extra CIDRs admitted by generated NetworkPolicies, for
	// CNIs whose cross-node traffic arrives from tunnel IPs rather than
	// node IPs (e.g. Calico VXLAN).
	AllowCIDRs []string
}

// +kubebuilder:rbac:groups=nfsz.dev,resources=sharedvolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nfsz.dev,resources=sharedvolumes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nfsz.dev,resources=sharedvolumes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

func (r *SharedVolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	sv := &nfszv1alpha1.SharedVolume{}
	if err := r.Get(ctx, req.NamespacedName, sv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sv.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sv)
	}

	if controllerutil.AddFinalizer(sv, finalizerName) {
		if err := r.Update(ctx, sv); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Server stack first; PVs need the Service ClusterIP.
	clusterIP, serverReady, err := r.reconcileServer(ctx, sv)
	if err != nil {
		return ctrl.Result{}, err
	}

	targets, err := r.targetNamespaces(ctx, sv)
	if err != nil {
		return ctrl.Result{}, err
	}

	var bindings []nfszv1alpha1.NamespaceBinding
	if clusterIP != "" {
		bindings, err = r.reconcileBindings(ctx, sv, targets, clusterIP)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.cleanupStaleBindings(ctx, sv, targets); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.reconcileNetworkPolicy(ctx, sv, targets); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, sv, clusterIP, serverReady, bindings); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	log.V(1).Info("reconciled", "phase", sv.Status.Phase, "bound", sv.Status.BoundSummary)
	return ctrl.Result{}, nil
}

// reconcileServer ensures backing PVC, ganesha ConfigMap, Deployment and
// Service exist, returning the Service ClusterIP and server readiness.
func (r *SharedVolumeReconciler) reconcileServer(ctx context.Context, sv *nfszv1alpha1.SharedVolume) (string, bool, error) {
	pvc := BuildBackingPVC(sv, r.OperatorNamespace)
	if err := r.createIfMissing(ctx, sv, pvc); err != nil {
		return "", false, err
	}

	cm := BuildConfigMap(sv, r.OperatorNamespace)
	existingCM := &corev1.ConfigMap{}
	switch err := r.Get(ctx, client.ObjectKeyFromObject(cm), existingCM); {
	case apierrors.IsNotFound(err):
		if err := r.createOwned(ctx, sv, cm); err != nil {
			return "", false, err
		}
	case err != nil:
		return "", false, err
	default:
		if existingCM.Data["ganesha.conf"] != cm.Data["ganesha.conf"] {
			existingCM.Data = cm.Data
			if err := r.Update(ctx, existingCM); err != nil {
				return "", false, err
			}
		}
	}

	deploy := BuildServerDeployment(sv, r.OperatorNamespace, r.GaneshaImage, cm)
	existingDeploy := &appsv1.Deployment{}
	switch err := r.Get(ctx, client.ObjectKeyFromObject(deploy), existingDeploy); {
	case apierrors.IsNotFound(err):
		if err := r.createOwned(ctx, sv, deploy); err != nil {
			return "", false, err
		}
	case err != nil:
		return "", false, err
	default:
		// Roll the pod on image or conf change; leave everything else alone.
		cur := existingDeploy.Spec.Template
		want := deploy.Spec.Template
		if cur.Spec.Containers[0].Image != want.Spec.Containers[0].Image ||
			cur.Annotations[confHashAnnotation] != want.Annotations[confHashAnnotation] {
			existingDeploy.Spec.Template = want
			if err := r.Update(ctx, existingDeploy); err != nil {
				return "", false, err
			}
		}
		deploy = existingDeploy
	}
	serverReady := deploy.Status.ReadyReplicas > 0

	svc := BuildService(sv, r.OperatorNamespace)
	existingSvc := &corev1.Service{}
	switch err := r.Get(ctx, client.ObjectKeyFromObject(svc), existingSvc); {
	case apierrors.IsNotFound(err):
		if err := r.createOwned(ctx, sv, svc); err != nil {
			return "", false, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(svc), existingSvc); err != nil {
			return "", false, err
		}
	case err != nil:
		return "", false, err
	}
	// The ClusterIP is stamped into immutable PV specs; the Service must
	// never be recreated while PVs exist. We only ever read it here.
	return existingSvc.Spec.ClusterIP, serverReady, nil
}

// nodeIPs returns the sorted addresses of all cluster nodes. Kubelet mounts
// NFS from the node network namespace, so generated NetworkPolicies must
// admit these.
func (r *SharedVolumeReconciler) nodeIPs(ctx context.Context) ([]string, error) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for i := range nodeList.Items {
		for _, addr := range nodeList.Items[i].Status.Addresses {
			if addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP {
				set[addr.Address] = true
			}
		}
	}
	ips := make([]string, 0, len(set))
	for ip := range set {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	return ips, nil
}

// reconcileNetworkPolicy keeps the per-volume ingress policy in sync with
// the target namespaces and current node IPs, or removes it when disabled.
func (r *SharedVolumeReconciler) reconcileNetworkPolicy(ctx context.Context, sv *nfszv1alpha1.SharedVolume, targets []string) error {
	existing := &networkingv1.NetworkPolicy{}
	key := types.NamespacedName{Namespace: r.OperatorNamespace, Name: ServerName(sv)}
	getErr := r.Get(ctx, key, existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return getErr
	}

	if sv.Spec.NetworkPolicy == nfszv1alpha1.NetworkPolicyDisabled {
		if getErr == nil {
			return client.IgnoreNotFound(r.Delete(ctx, existing))
		}
		return nil
	}

	ips, err := r.nodeIPs(ctx)
	if err != nil {
		return err
	}
	desired := BuildNetworkPolicy(sv, r.OperatorNamespace, targets, ips, r.AllowCIDRs)

	if apierrors.IsNotFound(getErr) {
		return r.createOwned(ctx, sv, desired)
	}
	if !apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		existing.Spec = desired.Spec
		return r.Update(ctx, existing)
	}
	return nil
}

// targetNamespaces resolves spec.namespaces to the sorted set of existing,
// non-terminating namespaces.
func (r *SharedVolumeReconciler) targetNamespaces(ctx context.Context, sv *nfszv1alpha1.SharedVolume) ([]string, error) {
	set := map[string]bool{}

	for _, name := range sv.Spec.Namespaces.Names {
		ns := &corev1.Namespace{}
		err := r.Get(ctx, types.NamespacedName{Name: name}, ns)
		switch {
		case apierrors.IsNotFound(err):
			continue // not created yet; the Namespace watch will requeue us
		case err != nil:
			return nil, err
		}
		if ns.DeletionTimestamp.IsZero() {
			set[name] = true
		}
	}

	if sel := sv.Spec.Namespaces.Selector; sel != nil {
		selector, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return nil, fmt.Errorf("invalid namespace selector: %w", err)
		}
		nsList := &corev1.NamespaceList{}
		if err := r.List(ctx, nsList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, err
		}
		for i := range nsList.Items {
			if nsList.Items[i].DeletionTimestamp.IsZero() {
				set[nsList.Items[i].Name] = true
			}
		}
	}

	targets := make([]string, 0, len(set))
	for ns := range set {
		targets = append(targets, ns)
	}
	sort.Strings(targets)
	return targets, nil
}

// reconcileBindings ensures a pre-bound PV+PVC pair per target namespace.
func (r *SharedVolumeReconciler) reconcileBindings(ctx context.Context, sv *nfszv1alpha1.SharedVolume, targets []string, clusterIP string) ([]nfszv1alpha1.NamespaceBinding, error) {
	bindings := make([]nfszv1alpha1.NamespaceBinding, 0, len(targets))
	for _, ns := range targets {
		phase, err := r.reconcileBinding(ctx, sv, ns, clusterIP)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, nfszv1alpha1.NamespaceBinding{
			Namespace: ns,
			PVCName:   ConsumerPVCName(sv),
			Phase:     phase,
		})
	}
	return bindings, nil
}

func (r *SharedVolumeReconciler) reconcileBinding(ctx context.Context, sv *nfszv1alpha1.SharedVolume, ns, clusterIP string) (nfszv1alpha1.BindingPhase, error) {
	pv := BuildPV(sv, ns, clusterIP)
	existingPV := &corev1.PersistentVolume{}
	switch err := r.Get(ctx, client.ObjectKeyFromObject(pv), existingPV); {
	case apierrors.IsNotFound(err):
		if err := r.createOwned(ctx, sv, pv); err != nil {
			return "", err
		}
		existingPV = pv
	case err != nil:
		return "", err
	default:
		// Released-PV repair: when the user deletes the consumer PVC, the PV
		// keeps a claimRef with the dead PVC's UID and never rebinds. Clear
		// the UID so the recreated PVC can bind again.
		if existingPV.Status.Phase == corev1.VolumeReleased && existingPV.Spec.ClaimRef != nil && existingPV.Spec.ClaimRef.UID != "" {
			existingPV.Spec.ClaimRef.UID = ""
			existingPV.Spec.ClaimRef.ResourceVersion = ""
			if err := r.Update(ctx, existingPV); err != nil {
				return "", err
			}
		}
	}

	pvc := BuildConsumerPVC(sv, ns)
	existingPVC := &corev1.PersistentVolumeClaim{}
	switch err := r.Get(ctx, client.ObjectKeyFromObject(pvc), existingPVC); {
	case apierrors.IsNotFound(err):
		if err := r.createOwned(ctx, sv, pvc); err != nil {
			return "", err
		}
		return nfszv1alpha1.BindingPending, nil
	case err != nil:
		return "", err
	}

	// Never adopt a PVC we didn't create.
	if existingPVC.Labels[LabelSharedVolume] != sv.Name {
		r.Recorder.Eventf(sv, corev1.EventTypeWarning, "PVCConflict",
			"namespace %s already has a PVC named %s not managed by this SharedVolume", ns, pvc.Name)
		return nfszv1alpha1.BindingConflict, nil
	}
	if !existingPVC.DeletionTimestamp.IsZero() {
		return nfszv1alpha1.BindingTerminating, nil
	}
	if existingPVC.Status.Phase == corev1.ClaimBound {
		return nfszv1alpha1.BindingBound, nil
	}
	return nfszv1alpha1.BindingPending, nil
}

// cleanupStaleBindings removes PV/PVC pairs for namespaces that left the
// target set. PVC first, then PV.
func (r *SharedVolumeReconciler) cleanupStaleBindings(ctx context.Context, sv *nfszv1alpha1.SharedVolume, targets []string) error {
	pvList := &corev1.PersistentVolumeList{}
	if err := r.List(ctx, pvList, client.MatchingLabels{LabelSharedVolume: sv.Name}); err != nil {
		return err
	}
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Spec.ClaimRef == nil {
			continue
		}
		ns := pv.Spec.ClaimRef.Namespace
		if slices.Contains(targets, ns) {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: pv.Spec.ClaimRef.Name}, pvc)
		switch {
		case err == nil && pvc.Labels[LabelSharedVolume] == sv.Name:
			if pvc.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
			// PVC still exists (possibly blocked by a running pod); keep the
			// PV until the claim is really gone.
			continue
		case err != nil && !apierrors.IsNotFound(err):
			return err
		}
		if err := r.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// reconcileDelete tears down in order: consumer PVCs -> PVs -> server stack
// -> backing PVC (honoring reclaimPolicy), then drops the finalizer.
func (r *SharedVolumeReconciler) reconcileDelete(ctx context.Context, sv *nfszv1alpha1.SharedVolume) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(sv, finalizerName) {
		return ctrl.Result{}, nil
	}

	if sv.Status.Phase != nfszv1alpha1.PhaseTerminating {
		sv.Status.Phase = nfszv1alpha1.PhaseTerminating
		if err := r.Status().Update(ctx, sv); err != nil && !apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
	}

	// 1. Consumer PVCs, then their PVs.
	pvList := &corev1.PersistentVolumeList{}
	if err := r.List(ctx, pvList, client.MatchingLabels{LabelSharedVolume: sv.Name}); err != nil {
		return ctrl.Result{}, err
	}
	remaining := 0
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if ref := pv.Spec.ClaimRef; ref != nil {
			pvc := &corev1.PersistentVolumeClaim{}
			err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, pvc)
			switch {
			case err == nil && pvc.Labels[LabelSharedVolume] == sv.Name:
				if pvc.DeletionTimestamp.IsZero() {
					if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
						return ctrl.Result{}, err
					}
				}
				remaining++ // wait for the claim to vanish before deleting the PV
				continue
			case err != nil && !apierrors.IsNotFound(err):
				return ctrl.Result{}, err
			}
		}
		if err := r.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	if remaining > 0 {
		log.Info("waiting for consumer PVCs to terminate", "remaining", remaining)
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	// 2. Server stack.
	for _, obj := range []client.Object{
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: ServerName(sv), Namespace: r.OperatorNamespace}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: ServerName(sv), Namespace: r.OperatorNamespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ServerName(sv), Namespace: r.OperatorNamespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName(sv), Namespace: r.OperatorNamespace}},
	} {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// 3. Backing PVC.
	backing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: r.OperatorNamespace, Name: BackingPVCName(sv)}, backing)
	switch {
	case err == nil:
		if sv.Spec.ReclaimPolicy == nfszv1alpha1.ReclaimRetain {
			if backing.Annotations[AnnotationRetained] == "" {
				if backing.Annotations == nil {
					backing.Annotations = map[string]string{}
				}
				backing.Annotations[AnnotationRetained] = sv.Name
				// Detach from the owner so GC does not delete the data.
				backing.OwnerReferences = nil
				if err := r.Update(ctx, backing); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else {
			if backing.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, backing); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
			}
		}
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(sv, finalizerName)
	return ctrl.Result{}, r.Update(ctx, sv)
}

func (r *SharedVolumeReconciler) updateStatus(ctx context.Context, sv *nfszv1alpha1.SharedVolume, clusterIP string, serverReady bool, bindings []nfszv1alpha1.NamespaceBinding) error {
	bound := 0
	conflict := false
	for _, b := range bindings {
		switch b.Phase {
		case nfszv1alpha1.BindingBound:
			bound++
		case nfszv1alpha1.BindingConflict:
			conflict = true
		}
	}

	sv.Status.ObservedGeneration = sv.Generation
	sv.Status.Bindings = bindings
	sv.Status.BoundSummary = fmt.Sprintf("%d/%d", bound, len(bindings))
	sv.Status.BackingPVC = r.OperatorNamespace + "/" + BackingPVCName(sv)
	if clusterIP != "" {
		sv.Status.ServerEndpoint = ServerEndpoint(clusterIP)
	}

	switch {
	case conflict:
		// Needs user action regardless of server state.
		sv.Status.Phase = nfszv1alpha1.PhaseDegraded
	case clusterIP == "" || !serverReady:
		sv.Status.Phase = nfszv1alpha1.PhaseProvisioning
	case len(bindings) > 0 && bound == len(bindings):
		sv.Status.Phase = nfszv1alpha1.PhaseReady
	default:
		sv.Status.Phase = nfszv1alpha1.PhaseProvisioning
	}

	meta.SetStatusCondition(&sv.Status.Conditions, metav1.Condition{
		Type:               nfszv1alpha1.ConditionServerReady,
		Status:             boolToCondition(serverReady),
		Reason:             ifElse(serverReady, "ServerRunning", "ServerStarting"),
		Message:            ifElse(serverReady, "NFS server is serving", "NFS server pod is not ready"),
		ObservedGeneration: sv.Generation,
	})
	meta.SetStatusCondition(&sv.Status.Conditions, metav1.Condition{
		Type:               nfszv1alpha1.ConditionAllBound,
		Status:             boolToCondition(len(bindings) > 0 && bound == len(bindings)),
		Reason:             ifElse(bound == len(bindings) && len(bindings) > 0, "AllBound", "NotAllBound"),
		Message:            fmt.Sprintf("%d of %d target namespaces bound", bound, len(bindings)),
		ObservedGeneration: sv.Generation,
	})

	return r.Status().Update(ctx, sv)
}

// createOwned creates obj with sv as its controller owner.
func (r *SharedVolumeReconciler) createOwned(ctx context.Context, sv *nfszv1alpha1.SharedVolume, obj client.Object) error {
	if err := controllerutil.SetControllerReference(sv, obj, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *SharedVolumeReconciler) createIfMissing(ctx context.Context, sv *nfszv1alpha1.SharedVolume, obj client.Object) error {
	existing := obj.DeepCopyObject().(client.Object)
	switch err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing); {
	case apierrors.IsNotFound(err):
		return r.createOwned(ctx, sv, obj)
	case err != nil:
		return err
	}
	return nil
}

func boolToCondition(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func ifElse(cond bool, t, f string) string {
	if cond {
		return t
	}
	return f
}

// requeueDelay paces polling while waiting on slow external state
// (terminating PVCs, server rollout).
const requeueDelay = 5 * time.Second

// SetupWithManager sets up the controller with the Manager.
func (r *SharedVolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Namespace events map to every SharedVolume that could select them so
	// newly created or newly labeled namespaces get their PVC immediately.
	mapNamespace := func(ctx context.Context, obj client.Object) []reconcile.Request {
		svList := &nfszv1alpha1.SharedVolumeList{}
		if err := mgr.GetClient().List(ctx, svList); err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for i := range svList.Items {
			sv := &svList.Items[i]
			if namespaceCouldMatch(sv, obj) {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: sv.Name}})
			}
		}
		return reqs
	}

	// Node add/remove must refresh every volume's NetworkPolicy ipBlocks.
	mapNode := func(ctx context.Context, _ client.Object) []reconcile.Request {
		svList := &nfszv1alpha1.SharedVolumeList{}
		if err := mgr.GetClient().List(ctx, svList); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(svList.Items))
		for i := range svList.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: svList.Items[i].Name}})
		}
		return reqs
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nfszv1alpha1.SharedVolume{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.PersistentVolume{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(mapNamespace)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(mapNode)).
		Named("sharedvolume").
		Complete(r)
}

// namespaceCouldMatch reports whether ns is named by or label-selected by sv.
func namespaceCouldMatch(sv *nfszv1alpha1.SharedVolume, ns client.Object) bool {
	if slices.Contains(sv.Spec.Namespaces.Names, ns.GetName()) {
		return true
	}
	if sel := sv.Spec.Namespaces.Selector; sel != nil {
		selector, err := metav1.LabelSelectorAsSelector(sel)
		if err == nil && selector.Matches(labels.Set(ns.GetLabels())) {
			return true
		}
	}
	return false
}
