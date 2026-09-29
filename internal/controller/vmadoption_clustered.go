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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Adoption on a CLUSTERED provider (ADR-0007 Addendum A, A3 / slice 4).
//
// A clustered provider lists every host it fronts (VMInfo.HostID) and names
// the hosts it could not list (VMList.UnreachableHostIDs). Adoption follows
// the single-host rules (the VirtualMachine and its VMClass are created in the
// Provider's own namespace, so no consumer grant is needed; an existing
// adopted-labelled VirtualMachine is bound only when it references this
// Provider and every cross-namespace reference it has is granted; a domain
// stamped with the UID of a VirtualMachine that still exists is never
// adopted), with one deliberate restriction and four differences.
//
// The restriction (ADR-0007 A6, until A6.4): only UNSTAMPED domains are
// adopted. A domain carrying any VirtRigaud owner stamp whose VirtualMachine no
// longer exists is the previous incarnation of a VirtualMachine that was
// deleted with orphan-on-delete or restored from a backup (a new UID). Adopting
// it would pre-empt A6's hold and runbook and break its one-domain-per-name
// rule, so it is skipped and named in Provider.status.adoption.message with a
// hint: re-attach it per the A6 runbook, or remove it. Single-host adoption
// still adopts such domains; it is unchanged.
//
// The differences:
//
//  1. A listed VM is identified by (host id, VM id), never by its id or name
//     alone: two hosts may each have a domain named "web". A VM is managed
//     when a VirtualMachine of this Provider is bound to that pair
//     (status.placement.host, status.id). The adopted VirtualMachine's name is
//     derived from the pair (clusteredAdoptedVMName) and recorded in its
//     annotations, so the same name on two hosts gives two VirtualMachines.
//  2. The domain's owner is transferred to the adopted VirtualMachine
//     (TransferOwner: a serialized check-and-set on the domain UUID and its
//     stamp, with read-back) BEFORE the binding is written: every routed
//     per-VM call is owner checked, so a VirtualMachine bound to a domain it
//     does not own could never describe or manage it. Adoption transfers only
//     unstamped domains, so it never lists a stamp as replaceable.
//  3. The binding (status.id, status.boundProvider, status.placement.host and
//     .pool) is written only after a routed, owner-checked Describe of the
//     adopted VirtualMachine confirms the domain, and it records the domain's
//     size from provider truth — status.currentResources (never below the
//     vCPUs Describe reports online) and status.placement.memoryCeilingMiB
//     (Describe's memory maximum) — so the committed-capacity accounting
//     counts the VM from the moment it is bound.
//  4. A host listed as unreachable is UNKNOWN, not empty. Nothing about the
//     VMs on it is concluded: adoption only ever adds, and it acts only on
//     VMs it positively listed. The unreachable hosts are named in
//     Provider.status.adoption.message and discovery is retried sooner. The
//     Host itself reports its reachability on its Ready condition, from the
//     Host controller's own inventory probe.
//
// A domain whose owner was transferred but whose binding write was lost is
// found again by its stamp — it names a VirtualMachine that is still waiting
// for its binding — and the binding is completed (completeClusteredAdoption),
// never adopted twice.

const (
	// AdoptedHostAnnotation records, on a VirtualMachine adopted from a
	// clustered provider, the host (Host name) its domain was listed on.
	AdoptedHostAnnotation = "virtrigaud.io/adopted-host"
	// AdoptedIDAnnotation records, on a VirtualMachine adopted from a
	// clustered provider, the provider id of its domain on AdoptedHostAnnotation.
	AdoptedIDAnnotation = "virtrigaud.io/adopted-id"

	// adoptedVMNameHashLen is the number of hex digits of the (host, id) digest
	// a clustered adopted VirtualMachine's name ends with.
	adoptedVMNameHashLen = 10
	// adoptedVMNameMaxLen is the maximum length of an adopted VirtualMachine's
	// name (a DNS label, as single-host adoption).
	adoptedVMNameMaxLen = 63

	// clusteredAdoptionRetryInterval is how soon a clustered discovery is
	// repeated when a host could not be listed or an adoption failed.
	clusteredAdoptionRetryInterval = 5 * time.Minute
	// clusteredAdoptionInterval is the steady-state rediscovery interval (the
	// same as single-host).
	clusteredAdoptionInterval = time.Hour

	// errReasonAdoptionCapabilities labels a failed capability query.
	errReasonAdoptionCapabilities = "adoption-capabilities"

	// unreachableHostsListed caps how many unreachable hosts the adoption
	// status message names (the count is always given in full).
	unreachableHostsListed = 10
)

// clusteredAdoptionNotSupportedMessage is recorded on Provider.status.adoption
// for a clustered provider that does not report supports_routed_adoption (it
// predates ADR-0007 Addendum A slice 4, or does not report capabilities).
const clusteredAdoptionNotSupportedMessage = "Adoption from this clustered (topology: cluster) provider is not possible: " +
	"it does not list VMs across its hosts or transfer a domain's owner (supportsRoutedAdoption=false; " +
	"a clustered provider older than ADR-0007 Addendum A slice 4). Upgrade the provider"

// errAdoptionSkipped marks a listed VM that is deliberately not adopted this
// time (neither adopted nor a failure): a VirtualMachine of the adopted name
// that is not waiting for it, or one whose cross-namespace references are not
// granted.
var errAdoptionSkipped = errors.New("adoption skipped")

// errAdoptionDeferred marks an adoption whose binding write could not be made
// this time although nothing failed: the adopting VirtualMachine kept changing
// (conflicts) or is no longer waiting for this binding (deleted, being
// deleted, or replaced). The next discovery completes it from the stamp, or
// reports the domain. It is counted apart from failures.
var errAdoptionDeferred = errors.New("adoption deferred")

// errTransferRefused marks an adoption whose TransferOwner the provider
// refused for good — the domain is owned by someone else or its stamp is
// unusable (Conflict), it is gone or was replaced (NotFound), or the request
// was refused (InvalidSpec) — so the domain was NOT handed over. Only this
// makes the waiting VirtualMachine stranded; a failure after a successful
// transfer (the domain already carries the VM's stamp) never does, and the
// binding is retried.
var errTransferRefused = errors.New("the provider refused the owner transfer")

// errAdoptionStranded marks a refused adoption whose waiting VirtualMachine
// could not be removed; the adoption message names it for an administrator.
var errAdoptionStranded = errors.New("adopted VirtualMachine stranded")

// clusteredAdoptionPlan is what one clustered discovery found to do.
type clusteredAdoptionPlan struct {
	// adopt are listed VMs no VirtualMachine manages.
	adopt []contracts.VMInfo
	// complete are listed VMs whose owner was already transferred to an
	// adopted VirtualMachine that is still waiting for its binding.
	complete []pendingAdoption
	// skipped are listed VMs that are deliberately not adopted, with why.
	skipped []adoptionSkip
}

// adoptionSkip is a listed VM a clustered discovery does not adopt.
type adoptionSkip struct {
	info   contracts.VMInfo
	reason adoptionSkipReason
}

// adoptionSkipReason says why a listed VM is not adopted. Each is reported,
// by count and a few names, in Provider.status.adoption.message.
type adoptionSkipReason string

const (
	// skipNoIdentity: listed without a host id or a UUID; it cannot be bound
	// or transferred safely.
	skipNoIdentity adoptionSkipReason = "listed without a host id or UUID"
	// skipPreviousIncarnation: stamped only by VirtualMachines that no longer
	// exist — deleted with orphan-on-delete, or restored with a new UID. Until
	// ADR-0007 A6.4 it is re-attached by an administrator, never adopted.
	skipPreviousIncarnation adoptionSkipReason = "previous incarnation of a VirtualMachine that no longer exists " +
		"(re-attach it per the ADR-0007 A6 runbook, or remove it)"
	// skipDuplicate: the same domain (its UUID) is defined on more than one
	// host, or one of its disks is used by a VM listed on another host — a
	// brownfield duplicate, common with a shared (NFS) pool. Adopting both
	// would let deleting the stale definition remove the running VM's disk.
	skipDuplicate adoptionSkipReason = "defined on more than one host (the same UUID, or a disk a VM on another host uses; " +
		"remove the stale definition)"
	// skipSharedEndpoint: listed on a Host whose endpoint another Host object
	// (of any Provider, in any namespace) also names. Two Providers adopting
	// from one hypervisor could each take the same unstamped domain.
	skipSharedEndpoint adoptionSkipReason = "on a Host whose endpoint another Host object also names " +
		"(one clustered Provider per host endpoint)"
	// skipStampUnreliable: its owner stamp cannot be read, records no UID, or
	// names more than one owner across its definitions; a transfer would be
	// refused, so nothing is created for it.
	skipStampUnreliable adoptionSkipReason = "owner stamp unreadable or naming more than one owner (inspect the domain's metadata)"
	// skipSingleHostManaged: an unstamped domain whose name is the status.id
	// of a VirtualMachine bound through a single-host (not clustered)
	// Provider. A hypervisor fronted by both a single-host and a clustered
	// Provider: the single-host provider does not stamp, so the name is the
	// only sign the domain is already managed.
	skipSingleHostManaged adoptionSkipReason = "managed through a single-host Provider fronting the same hypervisor " +
		"(do not front one host with both a single-host and a clustered Provider)"
	// skipAmbiguousName: a domain name the hypervisor tool would read as a
	// domain id or UUID before a name (virsh), so it cannot be addressed by
	// name safely; the provider would refuse its transfer.
	skipAmbiguousName adoptionSkipReason = "named like a domain id or UUID (rename the domain to adopt it)"
	// skipHostsUnknown: not adopted in a discovery that could not list every
	// host. A copy of the domain (the same UUID, or its shared disk) on an
	// unlisted host could not be ruled out, and adopting the stale copy would
	// let its delete remove the running VM's disk. Retried with the next
	// discovery; bindings already in progress are still completed.
	skipHostsUnknown adoptionSkipReason = "not adopted while a host is unknown (a copy of it there could not be ruled out)"
)

// adoptionGuards are facts from outside the listing that make a clustered
// discovery skip listed VMs.
type adoptionGuards struct {
	// sharedEndpointHosts are the Provider's Hosts whose endpoint another Host
	// object names (hostsWithSharedEndpoints).
	sharedEndpointHosts map[string]bool
	// singleHostIDs are the status.id values of VirtualMachines bound through
	// a Provider that is not clustered (singleHostBoundIDs).
	singleHostIDs map[string]bool
}

// pendingAdoption is an adopted VirtualMachine whose domain already carries
// its stamp but whose binding is not written yet.
type pendingAdoption struct {
	info contracts.VMInfo
	vm   *infravirtrigaudiov1beta1.VirtualMachine
}

// vmHostKey is the (host id, VM id) identity of a VM on a clustered provider.
type vmHostKey struct{ host, id string }

// reconcileClusteredAdoption is Reconcile's adoption pass for a clustered
// Provider (see the file comment).
func (r *VMAdoptionReconciler) reconcileClusteredAdoption(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	filter *VMAdoptionFilter) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if last := provider.Status.Adoption.LastDiscoveryTime; last != nil && time.Since(last.Time) < clusteredAdoptionRetryInterval {
		logger.Info("Skipping discovery, last discovery was recent", "timeSince", time.Since(last.Time))
		return ctrl.Result{RequeueAfter: clusteredAdoptionRetryInterval}, nil
	}

	providerInstance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		return r.clusteredAdoptionFailed(ctx, provider, fmt.Sprintf("Discovery failed: %v", err), errReasonDiscoverVMs)
	}
	transferrer, ok := r.routedAdopter(ctx, provider, providerInstance)
	if !ok {
		return ctrl.Result{RequeueAfter: clusteredAdoptionRetryInterval}, nil
	}

	// ListVMs BEFORE the VirtualMachines are read from the cache: a domain a
	// VirtualMachine created (and stamped) is listed only after that
	// VirtualMachine was in the shared cache, so its UID is seen as live.
	listed, err := providerInstance.ListVMs(ctx)
	if err != nil {
		return r.clusteredAdoptionFailed(ctx, provider, fmt.Sprintf("Discovery failed: %v", err), errReasonDiscoverVMs)
	}
	vmList := &infravirtrigaudiov1beta1.VirtualMachineList{}
	if err := r.List(ctx, vmList); err != nil {
		return r.clusteredAdoptionFailed(ctx, provider, fmt.Sprintf("Discovery failed: list VirtualMachines: %v", err), errReasonDiscoverVMs)
	}

	// The guards read every Host and Provider. They fail closed: without them
	// this discovery adopts nothing (a shared endpoint or a single-host
	// binding could not be ruled out).
	hosts := &infravirtrigaudiov1beta1.HostList{}
	if err := r.List(ctx, hosts); err != nil {
		return r.clusteredAdoptionFailed(ctx, provider, fmt.Sprintf("Discovery failed: list Hosts: %v", err), errReasonDiscoverVMs)
	}
	providers := &infravirtrigaudiov1beta1.ProviderList{}
	if err := r.List(ctx, providers); err != nil {
		return r.clusteredAdoptionFailed(ctx, provider, fmt.Sprintf("Discovery failed: list Providers: %v", err), errReasonDiscoverVMs)
	}
	guards := adoptionGuards{
		sharedEndpointHosts: hostsWithSharedEndpoints(ctx, provider, hosts.Items, providers.Items),
		singleHostIDs:       singleHostBoundIDs(vmList.Items, providers.Items),
	}
	plan := planClusteredAdoption(provider, listed, vmList.Items, guards)
	var unmanaged []contracts.VMInfo
	for _, info := range plan.adopt {
		if filter != nil && !r.matchesFilter(info, filter) {
			logger.V(1).Info("VM filtered out by adoption filter", "host", info.HostID, "vm_id", info.ID)
			continue
		}
		unmanaged = append(unmanaged, info)
	}
	// A discovery that could not list every host starts no new adoption: a
	// duplicate of a candidate (the same UUID, or a disk it shares) on an
	// unlisted host is invisible, and adopting the stale copy would let its
	// delete remove the running VM's disk. Bindings already in progress
	// (plan.complete: the owner was transferred before) are still completed.
	if len(listed.UnreachableHostIDs) > 0 {
		for _, info := range unmanaged {
			plan.skipped = append(plan.skipped, adoptionSkip{info: info, reason: skipHostsUnknown})
		}
		unmanaged = nil
	}
	logger.Info("Clustered discovery", "listed", len(listed.VMs), "unmanaged", len(unmanaged),
		"pendingBindings", len(plan.complete), "unreachableHosts", listed.UnreachableHostIDs)

	adopted, failed, deferred := int32(0), int32(0), int32(0)
	var stranded []string
	count := func(err error, msg string, info contracts.VMInfo) {
		if errors.Is(err, errAdoptionStranded) {
			stranded = append(stranded, clusteredAdoptedVMName(info.HostID, info.ID))
		}
		switch {
		case err == nil:
			adopted++
		case errors.Is(err, errAdoptionSkipped):
			logger.Info("Not adopting a listed VM", "host", info.HostID, "vm_id", info.ID, "reason", err.Error())
		case errors.Is(err, errAdoptionDeferred):
			logger.Info("Adoption deferred to the next discovery", "host", info.HostID, "vm_id", info.ID, "reason", err.Error())
			deferred++
		default:
			logger.Error(err, msg, "host", info.HostID, "vm_id", info.ID)
			failed++
		}
	}
	for _, p := range plan.complete {
		count(r.completeAdoption(ctx, provider, transferrer, providerInstance, p.vm, p.info),
			"Failed to complete the adoption of a VM", p.info)
	}
	for _, info := range unmanaged {
		count(r.adoptClusteredVM(ctx, provider, transferrer, providerInstance, info), "Failed to adopt VM", info)
	}

	now := metav1.Now()
	provider.Status.Adoption.LastDiscoveryTime = &now
	// #nosec G115 -- a VM count never approaches 2^31.
	provider.Status.Adoption.DiscoveredVMs = int32(len(unmanaged) + countSkipped(plan.skipped, skipHostsUnknown))
	provider.Status.Adoption.AdoptedVMs = adopted
	provider.Status.Adoption.FailedAdoptions = failed
	provider.Status.Adoption.Message = clusteredAdoptionMessage(adopted, failed, deferred, plan.skipped, listed.UnreachableHostIDs) +
		strandedSummary(stranded)
	if err := r.Status().Update(ctx, provider); err != nil {
		logger.Error(err, "Failed to update adoption status")
		metrics.RecordError(errReasonAdoptionStatus, metrics.ComponentManager)
		return ctrl.Result{}, err
	}
	if failed > 0 || deferred > 0 || len(listed.UnreachableHostIDs) > 0 {
		return ctrl.Result{RequeueAfter: clusteredAdoptionRetryInterval}, nil
	}
	return ctrl.Result{RequeueAfter: clusteredAdoptionInterval}, nil
}

// clusteredAdoptionMessage summarizes one clustered discovery. It names the
// hosts that could not be listed (Host names, never an endpoint): their VMs
// are unknown, not absent. It names, by reason, the listed VMs that were
// deliberately not adopted (skipped): a previous incarnation by the namespace
// and name its stamp records, with the hint to re-attach it per the A6
// runbook or remove it. Every list is capped (unreachableHostsListed).
func clusteredAdoptionMessage(adopted, failed, deferred int32, skipped []adoptionSkip, unreachable []string) string {
	msg := fmt.Sprintf("Successfully adopted %d VMs", adopted)
	if failed > 0 {
		msg = fmt.Sprintf("Adopted %d VMs, %d failed", adopted, failed)
	}
	if deferred > 0 {
		msg += fmt.Sprintf(", %d deferred to the next discovery", deferred)
	}
	msg += skippedSummary(skipped)
	if len(unreachable) > 0 {
		hosts := append([]string(nil), unreachable...)
		sort.Strings(hosts)
		named := hosts
		if len(named) > unreachableHostsListed {
			named = named[:unreachableHostsListed]
		}
		msg += fmt.Sprintf("; %d host(s) could not be listed, their VMs are unknown (not absent) and discovery is retried: %s",
			len(hosts), strings.Join(named, ", "))
		if more := len(hosts) - len(named); more > 0 {
			msg += fmt.Sprintf(" and %d more", more)
		}
	}
	return msg
}

// strandedSummary names, for the adoption message, the adopting
// VirtualMachines the provider refused a domain for and that could not be
// removed (at most unreachableHostsListed of them).
func strandedSummary(names []string) string {
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	shown := names
	if len(shown) > unreachableHostsListed {
		shown = shown[:unreachableHostsListed]
	}
	msg := fmt.Sprintf("; %d adopted VirtualMachine(s) wait for a domain the provider refused and could not be removed "+
		"(delete them; no provider call is made): %s", len(names), strings.Join(shown, ", "))
	if more := len(names) - len(shown); more > 0 {
		msg += fmt.Sprintf(" and %d more", more)
	}
	return msg
}

// countSkipped counts the skipped VMs with reason.
func countSkipped(skipped []adoptionSkip, reason adoptionSkipReason) int {
	n := 0
	for _, sk := range skipped {
		if sk.reason == reason {
			n++
		}
	}
	return n
}

// skippedSummary renders the skipped VMs for the adoption message, grouped
// by reason in a fixed order, each naming at most unreachableHostsListed VMs.
func skippedSummary(skipped []adoptionSkip) string {
	if len(skipped) == 0 {
		return ""
	}
	byReason := map[adoptionSkipReason][]string{}
	var order []adoptionSkipReason
	for _, sk := range skipped {
		if _, seen := byReason[sk.reason]; !seen {
			order = append(order, sk.reason)
		}
		byReason[sk.reason] = append(byReason[sk.reason], skippedVMName(sk))
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	var parts []string
	for _, reason := range order {
		names := byReason[reason]
		sort.Strings(names)
		shown := names
		if len(shown) > unreachableHostsListed {
			shown = shown[:unreachableHostsListed]
		}
		part := fmt.Sprintf("%d %s: %s", len(names), reason, strings.Join(shown, ", "))
		if more := len(names) - len(shown); more > 0 {
			part += fmt.Sprintf(" and %d more", more)
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("; %d not adopted — %s", len(skipped), strings.Join(parts, "; "))
}

// skippedVMName names a skipped VM for the adoption message: a previous
// incarnation by the VirtualMachine its stamp records (namespace/name), when
// known, and always by its domain and Host name (never an endpoint).
func skippedVMName(sk adoptionSkip) string {
	name := fmt.Sprintf("%s on %s", sk.info.ID, sk.info.HostID)
	if sk.reason == skipPreviousIncarnation && sk.info.OwnerNamespace != "" && sk.info.OwnerName != "" {
		name = fmt.Sprintf("%s/%s (%s)", sk.info.OwnerNamespace, sk.info.OwnerName, name)
	}
	return name
}

// clusteredAdoptionFailed records a discovery failure on the Provider and
// requeues.
func (r *VMAdoptionReconciler) clusteredAdoptionFailed(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	msg, reason string) (ctrl.Result, error) {
	provider.Status.Adoption.Message = msg
	if err := r.Status().Update(ctx, provider); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update adoption status")
	}
	metrics.RecordError(reason, metrics.ComponentManager)
	return ctrl.Result{RequeueAfter: clusteredAdoptionRetryInterval}, nil
}

// routedAdopter returns providerInstance's TransferOwner when the provider
// reports supports_routed_adoption (D7, fail closed). Otherwise it records why
// on the Provider and returns false; nothing is listed or created.
func (r *VMAdoptionReconciler) routedAdopter(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	providerInstance contracts.Provider) (contracts.OwnerTransferrer, bool) {
	logger := log.FromContext(ctx)
	reporter, okCaps := providerInstance.(contracts.CapabilityReporter)
	transferrer, okTransfer := providerInstance.(contracts.OwnerTransferrer)
	msg := clusteredAdoptionNotSupportedMessage
	if okCaps && okTransfer {
		caps, err := reporter.GetCapabilities(ctx)
		switch {
		case err != nil:
			msg = fmt.Sprintf("Waiting for the provider's capabilities before a clustered discovery: %v", err)
			metrics.RecordError(errReasonAdoptionCapabilities, metrics.ComponentManager)
		case caps.SupportsRoutedAdoption:
			return transferrer, true
		}
	}
	logger.Info("Not adopting from a clustered provider", "provider", provider.Name, "reason", msg)
	provider.Status.Adoption.Message = msg
	if err := r.Status().Update(ctx, provider); err != nil {
		logger.Error(err, "Failed to update adoption status")
	}
	return nil, false
}

// planClusteredAdoption decides, keyed on (host id, VM id), what one clustered
// discovery does with each listed VM:
//
//   - managed — a VirtualMachine of this Provider is bound to (host, id), or
//     the VM is stamped with the UID of a VirtualMachine that still exists (in
//     any namespace, through any Provider object): left alone, except
//   - a VM stamped with exactly the UID of an adopted VirtualMachine of this
//     Provider that is still waiting for its binding and was created for this
//     very (host, id) (isAwaitingAdoptionOf): its binding is completed;
//   - a VM without a host id or UUID cannot be adopted (skipped);
//   - a VM on a Host whose endpoint another Host object names is skipped
//     (sharedEndpointHosts);
//   - a VM whose UUID is listed on more than one host, or one of whose disk
//     paths a VM listed on another host uses, is a cross-host duplicate
//     (skipped and reported): adopting both copies would let one's delete
//     remove the other's disk. A duplicate on an UNREACHABLE host is not seen
//     (that host's VMs are unknown); see the docs;
//   - a VM with any other owner stamp is a previous incarnation (skipped and
//     reported; ADR-0007 A6);
//   - an unstamped VM whose name is the status.id of a VirtualMachine bound
//     through a single-host Provider is skipped (guards.singleHostIDs);
//   - an unstamped VM is unmanaged and may be adopted.
//
// VMs on unreachable hosts are not in the list, and nothing is concluded
// about them.
func planClusteredAdoption(provider *infravirtrigaudiov1beta1.Provider, listed contracts.VMList,
	vms []infravirtrigaudiov1beta1.VirtualMachine, guards adoptionGuards) clusteredAdoptionPlan {
	managed := map[vmHostKey]bool{}
	liveUIDs := make(map[string]bool, len(vms))
	awaiting := map[string]*infravirtrigaudiov1beta1.VirtualMachine{}
	providerKey := client.ObjectKeyFromObject(provider)
	for i := range vms {
		vm := &vms[i]
		if vm.UID != "" {
			liveUIDs[string(vm.UID)] = true
		}
		if placementProviderKey(vm) != providerKey && vmProviderKey(vm) != providerKey {
			continue
		}
		if host := boundHost(vm); host != "" && vm.Status.ID != "" {
			managed[vmHostKey{host: host, id: vm.Status.ID}] = true
		}
		if vm.UID != "" && awaitingClusteredAdoption(vm) {
			awaiting[string(vm.UID)] = vm
		}
	}

	var plan clusteredAdoptionPlan
	skip := func(info contracts.VMInfo, reason adoptionSkipReason) {
		plan.skipped = append(plan.skipped, adoptionSkip{info: info, reason: reason})
	}
	duplicated := crossHostDuplicates(listed.VMs)
	for _, info := range listed.VMs {
		if strings.TrimSpace(info.HostID) == "" || strings.TrimSpace(info.ProviderRaw[contracts.VMInfoUUIDKey]) == "" {
			skip(info, skipNoIdentity)
			continue
		}
		if managed[vmHostKey{host: info.HostID, id: info.ID}] {
			continue
		}
		if guards.sharedEndpointHosts[info.HostID] {
			skip(info, skipSharedEndpoint)
			continue
		}
		if info.ProviderRaw[contracts.VMInfoOwnerStampStateKey] != "" {
			skip(info, skipStampUnreliable)
			continue
		}
		if ambiguousVMID(info.ID) {
			skip(info, skipAmbiguousName)
			continue
		}
		if duplicated(info) {
			skip(info, skipDuplicate)
			continue
		}
		uids := ownerUIDs(info)
		if len(uids) == 1 {
			if vm := awaiting[uids[0]]; vm != nil && isAwaitingAdoptionOf(vm, provider, info) {
				plan.complete = append(plan.complete, pendingAdoption{info: info, vm: vm})
				continue
			}
		}
		if ownedByLiveVM(info, liveUIDs) {
			continue
		}
		if len(uids) > 0 {
			skip(info, skipPreviousIncarnation)
			continue
		}
		if guards.singleHostIDs[info.ID] {
			skip(info, skipSingleHostManaged)
			continue
		}
		plan.adopt = append(plan.adopt, info)
	}
	return plan
}

// crossHostDuplicates returns a predicate that reports whether a listed VM is
// defined on more than one host: its UUID is listed on another host too, or
// one of its disk paths is a disk path of a VM listed on another host. Paths
// are compared as listed (the provider reports each domain's disk source).
func crossHostDuplicates(vms []contracts.VMInfo) func(contracts.VMInfo) bool {
	uuidHosts := map[string]map[string]bool{}
	diskHosts := map[string]map[string]bool{}
	note := func(m map[string]map[string]bool, key, host string) {
		if key == "" {
			return
		}
		if m[key] == nil {
			m[key] = map[string]bool{}
		}
		m[key][host] = true
	}
	for _, v := range vms {
		note(uuidHosts, strings.ToLower(strings.TrimSpace(v.ProviderRaw[contracts.VMInfoUUIDKey])), v.HostID)
		for _, d := range v.Disks {
			note(diskHosts, strings.TrimSpace(d.Path), v.HostID)
		}
	}
	return func(v contracts.VMInfo) bool {
		if len(uuidHosts[strings.ToLower(strings.TrimSpace(v.ProviderRaw[contracts.VMInfoUUIDKey]))]) > 1 {
			return true
		}
		for _, d := range v.Disks {
			if len(diskHosts[strings.TrimSpace(d.Path)]) > 1 {
				return true
			}
		}
		return false
	}
}

// ambiguousVMID reports whether a listed VM id is one virsh resolves as a
// domain id (optional sign, decimal digits) or a UUID (32 hex digits, any
// '-' or space separators) before trying it as a name — so the clustered
// provider refuses to transfer it (TransferOwner) and adoption skips it
// instead of creating a VirtualMachine that could only be removed again.
func ambiguousVMID(id string) bool {
	s := strings.TrimSpace(id)
	s = strings.TrimLeft(s, "+-")
	if s != "" && strings.Trim(s, "0123456789") == "" {
		return true
	}
	hex := 0
	for _, r := range id {
		switch {
		case r == '-' || r == ' ' || r == '\t':
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
			hex++
		default:
			return false
		}
	}
	return hex == 32
}

// ownerUIDs returns the non-empty owner UIDs a listed VM is stamped with.
func ownerUIDs(info contracts.VMInfo) []string {
	var out []string
	for _, uid := range strings.Split(info.ProviderRaw[contracts.VMInfoOwnerUIDKey], ",") {
		if uid = strings.TrimSpace(uid); uid != "" {
			out = append(out, uid)
		}
	}
	return out
}

// awaitingClusteredAdoption reports whether vm is an adopted VirtualMachine
// that is not bound yet: labelled adopted, no status.id, no create pending,
// not being deleted.
func awaitingClusteredAdoption(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return vmIsAdopted(vm) && vm.Status.ID == "" && pendingHost(vm) == "" && vm.DeletionTimestamp.IsZero()
}

// isAwaitingAdoptionOf reports whether vm — an adopted VirtualMachine not
// bound yet — was created to adopt info from provider: it is in the
// Provider's namespace, references the Provider, and carries the name and the
// annotations clustered adoption derives from (info.HostID, info.ID).
func isAwaitingAdoptionOf(vm *infravirtrigaudiov1beta1.VirtualMachine, provider *infravirtrigaudiov1beta1.Provider, info contracts.VMInfo) bool {
	return awaitingClusteredAdoption(vm) &&
		vm.Namespace == provider.Namespace &&
		vmProviderKey(vm) == client.ObjectKeyFromObject(provider) &&
		vm.Name == clusteredAdoptedVMName(info.HostID, info.ID) &&
		vm.Annotations[AdoptedHostAnnotation] == info.HostID &&
		vm.Annotations[AdoptedIDAnnotation] == info.ID
}

// clusteredAdoptedVMName is the name of the VirtualMachine that adopts VM id
// on host: the sanitized VM name (as single-host adoption) followed by a
// digest of (host, id), so the same domain name on two hosts gives two
// VirtualMachines and a retry finds the same one. It is a DNS label.
func clusteredAdoptedVMName(host, id string) string {
	sum := sha256.Sum256([]byte(host + "/" + id))
	suffix := hex.EncodeToString(sum[:])[:adoptedVMNameHashLen]
	base := sanitizeVMName(id)
	if base == "" {
		base = "vm"
	}
	if limit := adoptedVMNameMaxLen - adoptedVMNameHashLen - 1; len(base) > limit {
		base = strings.Trim(base[:limit], "-")
	}
	return base + "-" + suffix
}

// adoptClusteredVM adopts one unmanaged VM a clustered provider listed:
// resolves its Host, creates (or re-finds) the adopting VirtualMachine and
// hands it the domain (completeClusteredAdoption).
func (r *VMAdoptionReconciler) adoptClusteredVM(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	transferrer contracts.OwnerTransferrer, providerInstance contracts.Provider, info contracts.VMInfo) error {
	logger := log.FromContext(ctx).WithValues("host", info.HostID, "vm_id", info.ID)
	if _, err := r.adoptionHost(ctx, provider, info.HostID); err != nil {
		return err
	}

	name := clusteredAdoptedVMName(info.HostID, info.ID)
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	err := r.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: name}, vm)
	switch {
	case apierrors.IsNotFound(err):
		vm, err = r.createClusteredAdoptedVM(ctx, provider, info, name)
		if err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("get VirtualMachine %s/%s: %w", provider.Namespace, name, err)
	default:
		// A retry after an earlier attempt created the VirtualMachine but did
		// not finish, or a VirtualMachine someone created under that name.
		if !isAwaitingAdoptionOf(vm, provider, info) {
			logger.V(1).Info("A VirtualMachine of the adopted name exists and is not waiting for this adoption", "vm_name", name)
			return fmt.Errorf("VirtualMachine %s/%s exists and is not waiting for this adoption: %w", vm.Namespace, name, errAdoptionSkipped)
		}
	}
	return r.completeAdoption(ctx, provider, transferrer, providerInstance, vm, info)
}

// completeAdoption runs completeClusteredAdoption for an adopting
// VirtualMachine (newly created, left by an earlier discovery, or waiting for
// a lost binding) and, when the provider refused the owner transfer for good
// (errTransferRefused), removes that VirtualMachine so it does not wait for a
// binding it will never get (removeStrandedAdoptedVM). A VM it could not
// remove is reported as stranded (errAdoptionStranded). Any other failure —
// including one after a successful transfer — keeps the VM, and the next
// discovery retries the binding from the stamp.
func (r *VMAdoptionReconciler) completeAdoption(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	transferrer contracts.OwnerTransferrer, providerInstance contracts.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine, info contracts.VMInfo) error {
	err := r.completeClusteredAdoption(ctx, provider, transferrer, providerInstance, vm, info, nil)
	if !errors.Is(err, errTransferRefused) {
		return err
	}
	if !r.removeStrandedAdoptedVM(ctx, provider, vm, info) {
		return fmt.Errorf("%w (%s/%s): %w", errAdoptionStranded, vm.Namespace, vm.Name, err)
	}
	return err
}

// refusedForGood reports whether an adoption failed with a provider answer a
// retry cannot change: the domain is owned by someone else or its stamp is
// unusable (Conflict), it is gone or was replaced (NotFound), or the request
// was refused (InvalidSpec).
func refusedForGood(err error) bool {
	return err != nil && (contracts.IsConflict(err) || contracts.IsNotFound(err) || contracts.IsInvalidSpec(err))
}

// removeStrandedAdoptedVM deletes vm — a VirtualMachine waiting to adopt the
// domain info names, whose owner transfer the provider refused for good — so
// it does not wait for a binding it will never get, whichever discovery
// created it. It re-reads the VM first and deletes it only while the fresh
// object is the same VirtualMachine (UID), unbound and still waiting for
// exactly this (host, id) (isAwaitingAdoptionOf); such a VM has no status.id
// and no pending host, so its deletion makes no provider call. The delete is
// preconditioned on the UID and resourceVersion read. It reports whether the
// VM is out of the way (deleted, gone, replaced, bound or being deleted);
// false means it could not be removed and is left for an administrator.
func (r *VMAdoptionReconciler) removeStrandedAdoptedVM(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine, info contracts.VMInfo) bool {
	logger := log.FromContext(ctx).WithValues("vm", vm.Name)
	fresh := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := r.freshReader().Get(ctx, client.ObjectKeyFromObject(vm), fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		logger.Error(err, "Failed to re-read the adopted VirtualMachine the provider refused")
		return false
	}
	if fresh.UID != vm.UID || !isAwaitingAdoptionOf(fresh, provider, info) {
		return true // replaced, bound or being deleted: not stranded
	}
	uid, rv := fresh.UID, fresh.ResourceVersion
	if err := r.Delete(ctx, fresh, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to remove the adopted VirtualMachine the provider refused")
		return false
	}
	logger.Info("Removed the adopted VirtualMachine the provider refused (no provider call)")
	return true
}

// createClusteredAdoptedVM creates the VirtualMachine (and its VMClass) that
// adopts info, in the Provider's namespace — the single-host adoption spec,
// named and annotated from (host, id). Its status is written only once the
// domain is its own.
func (r *VMAdoptionReconciler) createClusteredAdoptedVM(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	info contracts.VMInfo, name string) (*infravirtrigaudiov1beta1.VirtualMachine, error) {
	vmClass, err := r.ensureVMClass(ctx, info, provider.Namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure VMClass: %w", err)
	}
	vm := adoptedVMObject(provider, info, name, vmClass)
	vm.Annotations = map[string]string{AdoptedHostAnnotation: info.HostID, AdoptedIDAnnotation: info.ID}
	// No status until the domain is this VirtualMachine's: the API server
	// drops it on create anyway, and nothing may ever read a status.id for a
	// domain whose owner has not been transferred yet.
	vm.Status = infravirtrigaudiov1beta1.VirtualMachineStatus{}
	if err := r.Create(ctx, vm); err != nil {
		return nil, fmt.Errorf("create VirtualMachine %s/%s: %w", provider.Namespace, name, err)
	}
	log.FromContext(ctx).Info("Created the VirtualMachine that adopts a clustered VM", "vm_name", name,
		"host", info.HostID, "vm_id", info.ID)
	return vm, nil
}

// adoptionHost returns the Host host names when it is a Host of provider that
// is not being deleted; anything else is an error and nothing is adopted onto
// it (a binding to a Host that is not the Provider's would route every call
// to nowhere, and one being deleted is about to go).
func (r *VMAdoptionReconciler) adoptionHost(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, host string) (*infravirtrigaudiov1beta1.Host, error) {
	h := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: host}, h); err != nil {
		return nil, fmt.Errorf("get Host %s/%s of the listed VM: %w", provider.Namespace, host, err)
	}
	switch {
	case h.Spec.ProviderRef.Name != provider.Name:
		return nil, fmt.Errorf("host %s is not a Host of provider %s; not adopting from it", host, provider.Name)
	case !h.DeletionTimestamp.IsZero():
		return nil, fmt.Errorf("host %s is being deleted; not adopting from it", host)
	}
	return h, nil
}

// completeClusteredAdoption hands the domain info names to vm and binds vm to
// it: TransferOwner (compare-and-swap: only the stamps of VirtualMachines that
// no longer exist are replaced), a routed owner-checked Describe that must
// find the domain, then one checked status write with the binding and the
// domain's size. Nothing is bound when any step fails; a retry repeats it.
//
// It is the building block ADR-0007 Addendum A's A6 (restore re-binding) can
// reuse: a restored VirtualMachine (new UID, binding lost) whose domain is
// known from a restored hint would go through the same transfer, Describe and
// binding write.
func (r *VMAdoptionReconciler) completeClusteredAdoption(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	transferrer contracts.OwnerTransferrer, providerInstance contracts.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine, info contracts.VMInfo, replaceable []contracts.ObjectIdentity) error {
	logger := log.FromContext(ctx).WithValues("host", info.HostID, "vm_id", info.ID, "vm_name", vm.Name)
	host, err := r.adoptionHost(ctx, provider, info.HostID)
	if err != nil {
		return err
	}
	// As single-host: never bind — nor hand a domain to — a VirtualMachine
	// whose VMClass or VMImage is in another namespace that does not select
	// the Provider's (the VirtualMachine controller would refuse to manage it,
	// and its deletion would still destroy the adopted domain). A newly
	// created one references only the Provider's own namespace.
	if err := checkVMConsumerRefs(ctx, r.Client, vm); err != nil {
		if isConsumerNotAllowed(err) {
			return fmt.Errorf("VirtualMachine %s/%s references an ungranted cross-namespace object (%s): %w",
				vm.Namespace, vm.Name, err.Error(), errAdoptionSkipped)
		}
		return fmt.Errorf("check consumer grants of adopted VirtualMachine %s/%s: %w", vm.Namespace, vm.Name, err)
	}
	ref := contracts.VMRef{ID: info.ID, HostID: info.HostID, Owner: vmOwnerIdentity(vm)}
	if ref.Owner.IsZero() {
		return fmt.Errorf("VirtualMachine %s/%s has no UID yet", vm.Namespace, vm.Name)
	}

	// Every stamp the domain was listed with must be this VirtualMachine's (an
	// idempotent retry after a lost binding write) or one the caller names as
	// replaceable. Adoption names none until ADR-0007 A6.4: any other stamp is
	// a previous incarnation (planClusteredAdoption skips it; this re-checks
	// it). A6.4's re-attach will name the previous incarnation.
	replaceableUIDs := make([]string, 0, len(replaceable))
	for _, o := range replaceable {
		replaceableUIDs = append(replaceableUIDs, o.UID)
	}
	for _, uid := range ownerUIDs(info) {
		if uid != ref.Owner.UID && !slices.Contains(replaceableUIDs, uid) {
			return fmt.Errorf("VM %s on host %s carries another VirtualMachine's owner stamp: %w",
				info.ID, info.HostID, errAdoptionSkipped)
		}
	}
	// Right before the transfer, prove with an uncached read that no
	// replaceable owner exists (defense in depth against a lagging cache).
	if err := r.confirmOwnersGone(ctx, replaceable); err != nil {
		return err
	}
	if err := transferrer.TransferOwner(ctx, contracts.TransferOwnerRequest{
		VM:                   ref,
		ReplaceableOwnerUIDs: replaceableUIDs,
		ExpectedUUID:         info.ProviderRaw[contracts.VMInfoUUIDKey],
	}); err != nil {
		if refusedForGood(err) {
			return fmt.Errorf("transfer the owner of VM %s on host %s: %w: %w", info.ID, info.HostID, errTransferRefused, err)
		}
		return fmt.Errorf("transfer the owner of VM %s on host %s: %w", info.ID, info.HostID, err)
	}

	desc, err := providerInstance.Describe(ctx, ref)
	if err != nil {
		return fmt.Errorf("describe adopted VM %s on host %s: %w", info.ID, info.HostID, err)
	}
	if !desc.Exists {
		return fmt.Errorf("adopted VM %s on host %s is not visible to its VirtualMachine after the owner transfer", info.ID, info.HostID)
	}

	class := r.adoptedClass(ctx, vm)
	cpu, mem, ceiling := adoptedSize(vm, class, info, desc)
	if err := r.writeAdoptionBinding(ctx, provider, vm, info, adoptionBinding{
		pool: host.Spec.PoolRef.Name, cpu: cpu, memMiB: mem, ceilingMiB: ceiling, desc: desc,
	}); err != nil {
		return err
	}
	logger.Info("Adopted clustered VM", "cpu", cpu, "memoryMiB", mem, "memoryCeilingMiB", ceiling)
	return nil
}

// adoptionBinding is what the binding write of an adopted clustered VM
// records besides its id, host and Provider.
type adoptionBinding struct {
	pool               string
	cpu                int32
	memMiB, ceilingMiB int64
	desc               contracts.DescribeResponse
}

// writeAdoptionBinding writes the binding of vm — adopted for info from
// provider — in one status write: status.id, boundProvider, placement (host,
// pool, memory ceiling), currentResources, the observed power state and IPs,
// and Placed=True/Bound. Right before that status write it sets the
// VirtualMachine's restore marker (infra.virtrigaud.io/placement-uid, ADR-0007
// A6.2, R1) to its own UID — a metadata patch, as the status subresource
// cannot carry an annotation — so an adopted VM is never held as restored.
//
// The VirtualMachine controller adds its finalizer and writes the VM's status
// right after the VM is created, so the object read before the owner transfer
// is usually stale: the write re-reads the VM (from the API server when an
// uncached reader is configured) and retries on conflict. It writes only
// while the fresh object is the same VirtualMachine (UID) and still waiting
// for exactly this binding (isAwaitingAdoptionOf, not being deleted); a VM
// already bound to this (host, id) is a success. Anything else — or
// conflicts that outlast the retries — is errAdoptionDeferred: the next
// discovery completes the binding from the stamp.
func (r *VMAdoptionReconciler) writeAdoptionBinding(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine, info contracts.VMInfo, b adoptionBinding) error {
	key := client.ObjectKeyFromObject(vm)
	wantUID := vm.UID
	var alreadyBound bool
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
		case latest.Status.ID == info.ID && boundHost(latest) == info.HostID && latest.DeletionTimestamp.IsZero():
			alreadyBound = true
			return nil
		case !isAwaitingAdoptionOf(latest, provider, info):
			return fmt.Errorf("VirtualMachine %s is no longer waiting for this adoption: %w", key, errAdoptionDeferred)
		}
		// ADR-0007 A6, R1: the restore marker names the adopting
		// VirtualMachine's own UID, written right before (and with the same
		// resourceVersion lock as) its binding, so an adopted VM is never
		// held as restored and a backup of it carries the UID it was bound
		// under. A conflict re-reads the VM and retries both.
		if err := ensurePlacementUIDMarker(ctx, r.Client, latest); err != nil {
			return err
		}
		now := metav1.Now()
		ceiling := b.ceilingMiB
		cpu, mem := b.cpu, b.memMiB
		latest.Status.ID = info.ID
		recordBoundProvider(latest, provider)
		latest.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{
			Host:              info.HostID,
			Pool:              b.pool,
			MemoryCeilingMiB:  &ceiling,
			LastScheduledTime: &now,
			Reason:            "adopted: the VM was found on this host (ADR-0007 Addendum A, slice 4)",
		}
		latest.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
		latest.Status.PowerState = observedPowerState(b.desc.PowerState)
		latest.Status.IPs = b.desc.IPs
		latest.Status.Provider = adoptedProviderRaw(info.ProviderRaw)
		setPlacedCondition(latest, metav1.ConditionTrue, k8s.ReasonBound, fmt.Sprintf("VM is bound to host %s", info.HostID))
		if err := r.Status().Update(ctx, latest); err != nil {
			return err
		}
		*vm = *latest
		return nil
	})
	switch {
	case err == nil:
		if alreadyBound {
			log.FromContext(ctx).Info("Adopted VirtualMachine is already bound to its domain", "vm", key.String())
		}
		return nil
	case apierrors.IsConflict(err):
		return fmt.Errorf("write the binding of adopted VirtualMachine %s: kept conflicting: %w", key, errAdoptionDeferred)
	case errors.Is(err, errAdoptionDeferred):
		return err
	}
	return fmt.Errorf("write the binding of adopted VirtualMachine %s: %w", key, err)
}

// errOwnerStillExists marks a replaceable owner that the uncached read found
// alive: its domain is never re-stamped.
var errOwnerStillExists = errors.New("the stamped owner VirtualMachine still exists")

// confirmOwnersGone proves, with an UNCACHED read of each owner's
// namespace/name, that no VirtualMachine with one of the owners' UIDs exists
// (a terminating one counts as existing). A VirtualMachine's namespace and
// name never change, so a GET of the stamp's namespace/name finds it if it
// exists. It fails closed: an owner without a namespace, name or UID, a read
// error, or no uncached reader configured refuses the transfer.
func (r *VMAdoptionReconciler) confirmOwnersGone(ctx context.Context, owners []contracts.ObjectIdentity) error {
	if len(owners) == 0 {
		return nil
	}
	if r.APIReader == nil {
		return fmt.Errorf("no uncached reader to confirm that a stamped owner is gone: %w", errOwnerStillExists)
	}
	for _, o := range owners {
		if o.UID == "" || o.Namespace == "" || o.Name == "" {
			return fmt.Errorf("stamped owner %q cannot be looked up (namespace/name unknown): %w", o.UID, errOwnerStillExists)
		}
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: o.Namespace, Name: o.Name}, vm)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return fmt.Errorf("confirm stamped owner %s/%s is gone: %w", o.Namespace, o.Name, err)
		case string(vm.UID) == o.UID:
			return fmt.Errorf("stamped owner %s/%s (uid %s): %w", o.Namespace, o.Name, o.UID, errOwnerStillExists)
		}
	}
	return nil
}

// hostsWithSharedEndpoints returns the names of provider's Hosts whose
// endpoint (scheme, host and port; the user is ignored) another Host object —
// of any Provider, in any namespace — or a single-host (not clustered)
// Provider's spec.endpoint also names, and logs a warning for each. Adoption
// from such a host is skipped: two Providers fronting one hypervisor could
// each adopt the same unstamped domain, and a single-host provider does not
// stamp the domains it manages. Residual risk: the same hypervisor reached
// under two names (a DNS alias, a hostname and its IP) is not recognized.
func hostsWithSharedEndpoints(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	hosts []infravirtrigaudiov1beta1.Host, providers []infravirtrigaudiov1beta1.Provider) map[string]bool {
	logger := log.FromContext(ctx)
	byEndpoint := map[string][]string{}
	for i := range hosts {
		h := &hosts[i]
		if key := endpointKey(h.Spec.Endpoint); key != "" {
			byEndpoint[key] = append(byEndpoint[key], "Host "+h.Namespace+"/"+h.Name)
		}
	}
	for i := range providers {
		p := &providers[i]
		if isClusterTopology(p) {
			continue
		}
		if key := endpointKey(p.Spec.Endpoint); key != "" {
			byEndpoint[key] = append(byEndpoint[key], "Provider "+p.Namespace+"/"+p.Name)
		}
	}
	shared := map[string]bool{}
	for i := range hosts {
		h := &hosts[i]
		if h.Namespace != provider.Namespace || h.Spec.ProviderRef.Name != provider.Name {
			continue
		}
		if users := byEndpoint[endpointKey(h.Spec.Endpoint)]; len(users) > 1 {
			shared[h.Name] = true
			logger.Info("WARNING: a Host shares its hypervisor endpoint with another Host or a single-host Provider; "+
				"not adopting from it (one clustered Provider per host endpoint)", "host", h.Name, "users", users)
		}
	}
	return shared
}

// singleHostBoundIDs returns the status.id of every VirtualMachine in vms that
// is bound through a Provider that is not clustered — including one whose
// Provider is not in providers (conservative). A single-host provider never
// stamps the domains it manages, so on a hypervisor fronted by both a
// single-host and a clustered Provider the id is the only sign an unstamped
// domain is managed.
func singleHostBoundIDs(vms []infravirtrigaudiov1beta1.VirtualMachine, providers []infravirtrigaudiov1beta1.Provider) map[string]bool {
	clustered := map[types.NamespacedName]bool{}
	for i := range providers {
		if isClusterTopology(&providers[i]) {
			clustered[client.ObjectKeyFromObject(&providers[i])] = true
		}
	}
	ids := map[string]bool{}
	for i := range vms {
		vm := &vms[i]
		if vm.Status.ID != "" && !clustered[placementProviderKey(vm)] {
			ids[vm.Status.ID] = true
		}
	}
	return ids
}

// endpointDefaultPorts are the ports an endpoint without one connects to, by
// scheme: libvirt's SSH transport (and plain ssh) use SSH's 22, its TCP and
// TLS transports libvirtd's 16509 and 16514. A scheme not listed keeps an
// empty port.
var endpointDefaultPorts = map[string]string{
	"qemu+ssh": "22",
	"ssh":      "22",
	"qemu+tcp": "16509",
	"qemu+tls": "16514",
}

// endpointKey reduces a Host endpoint to what identifies the hypervisor: the
// URL scheme, host name (lower-cased) and port, without the user or path. An
// omitted port is the scheme's default (endpointDefaultPorts), so
// "qemu+ssh://h1/system" and "qemu+ssh://h1:22/system" are the same host. An
// endpoint that does not parse is compared as written.
func endpointKey(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return endpoint
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = endpointDefaultPorts[scheme]
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// adoptedProviderRaw is the listing's provider data recorded in an adopted
// VM's status.provider, without the owner-stamp keys: after the transfer they
// describe the domain's previous owner (possibly another namespace's UID), are
// stale, and are nothing the VM's reader needs.
func adoptedProviderRaw(raw map[string]string) map[string]string {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch k {
		case contracts.VMInfoOwnerUIDKey, contracts.VMInfoOwnerStampStateKey:
			continue
		}
		out[k] = v
	}
	return out
}

// freshReader is the reader the adoption re-reads objects with before it
// writes or decides on them: the uncached API reader when configured, the
// cached client otherwise.
func (r *VMAdoptionReconciler) freshReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// adoptedClass returns vm's VMClass, or nil when it cannot be read (the size
// then comes from the listing).
func (r *VMAdoptionReconciler) adoptedClass(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VMClass {
	key, ok := vmClassKey(vm)
	if !ok {
		return nil
	}
	class := &infravirtrigaudiov1beta1.VMClass{}
	if err := getForConsumer(ctx, r.Client, key, class, vm.Namespace); err != nil {
		log.FromContext(ctx).V(1).Info("VMClass of an adopted VM not readable; sizing it from the listing", "class", key.String(), "error", err.Error())
		return nil
	}
	return class
}

// adoptedSize is the size an adopted clustered VM is recorded at:
//
//   - CPU and memory are its effective resources (its adopted VMClass and
//     spec.resources — what the VirtualMachine controller compares the spec
//     with, so the adoption triggers no Reconfigure), or the listed size when
//     the class cannot be read; the CPU is raised to the vCPUs Describe
//     reports online, never lowered;
//   - the memory ceiling is Describe's memory maximum when it exceeds that
//     memory, else 0 (none), exactly as syncMemoryCeiling records it.
//
// Every provider figure is bounded as for a created VM (clampReported*).
func adoptedSize(vm *infravirtrigaudiov1beta1.VirtualMachine, class *infravirtrigaudiov1beta1.VMClass,
	info contracts.VMInfo, desc contracts.DescribeResponse) (cpu int32, mem, ceiling int64) {
	cpu, mem = clampReportedVCPUs(info.CPU), clampReportedMemoryMiB(info.MemoryMiB)
	if class != nil {
		if c, m, err := effectiveResources(vm, class); err == nil {
			cpu, mem = c, int64(m)
		}
	}
	cpu = max(cpu, clampReportedVCPUs(desc.VCPUs), minFootprintCPU)
	mem = max(mem, minFootprintMemoryMiB)
	if reported := clampReportedMemoryMiB(desc.MaxMemoryMiB); reported > mem {
		ceiling = reported
	}
	return cpu, mem, ceiling
}
