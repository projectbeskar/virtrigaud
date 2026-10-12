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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The administrator's disk-size cap (backlog #7). The manager's
// --max-disk-size ceiling, lowered per Provider by its owner's
// infra.virtrigaud.io/max-disk-size annotation
// (infravirtrigaudiov1beta1.EffectiveMaxDiskSize), bounds every disk
// VirtRigaud asks a provider to create or grow. It is enforced here, before
// any provider call — the admission webhooks are optional and may be off:
//
//   - a VirtualMachine Create (and the re-create of a vanished VM): every
//     disk it asks for — the VMClass spec.diskDefaults.size primary disk and
//     each spec.disks entry — must fit, or nothing is sent
//     (holdForDiskSizeLimit);
//   - a Reconfigure of an existing VM: a disk size above the limit is
//     withheld from the request (sent as 0, which every provider reads as
//     "leave this disk as it is"), so a disk is never grown beyond the limit,
//     while the VM's other changes are applied. A VM created before the limit
//     with larger disks keeps running and is never shrunk
//     (withholdDiskGrowthOverLimit);
//   - a VMClone before its Clone RPC, and a VMMigration in its Validating
//     phase, for the disks they would create on the target.

// diskSizeLimitRetryInterval re-checks an object held by the disk-size limit.
// The hold ends only with an edit (a smaller size, a raised limit), and the
// controllers do not watch the manager's flag or every VMClass, so the
// re-check is unhurried.
const diskSizeLimitRetryInterval = 2 * time.Minute

// errReasonDiskSizeLimit is the metrics reason of a create, clone or
// migration held by the disk-size limit.
const errReasonDiskSizeLimit = "disk-size-limit"

// diskSizeLimitUnverifiableMessage is the constant message of a hold whose
// Provider's limit annotation does not parse.
const diskSizeLimitUnverifiableMessage = "The Provider's " + infravirtrigaudiov1beta1.ProviderMaxDiskSizeAnnotation +
	" annotation is not a valid disk-size limit, so no disk is created, cloned, migrated or grown through it: " +
	"its owner must set a quantity of at least 1Gi (for example 500Gi) or remove the annotation"

// ParseMaxDiskSizeFlag parses the manager's --max-disk-size: empty means no
// ceiling (nil); anything else must be a quantity of at least 1Gi
// (infravirtrigaudiov1beta1.ParseMaxDiskSize).
func ParseMaxDiskSizeFlag(value string) (*resource.Quantity, error) {
	if value == "" {
		return nil, nil
	}
	return infravirtrigaudiov1beta1.ParseMaxDiskSize(value)
}

// diskSizeRefusal is why disks may not be created through a Provider: a
// condition reason and a message that is stable for the same spec and limit.
type diskSizeRefusal struct {
	reason  string
	message string
}

// checkDiskSizeLimit returns the refusal for disks under the limit that
// applies to provider (ceiling lowered by its annotation), or nil when all
// fit. outcome completes the message ("nothing is created"). An invalid
// annotation is a refusal too (fail closed).
func checkDiskSizeLimit(ceiling *resource.Quantity, provider *infravirtrigaudiov1beta1.Provider,
	disks []infravirtrigaudiov1beta1.DiskSizeRequest, outcome string) *diskSizeRefusal {
	limit, err := infravirtrigaudiov1beta1.EffectiveMaxDiskSize(ceiling, provider)
	if err != nil {
		return &diskSizeRefusal{reason: k8s.ReasonDiskSizeLimitUnverifiable, message: diskSizeLimitUnverifiableMessage}
	}
	over, ok := infravirtrigaudiov1beta1.FirstDiskOverLimit(disks, limit)
	if !ok {
		return nil
	}
	return &diskSizeRefusal{reason: k8s.ReasonDiskSizeExceedsLimit, message: diskSizeExceedsMessage(over, limit, outcome)}
}

// diskSizeExceedsMessage is the message of a disk above limit.
func diskSizeExceedsMessage(over infravirtrigaudiov1beta1.DiskSizeRequest, limit *resource.Quantity, outcome string) string {
	return fmt.Sprintf("%s asks for %s, above the disk-size limit %s: %s. Lower it to the limit or below, or ask the "+
		"cluster administrator (the manager's --max-disk-size) or the Provider's owner (its %s annotation) to raise the limit",
		over.Field, over.Size.String(), limit.String(), outcome, infravirtrigaudiov1beta1.ProviderMaxDiskSizeAnnotation)
}

// vmDiskSizeRequests are the disk sizes a Create of vm with vmClass asks for:
// the class's primary-disk size and each spec.disks entry.
func vmDiskSizeRequests(vm *infravirtrigaudiov1beta1.VirtualMachine, vmClass *infravirtrigaudiov1beta1.VMClass) []infravirtrigaudiov1beta1.DiskSizeRequest {
	disks := infravirtrigaudiov1beta1.SpecDiskSizeRequests(vm.Spec.Disks)
	if req, ok := infravirtrigaudiov1beta1.ClassDiskSizeRequest(vmClass); ok {
		disks = append([]infravirtrigaudiov1beta1.DiskSizeRequest{req}, disks...)
	}
	return disks
}

// refuseCreateOverDiskSizeLimit holds vm when a Create of it would ask for a
// disk above the limit that applies to provider. It reports whether it held
// vm; the caller must then return res.
func (r *VirtualMachineReconciler) refuseCreateOverDiskSizeLimit(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	provider *infravirtrigaudiov1beta1.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass,
) (ctrl.Result, bool) {
	refusal := checkDiskSizeLimit(r.MaxDiskSize, provider, vmDiskSizeRequests(vm, vmClass), "nothing is created")
	if refusal == nil {
		return ctrl.Result{}, false
	}
	return r.holdForDiskSizeLimit(ctx, vm, persisted, refusal), true
}

// holdForDiskSizeLimit keeps vm from being created (or re-created): no
// provider call is made — no image prepare, no placement, no Create. Ready=False
// and Provisioning=False carry refusal (Provisioning is left alone while the
// create marker is set: a Create may already have made the VM, and the marker
// must survive), a Warning event is recorded when the hold begins or its
// message changes, and the VM is re-checked every diskSizeLimitRetryInterval.
// persisted is the status as read; the status is written only when it
// changes.
func (r *VirtualMachineReconciler) holdForDiskSizeLimit(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	refusal *diskSizeRefusal,
) ctrl.Result {
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	isNew := prev == nil || prev.Reason != refusal.reason || prev.Message != refusal.message
	condTypes := []string{k8s.ConditionReady, k8s.ConditionProvisioning}
	if createRequested(vm) {
		condTypes = condTypes[:1]
	}
	for _, condType := range condTypes {
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             refusal.reason,
			Message:            refusal.message,
			ObservedGeneration: vm.Generation,
		})
	}
	log.FromContext(ctx).Info("Not creating the VM: a disk is above the disk-size limit",
		"reason", refusal.reason, "retryAfter", diskSizeLimitRetryInterval.String())
	metrics.RecordError(errReasonDiskSizeLimit, metrics.ComponentManager)
	if persisted == nil || !equality.Semantic.DeepEqual(persisted, &vm.Status) {
		r.updateStatus(ctx, vm)
	}
	if isNew {
		r.recordEvent(vm, corev1.EventTypeWarning, refusal.reason, refusal.message)
	}
	return ctrl.Result{RequeueAfter: diskSizeLimitRetryInterval}
}

// withholdDiskGrowthOverLimit removes from a Reconfigure request every disk
// size above the limit that applies to provider: it is sent as 0, which every
// provider reads as "leave this disk as it is" (vSphere and Proxmox resize a
// disk only for a positive SizeGiB, libvirt only for a positive
// Class.DiskDefaults.SizeGiB). The sizes are compared as sent (whole GiB). It
// returns the refusal to report, nil when nothing was withheld. With an
// invalid limit annotation every disk size is withheld (fail closed).
func withholdDiskGrowthOverLimit(req *contracts.CreateRequest, ceiling *resource.Quantity,
	provider *infravirtrigaudiov1beta1.Provider, vmClass *infravirtrigaudiov1beta1.VMClass) *diskSizeRefusal {
	limit, err := infravirtrigaudiov1beta1.EffectiveMaxDiskSize(ceiling, provider)
	if err == nil && limit == nil {
		return nil
	}
	over := func(gib int32) bool {
		if gib <= 0 {
			return false
		}
		if err != nil {
			return true
		}
		size := infravirtrigaudiov1beta1.DiskSizeGiBQuantity(gib)
		return size.Cmp(*limit) > 0
	}
	var withheld []infravirtrigaudiov1beta1.DiskSizeRequest
	if dd := req.Class.DiskDefaults; dd != nil && over(dd.SizeGiB) {
		field := "VMClass spec.diskDefaults.size"
		if vmClass != nil {
			field = fmt.Sprintf("VMClass %s spec.diskDefaults.size", vmClass.Name)
		}
		withheld = append(withheld, infravirtrigaudiov1beta1.DiskSizeRequest{Field: field, Size: infravirtrigaudiov1beta1.DiskSizeGiBQuantity(dd.SizeGiB)})
		dd.SizeGiB = 0
	}
	for i := range req.Disks {
		if over(req.Disks[i].SizeGiB) {
			withheld = append(withheld, infravirtrigaudiov1beta1.DiskSizeRequest{
				Field: fmt.Sprintf("spec.disks[%s].sizeGiB", req.Disks[i].Name),
				Size:  infravirtrigaudiov1beta1.DiskSizeGiBQuantity(req.Disks[i].SizeGiB),
			})
			req.Disks[i].SizeGiB = 0
		}
	}
	if len(withheld) == 0 {
		return nil
	}
	if err != nil {
		return &diskSizeRefusal{reason: k8s.ReasonDiskSizeLimitUnverifiable, message: diskSizeLimitUnverifiableMessage}
	}
	fields := make([]string, 0, len(withheld))
	for _, w := range withheld {
		fields = append(fields, fmt.Sprintf("%s (%s)", w.Field, w.Size.String()))
	}
	return &diskSizeRefusal{
		reason: k8s.ReasonDiskSizeExceedsLimit,
		message: fmt.Sprintf("%s: above the disk-size limit %s, so not grown; the VM's other changes go ahead and no disk "+
			"is shrunk. Lower it to the limit or below, or ask the cluster administrator (the manager's --max-disk-size) or "+
			"the Provider's owner (its %s annotation) to raise the limit",
			strings.Join(fields, ", "), limit.String(), infravirtrigaudiov1beta1.ProviderMaxDiskSizeAnnotation),
	}
}

// warnDiskGrowthWithheld records the Warning event of a Reconfigure whose
// disk growth was withheld, once per VM and message (r.diskLimitWarned); a
// nil refusal forgets the VM so the next withheld growth is reported again.
func (r *VirtualMachineReconciler) warnDiskGrowthWithheld(vm *infravirtrigaudiov1beta1.VirtualMachine, refusal *diskSizeRefusal) {
	key := string(vm.UID)
	if refusal == nil {
		r.diskLimitWarned.Delete(key)
		return
	}
	if prev, ok := r.diskLimitWarned.Load(key); ok && prev == refusal.message {
		return
	}
	r.diskLimitWarned.Store(key, refusal.message)
	r.recordEvent(vm, corev1.EventTypeWarning, refusal.reason, refusal.message)
}

// classDiskSizeRequest reads the VMClass at key and returns its primary-disk
// size request (ok false when it sets none). found is false when the class
// does not exist; any other read error is returned.
func classDiskSizeRequest(ctx context.Context, c client.Reader, key types.NamespacedName) (req infravirtrigaudiov1beta1.DiskSizeRequest, ok, found bool, err error) {
	class := &infravirtrigaudiov1beta1.VMClass{}
	if err := c.Get(ctx, key, class); err != nil {
		if apierrors.IsNotFound(err) {
			return infravirtrigaudiov1beta1.DiskSizeRequest{}, false, false, nil
		}
		return infravirtrigaudiov1beta1.DiskSizeRequest{}, false, false, fmt.Errorf("get VMClass %s: %w", key, err)
	}
	req, ok = infravirtrigaudiov1beta1.ClassDiskSizeRequest(class)
	return req, ok, true, nil
}

// classMissingRefusal is the refusal of a clone or migration whose size-
// bearing VMClass at key does not exist while a limit applies: its size cannot
// be checked, so nothing is done (fail closed).
func classMissingRefusal(key types.NamespacedName, outcome string) *diskSizeRefusal {
	return &diskSizeRefusal{
		reason: k8s.ReasonDiskSizeLimitUnverifiable,
		message: fmt.Sprintf("VMClass %s does not exist, so its disk size cannot be checked against the disk-size limit: %s",
			key.String(), outcome),
	}
}

// cloneDiskSizeRefusal checks the disks a clone would create on provider
// against the limit: the source VM's spec.disks and the primary-disk sizes of
// its VMClass and of the clone's target VMClass override (a provider may grow
// the cloned primary disk to it). It returns nil when all fit or no limit
// applies, and an error only for a failed read (retried).
func (r *VMCloneReconciler) cloneDiskSizeRefusal(ctx context.Context, clone *infravirtrigaudiov1beta1.VMClone,
	sourceVM *infravirtrigaudiov1beta1.VirtualMachine, provider *infravirtrigaudiov1beta1.Provider) (*diskSizeRefusal, error) {
	const outcome = "nothing is cloned"
	limit, err := infravirtrigaudiov1beta1.EffectiveMaxDiskSize(r.MaxDiskSize, provider)
	if err == nil && limit == nil {
		return nil, nil
	}
	if err != nil {
		return checkDiskSizeLimit(r.MaxDiskSize, provider, nil, outcome), nil
	}
	disks := infravirtrigaudiov1beta1.SpecDiskSizeRequests(sourceVM.Spec.Disks)
	// The source's primary disk is copied: its class must be readable.
	if key, ok := vmClassKey(sourceVM); ok {
		req, sized, found, err := classDiskSizeRequest(ctx, r.Client, key)
		if err != nil {
			return nil, err
		}
		if !found {
			return classMissingRefusal(key, outcome), nil
		}
		if sized {
			disks = append(disks, req)
		}
	}
	// A target class override that does not exist is not sent (classJSON),
	// so it grows nothing.
	if ref := clone.Spec.Target.ClassRef; ref != nil && ref.Name != "" {
		req, sized, _, err := classDiskSizeRequest(ctx, r.Client, types.NamespacedName{Namespace: clone.Namespace, Name: ref.Name})
		if err != nil {
			return nil, err
		}
		if sized {
			disks = append(disks, req)
		}
	}
	return checkDiskSizeLimit(r.MaxDiskSize, provider, disks, outcome), nil
}

// holdCloneForDiskSizeLimit keeps clone Pending (no Clone RPC, no target VM)
// with refusal as its Ready=False reason and message, written only when it
// changes (markPending), with a Warning event when the hold begins or its
// message changes; it is re-checked every diskSizeLimitRetryInterval.
func (r *VMCloneReconciler) holdCloneForDiskSizeLimit(ctx context.Context, clone *infravirtrigaudiov1beta1.VMClone, refusal *diskSizeRefusal) ctrl.Result {
	prev := meta.FindStatusCondition(clone.Status.Conditions, infravirtrigaudiov1beta1.VMCloneConditionReady)
	isNew := prev == nil || prev.Reason != refusal.reason || prev.Message != refusal.message
	log.FromContext(ctx).Info("Not cloning: a disk is above the disk-size limit", "reason", refusal.reason)
	metrics.RecordError(errReasonDiskSizeLimit, metrics.ComponentManager)
	r.markPending(ctx, clone, refusal.reason, refusal.message)
	if isNew && r.Recorder != nil {
		r.Recorder.Event(clone, corev1.EventTypeWarning, refusal.reason, refusal.message)
	}
	return ctrl.Result{RequeueAfter: diskSizeLimitRetryInterval}
}

// migrationDiskSizeRefusal checks the disks a migration would create on its
// target Provider against the limit: the exported primary disk (sized by the
// source VM's VMClass) and the target VMClass's primary-disk size (the target
// VM's primary disk is grown to it). It returns nil when all fit or no limit
// applies, and an error only for a failed read (retried).
func (r *VMMigrationReconciler) migrationDiskSizeRefusal(ctx context.Context, migration *infravirtrigaudiov1beta1.VMMigration,
	sourceVM *infravirtrigaudiov1beta1.VirtualMachine, targetProvider *infravirtrigaudiov1beta1.Provider) (*diskSizeRefusal, error) {
	const outcome = "nothing is migrated"
	limit, err := infravirtrigaudiov1beta1.EffectiveMaxDiskSize(r.MaxDiskSize, targetProvider)
	if err == nil && limit == nil {
		return nil, nil
	}
	if err != nil {
		return checkDiskSizeLimit(r.MaxDiskSize, targetProvider, nil, outcome), nil
	}
	var disks []infravirtrigaudiov1beta1.DiskSizeRequest
	if key, ok := vmClassKey(sourceVM); ok {
		req, sized, found, err := classDiskSizeRequest(ctx, r.Client, key)
		if err != nil {
			return nil, err
		}
		if !found {
			return classMissingRefusal(key, outcome), nil
		}
		if sized {
			disks = append(disks, req)
		}
	}
	// A target class that does not exist leaves the target VM waiting for it:
	// it grows nothing.
	if ref := migration.Spec.Target.ClassRef; ref != nil && ref.Name != "" {
		req, sized, _, err := classDiskSizeRequest(ctx, r.Client, types.NamespacedName{Namespace: migration.Namespace, Name: ref.Name})
		if err != nil {
			return nil, err
		}
		if sized {
			disks = append(disks, req)
		}
	}
	return checkDiskSizeLimit(r.MaxDiskSize, targetProvider, disks, outcome), nil
}

// holdMigrationForDiskSizeLimit keeps migration in its Validating phase — no
// snapshot, export, import or target VM — with refusal as its Validating=False
// reason and message, written only when it changes, with a Warning event when
// the hold begins or its message changes; it is re-checked every
// diskSizeLimitRetryInterval.
func (r *VMMigrationReconciler) holdMigrationForDiskSizeLimit(ctx context.Context, migration *infravirtrigaudiov1beta1.VMMigration,
	refusal *diskSizeRefusal) (ctrl.Result, error) {
	log.FromContext(ctx).Info("Not migrating: a disk is above the disk-size limit", "reason", refusal.reason)
	metrics.RecordError(errReasonDiskSizeLimit, metrics.ComponentManager)
	prev := meta.FindStatusCondition(migration.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionValidating)
	if prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == refusal.reason && prev.Message == refusal.message {
		return ctrl.Result{RequeueAfter: diskSizeLimitRetryInterval}, nil
	}
	meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type:               infravirtrigaudiov1beta1.VMMigrationConditionValidating,
		Status:             metav1.ConditionFalse,
		Reason:             refusal.reason,
		Message:            refusal.message,
		ObservedGeneration: migration.Generation,
	})
	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(migration, corev1.EventTypeWarning, refusal.reason, refusal.message)
	}
	return ctrl.Result{RequeueAfter: diskSizeLimitRetryInterval}, nil
}
