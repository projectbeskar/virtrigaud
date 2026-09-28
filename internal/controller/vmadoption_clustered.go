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
)

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

	plan := planClusteredAdoption(provider, listed, vmList.Items)
	var unmanaged []contracts.VMInfo
	for _, info := range plan.adopt {
		if filter != nil && !r.matchesFilter(info, filter) {
			logger.V(1).Info("VM filtered out by adoption filter", "host", info.HostID, "vm_id", info.ID)
			continue
		}
		unmanaged = append(unmanaged, info)
	}
	logger.Info("Clustered discovery", "listed", len(listed.VMs), "unmanaged", len(unmanaged),
		"pendingBindings", len(plan.complete), "unreachableHosts", listed.UnreachableHostIDs)

	adopted, failed, deferred := int32(0), int32(0), int32(0)
	count := func(err error, msg string, info contracts.VMInfo) {
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
		count(r.completeClusteredAdoption(ctx, provider, transferrer, providerInstance, p.vm, p.info),
			"Failed to complete the adoption of a VM", p.info)
	}
	for _, info := range unmanaged {
		count(r.adoptClusteredVM(ctx, provider, transferrer, providerInstance, info), "Failed to adopt VM", info)
	}

	now := metav1.Now()
	provider.Status.Adoption.LastDiscoveryTime = &now
	// #nosec G115 -- a VM count never approaches 2^31.
	provider.Status.Adoption.DiscoveredVMs = int32(len(unmanaged))
	provider.Status.Adoption.AdoptedVMs = adopted
	provider.Status.Adoption.FailedAdoptions = failed
	provider.Status.Adoption.Message = clusteredAdoptionMessage(adopted, failed, deferred, plan.skipped, listed.UnreachableHostIDs)
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
//   - a VM whose UUID is listed on more than one host, or one of whose disk
//     paths a VM listed on another host uses, is a cross-host duplicate
//     (skipped and reported): adopting both copies would let one's delete
//     remove the other's disk. A duplicate on an UNREACHABLE host is not seen
//     (that host's VMs are unknown); see the docs;
//   - a VM with any other owner stamp is a previous incarnation (skipped and
//     reported; ADR-0007 A6);
//   - an unstamped VM is unmanaged and may be adopted.
//
// VMs on unreachable hosts are not in the list, and nothing is concluded
// about them.
func planClusteredAdoption(provider *infravirtrigaudiov1beta1.Provider, listed contracts.VMList,
	vms []infravirtrigaudiov1beta1.VirtualMachine) clusteredAdoptionPlan {
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
	return r.completeClusteredAdoption(ctx, provider, transferrer, providerInstance, vm, info)
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
	vm *infravirtrigaudiov1beta1.VirtualMachine, info contracts.VMInfo) error {
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

	// Until ADR-0007 A6.4 adoption takes only an unstamped domain, or one
	// already stamped with this VirtualMachine (an idempotent retry after a
	// lost binding write). Any other stamp is a previous incarnation
	// (planClusteredAdoption skips it; this re-checks it), so nothing is ever
	// listed as replaceable here.
	for _, uid := range ownerUIDs(info) {
		if uid != ref.Owner.UID {
			return fmt.Errorf("VM %s on host %s carries another VirtualMachine's owner stamp: %w",
				info.ID, info.HostID, errAdoptionSkipped)
		}
	}
	if err := transferrer.TransferOwner(ctx, contracts.TransferOwnerRequest{
		VM:           ref,
		ExpectedUUID: info.ProviderRaw[contracts.VMInfoUUIDKey],
	}); err != nil {
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
// and Placed=True/Bound.
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
		latest.Status.Provider = info.ProviderRaw
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
