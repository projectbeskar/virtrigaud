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

package k8s

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Common condition types
const (
	// ConditionReady indicates the resource is ready for use
	ConditionReady = "Ready"
	// ConditionProvisioning indicates the resource is being provisioned
	ConditionProvisioning = "Provisioning"
	// ConditionReconfiguring indicates the resource is being reconfigured
	ConditionReconfiguring = "Reconfiguring"
	// ConditionDiskSizeWithinLimit is False (reason DiskSizeExceedsLimit or
	// DiskSizeLimitUnverifiable) on a VirtualMachine whose last Reconfigure
	// withheld the growth of a disk beyond the disk-size limit: the disk was
	// left as it is while the VM's other changes went ahead. It is set
	// whatever that Reconfigure's outcome (synchronous, an async task, a
	// change pending a restart, a failure) and removed by the next Reconfigure
	// that withholds nothing, or by a reconcile once the spec's sizes fit the
	// limit again. It is never set on a VM without a limit.
	ConditionDiskSizeWithinLimit = "DiskSizeWithinLimit"
	// ConditionError indicates an error condition
	ConditionError = "Error"
	// ConditionHealthy indicates the resource is healthy
	ConditionHealthy = "Healthy"
)

// Common condition reasons
const (
	// ReasonReconcileSuccess indicates successful reconciliation
	ReasonReconcileSuccess = "ReconcileSuccess"
	// ReasonReconcileError indicates reconciliation error
	ReasonReconcileError = "ReconcileError"
	// ReasonProviderError indicates provider-specific error
	ReasonProviderError = "ProviderError"
	// ReasonValidationError indicates validation error
	ReasonValidationError = "ValidationError"
	// ReasonCreating indicates resource is being created
	ReasonCreating = "Creating"
	// ReasonDeleting indicates resource is being deleted
	ReasonDeleting = "Deleting"
	// ReasonUpdating indicates resource is being updated
	ReasonUpdating = "Updating"
	// ReasonWaitingForDependencies indicates waiting for dependencies
	ReasonWaitingForDependencies = "WaitingForDependencies"
	// ReasonTaskInProgress indicates async task in progress
	ReasonTaskInProgress = "TaskInProgress"
	// ReasonProviderConflict indicates the provider refused to create the
	// resource because one with the same provider-side identity (e.g. a libvirt
	// domain name) already exists and is NOT owned by this object. It is never
	// resolved by retrying: an operator must adopt the existing resource through
	// the adoption flow, remove it, or rename this object.
	ReasonProviderConflict = "ProviderConflict"
	// ReasonDeleteBlocked indicates the provider refused to delete the
	// resource because other resources on the hypervisor still depend on it —
	// e.g. a libvirt linked clone whose disk is backed by this VM's disk.
	// Deleting it would destroy their data, so the finalizer is kept and the
	// delete is retried until the dependents are gone.
	ReasonDeleteBlocked = "DeleteBlocked"
	// ReasonPowerStateUnmanaged is Ready=False on a VirtualMachine whose
	// provider reports it Suspended (paused or suspended to RAM: still active)
	// or Unknown. The manager neither powers it on or off nor reconfigures it
	// until it is running or powered off.
	ReasonPowerStateUnmanaged = "PowerStateUnmanaged"
	// ReasonRestartRequired is Reconfiguring=True on a VirtualMachine whose
	// last Reconfigure the provider applied to the VM's persistent definition
	// only: the new size takes effect at the VM's next power cycle (power off,
	// then on). status.currentResources then holds, per resource, the larger
	// of the running size and the next-boot size.
	ReasonRestartRequired = "RestartRequired"
	// ReasonProviderLacksHonestReconfigure is Reconfiguring=False on a VM of a
	// clustered Provider that does not report supportsHonestReconfigure (an
	// older provider image): its Reconfigure could report a change applied that
	// was not, which the committed-capacity accounting would trust, so the
	// resize — grow or shrink — is not sent until the provider is upgraded.
	ReasonProviderLacksHonestReconfigure = "ProviderLacksHonestReconfigure"
)

// ConditionLinkedClonesDependOnDisk is True on a VirtualMachine whose disk other
// VMs on its hypervisor use as their backing file (libvirt linked clones made
// by an earlier release) — as the provider counted when the VM was last
// started. Powering the VM on while those clones are shut off corrupts them.
// It is a warning only: nothing is refused. Its reason is the same string.
const ConditionLinkedClonesDependOnDisk = "LinkedClonesDependOnDisk"

// Placement / scheduling condition reasons (ADR-0007 P1, D4). Surfaced on a
// VirtualMachine's Provisioning=False condition when the operator scheduler
// cannot bind the VM to a host on a clustered ("brain-in-operator") provider.
// Each names WHY placement could not proceed, so an operator can tell a
// misconfiguration (no/multiple pools, dangling policy) apart from a genuine
// capacity shortfall (no feasible host).
const (
	// ReasonNoHostPool indicates the clustered Provider has no HostPool in the
	// VM's namespace, so there is no candidate set to schedule into.
	ReasonNoHostPool = "NoHostPool"
	// ReasonMultipleHostPools indicates the clustered Provider has more than one
	// HostPool in the VM's namespace; v1 supports exactly one pool per clustered
	// provider (multi-pool selection is deferred), so the operator refuses to
	// pick one silently.
	ReasonMultipleHostPools = "MultipleHostPools"
	// ReasonPlacementPolicyNotFound indicates the VM's spec.placementRef points at
	// a VMPlacementPolicy that does not exist, so the scheduler inputs cannot be
	// resolved.
	ReasonPlacementPolicyNotFound = "PlacementPolicyNotFound"
	// ReasonUnschedulable indicates the scheduler found no feasible host for the
	// VM among the pool's candidates (capacity, visibility, or affinity filters
	// eliminated all of them). The condition message carries the per-category
	// tally and, when capacity was short, the committed-capacity arithmetic
	// (numbers only, no host or VM names). It is set on Provisioning=False and,
	// since the ADR-0007 scheduler-accuracy amendment, also on Placed=False.
	ReasonUnschedulable = "Unschedulable"
	// ReasonPlacementError indicates the scheduler rejected a malformed input the
	// admin must fix (an unparseable overcommit ratio or affinity selector) —
	// distinct from an ordinary no-fit.
	ReasonPlacementError = "PlacementError"
	// ReasonInsufficientHostCapacity is Reconfiguring=False on a VM of a
	// clustered Provider: a resize-up exceeds the free capacity of the VM's
	// host (ADR-0007 Addendum A, scheduler-accuracy amendment). The VM keeps
	// its current size and the resize is retried with a backoff; a shrink is
	// never refused.
	ReasonInsufficientHostCapacity = "InsufficientHostCapacity"
	// ReasonOrphanOnDeleteNotAllowed is Ready=False on a VirtualMachine being
	// deleted with virtrigaud.io/orphan-on-delete=true whose clustered
	// Provider is in another namespace and has not allowed consumers to
	// detach VMs: an orphaned VM would keep running on its host outside the
	// committed-capacity accounting. The finalizer is kept.
	ReasonOrphanOnDeleteNotAllowed = "OrphanOnDeleteNotAllowed"
	// ReasonForceDeleteNotAllowed is the reason of ConditionForceDeleteRefused
	// (and of its Warning event): virtrigaud.io/force-delete=true is set on a
	// VirtualMachine whose clustered Provider is in another namespace and has
	// not allowed consumers to leave VMs on its hosts. The force-delete is
	// ignored and the delete is retried as if it were not set.
	ReasonForceDeleteNotAllowed = "ForceDeleteNotAllowed"
	// ReasonProviderDeleting is Ready=False (and Provisioning=False) on a
	// VirtualMachine that is not created on the hypervisor yet — or whose
	// hypervisor VM is gone and would be re-created — while its Provider is
	// being deleted (a non-zero deletionTimestamp). Nothing is created through
	// a Provider on its way out: a new VM would keep its deletion waiting. The
	// VM is re-checked periodically and proceeds once the Provider is gone and
	// re-created, or is deleted itself.
	ReasonProviderDeleting = "ProviderDeleting"
	// ReasonCreateRequested is the Provisioning reason of a single-host
	// VirtualMachine with no status.id for which a provider Create was
	// requested: it is written, and confirmed, BEFORE the Create is sent, and
	// replaced by the Create's outcome once the provider answers and its id is
	// recorded. It is the durable record that the VM may exist on the
	// hypervisor although no id is recorded: such a VM is never released on
	// delete as if nothing had been created, and it counts as using its
	// Provider.
	ReasonCreateRequested = "CreateRequested"
	// ReasonCreateOutcomeUnknown is Ready=False on a single-host VirtualMachine
	// whose Create was requested (ReasonCreateRequested) but whose outcome was
	// never recorded, when the provider cannot tell whether that Create made a
	// VM: no second Create is sent (and, on delete, the finalizer is kept)
	// until an operator records the VM's id or confirms it was not created.
	ReasonCreateOutcomeUnknown = "CreateOutcomeUnknown"
	// ReasonCreateBackoff is Ready=False (and, when the provider performed
	// nothing, Provisioning=False) on a VirtualMachine whose Create the
	// provider answered with a retryable VM_DISK_CHECK_FAILED or
	// VM_OPERATION_FAILED: the Create is retried with a backoff (15 s
	// doubling to 5 min) counted from when Ready took this reason, instead of
	// every few seconds.
	ReasonCreateBackoff = "CreateBackoff"
	// ReasonCreateRefused is Ready=False and Provisioning=False on a
	// VirtualMachine whose Create the provider refused with a non-retryable
	// FailedPrecondition (a precondition only an operator can satisfy, e.g.
	// an imported disk that no longer exists) or PermissionDenied: nothing
	// was created. The Create is re-checked with a backoff (15 s doubling to
	// 5 min) counted from when Ready took this reason.
	ReasonCreateRefused = "CreateRefused"
	// ReasonPendingSizeGrew is Placed=False (and Provisioning=False) on a
	// clustered VM whose pending Create would now ask for more than the size
	// it was admitted at (status.placement.pendingResources), because its
	// VMClass grew in the meantime. The Create is not sent and the pending
	// host is kept; restoring the size lets it continue.
	ReasonPendingSizeGrew = "PendingSizeGrew"
	// ReasonShrinkPendingPowerOff is Reconfiguring=False on a running VM of a
	// clustered Provider whose spec asks for less CPU or memory than it holds.
	// A running guest can keep using what it has (a live vCPU unplug can
	// fail, a live memory change only moves the balloon target), so the shrink
	// is applied only once the VM is powered off; until then the VM counts at
	// its current size. The operator never powers the VM off by itself.
	ReasonShrinkPendingPowerOff = "ShrinkPendingPowerOff"
	// ReasonResizePendingPowerOff is Reconfiguring=False on a running VM whose
	// change the provider refused to apply to it while it runs and cannot
	// stage for its next boot (vSphere: CPU or memory hot-add — or CPU
	// hot-remove, for fewer vCPUs — is not enabled on the VM, the memory is
	// above its hot-plug limit, memory is never removed from a running VM, a
	// shared-bus or multi-writer disk is grown only while the VM is off, and
	// a suspended VM takes no change). Nothing was changed. The change is
	// applied by the next Reconfigure after the VM is powered off
	// (spec.powerState: Off); until then it is asked again on the VM's
	// reconfigure backoff, and at once after a spec change. The operator never
	// powers the VM off by itself.
	ReasonResizePendingPowerOff = "ResizePendingPowerOff"
	// ReasonDiskSizeExceedsLimit is Ready=False (and Provisioning=False) on a
	// VirtualMachine that is not created yet — or whose hypervisor VM is gone
	// and would be re-created — when one of the disks it asks for (VMClass
	// spec.diskDefaults.size, spec.disks[].sizeGiB) is above the disk-size
	// limit that applies to its Provider: the smaller of the manager's
	// --max-disk-size and the Provider's infra.virtrigaud.io/max-disk-size
	// annotation. Nothing is created: no image prepare, placement or Create
	// is sent. The VM is re-checked periodically and created once its sizes
	// fit. On a VM that exists it is the DiskSizeWithinLimit=False reason of a
	// Reconfigure that sent everything except the growth of a disk beyond the
	// limit (that disk is never grown past it, and never shrunk). It is also
	// the Ready=False reason of a pending VMClone, and the Validating=False,
	// Exporting=False or Importing=False reason of a VMMigration, held for the
	// same cause — a clone or migration by the source's real disk sizes.
	ReasonDiskSizeExceedsLimit = "DiskSizeExceedsLimit"
	// ReasonDiskSizeLimitUnverifiable is used like ReasonDiskSizeExceedsLimit
	// when the disk-size limit cannot be checked: the Provider's
	// infra.virtrigaud.io/max-disk-size annotation is not a valid limit, or a
	// VMClass whose size counts is missing. VirtRigaud fails closed: nothing is
	// created, cloned, migrated or grown until it can be checked.
	ReasonDiskSizeLimitUnverifiable = "DiskSizeLimitUnverifiable"
	// ReasonWaitingForImport is Provisioning=False (and Ready=False) on the
	// target VirtualMachine of a VMMigration into a clustered Provider
	// (ADR-0007 Addendum B, B1): spec.importedDisk names the migration
	// (migrationRef) but no disk yet (an empty diskID). The VM is placed — its
	// pending host reserves its capacity — but no Create is sent until the
	// migration has imported its disk to that host and recorded it in
	// spec.importedDisk.
	ReasonWaitingForImport = "WaitingForImport"
	// ReasonMigrationNotFound is Provisioning=False (and Ready=False) on a
	// VirtualMachine waiting for its import whose spec.importedDisk.migrationRef
	// names a VMMigration that does not exist in its namespace, or that did
	// not create this VirtualMachine (its recorded UID or claim). It is never
	// placed or created from a disk it cannot prove (fail closed); deleting
	// the VirtualMachine, or a migration that owns it, lifts the hold.
	ReasonMigrationNotFound = "MigrationNotFound"
	// ReasonImportedDiskPinned is Placed=False (and Provisioning=False) on a
	// clustered migration target whose Create on its pending host was refused
	// with a name conflict after its migration imported the disk to that host
	// (ADR-0007 Addendum B, B1.2): the host is pinned — the VM is neither
	// excluded from it nor re-scheduled, because the landed disk exists only
	// there. An operator resolves the conflict on the host; the Create is
	// re-checked with a backoff.
	ReasonImportedDiskPinned = "ImportedDiskPinned"
)

// ConditionPlaced is the single positive placement condition of a VirtualMachine
// on a clustered ("brain-in-operator") provider (ADR-0007 Addendum A, A2). True
// means the VM has a confirmed host binding (status.placement.host); False
// carries WHY per-VM calls cannot (yet) be routed to a host. It is never set on
// a VM of a single-host or thin-client provider (D9).
const ConditionPlaced = "Placed"

// Placed condition reasons (ADR-0007 Addendum A, A2). The vocabulary is fixed by
// the ADR: these four, plus the two the A2 amendment (slice 2) adds for a Create
// refused with a name conflict (ReasonHostExcluded, ReasonAllHostsExcluded),
// plus ReasonUnschedulable (declared with the scheduling reasons above) from the
// scheduler-accuracy amendment in A5, plus ReasonRestorePending from A6,
// ReasonOwnDomainOnAnotherHost (declared with the DeleteBlocked reasons below)
// from A6.1, and ReasonProviderLacksListOwnerFilter and
// ReasonUniquenessCheckFailed from A6.2 (the pre-schedule uniqueness check).
const (
	// ReasonBound is Placed=True: the provider confirmed the VM on the host named
	// by status.placement.host, and every per-VM call is routed there.
	ReasonBound = "Bound"
	// ReasonCreatePending is Placed=False: the operator recorded the attempted
	// host in status.placement.pendingHost and the provider has not yet confirmed
	// the Create there. A retry reuses the same host; the scheduler is not re-run.
	ReasonCreatePending = "CreatePending"
	// ReasonHostUnavailable is Placed=False: the pending host is unreachable (the
	// provider keeps answering Create with Unavailable). The VM is NEVER
	// re-scheduled automatically, because a domain may already exist there; it
	// waits for the host to return or for an administrator to clear pendingHost.
	ReasonHostUnavailable = "HostUnavailable"
	// ReasonUnbound is Placed=False: the VM has a provider id but no confirmed
	// host binding (e.g. its status was lost in a backup restore). No per-VM call
	// is sent — the provider is never allowed to pick a host.
	ReasonUnbound = "Unbound"
	// ReasonHostExcluded is Placed=False: the Create on the pending host was
	// refused with a name conflict (a same-named domain this VM does not own is
	// there, so this VM has no domain on it and the attempt created nothing).
	// The host was added to
	// status.placement.excludedHosts, pendingHost was cleared, and the VM is
	// re-scheduled onto another host.
	ReasonHostExcluded = "HostExcluded"
	// ReasonAllHostsExcluded is Placed=False: every candidate host of the pool
	// is in status.placement.excludedHosts. The VM stays unplaced and is only
	// re-checked slowly until an administrator resolves the name conflicts (or
	// renames the VM) and clears the list.
	ReasonAllHostsExcluded = "AllHostsExcluded"
	// ReasonRestorePending is Placed=False (and Provisioning=False): the VM is
	// held because a previous incarnation of it — a domain VirtRigaud created
	// for the same namespace and name under another UID, left by
	// orphan-on-delete, a force-delete or a backup restore — exists on a host
	// of its clustered Provider (ADR-0007 A6). Nothing is created until an
	// administrator re-attaches or removes that domain (the A6 runbook). In
	// A6.1 it is set when the provider answers a Create or Clone with
	// VM_PREVIOUS_INCARNATION (R2): the VM keeps its pendingHost, the host is
	// NOT excluded, and it is re-checked with a backoff (15 s doubling to
	// 5 min). When the domain found is the VM's OWN (stamped with its UID),
	// the reason is ReasonOwnDomainOnAnotherHost instead. Since A6.2 it is
	// also set BEFORE a VM is first scheduled (no status.id, no pendingHost):
	// by R1, when the VM's restore marker (infra.virtrigaud.io/placement-uid)
	// names another UID, and by R4, when the pre-schedule check finds a domain
	// stamped with the VM's namespace and name under another UID (or more than
	// one, or one whose stamp cannot be read). Nothing is scheduled or
	// created, with the same backoff. On a bound VM whose marker names another
	// UID (its status was restored) it replaces VMMissingOnHost on Ready.
	ReasonRestorePending = "RestorePending"
	// ReasonProviderLacksListOwnerFilter is Placed=False (and
	// Provisioning=False) on a clustered VM that has never been placed: its
	// Provider does not report supportsListOwnerFilter (an older provider
	// image), or answered the owner-filtered ListVMs without applying the
	// filter, so the pre-schedule uniqueness check (ADR-0007 A6.2, R4) cannot
	// run. The VM is not scheduled or created until the provider is upgraded.
	ReasonProviderLacksListOwnerFilter = "ProviderLacksListOwnerFilter"
	// ReasonUniquenessCheckFailed is Placed=False (and Provisioning=False) on
	// a clustered VM that has never been placed: the pre-schedule uniqueness
	// check (ADR-0007 A6.2, R4) could not ask the Provider's hosts (its
	// capabilities or its owner-filtered ListVMs failed). Nothing is scheduled
	// or created; the check is retried with a backoff (15 s doubling to
	// 5 min). A host that could not be reached does not cause it: the check
	// proceeds on what the reachable hosts report (A6 decision 4).
	ReasonUniquenessCheckFailed = "UniquenessCheckFailed"
)

// ConditionDeleteBlocked is True while the deletion of a clustered
// VirtualMachine is held because the provider could not delete it safely yet
// (ADR-0007 A6.1), or because the manager could not reach its Provider at all.
// Its reason says why (ReasonHostUnreachable, ReasonDiskCheckFailed,
// ReasonOwnDomainOnAnotherHost, ReasonDiskInUse, ReasonProviderUnavailable);
// it is removed when a later delete attempt fails for another reason (Ready
// then says ReasonProviderError). Ready is False with ReasonDeleteBlocked
// meanwhile. Its LastTransitionTime is when the hold began, which paces the
// retries (15 s doubling to 5 min). Its message never names a host.
const ConditionDeleteBlocked = "DeleteBlocked"

// ConditionForceDeleteRefused is True, with ReasonForceDeleteNotAllowed, on a
// VirtualMachine being deleted whose virtrigaud.io/force-delete=true is not
// honoured: it is a consumer, in another namespace, of a clustered Provider
// that does not carry infra.virtrigaud.io/allow-consumer-orphan-on-delete:
// "true". Releasing it without a confirmed provider delete would leave its
// domain running on the shared hosts outside the committed-capacity
// accounting. The delete is retried as if force-delete were not set (Ready
// and DeleteBlocked say why it waits); the condition is removed when the
// annotation is removed or the Provider allows it. Its message is constant.
const ConditionForceDeleteRefused = "ForceDeleteRefused"

// DeleteBlocked condition reasons (ADR-0007 A6.1).
const (
	// ReasonHostUnreachable: a host the delete needs — the VM's own, or
	// another host of the Provider that must be checked before the VM's disk
	// is removed — could not be reached. Nothing was deleted.
	ReasonHostUnreachable = "HostUnreachable"
	// ReasonDiskCheckFailed: the provider could not verify that no VM on
	// another host of the Provider uses the VM's disk. Nothing was deleted.
	ReasonDiskCheckFailed = "DiskCheckFailed"
	// ReasonOwnDomainOnAnotherHost: the VM's last create (or its clone) was
	// answered with its OWN domain on another host of the Provider (its
	// placement record was lost). It is the Placed (and Provisioning) reason
	// of that hold — Placed=False, pendingHost kept, as for ReasonRestorePending
	// — and, once the VM is deleted, the DeleteBlocked reason: deleting it on
	// its pending host would leave that domain running, so the finalizer is
	// kept until the pending host points at it.
	ReasonOwnDomainOnAnotherHost = "OwnDomainOnAnotherHost"
	// ReasonDiskInUse: the provider refused the delete because another VM —
	// on the VM's own host (e.g. a linked clone of it) or on another host of
	// the Provider — uses its disk (VM_DISK_IN_USE). Nothing was deleted.
	ReasonDiskInUse = "DiskInUse"
	// ReasonProviderUnavailable: the VM's Provider exists, but the manager has
	// no usable client for it — its provider runtime is not Running or has no
	// endpoint, or the TLS configuration, gRPC client or validation call
	// failed (as during every provider rollout or restart). No Delete was
	// sent, so the hypervisor VM may still exist, and the finalizer is kept
	// until the Provider answers. It is the DeleteBlocked reason of a clustered
	// VM and the Ready reason of a single-host VM held for it (a single-host VM
	// never carries DeleteBlocked).
	ReasonProviderUnavailable = "ProviderUnavailable"
	// ReasonVMIsImageArtifact: the provider refused a Delete or a power
	// change because the hypervisor object the VirtualMachine is bound to is
	// a template or a VirtRigaud image artifact, not an ordinary VM
	// (VM_IS_IMAGE_ARTIFACT); acting on it would destroy or change the image
	// for every VM cloned from it. Nothing was changed. It is the Ready reason
	// of a single-host VM held for it (delete or power) and the DeleteBlocked
	// reason of a clustered one. The hold lasts until the VirtualMachine is
	// detached with virtrigaud.io/orphan-on-delete=true.
	ReasonVMIsImageArtifact = "VMIsImageArtifact"
	// ReasonOwnerStampUnreadable: the provider refused a Delete (or, single-host
	// Proxmox, a Describe, power change or reconfigure) because the
	// VM's owner stamp cannot be read or records more than one owner
	// (OWNER_STAMP_UNREADABLE), so who owns it cannot be told. Nothing was
	// changed. It is the Ready reason of a single-host VM held for it and the
	// DeleteBlocked reason of a clustered one; the hold lasts until an
	// administrator fixes the stamp or removes the VM, or the VirtualMachine
	// is detached.
	ReasonOwnerStampUnreadable = "OwnerStampUnreadable"
)

// ReasonPlacementTopologyMismatch indicates that a VirtualMachine records a
// clustered placement (status.placement.host / .pendingHost) but its Provider
// is not topology: cluster (ADR-0007 Addendum A). No provider call is made for
// it: the single-host path neither routes to the recorded host nor checks
// ownership. Provider topology is immutable, so this only affects objects
// edited before that rule existed.
const ReasonPlacementTopologyMismatch = "PlacementTopologyMismatch"

// ReasonProviderRefMismatch indicates that a bound VirtualMachine's
// spec.providerRef no longer resolves to the Provider it is bound through
// (status.boundProvider: namespace, name and, when recorded, UID). Its
// status.id is meaningful only on that Provider's hypervisor — on another one
// it may name an unrelated VM — so no provider call is made for it, and its
// finalizer does not delete through the mismatched Provider. spec.providerRef
// is immutable once bound, so this only affects objects edited before that
// rule existed, or a Provider deleted and re-created under the same name.
const ReasonProviderRefMismatch = "ProviderRefMismatch"

// ReasonConsumerNotAllowed indicates that an object references a Provider,
// VMClass or VMImage in ANOTHER namespace whose spec.consumerNamespaceSelector
// is unset or does not select the referencing namespace. No provider call is
// made through, or for, the reference while it holds; it is lifted by whoever
// can set the referenced object's selector (or label the referencing
// namespace so an existing selector matches it).
const ReasonConsumerNotAllowed = "ConsumerNotAllowed"

// ReasonVMMissingOnHost indicates that a clustered VM's bound host reports the
// hypervisor VM does not exist (ADR-0007 Addendum A, A4). The operator does NOT
// re-create it — neither on the bound host nor elsewhere — because without
// fencing that risks two running copies of one disk (D8). An administrator must
// restore the VM, or delete and re-create the VirtualMachine.
const ReasonVMMissingOnHost = "VMMissingOnHost"

// ReasonVMNotOwned indicates that a single-host provider answered a
// VirtualMachine's Describe, power change or reconfigure with VM_NOT_OWNED: a
// VM exists at the VirtualMachine's id but is not its own (it carries another
// owner's stamp, or it is unstamped and named differently — e.g. a Proxmox
// VMID reused after the VirtualMachine's VM was removed outside VirtRigaud).
// Nothing of that VM was read or changed, and no VM is created in its place;
// the VirtualMachine is held until it is deleted (which leaves that VM
// untouched) or the provider recognizes the VM as its own again.
const ReasonVMNotOwned = "VMNotOwned"

// SetCondition sets the condition conditionType on conditions, recording
// observedGeneration — the generation of the object the conditions belong to
// (its metadata.generation), as the spec this condition reflects.
//
// LastTransitionTime is set when the condition is added and when its status
// or reason changes; a change of the message alone keeps it. A repeated call
// with the same values (and generation) changes nothing.
func SetCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string,
	observedGeneration int64) {
	newCondition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: observedGeneration,
	}

	for i, existing := range *conditions {
		if existing.Type == conditionType {
			// Update existing condition only if status or reason changed
			if existing.Status != status || existing.Reason != reason {
				newCondition.LastTransitionTime = metav1.Now()
			} else {
				newCondition.LastTransitionTime = existing.LastTransitionTime
			}
			(*conditions)[i] = newCondition
			return
		}
	}

	// Add new condition
	*conditions = append(*conditions, newCondition)
}

// GetCondition returns the condition with the given type
func GetCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i, condition := range conditions {
		if condition.Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// IsConditionTrue returns true if the condition is present and true
func IsConditionTrue(conditions []metav1.Condition, conditionType string) bool {
	condition := GetCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// IsConditionFalse returns true if the condition is present and false
func IsConditionFalse(conditions []metav1.Condition, conditionType string) bool {
	condition := GetCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionFalse
}

// IsConditionUnknown returns true if the condition is present and unknown
func IsConditionUnknown(conditions []metav1.Condition, conditionType string) bool {
	condition := GetCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionUnknown
}

// SetReadyCondition sets the Ready condition (see SetCondition).
func SetReadyCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string,
	observedGeneration int64) {
	SetCondition(conditions, ConditionReady, status, reason, message, observedGeneration)
}

// SetProvisioningCondition sets the Provisioning condition (see SetCondition).
func SetProvisioningCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string,
	observedGeneration int64) {
	SetCondition(conditions, ConditionProvisioning, status, reason, message, observedGeneration)
}

// SetReconfiguringCondition sets the Reconfiguring condition (see SetCondition).
func SetReconfiguringCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string,
	observedGeneration int64) {
	SetCondition(conditions, ConditionReconfiguring, status, reason, message, observedGeneration)
}

// SetErrorCondition sets the Error condition (see SetCondition).
func SetErrorCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string,
	observedGeneration int64) {
	SetCondition(conditions, ConditionError, status, reason, message, observedGeneration)
}

// SetHealthyCondition sets the Healthy condition (see SetCondition).
func SetHealthyCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string,
	observedGeneration int64) {
	SetCondition(conditions, ConditionHealthy, status, reason, message, observedGeneration)
}
