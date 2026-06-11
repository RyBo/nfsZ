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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nfszv1alpha1 "github.com/rybo/nfsZ/api/v1alpha1"
)

const (
	operatorNS = "nfsz-system"
	timeout10s = 10 * time.Second
)

func newReconciler() *SharedVolumeReconciler {
	return &SharedVolumeReconciler{
		Client:            k8sClient,
		Scheme:            scheme.Scheme,
		Recorder:          events.NewFakeRecorder(64),
		OperatorNamespace: operatorNS,
		GaneshaImage:      "nfsz/ganesha:test",
		AllowCIDRs:        []string{"10.99.0.0/16"},
	}
}

func reconcileOnce(name string) {
	GinkgoHelper()
	_, err := newReconciler().Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	Expect(err).NotTo(HaveOccurred())
}

func ensureNamespace(name string, nsLabels map[string]string) {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nsLabels}}
	err := k8sClient.Create(ctx, ns)
	if apierrors.IsAlreadyExists(err) {
		existing := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, existing)).To(Succeed())
		existing.Labels = nsLabels
		Expect(k8sClient.Update(ctx, existing)).To(Succeed())
		return
	}
	Expect(err).NotTo(HaveOccurred())
}

var svCounter int

func newSharedVolume(targets []string, selector *metav1.LabelSelector) *nfszv1alpha1.SharedVolume {
	svCounter++
	return &nfszv1alpha1.SharedVolume{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("test-sv-%d", svCounter)},
		Spec: nfszv1alpha1.SharedVolumeSpec{
			Capacity: resource.MustParse("1Gi"),
			Namespaces: nfszv1alpha1.NamespaceSelection{
				Names:    targets,
				Selector: selector,
			},
		},
	}
}

var _ = Describe("SharedVolume controller", func() {
	BeforeEach(func() {
		ensureNamespace(operatorNS, nil)
	})

	It("creates the server stack and pre-bound PV/PVC pairs", func() {
		ensureNamespace("team-a", nil)
		ensureNamespace("team-b", nil)
		sv := newSharedVolume([]string{"team-a", "team-b"}, nil)
		Expect(k8sClient.Create(ctx, sv)).To(Succeed())
		reconcileOnce(sv.Name) // adds finalizer + server stack
		reconcileOnce(sv.Name) // bindings now that the Service IP exists

		By("server stack in the operator namespace")
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: BackingPVCName(sv)}, pvc)).To(Succeed())
		Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: ServerName(sv)}, deploy)).To(Succeed())
		Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("nfsz/ganesha:test"))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: ServerName(sv)}, svc)).To(Succeed())
		Expect(svc.Spec.ClusterIP).NotTo(BeEmpty())

		By("a pre-bound PV and PVC per namespace")
		for _, ns := range []string{"team-a", "team-b"} {
			pv := &corev1.PersistentVolume{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: PVName(sv, ns)}, pv)).To(Succeed())
			Expect(pv.Spec.NFS.Server).To(Equal(svc.Spec.ClusterIP))
			Expect(pv.Spec.StorageClassName).To(Equal(""))
			Expect(pv.Spec.ClaimRef.Namespace).To(Equal(ns))
			Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
			Expect(pv.Spec.MountOptions).To(ContainElement("nfsvers=4.1"))

			cpvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sv.Name}, cpvc)).To(Succeed())
			Expect(*cpvc.Spec.StorageClassName).To(Equal(""))
			Expect(cpvc.Spec.VolumeName).To(Equal(PVName(sv, ns)))
			Expect(cpvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteMany))
		}

		By("status is populated")
		got := &nfszv1alpha1.SharedVolume{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sv.Name}, got)).To(Succeed())
		Expect(got.Status.ServerEndpoint).To(Equal(svc.Spec.ClusterIP + ":2049"))
		Expect(got.Status.Bindings).To(HaveLen(2))
		Expect(got.Finalizers).To(ContainElement("nfsz.dev/teardown"))
	})

	It("targets namespaces via label selector and follows label changes", func() {
		key := "nfsz.test/selected"
		ensureNamespace("sel-a", map[string]string{key: "yes"})
		ensureNamespace("sel-b", nil)
		sv := newSharedVolume(nil, &metav1.LabelSelector{MatchLabels: map[string]string{key: "yes"}})
		Expect(k8sClient.Create(ctx, sv)).To(Succeed())
		reconcileOnce(sv.Name)
		reconcileOnce(sv.Name)

		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "sel-a", Name: sv.Name}, &corev1.PersistentVolumeClaim{})).To(Succeed())
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "sel-b", Name: sv.Name}, &corev1.PersistentVolumeClaim{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		By("labeling sel-b adds it; the PVC appears on next reconcile")
		ensureNamespace("sel-b", map[string]string{key: "yes"})
		reconcileOnce(sv.Name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "sel-b", Name: sv.Name}, &corev1.PersistentVolumeClaim{})).To(Succeed())

		By("unlabeling sel-b garbage-collects its pair")
		ensureNamespace("sel-b", map[string]string{})
		reconcileOnce(sv.Name)
		Eventually(func() bool {
			pvc := &corev1.PersistentVolumeClaim{}
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "sel-b", Name: sv.Name}, pvc)
			// envtest has no namespace controller to fully remove objects,
			// but the deletionTimestamp proves the operator deleted it.
			return apierrors.IsNotFound(err) || !pvc.DeletionTimestamp.IsZero()
		}).Should(BeTrue(), "consumer PVC in sel-b should be deleted")
	})

	It("reports Conflict for a pre-existing foreign PVC and never adopts it", func() {
		ensureNamespace("conflict-ns", nil)
		sv := newSharedVolume([]string{"conflict-ns"}, nil)
		foreign := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: sv.Name, Namespace: "conflict-ns"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		Expect(k8sClient.Create(ctx, sv)).To(Succeed())
		reconcileOnce(sv.Name)
		reconcileOnce(sv.Name)

		got := &nfszv1alpha1.SharedVolume{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sv.Name}, got)).To(Succeed())
		Expect(got.Status.Bindings).To(HaveLen(1))
		Expect(got.Status.Bindings[0].Phase).To(Equal(nfszv1alpha1.BindingConflict))
		Expect(got.Status.Phase).To(Equal(nfszv1alpha1.PhaseDegraded))

		check := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "conflict-ns", Name: sv.Name}, check)).To(Succeed())
		Expect(check.Labels).NotTo(HaveKey(LabelSharedVolume))
	})

	It("repairs a Released PV so a recreated PVC can rebind", func() {
		ensureNamespace("repair-ns", nil)
		sv := newSharedVolume([]string{"repair-ns"}, nil)
		Expect(k8sClient.Create(ctx, sv)).To(Succeed())
		reconcileOnce(sv.Name)
		reconcileOnce(sv.Name)

		pv := &corev1.PersistentVolume{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: PVName(sv, "repair-ns")}, pv)).To(Succeed())
		// Simulate what the PV controller does after the bound PVC is deleted.
		pv.Spec.ClaimRef.UID = "dead-beef"
		Expect(k8sClient.Update(ctx, pv)).To(Succeed())
		pv.Status.Phase = corev1.VolumeReleased
		Expect(k8sClient.Status().Update(ctx, pv)).To(Succeed())

		reconcileOnce(sv.Name)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: PVName(sv, "repair-ns")}, pv)).To(Succeed())
		Expect(string(pv.Spec.ClaimRef.UID)).To(BeEmpty())
	})

	It("manages the per-volume NetworkPolicy", func() {
		ensureNamespace("np-ns", nil)
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "np-node"}}
		if err := k8sClient.Create(ctx, node); err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "np-node"}, node)).To(Succeed())
		node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.9"}}
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

		sv := newSharedVolume([]string{"np-ns"}, nil)
		Expect(k8sClient.Create(ctx, sv)).To(Succeed())
		reconcileOnce(sv.Name)
		reconcileOnce(sv.Name)

		By("policy admits target namespaces and node IPs on 2049 only")
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: ServerName(sv)}, np)).To(Succeed())
		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(LabelSharedVolume, sv.Name))
		Expect(np.Spec.Ingress).To(HaveLen(1))
		Expect(np.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(2049))

		var nsPeers, ipPeers []string
		for _, p := range np.Spec.Ingress[0].From {
			if p.NamespaceSelector != nil {
				nsPeers = append(nsPeers, p.NamespaceSelector.MatchLabels[corev1.LabelMetadataName])
			}
			if p.IPBlock != nil {
				ipPeers = append(ipPeers, p.IPBlock.CIDR)
			}
		}
		Expect(nsPeers).To(ContainElement("np-ns"))
		Expect(ipPeers).To(ContainElement("172.18.0.9/32"))
		Expect(ipPeers).To(ContainElement("10.99.0.0/16"), "operator AllowCIDRs included")

		By("disabling removes the policy")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sv.Name}, sv)).To(Succeed())
		sv.Spec.NetworkPolicy = nfszv1alpha1.NetworkPolicyDisabled
		Expect(k8sClient.Update(ctx, sv)).To(Succeed())
		reconcileOnce(sv.Name)
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: ServerName(sv)}, np)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("tears down in order and honors reclaimPolicy Retain", func() {
		ensureNamespace("retain-ns", nil)
		sv := newSharedVolume([]string{"retain-ns"}, nil)
		sv.Spec.ReclaimPolicy = nfszv1alpha1.ReclaimRetain
		Expect(k8sClient.Create(ctx, sv)).To(Succeed())
		reconcileOnce(sv.Name)
		reconcileOnce(sv.Name)

		Expect(k8sClient.Delete(ctx, sv)).To(Succeed())
		// Teardown needs several reconciles while consumer PVCs terminate.
		// envtest's apiserver adds the pvc-protection finalizer but runs no
		// controller to clear it, so strip it here to let deletion finish.
		Eventually(func() error {
			pvc := &corev1.PersistentVolumeClaim{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "retain-ns", Name: sv.Name}, pvc); err == nil &&
				!pvc.DeletionTimestamp.IsZero() && len(pvc.Finalizers) > 0 {
				pvc.Finalizers = nil
				_ = k8sClient.Update(ctx, pvc)
			}

			got := &nfszv1alpha1.SharedVolume{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: sv.Name}, got); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			if _, err := newReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: sv.Name}}); err != nil {
				return err
			}
			return fmt.Errorf("SharedVolume still present")
		}).WithTimeout(timeout10s).Should(Succeed())

		By("backing PVC is retained, annotated, and unowned")
		backing := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: BackingPVCName(sv)}, backing)).To(Succeed())
		Expect(backing.Annotations).To(HaveKeyWithValue(AnnotationRetained, sv.Name))
		Expect(backing.OwnerReferences).To(BeEmpty())

		By("server stack is gone")
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: ServerName(sv)}, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})

var _ = Describe("resource builders", func() {
	It("renders the ganesha conf hash into the pod template", func() {
		sv := &nfszv1alpha1.SharedVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "hashy"},
			Spec:       nfszv1alpha1.SharedVolumeSpec{Capacity: resource.MustParse("1Gi")},
		}
		cm := BuildConfigMap(sv, operatorNS)
		deploy := BuildServerDeployment(sv, operatorNS, "img", cm)
		Expect(deploy.Spec.Template.Annotations).To(HaveKey("nfsz.dev/conf-hash"))

		cm.Data["ganesha.conf"] += "\n# changed"
		deploy2 := BuildServerDeployment(sv, operatorNS, "img", cm)
		Expect(deploy2.Spec.Template.Annotations["nfsz.dev/conf-hash"]).
			NotTo(Equal(deploy.Spec.Template.Annotations["nfsz.dev/conf-hash"]))
	})

	It("uses ClusterIP, pseudo-root path and v4.1 mount options in PVs", func() {
		sv := &nfszv1alpha1.SharedVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvtest"},
			Spec:       nfszv1alpha1.SharedVolumeSpec{Capacity: resource.MustParse("2Gi")},
		}
		pv := BuildPV(sv, "ns-x", "10.0.0.7")
		Expect(pv.Spec.NFS.Server).To(Equal("10.0.0.7"))
		Expect(pv.Spec.NFS.Path).To(Equal("/"))
		Expect(pv.Name).To(Equal("nfsz-pvtest-ns-x"))
		Expect(client.ObjectKeyFromObject(pv).Name).To(Equal(PVName(sv, "ns-x")))
	})
})
