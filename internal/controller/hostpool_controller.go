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

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// Condition reasons surfaced on HostPool.status.conditions[Ready]. Ready=True
// means the scheduler has at least one host in the pool it can place a new VM
// on; the two False reasons tell an empty pool apart from a pool whose hosts
// are all unusable.
const (
	// reasonHostPoolHostsAvailable is Ready=True: at least one member Host is
	// schedulable (status.schedulableHosts > 0).
	reasonHostPoolHostsAvailable = "HostsAvailable"
	// reasonHostPoolNoMemberHosts is Ready=False: no Host in the pool's
	// namespace names the pool in spec.poolRef.
	reasonHostPoolNoMemberHosts = "NoMemberHosts"
	// reasonHostPoolNoSchedulableHosts is Ready=False: the pool has member
	// Hosts but none of them is schedulable.
	reasonHostPoolNoSchedulableHosts = "NoSchedulableHosts"
)

// Reason labels for metrics.RecordError from the HostPool reconciler. Kept
// small: the `reason` label is cardinality-sensitive.
const (
	errReasonHostPoolGet          = "hostpool-get"
	errReasonHostPoolListHosts    = "hostpool-list-hosts"
	errReasonHostPoolStatusUpdate = "hostpool-status-update"
)

// HostPoolReconciler writes HostPool.status from the pool's member Hosts
// (ADR-0007 D3): how many there are, how many report Health=Ready, and how
// many the scheduler can place a new VM on. It is status-only: it never
// changes a HostPool's spec or metadata, and never writes a Host.
//
// A Host is a member of a pool when it is in the pool's namespace and its
// spec.poolRef names the pool (the same-namespace model: Host and HostPool
// references are namespace-local, so a Host in another namespace that names
// a same-named pool is never counted).
type HostPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// hostPoolCounts is the member-host tally a HostPool's status reports.
type hostPoolCounts struct {
	// total is every member Host, including one being deleted.
	total int32
	// ready is every member Host whose status.health is Ready.
	ready int32
	// schedulable is every member Host the scheduler can place a new VM on.
	schedulable int32
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hostpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hostpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts,verbs=get;list;watch

// Reconcile recounts one HostPool's member Hosts and writes the counts and the
// Ready condition to its status. It writes nothing when the status would not
// change, so the frequent Host heartbeats never turn into pool writes.
//
// Named return values (`result`, `retErr`) are required by the deferred metrics
// block, which infers the reconcile outcome from them.
func (r *HostPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	timer := metrics.NewReconcileTimer("HostPool")
	defer func() {
		outcome := metrics.OutcomeSuccess
		if retErr != nil {
			outcome = metrics.OutcomeError
		}
		timer.Finish(outcome)
	}()

	pool := &infravirtrigaudiov1beta1.HostPool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		metrics.RecordError(errReasonHostPoolGet, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("get HostPool %s: %w", req.NamespacedName, err)
	}
	if k8s.IsBeingDeleted(pool) {
		// A HostPool carries no finalizer of ours; nothing to release, and a
		// status write to an object on its way out is pointless.
		return ctrl.Result{}, nil
	}

	// Members are only ever looked up in the pool's own namespace (the
	// same-namespace model).
	var hosts infravirtrigaudiov1beta1.HostList
	if err := r.List(ctx, &hosts, client.InNamespace(pool.Namespace)); err != nil {
		metrics.RecordError(errReasonHostPoolListHosts, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("list Hosts in namespace %s for HostPool %s: %w", pool.Namespace, pool.Name, err)
	}

	base := pool.DeepCopy()
	applyHostPoolStatus(pool, countPoolHosts(pool, hosts.Items))
	if equality.Semantic.DeepEqual(pool.Status, base.Status) {
		return ctrl.Result{}, nil
	}

	// HostPool status has a single writer (this controller), and every field
	// is recomputed from the live member Hosts on each reconcile, so a merge
	// patch (no resourceVersion precondition) cannot drop another writer's
	// field and never fails on a conflict. A count computed from a cache that
	// lags behind is corrected by the Host event that follows it.
	if err := r.Status().Patch(ctx, pool, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		metrics.RecordError(errReasonHostPoolStatusUpdate, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("patch HostPool %s status: %w", req.NamespacedName, err)
	}
	log.FromContext(ctx).V(1).Info("Updated HostPool status",
		"hostPool", pool.Name,
		"totalHosts", pool.Status.TotalHosts,
		"readyHosts", pool.Status.ReadyHosts,
		"schedulableHosts", pool.Status.SchedulableHosts)
	return ctrl.Result{}, nil
}

// isHostPoolMember reports whether host is a member of pool: in the pool's
// namespace, with spec.poolRef naming the pool.
func isHostPoolMember(pool *infravirtrigaudiov1beta1.HostPool, host *infravirtrigaudiov1beta1.Host) bool {
	return host.Namespace == pool.Namespace && host.Spec.PoolRef.Name == pool.Name
}

// isHostSchedulableInPool reports whether the scheduler can place a new VM on
// member host of pool. It mirrors the placement path exactly
// (resolveClusterPlacement and the scheduler's health/cordon filter):
//   - status.health is Ready;
//   - spec.schedulable is true (not cordoned);
//   - the Host is not being deleted (placement treats it as cordoned);
//   - the Host is fronted by the pool's own Provider (spec.providerRef matches
//     the pool's), since placement only considers such hosts.
//
// Per-VM constraints (capacity, labels, placement policy) are not pool-level
// facts and are not part of it.
func isHostSchedulableInPool(pool *infravirtrigaudiov1beta1.HostPool, host *infravirtrigaudiov1beta1.Host) bool {
	return host.Status.Health == infravirtrigaudiov1beta1.HostHealthReady &&
		host.Spec.Schedulable &&
		host.DeletionTimestamp.IsZero() &&
		host.Spec.ProviderRef.Name == pool.Spec.ProviderRef.Name
}

// countPoolHosts tallies pool's member Hosts among hosts.
func countPoolHosts(pool *infravirtrigaudiov1beta1.HostPool, hosts []infravirtrigaudiov1beta1.Host) hostPoolCounts {
	var c hostPoolCounts
	for i := range hosts {
		h := &hosts[i]
		if !isHostPoolMember(pool, h) {
			continue
		}
		c.total++
		if h.Status.Health == infravirtrigaudiov1beta1.HostHealthReady {
			c.ready++
		}
		if isHostSchedulableInPool(pool, h) {
			c.schedulable++
		}
	}
	return c
}

// applyHostPoolStatus writes counts, the observed generation, and the Ready
// condition onto pool.Status. meta.SetStatusCondition only moves
// LastTransitionTime when the condition status flips, so applying the same
// counts twice leaves the status unchanged.
func applyHostPoolStatus(pool *infravirtrigaudiov1beta1.HostPool, c hostPoolCounts) {
	pool.Status.TotalHosts = c.total
	pool.Status.ReadyHosts = c.ready
	pool.Status.SchedulableHosts = c.schedulable
	pool.Status.ObservedGeneration = pool.Generation

	status, reason, msg := hostPoolReadiness(c)
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: pool.Generation,
	})
}

// hostPoolReadiness maps member-host counts onto the pool's Ready condition.
// The message names counts only — never a host endpoint or credential.
func hostPoolReadiness(c hostPoolCounts) (metav1.ConditionStatus, string, string) {
	switch {
	case c.total == 0:
		return metav1.ConditionFalse, reasonHostPoolNoMemberHosts,
			"no Host in this namespace names this pool in spec.poolRef"
	case c.schedulable == 0:
		return metav1.ConditionFalse, reasonHostPoolNoSchedulableHosts,
			fmt.Sprintf("0 of %d member host(s) are schedulable (%d Ready); a host is schedulable when it is Ready, "+
				"spec.schedulable is true, it is not being deleted and its spec.providerRef is this pool's Provider",
				c.total, c.ready)
	default:
		return metav1.ConditionTrue, reasonHostPoolHostsAvailable,
			fmt.Sprintf("%d of %d member host(s) are schedulable (%d Ready)", c.schedulable, c.total, c.ready)
	}
}

// hostPoolForHost maps a Host event to a reconcile request for the HostPool
// its spec.poolRef names, in the Host's own namespace. For an update the
// handler maps both the old and the new object, so a Host whose poolRef moved
// re-counts the pool it left as well as the pool it joined; a delete event
// re-counts the pool the Host was in.
func hostPoolForHost(_ context.Context, obj client.Object) []reconcile.Request {
	host, ok := obj.(*infravirtrigaudiov1beta1.Host)
	if !ok || host.Spec.PoolRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: host.Namespace, Name: host.Spec.PoolRef.Name},
	}}
}

// hostPoolMembershipChanged passes a Host update only when it can change a
// pool's counts: its poolRef, providerRef, schedulable flag, health, or
// deletion state. Every Host heartbeat (status.lastHeartbeatTime, capacity
// facts) is an update too; filtering those out keeps the pool controller idle
// while nothing it counts has moved. Create, delete and generic events always
// pass.
func hostPoolMembershipChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldHost, okOld := e.ObjectOld.(*infravirtrigaudiov1beta1.Host)
			newHost, okNew := e.ObjectNew.(*infravirtrigaudiov1beta1.Host)
			if !okOld || !okNew {
				return true
			}
			return oldHost.Spec.PoolRef != newHost.Spec.PoolRef ||
				oldHost.Spec.ProviderRef != newHost.Spec.ProviderRef ||
				oldHost.Spec.Schedulable != newHost.Spec.Schedulable ||
				oldHost.Status.Health != newHost.Status.Health ||
				oldHost.DeletionTimestamp.IsZero() != newHost.DeletionTimestamp.IsZero()
		},
	}
}

// SetupWithManager wires the HostPool controller. It reconciles a HostPool on
// create and on a spec (generation) change — its own status writes do not
// re-trigger it — and re-counts a pool whenever one of its member Hosts
// joins, leaves, or changes in a way that moves a count (see
// hostPoolMembershipChanged). No periodic requeue is needed: every input is
// a watched object.
func (r *HostPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.HostPool{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&infravirtrigaudiov1beta1.Host{},
			handler.EnqueueRequestsFromMapFunc(hostPoolForHost),
			builder.WithPredicates(hostPoolMembershipChanged()),
		).
		Named("hostpool").
		Complete(r)
}
