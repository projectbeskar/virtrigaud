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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
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
	// imageReasonProviderUIDMissing marks a VM create held because the
	// Provider's entry is available but records no providerUID, and
	// Prepare.OnMissing (Fail, Wait) forbids the prepare that would re-validate
	// it.
	imageReasonProviderUIDMissing = "ProviderUIDMissing"
	// imageReasonSourceDigestMissing marks a VM create held because the
	// Provider's entry is available but records no sourceDigest (it was
	// prepared by an earlier release), and Prepare.OnMissing (Fail, Wait)
	// forbids the prepare that would re-validate it (ADR-0009 D8).
	imageReasonSourceDigestMissing = "SourceDigestMissing"
	// imageReasonProviderLacksArtifactIdentity marks a VM create held because
	// the Provider supports image import but does not report prepared-image
	// artifact identity; no prepare is sent to it (ADR-0009 D7).
	imageReasonProviderLacksArtifactIdentity = "ProviderLacksArtifactIdentity"
	// imageReasonArtifactConflict marks a VM create held because the provider
	// found an artifact at the name derived for this VMImage and its source
	// that was not prepared for this VMImage, and refused to use or replace it
	// (ADR-0009 D4).
	imageReasonArtifactConflict = "ArtifactConflict"
)

// eventReasonImagePrepareStateAccepted is the Warning event recorded on a VM
// whose VMImage has spec.prepare.onMissing Fail or Wait and an available
// status.providerStatus entry for the VM's Provider that was not recorded
// through that Provider object (the Provider was re-created, or the entry
// predates status.providerStatus[].providerUID). The prepare that would
// re-validate it is forbidden, so the entry is accepted and the Provider's
// current UID recorded.
const eventReasonImagePrepareStateAccepted = "ImagePrepareStateAccepted"

// eventReasonImageArtifactConflict is the Warning event recorded on a VMImage
// when a Provider's entry changes to the ArtifactConflict hold (ADR-0009 D11):
// once per change of state, not on every retry.
const eventReasonImageArtifactConflict = "ImageArtifactConflict"

// errImagePrepareHold is a sentinel returned by EnsureImageOnProvider when the
// image is not (yet) prepared and the VM must NOT proceed to create — for
// example when Prepare.OnMissing is Fail or Wait. reconcileVM translates it into
// a requeue (not an error), because the condition is recorded on the VMImage and
// retrying forever as an error would only add log/metric noise. Callers compare
// with errors.Is.
var errImagePrepareHold = errors.New("image prepare: holding VM create")

// errImageCRDOutdated is the hold (errors.Is errImagePrepareHold) returned while
// the installed CRDs cannot record or read per-Provider prepare state
// (VMImageCRDFeatureReporter). No provider is called and the VMImage status is
// not written.
var errImageCRDOutdated = fmt.Errorf("%w: the installed CRDs lack VMImage status.providerStatus[].providerUID, taskRef "+
	"or sourceDigest, or Provider status.reportedCapabilities.supportsImageArtifactIdentity; upgrade the CRDs", errImagePrepareHold)

// errImageArtifactNotConfirmed is returned (wrapped) when a provider answered
// an image prepare without confirming the requested identity (ADR-0009 D7):
// no artifact stamp in the answer (a provider older than ADR-0009, one whose
// reported capability is stale, or deprecated legacy mode), or one for another
// VMImage UID or source digest. Such an answer is never recorded, so no VM is
// created from it.
var errImageArtifactNotConfirmed = errors.New("the provider did not confirm the prepared image's identity " +
	"(no artifact stamp for this VMImage and its spec.source); the answer is not used")

// imageCRDOutdatedRequeueAfter re-checks a create held on errImageCRDOutdated.
// The CRD changes only with an upgrade, and the feature checker re-reads it
// once a minute.
const imageCRDOutdatedRequeueAfter = 30 * time.Second

// imageArtifactIdentityHoldRequeueAfter re-checks a create held because its
// Provider does not report artifact identity. It clears only when the
// provider is upgraded and its Provider reports the capability.
const imageArtifactIdentityHoldRequeueAfter = 30 * time.Second

// imageArtifactConflictRequeueAfter re-tries a create held on an artifact
// conflict. Only an operator can clear it (ADR-0009 D11).
const imageArtifactConflictRequeueAfter = 5 * time.Minute

// imageArtifactInProgressRequeueAfter re-tries a create whose artifact is
// still being prepared by another request: every retry is a prepare call
// (the provider probes its image location), so it is slower than the poll of
// a task this Provider owns.
const imageArtifactInProgressRequeueAfter = 30 * time.Second

// imageHoldError is a hold (errors.Is(err, errImagePrepareHold)) that carries
// the interval after which the held create is retried.
type imageHoldError struct {
	// msg says why the create is held; it is shown on the VM.
	msg string
	// requeueAfter is when to retry; 0 uses imagePrepareRequeueAfter.
	requeueAfter time.Duration
}

// Error implements error.
func (e *imageHoldError) Error() string { return errImagePrepareHold.Error() + ": " + e.msg }

// Is makes errors.Is(err, errImagePrepareHold) true.
func (e *imageHoldError) Is(target error) bool { return target == errImagePrepareHold }

// imageHoldRequeueAfter returns when to retry a create held on err (a hold).
func imageHoldRequeueAfter(err error) time.Duration {
	var hold *imageHoldError
	switch {
	case errors.As(err, &hold) && hold.requeueAfter > 0:
		return hold.requeueAfter
	case errors.Is(err, errImageCRDOutdated):
		return imageCRDOutdatedRequeueAfter
	}
	return imagePrepareRequeueAfter
}

// VMImageCRDFeatureReporter reports whether the installed CRDs lack a field
// image preparation records or reads: VMImage
// status.providerStatus[].providerUID, taskRef or sourceDigest, or Provider
// status.reportedCapabilities.supportsImageArtifactIdentity.
// *VMCRDFeatureChecker implements it.
type VMImageCRDFeatureReporter interface {
	// VMImagePrepareStateMissing is true only when a CRD verifiably lacks
	// one of them (not when it cannot be read).
	VMImagePrepareStateMissing(ctx context.Context) bool
}

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

// imageSourceDigest returns the ADR-0009 D2 digest of vmImage's current
// spec.source (imageartifact.SourceDigest): the identity of the content an
// entry must have been prepared for to be used.
func imageSourceDigest(vmImage *infravirtrigaudiov1beta1.VMImage) (string, error) {
	digest, err := imageartifact.SourceDigest(vmImage.Spec.Source)
	if err != nil {
		return "", fmt.Errorf("compute the source digest of VMImage %s/%s: %w", vmImage.Namespace, vmImage.Name, err)
	}
	return digest, nil
}

// imageEntryForSource reports whether the providerStatus entry ps was
// prepared for the spec.source whose digest is digest (ADR-0009 D8). An entry
// without a sourceDigest (recorded by an earlier release) is for no source: it
// is never used, and is prepared again (or held) before a create.
func imageEntryForSource(ps infravirtrigaudiov1beta1.ProviderImageStatus, digest string) bool {
	return ps.SourceDigest != "" && ps.SourceDigest == digest
}

// imagePrepareRequest returns the ADR-0009 D7 identity request that prepares
// vmImage's current spec.source (whose digest is digest) through provider.
//
// TargetName is left EMPTY: the provider derives the artifact name from the
// identity, and a provider older than ADR-0009 (one whose reported capability
// is stale) refuses an empty target name instead of importing under a bare
// name. ImageJSON is the JSON-encoded spec — what the provider-side parsers
// consume ({"source":{...},"prepare":{...}}) — without
// spec.consumerNamespaceSelector, which is operator-side policy (which
// namespaces may use the image), not image data. An empty StorageHint lets
// the provider pick its default storage.
//
// A VMImage without a UID (never the case for an object read from the API
// server) cannot be prepared: its UID is what the artifact is named and
// stamped by.
func imagePrepareRequest(vmImage *infravirtrigaudiov1beta1.VMImage, provider *infravirtrigaudiov1beta1.Provider, digest string) (contracts.ImagePrepareRequest, error) {
	if vmImage.UID == "" {
		return contracts.ImagePrepareRequest{}, fmt.Errorf("VMImage %s/%s has no UID; refusing to prepare it without an identity",
			vmImage.Namespace, vmImage.Name)
	}
	imageSpec := vmImage.Spec
	imageSpec.ConsumerNamespaceSelector = nil
	imageJSON, err := json.Marshal(imageSpec)
	if err != nil {
		return contracts.ImagePrepareRequest{}, fmt.Errorf("marshal VMImage %s/%s spec for prepare: %w", vmImage.Namespace, vmImage.Name, err)
	}
	req := contracts.ImagePrepareRequest{
		ImageJSON:    string(imageJSON),
		TargetName:   "",
		StorageHint:  "",
		Image:        contracts.ObjectIdentity{UID: string(vmImage.UID), Namespace: vmImage.Namespace, Name: vmImage.Name},
		SourceDigest: digest,
	}
	// The Provider identity is informational (the stamp's preparedBy); it is
	// sent only with its UID, as the transport requires.
	if provider.UID != "" {
		req.Provider = contracts.ObjectIdentity{UID: string(provider.UID), Namespace: provider.Namespace, Name: provider.Name}
	}
	return req, nil
}

// EnsureImageOnProvider drives lazy, VM-create-driven image preparation for the
// image referenced by vm against provider (issue #154, PR-5; ADR-0005 as
// amended by ADR-0009).
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
// through that Provider object (imageEntryRecordedThrough) FOR THE CURRENT
// spec.source (imageEntryForSource, ADR-0009 D8), and polls only the prepare
// task recorded in that entry, through that Provider.
//
// The ADR-0009 D7 gate, in order:
//  1. the provider instance is not an ImagePreparer, or the Provider does not
//     advertise SupportsImageImport: fall through unchanged to the
//     by-reference create (ADR-0005 decision 2);
//  2. it supports import but does not advertise SupportsImageArtifactIdentity:
//     hold (ProviderLacksArtifactIdentity), and send nothing;
//  3. otherwise prepare with the image identity, the source digest and an
//     EMPTY target name, and record the answer only when its artifact stamp
//     echo confirms that identity (contracts.ImagePrepareResponse.ConfirmsIdentity).
//
// Return contract, consumed by reconcileVM:
//   - (false, nil):              nothing to do or already prepared — proceed to create.
//   - (true,  nil):              a prepare is in flight — requeue, do NOT create yet.
//   - (false, errImagePrepareHold): the image may not be prepared now (OnMissing
//     Fail/Wait, a Provider without artifact identity, an artifact conflict, an
//     artifact another request is preparing, an outdated CRD) — requeue without
//     creating (imageHoldRequeueAfter).
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

	// Gate step 1 — no regression for non-preparing providers. We require
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
	digest, err := imageSourceDigest(vmImage)
	if err != nil {
		return false, err
	}
	forSource := found && imageEntryForSource(entry, digest)

	// Idempotency — already prepared through this Provider object, for the
	// current spec.source. The provider's own PrepareImage is also idempotent,
	// but short-circuiting here avoids an RPC and a status write on the
	// steady-state reconcile. An entry prepared for another spec.source (or for
	// none: recorded by an earlier release) never satisfies it (ADR-0009 D8).
	if recordedThrough && entry.Available && forSource {
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
	// by-reference create path already consumes these directly — an entry left
	// from an earlier import-style source does not match this source's digest, so
	// overrideImageWithPreparedLocation ignores it — and a wrong path/template
	// still fails honestly at create time, which is where a missing backing
	// artifact belongs.
	if !imageSourceNeedsPrepare(vmImage) {
		logger.V(1).Info("Image source is already present on the provider (no import needed); proceeding to create",
			"provider", key, "image", vmImage.Name)
		return false, nil
	}

	// Every step below reads or writes the per-Provider prepare state. An
	// installed VMImage CRD older than the manager prunes providerUID, taskRef
	// and sourceDigest, so no entry would ever be trusted and every
	// asynchronous prepare would be issued again on each reconcile; a Provider
	// CRD without supportsImageArtifactIdentity would make every Provider look
	// like one without artifact identity. Hold (no provider call, no status
	// write) until the CRDs are upgraded; the CRD feature checker also fails
	// the manager's readiness meanwhile.
	if r.ImageCRDFeatures != nil && r.ImageCRDFeatures.VMImagePrepareStateMissing(ctx) {
		logger.Info("Holding image prepare: the installed CRDs lack the per-Provider prepare state fields; upgrade the CRDs",
			"provider", key, "image", vmImage.Name)
		return false, errImageCRDOutdated
	}

	// Honor OnMissing. Fail and Wait never prepare: they use an entry that is
	// already there, or hold.
	if action := imageMissingAction(vmImage); action != infravirtrigaudiov1beta1.ImageMissingActionImport {
		return false, r.ensureImageWithoutPrepare(ctx, vm, vmImage, provider, action, digest)
	}

	// Gate step 2 (ADR-0009 D7): a provider that imports images but does not
	// name, stamp and verify its artifacts by the VMImage's identity could
	// hand this VMImage an artifact another VMImage (another tenant's, even)
	// prepared under the same bare name. Nothing is sent to it: the create is
	// held until the provider is upgraded.
	if !providerAdvertisesImageArtifactIdentity(provider) {
		logger.Info("Holding image prepare: the Provider supports image import but not prepared-image artifact identity; upgrade the provider",
			"provider", key, "image", vmImage.Name)
		return false, r.holdProviderLacksArtifactIdentity(ctx, vmImage, provider)
	}

	// Poll this Provider's own outstanding prepare task, through this Provider.
	// A task recorded through another Provider object is never polled: a task
	// ID means nothing to a different provider backend, and its completion
	// proves nothing about this one. Nor is a task started for an earlier
	// spec.source: it prepares other content. The prepare is issued again
	// instead.
	completedTask := ""
	if found && entry.TaskRef != "" {
		switch {
		case !recordedThrough:
			logger.Info("Discarding an image prepare task recorded through another Provider object; preparing again",
				"provider", key, "image", vmImage.Name, "taskRef", entry.TaskRef,
				"recordedUID", entry.ProviderUID, "providerUID", string(provider.UID))
		case !forSource:
			logger.Info("Discarding an image prepare task started for an earlier spec.source; preparing the current one",
				"provider", key, "image", vmImage.Name, "taskRef", entry.TaskRef)
		default:
			done, terr := providerInstance.IsTaskComplete(ctx, entry.TaskRef)
			if terr != nil && !done {
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
			// The task ended (a failed task ends too). Its end does not prove
			// the artifact (ADR-0009 D7): the prepare is sent again — it is
			// idempotent — and only its confirmed stamp echo is recorded. A
			// failed import is found abandoned and imported again by the
			// provider.
			if terr != nil {
				logger.Info("Image prepare task failed; asking the provider again",
					"provider", key, "image", vmImage.Name, "taskRef", entry.TaskRef, "error", terr.Error())
			} else {
				logger.Info("Image prepare task completed; confirming the prepared artifact",
					"provider", key, "image", vmImage.Name, "taskRef", entry.TaskRef)
			}
			completedTask = entry.TaskRef
		}
	}
	if found && entry.Available && (!recordedThrough || !forSource) {
		logger.Info("Re-validating prepare state that was not recorded through this Provider object for the current spec.source",
			"provider", key, "image", vmImage.Name, "recordedUID", entry.ProviderUID, "providerUID", string(provider.UID),
			"digestRecorded", entry.SourceDigest != "")
	}

	// Gate step 3: prepare with the identity.
	req, err := imagePrepareRequest(vmImage, provider, digest)
	if err != nil {
		return false, err
	}
	res, perr := r.prepareImageOnce(ctx, ip, vmImage, provider, req, completedTask)
	if perr != nil {
		return false, perr
	}
	// The prepare's outcome was recorded on the VMImage once, by whichever
	// reconcile issued it (or was found already recorded): reflect it on this
	// reconcile's copy, so the create consumes the prepared location.
	res.status.DeepCopyInto(&vmImage.Status)
	return res.inFlight, nil
}

// ensureImageWithoutPrepare handles a VM create when Prepare.OnMissing
// (action: Fail or Wait) forbids the prepare. It returns nil when the create
// may proceed from provider's entry, and otherwise a hold (errImagePrepareHold)
// recorded on the VMImage.
//
// An available entry for provider that is not usable as is:
//   - records no providerUID: it cannot be tied to any Provider object, so it
//     is never trusted (reason ProviderUIDMissing);
//   - records no sourceDigest (prepared by an earlier release): it cannot be
//     matched to spec.source, so it is never used (reason SourceDigestMissing,
//     ADR-0009 D8);
//   - records another sourceDigest: the image is not prepared for the current
//     spec.source, which is the normal Fail/Wait case;
//   - was prepared for the current spec.source through a previous object of
//     this Provider (another, non-empty UID): it is accepted with a warning
//     event and the current UID recorded — as for a VM's bound Provider
//     (#341), the same namespace owns the old and the new Provider.
//
// Each hold message tells the image's owner to set spec.prepare.onMissing to
// Import, so this controller re-validates the entry through the Provider. None
// ever asks anyone to write VMImage status: this controller is its single
// writer (ADR-0005). Only creates wait on this: a VM that exists never
// prepares its image.
func (r *VirtualMachineReconciler) ensureImageWithoutPrepare(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	action infravirtrigaudiov1beta1.ImageMissingAction,
	digest string,
) error {
	logger := log.FromContext(ctx)
	key := imageProviderKey(provider)
	entry, found := vmImage.Status.ProviderStatus[key]
	otherSource := ""
	if found && entry.Available {
		switch {
		case entry.ProviderUID == "":
			logger.Info("Holding VM create: prepare state without a Provider UID, and Prepare.OnMissing forbids re-validating it",
				"provider", key, "image", vmImage.Name, "onMissing", string(action))
			return r.holdImage(ctx, vmImage, action, imageReasonProviderUIDMissing, fmt.Sprintf(
				"status.providerStatus[%q] is available but records no providerUID, so it cannot be tied to that Provider "+
					"and is not used; set spec.prepare.onMissing to Import so the controller re-validates it through "+
					"the Provider", key))
		case entry.SourceDigest == "":
			logger.Info("Holding VM create: prepare state without a source digest, and Prepare.OnMissing forbids re-validating it",
				"provider", key, "image", vmImage.Name, "onMissing", string(action))
			return r.holdImage(ctx, vmImage, action, imageReasonSourceDigestMissing, fmt.Sprintf(
				"status.providerStatus[%q] is available but records no sourceDigest (it was prepared by an earlier "+
					"release), so it cannot be matched to spec.source and is not used; set spec.prepare.onMissing to "+
					"Import so the controller re-validates it through the Provider", key))
		case entry.SourceDigest != digest:
			otherSource = " for the current spec.source (it was prepared for an earlier one)"
		case !imageEntryRecordedThrough(entry, provider):
			logger.Info("WARNING: accepting prepare state recorded through a previous object of this Provider; "+
				"Prepare.OnMissing forbids the prepare that would re-validate it",
				"provider", key, "image", vmImage.Name, "recordedUID", entry.ProviderUID, "providerUID", string(provider.UID),
				"onMissing", string(action))
			r.recordEvent(vm, corev1.EventTypeWarning, eventReasonImagePrepareStateAccepted, fmt.Sprintf(
				"VMImage %s/%s: accepted prepare state for Provider %s recorded through a previous object of it (uid %s, now %s); "+
					"spec.prepare.onMissing=%s forbids re-validating it",
				vmImage.Namespace, vmImage.Name, key, entry.ProviderUID, provider.UID, action))
			return r.acceptImageEntry(ctx, vmImage, provider)
		}
	}

	// Fail records a terminal-ish condition and holds; Wait holds pending an
	// out-of-band preparer without erroring. Both return errImagePrepareHold so
	// reconcileVM requeues instead of creating.
	if action == infravirtrigaudiov1beta1.ImageMissingActionFail {
		logger.Info("VMImage Prepare.OnMissing=Fail and image not prepared on provider; not preparing",
			"provider", key, "image", vmImage.Name)
		return r.holdImage(ctx, vmImage, action, imageReasonMissingOnProvider,
			fmt.Sprintf("image not available on provider %q%s and Prepare.OnMissing=Fail", key, otherSource))
	}
	logger.Info("VMImage Prepare.OnMissing=Wait and image not prepared on provider; waiting (not preparing)",
		"provider", key, "image", vmImage.Name)
	return r.holdImage(ctx, vmImage, action, imageReasonWaitingForImage,
		fmt.Sprintf("image not available on provider %q%s; waiting (Prepare.OnMissing=Wait)", key, otherSource))
}

// issueImagePrepare sends req (an ADR-0009 identity request) through ip and
// records the outcome in provider's ProviderStatus entry. It runs inside
// prepareImageOnce, so concurrent reconciles of one (image, Provider object,
// source) issue ONE call and ONE status write between them.
//
// Outcomes:
//   - an answer whose stamp echo confirms req's identity: an asynchronous
//     prepare's task is recorded with the digest (inFlight=true), a completed
//     prepare's location and digest are recorded as available;
//   - an answer that does not confirm it: nothing is recorded, and an error
//     wrapping errImageArtifactNotConfirmed is returned;
//   - Conflict (a foreign or unproven artifact at the derived name): the
//     ArtifactConflict hold is recorded, with an event on the VMImage when the
//     entry changes to it;
//   - InProgress (another request is preparing the artifact): a hold, nothing
//     recorded;
//   - InvalidSpec (the provider rejected the source): recorded on the VMImage.
//
// completedTask is the task whose end this call confirms ("" for a fresh
// prepare): an outcome is recorded only while the entry still records it, so
// a newer prepare is never overwritten by the confirmation of an older one;
// inFlight is then true, to poll whatever is recorded.
//
// Every outcome is counted in virtrigaud_image_prepare_artifact_total, except
// the reuse that confirms this Provider's own completed task (its import was
// counted when it started).
func (r *VirtualMachineReconciler) issueImagePrepare(
	ctx context.Context,
	ip contracts.ImagePreparer,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	req contracts.ImagePrepareRequest,
	completedTask string,
) (inFlight bool, err error) {
	logger := log.FromContext(ctx)
	key := imageProviderKey(provider)
	providerType := string(provider.Spec.Type)
	logger.Info("Triggering image prepare on provider", "provider", key, "image", vmImage.Name,
		"imageUID", req.Image.UID, "sourceDigest", req.SourceDigest, "confirmingTask", completedTask)
	resp, perr := ip.PrepareImage(ctx, req)
	if perr != nil {
		switch {
		case contracts.IsConflict(perr):
			metrics.RecordImagePrepareArtifactOutcome(providerType, metrics.ImageArtifactOutcomeConflict)
			return false, r.holdArtifactConflict(ctx, vmImage, provider, perr)
		case contracts.IsInProgress(perr):
			metrics.RecordImagePrepareArtifactOutcome(providerType, metrics.ImageArtifactOutcomeInProgress)
			logger.Info("The prepared image is still being prepared by another request; waiting",
				"provider", key, "image", vmImage.Name, "detail", providerErrorMessage(perr))
			return false, &imageHoldError{
				msg: fmt.Sprintf("the image is still being prepared at the image location of provider %q by another "+
					"request; waiting for it", key),
				requeueAfter: imageArtifactInProgressRequeueAfter,
			}
		case contracts.IsInvalidSpec(perr):
			// The provider rejected the image source itself (e.g. a libvirt path
			// outside its allowed image directories), or — a provider older than
			// ADR-0009 whose reported capability is stale — the empty target
			// name. Record that on the VMImage so its owner sees WHY, not just
			// the VM; the caller backs off.
			if werr := r.markImageSourceRejected(ctx, vmImage, provider, perr); werr != nil {
				logger.Error(werr, "Failed to record rejected image source on VMImage",
					"provider", key, "image", vmImage.Name)
			}
		}
		return false, fmt.Errorf("prepare image %s on provider %s: %w", vmImage.Name, key, perr)
	}

	// ADR-0009 D7: record nothing the provider did not prove. Only the UID and
	// digest of the echo are compared; its namespace and name come from the
	// hypervisor and are never logged or shown.
	if !resp.ConfirmsIdentity(req) {
		logger.Error(errImageArtifactNotConfirmed, "Not recording an image prepare answer that does not confirm the requested identity",
			"provider", key, "image", vmImage.Name, "hasArtifact", resp.Artifact != nil,
			"uidMatches", resp.Artifact != nil && resp.Artifact.Image.UID == req.Image.UID,
			"digestMatches", resp.Artifact != nil && resp.Artifact.SourceDigest == req.SourceDigest)
		return false, fmt.Errorf("prepare image %s on provider %s: %w", vmImage.Name, key, errImageArtifactNotConfirmed)
	}
	switch {
	case !resp.Artifact.Reused:
		metrics.RecordImagePrepareArtifactOutcome(providerType, metrics.ImageArtifactOutcomeCreated)
	case completedTask == "":
		metrics.RecordImagePrepareArtifactOutcome(providerType, metrics.ImageArtifactOutcomeReused)
	}
	prepared := preparedLocation{id: resp.PreparedImageID, path: resp.PreparedImagePath, sourceDigest: resp.Artifact.SourceDigest}

	if resp.TaskRef != "" {
		// Asynchronous prepare — persist the task ref and the digest it
		// prepares IN THIS PROVIDER'S ENTRY and requeue to poll it through this
		// Provider. The prepared location (id/path) is already known at
		// trigger time (issue #154 PR-6 / #214), so it is stamped now even
		// though Available stays false until the task ends and a second call
		// confirms the artifact.
		if werr := r.writeImageStatusE(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) error {
			if cur := img.Status.ProviderStatus[key]; completedTask != "" && cur.TaskRef != completedTask {
				return errSkipImageStatusWrite // a newer prepare replaced the task being confirmed
			}
			if img.Status.ProviderStatus == nil {
				img.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{}
			}
			now := metav1.Now()
			ps := img.Status.ProviderStatus[key]
			ps.Available = false // not ready until the task completes and is confirmed
			ps.ProviderUID = string(provider.UID)
			ps.TaskRef = resp.TaskRef
			ps.SourceDigest = prepared.sourceDigest
			ps.ID = prepared.id
			ps.Path = prepared.path
			ps.LastUpdated = &now
			ps.Message = "image import in progress"
			img.Status.ProviderStatus[key] = ps
			img.Status.AvailableOn = removeString(img.Status.AvailableOn, key)
			img.Status.LastPrepareTime = &now
			markImageImporting(img, fmt.Sprintf("importing image into provider %q", key))
			return nil
		}); werr != nil {
			return false, werr
		}
		logger.Info("Image prepare started asynchronously; requeueing to poll",
			"provider", key, "image", vmImage.Name, "taskRef", resp.TaskRef,
			"preparedID", resp.PreparedImageID, "preparedPath", resp.PreparedImagePath)
		return true, nil
	}

	// Synchronous prepare (e.g. libvirt/vSphere import-on-call), or the
	// confirmation of a completed asynchronous one — stamp completion
	// immediately, recording the prepared location and digest, and let create
	// proceed.
	logger.Info("Image prepared on provider",
		"provider", key, "image", vmImage.Name, "reused", resp.Artifact.Reused,
		"preparedID", resp.PreparedImageID, "preparedPath", resp.PreparedImagePath)
	applied, werr := r.markImagePrepared(ctx, vmImage, provider, prepared, completedTask)
	if werr != nil {
		return false, werr
	}
	return !applied, nil
}

// holdArtifactConflict records that the provider refused the artifact at the
// name derived for this VMImage and its spec.source because it was not
// prepared for this VMImage (ADR-0009 D4; cause is the provider's Conflict).
// The entry and — while the image is available on no provider — the image
// get reason ArtifactConflict; a Warning event is recorded on the VMImage
// when the entry changes to that state. It returns the hold, retried after
// imageArtifactConflictRequeueAfter: an operator must act.
//
// The message is the manager's own, naming only this Provider, followed by
// the provider's (uniform) refusal, sanitized and length-capped
// (sanitizeProviderDetail): it names only the requester's artifact, so an
// operator can find it, and who else prepared it is in the provider's log
// only (ADR-0009 D11). The event lands in the VMImage's namespace, which may
// not be the requester's.
func (r *VirtualMachineReconciler) holdArtifactConflict(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	cause error,
) error {
	key := imageProviderKey(provider)
	msg := fmt.Sprintf("provider %q refuses to use or replace an existing artifact at the name derived for this VMImage "+
		"and its spec.source, because it was not prepared for this VMImage; VirtRigaud never adopts, overwrites "+
		"or deletes it: an operator must investigate and remove it (provider detail: %s)", key, sanitizeProviderDetail(cause))
	log.FromContext(ctx).Error(cause, "Holding VM create: prepared-image artifact conflict",
		"provider", key, "image", vmImage.Name)
	changed, err := r.markImageEntryHeld(ctx, vmImage, provider, imageReasonArtifactConflict,
		infravirtrigaudiov1beta1.ImagePhaseFailed, msg)
	if err != nil {
		return err
	}
	if changed && r.Recorder != nil {
		r.Recorder.Event(vmImage, corev1.EventTypeWarning, eventReasonImageArtifactConflict, msg)
	}
	return &imageHoldError{msg: msg, requeueAfter: imageArtifactConflictRequeueAfter}
}

// holdProviderLacksArtifactIdentity records that provider supports image
// import but does not report prepared-image artifact identity, so nothing is
// sent to it (ADR-0009 D7, gate step 2): the entry and — while the image is
// available on no provider — the image get reason
// ProviderLacksArtifactIdentity. It returns the hold, retried after
// imageArtifactIdentityHoldRequeueAfter.
func (r *VirtualMachineReconciler) holdProviderLacksArtifactIdentity(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
) error {
	key := imageProviderKey(provider)
	msg := fmt.Sprintf("provider %q supports image import but does not report prepared-image artifact identity "+
		"(status.reportedCapabilities.supportsImageArtifactIdentity), so this import-style image is not prepared "+
		"through it; upgrade the provider to a release that supports it (ADR-0009)", key)
	if _, err := r.markImageEntryHeld(ctx, vmImage, provider, imageReasonProviderLacksArtifactIdentity,
		infravirtrigaudiov1beta1.ImagePhasePending, msg); err != nil {
		return err
	}
	return &imageHoldError{msg: msg, requeueAfter: imageArtifactIdentityHoldRequeueAfter}
}

// holdImage records on the VMImage that a VM create is held on a Provider for
// reason — Ready=False, Phase Failed for OnMissing=Fail and Pending otherwise,
// and msg — and returns errImagePrepareHold wrapped with msg (or the status
// write error).
func (r *VirtualMachineReconciler) holdImage(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	action infravirtrigaudiov1beta1.ImageMissingAction,
	reason, msg string,
) error {
	phase := infravirtrigaudiov1beta1.ImagePhasePending
	if action == infravirtrigaudiov1beta1.ImageMissingActionFail {
		phase = infravirtrigaudiov1beta1.ImagePhaseFailed
	}
	if werr := r.writeImageStatus(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) {
		img.Status.Ready = false
		img.Status.Phase = phase
		img.Status.Message = msg
		meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
			Type:               infravirtrigaudiov1beta1.VMImageConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: img.Generation,
		})
	}); werr != nil {
		return werr
	}
	return fmt.Errorf("%w: %s", errImagePrepareHold, msg)
}

// prepareImageForCreate prepares vm's image on provider right before a create
// (EnsureImageOnProvider) and translates the outcome for reconcileVM. It is
// called only for a VM that is not bound yet, or that is being re-created
// because it no longer exists on the hypervisor: a VM that exists never
// prepares its image, so an image problem (a source the provider now refuses,
// a template deleted out of band, an entry dropped by the migration) never
// stops describe, power or reconfigure of a running VM.
//
// done is false when the create may proceed. Otherwise res and err are what
// reconcileVM returns: a refused consumer, a hold (see EnsureImageOnProvider),
// a prepare in flight, or a prepare error.
func (r *VirtualMachineReconciler) prepareImageForCreate(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	providerInstance contracts.Provider,
) (done bool, res ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	requeue, perr := r.EnsureImageOnProvider(ctx, vm, vmImage, provider, providerInstance)
	switch {
	case perr == nil && !requeue:
		return false, ctrl.Result{}, nil
	case perr == nil:
		// A prepare is in flight; surface a provisioning condition and requeue
		// to poll it. The VM is NOT created until the image is Ready on the
		// provider.
		logger.Info("Waiting for image prepare to complete before creating VM",
			"image", vmImage.Name, "provider", provider.Name)
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonTaskInProgress,
			fmt.Sprintf("Preparing image %s on provider %s", vmImage.Name, provider.Name))
		r.updateStatus(ctx, vm)
		return true, imageEnsureResultToReconcile(), nil
	case isConsumerNotAllowed(perr):
		res, err = r.refuseConsumer(ctx, vm, persisted, perr)
		return true, res, err
	case errors.Is(perr, errImagePrepareHold):
		// The prepare may not run now (see errImagePrepareHold); the reason is
		// recorded on the VMImage (except for an outdated CRD or an artifact
		// another request is preparing, which are not written to). Reflect it
		// on the VM and requeue without treating it as a reconcile error.
		logger.Info("Holding VM create: referenced image is not prepared and may not be prepared now",
			"image", vmImage.Name, "provider", provider.Name, "reason", perr.Error())
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonWaitingForDependencies,
			fmt.Sprintf("Image %s not prepared on provider %s: %s", vmImage.Name, provider.Name,
				strings.TrimPrefix(perr.Error(), errImagePrepareHold.Error()+": ")))
		r.updateStatus(ctx, vm)
		return true, ctrl.Result{RequeueAfter: imageHoldRequeueAfter(perr)}, nil
	default:
		reason, requeueAfter := providerFailureOutcome(perr)
		logger.Error(perr, "Failed to ensure image on provider - will retry",
			"image", vmImage.Name, "provider", provider.Name, "retryIn", requeueAfter)
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, reason,
			fmt.Sprintf("Image prepare failed: %s", sanitizeProviderDetail(perr)))
		metrics.RecordError(errReasonImagePrepare, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return true, ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
}

// imagePrepareResult is the outcome of one de-duplicated prepare
// (prepareImageOnce).
type imagePrepareResult struct {
	// inFlight is true while an asynchronous prepare task is outstanding: the
	// VM must wait and requeue to poll it.
	inFlight bool
	// status is the VMImage status with the outcome recorded (or as re-read,
	// when a prepare through this Provider object was already recorded). It is
	// shared by every reconcile of the flight: read it, never modify it.
	status *infravirtrigaudiov1beta1.VMImageStatus
}

// imagePrepareFlightKey identifies one prepare of vmImage (this object, at its
// current generation) for the spec.source whose digest is digest, through one
// Provider object, for prepareImageOnce.
func imagePrepareFlightKey(vmImage *infravirtrigaudiov1beta1.VMImage, provider *infravirtrigaudiov1beta1.Provider, digest string) string {
	return fmt.Sprintf("%s/%s@%s#%d|%s|%s|%s", vmImage.Namespace, vmImage.Name, vmImage.UID, vmImage.Generation,
		imageProviderKey(provider), provider.UID, digest)
}

// prepareImageOnce prepares vmImage through provider, de-duplicated within
// this manager: concurrent reconciles (of different VMs) preparing the same
// VMImage for the same spec.source through the same Provider object share ONE
// PrepareImage call AND the one status write that records its outcome
// (issueImagePrepare), so a multi-GB import is never started twice in
// parallel and the reconciles do not race each other to write the same
// outcome. The call first re-reads the VMImage and issues nothing when a
// prepare through this Provider object for req's source digest is already
// recorded (available, or with a task in flight other than completedTask, the
// task this call confirms); since the outcome is recorded before the flight
// ends, a reconcile arriving after it finds it there. A read error does not
// block the (idempotent) prepare; a VMImage that was deleted and re-created
// meanwhile is not prepared for its predecessor.
func (r *VirtualMachineReconciler) prepareImageOnce(
	ctx context.Context,
	ip contracts.ImagePreparer,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	req contracts.ImagePrepareRequest,
	completedTask string,
) (imagePrepareResult, error) {
	logger := log.FromContext(ctx)
	key := imageProviderKey(provider)
	v, err, shared := r.imagePrepares.Do(imagePrepareFlightKey(vmImage, provider, req.SourceDigest), func() (any, error) {
		fresh := &infravirtrigaudiov1beta1.VMImage{}
		if gerr := r.Get(ctx, client.ObjectKeyFromObject(vmImage), fresh); gerr != nil {
			logger.V(1).Info("Could not re-read the VMImage before preparing it; preparing anyway (idempotent)",
				"image", vmImage.Name, "error", gerr.Error())
		} else if err := checkSameImageObject(vmImage, fresh); err != nil {
			return nil, err
		} else if ps, ok := fresh.Status.ProviderStatus[key]; ok && imageEntryRecordedThrough(ps, provider) &&
			imageEntryForSource(ps, req.SourceDigest) && (ps.Available || (ps.TaskRef != "" && ps.TaskRef != completedTask)) {
			logger.V(1).Info("Image prepare already recorded for this Provider by a concurrent reconcile",
				"provider", key, "image", vmImage.Name, "available", ps.Available, "taskRef", ps.TaskRef)
			return imagePrepareResult{inFlight: !ps.Available, status: &fresh.Status}, nil
		}
		inFlight, perr := r.issueImagePrepare(ctx, ip, vmImage, provider, req, completedTask)
		if perr != nil {
			return nil, perr
		}
		return imagePrepareResult{inFlight: inFlight, status: vmImage.Status.DeepCopy()}, nil
	})
	if err != nil {
		return imagePrepareResult{}, err
	}
	if shared {
		logger.V(1).Info("Image prepare shared with a concurrent reconcile", "provider", key, "image", vmImage.Name)
	}
	res, ok := v.(imagePrepareResult)
	if !ok || res.status == nil {
		return imagePrepareResult{}, fmt.Errorf("prepare image %s on provider %s: unexpected result %T", vmImage.Name, key, v)
	}
	return res, nil
}

// checkSameImageObject returns an error when latest, a fresh read of vmImage,
// is another object: vmImage was deleted and re-created under the same name
// (a new UID). Prepare state decided for the previous object must never be
// recorded on — or consumed from — its successor, whose artifacts carry its
// own UID (ADR-0009 D1). A vmImage without a UID is not checked.
func checkSameImageObject(vmImage, latest *infravirtrigaudiov1beta1.VMImage) error {
	if vmImage.UID == "" || latest.UID == vmImage.UID {
		return nil
	}
	return fmt.Errorf("VMImage %s/%s was deleted and re-created (uid %s, now %s); not acting on its predecessor's prepare state",
		vmImage.Namespace, vmImage.Name, vmImage.UID, latest.UID)
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

// providerAdvertisesImageArtifactIdentity reports whether the Provider CR
// advertises SupportsImageArtifactIdentity (ADR-0009 D7): its image prepare
// names, stamps and verifies each artifact by the VMImage's UID and source
// digest, and never reuses one by bare name. A nil ReportedCapabilities reads
// as false, which holds import-style prepares — fail closed.
func providerAdvertisesImageArtifactIdentity(provider *infravirtrigaudiov1beta1.Provider) bool {
	caps := provider.Status.ReportedCapabilities
	return caps != nil && caps.SupportsImageArtifactIdentity
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
// (ImagePrepareResponse.PreparedImageID / PreparedImagePath) and the source
// digest its confirmed stamp echo carries (ADR-0009 D8).
type preparedLocation struct {
	id           string
	path         string
	sourceDigest string
}

// markImagePrepared stamps a completed prepare on provider: it records the
// provider's ProviderStatus entry (keyed by imageProviderKey) as available,
// recorded through provider's current UID, for loc.sourceDigest, clears the
// entry's task ref, adds the key to AvailableOn (deduped), and sets
// Ready/Phase=Ready. Ready is the OR across providers — any provider having
// the image Available makes the image Ready — while ProviderStatus/AvailableOn
// carry the per-provider truth.
//
// loc is the prepared location and digest of an answer that confirmed the
// requested identity (ADR-0009 D7), recorded as is so create can consume the
// location instead of re-resolving the source (issue #154 PR-6 / #214), and
// only for the spec.source it was prepared for (D8).
//
// completedTask, when set, is the task whose completion is being recorded:
// if the entry meanwhile records a different task (a newer prepare replaced
// it), nothing is written and applied is false, so the newer task is not
// cleared and is polled on its own. If another reconcile confirming the same
// task already recorded its completion, nothing is written and applied is
// true.
func (r *VirtualMachineReconciler) markImagePrepared(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	loc preparedLocation,
	completedTask string,
) (applied bool, err error) {
	key := imageProviderKey(provider)
	err = r.writeImageStatusE(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) error {
		applied = false
		if cur := img.Status.ProviderStatus[key]; completedTask != "" && cur.TaskRef != completedTask {
			// A newer prepare replaced the task (not applied), or a concurrent
			// reconcile confirming the same task already recorded its completion.
			applied = cur.Available && cur.TaskRef == "" && cur.ProviderUID == string(provider.UID) &&
				imageEntryForSource(cur, loc.sourceDigest)
			return errSkipImageStatusWrite
		}
		applied = true
		if img.Status.ProviderStatus == nil {
			img.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{}
		}
		now := metav1.Now()
		ps := img.Status.ProviderStatus[key]
		ps.Available = true
		ps.ProviderUID = string(provider.UID)
		ps.TaskRef = ""
		ps.SourceDigest = loc.sourceDigest
		ps.ID = loc.id
		ps.Path = loc.path
		ps.LastUpdated = &now
		ps.Message = "image prepared"
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
		return nil
	})
	return applied, err
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
// such as a libvirt path outside the provider's allowed image directories):
// the entry and — while the image is available on no provider — the image get
// reason InvalidSource (markImageEntryHeld). The provider's reason follows the
// manager's message, sanitized and length-capped (sanitizeProviderDetail): the
// owner needs it to fix the source, and a shared VMImage's status is read in
// other namespaces too.
func (r *VirtualMachineReconciler) markImageSourceRejected(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	cause error,
) error {
	key := imageProviderKey(provider)
	msg := fmt.Sprintf("image source rejected by provider %q: %s", key, sanitizeProviderDetail(cause))
	_, err := r.markImageEntryHeld(ctx, vmImage, provider, imageReasonInvalidSource, infravirtrigaudiov1beta1.ImagePhaseFailed, msg)
	return err
}

// markImageEntryHeld records that no VM can be created from provider's entry
// for reason (with msg): the entry is not available, is recorded through
// provider's current UID, has no task and no source digest (it records no
// prepared artifact), and carries msg; the key leaves AvailableOn. The
// image-level Phase, Ready, Message and Ready condition (reason) change only
// while the image is available on no provider, so a problem on one Provider
// never masks the image being Ready on another (Ready is the OR across
// providers).
//
// changed reports whether the entry changed (it was not already held with
// msg), for callers that signal only a change of state. An unchanged entry is
// not rewritten, so a hold retried on every requeue does not write the
// VMImage each time.
func (r *VirtualMachineReconciler) markImageEntryHeld(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	provider *infravirtrigaudiov1beta1.Provider,
	reason string,
	phase infravirtrigaudiov1beta1.ImagePhase,
	msg string,
) (changed bool, err error) {
	key := imageProviderKey(provider)
	uid := string(provider.UID)
	err = r.writeImageStatusE(ctx, vmImage, func(img *infravirtrigaudiov1beta1.VMImage) error {
		ps, found := img.Status.ProviderStatus[key]
		changed = !found || ps.Available || ps.ProviderUID != uid || ps.TaskRef != "" || ps.SourceDigest != "" ||
			ps.Message != msg
		if changed {
			if img.Status.ProviderStatus == nil {
				img.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{}
			}
			now := metav1.Now()
			ps.Available = false
			ps.ProviderUID = uid
			ps.TaskRef = ""
			ps.SourceDigest = ""
			ps.Message = msg
			ps.LastUpdated = &now
			img.Status.ProviderStatus[key] = ps
		}
		img.Status.AvailableOn = removeString(img.Status.AvailableOn, key)
		if imageAvailableOnAnyProvider(img) {
			return nil
		}
		img.Status.Ready = false
		img.Status.Phase = phase
		img.Status.Message = msg
		meta.SetStatusCondition(&img.Status.Conditions, metav1.Condition{
			Type:               infravirtrigaudiov1beta1.VMImageConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: img.Generation,
		})
		return nil
	})
	return changed, err
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
			return errSkipImageStatusWrite // migrated by a concurrent reconcile
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

// imageStatusRetry is the conflict backoff of every VMImage status write
// (writeImageStatusE). A shared VMImage is written by the reconciles of every
// VM that prepares it, on every Provider, so it allows more attempts than
// retry.DefaultRetry (5 at a fixed ~10ms) and uses full jitter, so writers
// that conflicted once do not retry in lockstep and conflict again. Worst case
// it waits a few seconds in total, still well inside one reconcile.
var imageStatusRetry = wait.Backoff{
	Steps:    10,
	Duration: 10 * time.Millisecond,
	Factor:   1.5,
	Jitter:   1.0,
	Cap:      time.Second,
}

// errSkipImageStatusWrite, returned by a writeImageStatusE mutate, means the
// status needs no change: nothing is written, and the status that was read is
// mirrored onto the caller's copy.
var errSkipImageStatusWrite = errors.New("VMImage status needs no change")

// writeImageStatusE is writeImageStatus for a mutate that can fail: an error
// from mutate aborts the write (nothing is updated) and is returned wrapped,
// except errSkipImageStatusWrite (see there). Each attempt reads the VMImage
// into a fresh object and re-applies mutate to it, so nothing a failed attempt
// changed carries over and no concurrent update is lost; a mutate that changes
// nothing writes nothing. Conflicts are retried with imageStatusRetry. Nothing
// is written when the VMImage was deleted and re-created since vmImage was read
// (checkSameImageObject): what the caller decided was decided for another
// object.
func (r *VirtualMachineReconciler) writeImageStatusE(
	ctx context.Context,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	mutate func(*infravirtrigaudiov1beta1.VMImage) error,
) error {
	key := types.NamespacedName{Name: vmImage.Name, Namespace: vmImage.Namespace}
	var latest *infravirtrigaudiov1beta1.VMImage
	if err := retry.RetryOnConflict(imageStatusRetry, func() error {
		latest = &infravirtrigaudiov1beta1.VMImage{}
		if getErr := r.Get(ctx, key, latest); getErr != nil {
			return getErr
		}
		if sameErr := checkSameImageObject(vmImage, latest); sameErr != nil {
			return sameErr
		}
		before := latest.Status.DeepCopy()
		if mutateErr := mutate(latest); mutateErr != nil {
			return mutateErr
		}
		if equality.Semantic.DeepEqual(before, &latest.Status) {
			return nil // nothing to write
		}
		return r.Status().Update(ctx, latest)
	}); err != nil && !errors.Is(err, errSkipImageStatusWrite) {
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
