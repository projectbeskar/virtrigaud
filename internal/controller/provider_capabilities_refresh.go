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
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// When the Provider controller (re-)reads a provider's capabilities into
// status.reportedCapabilities (v0.4.1-rc1 lab finding B2).
//
// A capability read is only taken once no pod of a superseded ReplicaSet is
// left running beside the current ones (deploymentRolloutComplete):
// mid-rollout, any connection can reach a pod of the superseded ReplicaSet. It
// is taken over a connection of its own
// (ProviderCapabilityReader), never the cached client of the other
// controllers, whose connection stays with the pod it first reached. It is
// taken again:
//   - when the runtime revision changes (a new Deployment, or a Deployment
//     spec change: image, env, mounts, replicas) — the reconcile that sees the
//     rollout complete reads at once;
//   - once more, capabilitySettleDelay later (the old pods' termination grace
//     period plus a margin): at the moment a rollout completes, an old pod can
//     still be terminating, and the Service can route a new connection to it
//     until the endpoint update has propagated. The second read is answered by
//     the new runtime only;
//   - every capabilityRefreshInterval while the Provider is healthy, which
//     also catches a change no Deployment spec change shows (a mutable image
//     tag pulled again by a restarted pod);
//   - after a failed read, capabilityRetryInterval later — not sooner, whatever
//     else re-drives the Provider (capabilityReadFailures).
//
// What was read, and for which revision, is kept in memory (per manager
// process): a manager that starts reads every Provider again, which is also
// when its client cache is empty.
const (
	// defaultCapabilityRefreshInterval is how often a healthy Provider's
	// capabilities are read again when nothing else triggers a read.
	defaultCapabilityRefreshInterval = 10 * time.Minute

	// capabilityRetryInterval is when a failed capability read is retried.
	capabilityRetryInterval = time.Minute

	// capabilityReadTimeout bounds one capability read (it is retried
	// anyway), so an unreachable runtime does not hold a reconcile worker for
	// the transport's whole GetCapabilities timeout.
	capabilityReadTimeout = 10 * time.Second

	// capabilitySettleMargin is added to the pod termination grace period to
	// get the delay of the confirming read after a rollout.
	capabilitySettleMargin = 15 * time.Second

	// defaultPodTerminationGracePeriod is the Kubernetes default, used when
	// the Deployment's pod template does not set one.
	defaultPodTerminationGracePeriod = 30 * time.Second
)

// ProviderCapabilityReader reads the self-reported capabilities of a
// Provider's runtime (GetCapabilities) over a connection opened for the read.
// remote.Resolver implements it.
type ProviderCapabilityReader interface {
	ReadCapabilities(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Capabilities, error)
}

// capabilityReadState is what the Provider controller last read into a
// Provider's status.reportedCapabilities.
type capabilityReadState struct {
	// uid is the Provider's UID (a Provider re-created under the same name
	// starts over).
	uid types.UID
	// revision is the runtime revision (runtimeRevision) the read was taken at.
	revision string
	// readAt is when the read was taken.
	readAt time.Time
	// settled is true once a read was taken at least the settle delay after
	// the first read of this revision.
	settled bool
}

// capabilityReadFailure is the last failed capability read of a Provider: no
// read of the same Provider and runtime revision is attempted again before
// capabilityRetryInterval has passed.
type capabilityReadFailure struct {
	uid      types.UID
	revision string
	at       time.Time
}

// deploymentRolloutComplete reports whether every pod the Service can route a
// new connection to is of the Deployment's current revision: the controller
// has observed the latest spec, no (non-terminating) replica of an older
// ReplicaSet is left (status.replicas counts the replicas of every
// ReplicaSet, status.updatedReplicas those of the current one), and at least
// one current replica is available. Pods still terminating are not counted;
// the confirming read after the settle delay covers them. Unlike `kubectl
// rollout status` it does not wait for every desired replica: a Provider with
// one of its replicas unavailable (unschedulable, crash-looping) is still read
// from the current ones that serve.
func deploymentRolloutComplete(dep *appsv1.Deployment) bool {
	if dep == nil || dep.Generation == 0 || dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	s := dep.Status
	return s.UpdatedReplicas > 0 &&
		s.Replicas <= s.UpdatedReplicas &&
		s.AvailableReplicas > 0
}

// runtimeRevision identifies the revision of a Provider's runtime: the
// Deployment's UID and generation. The generation changes with every
// Deployment spec change (a new image among them), the UID when the
// Deployment is re-created.
func runtimeRevision(dep *appsv1.Deployment) string {
	return string(dep.UID) + "/" + strconv.FormatInt(dep.Generation, 10)
}

// capabilitySettleDelayFor is the delay of the confirming read after a
// rollout: the pods' termination grace period plus capabilitySettleMargin. An
// old pod has exited by then.
func (r *ProviderReconciler) capabilitySettleDelayFor(dep *appsv1.Deployment) time.Duration {
	if r.capabilitySettleDelay > 0 {
		return r.capabilitySettleDelay
	}
	grace := defaultPodTerminationGracePeriod
	if g := dep.Spec.Template.Spec.TerminationGracePeriodSeconds; g != nil && *g >= 0 {
		grace = time.Duration(*g) * time.Second
	}
	return grace + capabilitySettleMargin
}

// capabilityRefreshEvery is the periodic re-read interval.
func (r *ProviderReconciler) capabilityRefreshEvery() time.Duration {
	if r.capabilityRefreshInterval > 0 {
		return r.capabilityRefreshInterval
	}
	return defaultCapabilityRefreshInterval
}

// capabilityReadDue reports whether a capability read is due for a runtime at
// revision rev, given the last read st (nil: none), and otherwise how long
// until the next one is.
func capabilityReadDue(st *capabilityReadState, uid types.UID, rev string, now time.Time,
	settle, refresh time.Duration) (bool, time.Duration) {
	if st == nil || st.uid != uid || st.revision != rev {
		return true, 0
	}
	elapsed := now.Sub(st.readAt)
	if !st.settled {
		if elapsed >= settle {
			return true, 0
		}
		return false, settle - elapsed
	}
	if elapsed >= refresh {
		return true, 0
	}
	return false, refresh - elapsed
}

// refreshReportedCapabilities reads the Provider's capabilities into its
// status when a read is due (see the policy above). It returns when the
// Provider should be reconciled again for the next read (0: no capability
// requeue; a rollout in progress is re-driven by its Deployment's status
// events) and, when it read them, the read to record once the status holding
// them is written (commitCapabilityRead): a read whose status write fails is
// not recorded, so the next reconcile reads again. It never fails the
// reconcile; a failed read keeps what the status holds
// (reconcileReportedCapabilities). The caller calls it only for a healthy
// Provider whose runtime is Running.
func (r *ProviderReconciler) refreshReportedCapabilities(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	dep *appsv1.Deployment) (time.Duration, *capabilityReadState) {
	if !deploymentRolloutComplete(dep) {
		log.FromContext(ctx).V(1).Info("Provider runtime rollout not complete; not reading its capabilities yet",
			"provider", provider.Name, "namespace", provider.Namespace)
		return 0, nil
	}
	var st *capabilityReadState
	if v, ok := r.capabilityReads.Load(capabilityReadKey(provider)); ok {
		st, _ = v.(*capabilityReadState)
	}
	rev := runtimeRevision(dep)
	settle := r.capabilitySettleDelayFor(dep)
	refresh := r.capabilityRefreshEvery()
	now := time.Now()
	due, wait := capabilityReadDue(st, provider.UID, rev, now, settle, refresh)
	if !due {
		return wait, nil
	}
	key := capabilityReadKey(provider)
	if v, ok := r.capabilityReadFailures.Load(key); ok {
		if f, _ := v.(*capabilityReadFailure); f != nil && f.uid == provider.UID && f.revision == rev {
			if since := now.Sub(f.at); since < capabilityRetryInterval {
				return capabilityRetryInterval - since, nil
			}
		}
	}
	if !r.reconcileReportedCapabilities(ctx, provider) {
		r.capabilityReadFailures.Store(key, &capabilityReadFailure{uid: provider.UID, revision: rev, at: now})
		return capabilityRetryInterval, nil
	}
	next := &capabilityReadState{
		uid:      provider.UID,
		revision: rev,
		readAt:   now,
		// A read of a revision already read at least settle ago (the
		// confirming read, or a periodic one) is settled; the first read of
		// a revision is not.
		settled: st != nil && st.uid == provider.UID && st.revision == rev,
	}
	if !next.settled {
		return settle, next
	}
	return refresh, next
}

// capabilityReadKey is the capabilityReads key of a Provider.
func capabilityReadKey(provider *infravirtrigaudiov1beta1.Provider) string {
	return types.NamespacedName{Namespace: provider.Namespace, Name: provider.Name}.String()
}

// commitCapabilityRead records read (from refreshReportedCapabilities) once
// the Provider status holding its capabilities is written. A nil read records
// nothing.
func (r *ProviderReconciler) commitCapabilityRead(provider *infravirtrigaudiov1beta1.Provider, read *capabilityReadState) {
	if read != nil {
		key := capabilityReadKey(provider)
		r.capabilityReads.Store(key, read)
		r.capabilityReadFailures.Delete(key)
	}
}

// forgetCapabilityReads drops what was recorded for a Provider that is gone.
func (r *ProviderReconciler) forgetCapabilityReads(key types.NamespacedName) {
	r.capabilityReads.Delete(key.String())
	r.capabilityReadFailures.Delete(key.String())
}

// requeueNoLaterThan returns result with RequeueAfter at most after (after > 0).
// An immediate requeue is kept.
func requeueNoLaterThan(result ctrl.Result, after time.Duration) ctrl.Result {
	if after <= 0 || (result.Requeue && result.RequeueAfter == 0) {
		return result
	}
	if result.RequeueAfter == 0 || after < result.RequeueAfter {
		result.RequeueAfter = after
	}
	return result
}
