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
	stderrors "errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	k8s "github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// Source power-off before a migration (spec.source.powerOffBeforeMigration)
// has exactly ONE actor: the VirtualMachine controller. The VMMigration
// controller sets the source VirtualMachine's desired power state
// (spec.powerState) to Off and then only WAITS — with read-only Describe calls
// — until the source is observed Off, bounded by
// migrationSourcePowerOffTimeout. It never calls Power itself: when it did
// (v0.4.0-rc1 lab, F3), its hard power-off raced the VirtualMachine
// controller's own power-off of the same VM, the second `virsh destroy` failed
// on an already-stopped domain, and the migration failed terminally although
// the source was off. The source is not powered back on by the migration (its
// spec.powerState stays Off), as before.

// migrationPowerOffPollInterval is the requeue cadence while waiting for a
// source VM to reach the Off power state when source.powerOffBeforeMigration is
// set (the export is gated on it).
const migrationPowerOffPollInterval = 5 * time.Second

// migrationSourcePowerOffTimeout bounds how long a migration waits for its
// source VM to be powered off by the VirtualMachine controller. It is measured
// from the SourcePowerOff condition's transition to
// reasonWaitingForSourcePowerOff — a condition only this wait writes, so
// other checks of the Validating phase rewriting the Validating condition
// (a provider not ready, a binding or mount wait) never restart it — and
// survives requeues and manager restarts. A hard power-off takes seconds; the
// budget absorbs a provider that is briefly unavailable and the
// VirtualMachine controller's retry backoff.
const migrationSourcePowerOffTimeout = 10 * time.Minute

// migrationConditionSourcePowerOff is the VMMigration condition recording the
// source power-off gate of spec.source.powerOffBeforeMigration: False with
// reasonWaitingForSourcePowerOff from the first wait (its transition time
// starts the budget), True with reasonSourcePoweredOff once the source is
// observed Off. It is removed when the migration (re)starts Validating from
// Pending (handlePendingPhase), so a retried migration gets a fresh budget.
const migrationConditionSourcePowerOff = "SourcePowerOff"

// reasonSourcePoweredOff is the SourcePowerOff condition reason once the
// source VM is observed Off.
const reasonSourcePoweredOff = "SourcePoweredOff"

// sourcePoweredOffMessage is the constant message of SourcePowerOff=True.
const sourcePoweredOffMessage = "The source VM is powered off"

// reasonWaitingForSourcePowerOff is the Validating condition reason (and the
// event reason) of a migration waiting for its source VM to be powered off.
const reasonWaitingForSourcePowerOff = "WaitingForSourcePowerOff"

// waitingForSourcePowerOffMessage is the constant Validating condition and
// status message of a migration waiting for its source VM to be powered off.
const waitingForSourcePowerOffMessage = "Waiting for the source VM to be powered off before migration " +
	"(its spec.powerState is set to Off; the VirtualMachine controller powers it off)"

// sourcePowerOffTimedOutMessage is the constant failure message of a migration
// whose source VM was not observed powered off within
// migrationSourcePowerOffTimeout.
var sourcePowerOffTimedOutMessage = fmt.Sprintf(
	"Failed to power off source VM before migration: the source VM was not observed powered off within %s; "+
		"check the source VirtualMachine's Ready condition and events (its spec.powerState is Off)",
	migrationSourcePowerOffTimeout)

// errSourcePowerOffTimedOut is returned by ensureSourcePoweredOff when the
// source VM was not observed Off within migrationSourcePowerOffTimeout.
var errSourcePowerOffTimedOut = stderrors.New("timed out waiting for the source VM to be powered off")

// ensureSourcePoweredOff gates the export on a powered-off source when
// spec.source.powerOffBeforeMigration is set, returning done=true only once a
// live, read-only Describe reports the source Off. A stopped source yields a
// quiescent disk and, for a vSphere source, is REQUIRED — ESXi locks a running
// VM's disk so it cannot be cloned to streamOptimized (#236, Bug H).
//
// It sets the source VM's desired power state (spec.powerState) to Off, which
// also keeps the VirtualMachine reconciler from powering the source back on
// mid-export, and leaves the power-off itself to that reconciler (a hard
// power-off: reliable, no guest-agent dependency). While the source is not yet
// Off it records Validating=False/WaitingForSourcePowerOff (status written only
// on change) and returns done=false with a RequeueAfter; a failed Describe or
// an unresolvable provider instance is waited out the same way. After
// migrationSourcePowerOffTimeout it returns errSourcePowerOffTimedOut. Errors
// resolving the source VM or its Provider are returned as before (the caller
// fails the migration or marks it consumer-refused).
func (r *VMMigrationReconciler) ensureSourcePoweredOff(ctx context.Context, migration *infrav1beta1.VMMigration) (bool, ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	sourceVM, err := r.getSourceVM(ctx, migration)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("get source VM: %w", err)
	}
	if sourceVM.Status.ID == "" {
		return false, ctrl.Result{}, fmt.Errorf("source VM %s has no provider ID yet", sourceVM.Name)
	}

	// The source is observed only through the Provider it is bound through
	// (resolved before any side effect, including the spec patch below).
	sourceProvider, err := r.sourceProviderFor(ctx, migration, sourceVM)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("resolve source provider: %w", err)
	}

	// Align the source VM's desired power state with the migration intent: the
	// VirtualMachine reconciler powers it off, and does not race the export by
	// powering it back on (which can re-lock a vSphere disk and would, for a
	// non-deleted source, leave the origin VM running after the move). This
	// patch is idempotent.
	if sourceVM.Spec.PowerState != infrav1beta1.PowerStateOff {
		patch := client.MergeFrom(sourceVM.DeepCopy())
		sourceVM.Spec.PowerState = infrav1beta1.PowerStateOff
		if err := r.Patch(ctx, sourceVM, patch); err != nil {
			return false, ctrl.Result{}, fmt.Errorf("set source VM %s desired power state to Off: %w", sourceVM.Name, err)
		}
		logger.Info("Set source VM desired power state to Off for migration", "vm", sourceVM.Name)
	}

	ref, err := vmRefFor(sourceVM, sourceProvider)
	if err != nil {
		res, werr := r.waitForSourceBinding(ctx, migration, err)
		return false, res, werr
	}

	// A read-only Describe is the authority: the source's recorded
	// status.powerState may lag the hypervisor in either direction.
	providerInstance, err := r.getProviderInstance(ctx, sourceProvider)
	if err == nil {
		var desc contracts.DescribeResponse
		desc, err = providerInstance.Describe(ctx, ref)
		if err == nil && desc.PowerState == string(contracts.PowerStateOff) {
			logger.Info("Source VM is powered off; proceeding with migration", "vm", sourceVM.Name)
			// Recorded with the caller's status write (the phase advances).
			k8s.SetCondition(&migration.Status.Conditions, migrationConditionSourcePowerOff,
				metav1.ConditionTrue, reasonSourcePoweredOff, sourcePoweredOffMessage, migration.Generation)
			return true, ctrl.Result{}, nil
		}
	}
	if err != nil {
		// Transient (provider restarting, host briefly unreachable): keep
		// waiting within the budget; never fail the migration for it alone.
		logger.Info("Could not observe the source VM's power state; waiting", "vm", sourceVM.Name, "error", err.Error())
	}

	return r.waitForSourcePowerOff(ctx, migration)
}

// waitForSourcePowerOff records that migration waits for its source VM to be
// powered off — SourcePowerOff=False and Validating=False, both
// WaitingForSourcePowerOff with a constant message, an event when the wait
// starts only, and a status write only when something changed — and requeues,
// or returns errSourcePowerOffTimedOut once the wait (measured from the
// SourcePowerOff condition) has lasted migrationSourcePowerOffTimeout.
func (r *VMMigrationReconciler) waitForSourcePowerOff(ctx context.Context, migration *infrav1beta1.VMMigration) (bool, ctrl.Result, error) {
	prev := meta.FindStatusCondition(migration.Status.Conditions, migrationConditionSourcePowerOff)
	waiting := prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == reasonWaitingForSourcePowerOff
	if waiting && time.Since(prev.LastTransitionTime.Time) > migrationSourcePowerOffTimeout {
		return false, ctrl.Result{}, errSourcePowerOffTimedOut
	}

	before := migration.Status.DeepCopy()
	k8s.SetCondition(&migration.Status.Conditions, migrationConditionSourcePowerOff,
		metav1.ConditionFalse, reasonWaitingForSourcePowerOff, waitingForSourcePowerOffMessage, migration.Generation)
	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionValidating,
		metav1.ConditionFalse, reasonWaitingForSourcePowerOff, waitingForSourcePowerOffMessage, migration.Generation)
	migration.Status.Message = waitingForSourcePowerOffMessage
	if !waiting {
		r.Recorder.Event(migration, corev1.EventTypeNormal, reasonWaitingForSourcePowerOff, waitingForSourcePowerOffMessage)
	}
	if !equality.Semantic.DeepEqual(before, &migration.Status) {
		if err := r.updateStatus(ctx, migration); err != nil {
			return false, ctrl.Result{}, err
		}
	}
	return false, ctrl.Result{RequeueAfter: migrationPowerOffPollInterval}, nil
}
