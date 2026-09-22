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
)

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
)

// Reason labels for metrics.RecordError from the Host reconciler. Kept small and
// operationally meaningful (the `reason` label is cardinality-sensitive).
const (
	errReasonHostGet             = "host-get"
	errReasonHostProviderResolve = "host-provider-resolve"
	errReasonHostGetInfo         = "host-get-info"
	errReasonHostStatusUpdate    = "host-status-update"
)

// HostReconciler reconciles a Host object into live inventory facts (ADR-0007 P1,
// D1/D3). It is the operator side of the "brain in the operator" split: it calls
// the clustered Provider's GetHostInfo and writes the observed capacity/health
// into Host.status. It is a read-only, status-only sync — it never mutates a Host
// spec and never adds a finalizer (there are no external resources to clean up
// here; the Host spec is admin-authored desired inventory).
type HostReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RemoteResolver ProviderResolver
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch

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
			// Host deleted. Nothing to clean up: this controller owns no external
			// resources and holds no finalizer (observed-state sync only).
			return ctrl.Result{}, nil
		}
		metrics.RecordError(errReasonHostGet, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("get Host %s: %w", req.NamespacedName, err)
	}

	return r.syncHostStatus(ctx, host)
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

	// 1. Resolve the Provider. Namespace defaults to the Host's namespace when the
	//    ref omits one, matching the other controllers.
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

// resolveProvider fetches the Provider referenced by the Host. The ref namespace
// defaults to the Host's own namespace when unset. The error is wrapped with %w
// so callers can still test apierrors.IsNotFound through the wrap.
func (r *HostReconciler) resolveProvider(ctx context.Context, host *infravirtrigaudiov1beta1.Host) (*infravirtrigaudiov1beta1.Provider, error) {
	key := types.NamespacedName{Name: host.Spec.ProviderRef.Name, Namespace: host.Namespace}
	if host.Spec.ProviderRef.Namespace != "" {
		key.Namespace = host.Spec.ProviderRef.Namespace
	}
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
