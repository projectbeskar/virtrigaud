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
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Requeue cadences for the Host inventory-sync loop (ADR-0007 D1/D3). These are
// heartbeat/observed-state intervals, deliberately NOT tight loops: a Host's
// live facts (capacity/health) change on the order of minutes, and a clustered
// provider's GetHostInfo is a real per-host probe (short-lived connection lease
// + read-only virsh queries), so hammering it would waste host resources.
const (
	// hostHeartbeatInterval is the steady-state requeue after a successful sync
	// (or a config-level state that will not self-heal by retrying faster, e.g. a
	// non-clustered provider). It keeps Host.status.lastHeartbeatTime live so an
	// operator can tell a stale Host from a fresh one.
	hostHeartbeatInterval = 60 * time.Second
	// hostSyncBackoffInterval is the shorter requeue after a transient failure
	// (provider missing/not-ready/unreachable, or a GetHostInfo error), so a Host
	// recovers promptly once its provider comes back without a full heartbeat wait.
	hostSyncBackoffInterval = 15 * time.Second
	// hostInUseRetryInterval re-checks a Host whose deletion is blocked by the
	// in-use finalizer. VirtualMachine changes do not trigger a Host reconcile,
	// so this is how the Host notices its last VM went away.
	hostInUseRetryInterval = 30 * time.Second
)

// hostInUseListedVMs caps how many VirtualMachine names the HostInUse condition
// message lists (the count is always given in full).
const hostInUseListedVMs = 10

// Condition reasons surfaced on Host.status.conditions[Ready] (ADR-0007 D3). The
// health enum carries the fine-grained observed state; the Ready condition and
// these reasons carry the WHY, in a small, operator-facing taxonomy.
const (
	// reasonHostReady is Ready=True: the provider reports the host reachable and
	// schedulable.
	reasonHostReady = "HostReady"
	// reasonHostNotReady is Ready=False: the provider probed the host and reports
	// it not currently usable for placement/migration.
	reasonHostNotReady = "HostNotReady"
	// reasonHostHealthUnknown is Ready=Unknown: the sync succeeded but the provider
	// reported no health state for the host.
	reasonHostHealthUnknown = "HostHealthUnknown"
	// reasonProviderUnavailable is Ready=False: the Host's Provider is missing, its
	// runtime is not ready, or the inventory RPC failed transiently.
	reasonProviderUnavailable = "ProviderUnavailable"
	// reasonProviderNotClustered is Ready=False: the Host references a Provider that
	// is not clustered (spec.topology != cluster) or does not implement the
	// inventory RPC (Unimplemented) — a misconfiguration, not a transient failure.
	reasonProviderNotClustered = "ProviderNotClustered"
	// reasonHostNotFound is Ready=False: the Provider is clustered and reachable but
	// does not know this host id (e.g. it was dropped from the rendered inventory).
	reasonHostNotFound = "HostNotFound"
	// reasonHostInUse is Ready=False on a Host being deleted: VirtualMachines
	// are still bound to it (or have a create pending on it), so the in-use
	// finalizer holds the deletion (ADR-0007 Addendum A, A1).
	reasonHostInUse = "HostInUse"
)

// Reason labels for metrics.RecordError from the Host reconciler. Kept small and
// operationally meaningful (the `reason` label is cardinality-sensitive).
const (
	errReasonHostGet             = "host-get"
	errReasonHostProviderResolve = "host-provider-resolve"
	errReasonHostGetInfo         = "host-get-info"
	errReasonHostStatusUpdate    = "host-status-update"
	errReasonHostFinalizer       = "host-finalizer"
)

// HostReconciler reconciles a Host object into live inventory facts (ADR-0007 P1,
// D1/D3). It is the operator side of the "brain in the operator" split: it calls
// the clustered Provider's GetHostInfo and writes the observed capacity/health
// into Host.status. It never mutates a Host spec (the admin authors it). Its one
// metadata write is the in-use finalizer (ADR-0007 Addendum A, A1): a Host cannot
// be deleted while any VirtualMachine names it in status.placement.host or
// status.placement.pendingHost, because every per-VM call for that VM — and its
// own finalizer cleanup — is routed to the Host.
type HostReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RemoteResolver ProviderResolver
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmclasses,verbs=get;list;watch

// Reconcile syncs one Host's observed inventory into its status.
//
// Named return values (`result`, `retErr`) are required by the deferred metrics
// block, which infers the reconcile outcome from them — do not change the
// signature without updating the defer.
func (r *HostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	timer := metrics.NewReconcileTimer("Host")
	defer func() {
		outcome := metrics.OutcomeSuccess
		switch {
		case retErr != nil:
			outcome = metrics.OutcomeError
		case result.Requeue || result.RequeueAfter > 0:
			outcome = metrics.OutcomeRequeue
		}
		timer.Finish(outcome)
	}()

	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		if apierrors.IsNotFound(err) {
			// Host gone (its in-use finalizer was already released).
			return ctrl.Result{}, nil
		}
		metrics.RecordError(errReasonHostGet, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("get Host %s: %w", req.NamespacedName, err)
	}

	if k8s.IsBeingDeleted(host) {
		return r.handleHostDeletion(ctx, host)
	}

	// Hold every live Host with the in-use finalizer (ADR-0007 Addendum A, A1).
	if !k8s.HasFinalizer(host, infravirtrigaudiov1beta1.HostInUseFinalizer) {
		if err := k8s.AddFinalizer(ctx, r.Client, host, infravirtrigaudiov1beta1.HostInUseFinalizer); err != nil {
			metrics.RecordError(errReasonHostFinalizer, metrics.ComponentManager)
			return ctrl.Result{}, fmt.Errorf("add in-use finalizer to Host %s: %w", req.NamespacedName, err)
		}
	}

	r.publishCommitted(ctx, host)
	return r.syncHostStatus(ctx, host)
}

// hostProviderKey is the Provider a Host belongs to: the one its providerRef
// names, in the Host's own namespace (the same-namespace model).
func hostProviderKey(host *infravirtrigaudiov1beta1.Host) types.NamespacedName {
	return types.NamespacedName{Namespace: host.Namespace, Name: host.Spec.ProviderRef.Name}
}

// publishCommitted refreshes the Host's committed-capacity gauges
// (virtrigaud_host_committed_cpu / _memory_mib) from the VirtualMachines bound
// to or pending on it — the scheduler's accounting (ADR-0007 Addendum A,
// scheduler-accuracy amendment). Best effort: a failure is logged and the
// gauges keep their last value; it never fails the reconcile.
func (r *HostReconciler) publishCommitted(ctx context.Context, host *infravirtrigaudiov1beta1.Host) {
	provider := hostProviderKey(host)
	cpu, mem, err := committedOnHost(ctx, r.Client, provider, host.Name)
	if err != nil {
		log.FromContext(ctx).V(1).Info("Could not compute the Host's committed capacity; keeping the last published value",
			"host", host.Name, "error", err.Error())
		return
	}
	metrics.SetHostCommitted(provider.String(), host.Name, cpu, mem)
}

// handleHostDeletion releases the in-use finalizer of a Host being deleted only
// once no VirtualMachine is bound to it or has a create pending on it
// (ADR-0007 Addendum A, A1). While any does, it records Ready=False/HostInUse
// naming them and re-checks on hostInUseRetryInterval; it never touches the VMs.
func (r *HostReconciler) handleHostDeletion(ctx context.Context, host *infravirtrigaudiov1beta1.Host) (ctrl.Result, error) {
	if !k8s.HasFinalizer(host, infravirtrigaudiov1beta1.HostInUseFinalizer) {
		return ctrl.Result{}, nil
	}

	users, err := r.vmsUsingHost(ctx, host)
	if err != nil {
		metrics.RecordError(errReasonHostFinalizer, metrics.ComponentManager)
		return ctrl.Result{}, err
	}
	if len(users) > 0 {
		base := host.DeepCopy()
		setHostReady(host, metav1.ConditionFalse, reasonHostInUse, hostInUseMessage(users))
		log.FromContext(ctx).Info("Host deletion blocked: still in use", "host", host.Name, "vmCount", len(users))
		return r.persist(ctx, host, base, hostInUseRetryInterval)
	}

	if err := k8s.RemoveFinalizer(ctx, r.Client, host, infravirtrigaudiov1beta1.HostInUseFinalizer); err != nil {
		metrics.RecordError(errReasonHostFinalizer, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("remove in-use finalizer from Host %s: %w", host.Name, err)
	}
	// Nothing is bound or pending on it any more: drop its series.
	metrics.DeleteHostCommitted(hostProviderKey(host).String(), host.Name)
	return ctrl.Result{}, nil
}

// hostInUseMessage renders the HostInUse condition message: the number of
// VirtualMachines holding the Host and at most hostInUseListedVMs of their
// names. The list is bounded so the message stays far below the 32768-byte
// condition-message limit (an unbounded list could wedge every status write)
// and so an admin-facing object does not enumerate every tenant's VM names.
func hostInUseMessage(users []string) string {
	listed := users
	if len(listed) > hostInUseListedVMs {
		listed = listed[:hostInUseListedVMs]
	}
	more := ""
	if n := len(users) - len(listed); n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	return fmt.Sprintf("Host is being deleted but %d VirtualMachine(s) are bound to it or have a create pending on it "+
		"(%s%s); it is kept until they are deleted or moved", len(users), strings.Join(listed, ", "), more)
}

// vmsUsingHost returns "<namespace>/<name>" of every VirtualMachine that names
// host in status.placement.host or status.placement.pendingHost through the
// Host's own Provider. Under the same-namespace model (#330) a Host belongs to
// the Provider named by its providerRef in the Host's namespace; a VM routes to
// it only when its spec.providerRef resolves to that same Provider (namespace
// defaulting to the VM's own). VMs may live in other namespaces than their
// Provider, so all VirtualMachines are listed; a same-named Host of another
// Provider is never confused with this one.
func (r *HostReconciler) vmsUsingHost(ctx context.Context, host *infravirtrigaudiov1beta1.Host) ([]string, error) {
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	if err := r.List(ctx, &vms); err != nil {
		return nil, fmt.Errorf("list VirtualMachines for Host %s/%s: %w", host.Namespace, host.Name, err)
	}
	var users []string
	for i := range vms.Items {
		vm := &vms.Items[i]
		providerNS := vm.Spec.ProviderRef.Namespace
		if providerNS == "" {
			providerNS = vm.Namespace
		}
		if providerNS != host.Namespace || vm.Spec.ProviderRef.Name != host.Spec.ProviderRef.Name {
			continue
		}
		if boundHost(vm) == host.Name || pendingHost(vm) == host.Name {
			users = append(users, vm.Namespace+"/"+vm.Name)
		}
	}
	sort.Strings(users)
	return users, nil
}

// syncHostStatus runs the inventory sync for one Host and persists status. It
// never returns a hard error for an absent/unready/misconfigured provider — those
// are recorded on status and requeued, so a bad Provider reference can never
// crash-loop the Host controller.
func (r *HostReconciler) syncHostStatus(ctx context.Context, host *infravirtrigaudiov1beta1.Host) (ctrl.Result, error) {
	// Snapshot before mutating so patchStatus can skip a no-op write, and so the
	// spec is provably untouched (we only ever assign to host.Status below).
	base := host.DeepCopy()
	host.Status.ObservedGeneration = host.Generation

	// 1. Resolve the Provider — always in the Host's own namespace (the
	//    same-namespace model; providerRef is a LocalObjectReference).
	provider, err := r.resolveProvider(ctx, host)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.setHostUnavailable(host, reasonProviderUnavailable,
				fmt.Sprintf("Provider %s not found", host.Spec.ProviderRef.Name))
			return r.persist(ctx, host, base, hostSyncBackoffInterval)
		}
		metrics.RecordError(errReasonHostProviderResolve, metrics.ComponentManager)
		r.setHostUnavailable(host, reasonProviderUnavailable,
			fmt.Sprintf("failed to get Provider %s: %v", host.Spec.ProviderRef.Name, err))
		return r.persist(ctx, host, base, hostSyncBackoffInterval)
	}

	// 2. Only a clustered Provider fronts a host inventory (ADR-0007 D9). A
	//    single-host Provider cannot answer GetHostInfo — short-circuit before the
	//    RPC and flag the misconfiguration rather than probing a doomed endpoint.
	if !isClusterTopology(provider) {
		r.setHostUnavailable(host, reasonProviderNotClustered,
			fmt.Sprintf("Provider %s is not clustered (spec.topology != cluster); a Host requires a clustered Provider", provider.Name))
		return r.persist(ctx, host, base, hostHeartbeatInterval)
	}

	// 3. Obtain the provider client the same way the VM controller does. A
	//    not-ready runtime (no endpoint / phase != Running) surfaces here as a
	//    resolve error → keep last-known facts, mark health Unknown, requeue.
	providerInstance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		metrics.RecordError(errReasonHostProviderResolve, metrics.ComponentManager)
		r.setHostUnavailable(host, reasonProviderUnavailable,
			fmt.Sprintf("Provider %s runtime is not ready: %v", provider.Name, err))
		return r.persist(ctx, host, base, hostSyncBackoffInterval)
	}

	// 4. Cheap single-host refresh (NOT a full ListHosts). The host id is the Host
	//    CR name (#315 render / #318 HostInfo.id).
	info, err := providerInstance.GetHostInfo(ctx, host.Name)
	if err != nil {
		return r.handleGetHostInfoError(ctx, host, base, provider, err)
	}

	// 5. Map the live facts into status and stamp the heartbeat.
	r.applyHostInfo(host, info)
	log.FromContext(ctx).V(1).Info("Synced Host inventory",
		"host", host.Name, "provider", provider.Name, "health", host.Status.Health)
	return r.persist(ctx, host, base, hostHeartbeatInterval)
}

// handleGetHostInfoError maps a GetHostInfo failure onto a Host status + requeue.
// A missing RPC (a non-clustered provider) or an unknown host id are steady-state
// misconfigurations (heartbeat requeue); a transient/unreachable error is a
// shorter backoff. None of them is a hard reconcile error — the Host must never
// crash-loop on a bad or unreachable provider.
func (r *HostReconciler) handleGetHostInfoError(ctx context.Context, host, base *infravirtrigaudiov1beta1.Host, provider *infravirtrigaudiov1beta1.Provider, err error) (ctrl.Result, error) {
	switch {
	case contracts.IsNotSupported(err):
		// The Provider claims clustered topology but does not implement the
		// inventory RPC (gRPC Unimplemented). Treat it like a non-clustered
		// reference: a config error, requeued at heartbeat cadence, not a crash.
		r.setHostUnavailable(host, reasonProviderNotClustered,
			fmt.Sprintf("Provider %s does not implement GetHostInfo (not a clustered provider)", provider.Name))
		return r.persist(ctx, host, base, hostHeartbeatInterval)
	case contracts.IsNotFound(err):
		// The provider is clustered and reachable but does not know this host id.
		r.setHostUnavailable(host, reasonHostNotFound,
			fmt.Sprintf("host %s is not in Provider %s inventory", host.Name, provider.Name))
		return r.persist(ctx, host, base, hostSyncBackoffInterval)
	default:
		metrics.RecordError(errReasonHostGetInfo, metrics.ComponentManager)
		log.FromContext(ctx).Error(err, "GetHostInfo failed", "host", host.Name, "provider", provider.Name)
		r.setHostUnavailable(host, reasonProviderUnavailable,
			fmt.Sprintf("GetHostInfo failed on Provider %s: %v", provider.Name, err))
		return r.persist(ctx, host, base, hostSyncBackoffInterval)
	}
}

// applyHostInfo maps a successfully-fetched contracts.HostInfo onto Host.status
// and stamps LastHeartbeatTime (a successful sync just happened, regardless of
// the reported health value). It sets the Ready condition from the mapped health.
func (r *HostReconciler) applyHostInfo(host *infravirtrigaudiov1beta1.Host, info contracts.HostInfo) {
	health, condStatus, reason, msg := mapHostHealth(info.Health)
	host.Status.Health = health

	// Allocatable capacity — pointer fields so a genuine 0 reported by the
	// provider is distinct from "not yet synced". Take addresses of locals (the
	// repo avoids the indirect k8s.io/utils/ptr dep).
	cpu := info.AllocatableCPU
	mem := info.AllocatableMemMiB
	storage := info.AllocatableStorage
	host.Status.AllocatableCPU = &cpu
	host.Status.AllocatableMemoryMiB = &mem
	host.Status.AllocatableStorageBytes = &storage

	host.Status.CPUModel = info.CPUModel
	host.Status.CPUFeatures = info.CPUFeatures
	host.Status.MachineTypes = info.MachineTypes
	host.Status.EmulatorVersion = info.EmulatorVersion

	// BoundVMs is intentionally left at its zero value here. It is operator-owned
	// state derived from VirtualMachine.status.placement.host (ADR-0007 D1), which
	// does not exist yet (a later ADR-0007 slice). Writing a synthetic 0 as if it
	// were authoritative would be misleading, so the field stays owned by the
	// future placement-sync rather than by this inventory sync.

	now := metav1.Now()
	host.Status.LastHeartbeatTime = &now
	setHostReady(host, condStatus, reason, msg)
}

// mapHostHealth maps the transport-agnostic contracts.HostHealth onto the Host
// CRD's HostHealth and the Ready condition it implies (ADR-0007 D3):
//   - Ready       → HostHealthReady,    Ready=True
//   - NotReady    → HostHealthNotReady, Ready=False
//   - Unspecified → HostHealthUnknown,  Ready=Unknown (provider reported no health)
func mapHostHealth(h contracts.HostHealth) (infravirtrigaudiov1beta1.HostHealth, metav1.ConditionStatus, string, string) {
	switch h {
	case contracts.HostHealthReady:
		return infravirtrigaudiov1beta1.HostHealthReady, metav1.ConditionTrue, reasonHostReady,
			"host is reachable and schedulable"
	case contracts.HostHealthNotReady:
		return infravirtrigaudiov1beta1.HostHealthNotReady, metav1.ConditionFalse, reasonHostNotReady,
			"provider reports the host is not currently usable for placement or migration"
	default:
		return infravirtrigaudiov1beta1.HostHealthUnknown, metav1.ConditionUnknown, reasonHostHealthUnknown,
			"provider reported no health for the host"
	}
}

// setHostUnavailable records a provider- or config-level problem: health Unknown
// plus a Ready=False condition with the given reason. It deliberately does NOT
// clear previously-synced capacity (last-known facts stay visible with health
// flipped, matching node-status convention) and does NOT touch LastHeartbeatTime
// (this was not a successful sync — a stale heartbeat is the signal operators
// read).
func (r *HostReconciler) setHostUnavailable(host *infravirtrigaudiov1beta1.Host, reason, msg string) {
	host.Status.Health = infravirtrigaudiov1beta1.HostHealthUnknown
	setHostReady(host, metav1.ConditionFalse, reason, msg)
}

// setHostReady upserts the Host's Ready condition, stamping ObservedGeneration =
// host.Generation so a client can tell which spec generation the condition
// reflects (status-condition best practice). meta.SetStatusCondition only bumps
// LastTransitionTime on an actual status change, keeping the write idempotent.
func setHostReady(host *infravirtrigaudiov1beta1.Host, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: host.Generation,
	})
}

// persist writes Host.status via the status subresource (spec is never sent) and
// returns the requeue. A semantically-unchanged status is skipped so an
// idempotent re-reconcile issues no write (a successful sync always changes
// status because LastHeartbeatTime advances).
func (r *HostReconciler) persist(ctx context.Context, host, base *infravirtrigaudiov1beta1.Host, requeueAfter time.Duration) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(host.Status, base.Status) {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	if err := r.Status().Update(ctx, host); err != nil {
		metrics.RecordError(errReasonHostStatusUpdate, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("update Host %s status: %w", host.Name, err)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// resolveProvider fetches the Provider referenced by the Host. Under the
// same-namespace model (security) Host.spec.providerRef is a
// LocalObjectReference, so the Provider is ALWAYS looked up in the Host's own
// namespace — a Host can never drive a GetHostInfo against another namespace's
// Provider. The error is wrapped with %w so callers can still test
// apierrors.IsNotFound through the wrap.
func (r *HostReconciler) resolveProvider(ctx context.Context, host *infravirtrigaudiov1beta1.Host) (*infravirtrigaudiov1beta1.Provider, error) {
	key := types.NamespacedName{Name: host.Spec.ProviderRef.Name, Namespace: host.Namespace}
	provider := &infravirtrigaudiov1beta1.Provider{}
	if err := r.Get(ctx, key, provider); err != nil {
		return nil, fmt.Errorf("get Provider %s: %w", key.Name, err)
	}
	return provider, nil
}

// getProviderInstance resolves a Provider to a remote provider client, reusing
// the manager-side resolver the VM controller uses (no new provider-client path).
func (r *HostReconciler) getProviderInstance(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	if r.RemoteResolver == nil {
		return nil, fmt.Errorf("no remote resolver available")
	}
	return r.RemoteResolver.GetProvider(ctx, provider) //nolint:wrapcheck
}

// SetupWithManager wires the Host controller. It watches Host CRs (reconciling on
// create and spec/generation change, ignoring its own status writes to avoid a
// self-trigger loop) and sustains the inventory heartbeat via RequeueAfter, which
// is independent of the event predicate.
func (r *HostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.Host{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Named("host").
		Complete(r)
}
