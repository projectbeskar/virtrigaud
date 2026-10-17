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
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// The binding between a VMMigration and the target VirtualMachine it created
// (security review M1).
//
// A migration creates its target VirtualMachine under a name the user chose
// (spec.target.name), possibly in another namespace. Anyone who may create
// VirtualMachines there can create one under that name first, and every
// field a creator can write — the virtrigaud.io/migration provenance
// annotation, spec.importedDisk naming this migration and its status.importID
// — can be copied onto it. None of them proves that the migration created the
// object. Only the API server's answer to the migration's OWN Create does:
// the UID it returns. The migration records it (status.targetVMUID) and from
// then on acts only on the VirtualMachine with that UID.
//
// The one window that needs more is a Create that succeeded whose UID was
// never recorded (the status write lost a conflict, or the manager restarted
// between the two). For it the migration writes a claim BEFORE the Create:
// a fresh random token goes into the virtrigaud.io/migration-claim
// annotation of the VirtualMachine it creates, and only the token's SHA-256
// digest into status.targetVMClaim, with an optimistic-lock status write that
// must succeed before the Create is sent. A VirtualMachine found later is the
// migration's when its claim token hashes to the recorded digest. Someone
// who can read the VMMigration sees only the digest; the token is on the
// migration's own VirtualMachine only, so it cannot be put on another one
// before the migration's exists. (Residual: whoever can read that
// VirtualMachine, delete it and create another under its name, all before
// the UID is recorded, could copy the token. The admission policy that
// reserves the virtrigaud.io/migration-claim annotation for the manager,
// docs/admission-policy.md, closes that too.)
//
// A VirtualMachine under the target's name that is neither is someone
// else's: the migration fails with ReasonTargetVMNotOwned and never reads its
// status into its own, marks it migration-completed or deletes it. The check
// runs at Validating, before the source is powered off or anything is
// exported, and again in every phase that touches the target.

const (
	// migrationTargetClaimAnnotation carries, on the target VirtualMachine a
	// migration creates, the claim token whose SHA-256 digest the migration
	// recorded in status.targetVMClaim before the Create.
	migrationTargetClaimAnnotation = "virtrigaud.io/migration-claim"

	// migrationTargetClaimTokenBytes is the size of a claim token's random
	// part (hex-encoded in the annotation).
	migrationTargetClaimTokenBytes = 32

	// ReasonTargetVMNotOwned is the VMMigration condition reason when a
	// VirtualMachine under the target's name exists that this migration did
	// not create. The migration fails; that VirtualMachine is not touched.
	ReasonTargetVMNotOwned = "TargetVMNotOwned"

	// ReasonTargetVMGone is the VMMigration condition reason when the target
	// VirtualMachine this migration created no longer exists while it is
	// being validated.
	ReasonTargetVMGone = "TargetVMGone"

	// targetVMNotOwnedMessage is the constant message of a
	// ReasonTargetVMNotOwned failure. It names no object and no value of
	// the other VirtualMachine.
	targetVMNotOwnedMessage = "a VirtualMachine with the target's name (spec.target.name in the target namespace) " +
		"already exists and was not created by this migration; nothing was done to it. Delete or rename that " +
		"VirtualMachine, or choose another spec.target.name, and run the migration again"

	// targetVMGoneMessage is the constant message of a ReasonTargetVMGone
	// failure.
	targetVMGoneMessage = "the target VirtualMachine this migration created no longer exists"

	// targetVMRecordedMessage is the migration's status message once the
	// target VirtualMachine's UID is recorded and it is still provisioning.
	targetVMRecordedMessage = "Target VM created by this migration, waiting for provisioning"

	// targetVMWaitInterval re-checks a target VirtualMachine that is being
	// provisioned (a provider Create of a VM from an imported disk).
	targetVMWaitInterval = 30 * time.Second
)

// targetVMState classifies the VirtualMachine under a migration's target key.
type targetVMState int

const (
	// targetVMAbsent: no VirtualMachine under the target key.
	targetVMAbsent targetVMState = iota
	// targetVMOwned: the VirtualMachine this migration created
	// (migrationOwnsTargetVM).
	targetVMOwned
	// targetVMNotOwned: a VirtualMachine this migration did not create,
	// confirmed from the API server against the live VMMigration.
	targetVMNotOwned
	// targetVMUndecided: this reconcile's VMMigration is older than the live
	// one (a claim or UID it wrote may not be visible yet); retry.
	targetVMUndecided
)

// newMigrationTargetClaim returns a fresh claim token for the target
// VirtualMachine's migrationTargetClaimAnnotation and its digest for
// status.targetVMClaim.
func newMigrationTargetClaim() (token, digest string, err error) {
	b := make([]byte, migrationTargetClaimTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate the target VM claim token: %w", err)
	}
	token = hex.EncodeToString(b)
	return token, migrationTargetClaimDigest(token), nil
}

// migrationTargetClaimDigest is the hex SHA-256 digest of a claim token.
func migrationTargetClaimDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// migrationOwnsTargetVM reports whether vm is the target VirtualMachine
// migration created: its UID is status.targetVMUID or, while no UID is
// recorded, its claim token hashes to status.targetVMClaim. Once a UID is
// recorded only that UID counts. Provenance annotations and
// spec.importedDisk are never consulted: whoever creates a VirtualMachine
// can write them.
func migrationOwnsTargetVM(migration *infrav1beta1.VMMigration, vm *infrav1beta1.VirtualMachine) bool {
	if migration == nil || vm == nil || vm.UID == "" {
		return false
	}
	if uid := migration.Status.TargetVMUID; uid != "" {
		return string(vm.UID) == uid
	}
	token := vm.Annotations[migrationTargetClaimAnnotation]
	if token == "" || migration.Status.TargetVMClaim == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(migrationTargetClaimDigest(token)), []byte(migration.Status.TargetVMClaim)) == 1
}

// bindTargetVMUID records vm's UID as the migration's target
// (status.targetVMUID) when none is recorded yet. vm must be one
// migrationOwnsTargetVM accepted. It reports whether the status changed.
func bindTargetVMUID(migration *infrav1beta1.VMMigration, vm *infrav1beta1.VirtualMachine) bool {
	if migration.Status.TargetVMUID != "" {
		return false
	}
	migration.Status.TargetVMUID = string(vm.UID)
	return true
}

// classifyTargetVM reads the VirtualMachine under the migration's target key
// and classifies it. The informer cache answers when it shows the
// migration's own VirtualMachine; anything else is decided from the API
// server, and a VirtualMachine is reported as not owned only when this
// reconcile's VMMigration is the live one (otherwise targetVMUndecided), so
// neither a lagging VirtualMachine cache nor a lagging VMMigration cache can
// fail a migration over its own target. liveIfAbsent re-reads a VirtualMachine
// the cache does not show; without it a cache miss is reported as absent
// (a best-effort early check that a later phase repeats live). A read error
// is returned.
func (r *VMMigrationReconciler) classifyTargetVM(
	ctx context.Context,
	migration *infrav1beta1.VMMigration,
	liveIfAbsent bool,
) (targetVMState, *infrav1beta1.VirtualMachine, error) {
	key := migrationTargetVMKey(migration)
	cached := &infrav1beta1.VirtualMachine{}
	err := r.Get(ctx, key, cached)
	switch {
	case err == nil:
		if migrationOwnsTargetVM(migration, cached) {
			return targetVMOwned, cached, nil
		}
	case !errors.IsNotFound(err):
		return targetVMAbsent, nil, fmt.Errorf("get target VirtualMachine %s: %w", key, err)
	case !liveIfAbsent:
		return targetVMAbsent, nil, nil
	}

	live := &infrav1beta1.VirtualMachine{}
	if err := r.liveReader().Get(ctx, key, live); err != nil {
		if errors.IsNotFound(err) {
			return targetVMAbsent, nil, nil
		}
		return targetVMAbsent, nil, fmt.Errorf("re-read target VirtualMachine %s: %w", key, err)
	}
	if migrationOwnsTargetVM(migration, live) {
		return targetVMOwned, live, nil
	}
	stale, err := r.migrationCacheIsStale(ctx, migration)
	if err != nil {
		return targetVMAbsent, nil, err
	}
	if stale {
		return targetVMUndecided, nil, nil
	}
	return targetVMNotOwned, live, nil
}

// claimedByAnotherCreate reports whether vm carries a migration claim token
// (migrationTargetClaimAnnotation) at all. With the reserved-keys admission
// policy on, only a manager can have written it, so a VM that carries one
// but is not this migration's (migrationOwnsTargetVM false) is most likely
// the Create of an overlapping leader. Without the policy a tenant can plant
// the annotation too; it then only delays the failure by one requeue.
func claimedByAnotherCreate(vm *infrav1beta1.VirtualMachine) bool {
	return vm != nil && vm.Annotations[migrationTargetClaimAnnotation] != ""
}

// liveOwnedTargetVM re-reads the VirtualMachine under the migration's target
// key from the API server. nil (no error) means it is gone: absent, or being
// deleted. The caller checks ownership (migrationOwnsTargetVM) of a VM it
// returns. A read error is returned.
func (r *VMMigrationReconciler) liveOwnedTargetVM(ctx context.Context,
	migration *infrav1beta1.VMMigration) (*infrav1beta1.VirtualMachine, error) {
	key := migrationTargetVMKey(migration)
	vm := &infrav1beta1.VirtualMachine{}
	if err := r.liveReader().Get(ctx, key, vm); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("re-read target VirtualMachine %s: %w", key, err)
	}
	if !vm.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	return vm, nil
}

// releaseGoneTargetBinding clears status.targetVMUID and status.targetVMClaim
// (in memory; the caller writes the status) when the VirtualMachine they
// bind is no longer under the target key — absent, or another object — as
// read from the API server. A bound VM that still exists keeps its binding.
// Without the target namespace grant nothing is read there and the binding
// is kept (the next phase refuses anyway). A read error is returned.
func (r *VMMigrationReconciler) releaseGoneTargetBinding(ctx context.Context, migration *infrav1beta1.VMMigration) error {
	if migration.Status.TargetVMUID == "" && migration.Status.TargetVMClaim == "" {
		return nil
	}
	key := migrationTargetVMKey(migration)
	allowed, err := targetNamespaceAllowed(ctx, r.liveReader(), migration.Namespace, key.Namespace)
	if err != nil {
		return fmt.Errorf("check the target namespace grant before releasing the target VM binding: %w", err)
	}
	if !allowed {
		return nil
	}
	vm := &infrav1beta1.VirtualMachine{}
	switch err := r.liveReader().Get(ctx, key, vm); {
	case errors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("re-read target VirtualMachine %s before releasing its binding: %w", key, err)
	case migrationOwnsTargetVM(migration, vm):
		return nil
	}
	logging.FromContext(ctx).Info("Releasing the binding to a target VM that no longer exists", "target", key.String())
	migration.Status.TargetVMUID = ""
	migration.Status.TargetVMClaim = ""
	return nil
}

// failTargetVMNotOwned fails migration because a VirtualMachine under its
// target's name was not created by it: ReasonTargetVMNotOwned with the
// constant message (and Validating=False when it is still validating). That
// VirtualMachine is not read further, changed or deleted.
func (r *VMMigrationReconciler) failTargetVMNotOwned(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logging.FromContext(ctx).Info("A VirtualMachine under the migration's target name was not created by this migration; failing without touching it",
		"target", migrationTargetVMKey(migration).String(), "phase", migration.Status.Phase)
	if migration.Status.Phase == infrav1beta1.MigrationPhaseValidating {
		k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionValidating,
			metav1.ConditionFalse, ReasonTargetVMNotOwned, targetVMNotOwnedMessage, migration.Generation)
	}
	return r.transitionToFailedWithReason(ctx, migration, ReasonTargetVMNotOwned, targetVMNotOwnedMessage)
}

// reservedTargetLabels are the VirtualMachine labels only the manager sets
// (the reserved-keys admission policy reserves exactly these).
var reservedTargetLabels = []string{AdoptedLabel}

// eventReasonTargetLabelsDropped is the Normal event reason recorded when
// reserved labels in spec.target.labels were not copied onto the target VM.
const eventReasonTargetLabelsDropped = "TargetLabelsDropped"

// migrationTargetLabels copies spec.target.labels for the target VM without
// the reserved labels (reservedTargetLabels), such as virtrigaud.io/adopted,
// which would make the VirtualMachine controller wait for an adoption
// instead of creating the VM and could let adoption bind a hypervisor VM to
// it. The manager creates the VM, so the reserved-keys admission policy
// would not stop a label copied through it. Every other label, in any
// domain, is kept. It returns the labels (nil when none remain) and the
// dropped keys, sorted.
func migrationTargetLabels(user map[string]string) (map[string]string, []string) {
	var out map[string]string
	var dropped []string
	for k, v := range user {
		if slices.Contains(reservedTargetLabels, k) {
			dropped = append(dropped, k)
			continue
		}
		if out == nil {
			out = make(map[string]string, len(user))
		}
		out[k] = v
	}
	slices.Sort(dropped)
	return out, dropped
}

// targetLabelsDroppedMessage is the eventReasonTargetLabelsDropped message:
// the dropped keys (from the migration's own spec, so nothing leaks).
func targetLabelsDroppedMessage(dropped []string) string {
	return "spec.target.labels not copied to the target VM (reserved for the VirtRigaud manager): " +
		strings.Join(dropped, ", ")
}
