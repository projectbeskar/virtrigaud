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
)

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
