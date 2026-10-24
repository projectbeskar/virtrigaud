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
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Single-host adoption that STAMPS the adopted VM with its new owner.
//
// Every VirtRigaud provider stamps the VMs it creates with their
// VirtualMachine (vSphere ExtraConfig, libvirt domain metadata, Proxmox
// description), and owner-checked calls refuse a VM stamped for another
// VirtualMachine (the Proxmox Delete answers VM_NOT_OWNED and leaves it).
// Adoption used to leave the adopted VM's stamp as it was: an unstamped VM
// stayed unstamped (so another Provider object's adoption still saw it as
// unowned), and a VM stamped by a VirtualMachine that was detached with
// orphan-on-delete and then adopted again (adopt-stamped-vms) kept that
// VirtualMachine's stamp, so its new VirtualMachine's delete left it in place.
//
// Through a provider that reports Capabilities.SupportsOwnerTransfer, a
// single-host discovery now hands every VM it adopts to its VirtualMachine:
//
//  1. Which VMs may be stamped (cross-tenant safety). Only VMs single-host
//     adoption would adopt anyway (unmanagedProviderVMs' rules: never a VM
//     bound to a VirtualMachine of this Provider, never one stamped with the
//     UID of a VirtualMachine that exists in any namespace, never a template,
//     an image artifact or a VM whose stamp cannot be read), never a VM whose
//     id is the status.id of a VirtualMachine bound through ANOTHER
//     single-host Provider object of the same type and endpoint (a VM that
//     Provider object created before owner stamps, or adopted before this
//     release, carries no stamp of its VirtualMachine; stamping it would take
//     it away; skipBoundElsewhere), and of the rest:
//     - an UNSTAMPED VM;
//     - a VM stamped only with UIDs of no VirtualMachine in this cluster, only
//     with the Provider's adopt-stamped-vms opt-in (unchanged), and only after
//     an UNCACHED list of every VirtualMachine confirms that none has one of
//     those UIDs (a UID is never reused, so absence is final). Those UIDs are
//     the only stamps the provider may replace (replaceable_owner_uids).
//  2. Order. The VirtualMachine is created first, without status (it needs a
//     UID to be stamped with), annotated with the VM's id
//     (AdoptedIDAnnotation); it is labelled adopted, so the VirtualMachine
//     controller waits for its status.id instead of creating a VM. Right
//     before the transfer it is re-read uncached and must still be waiting
//     for that VM. TransferOwner then stamps the VM, compare-and-swap on the
//     provider (the VM must still carry an expected stamp: none, or one of
//     the replaceable ones), and only after it succeeded is status.id written
//     (re-read, retried on conflict, only while the VirtualMachine is still
//     waiting).
//  3. Every intermediate state is safe and is completed or undone by the next
//     discovery — whatever crashed or failed in between:
//     - created, not stamped: deleting the VirtualMachine makes no provider
//     call (no status.id, no create requested); the next discovery transfers
//     the owner to the waiting VirtualMachine (it is found by the annotation)
//     and binds it;
//     - stamped, not bound: the listing shows the VM stamped with the waiting
//     VirtualMachine's UID, and the binding is written (no second transfer).
//     Deleting the VirtualMachine in this window makes no provider call
//     either: the VM stays on the hypervisor, stamped with a UID that no
//     longer exists (adoptable again with adopt-stamped-vms);
//     - the provider refused the transfer for good (the VM is someone else's
//     now, gone, or replaced), or the listing shows the VM can no longer be
//     adopted by it: the waiting VirtualMachine is deleted again — re-read
//     first, preconditioned on its UID and resourceVersion, only while it is
//     still waiting; it has no status.id, so no provider call is made.
//
// Through an older provider (SupportsOwnerTransfer false) adoption is
// unchanged — the VirtualMachine is created and bound in one pass and the VM
// keeps its stamp — and the adoption status says the adopted VMs were not
// stamped. A provider whose capabilities cannot be read adopts nothing that
// discovery (fail closed: it might have been able to stamp).

const (
	// errReasonAdoptionTransfer labels a failed or refused owner transfer.
	errReasonAdoptionTransfer = "adoption-owner-transfer"

	// adoptionCapabilitiesUnknownMessage is the adoption message while a
	// single-host provider's capabilities cannot be read: nothing is adopted,
	// because the provider might be able to stamp what it adopts.
	adoptionCapabilitiesUnknownMessage = "Waiting for the provider's capabilities before a discovery " +
		"(whether it can stamp adopted VMs with their new owner); retried"

	// adoptionUnstampedNote ends the adoption message of a discovery through a
	// provider that cannot stamp the VMs it adopts.
	adoptionUnstampedNote = "; adopted VMs are not stamped with their new owner: the provider does not support it " +
		"(supportsOwnerTransfer=false), so a VM keeps the owner stamp it had and a re-adopted VM's delete may leave " +
		"it on the hypervisor; upgrade the provider"
)

// Reasons a single-host discovery does not adopt (or stamp) a listed VM; each
// is reported, by count and a few VM ids, in Provider.status.adoption.message.
const (
	// skipBoundElsewhere: an unstamped VM whose id is the status.id of a
	// VirtualMachine bound through another single-host Provider object of the
	// same type and endpoint. Stamping it would take it from that
	// VirtualMachine.
	skipBoundElsewhere adoptionSkipReason = "bound to a VirtualMachine of another Provider object for the same endpoint " +
		"(one Provider object per endpoint adopts)"
	// skipOwnerLive: its stamp names a VirtualMachine the uncached check found.
	skipOwnerLive adoptionSkipReason = "its owner stamp names a VirtualMachine that exists"
	// skipOwnerUnverified: its stamp could not be checked against the API
	// server (no uncached reader, or the list failed); retried.
	skipOwnerUnverified adoptionSkipReason = "its owner stamp could not be verified against the API server (retried)"
	// skipTransferRefused: the provider refused to stamp it (it is owned by
	// another VirtualMachine, or changed or went away since the listing); its
	// adopting VirtualMachine was removed again.
	skipTransferRefused adoptionSkipReason = "the provider refused to hand it over (owned by another VirtualMachine, " +
		"or changed since the listing)"
)

// singleHostCandidate is a listed VM a single-host discovery adopts.
type singleHostCandidate struct {
	info contracts.VMInfo
	// replaceable are the stale owner UIDs the VM is stamped with (opt-in
	// only), to be confirmed gone before the transfer.
	replaceable []string
	// waiting is the adopting VirtualMachine an earlier discovery created and
	// did not finish, or nil.
	waiting *infravirtrigaudiov1beta1.VirtualMachine
}

// singleHostAdoptionPlan is what one stamped single-host discovery found to do.
type singleHostAdoptionPlan struct {
	adopt []singleHostCandidate
	// complete are listed VMs already stamped with a waiting VirtualMachine's
	// UID: only its binding is written.
	complete []pendingAdoption
	// release are waiting VirtualMachines whose VM the listing shows can no
	// longer be adopted by them: they are removed (no provider call).
	release []pendingAdoption
	// foreignStamped counts VMs stamped by no VirtualMachine here, without the
	// adopt-stamped-vms opt-in.
	foreignStamped int
	skipped        []adoptionSkip
}

// singleHostTransferrer returns the provider's TransferOwner when it reports
// SupportsOwnerTransfer. ok is false for a provider that cannot stamp (no
// capability, or not a CapabilityReporter / OwnerTransferrer): adoption then
// runs unstamped. A capability query that fails is returned as err: adoption
// waits (it might have been able to stamp).
func singleHostTransferrer(ctx context.Context, providerInstance contracts.Provider) (contracts.OwnerTransferrer, bool, error) {
	reporter, okCaps := providerInstance.(contracts.CapabilityReporter)
	transferrer, okTransfer := providerInstance.(contracts.OwnerTransferrer)
	if !okCaps || !okTransfer {
		return nil, false, nil
	}
	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		return nil, false, err
	}
	return transferrer, caps.SupportsOwnerTransfer, nil
}

// adoptionIDOf is the VM id a single-host adopting VirtualMachine waits for:
// its AdoptedIDAnnotation, or — for one an older manager created and could not
// bind — its spec.importedDisk.diskID with the adoption source "manual". ""
// for any other VirtualMachine. A clustered adoption (AdoptedHostAnnotation)
// and a VMClone target (it carries no adoption id or adopted disk) never
// match.
func adoptionIDOf(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if vm.Annotations[AdoptedHostAnnotation] != "" {
		return ""
	}
	if id := vm.Annotations[AdoptedIDAnnotation]; id != "" {
		return id
	}
	if d := vm.Spec.ImportedDisk; d != nil && d.Source == adoptedDiskSource {
		return d.DiskID
	}
	return ""
}

// awaitingSingleHostAdoptionOf reports whether vm is an adopting VirtualMachine
// of provider that is not bound yet and waits for VM id: labelled adopted, in
// the Provider's namespace, referencing it, no status.id, no create requested
// and no pending host (it never went through Create), not being deleted,
// adoptionIDOf id. A VMClone's target (isCloneTarget) never is: it waits for
// its clone's bind, not for adoption.
func awaitingSingleHostAdoptionOf(vm *infravirtrigaudiov1beta1.VirtualMachine, provider *infravirtrigaudiov1beta1.Provider, id string) bool {
	return vmIsAdopted(vm) && !isCloneTarget(vm) && vm.Status.ID == "" && vm.DeletionTimestamp.IsZero() &&
		!createRequested(vm) && pendingHost(vm) == "" &&
		vm.Namespace == provider.Namespace && vmProviderKey(vm) == client.ObjectKeyFromObject(provider) &&
		id != "" && adoptionIDOf(vm) == id
}

// waitsToAdopt reports whether vm is the adopting VirtualMachine of provider
// for the listed VM info: awaitingSingleHostAdoptionOf info.ID AND named as
// adoption names it (contracts.AdoptedVMName). A VirtualMachine anyone could
// create in the Provider's namespace under another name, carrying the label
// and annotation, is never handed a VM nor bound to one (it would bypass the
// adoption filter and the adopted name).
func waitsToAdopt(vm *infravirtrigaudiov1beta1.VirtualMachine, provider *infravirtrigaudiov1beta1.Provider, info contracts.VMInfo) bool {
	return awaitingSingleHostAdoptionOf(vm, provider, info.ID) && vm.Name == contracts.AdoptedVMName(info.Name, info.ID)
}

// planSingleHostAdoption decides what one stamped single-host discovery does
// with each listed VM (see the file comment, point 1). vms are every
// VirtualMachine (cached), providers every Provider.
func planSingleHostAdoption(provider *infravirtrigaudiov1beta1.Provider, listed []contracts.VMInfo,
	vms []infravirtrigaudiov1beta1.VirtualMachine, providers []infravirtrigaudiov1beta1.Provider) singleHostAdoptionPlan {
	adoptStamped := provider.Annotations[AdoptStampedVMsAnnotation] == "true"
	waiting := map[string]*infravirtrigaudiov1beta1.VirtualMachine{}
	var others []infravirtrigaudiov1beta1.VirtualMachine
	for i := range vms {
		vm := &vms[i]
		if id := adoptionIDOf(vm); id != "" && awaitingSingleHostAdoptionOf(vm, provider, id) {
			waiting[id] = vm
			continue
		}
		others = append(others, *vm)
	}
	// The VMs single-host adoption leaves alone (managed, live-owned,
	// unreliable, artifacts, foreign-stamped without the opt-in), computed
	// without the waiting adopters: they never mark their own VM managed.
	unmanaged, foreign := unmanagedProviderVMs(provider, listed, others)
	adoptable := make(map[string]bool, len(unmanaged))
	for _, info := range unmanaged {
		adoptable[info.ID] = true
	}
	bound := boundThroughSiblingProviders(provider, vms, providers)
	libvirt := provider.Spec.Type == infravirtrigaudiov1beta1.ProviderTypeLibvirt

	isForeign := make(map[string]bool, len(foreign))
	for _, info := range foreign {
		isForeign[info.ID] = true
	}

	var plan singleHostAdoptionPlan
	for _, info := range listed {
		w := waiting[info.ID]
		if w != nil && !waitsToAdopt(w, provider, info) {
			w = nil // waits for this id under another name: not this adoption's
		}
		uids := ownerUIDs(info)
		switch {
		case w != nil && len(uids) == 1 && uids[0] == string(w.UID) && !ownerStampUnreliable(info) && !isArtifact(info):
			plan.complete = append(plan.complete, pendingAdoption{info: info, vm: w})
		case !adoptable[info.ID]:
			if isForeign[info.ID] {
				plan.foreignStamped++
			}
			if releasable(w) {
				plan.release = append(plan.release, pendingAdoption{info: info, vm: w})
			}
		case bound[info.ID]:
			plan.skipped = append(plan.skipped, adoptionSkip{info: info, reason: skipBoundElsewhere})
			if releasable(w) {
				plan.release = append(plan.release, pendingAdoption{info: info, vm: w})
			}
		case libvirt && strings.TrimSpace(info.ProviderRaw[contracts.VMInfoUUIDKey]) == "":
			// The libvirt provider refuses a transfer without the domain UUID.
			plan.skipped = append(plan.skipped, adoptionSkip{info: info, reason: skipNoIdentity})
			if releasable(w) {
				plan.release = append(plan.release, pendingAdoption{info: info, vm: w})
			}
		case libvirt && ambiguousVMID(info.ID):
			// The provider refuses to address such a domain by name.
			plan.skipped = append(plan.skipped, adoptionSkip{info: info, reason: skipAmbiguousName})
			if releasable(w) {
				plan.release = append(plan.release, pendingAdoption{info: info, vm: w})
			}
		default:
			if len(uids) > 0 && !adoptStamped {
				continue // unmanagedProviderVMs never returns it; defensive
			}
			plan.adopt = append(plan.adopt, singleHostCandidate{info: info, replaceable: uids, waiting: w})
		}
	}
	return plan
}

// boundThroughSiblingProviders returns the status.id of every VirtualMachine
// bound (status.boundProvider, else spec.providerRef) through a single-host
// Provider object OTHER than provider whose type and endpoint (endpointKey)
// are provider's: a VM those ids name is managed there, and may be unstamped
// (created before owner stamps, or adopted before this release). Residual: the
// same endpoint reached under two names is not recognized.
func boundThroughSiblingProviders(provider *infravirtrigaudiov1beta1.Provider, vms []infravirtrigaudiov1beta1.VirtualMachine,
	providers []infravirtrigaudiov1beta1.Provider) map[string]bool {
	self := client.ObjectKeyFromObject(provider)
	key := endpointKey(provider.Spec.Endpoint)
	siblings := map[types.NamespacedName]bool{}
	for i := range providers {
		p := &providers[i]
		pk := client.ObjectKeyFromObject(p)
		if pk == self || isClusterTopology(p) || p.Spec.Type != provider.Spec.Type || key == "" || endpointKey(p.Spec.Endpoint) != key {
			continue
		}
		siblings[pk] = true
	}
	out := map[string]bool{}
	if len(siblings) == 0 {
		return out
	}
	for i := range vms {
		vm := &vms[i]
		if vm.Status.ID != "" && siblings[placementProviderKey(vm)] {
			out[vm.Status.ID] = true
		}
	}
	return out
}

// reconcileStampedSingleHostAdoption is a single-host discovery through a
// provider that reports SupportsOwnerTransfer (see the file comment).
func (r *VMAdoptionReconciler) reconcileStampedSingleHostAdoption(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	filter *VMAdoptionFilter, providerInstance contracts.Provider, transferrer contracts.OwnerTransferrer) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	discoveryFailed := func(msg string) (ctrl.Result, error) {
		if err := r.writeAdoptionMessage(ctx, provider, msg); err != nil {
			logger.Error(err, "Failed to update adoption status")
		}
		metrics.RecordError(errReasonDiscoverVMs, metrics.ComponentManager)
		return ctrl.Result{RequeueAfter: singleHostDiscoveryInterval}, nil
	}

	// ListVMs BEFORE the VirtualMachines are read from the cache, as the
	// clustered discovery: a VM a VirtualMachine created (and stamped) is
	// listed only after that VirtualMachine was in the shared cache.
	listed, err := providerInstance.ListVMs(ctx)
	if err != nil {
		return discoveryFailed(fmt.Sprintf("Discovery failed: %v", err))
	}
	vmList := &infravirtrigaudiov1beta1.VirtualMachineList{}
	if err := r.List(ctx, vmList); err != nil {
		return discoveryFailed(fmt.Sprintf("Discovery failed: list VirtualMachines: %v", err))
	}
	providers := &infravirtrigaudiov1beta1.ProviderList{}
	if err := r.List(ctx, providers); err != nil {
		return discoveryFailed(fmt.Sprintf("Discovery failed: list Providers: %v", err))
	}
	plan := planSingleHostAdoption(provider, listed.VMs, vmList.Items, providers.Items)

	var candidates []singleHostCandidate
	for _, c := range plan.adopt {
		if filter != nil && !r.matchesFilter(c.info, filter) {
			logger.V(1).Info("VM filtered out by adoption filter", "vm_id", c.info.ID, "vm_name", c.info.Name)
			// One this flow created before the filter changed waits for a
			// VM it may no longer take: withdrawn.
			if releasable(c.waiting) {
				plan.release = append(plan.release, pendingAdoption{info: c.info, vm: c.waiting})
			}
			continue
		}
		candidates = append(candidates, c)
	}
	candidates, plan.skipped = r.confirmStaleOwners(ctx, candidates, plan.skipped)
	logger.Info("Single-host discovery (stamping adopted VMs)", "listed", len(listed.VMs), "adopt", len(candidates),
		"pendingBindings", len(plan.complete), "withdrawn", len(plan.release), "foreignStamped", plan.foreignStamped)

	var adopted, failed, deferred int32
	count := func(err error, info contracts.VMInfo) {
		switch {
		case err == nil:
			adopted++
		case errors.Is(err, errTransferRefused):
			plan.skipped = append(plan.skipped, adoptionSkip{info: info, reason: skipTransferRefused})
			metrics.RecordError(errReasonAdoptionTransfer, metrics.ComponentManager)
			logger.Info("The provider refused to hand a listed VM over; not adopted", "vm_id", info.ID, "reason", err.Error())
			if errors.Is(err, errAdoptionStranded) {
				failed++
			}
		case errors.Is(err, errAdoptionSkipped):
			logger.Info("Not adopting a listed VM", "vm_id", info.ID, "reason", err.Error())
		case errors.Is(err, errAdoptionDeferred):
			logger.Info("Adoption deferred to the next discovery", "vm_id", info.ID, "reason", err.Error())
			deferred++
		default:
			logger.Error(err, "Failed to adopt VM", "vm_id", info.ID)
			failed++
		}
	}
	for _, p := range plan.complete {
		count(r.writeSingleHostBinding(ctx, provider, p.vm, p.info), p.info)
	}
	for _, c := range candidates {
		count(r.adoptAndStampVM(ctx, provider, transferrer, c), c.info)
	}
	withdrawn := 0
	for _, p := range plan.release {
		if r.removeWaitingVM(ctx, p.vm, func(fresh *infravirtrigaudiov1beta1.VirtualMachine) bool {
			return waitsToAdopt(fresh, provider, p.info)
		}) {
			withdrawn++
			continue
		}
		failed++
	}

	now := metav1.Now()
	st := provider.Status.Adoption.DeepCopy()
	st.LastDiscoveryTime = &now
	// #nosec G115 -- a VM count never approaches 2^31.
	st.DiscoveredVMs = int32(len(candidates) + len(plan.complete))
	st.AdoptedVMs = adopted
	st.FailedAdoptions = failed
	st.Message = singleHostAdoptionMessage(adopted, failed, deferred, withdrawn, plan.skipped) + foreignStampedSummary(plan.foreignStamped)
	if err := r.writeAdoptionResult(ctx, provider, st); err != nil {
		logger.Error(err, "Failed to update adoption status")
		metrics.RecordError(errReasonAdoptionStatus, metrics.ComponentManager)
		return ctrl.Result{}, err
	}
	if failed > 0 || deferred > 0 || countSkipped(plan.skipped, skipOwnerUnverified) > 0 {
		return ctrl.Result{RequeueAfter: singleHostDiscoveryInterval}, nil
	}
	return ctrl.Result{RequeueAfter: clusteredAdoptionInterval}, nil
}

// singleHostAdoptionMessage summarizes one stamped single-host discovery.
func singleHostAdoptionMessage(adopted, failed, deferred int32, withdrawn int, skipped []adoptionSkip) string {
	msg := fmt.Sprintf("Successfully adopted %d VMs", adopted)
	if failed > 0 {
		msg = fmt.Sprintf("Adopted %d VMs, %d failed", adopted, failed)
	}
	if deferred > 0 {
		msg += fmt.Sprintf(", %d deferred to the next discovery", deferred)
	}
	if withdrawn > 0 {
		msg += fmt.Sprintf(", %d unfinished adoption(s) withdrawn (the VM can no longer be adopted)", withdrawn)
	}
	return msg + skippedSummary(skipped)
}

// confirmStaleOwners checks, with ONE uncached list of every VirtualMachine,
// that no stale UID a candidate is stamped with belongs to a VirtualMachine
// (the cache may lag; a UID is never reused, so a UID absent now never comes
// back). A candidate whose stamp names an existing VirtualMachine is skipped
// (skipOwnerLive); when the check cannot be made (no uncached reader, or the
// list failed), every stamped candidate is skipped (skipOwnerUnverified) and
// retried — fail closed. Unstamped candidates are returned as they are.
func (r *VMAdoptionReconciler) confirmStaleOwners(ctx context.Context, candidates []singleHostCandidate,
	skipped []adoptionSkip) ([]singleHostCandidate, []adoptionSkip) {
	var live map[string]bool
	var listErr error
	checked := false
	out := candidates[:0:0]
	for _, c := range candidates {
		if len(c.replaceable) == 0 {
			out = append(out, c)
			continue
		}
		if !checked {
			checked = true
			live, listErr = r.uncachedVMUIDs(ctx)
			if listErr != nil {
				log.FromContext(ctx).Error(listErr, "Cannot verify stale owner stamps; not adopting stamped VMs this discovery")
			}
		}
		switch {
		case listErr != nil:
			skipped = append(skipped, adoptionSkip{info: c.info, reason: skipOwnerUnverified})
		case anyLive(c.replaceable, live):
			skipped = append(skipped, adoptionSkip{info: c.info, reason: skipOwnerLive})
		default:
			out = append(out, c)
		}
	}
	return out, skipped
}

// anyLive reports whether one of uids is in live.
func anyLive(uids []string, live map[string]bool) bool {
	for _, uid := range uids {
		if live[uid] {
			return true
		}
	}
	return false
}

// uncachedVMUIDs lists every VirtualMachine through the uncached API reader
// and returns their UIDs (a terminating one counts). Without an uncached
// reader it fails: the cache is not proof that a UID is gone.
func (r *VMAdoptionReconciler) uncachedVMUIDs(ctx context.Context) (map[string]bool, error) {
	if r.APIReader == nil {
		return nil, fmt.Errorf("no uncached reader to confirm that a stamped owner is gone: %w", errOwnerStillExists)
	}
	list := &infravirtrigaudiov1beta1.VirtualMachineList{}
	if err := r.APIReader.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list VirtualMachines (uncached): %w", err)
	}
	out := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		out[string(list.Items[i].UID)] = true
	}
	return out, nil
}

// adoptAndStampVM adopts one listed VM: it creates (or re-finds) the waiting
// VirtualMachine, re-reads it uncached, hands it the VM (TransferOwner,
// compare-and-swap; only c.replaceable stamps may be replaced) and writes its
// binding. A transfer the provider refused for good removes the waiting
// VirtualMachine again (errTransferRefused; errAdoptionStranded when it could
// not be removed). Any other failure keeps it: the next discovery finishes.
func (r *VMAdoptionReconciler) adoptAndStampVM(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	transferrer contracts.OwnerTransferrer, c singleHostCandidate) error {
	info := c.info
	logger := log.FromContext(ctx).WithValues("vm_id", info.ID, "vm_name", info.Name)
	vm := c.waiting
	if vm == nil {
		name := contracts.AdoptedVMName(info.Name, info.ID)
		existing := &infravirtrigaudiov1beta1.VirtualMachine{}
		err := r.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: name}, existing)
		switch {
		case apierrors.IsNotFound(err):
			created, cerr := r.createWaitingAdoptedVM(ctx, provider, info, name)
			if cerr != nil {
				return cerr
			}
			vm = created
		case err != nil:
			return fmt.Errorf("get VirtualMachine %s/%s: %w", provider.Namespace, name, err)
		case !waitsToAdopt(existing, provider, info):
			return fmt.Errorf("VirtualMachine %s/%s exists and is not waiting for this adoption: %w",
				provider.Namespace, name, errAdoptionSkipped)
		default:
			vm = existing
		}
	}
	// Never hand a VM to — nor bind — a VirtualMachine whose VMClass or VMImage
	// is in another namespace that does not select the Provider's (a waiting
	// one may have been edited; a new one references only this namespace).
	if err := checkVMConsumerRefs(ctx, r.Client, vm); err != nil {
		if isConsumerNotAllowed(err) {
			return fmt.Errorf("VirtualMachine %s/%s references an ungranted cross-namespace object (%s): %w",
				vm.Namespace, vm.Name, err.Error(), errAdoptionSkipped)
		}
		return fmt.Errorf("check consumer grants of adopted VirtualMachine %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	// Live re-read before the non-idempotent call: a VirtualMachine deleted or
	// bound meanwhile is never stamped onto the VM.
	fresh := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := r.freshReader().Get(ctx, client.ObjectKeyFromObject(vm), fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("VirtualMachine %s/%s is gone: %w", vm.Namespace, vm.Name, errAdoptionDeferred)
		}
		return fmt.Errorf("re-read VirtualMachine %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	if fresh.UID != vm.UID || !waitsToAdopt(fresh, provider, info) {
		return fmt.Errorf("VirtualMachine %s/%s is no longer waiting for this adoption: %w", vm.Namespace, vm.Name, errAdoptionDeferred)
	}
	vm = fresh

	if err := transferrer.TransferOwner(ctx, contracts.TransferOwnerRequest{
		VM:                   contracts.VMRef{ID: info.ID, Owner: vmOwnerIdentity(vm)},
		ReplaceableOwnerUIDs: c.replaceable,
		ExpectedUUID:         strings.TrimSpace(info.ProviderRaw[contracts.VMInfoUUIDKey]),
	}); err != nil {
		if !refusedForGood(err) {
			metrics.RecordError(errReasonAdoptionTransfer, metrics.ComponentManager)
			return fmt.Errorf("transfer the owner of VM %s: %w", info.ID, err)
		}
		// Only an adopter this flow created is removed; one an older manager
		// created is left as it is (it keeps waiting, with no provider call).
		removed := !releasable(vm) || r.removeWaitingVM(ctx, vm, func(f *infravirtrigaudiov1beta1.VirtualMachine) bool {
			return waitsToAdopt(f, provider, info)
		})
		if !removed {
			return fmt.Errorf("transfer the owner of VM %s: %w (%s/%s): %w: %w", info.ID, errAdoptionStranded,
				vm.Namespace, vm.Name, errTransferRefused, err)
		}
		return fmt.Errorf("transfer the owner of VM %s: %w: %w", info.ID, errTransferRefused, err)
	}
	logger.Info("Handed the VM to its adopting VirtualMachine", "vm", vm.Namespace+"/"+vm.Name,
		"replacedStaleOwners", len(c.replaceable))
	return r.writeSingleHostBinding(ctx, provider, vm, info)
}

// createWaitingAdoptedVM creates the VirtualMachine (and its VMClass) that
// adopts info: the single-host adoption spec, annotated with the VM's id
// (AdoptedIDAnnotation), with NO status — status.id is written only once the
// VM carries the VirtualMachine's stamp.
func (r *VMAdoptionReconciler) createWaitingAdoptedVM(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	info contracts.VMInfo, name string) (*infravirtrigaudiov1beta1.VirtualMachine, error) {
	vmClass, err := r.ensureVMClass(ctx, info, provider.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure VMClass: %w", err)
	}
	vm := adoptedVMObject(provider, info, name, vmClass)
	vm.Annotations = map[string]string{AdoptedIDAnnotation: info.ID}
	vm.Status = infravirtrigaudiov1beta1.VirtualMachineStatus{}
	if err := r.Create(ctx, vm); err != nil {
		return nil, fmt.Errorf("create VirtualMachine %s/%s: %w", provider.Namespace, name, err)
	}
	log.FromContext(ctx).Info("Created the VirtualMachine that adopts a VM; handing the VM to it next",
		"vm_name", name, "vm_id", info.ID)
	return vm, nil
}

// writeSingleHostBinding writes the binding of vm — adopted for info from
// provider, its VM already stamped with vm's identity — in one status write:
// status.id, boundProvider, the observed power state, IPs and the listing's
// provider data (without the owner-stamp keys). It re-reads vm (uncached when
// configured) and retries on conflict; it writes only while the fresh object
// is the same VirtualMachine and still waiting for this VM. One already bound
// to it is a success; anything else is errAdoptionDeferred (the next
// discovery completes the binding from the stamp).
func (r *VMAdoptionReconciler) writeSingleHostBinding(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine, info contracts.VMInfo) error {
	key := client.ObjectKeyFromObject(vm)
	wantUID := vm.UID
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &infravirtrigaudiov1beta1.VirtualMachine{}
		if err := r.freshReader().Get(ctx, key, latest); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("VirtualMachine %s is gone: %w", key, errAdoptionDeferred)
			}
			return err
		}
		switch {
		case latest.UID != wantUID:
			return fmt.Errorf("VirtualMachine %s was replaced: %w", key, errAdoptionDeferred)
		case latest.Status.ID == info.ID && latest.DeletionTimestamp.IsZero():
			return nil // already bound
		case !waitsToAdopt(latest, provider, info):
			return fmt.Errorf("VirtualMachine %s is no longer waiting for this adoption: %w", key, errAdoptionDeferred)
		}
		// Never bind a VirtualMachine whose VMClass or VMImage is in another
		// namespace that does not select the Provider's — checked on the
		// object written, which may have been edited since the transfer.
		if err := checkVMConsumerRefs(ctx, r.Client, latest); err != nil {
			if isConsumerNotAllowed(err) {
				return fmt.Errorf("VirtualMachine %s references an ungranted cross-namespace object (%s): %w",
					key, err.Error(), errAdoptionSkipped)
			}
			return fmt.Errorf("check consumer grants of adopted VirtualMachine %s: %w", key, err)
		}
		latest.Status.ID = info.ID
		recordBoundProvider(latest, provider)
		latest.Status.PowerState = observedPowerState(info.PowerState)
		latest.Status.IPs = contracts.RoutableIPs(info.IPs)
		latest.Status.Provider = adoptedProviderRaw(info.ProviderRaw)
		if err := r.Status().Update(ctx, latest); err != nil {
			return err
		}
		*vm = *latest
		return nil
	})
	switch {
	case err == nil:
		log.FromContext(ctx).Info("Successfully adopted VM", "vm", key.String(), "vm_id", info.ID)
		return nil
	case apierrors.IsConflict(err):
		return fmt.Errorf("write the binding of adopted VirtualMachine %s: kept conflicting: %w", key, errAdoptionDeferred)
	case errors.Is(err, errAdoptionDeferred):
		return err
	}
	return fmt.Errorf("write the binding of adopted VirtualMachine %s: %w", key, err)
}

// removeWaitingVM deletes vm — an adopting VirtualMachine that will never be
// bound — after re-reading it: only while the fresh object is the same
// VirtualMachine (UID) and stillWaiting says it waits, and preconditioned on
// that UID and resourceVersion. Such a VM has no status.id (and no create
// requested), so its deletion makes no provider call. It reports whether the
// VM is out of the way (deleted, gone, replaced, bound or being deleted);
// false means it could not be removed and is left for an administrator.
func (r *VMAdoptionReconciler) removeWaitingVM(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	stillWaiting func(*infravirtrigaudiov1beta1.VirtualMachine) bool) bool {
	logger := log.FromContext(ctx).WithValues("vm", vm.Namespace+"/"+vm.Name)
	fresh := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := r.freshReader().Get(ctx, client.ObjectKeyFromObject(vm), fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		logger.Error(err, "Failed to re-read an adopting VirtualMachine that will not be bound")
		return false
	}
	if fresh.UID != vm.UID || !stillWaiting(fresh) {
		return true // replaced, bound or being deleted: not stranded
	}
	uid, rv := fresh.UID, fresh.ResourceVersion
	if err := r.Delete(ctx, fresh, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to remove an adopting VirtualMachine that will not be bound")
		return false
	}
	logger.Info("Removed an adopting VirtualMachine that will not be bound (no provider call)")
	return true
}

// adoptedDiskSource is spec.importedDisk.source of an adopted VirtualMachine
// (adoptedVMObject): the disk is the existing VM's, not an import.
const adoptedDiskSource = "manual"

// releasable reports whether a waiting adopter may be removed when its VM can
// no longer be adopted by it: only one this flow created (AdoptedIDAnnotation).
// One an older manager created is left as it is.
func releasable(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return vm != nil && vm.Annotations[AdoptedIDAnnotation] != ""
}
