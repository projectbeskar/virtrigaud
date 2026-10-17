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
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// The record of a single-host Create.
//
// A clustered VM records its pending host durably BEFORE Create (ADR-0007
// Addendum A, A2). A single-host VM records the create marker instead
// (Provisioning=True/CreateRequested, virtualmachine_create_marker.go) before
// the Create is sent, and its id after: if the status write that carries the
// new status.id were lost — typically a Conflict, because the VM was edited (or
// its deletion requested) during a Create of several minutes — the VM would
// look unbound, the next reconcile would send Create again, and a delete
// would release the finalizer without any provider Delete.
//
// The id is therefore recorded as a JSON Patch of the status subresource
// WITHOUT a resourceVersion precondition (patchVMStatus), retried on
// transient and transport errors, each attempt bounded. Only this controller
// writes a non-adopted VM's status, and one VM is never reconciled
// concurrently, so replacing the status overwrites nothing another writer
// set. "test" operations let it land only on the object the reconcile read
// (metadata.uid) and never over an id recorded meanwhile (status.id).
//
// If it still cannot be written, the durable marker already says that the VM
// may exist. The manager also keeps the id in memory (unsavedCreates), which
// still adds what the marker cannot: the id itself. While the manager runs,
// the next reconcile writes the record again — on every provider type,
// including those that cannot find their VM by owner (Proxmox, mock), which
// would otherwise wait for an operator — and a delete in the meantime deletes
// that VM by id. A Warning event names the provider's VM id either way.

// eventReasonVMRecordNotSaved is the Warning event reason recorded when the
// provider created a VM whose id could not be written to the VirtualMachine's
// status.
const eventReasonVMRecordNotSaved = "VMRecordNotSaved"

// defaultCreateRecordBackoff bounds the retries of a VM's create marker and
// record (patchVMStatus): six attempts over about six seconds, enough to ride
// out an API server restart or a dropped connection, never an unbounded wait.
var defaultCreateRecordBackoff = wait.Backoff{Steps: 6, Duration: 100 * time.Millisecond, Factor: 2, Jitter: 0.1}

// createRecordAttemptTimeout bounds each attempt of a create marker or record
// write (patchVMStatus), so a hung API call cannot hold the reconcile.
const createRecordAttemptTimeout = 5 * time.Second

// unsavedCreate is a single-host Create the provider confirmed whose record
// could not be written to the VirtualMachine's status: the provider's VM id
// (and task), the Provider it was created through, and the size it was
// created at.
type unsavedCreate struct {
	id               string
	taskRef          string
	provider         infravirtrigaudiov1beta1.BoundProviderRef
	currentResources *infravirtrigaudiov1beta1.VirtualMachineResources
}

// createRecordBackoff is the reconciler's record retry budget.
func (r *VirtualMachineReconciler) createRecordBackoff() wait.Backoff {
	if r.recordBackoff != nil {
		return *r.recordBackoff
	}
	return defaultCreateRecordBackoff
}

// lookupUnsavedCreate returns the unsaved record of vm's Create, if any.
func (r *VirtualMachineReconciler) lookupUnsavedCreate(vm *infravirtrigaudiov1beta1.VirtualMachine) (unsavedCreate, bool) {
	v, ok := r.unsavedCreates.Load(vm.UID)
	if !ok {
		return unsavedCreate{}, false
	}
	rec, ok := v.(unsavedCreate)
	return rec, ok
}

// forgetUnsavedCreate drops the unsaved record of the VM with uid.
func (r *VirtualMachineReconciler) forgetUnsavedCreate(uid types.UID) {
	r.unsavedCreates.Delete(uid)
}

// liveReader is the reader of the live VirtualMachine: the uncached
// APIReader, or the client when none is configured (tests).
func (r *VirtualMachineReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// readLiveVM reads vm as the API server has it now. gone is true when it no
// longer exists or was replaced by another object of the same name (another
// UID).
func (r *VirtualMachineReconciler) readLiveVM(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (
	live *infravirtrigaudiov1beta1.VirtualMachine, gone bool, err error,
) {
	live = &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(vm), live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("read VirtualMachine %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	if vm.UID != "" && live.UID != vm.UID {
		return live, true, nil
	}
	return live, false, nil
}

// idTest is the status.id a status patch requires the stored object to have:
// none (skip the test), or a value — "" meaning no id (the JSON null test, which
// passes when status.id is absent).
type idTest struct {
	check bool
	id    string
}

// expectID is the idTest for id.
func expectID(id string) idTest { return idTest{check: true, id: id} }

// vmStatusPatch is the JSON Patch patchVMStatus sends: a test of the read
// object's metadata.uid, optionally a test of status.id (want), then status
// replaced as a whole ("add" replaces an existing member and creates an
// absent one).
func vmStatusPatch(vm *infravirtrigaudiov1beta1.VirtualMachine, want idTest) (client.Patch, error) {
	applyDerivedPhase(vm)
	var ops []jsonPatchOp
	if vm.UID != "" {
		uid, err := json.Marshal(string(vm.UID))
		if err != nil {
			return nil, fmt.Errorf("encode the VirtualMachine uid test: %w", err)
		}
		ops = append(ops, jsonPatchOp{Op: "test", Path: "/metadata/uid", Value: uid})
	}
	if want.check {
		value := json.RawMessage("null")
		if want.id != "" {
			v, err := json.Marshal(want.id)
			if err != nil {
				return nil, fmt.Errorf("encode the VirtualMachine id test: %w", err)
			}
			value = v
		}
		ops = append(ops, jsonPatchOp{Op: "test", Path: "/status/id", Value: value})
	}
	s, err := json.Marshal(&vm.Status)
	if err != nil {
		return nil, fmt.Errorf("encode the VirtualMachine status: %w", err)
	}
	ops = append(ops, jsonPatchOp{Op: "add", Path: "/status", Value: s})
	data, err := json.Marshal(ops)
	if err != nil {
		return nil, fmt.Errorf("encode the VirtualMachine status patch: %w", err)
	}
	return client.RawPatch(types.JSONPatchType, data), nil
}

// patchVMStatus writes vm's status with vmStatusPatch, retrying transient API
// and transport errors (isRetriableRecordError) within createRecordBackoff,
// each attempt bounded by createRecordAttemptTimeout. It returns the last
// error; a failed test is an Invalid error — also the status.id test of an
// object that has no status at all (the API server's JSON Patch cannot test
// a member of an absent object), which recordCreatedVM resolves with a live
// read and a write without that test. It is used for the post-Create record
// only; the pre-Create marker is written with an optimistic lock
// (requestCreate).
func (r *VirtualMachineReconciler) patchVMStatus(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, want idTest) error {
	patch, err := vmStatusPatch(vm, want)
	if err != nil {
		return err
	}
	return retry.OnError(r.createRecordBackoff(), func(err error) bool { return isRetriableRecordError(ctx, err) }, func() error {
		attemptCtx, cancel := context.WithTimeout(ctx, createRecordAttemptTimeout)
		defer cancel()
		return r.Status().Patch(attemptCtx, vm, patch)
	})
}

// recordCreatedVM persists the record of a single-host Create the provider
// confirmed (vm's status already carries its id, bound Provider, size, task
// and conditions; the create marker it replaces was written before the
// Create, so the stored object has no id) and returns the reconcile result.
//
//   - Written: any earlier unsaved record is dropped; the VM is re-checked as
//     after every create.
//   - Any failure: the live object is read (review L2).
//     Gone, or replaced by another object (another UID): nothing can delete
//     that hypervisor VM through it any more — a Warning event names its id,
//     nothing is retried. The same id recorded already: done. Another id
//     recorded: it is not overwritten — a Warning event names the VM this
//     Create made. No id after a failed test (Invalid): written once more
//     without the id test.
//   - Still not written: the record is kept in memory (unsavedCreates), a
//     Warning event names the id, and the VM is re-checked shortly: the next
//     reconcile writes the record again without a second Create, and a delete
//     deletes the VM through the provider.
func (r *VirtualMachineReconciler) recordCreatedVM(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rec := unsavedCreate{id: vm.Status.ID, taskRef: vm.Status.LastTaskRef}
	if vm.Status.BoundProvider != nil {
		rec.provider = *vm.Status.BoundProvider
	}
	if vm.Status.CurrentResources != nil {
		rec.currentResources = vm.Status.CurrentResources.DeepCopy()
	}
	providerKey := types.NamespacedName{Namespace: rec.provider.Namespace, Name: rec.provider.Name}

	err := r.patchVMStatus(ctx, vm, expectID(""))
	if err != nil {
		// Whatever the failure, the live object tells what happened: a
		// failed test is not taken for a replaced VM unless it is one.
		live, gone, lerr := r.readLiveVM(ctx, vm)
		switch {
		case lerr != nil:
			err = lerr
		case gone:
			logger.Error(err, "The VirtualMachine was deleted or replaced before the VM the provider created could be recorded; "+
				"that VM is left unmanaged", "id", rec.id, "provider", providerKey.String())
			r.forgetUnsavedCreate(vm.UID)
			r.recordEvent(vm, corev1.EventTypeWarning, eventReasonVMRecordNotSaved, fmt.Sprintf(
				"The provider created the hypervisor VM %q (Provider %s), but this VirtualMachine was deleted or replaced before "+
					"its id could be recorded: that VM is left unmanaged — check for it and remove it by hand", rec.id, providerKey))
			return ctrl.Result{}, nil
		case live.Status.ID == rec.id:
			err = nil // recorded already (an earlier attempt landed)
		case live.Status.ID != "":
			logger.Error(err, "Another id was recorded on the VirtualMachine meanwhile; not overwriting it",
				"id", rec.id, "recorded", live.Status.ID, "provider", providerKey.String())
			r.forgetUnsavedCreate(vm.UID)
			r.recordEvent(vm, corev1.EventTypeWarning, eventReasonVMRecordNotSaved, fmt.Sprintf(
				"The provider created the hypervisor VM %q (Provider %s), but this VirtualMachine records another VM id "+
					"meanwhile, which is kept: the VM %q is left unmanaged — check for it and remove it by hand",
				rec.id, providerKey, rec.id))
			return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
		case apierrors.IsInvalid(err):
			// No id stored, yet the id test failed (an empty-string id): the
			// live read just showed none, so write it without that test.
			err = r.patchVMStatus(ctx, vm, idTest{})
		}
	}
	if err == nil {
		r.forgetUnsavedCreate(vm.UID)
		return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
	}
	logger.Error(err, "Failed to record the VM the provider created; keeping the record in memory and retrying",
		"id", rec.id, "provider", providerKey.String())
	if _, known := r.lookupUnsavedCreate(vm); !known {
		r.recordEvent(vm, corev1.EventTypeWarning, eventReasonVMRecordNotSaved, fmt.Sprintf(
			"The provider created the hypervisor VM %q (Provider %s), but its id could not be recorded on this "+
				"VirtualMachine's status; the manager retries the record (and deletes that VM if this VirtualMachine is "+
				"deleted meanwhile). If the manager restarts first, the VirtualMachine's CreateRequested marker keeps it "+
				"from being released or created twice", rec.id, providerKey))
	}
	r.unsavedCreates.Store(vm.UID, rec)
	return ctrl.Result{RequeueAfter: providerErrorRetryInterval}, nil
}

// resaveUnsavedCreate handles a single-host VM with no status.id whose Create
// the provider confirmed but whose record was not written (rec): instead of a
// second Create, the record is applied and written again (recordCreatedVM).
// A record made through another Provider object than provider (the VM's
// spec.providerRef was changed meanwhile) cannot be used: it is dropped with
// a Warning event naming the VM it leaves behind, and handled reports false.
func (r *VirtualMachineReconciler) resaveUnsavedCreate(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider *infravirtrigaudiov1beta1.Provider,
	rec unsavedCreate,
) (ctrl.Result, bool, error) {
	if *boundProviderRefFor(provider) != rec.provider {
		log.FromContext(ctx).Info("Dropping the unsaved record of a create made through another Provider; the VM it made is left unmanaged",
			"id", rec.id, "provider", rec.provider.Namespace+"/"+rec.provider.Name)
		r.forgetUnsavedCreate(vm.UID)
		r.recordEvent(vm, corev1.EventTypeWarning, eventReasonVMRecordNotSaved, fmt.Sprintf(
			"The hypervisor VM %q created through Provider %s/%s was never recorded on this VirtualMachine, whose "+
				"spec.providerRef now names another Provider: that VM is left unmanaged — check for it and remove it by hand",
			rec.id, rec.provider.Namespace, rec.provider.Name))
		return ctrl.Result{}, false, nil
	}
	log.FromContext(ctx).Info("Recording the VM the provider already created (its earlier record was not saved); not creating again",
		"id", rec.id)
	applyUnsavedCreate(vm, rec)
	setCreateOutcome(vm, rec.taskRef != "")
	res, err := r.recordCreatedVM(ctx, vm)
	return res, true, err
}

// applyUnsavedCreate sets rec on vm's in-memory status: its id, task, bound
// Provider and created size.
func applyUnsavedCreate(vm *infravirtrigaudiov1beta1.VirtualMachine, rec unsavedCreate) {
	vm.Status.ID = rec.id
	vm.Status.LastTaskRef = rec.taskRef
	bound := rec.provider
	vm.Status.BoundProvider = &bound
	if rec.currentResources != nil {
		vm.Status.CurrentResources = rec.currentResources.DeepCopy()
	}
}

// bindUnsavedCreateForDelete gives a VM being deleted with no status.id the
// id of a VM the provider created for it whose record was not saved, so the
// delete reaches that VM through the provider (or, with orphan-on-delete or
// force-delete, names it) instead of releasing the finalizer as if nothing
// had been created. It reports whether it did.
func (r *VirtualMachineReconciler) bindUnsavedCreateForDelete(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	if vm.Status.ID != "" {
		return false
	}
	rec, ok := r.lookupUnsavedCreate(vm)
	if !ok {
		return false
	}
	log.FromContext(ctx).Info("Deleting a VM the provider created whose record was not saved", "id", rec.id)
	vm.Status.ID = rec.id
	bound := rec.provider
	vm.Status.BoundProvider = &bound
	return true
}
