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
	"maps"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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

func BackupCronJobName(sv *nfszv1alpha1.SharedVolume) string {
	return "nfsz-" + sv.Name + "-backup"
}

func SeedJobName(sv *nfszv1alpha1.SharedVolume) string {
	return "nfsz-" + sv.Name + "-seed"
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
	podLabels := make(map[string]string, len(labels)+1)
	maps.Copy(podLabels, labels)
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

	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(targets)+len(nodeIPs)+len(extraCIDRs))
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

// backupPodSpec is the shared shape of the backup and seed pods: the backing
// PVC's data subPath at /data and the destination hostPath at /backup. Both
// read other-UID files written over NFS, so they run as root (rsync needs
// DAC override); namespaces enforcing baseline/restricted PSA will reject
// them (hostPath + root).
func backupPodSpec(sv *nfszv1alpha1.SharedVolume, image, script string, dataReadOnly bool) corev1.PodSpec {
	hostDir := sv.Spec.Backup.Destination.HostPath.Path
	return corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers: []corev1.Container{{
			Name:    "rsync",
			Image:   image,
			Command: []string{"sh", "-c", script},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "backing", SubPath: "data", MountPath: "/data", ReadOnly: dataReadOnly},
				{Name: "backup", MountPath: "/backup", ReadOnly: !dataReadOnly},
			},
		}},
		Volumes: []corev1.Volume{
			{
				Name: "backing",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: BackingPVCName(sv),
					},
				},
			},
			{
				Name: "backup",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: hostDir,
						Type: ptr.To(corev1.HostPathDirectoryOrCreate),
					},
				},
			},
		},
	}
}

// BuildBackupCronJob mirrors /export (the PVC's data subPath) to
// <hostPath>/<name>/ on the volume's schedule. The mirror is plain files —
// browsable and directly usable on the host — and deletions propagate
// (--delete). -rlt rather than -a: chown/chmod fail on shared host mounts
// (e.g. Docker Desktop virtiofs) and don't matter for a mirror.
//
// The pod mounts the RWO backing PVC directly instead of the NFS export, so
// backups work even when the server is down; required podAffinity to the
// server pod keeps it on the same node for RWO block storage (on local-path
// the PV's node affinity already forces this).
func BuildBackupCronJob(sv *nfszv1alpha1.SharedVolume, operatorNamespace, image string) *batchv1.CronJob {
	labels := managedLabels(sv)
	podLabels := make(map[string]string, len(labels)+1)
	maps.Copy(podLabels, labels)
	podLabels["app.kubernetes.io/component"] = "backup"

	script := fmt.Sprintf(
		"set -e\nmkdir -p /backup/%[1]s\nexec rsync -rlt --delete --stats /data/ /backup/%[1]s/",
		sv.Name,
	)
	podSpec := backupPodSpec(sv, image, script, true)
	podSpec.Affinity = &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						LabelSharedVolume:             sv.Name,
						"app.kubernetes.io/component": "nfs-server",
					},
				},
				TopologyKey: corev1.LabelHostname,
			}},
		},
	}

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BackupCronJobName(sv),
			Namespace: operatorNamespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   sv.Spec.Backup.Schedule,
			Suspend:                    sv.Spec.Backup.Suspend,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			StartingDeadlineSeconds:    ptr.To(int64(300)),
			SuccessfulJobsHistoryLimit: ptr.To(int32(1)),
			FailedJobsHistoryLimit:     ptr.To(int32(1)),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					BackoffLimit:          ptr.To(int32(2)),
					ActiveDeadlineSeconds: ptr.To(int64(3600)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
						Spec:       podSpec,
					},
				},
			},
		},
	}
}

// BuildSeedJob copies an existing host backup into a fresh backing PVC before
// the server Deployment is first created. Both guards live in the script so
// the decision is atomic with the copy: no backup dir → no-op, data already
// present → refuse (never clobbers a retained or re-adopted PVC). No affinity:
// the seed is the PVC's first and only consumer.
func BuildSeedJob(sv *nfszv1alpha1.SharedVolume, operatorNamespace, image string) *batchv1.Job {
	labels := managedLabels(sv)
	podLabels := make(map[string]string, len(labels)+1)
	maps.Copy(podLabels, labels)
	podLabels["app.kubernetes.io/component"] = "seed"

	script := fmt.Sprintf(`set -e
SRC=/backup/%s
[ -d "$SRC" ] || { echo "no backup at $SRC; nothing to seed"; exit 0; }
[ -z "$(ls -A /data 2>/dev/null)" ] || { echo "/data not empty; refusing to seed"; exit 0; }
exec rsync -rlt --stats "$SRC"/ /data/`, sv.Name)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SeedJobName(sv),
			Namespace: operatorNamespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(3)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec:       backupPodSpec(sv, image, script, false),
			},
		},
	}
}
