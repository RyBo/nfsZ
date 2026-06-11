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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	nfszv1alpha1 "github.com/rybo/nfsZ/api/v1alpha1"
	"github.com/rybo/nfsZ/internal/ganesha"
)

const (
	// LabelSharedVolume marks every object owned by a SharedVolume.
	LabelSharedVolume = "nfsz.dev/shared-volume"
	// LabelManagedBy identifies objects this operator manages.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "nfsz"

	confHashAnnotation = "nfsz.dev/conf-hash"

	nfsPort = 2049
)

func managedLabels(sv *nfszv1alpha1.SharedVolume) map[string]string {
	return map[string]string{
		LabelSharedVolume: sv.Name,
		LabelManagedBy:    managedByValue,
	}
}

// Object names. All server-side objects live in the operator namespace and
// are prefixed to avoid colliding with user objects there.

func BackingPVCName(sv *nfszv1alpha1.SharedVolume) string {
	return "nfsz-" + sv.Name
}

func ServerName(sv *nfszv1alpha1.SharedVolume) string {
	return "nfsz-" + sv.Name + "-server"
}

func ConfigMapName(sv *nfszv1alpha1.SharedVolume) string {
	return "nfsz-" + sv.Name + "-ganesha"
}

// PVName is the cluster-scoped PV created for one target namespace.
func PVName(sv *nfszv1alpha1.SharedVolume, namespace string) string {
	return "nfsz-" + sv.Name + "-" + namespace
}

// ConsumerPVCName is what users reference in their pods: the SharedVolume name.
func ConsumerPVCName(sv *nfszv1alpha1.SharedVolume) string {
	return sv.Name
}

// BuildBackingPVC is the RWO volume that stores the data and backs the
// ganesha export.
func BuildBackingPVC(sv *nfszv1alpha1.SharedVolume, operatorNamespace string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BackingPVCName(sv),
			Namespace: operatorNamespace,
			Labels:    managedLabels(sv),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: sv.Spec.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: sv.Spec.Capacity},
			},
		},
	}
}

func BuildConfigMap(sv *nfszv1alpha1.SharedVolume, operatorNamespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName(sv),
			Namespace: operatorNamespace,
			Labels:    managedLabels(sv),
		},
		Data: map[string]string{"ganesha.conf": ganesha.Config()},
	}
}

func confHash(cm *corev1.ConfigMap) string {
	sum := sha256.Sum256([]byte(cm.Data["ganesha.conf"]))
	return hex.EncodeToString(sum[:8])
}

// BuildServerDeployment runs the single-replica ganesha server. Recreate
// strategy because the backing PVC is RWO. The conf hash annotation rolls the
// pod when the ganesha config changes.
func BuildServerDeployment(sv *nfszv1alpha1.SharedVolume, operatorNamespace, image string, cm *corev1.ConfigMap) *appsv1.Deployment {
	labels := managedLabels(sv)
	selector := map[string]string{LabelSharedVolume: sv.Name, "app.kubernetes.io/component": "nfs-server"}
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	podLabels["app.kubernetes.io/component"] = "nfs-server"

	backingVolume := corev1.Volume{
		Name: "backing",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: BackingPVCName(sv),
			},
		},
	}
	confVolume := corev1.Volume{
		Name: "conf",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name},
			},
		},
	}
	// The exported tree and ganesha's NFSv4 recovery state both live on the
	// backing PVC (separate subPaths) so client reclaim works across pod
	// restarts and the export is always a real filesystem, never overlayfs.
	dataMounts := []corev1.VolumeMount{
		{Name: "backing", SubPath: "data", MountPath: "/export"},
		{Name: "backing", SubPath: ".ganesha", MountPath: "/var/lib/nfs/ganesha"},
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServerName(sv),
			Namespace: operatorNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: map[string]string{confHashAnnotation: confHash(cm)},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{
						Name:         "init-perms",
						Image:        image,
						Command:      []string{"sh", "-c", "chmod 1777 /export && mkdir -p /var/lib/nfs/ganesha"},
						VolumeMounts: dataMounts,
					}},
					Containers: []corev1.Container{{
						Name:  "ganesha",
						Image: image,
						Ports: []corev1.ContainerPort{{Name: "nfs", ContainerPort: nfsPort}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities: &corev1.Capabilities{
								Add: []corev1.Capability{"DAC_READ_SEARCH", "SYS_RESOURCE"},
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(nfsPort)},
							},
							InitialDelaySeconds: 2,
							PeriodSeconds:       5,
						},
						VolumeMounts: append([]corev1.VolumeMount{
							{Name: "conf", MountPath: "/etc/ganesha"},
						}, dataMounts...),
					}},
					Volumes: []corev1.Volume{confVolume, backingVolume},
				},
			},
		},
	}
}

func BuildService(sv *nfszv1alpha1.SharedVolume, operatorNamespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServerName(sv),
			Namespace: operatorNamespace,
			Labels:    managedLabels(sv),
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{LabelSharedVolume: sv.Name, "app.kubernetes.io/component": "nfs-server"},
			Ports: []corev1.ServicePort{{
				Name:     "nfs",
				Port:     nfsPort,
				Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// BuildPV is the pre-bound, cluster-scoped PV for one target namespace.
//
// Sharp edges encoded here:
//   - storageClassName "" keeps dynamic provisioners away from the claim.
//   - claimRef pre-binds to exactly one PVC; with the PVC's volumeName set
//     too, binding is deterministic.
//   - The server address is the Service ClusterIP, not DNS: the in-tree nfs
//     plugin resolves the address on the node, where cluster DNS does not
//     resolve (notably in kind).
//   - Reclaim Retain: the operator owns PV lifecycle; nothing is recycled
//     behind its back.
func BuildPV(sv *nfszv1alpha1.SharedVolume, namespace, serverIP string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   PVName(sv, namespace),
			Labels: managedLabels(sv),
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: sv.Spec.Capacity},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              "",
			MountOptions:                  []string{"nfsvers=4.1", "proto=tcp", "hard"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: serverIP, Path: "/"},
			},
			ClaimRef: &corev1.ObjectReference{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Namespace:  namespace,
				Name:       ConsumerPVCName(sv),
			},
		},
	}
}

// BuildConsumerPVC is the namespaced PVC users mount, pre-bound to its PV.
func BuildConsumerPVC(sv *nfszv1alpha1.SharedVolume, namespace string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConsumerPVCName(sv),
			Namespace: namespace,
			Labels:    managedLabels(sv),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptr.To(""), // empty string, not nil: keep the default SC away
			VolumeName:       PVName(sv, namespace),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: sv.Spec.Capacity},
			},
		},
	}
}

func ServerEndpoint(clusterIP string) string {
	return fmt.Sprintf("%s:%d", clusterIP, nfsPort)
}

// BuildNetworkPolicy restricts ingress to this volume's NFS server pod.
//
// Threat model nuance: legitimate NFS traffic originates from *nodes*, not
// consumer pods — kubelet performs the mount in the node's network
// namespace. The rogue path being closed is a pod speaking NFS directly
// (userspace client over TCP, no mount privileges needed). So the policy
// admits: cluster node IPs (nodeIPs, auto-discovered), pods in target
// namespaces (matched via the kubernetes.io/metadata.name label), and any
// operator-configured extra CIDRs (for CNIs that SNAT cross-node traffic to
// tunnel IPs, e.g. Calico VXLAN). Everything else is denied.
//
// Enforcement requires a CNI that implements NetworkPolicy; kind's default
// kindnet does not.
func BuildNetworkPolicy(sv *nfszv1alpha1.SharedVolume, operatorNamespace string, targets, nodeIPs, extraCIDRs []string) *networkingv1.NetworkPolicy {
	nfsTCP := intstr.FromInt32(nfsPort)
	tcp := corev1.ProtocolTCP

	var peers []networkingv1.NetworkPolicyPeer
	for _, ns := range targets {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{corev1.LabelMetadataName: ns},
			},
		})
	}
	for _, ip := range nodeIPs {
		cidr := ip + "/32"
		if strings.Contains(ip, ":") {
			cidr = ip + "/128"
		}
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		})
	}
	for _, cidr := range extraCIDRs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServerName(sv),
			Namespace: operatorNamespace,
			Labels:    managedLabels(sv),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelSharedVolume:             sv.Name,
					"app.kubernetes.io/component": "nfs-server",
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &nfsTCP}},
				From:  peers,
			}},
		},
	}
}
