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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// setOperationIdentityReadiness records on provider whether what it advertises
// (capabilities 31-34, read as caps) matches the protection its type requires
// (operation.RequiredProtection), as its Ready condition:
//
//   - a type that requires operation identity: Ready=True/OperationIdentityMatched
//     when it advertises supports_ and requires_operation_identity and no
//     fencing and the manager has a signer; otherwise Ready=False with
//     OperationSignerUnavailable or OperationIdentityMismatch;
//   - any other type: Ready=False/OperationIdentityMismatch only while it
//     advertises any of the four gates (impossible for a built-in Provider,
//     which advertises none); a Ready condition this function set is removed
//     once it no longer does, and no other Provider gets one.
//
// The required protection is never lowered to match what is advertised.
func setOperationIdentityReadiness(provider *infravirtrigaudiov1beta1.Provider, caps contracts.Capabilities, signerLoaded bool) {
	required := operation.RequiredProtection(provider.Spec.Type)
	why := operationHintMismatch(required, caps)
	switch {
	case required == operation.ProtectionIdentity && why != "":
		k8s.SetReadyCondition(&provider.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationIdentityMismatch,
			why+"; the manager never lowers the protection it requires, and sends nothing to this Provider until it advertises a match",
			provider.Generation)
	case required == operation.ProtectionIdentity && !signerLoaded:
		k8s.SetReadyCondition(&provider.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationSignerUnavailable,
			msgOperationSignerUnavailable, provider.Generation)
	case required == operation.ProtectionIdentity:
		k8s.SetReadyCondition(&provider.Status.Conditions, metav1.ConditionTrue, k8s.ReasonOperationIdentityMatched,
			"the Provider advertises the operation identity its type requires", provider.Generation)
	case why != "":
		k8s.SetReadyCondition(&provider.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationIdentityMismatch,
			why+"; the manager sends it unsigned calls only, as for its type", provider.Generation)
	default:
		if c := meta.FindStatusCondition(provider.Status.Conditions, k8s.ConditionReady); c != nil &&
			c.Reason == k8s.ReasonOperationIdentityMismatch {
			meta.RemoveStatusCondition(&provider.Status.Conditions, k8s.ConditionReady)
		}
	}
}
