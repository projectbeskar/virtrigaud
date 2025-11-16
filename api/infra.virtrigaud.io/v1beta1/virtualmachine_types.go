/*
Copyright 2025.

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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// VirtualMachineFinalizer is the finalizer for VirtualMachine resources
	VirtualMachineFinalizer = "virtualmachine.infra.virtrigaud.io/finalizer"
)

// VirtualMachineSpec defines the desired state of VirtualMachine.
type VirtualMachineSpec struct {
	// ProviderRef references the Provider that manages this VM
	ProviderRef ObjectRef `json:"providerRef"`

	// ClassRef references the VMClass that defines resource allocation
	ClassRef ObjectRef `json:"classRef"`

	// ImageRef references the VMImage to use as base template.
	// Either ImageRef or ImportedDisk must be specified, but not both.
	// +optional
	ImageRef *ObjectRef `json:"imageRef,omitempty"`

	// ImportedDisk references a pre-imported disk (e.g., from migration).
	// Either ImageRef or ImportedDisk must be specified, but not both.
	// +optional
	ImportedDisk *ImportedDiskRef `json:"importedDisk,omitempty"`

	// Networks specifies network attachments for the VM
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Networks []VMNetworkRef `json:"networks,omitempty"`

	// Disks specifies additional disks beyond the root disk
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Disks []DiskSpec `json:"disks,omitempty"`

	// UserData contains cloud-init configuration
	// +optional
	UserData *UserData `json:"userData,omitempty"`

	// Placement provides hints for VM placement
	// +optional
	Placement *Placement `json:"placement,omitempty"`

	// PowerState specifies the desired power state
	// +optional
	PowerState PowerState `json:"powerState,omitempty"`

	// Tags are applied to the VM for organization
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Tags []string `json:"tags,omitempty"`

	// Resources allows overriding resource allocation from the VMClass
	// +optional
	Resources *VirtualMachineResources `json:"resources,omitempty"`

	// PlacementRef references a VMPlacementPolicy for advanced placement rules
	// +optional
	PlacementRef *LocalObjectReference `json:"placementRef,omitempty"`

	// Snapshot defines snapshot-related operations
	// +optional
	Snapshot *VMSnapshotOperation `json:"snapshot,omitempty"`

	// Lifecycle defines VM lifecycle configuration
	// +optional
	Lifecycle *VirtualMachineLifecycle `json:"lifecycle,omitempty"`
}

// PowerState represents the desired power state of a VM
// +kubebuilder:validation:Enum=On;Off;OffGraceful
type PowerState string

const (
	// PowerStateOn indicates the VM should be powered on
	PowerStateOn PowerState = "On"
	// PowerStateOff indicates the VM should be powered off
	PowerStateOff PowerState = "Off"
	// PowerStateOffGraceful indicates the VM should be gracefully shut down
	PowerStateOffGraceful PowerState = "OffGraceful"
)

// VirtualMachineLifecycle defines lifecycle configuration for a VM
type VirtualMachineLifecycle struct {
	// PreStop defines actions to take before stopping the VM
	// +optional
	PreStop *LifecycleHandler `json:"preStop,omitempty"`

	// PostStart defines actions to take after starting the VM
	// +optional
	PostStart *LifecycleHandler `json:"postStart,omitempty"`

	// GracefulShutdownTimeout defines how long to wait for graceful shutdown
	// +optional
	// +kubebuilder:default="60s"
	GracefulShutdownTimeout *metav1.Duration `json:"gracefulShutdownTimeout,omitempty"`
}

// LifecycleHandler defines a specific action that should be taken
type LifecycleHandler struct {
	// Exec specifies a command to execute
	// +optional
	Exec *ExecAction `json:"exec,omitempty"`

	// HTTPGet specifies an HTTP GET request
	// +optional
	HTTPGet *HTTPGetAction `json:"httpGet,omitempty"`

	// Snapshot specifies a snapshot to create
	// +optional
	Snapshot *SnapshotAction `json:"snapshot,omitempty"`
}

// ExecAction describes a command to be executed
type ExecAction struct {
	// Command is the command line to execute
	// +kubebuilder:validation:MinItems=1
	Command []string `json:"command"`
}

// HTTPGetAction describes an HTTP GET request
type HTTPGetAction struct {
	// Path is the HTTP path to access
	Path string `json:"path,omitempty"`

	// Port is the port to access
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Host name to connect to (defaults to VM IP)
	// +optional
	Host string `json:"host,omitempty"`

	// Scheme to use for connecting (HTTP or HTTPS)
	// +optional
	// +kubebuilder:default="HTTP"
	// +kubebuilder:validation:Enum=HTTP;HTTPS
	Scheme string `json:"scheme,omitempty"`
}

// SnapshotAction describes a snapshot operation
type SnapshotAction struct {
	// Name is the name hint for the snapshot
	Name string `json:"name"`

	// IncludeMemory indicates whether to include memory state
	// +optional
	IncludeMemory bool `json:"includeMemory,omitempty"`

	// Description provides context for the snapshot
	// +optional
	Description string `json:"description,omitempty"`
}

// VirtualMachineResources defines resource overrides for a VM
type VirtualMachineResources struct {
	// CPU specifies the number of virtual CPUs
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	CPU *int32 `json:"cpu,omitempty"`

	// MemoryMiB specifies the amount of memory in MiB
	// +optional
	// +kubebuilder:validation:Minimum=128
	// +kubebuilder:validation:Maximum=1048576
	MemoryMiB *int64 `json:"memoryMiB,omitempty"`

	// GPU specifies GPU configuration
	// +optional
	GPU *GPUConfig `json:"gpu,omitempty"`
}

// GPUConfig defines GPU configuration for a VM
type GPUConfig struct {
	// Count specifies the number of GPUs to assign
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	Count int32 `json:"count"`

	// Type specifies the GPU type (provider-specific)
	// +optional
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9-_]+$"
	Type string `json:"type,omitempty"`

	// Memory specifies GPU memory in MiB
	// +optional
	// +kubebuilder:validation:Minimum=512
	Memory *int64 `json:"memory,omitempty"`
}

// VMSnapshotOperation defines snapshot operations in VM spec
type VMSnapshotOperation struct {
	// RevertToRef specifies a snapshot to revert to
	// +optional
	RevertToRef *LocalObjectReference `json:"revertToRef,omitempty"`
}

// VirtualMachineStatus defines the observed state of VirtualMachine.
type VirtualMachineStatus struct {
	// ID is the provider-specific identifier for this VM
	// +optional
	ID string `json:"id,omitempty"`

	// PowerState reflects the current power state
	// +optional
	PowerState PowerState `json:"powerState,omitempty"`

	// IPs contains the IP addresses assigned to the VM
	// +optional
	IPs []string `json:"ips,omitempty"`

	// ConsoleURL provides access to the VM console
	// +optional
	ConsoleURL string `json:"consoleURL,omitempty"`

	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastTaskRef references the last async operation
	// +optional
	LastTaskRef string `json:"lastTaskRef,omitempty"`

	// Provider contains provider-specific details
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Provider map[string]string `json:"provider,omitempty"`

	// ReconfigureTaskRef tracks reconfiguration operations
	// +optional
	ReconfigureTaskRef string `json:"reconfigureTaskRef,omitempty"`

	// LastReconfigureTime records when the last reconfiguration occurred
	// +optional
	LastReconfigureTime *metav1.Time `json:"lastReconfigureTime,omitempty"`

	// CurrentResources shows the current resource allocation
	// +optional
	CurrentResources *VirtualMachineResources `json:"currentResources,omitempty"`

	// Snapshots lists available snapshots for this VM
	// +optional
	Snapshots []VMSnapshotInfo `json:"snapshots,omitempty"`

	// Phase represents the current phase of the VM
	// +optional
	Phase VirtualMachinePhase `json:"phase,omitempty"`

	// Message provides additional details about the current state
	// +optional
	Message string `json:"message,omitempty"`
}

// VirtualMachinePhase represents the phase of a VM
// +kubebuilder:validation:Enum=Pending;Provisioning;Running;Stopped;Reconfiguring;Deleting;Failed
type VirtualMachinePhase string

const (
	// VirtualMachinePhasePending indicates the VM is waiting to be processed
	VirtualMachinePhasePending VirtualMachinePhase = "Pending"
	// VirtualMachinePhaseProvisioning indicates the VM is being created
	VirtualMachinePhaseProvisioning VirtualMachinePhase = "Provisioning"
	// VirtualMachinePhaseRunning indicates the VM is running
	VirtualMachinePhaseRunning VirtualMachinePhase = "Running"
	// VirtualMachinePhaseStopped indicates the VM is stopped
	VirtualMachinePhaseStopped VirtualMachinePhase = "Stopped"
	// VirtualMachinePhaseReconfiguring indicates the VM is being reconfigured
	VirtualMachinePhaseReconfiguring VirtualMachinePhase = "Reconfiguring"
	// VirtualMachinePhaseDeleting indicates the VM is being deleted
	VirtualMachinePhaseDeleting VirtualMachinePhase = "Deleting"
	// VirtualMachinePhaseFailed indicates the VM is in a failed state
	VirtualMachinePhaseFailed VirtualMachinePhase = "Failed"
)

// VMSnapshotInfo provides information about a VM snapshot
type VMSnapshotInfo struct {
	// ID is the provider-specific snapshot identifier
	ID string `json:"id"`

	// Name is the snapshot name
	Name string `json:"name"`

	// CreationTime is when the snapshot was created
	CreationTime metav1.Time `json:"creationTime"`

	// Description provides additional context
	// +optional
	Description string `json:"description,omitempty"`

	// SizeBytes is the size of the snapshot
	// +optional
	SizeBytes *int64 `json:"sizeBytes,omitempty"`

	// HasMemory indicates if memory state is included
	// +optional
	HasMemory bool `json:"hasMemory,omitempty"`
}

// LocalObjectReference represents a reference to an object in the same namespace
type LocalObjectReference struct {
	// Name of the referenced object
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ObjectRef represents a reference to another object
type ObjectRef struct {
	// Name of the referenced object
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace of the referenced object (defaults to current namespace)
	// +optional
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
}

// VMNetworkRef represents a reference to a network attachment
type VMNetworkRef struct {
	// Name is the name of this network attachment
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// NetworkRef references the VMNetworkAttachment
	NetworkRef ObjectRef `json:"networkRef"`

	// IPAddress specifies a static IP address (optional)
	// +optional
	// +kubebuilder:validation:Pattern="^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$"
	IPAddress string `json:"ipAddress,omitempty"`

	// MACAddress specifies a static MAC address (optional)
	// +optional
	// +kubebuilder:validation:Pattern="^([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})$"
	MACAddress string `json:"macAddress,omitempty"`
}

// DiskSpec defines a disk configuration
type DiskSpec struct {
	// Name is the disk identifier
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// SizeGiB is the size of the disk in GiB
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65536
	SizeGiB int32 `json:"sizeGiB"`

	// Type specifies the disk type (provider-specific)
	// +optional
	// +kubebuilder:default="thin"
	// +kubebuilder:validation:Enum=thin;thick;eagerzeroedthick;ssd;hdd
	Type string `json:"type,omitempty"`

	// ExpandPolicy defines how the disk can be expanded
	// +optional
	// +kubebuilder:default="Offline"
	// +kubebuilder:validation:Enum=Online;Offline
	ExpandPolicy string `json:"expandPolicy,omitempty"`

	// StorageClass specifies the storage class (optional)
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
}

// ImportedDiskRef references a disk that was imported via migration or other means.
// This allows VMs to be created from pre-existing disk images rather than templates.
type ImportedDiskRef struct {
	// DiskID is the provider-specific disk identifier.
	// For libvirt, this is typically the volume name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	DiskID string `json:"diskID"`

	// Path is the optional disk path on the provider (e.g., /var/lib/libvirt/images/disk.qcow2).
	// If not specified, the provider will determine the path based on DiskID.
	// +optional
	Path string `json:"path,omitempty"`

	// Format specifies the disk format (qcow2, vmdk, raw, etc.).
	// +optional
	// +kubebuilder:default="qcow2"
	// +kubebuilder:validation:Enum=qcow2;vmdk;raw;vdi;vhdx
	Format string `json:"format,omitempty"`

	// Source indicates where the disk came from.
	// +optional
	// +kubebuilder:validation:Enum=migration;clone;import;snapshot;manual
	Source string `json:"source,omitempty"`

	// MigrationRef references the VMMigration that imported this disk.
	// This provides traceability and audit trail for migrated disks.
	// +optional
	MigrationRef *LocalObjectReference `json:"migrationRef,omitempty"`

	// SizeGiB specifies the expected disk size in GiB.
	// Used for validation and capacity planning.
	// +optional
	// +kubebuilder:validation:Minimum=1
	SizeGiB int32 `json:"sizeGiB,omitempty"`
}

// UserData defines cloud-init configuration
type UserData struct {
	// CloudInit contains cloud-init configuration
	// +optional
	CloudInit *CloudInit `json:"cloudInit,omitempty"`

	// Ignition contains Ignition configuration for CoreOS/RHEL
	// +optional
	Ignition *Ignition `json:"ignition,omitempty"`
}

// CloudInit defines cloud-init configuration
type CloudInit struct {
	// Inline contains inline cloud-init data
	// +optional
	Inline string `json:"inline,omitempty"`

	// SecretRef references a Secret containing cloud-init data
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
}

// Ignition defines Ignition configuration
type Ignition struct {
	// Inline contains inline Ignition data
	// +optional
	Inline string `json:"inline,omitempty"`

	// SecretRef references a Secret containing Ignition data
	// +optional
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
}

// Placement provides hints for VM placement
type Placement struct {
	// Cluster specifies the target cluster
	// +optional
	Cluster string `json:"cluster,omitempty"`

	// Host specifies the target host
	// +optional
	Host string `json:"host,omitempty"`

	// Datastore specifies the target datastore
	// +optional
	Datastore string `json:"datastore,omitempty"`

	// Folder specifies the target folder
	// +optional
	Folder string `json:"folder,omitempty"`

	// ResourcePool specifies the target resource pool
	// +optional
	ResourcePool string `json:"resourcePool,omitempty"`
}

// VM condition types
const (
	// VirtualMachineConditionReady indicates whether the VM is ready
	VirtualMachineConditionReady = "Ready"
	// VirtualMachineConditionProvisioning indicates whether the VM is being provisioned
	VirtualMachineConditionProvisioning = "Provisioning"
	// VirtualMachineConditionReconfiguring indicates whether the VM is being reconfigured
	VirtualMachineConditionReconfiguring = "Reconfiguring"
	// VirtualMachineConditionDeleting indicates whether the VM is being deleted
	VirtualMachineConditionDeleting = "Deleting"
)

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerRef.name`
//+kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.classRef.name`
//+kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.imageRef.name`
//+kubebuilder:printcolumn:name="IPs",type=string,JSONPath=`.status.ips[*]`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
//+kubebuilder:storageversion

// VirtualMachine is the Schema for the virtualmachines API
type VirtualMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualMachineSpec   `json:"spec,omitempty"`
	Status VirtualMachineStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VirtualMachineList contains a list of VirtualMachine
type VirtualMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualMachine{}, &VirtualMachineList{})
}
