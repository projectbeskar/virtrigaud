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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	// imageReasonInvalidSource marks a provider rejecting the image source as
	// an invalid specification (e.g. a disallowed libvirt image path).
	imageReasonInvalidSource = "InvalidSource"
	// imageReasonPrepareStateDropped marks prepare state recorded by an earlier
	// release under a bare Provider name that is not a Provider in the
	// VMImage's namespace: it cannot be attributed to a Provider, so it was
	// dropped and the image is prepared again on first use.
	imageReasonPrepareStateDropped = "PrepareStateDropped"
)

// eventReasonImagePrepareStateAccepted is the Warning event recorded on a VM
// whose VMImage has spec.prepare.onMissing Fail or Wait and an available
// status.providerStatus entry for the VM's Provider that was not recorded
// through that Provider object (the Provider was re-created, or the entry
// predates status.providerStatus[].providerUID). The prepare that would
// re-validate it is forbidden, so the entry is accepted and the Provider's
// current UID recorded.
const eventReasonImagePrepareStateAccepted = "ImagePrepareStateAccepted"

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

// imageProviderKey is the key of provider's entry in VMImage
// status.providerStatus, and its element of status.availableOn: the
// Provider's identity "<namespace>/<name>".
//
// Keying by identity rather than by the bare name is a security property: a
// VMImage can be shared across namespaces (spec.consumerNamespaceSelector), and
// two namespaces can each have a Provider of the same name that fronts a
// different hypervisor with different credentials. Keyed by name, the first
// namespace to prepare the image would have the other's VMs skip their own
// prepare and create from an artifact they did not prepare.
func imageProviderKey(provider *infravirtrigaudiov1beta1.Provider) string {
	return types.NamespacedName{Namespace: provider.Namespace, Name: provider.Name}.String()
}

// isImageProviderKey reports whether key is a "<namespace>/<name>" identity
// key. Kubernetes object names never contain "/", so a key without one is a
// bare Provider name written by an earlier release.
func isImageProviderKey(key string) bool {
	return strings.Contains(key, "/")
}

// imageEntryRecordedThrough reports whether the providerStatus entry ps was
// recorded through provider's current object: its providerUID is set and is
// provider's UID. Only such an entry is trusted as is. An entry with another
// UID (the Provider was deleted and re-created under the same namespace and
// name) or with none (migrated from an earlier release) is re-validated
// through provider before a VM is created from it.
func imageEntryRecordedThrough(ps infravirtrigaudiov1beta1.ProviderImageStatus, provider *infravirtrigaudiov1beta1.Provider) bool {
	return ps.ProviderUID != "" && ps.ProviderUID == string(provider.UID)
}

// EnsureImageOnProvider drives lazy, VM-create-driven image preparation for the
// image referenced by vm against provider (issue #154, PR-5).
//
// It is the single writer of the prepare-related fields of the VMImage status
// (ProviderStatus[<provider namespace>/<provider name>], Phase, Ready,
// AvailableOn, LastPrepareTime, Message, and the migration of state an earlier
// release wrote). Centralizing those writes in the VirtualMachine controller —
// the only actor that holds the (image, provider) pair — avoids the two-writer
// status race that bit issue #189: concurrent VMs referencing the same image on
// different providers each own their own ProviderStatus entry and always write
// via retry.RetryOnConflict after re-GETting the VMImage, so they never clobber
// each other.
//
// Prepare state is per Provider IDENTITY (imageProviderKey): the VM consults
// only the entry of the Provider it uses, trusts it only when it was recorded
// through that Provider object (imageEntryRecordedThrough), and polls only the
// prepare task recorded in that entry, through that Provider.
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

	// The image and the provider must both be usable from the VM's namespace
	// (spec.consumerNamespaceSelector). reconcileVM resolved them through
	// getDependencies, which already refuses an ungranted cross-namespace
	// reference; re-checking here guarantees that no prepare RPC is sent and no
	// VMImage status is written for a pair the VM may not use, whoever calls
	// this. A refusal is returned as a *ConsumerNotAllowedError.
	for _, obj := range []client.Object{vmImage, provider} {
		if err := checkConsumer(ctx, r.Client, obj, vm.Namespace); err != nil {
			return false, err
		}
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

	// State an earlier release recorded under bare Provider names (and its
	// single image-wide task ref) is migrated before any entry is consulted, so
	// it can never satisfy a Provider it was not recorded for.
	if err := r.migrateLegacyImagePrepareState(ctx, vmImage); err != nil {
		return false, err
	}

	key := imageProviderKey(provider)
	entry, found := vmImage.Status.ProviderStatus[key]
	recordedThrough := found && imageEntryRecordedThrough(entry, provider)

	// Idempotency — already prepared through this Provider object. The
	// provider's own PrepareImage is also idempotent, but short-circuiting here
	// avoids an RPC and a status write on the steady-state reconcile.
	if recordedThrough && entry.Available {
		logger.V(1).Info("Image already prepared on provider; proceeding to create",
			"provider", key, "image", vmImage.Name)
		return false, nil
	}

	// Reference-style sources need no import. A libvirt pool-file path, an
	// existing vSphere template / content-library item, or an existing Proxmox
	// template is already present on the provider — there is nothing to download
	// or convert, so it is "available" by construction. Proceed straight to create
	// using the source as-is, even when Prepare.OnMissing=Fail, rather than holding
	// the VM for a prepare that need not (and will not) happen (issue #227). The
	// by-reference create path already consumes these directly — see
	// overrideImageWithPreparedLocation's fallback — and a wrong path/template
	// still fails honestly at create time, which is where a missing backing
	// artifact belongs.
	if !imageSourceNeedsPrepare(vmImage) {
		logger.V(1).Info("Image source is already present on the provider (no import needed); proceeding to create",
			"provider", key, "image", vmImage.Name)
		return false, nil
	}

	// An entry for this identity that was NOT recorded through this Provider
	// object: the Provider was deleted and re-created under the same namespace
	// and name (a different UID), or the entry predates providerUID (migrated
	// from an earlier release). As for a VM's bound Provider (#341), the
	// re-created Provider is accepted; what it prepared is not assumed. Under
	// OnMissing=Import the prepare below re-validates it: PrepareImage is
	// idempotent on every provider, so it confirms the artifact through THIS
	// Provider (or re-creates it) and records its UID. OnMissing Fail and Wait
	// forbid that prepare; holding instead would stop VMs that already run from
	// this image, so the entry is accepted with a warning and the current UID
	// recorded (the same namespace owns the old and the new Provider).
	stale := found && !recordedThrough
	if stale && entry.Available && imageMissingAction(vmImage) != infravirtrigaudiov1beta1.ImageMissingActionImport {
		logger.Info("WARNING: accepting prepare state that was not recorded through this Provider object; "+
			"Prepare.OnMissing forbids the prepare that would re-validate it",
			"provider", key, "image", vmImage.Name, "recordedUID", entry.ProviderUID, "providerUID", string(provider.UID),
			"onMissing", string(imageMissingAction(vmImage)))
		r.recordEvent(vm, corev1.EventTypeWarning, eventReasonImagePrepareStateAccepted, fmt.Sprintf(
			"VMImage %s/%s: accepted prepare state for Provider %s that was not recorded through its current object (uid %s); "+
				"spec.prepare.onMissing=%s forbids re-validating it",
			vmImage.Namespace, vmImage.Name, key, provider.UID, imageMissingAction(vmImage)))
		if werr := r.acceptImageEntry(ctx, vmImage, provider); werr != nil {
			return false, werr
		}
		return false, nil
	}

	// Honor OnMissing. Default (and explicit Import) prepares. Fail records a
	// terminal-ish condition and holds; Wait holds pending an out-of-band
	// preparer without erroring. Both return errImagePrepareHold so reconcileVM
	// requeues instead of creating.
	switch imageMissingAction(vmImage) {
	case infravirtrigaudiov1beta1.ImageMissingActionFail:
		logger.Info("VMImage Prepare.OnMissing=Fail and image not prepared on provider; not preparing",
			"provider", key, "image", vmImage.Name)
		if werr := r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
			img.Status.Ready = false
			img.Status.Phase = infravirtrigaudiov1beta1.ImagePhaseFailed
			img.Status.Message = fmt.Sprintf("image not available on provider %q and Prepare.OnMissing=Fail", key)
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
			"provider", key, "image", vmImage.Name)
		if werr := r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
			img.Status.Ready = false
			img.Status.Phase = infravirtrigaudiov1beta1.ImagePhasePending
			img.Status.Message = fmt.Sprintf("image not available on provider %q; waiting (Prepare.OnMissing=Wait)", key)
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

	// Poll this Provider's own outstanding prepare task, through this Provider.
	// A task recorded through another Provider object is never polled: a task
	// ID means nothing to a different provider backend, and its completion
	// proves nothing about this one. The prepare is issued again instead.
	if found && entry.TaskRef != "" {
		if !recordedThrough {
			logger.Info("Discarding an image prepare task recorded through another Provider object; preparing again",
				"provider", key, "image", vmImage.Name, "taskRef", entry.TaskRef,
				"recordedUID", entry.ProviderUID, "providerUID", string(provider.UID))
		} else {
			done, terr := providerInstance.IsTaskComplete(ctx, entry.TaskRef)
			if terr != nil {
				return false, fmt.Errorf("check image prepare task %s on provider %s: %w", entry.TaskRef, key, terr)
			}
			if !done {
				logger.Info("Image prepare task still in progress",
					"provider", key, "image", vmImage.Name, "taskRef", entry.TaskRef)
				if werr := r.markImagePreparing(ctx, vmImage); werr != nil {
					return false, werr
				}
				return true, nil
			}
			// Task completed — flip Available=true and let create proceed. The
			// prepared location (id/path) was already stamped at trigger time,
			// so a nil location preserves it.
			logger.Info("Image prepare task completed",
				"provider", key, "image", vmImage.Name, "taskRef", entry.TaskRef)
			if werr := r.markImagePrepared(ctx, vmImage, provider, nil); werr != nil {
				return false, werr
			}
			return false, nil
		}
	}
	if stale && entry.Available {
		logger.Info("Re-validating prepare state that was not recorded through this Provider object",
			"provider", key, "image", vmImage.Name, "recordedUID", entry.ProviderUID, "providerUID", string(provider.UID))
	}

	// Trigger a prepare. ImageJSON is the JSON-encoded VMImage spec — exactly
	// what the provider-side parsers consume ({"source":{...},"prepare":{...}}).
	// TargetName is the VMImage name; an empty StorageHint lets the provider pick
	// its default storage (datastore / pool / Proxmox storage).
	//
	// spec.consumerNamespaceSelector is operator-side policy (which namespaces
	// may use the image), not image data: it is never sent to a provider.
	imageSpec := vmImage.Spec
	imageSpec.ConsumerNamespaceSelector = nil
	imageJSON, jerr := json.Marshal(imageSpec)
	if jerr != nil {
		return false, fmt.Errorf("marshal VMImage %s spec for prepare: %w", vmImage.Name, jerr)
	}

	logger.Info("Triggering image prepare on provider",
		"provider", key, "image", vmImage.Name)
	resp, perr := ip.PrepareImage(ctx, contracts.ImagePrepareRequest{
		ImageJSON:   string(imageJSON),
		TargetName:  vmImage.Name,
		StorageHint: "",
	})
	if perr != nil {
		if contracts.IsInvalidSpec(perr) {
			// The provider rejected the image source itself (e.g. a libvirt path
			// outside its allowed image directories). Record that on the VMImage
			// so its owner sees WHY, not just the VM; the caller backs off.
			if werr := r.markImageSourceRejected(ctx, vmImage, provider, perr); werr != nil {
				logger.Error(werr, "Failed to record rejected image source on VMImage",
					"provider", key, "image", vmImage.Name)
			}
		}
		return false, fmt.Errorf("prepare image %s on provider %s: %w", vmImage.Name, key, perr)
	}

	if resp.TaskRef != "" {
		// Asynchronous prepare — persist the task ref IN THIS PROVIDER'S ENTRY
		// and requeue to poll it through this Provider. The prepared location
		// (id/path) is already known at trigger time (issue #154 PR-6 / #214),
		// so it is stamped now even though Available stays false until the task
		// completes. This lets the eventual create consume the prepared template
		// without re-discovering its location.
		if werr := r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
			if img.Status.ProviderStatus == nil {
				img.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{}
			}
			now := metav1.Now()
			ps := img.Status.ProviderStatus[key]
			ps.Available = false // not ready until the task completes
			ps.ProviderUID = string(provider.UID)
			ps.TaskRef = resp.TaskRef
			ps.ID = resp.PreparedImageID
			ps.Path = resp.PreparedImagePath
			ps.LastUpdated = &now
			ps.Message = "image import in progress"
			img.Status.ProviderStatus[key] = ps
			img.Status.AvailableOn = removeString(img.Status.AvailableOn, key)
			img.Status.LastPrepareTime = &now
			markImageImporting(img, fmt.Sprintf("importing image into provider %q", key))
		}); werr != nil {
			return false, werr
		}
		logger.Info("Image prepare started asynchronously; requeueing to poll",
			"provider", key, "image", vmImage.Name, "taskRef", resp.TaskRef,
			"preparedID", resp.PreparedImageID, "preparedPath", resp.PreparedImagePath)
		return true, nil
	}

	// Synchronous prepare (e.g. libvirt/vSphere import-on-call) — stamp
	// completion immediately, recording the prepared location, and let create
	// proceed.
	logger.Info("Image prepared synchronously on provider",
		"provider", key, "image", vmImage.Name,
		"preparedID", resp.PreparedImageID, "preparedPath", resp.PreparedImagePath)
	if werr := r.markImagePrepared(ctx, vmImage, provider, &preparedLocation{
		id: resp.PreparedImageID, path: resp.PreparedImagePath,
	}); werr != nil {
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

// imageSourceNeedsPrepare reports whether the VMImage's source must be imported
// onto the provider before a VM can use it.
//
// Import-style sources produce a NEW artifact on the provider and DO need
// preparing: a libvirt download URL, a vSphere OVA URL, and any HTTP / container
// registry / DataVolume pull.
//
// Reference-style sources point at something ALREADY PRESENT on the provider and
// need NO preparation — a libvirt pool-file path, an existing vSphere template or
// content-library item, or an existing Proxmox template (by id or name). These
// are exactly the locations the by-reference create path already consumes
// directly (see overrideImageWithPreparedLocation), so returning false for them
// lets such an image create normally instead of being held for an import that
// need not happen (issue #227).
//
// Ambiguous or unrecognized sources return true (prefer running the idempotent
// prepare over silently skipping a real import): a libvirt source carrying BOTH a
// path and a URL, an HTTP/Registry/DataVolume source, or an entirely empty
// source.
func imageSourceNeedsPrepare(vmImage *infravirtrigaudiov1beta1.VMImage) bool {
	src := vmImage.Spec.Source
	switch {
	case src.Libvirt != nil:
		// A pool-file path is already present; a URL must be downloaded. A source
		// carrying both is treated as an import (URL wins) to avoid skipping a
		// real download.
		return src.Libvirt.URL != "" || src.Libvirt.Path == ""
	case src.VSphere != nil:
		// An OVA URL imports; an existing template or content-library item does not.
		if src.VSphere.OVAURL != "" {
			return true
		}
		return src.VSphere.TemplateName == "" && src.VSphere.ContentLibrary == nil
	case src.Proxmox != nil:
		// Proxmox sources only ever reference an existing template (by id or name);
		// there is no import URL. Anything with a template reference is present.
		return src.Proxmox.TemplateID == nil && src.Proxmox.TemplateName == ""
	default:
		// HTTP / Registry / DataVolume (always a fetch) or an unset source.
		return true
	}
}

// imageAvailableOnAnyProvider reports whether any providerStatus entry of img
// is available. The image-level Ready is the OR across providers.
func imageAvailableOnAnyProvider(img *infravirtrigaudiov1beta1.VMImage) bool {
	for _, ps := range img.Status.ProviderStatus {
		if ps.Available {
			return true
		}
	}
	return false
}

// imagePrepareInFlight reports whether any providerStatus entry of img records
// an outstanding prepare task.
func imagePrepareInFlight(img *infravirtrigaudiov1beta1.VMImage) bool {
	for _, ps := range img.Status.ProviderStatus {
		if ps.TaskRef != "" {
			return true
		}
	}
	return false
}

// markImageImporting records an in-flight prepare at the image level: the
// Importing condition, and — only while the image is not available on any
// provider — Phase=Importing, Ready=false and a Ready=False condition. A
// prepare on one Provider therefore never hides the image being Ready on
// another (Ready is the OR across providers).
func markImageImporting(img *infravirtrigaudiov1beta1.VMImage, msg string) {
	meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
		Type:               infravirtrigaudiov1beta1.VMImageConditionImporting,
		Status:             metav1.ConditionTrue,
		Reason:             imageReasonImporting,
		Message:            msg,
		ObservedGeneration: img.Generation,
	})
	if imageAvailableOnAnyProvider(img) {
		return
	}
	img.Status.Phase = infravirtrigaudiov1beta1.ImagePhaseImporting
	img.Status.Ready = false
	img.Status.Message = msg
	meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
		Type:               infravirtrigaudiov1beta1.VMImageConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             imageReasonImporting,
		Message:            msg,
		ObservedGeneration: img.Generation,
	})
}

// markImagePreparing records the in-progress (Importing) state on the VMImage
// while a prepare task is outstanding (see markImageImporting). Idempotent: it
// does not touch the task ref the trigger persisted.
func (r *VirtualMachineReconciler) markImagePreparing(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
) error {
	return r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
		markImageImporting(img, "image import in progress")
	})
}

// preparedLocation is where a provider placed a prepared image
// (ImagePrepareResponse.PreparedImageID / PreparedImagePath).
type preparedLocation struct {
	id   string
	path string
}

// markImagePrepared stamps a completed prepare on provider: it records the
// provider's ProviderStatus entry (keyed by imageProviderKey) as available and
// recorded through provider's current UID, clears the entry's task ref, adds
// the key to AvailableOn (deduped), and sets Ready/Phase=Ready. Ready is the OR
// across providers — any provider having the image Available makes the image
// Ready — while ProviderStatus/AvailableOn carry the per-provider truth.
//
// loc is the prepared location returned by PrepareImage, recorded as is so
// create can consume it instead of re-resolving the source (issue #154 PR-6 /
// #214). A nil loc keeps the location already recorded — the async trigger
// stamps it, so the task-completion poll path passes nil.
func (r *VirtualMachineReconciler) markImagePrepared(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	loc *preparedLocation,
) error {
	key := imageProviderKey(provider)
	return r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
		if img.Status.ProviderStatus == nil {
			img.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{}
		}
		now := metav1.Now()
		ps := img.Status.ProviderStatus[key]
		ps.Available = true
		ps.ProviderUID = string(provider.UID)
		ps.TaskRef = ""
		ps.LastUpdated = &now
		ps.Message = "image prepared"
		if loc != nil {
			ps.ID = loc.id
			ps.Path = loc.path
		}
		img.Status.ProviderStatus[key] = ps

		img.Status.AvailableOn = appendDedup(img.Status.AvailableOn, key)
		img.Status.Ready = true
		img.Status.Phase = infravirtrigaudiov1beta1.ImagePhaseReady
		img.Status.Message = ""
		img.Status.LastPrepareTime = &now
		meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
			Type:               infravirtrigaudiov1beta1.VMImageConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             imageReasonPrepared,
			Message:            fmt.Sprintf("image prepared on provider %q", key),
			ObservedGeneration: img.Generation,
		})
		// Another Provider's prepare may still be running; its next poll keeps
		// the Importing condition, so only clear it when none is.
		if !imagePrepareInFlight(img) {
			meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
				Type:               infravirtrigaudiov1beta1.VMImageConditionImporting,
				Status:             metav1.ConditionFalse,
				Reason:             imageReasonPrepared,
				Message:            "image import complete",
				ObservedGeneration: img.Generation,
			})
		}
	})
}

// acceptImageEntry records provider's current UID on its available
// ProviderStatus entry without re-validating it (EnsureImageOnProvider, when
// Prepare.OnMissing forbids the prepare that would). It changes nothing when
// the entry is no longer available.
func (r *VirtualMachineReconciler) acceptImageEntry(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
) error {
	key := imageProviderKey(provider)
	return r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
		ps, ok := img.Status.ProviderStatus[key]
		if !ok || !ps.Available {
			return
		}
		now := metav1.Now()
		ps.ProviderUID = string(provider.UID)
		ps.TaskRef = ""
		ps.LastUpdated = &now
		img.Status.ProviderStatus[key] = ps
	})
}

// markImageSourceRejected records that provider rejected the image source as
// an invalid specification (a non-retryable InvalidArgument from ImagePrepare,
// such as a libvirt path outside the provider's allowed image directories). The
// provider's ProviderStatus entry carries the reason. The image-level
// Phase/Ready condition flip to Failed/InvalidSource only while the image is not
// available on ANY provider, so a rejection on one provider never masks the
// image being Ready elsewhere (Ready is the OR across providers).
func (r *VirtualMachineReconciler) markImageSourceRejected(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	cause error,
) error {
	key := imageProviderKey(provider)
	msg := fmt.Sprintf("image source rejected by provider %q: %s", key, providerErrorMessage(cause))
	return r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
		if img.Status.ProviderStatus == nil {
			img.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{}
		}
		now := metav1.Now()
		ps := img.Status.ProviderStatus[key]
		ps.Available = false
		ps.ProviderUID = string(provider.UID)
		ps.TaskRef = ""
		ps.Message = msg
		ps.LastUpdated = &now
		img.Status.ProviderStatus[key] = ps
		img.Status.AvailableOn = removeString(img.Status.AvailableOn, key)
		if imageAvailableOnAnyProvider(img) {
			return
		}
		img.Status.Ready = false
		img.Status.Phase = infravirtrigaudiov1beta1.ImagePhaseFailed
		img.Status.Message = msg
		meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
			Type:               infravirtrigaudiov1beta1.VMImageConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             imageReasonInvalidSource,
			Message:            msg,
			ObservedGeneration: img.Generation,
		})
	})
}

// hasLegacyImagePrepareState reports whether status carries prepare state
// written by a release that keyed it by the bare Provider name: a
// providerStatus key or availableOn element that is not a
// "<namespace>/<name>" identity, or the image-wide prepareTaskRef.
func hasLegacyImagePrepareState(status *infravirtrigaudiov1beta1.VMImageStatus) bool {
	if status.PrepareTaskRef != "" {
		return true
	}
	for key := range status.ProviderStatus {
		if !isImageProviderKey(key) {
			return true
		}
	}
	for _, p := range status.AvailableOn {
		if !isImageProviderKey(p) {
			return true
		}
	}
	return false
}

// migrateLegacyImagePrepareState migrates the prepare state an earlier release
// recorded on vmImage (see hasLegacyImagePrepareState) in one conflict-safe
// status write; it does nothing when there is none.
//
// Before VMImages could be shared across namespaces by grant, the entries were
// in practice written through the Provider of the same name in the VMImage's
// own namespace. A bare-name entry is therefore re-keyed to
// "<VMImage namespace>/<name>" only when that Provider exists, and dropped
// otherwise: it can never satisfy a Provider of that name in another
// namespace. The writer's UID is unknown, so a re-keyed entry carries none and
// is re-validated through its Provider before a VM is created from it (see
// EnsureImageOnProvider). That also covers an entry an earlier release wrote
// through an ungranted cross-namespace reference to a same-named Provider. The
// image-wide prepareTaskRef cannot be attributed to a Provider: it is cleared,
// never polled, and the (idempotent) prepare is issued again.
func (r *VirtualMachineReconciler) migrateLegacyImagePrepareState(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
) error {
	if !hasLegacyImagePrepareState(&vmImage.Status) {
		return nil
	}
	logger := log.FromContext(ctx)
	return r.writeImageStatusE(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) error {
		if !hasLegacyImagePrepareState(&img.Status) {
			return nil
		}
		owners, err := r.legacyImageEntryOwners(ctx, img)
		if err != nil {
			return err
		}
		migrated, dropped := migrateLegacyImageStatus(img, owners)
		logger.Info("Migrated VMImage prepare state recorded under bare Provider names",
			"image", client.ObjectKeyFromObject(img).String(), "migrated", migrated, "dropped", dropped)
		return nil
	})
}

// legacyImageEntryOwners returns, for each bare Provider name in img's
// providerStatus keys and availableOn, the identity key of the Provider of
// that name in img's namespace — only for those that exist.
func (r *VirtualMachineReconciler) legacyImageEntryOwners(
	ctx context.Context,
	img *infravirtrigaudiov1beta1.VMImage,
) (map[string]string, error) {
	names := map[string]struct{}{}
	for key := range img.Status.ProviderStatus {
		if !isImageProviderKey(key) {
			names[key] = struct{}{}
		}
	}
	for _, p := range img.Status.AvailableOn {
		if !isImageProviderKey(p) {
			names[p] = struct{}{}
		}
	}
	owners := make(map[string]string, len(names))
	for name := range names {
		p := &infravirtrigaudiov1beta1.Provider{}
		err := r.Get(ctx, types.NamespacedName{Namespace: img.Namespace, Name: name}, p)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("get Provider %s/%s to migrate the prepare state of VMImage %s/%s: %w",
				img.Namespace, name, img.Namespace, img.Name, err)
		}
		owners[name] = imageProviderKey(p)
	}
	return owners, nil
}

// migrateLegacyImageStatus rewrites img's legacy prepare state in place:
// every bare-name providerStatus entry is moved to owners[name] (without a
// providerUID or task ref, so it is re-validated before use) or dropped when
// the name has no owner; an entry already present under the identity key
// wins. availableOn is rewritten the same way, keeping a migrated element only
// while its entry is available. The image-wide prepareTaskRef is cleared. When
// nothing is available any more, Ready is cleared with reason
// PrepareStateDropped. It returns the migrated and dropped names, sorted.
func migrateLegacyImageStatus(img *infravirtrigaudiov1beta1.VMImage, owners map[string]string) (migrated, dropped []string) {
	var bare []string
	for key := range img.Status.ProviderStatus {
		if !isImageProviderKey(key) {
			bare = append(bare, key)
		}
	}
	sort.Strings(bare)
	for _, name := range bare {
		ps := img.Status.ProviderStatus[name]
		delete(img.Status.ProviderStatus, name)
		key, ok := owners[name]
		if !ok {
			dropped = append(dropped, name)
			continue
		}
		migrated = append(migrated, name)
		if _, exists := img.Status.ProviderStatus[key]; exists {
			continue
		}
		ps.ProviderUID = ""
		ps.TaskRef = ""
		img.Status.ProviderStatus[key] = ps
	}

	var availableOn []string
	for _, p := range img.Status.AvailableOn {
		if !isImageProviderKey(p) {
			key, ok := owners[p]
			if !ok {
				continue
			}
			if ps, exists := img.Status.ProviderStatus[key]; !exists || !ps.Available {
				continue
			}
			p = key
		}
		availableOn = appendDedup(availableOn, p)
	}
	img.Status.AvailableOn = availableOn
	img.Status.PrepareTaskRef = ""

	if img.Status.Ready && !imageAvailableOnAnyProvider(img) {
		msg := "prepare state recorded by an earlier release under a Provider name that is not a Provider in this " +
			"namespace was dropped; the image is prepared again on first use"
		img.Status.Ready = false
		img.Status.Phase = infravirtrigaudiov1beta1.ImagePhasePending
		img.Status.Message = msg
		meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
			Type:               infravirtrigaudiov1beta1.VMImageConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             imageReasonPrepareStateDropped,
			Message:            msg,
			ObservedGeneration: img.Generation,
		})
	}
	return migrated, dropped
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
	return r.writeImageStatusE(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) error {
		mutate(img)
		return nil
	})
}

// writeImageStatusE is writeImageStatus for a mutate that can fail: an error
// from mutate aborts the write (nothing is updated) and is returned wrapped.
// Each attempt reads the VMImage into a fresh object, so nothing a failed
// attempt changed carries over.
func (r *VirtualMachineReconciler) writeImageStatusE(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	mutate func(*infravirtrigaudiov1beta1.VMImage) error,
) error {
	key := types.NamespacedName{Name: vmImage.Name, Namespace: vmImage.Namespace}
	var latest *infravirtrigaudiov1beta1.VMImage
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest = &infravirtrigaudiov1beta1.VMImage{}
		if getErr := r.Get(ctx, key, latest); getErr != nil {
			return getErr
		}
		if mutateErr := mutate(latest); mutateErr != nil {
			return mutateErr
		}
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

// removeString returns list without any element equal to s, preserving order.
func removeString(list []string, s string) []string {
	out := list[:0:0]
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// imageEnsureResultToReconcile is a small helper used by reconcileVM to turn the
// EnsureImageOnProvider result into a requeue ctrl.Result. It exists so the
// requeue cadence lives in one place next to imagePrepareRequeueAfter.
func imageEnsureResultToReconcile() ctrl.Result {
	return ctrl.Result{RequeueAfter: imagePrepareRequeueAfter}
}
