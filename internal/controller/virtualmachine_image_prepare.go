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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Condition reasons used on VMImage status by the image-prepare flow. They are
// CamelCase per the metav1.Condition convention (distinct from the kebab-case
// metrics `reason` taxonomy) and describe WHAT the prepare flow observed.
const (
	// imageReasonImporting marks an in-flight prepare on a provider.
	imageReasonImporting = "Importing"
	// imageReasonPrepared marks a completed prepare on a provider.
	imageReasonPrepared = "Prepared"
	// imageReasonMissingOnProvider marks Prepare.OnMissing=Fail holding create.
	imageReasonMissingOnProvider = "MissingOnProvider"
	// imageReasonWaitingForImage marks Prepare.OnMissing=Wait holding create.
	imageReasonWaitingForImage = "WaitingForImage"
)

// errImagePrepareHold is a sentinel returned by EnsureImageOnProvider when the
// image is not (yet) prepared and the VM must NOT proceed to create — for
// example when Prepare.OnMissing is Fail or Wait. reconcileVM translates it into
// a requeue (not an error), because the condition is recorded on the VMImage and
// retrying forever as an error would only add log/metric noise. Callers compare
// with errors.Is.
var errImagePrepareHold = errors.New("image prepare: holding VM create")

// imagePrepareRequeueAfter is the requeue interval used while an image-prepare
// task is outstanding. It reflects the cadence of a real provider import
// operation (download + convert + register), matching the VM controller's other
// task-poll requeues, and deliberately avoids a tight reconcile loop.
const imagePrepareRequeueAfter = 5 * time.Second

// EnsureImageOnProvider drives lazy, VM-create-driven image preparation for the
// image referenced by vm against provider (issue #154, PR-5).
//
// It is the single writer of the prepare-related fields of the VMImage status
// (ProviderStatus[provider.Name], PrepareTaskRef, Phase, Ready, AvailableOn,
// LastPrepareTime, Message). Centralizing those writes in the VirtualMachine
// controller — the only actor that holds the (image, provider) pair — avoids the
// two-writer status race that bit issue #189: concurrent VMs referencing the
// same image on different providers each own their own ProviderStatus entry and
// always write via retry.RetryOnConflict after re-GETting the VMImage, so they
// never clobber each other.
//
// Return contract, consumed by reconcileVM:
//   - (false, nil):              nothing to do or already prepared — proceed to create.
//   - (true,  nil):              a prepare is in flight — requeue, do NOT create yet.
//   - (false, errImagePrepareHold): OnMissing forbids preparing (Fail/Wait) — a
//     condition is recorded on the VMImage; requeue without creating.
//   - (false, err):              a real error (provider/transport/status) — surface it.
//
// The first return value (requeue) is only meaningful when err is nil.
func (r *VirtualMachineReconciler) EnsureImageOnProvider(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	providerInstance contracts.Provider,
) (requeue bool, err error) {
	logger := log.FromContext(ctx)

	// Skip: nothing to prepare. ImportedDisk VMs (vm.Spec.ImageRef == nil) carry
	// their own disk and never reference a VMImage; a nil vmImage means the same.
	if vm.Spec.ImageRef == nil || vmImage == nil {
		return false, nil
	}

	// Capability gate — no regression for non-preparing providers. We require
	// BOTH: the provider instance must implement the optional ImagePreparer
	// capability AND the Provider CR must advertise SupportsImageImport (surfaced
	// from the GetCapabilities RPC by issue #176). If either is absent we fall
	// through to today's by-reference create path unchanged. This keeps providers
	// that cannot prepare (or have not reported the capability yet) working
	// exactly as before, instead of silently no-oping a feature.
	ip, ok := providerInstance.(contracts.ImagePreparer)
	if !ok {
		logger.V(1).Info("Provider instance does not implement ImagePreparer; skipping image prepare",
			"provider", provider.Name, "image", vmImage.Name)
		return false, nil
	}
	if !providerAdvertisesImageImport(provider) {
		logger.V(1).Info("Provider does not advertise SupportsImageImport; skipping image prepare",
			"provider", provider.Name, "image", vmImage.Name)
		return false, nil
	}

	// Idempotency — already prepared on this provider. The provider's own
	// PrepareImage is also idempotent, but short-circuiting here avoids an RPC
	// and a status write on the steady-state reconcile.
	if ps, found := vmImage.Status.ProviderStatus[provider.Name]; found && ps.Available {
		logger.V(1).Info("Image already prepared on provider; proceeding to create",
			"provider", provider.Name, "image", vmImage.Name)
		return false, nil
	}

	// Honor OnMissing. Default (and explicit Import) prepares. Fail records a
	// terminal-ish condition and holds; Wait holds pending an out-of-band
	// preparer without erroring. Both return errImagePrepareHold so reconcileVM
	// requeues instead of creating.
	switch imageMissingAction(vmImage) {
	case infravirtrigaudiov1beta1.ImageMissingActionFail:
		logger.Info("VMImage Prepare.OnMissing=Fail and image not prepared on provider; not preparing",
			"provider", provider.Name, "image", vmImage.Name)
		if werr := r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
			img.Status.Ready = false
			img.Status.Phase = infravirtrigaudiov1beta1.ImagePhaseFailed
			img.Status.Message = fmt.Sprintf("image not available on provider %q and Prepare.OnMissing=Fail", provider.Name)
			meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
				Type:               infravirtrigaudiov1beta1.VMImageConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             imageReasonMissingOnProvider,
				Message:            img.Status.Message,
				ObservedGeneration: img.Generation,
			})
		}); werr != nil {
			return false, werr
		}
		return false, errImagePrepareHold
	case infravirtrigaudiov1beta1.ImageMissingActionWait:
		logger.Info("VMImage Prepare.OnMissing=Wait and image not prepared on provider; waiting (not preparing)",
			"provider", provider.Name, "image", vmImage.Name)
		if werr := r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
			img.Status.Ready = false
			img.Status.Phase = infravirtrigaudiov1beta1.ImagePhasePending
			img.Status.Message = fmt.Sprintf("image not available on provider %q; waiting (Prepare.OnMissing=Wait)", provider.Name)
			meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
				Type:               infravirtrigaudiov1beta1.VMImageConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             imageReasonWaitingForImage,
				Message:            img.Status.Message,
				ObservedGeneration: img.Generation,
			})
		}); werr != nil {
			return false, werr
		}
		return false, errImagePrepareHold
	}

	// Poll an outstanding prepare task. The VM controller that triggered the
	// prepare polls it to completion using the SAME provider instance, so a
	// TaskRef is always poll-able here.
	if vmImage.Status.PrepareTaskRef != "" {
		done, terr := providerInstance.IsTaskComplete(ctx, vmImage.Status.PrepareTaskRef)
		if terr != nil {
			return false, fmt.Errorf("check image prepare task %s: %w", vmImage.Status.PrepareTaskRef, terr)
		}
		if !done {
			logger.Info("Image prepare task still in progress",
				"provider", provider.Name, "image", vmImage.Name, "taskRef", vmImage.Status.PrepareTaskRef)
			if werr := r.markImagePreparing(ctx, vmImage); werr != nil {
				return false, werr
			}
			return true, nil
		}
		// Task completed — stamp completion and let create proceed.
		logger.Info("Image prepare task completed",
			"provider", provider.Name, "image", vmImage.Name, "taskRef", vmImage.Status.PrepareTaskRef)
		if werr := r.markImagePrepared(ctx, vmImage, provider.Name); werr != nil {
			return false, werr
		}
		return false, nil
	}

	// Trigger a prepare. ImageJSON is the JSON-encoded VMImage spec — exactly
	// what the provider-side parsers consume ({"source":{...},"prepare":{...}}).
	// TargetName is the VMImage name; an empty StorageHint lets the provider pick
	// its default storage (datastore / pool / Proxmox storage).
	imageJSON, jerr := json.Marshal(vmImage.Spec)
	if jerr != nil {
		return false, fmt.Errorf("marshal VMImage %s spec for prepare: %w", vmImage.Name, jerr)
	}

	logger.Info("Triggering image prepare on provider",
		"provider", provider.Name, "image", vmImage.Name)
	resp, perr := ip.PrepareImage(ctx, contracts.ImagePrepareRequest{
		ImageJSON:   string(imageJSON),
		TargetName:  vmImage.Name,
		StorageHint: "",
	})
	if perr != nil {
		return false, fmt.Errorf("prepare image %s on provider %s: %w", vmImage.Name, provider.Name, perr)
	}

	if resp.TaskRef != "" {
		// Asynchronous prepare — persist the task ref and requeue to poll it.
		if werr := r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
			img.Status.PrepareTaskRef = resp.TaskRef
			img.Status.Phase = infravirtrigaudiov1beta1.ImagePhaseImporting
			img.Status.Ready = false
			img.Status.Message = fmt.Sprintf("importing image into provider %q", provider.Name)
			now := metav1.Now()
			img.Status.LastPrepareTime = &now
			meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
				Type:               infravirtrigaudiov1beta1.VMImageConditionImporting,
				Status:             metav1.ConditionTrue,
				Reason:             imageReasonImporting,
				Message:            img.Status.Message,
				ObservedGeneration: img.Generation,
			})
		}); werr != nil {
			return false, werr
		}
		logger.Info("Image prepare started asynchronously; requeueing to poll",
			"provider", provider.Name, "image", vmImage.Name, "taskRef", resp.TaskRef)
		return true, nil
	}

	// Synchronous prepare (e.g. libvirt/vSphere import-on-call) — stamp
	// completion immediately and let create proceed.
	logger.Info("Image prepared synchronously on provider",
		"provider", provider.Name, "image", vmImage.Name)
	if werr := r.markImagePrepared(ctx, vmImage, provider.Name); werr != nil {
		return false, werr
	}
	return false, nil
}

// providerAdvertisesImageImport reports whether the Provider CR advertises the
// SupportsImageImport capability via its self-reported capabilities
// (Status.ReportedCapabilities, surfaced from GetCapabilities by issue #176). A
// nil ReportedCapabilities (provider has not reported yet, or runs an older
// provider) reads as false, which makes EnsureImageOnProvider fall through to
// the unchanged by-reference create path — fail-safe, not fail-open into a
// possibly-Unimplemented RPC.
func providerAdvertisesImageImport(provider *infravirtrigaudiov1beta1.Provider) bool {
	caps := provider.Status.ReportedCapabilities
	return caps != nil && caps.SupportsImageImport
}

// imageMissingAction returns the effective Prepare.OnMissing action for the
// image, defaulting to Import when Prepare is unset or OnMissing is empty (the
// CRD default is Import).
func imageMissingAction(vmImage *infravirtrigaudiov1beta1.VMImage) infravirtrigaudiov1beta1.ImageMissingAction {
	if vmImage.Spec.Prepare == nil || vmImage.Spec.Prepare.OnMissing == "" {
		return infravirtrigaudiov1beta1.ImageMissingActionImport
	}
	return vmImage.Spec.Prepare.OnMissing
}

// markImagePreparing records the in-progress (Importing) state on the VMImage
// while a prepare task is outstanding. Idempotent: it sets Phase=Importing and
// the Importing condition without touching the task ref the trigger persisted.
func (r *VirtualMachineReconciler) markImagePreparing(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
) error {
	return r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
		img.Status.Phase = infravirtrigaudiov1beta1.ImagePhaseImporting
		img.Status.Ready = false
		meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
			Type:               infravirtrigaudiov1beta1.VMImageConditionImporting,
			Status:             metav1.ConditionTrue,
			Reason:             imageReasonImporting,
			Message:            "image import in progress",
			ObservedGeneration: img.Generation,
		})
	})
}

// markImagePrepared stamps a completed prepare for providerName: it records the
// per-provider ProviderStatus entry, adds providerName to AvailableOn (deduped),
// clears the PrepareTaskRef, and sets Ready/Phase=Ready. Ready is the OR across
// providers — any provider having the image Available makes the image Ready —
// while ProviderStatus/AvailableOn carry the per-provider truth.
func (r *VirtualMachineReconciler) markImagePrepared(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	providerName string,
) error {
	return r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
		if img.Status.ProviderStatus == nil {
			img.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{}
		}
		now := metav1.Now()
		ps := img.Status.ProviderStatus[providerName]
		ps.Available = true
		ps.LastUpdated = &now
		ps.Message = "image prepared"
		img.Status.ProviderStatus[providerName] = ps

		img.Status.AvailableOn = appendDedup(img.Status.AvailableOn, providerName)
		img.Status.PrepareTaskRef = ""
		img.Status.Ready = true
		img.Status.Phase = infravirtrigaudiov1beta1.ImagePhaseReady
		img.Status.Message = ""
		img.Status.LastPrepareTime = &now
		meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
			Type:               infravirtrigaudiov1beta1.VMImageConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             imageReasonPrepared,
			Message:            fmt.Sprintf("image prepared on provider %q", providerName),
			ObservedGeneration: img.Generation,
		})
		meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
			Type:               infravirtrigaudiov1beta1.VMImageConditionImporting,
			Status:             metav1.ConditionFalse,
			Reason:             imageReasonPrepared,
			Message:            "image import complete",
			ObservedGeneration: img.Generation,
		})
	})
}

// writeImageStatus applies mutate to the VMImage status under
// retry.RetryOnConflict after re-GETting the latest object, then mirrors the
// committed status back onto the in-memory vmImage so the caller observes its
// own write (important for the immediate idempotency check on the next call and
// for unit-test assertions). This is the single-writer, conflict-safe path the
// VirtualMachine controller uses for every prepare-related VMImage status field;
// it never blind-overwrites, so two VMs preparing the same image on different
// providers cannot clobber each other's ProviderStatus entry (issue #189-class
// race avoidance).
func (r *VirtualMachineReconciler) writeImageStatus(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	mutate func(*infravirtrigaudiov1beta1.VMImage),
) error {
	key := types.NamespacedName{Name: vmImage.Name, Namespace: vmImage.Namespace}
	latest := &infravirtrigaudiov1beta1.VMImage{}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if getErr := r.Get(ctx, key, latest); getErr != nil {
			return getErr
		}
		mutate(latest)
		return r.Status().Update(ctx, latest)
	}); err != nil {
		return fmt.Errorf("update VMImage %s status: %w", vmImage.Name, err)
	}
	// Reflect the committed status onto the caller's copy.
	latest.Status.DeepCopyInto(&vmImage.Status)
	vmImage.ResourceVersion = latest.ResourceVersion
	return nil
}

// appendDedup appends s to list if it is not already present, preserving order.
func appendDedup(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// imageEnsureResultToReconcile is a small helper used by reconcileVM to turn the
// EnsureImageOnProvider result into a requeue ctrl.Result. It exists so the
// requeue cadence lives in one place next to imagePrepareRequeueAfter.
func imageEnsureResultToReconcile() ctrl.Result {
	return ctrl.Result{RequeueAfter: imagePrepareRequeueAfter}
}
