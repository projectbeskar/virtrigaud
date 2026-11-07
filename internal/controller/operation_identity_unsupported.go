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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Operations without a signed path (development builds only). A Provider that
// requires operation identity is sent the VM lifecycle kinds (Create, Delete,
// the power operations, Reconfigure) as signed operations. Every other
// mutating kind of the provider contract (hardware upgrade, snapshot create,
// delete and revert, clone, image prepare, disk export and import, staged and
// landed disk deletion, adoption) has no signed path in the manager: for such
// a Provider it is refused locally, before any client is resolved, so nothing
// is sent, signed or unsigned, and the refusal is surfaced as
// Ready=False/OperationIdentityUnsupported with a fixed message and one
// Warning event when it begins. The Provider client refuses the same calls
// again (contracts.ErrOperationIdentityNotImplemented) as a second lock.

// Operations refused for a Provider that requires operation identity, as
// named in the refusal's message.
const (
	identityUnsupportedSnapshot     = "a VM snapshot (create, delete or revert)"
	identityUnsupportedClone        = "a VM clone"
	identityUnsupportedImagePrepare = "an image prepare"
	identityUnsupportedMigration    = "a VM migration (snapshot, disk export and import, staged and landed disk deletion)"
	identityUnsupportedAdoption     = "a VM adoption (owner transfer)"
)

// errOperationIdentityUnsupported is returned, without resolving a client,
// when an operation with no signed path is asked of a Provider that requires
// operation identity.
var errOperationIdentityUnsupported = fmt.Errorf("operation identity unsupported: %w", contracts.ErrOperationIdentityNotImplemented)

// isOperationIdentityUnsupported reports whether err is that refusal.
func isOperationIdentityUnsupported(err error) bool {
	return errors.Is(err, errOperationIdentityUnsupported)
}

// refuseUnsupportedForIdentity returns errOperationIdentityUnsupported for a
// Provider that requires operation identity, and nil for any other: the guard
// of a controller's provider-client lookup for operations with no signed path.
func refuseUnsupportedForIdentity(provider *infravirtrigaudiov1beta1.Provider, op string) error {
	if identityRequired(provider) {
		return fmt.Errorf("%s through Provider %s/%s: %w", op, provider.Namespace, provider.Name, errOperationIdentityUnsupported)
	}
	return nil
}

// identityUnsupportedMessage is the fixed message of the refusal of op.
func identityUnsupportedMessage(op string) string {
	return fmt.Sprintf("%s has no signed path, and the Provider requires operation identity: it is refused and nothing is sent to the "+
		"Provider, signed or unsigned (docs/operation-grants-manager.md)", op)
}

// markIdentityUnsupported records the refusal of op on obj's conditions
// (conditionType=False, reason OperationIdentityUnsupported, the fixed
// message) and, when the refusal begins, one Warning event (recorder may be
// nil). It reports whether the condition changed.
func markIdentityUnsupported(recorder record.EventRecorder, obj runtime.Object, conds *[]metav1.Condition, conditionType string,
	generation int64, op string) bool {
	msg := identityUnsupportedMessage(op)
	prev := meta.FindStatusCondition(*conds, conditionType)
	if prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == k8s.ReasonOperationIdentityUnsupported && prev.Message == msg &&
		prev.ObservedGeneration == generation {
		return false
	}
	if recorder != nil && (prev == nil || prev.Reason != k8s.ReasonOperationIdentityUnsupported) {
		recorder.Event(obj, corev1.EventTypeWarning, k8s.ReasonOperationIdentityUnsupported, msg)
	}
	meta.SetStatusCondition(conds, metav1.Condition{
		Type: conditionType, Status: metav1.ConditionFalse, Reason: k8s.ReasonOperationIdentityUnsupported,
		Message: msg, ObservedGeneration: generation,
	})
	return true
}
