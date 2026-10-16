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
	stderrors "errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
)

// VMMigration status writes.
//
// A reconcile computes the migration's next status from the object it read.
// That read can be stale: the informer cache may not yet show a write the
// previous reconcile of the same object made (the rc1 lab run: reconcile A
// recorded the imported disk and moved to Creating; reconcile B, started from
// a cache that predated A's write, took the already-imported branch and its
// write replaced the live status, dropping status.diskInfo, so the Creating
// phase failed with "import did not record disk info").
//
// Two rules keep a status write from undoing another one:
//
//   - updateStatus writes the whole status with the resourceVersion this
//     reconcile read (optimistic concurrency). A conflict is never retried
//     onto a re-read object: the status this reconcile computed may rest on
//     the very fields the other writer changed. The conflict is returned and
//     Reconcile requeues (migrationStatusConflictRequeue), so the next
//     reconcile recomputes from the live state.
//
//   - recordMigrationResult persists what a non-idempotent provider RPC
//     (SnapshotCreate, ExportDisk, ImportDisk) returned. Requeueing would
//     lose it (and the in-memory guard then keeps the RPC from being sent
//     again), so on a conflict it re-reads the VMMigration from the API
//     server and applies ONLY the RPC's result fields (and the phase
//     transition it completes) to the live object, as long as the live phase
//     is still the one the RPC was sent in. A result that still cannot be
//     written is kept in memory (pendingMigrationResult) and applied by the
//     next reconcile of that phase.

// migrationStatusConflictRequeue is when a reconcile whose status write lost
// to a concurrent write (or found the live status moved on) runs again. The
// other write's own watch event usually re-drives the migration sooner; this
// is the bound, and it is never an immediate requeue.
const migrationStatusConflictRequeue = 2 * time.Second

// errMigrationStatusConflict marks a status write of this controller that
// lost to a concurrent write of the VMMigration (an API Conflict).
var errMigrationStatusConflict = stderrors.New("the VMMigration changed since this reconcile read it")

// errMigrationStatusMovedOn means the live VMMigration is no longer in the
// phase a provider RPC's result belongs to: another write already moved it
// on, so the result is not applied over it.
var errMigrationStatusMovedOn = stderrors.New("the VMMigration's live status is no longer in the phase this result belongs to")

// errMigrationRecreated means the VMMigration was deleted and re-created
// (another UID) since this reconcile read it: nothing decided for the old
// object is written to the new one.
var errMigrationRecreated = stderrors.New("the VMMigration was re-created since it was read")

// isMigrationStatusContention reports whether err is a status write of this
// controller that lost to a concurrent write (errMigrationStatusConflict) or
// found the live status moved on (errMigrationStatusMovedOn). Such a
// reconcile is requeued after migrationStatusConflictRequeue rather than
// reported as an error. Other conflicts (a metadata update) are unchanged.
func isMigrationStatusContention(err error) bool {
	return stderrors.Is(err, errMigrationStatusConflict) || stderrors.Is(err, errMigrationStatusMovedOn)
}

// wrapMigrationStatusConflict marks err as errMigrationStatusConflict when it
// is an API Conflict, keeping err in the chain.
func wrapMigrationStatusConflict(err error) error {
	if apierrors.IsConflict(err) {
		return fmt.Errorf("%w: %w", errMigrationStatusConflict, err)
	}
	return err
}

// updateStatus writes migration's status under the resourceVersion this
// reconcile read (or last wrote). It never retries a conflict onto a re-read
// object, so it can never replace a status another reconcile wrote with one
// computed from an older read: a conflict is returned (Reconcile turns it into
// a requeue after migrationStatusConflictRequeue) and the next reconcile
// starts from the live state. A VMMigration deleted and re-created meanwhile
// has another resourceVersion, so it is never written either.
//
// The result of a non-idempotent provider RPC is written with
// recordMigrationResult instead.
func (r *VMMigrationReconciler) updateStatus(ctx context.Context, migration *infrav1beta1.VMMigration) error {
	if err := r.Status().Update(ctx, migration); err != nil {
		logger := logging.FromContext(ctx)
		if apierrors.IsConflict(err) {
			logger.Info("VMMigration changed since this reconcile read it; not overwriting its status, requeueing")
		} else {
			logger.Error(err, "Failed to update VMMigration status")
		}
		return fmt.Errorf("update status of VMMigration %s/%s: %w", migration.Namespace, migration.Name, wrapMigrationStatusConflict(err))
	}
	return nil
}

// recordMigrationResult applies the result of a non-idempotent provider RPC
// sent in phase (apply) to migration and persists it. apply must set only the
// fields that record the RPC's result and the transition it completes, and be
// idempotent: it is applied to migration, and on a conflict to the VMMigration
// re-read from the API server.
//
// The first attempt writes migration as this reconcile holds it, under the
// resourceVersion it read. On a conflict the live object is re-read and, when
// it is the same VMMigration (UID) still in phase, apply is applied to it and
// written (retry.RetryOnConflict); every other field keeps the live value.
// Live status in another phase is errMigrationStatusMovedOn and a re-created
// VMMigration errMigrationRecreated: nothing is written over either. On
// success migration holds the object as written.
func (r *VMMigrationReconciler) recordMigrationResult(
	ctx context.Context,
	migration *infrav1beta1.VMMigration,
	phase infrav1beta1.MigrationPhase,
	apply func(*infrav1beta1.VMMigration),
) error {
	logger := logging.FromContext(ctx)
	apply(migration)
	written := migration
	attempt := 0
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		attempt++
		if attempt > 1 {
			live := &infrav1beta1.VMMigration{}
			if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(migration), live); err != nil {
				return fmt.Errorf("re-read VMMigration %s/%s to record a provider result: %w", migration.Namespace, migration.Name, err)
			}
			if live.UID != migration.UID {
				return fmt.Errorf("VMMigration %s/%s (uid %s, was %s): %w",
					migration.Namespace, migration.Name, live.UID, migration.UID, errMigrationRecreated)
			}
			if live.Status.Phase != phase {
				return fmt.Errorf("VMMigration %s/%s is in phase %s, not %s: %w",
					migration.Namespace, migration.Name, live.Status.Phase, phase, errMigrationStatusMovedOn)
			}
			logger.Info("VMMigration changed while a provider operation ran; recording its result on the live object",
				"attempt", attempt, "phase", string(phase))
			apply(live)
			written = live
		}
		return r.Status().Update(ctx, written)
	})
	if err != nil {
		return fmt.Errorf("record provider result on VMMigration %s/%s: %w", migration.Namespace, migration.Name, wrapMigrationStatusConflict(err))
	}
	if written != migration {
		written.DeepCopyInto(migration)
	}
	return nil
}

// pendingMigrationResult is the result of a long-running provider RPC
// (ExportDisk / ImportDisk) that could not be persisted. It is kept in
// longOpInFlight in place of the in-flight sentinel, so the in-memory guard
// still keeps the RPC from being sent again, and the next reconcile of the
// phase applies it (applyPendingMigrationResult) instead of advancing without
// it.
type pendingMigrationResult struct {
	apply func(*infrav1beta1.VMMigration)
}

// recordLongOpResult persists the result of the long-running RPC op with
// recordMigrationResult. When it cannot be written for any reason other than
// the live status having moved on or the VMMigration having been re-created,
// the result (resultOnly: its fields without the phase transition) is kept as
// a pendingMigrationResult for the next reconcile of the phase.
func (r *VMMigrationReconciler) recordLongOpResult(
	ctx context.Context,
	migration *infrav1beta1.VMMigration,
	op migrationLongOp,
	phase infrav1beta1.MigrationPhase,
	resultOnly, apply func(*infrav1beta1.VMMigration),
) error {
	key := longOpKey(migration, op)
	err := r.recordMigrationResult(ctx, migration, phase, apply)
	if err != nil && !stderrors.Is(err, errMigrationStatusMovedOn) && !stderrors.Is(err, errMigrationRecreated) {
		logging.FromContext(ctx).Error(err, "Could not record the provider's result; it is kept for the next reconcile",
			"operation", string(op))
		r.longOpInFlight.Store(key, pendingMigrationResult{apply: resultOnly})
	}
	return err
}

// applyPendingMigrationResult applies to migration the result of op that an
// earlier reconcile could not persist, if there is one, and reports whether
// it did. The caller writes it; clearPendingMigrationResult then drops it.
func (r *VMMigrationReconciler) applyPendingMigrationResult(migration *infrav1beta1.VMMigration, op migrationLongOp) bool {
	v, ok := r.longOpInFlight.Load(longOpKey(migration, op))
	if !ok {
		return false
	}
	pending, ok := v.(pendingMigrationResult)
	if !ok {
		return false
	}
	pending.apply(migration)
	return true
}

// clearPendingMigrationResult drops a persisted pending result of op, keeping
// the in-flight sentinel (the RPC was sent for this generation).
func (r *VMMigrationReconciler) clearPendingMigrationResult(migration *infrav1beta1.VMMigration, op migrationLongOp) {
	r.longOpInFlight.Store(longOpKey(migration, op), struct{}{})
}
