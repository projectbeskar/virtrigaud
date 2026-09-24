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
)

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
	// eliminated all of them). The condition message carries the per-host
	// breakdown.
	ReasonUnschedulable = "Unschedulable"
	// ReasonPlacementError indicates the scheduler rejected a malformed input the
	// admin must fix (an unparseable overcommit ratio or affinity selector) —
	// distinct from an ordinary no-fit.
	ReasonPlacementError = "PlacementError"
)

// ConditionPlaced is the single positive placement condition of a VirtualMachine
// on a clustered ("brain-in-operator") provider (ADR-0007 Addendum A, A2). True
// means the VM has a confirmed host binding (status.placement.host); False
// carries WHY per-VM calls cannot (yet) be routed to a host. It is never set on
// a VM of a single-host or thin-client provider (D9).
const ConditionPlaced = "Placed"

// Placed condition reasons (ADR-0007 Addendum A, A2). The vocabulary is fixed by
// the ADR: exactly these four.
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
)

// ReasonVMMissingOnHost indicates that a clustered VM's bound host reports the
// hypervisor VM does not exist (ADR-0007 Addendum A, A4). The operator does NOT
// re-create it — neither on the bound host nor elsewhere — because without
// fencing that risks two running copies of one disk (D8). An administrator must
// restore the VM, or delete and re-create the VirtualMachine.
const ReasonVMMissingOnHost = "VMMissingOnHost"

// SetCondition sets a condition on the given list of conditions
func SetCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string) {
	newCondition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
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

// SetReadyCondition sets the Ready condition
func SetReadyCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string) {
	SetCondition(conditions, ConditionReady, status, reason, message)
}

// SetProvisioningCondition sets the Provisioning condition
func SetProvisioningCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string) {
	SetCondition(conditions, ConditionProvisioning, status, reason, message)
}

// SetReconfiguringCondition sets the Reconfiguring condition
func SetReconfiguringCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string) {
	SetCondition(conditions, ConditionReconfiguring, status, reason, message)
}

// SetErrorCondition sets the Error condition
func SetErrorCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string) {
	SetCondition(conditions, ConditionError, status, reason, message)
}

// SetHealthyCondition sets the Healthy condition
func SetHealthyCondition(conditions *[]metav1.Condition, status metav1.ConditionStatus, reason, message string) {
	SetCondition(conditions, ConditionHealthy, status, reason, message)
}
