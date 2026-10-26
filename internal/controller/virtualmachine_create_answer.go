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
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// The record of a provider's answer to a Create that failed.
//
// A Create the provider answered with a failure — a refusal, an answer that
// it did not perform the Create (CREATE_NOT_PERFORMED), a failure on its host
// — is recorded on the VirtualMachine's conditions: for an answer whose
// outcome is known (createOutcomeKnown), Provisioning replaces the create
// marker. That status write used to be a plain update under the
// resourceVersion the reconcile read, so any edit of the VirtualMachine made
// while the provider ran the Create (a label, a spec change, another
// controller's metadata write) made it fail with a Conflict, and the answer
// was lost: the marker stayed, and a delete that followed was held as
// CreateOutcomeUnknown for an operator although the provider had answered
// that nothing was made (v0.4.1-rc2 lab, B2).
//
// writeCreateAnswer therefore re-applies the answer to the live object after
// a Conflict, as persistSnapshotRevert does for a revert's answer: only the
// conditions the answer sets (createAnswerConditionTypes), and only while the
// stored VirtualMachine still waits for that answer (createAttempt.awaitedBy:
// no id recorded, the create marker of the attempt the Create was sent under,
// the pending host it was sent to). Anything newer — an id, another attempt's
// marker, another pending host — is never overwritten: the next reconcile
// works from it. The conditions keep the ObservedGeneration of the spec the
// Create was built from, and status.observedGeneration keeps its stored value,
// so a spec edit made during the Create is still seen as not yet observed.

// createAnswerConditionTypes are the VirtualMachine conditions the answer to a
// failed Create sets: Ready, Provisioning (which replaces the create marker
// when the outcome is known) and, for a clustered Create, Placed.
var createAnswerConditionTypes = []string{k8s.ConditionReady, k8s.ConditionProvisioning, k8s.ConditionPlaced}

// awaitedBy reports whether live — the VirtualMachine as stored — still waits
// for the answer to the Create of attempt: no id is recorded, its create
// marker is the one the Create was sent under (none for a clustered Create),
// and its pending host is the host the Create was sent to (none for a
// single-host Create).
func (a createAttempt) awaitedBy(live *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return live.Status.ID == "" && markerAttemptID(live) == a.sentMarkerID && pendingHost(live) == a.sentHost
}

// putCondition sets c in conds as it is, LastTransitionTime included,
// replacing the condition of its type or appending it.
func putCondition(conds *[]metav1.Condition, c metav1.Condition) {
	for i := range *conds {
		if (*conds)[i].Type == c.Type {
			(*conds)[i] = c
			return
		}
	}
	*conds = append(*conds, c)
}

// writeCreateAnswer writes vm's status after the provider answered the Create
// of attempt with a failure (see the file comment). The first write is the
// usual one, under vm's resourceVersion. On a Conflict the VirtualMachine is
// re-read live and, while it still waits for this answer
// (attempt.awaitedBy), the answer's conditions as vm carries them are written
// over the stored status (retry.RetryOnConflict); every other field keeps its
// stored value. A VirtualMachine that is gone, replaced or no longer waiting
// for this answer gets nothing. Any other failure is logged, as updateStatus
// does. On a re-applied write vm takes the written status and
// resourceVersion.
func (r *VirtualMachineReconciler) writeCreateAnswer(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	attempt createAttempt,
) {
	logger := log.FromContext(ctx)
	err := r.writeStatusChecked(ctx, vm)
	if err == nil {
		return
	}
	if !apierrors.IsConflict(err) {
		logger.Error(err, "Failed to update VirtualMachine status")
		return
	}
	var answer []metav1.Condition
	for _, t := range createAnswerConditionTypes {
		if c := meta.FindStatusCondition(vm.Status.Conditions, t); c != nil {
			answer = append(answer, *c.DeepCopy())
		}
	}
	var written *infravirtrigaudiov1beta1.VirtualMachine
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		written = nil
		live, gone, err := r.readLiveVM(ctx, vm)
		if err != nil {
			return err
		}
		if gone {
			logger.Info("The VirtualMachine was deleted or replaced before the provider's answer to its Create could be recorded")
			return nil
		}
		if !attempt.awaitedBy(live) {
			logger.Info("The VirtualMachine records something newer than this Create's answer; not overwriting it",
				"id", live.Status.ID, "attempt", markerAttemptID(live), "pendingHost", pendingHost(live))
			return nil
		}
		for _, c := range answer {
			putCondition(&live.Status.Conditions, c)
		}
		if err := r.writeStatusChecked(ctx, live); err != nil {
			return err
		}
		written = live
		return nil
	})
	if err != nil {
		logger.Error(err, "Failed to record the provider's answer to the Create after a conflict")
		return
	}
	if written != nil {
		logger.V(1).Info("Recorded the provider's answer to the Create on the VirtualMachine as stored (its first write conflicted)")
		vm.ResourceVersion = written.ResourceVersion
		vm.Status = written.Status
	}
}
