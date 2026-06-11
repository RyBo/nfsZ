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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReclaimPolicy controls what happens to the backing PVC (and its data) when
// the SharedVolume is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type ReclaimPolicy string

const (
	// ReclaimDelete deletes the backing PVC together with the SharedVolume.
	ReclaimDelete ReclaimPolicy = "Delete"
	// ReclaimRetain orphans the backing PVC so the data survives deletion.
	ReclaimRetain ReclaimPolicy = "Retain"
)

// NetworkPolicyMode controls whether the operator manages a NetworkPolicy
// restricting access to this volume's NFS server.
// +kubebuilder:validation:Enum=Enabled;Disabled
type NetworkPolicyMode string

const (
	// NetworkPolicyEnabled creates a per-volume NetworkPolicy admitting only
	// cluster nodes (kubelet performs the mounts) and pods in target
	// namespaces. Enforcement requires a CNI that implements NetworkPolicy.
	NetworkPolicyEnabled NetworkPolicyMode = "Enabled"
	// NetworkPolicyDisabled leaves the NFS server reachable by any pod in
	// the cluster.
	NetworkPolicyDisabled NetworkPolicyMode = "Disabled"
)

// RestorePolicy controls whether a brand-new (empty) volume is seeded from an
// existing backup at the destination before the NFS server starts serving.
// +kubebuilder:validation:Enum=Auto;Never
type RestorePolicy string

const (
	// RestoreAuto seeds a fresh volume from the backup destination when a
	// backup for this volume exists there and the volume's data is empty.
	RestoreAuto RestorePolicy = "Auto"
	// RestoreNever disables seeding; new volumes always start empty.
	RestoreNever RestorePolicy = "Never"
)

// HostPathDestination mirrors backups into a directory that exists on every
// node, e.g. a kind extraMount shared from the host machine.
type HostPathDestination struct {
	// path is an absolute directory on the node. Data is mirrored to
	// <path>/<sharedvolume-name>/.
	// +kubebuilder:validation:Pattern=`^/`
	// +required
	Path string `json:"path"`
}

// BackupDestination selects where backups are written. Exactly one member
// must be set; the struct exists so other destinations (e.g. object storage)
// can be added without API churn.
// +kubebuilder:validation:XValidation:rule="has(self.hostPath)",message="destination requires hostPath"
type BackupDestination struct {
	// hostPath writes backups to a node-local directory.
	// +optional
	HostPath *HostPathDestination `json:"hostPath,omitempty"`
}

// BackupSpec configures a scheduled rsync mirror of this volume's data. The
// mirror is a plain browsable file tree; deletions in the volume propagate to
// the mirror on the next run.
type BackupSpec struct {
	// schedule is a standard cron expression, evaluated by the CronJob
	// controller in the cluster's time zone.
	// +kubebuilder:validation:MinLength=1
	// +required
	Schedule string `json:"schedule"`

	// destination is where backups are written.
	// +required
	Destination BackupDestination `json:"destination"`

	// suspend pauses scheduled backups without deleting the CronJob.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`

	// restorePolicy controls auto-seeding of fresh volumes from an existing
	// backup at the destination.
	// +kubebuilder:default=Auto
	// +optional
	RestorePolicy RestorePolicy `json:"restorePolicy,omitempty"`
}

// NamespaceSelection picks the namespaces that receive a PVC for this volume.
// The effective set is the union of Names and namespaces matching Selector.
type NamespaceSelection struct {
	// names is an explicit list of namespace names.
	// +optional
	Names []string `json:"names,omitempty"`

	// selector selects namespaces by label.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// SharedVolumeSpec defines the desired state of SharedVolume
type SharedVolumeSpec struct {
	// capacity is the size of the shared volume. Immutable.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="capacity is immutable"
	// +required
	Capacity resource.Quantity `json:"capacity"`

	// storageClassName is the StorageClass used for the backing PVC.
	// Unset means the cluster default StorageClass.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// namespaces selects which namespaces receive a ready-to-use PVC.
	// +required
	Namespaces NamespaceSelection `json:"namespaces"`

	// reclaimPolicy controls the fate of the backing PVC on deletion.
	// +kubebuilder:default=Delete
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// networkPolicy controls the per-volume NetworkPolicy that restricts
	// NFS access to cluster nodes and target namespaces.
	// +kubebuilder:default=Enabled
	// +optional
	NetworkPolicy NetworkPolicyMode `json:"networkPolicy,omitempty"`

	// backup configures a scheduled mirror of this volume's data to a
	// destination that survives the cluster, plus auto-seeding of fresh
	// volumes from that mirror.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`
}

// SharedVolumePhase is a coarse rollup of the volume's state.
type SharedVolumePhase string

const (
	PhasePending      SharedVolumePhase = "Pending"
	PhaseProvisioning SharedVolumePhase = "Provisioning"
	PhaseReady        SharedVolumePhase = "Ready"
	PhaseDegraded     SharedVolumePhase = "Degraded"
	PhaseTerminating  SharedVolumePhase = "Terminating"
)

// BindingPhase describes the state of one namespace's PVC binding.
type BindingPhase string

const (
	BindingPending     BindingPhase = "Pending"
	BindingBound       BindingPhase = "Bound"
	BindingConflict    BindingPhase = "Conflict"
	BindingTerminating BindingPhase = "Terminating"
)

// NamespaceBinding reports the PVC state in one target namespace.
type NamespaceBinding struct {
	Namespace string       `json:"namespace"`
	PVCName   string       `json:"pvcName"`
	Phase     BindingPhase `json:"phase"`
}

// Condition types reported on SharedVolume.
const (
	ConditionServerReady = "ServerReady"
	ConditionAllBound    = "AllBound"
	// ConditionRestored reports the outcome of the one-shot seed-from-backup
	// step that runs before the NFS server first starts.
	ConditionRestored = "Restored"
)

// Reasons used with ConditionRestored.
const (
	ReasonSeedRunning       = "SeedRunning"
	ReasonSeeded            = "Seeded"
	ReasonSeedFailed        = "SeedFailed"
	ReasonPreexistingServer = "PreexistingServer"
)

// SharedVolumeStatus defines the observed state of SharedVolume.
type SharedVolumeStatus struct {
	// +optional
	Phase SharedVolumePhase `json:"phase,omitempty"`

	// serverEndpoint is the ClusterIP:port of the in-cluster NFS server.
	// +optional
	ServerEndpoint string `json:"serverEndpoint,omitempty"`

	// backingPVC is "namespace/name" of the PVC storing the data.
	// +optional
	BackingPVC string `json:"backingPVC,omitempty"`

	// boundSummary is "<bound>/<total>" across target namespaces.
	// +optional
	BoundSummary string `json:"boundSummary,omitempty"`

	// bindings lists per-namespace PVC states.
	// +optional
	Bindings []NamespaceBinding `json:"bindings,omitempty"`

	// lastBackupTime is the last time a scheduled backup completed
	// successfully, copied from the backup CronJob's status.
	// +optional
	LastBackupTime *metav1.Time `json:"lastBackupTime,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sv;svol
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Capacity",type=string,JSONPath=`.spec.capacity`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.status.serverEndpoint`
// +kubebuilder:printcolumn:name="Bound",type=string,JSONPath=`.status.boundSummary`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SharedVolume is a cluster-scoped RWX volume shared across namespaces,
// served by an operator-managed in-cluster NFSv4 server.
type SharedVolume struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SharedVolume
	// +required
	Spec SharedVolumeSpec `json:"spec"`

	// status defines the observed state of SharedVolume
	// +optional
	Status SharedVolumeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SharedVolumeList contains a list of SharedVolume
type SharedVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SharedVolume `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SharedVolume{}, &SharedVolumeList{})
}
