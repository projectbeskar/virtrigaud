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

package opstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Operation-store crash points, in addition to the journal's (a commit's
// PointAfterStoreCommit and PointAfterAnchor). Tests pass a crash hook in
// Options.Journal.Hooks.
const (
	// PointBeforeAccept: admission decided to accept; nothing is written.
	PointBeforeAccept journal.Point = "before-accept"
	// PointAfterIntent: a step intent is durable; no witness, no effect.
	PointAfterIntent journal.Point = "after-intent"
	// PointAfterWitness: a once step's witness is durable; no effect.
	PointAfterWitness journal.Point = "after-witness"
	// PointAfterEffect: the executor performed the step; done is not durable.
	PointAfterEffect journal.Point = "after-effect"
	// PointAfterLastDone: every step is done; the terminal record is not
	// written.
	PointAfterLastDone journal.Point = "after-last-done"
	// PointAfterTerminal: the terminal transaction is durable and anchored;
	// the caller has not answered.
	PointAfterTerminal journal.Point = "after-terminal"
	// PointMidCompaction: one compaction batch is durable, more remain.
	PointMidCompaction journal.Point = "mid-compaction"
	// PointMidReenrolment: the predecessor enrolment is frozen; the new one
	// is not active.
	PointMidReenrolment journal.Point = "mid-reenrolment"
)

// DefaultMaxStoreBytes is the default store size above which new operations
// are refused (RefusalStoreFull). Terminal records are always written.
const DefaultMaxStoreBytes = 256 << 20

// DefaultStepDeadline is the default time a step may run before it is
// resolved by inspection.
const DefaultStepDeadline = 10 * time.Minute

// Options configures Open.
type Options struct {
	// Journal configures the journal. Buckets is set by the operation store.
	Journal journal.Options
	// Executor performs the steps. Required.
	Executor executor.Executor
	// StepDeadlines bounds each step's run per kind. A missing kind uses
	// DefaultStepDeadline.
	StepDeadlines map[providerv1.OperationKind]time.Duration
	// MaxStoreBytes is the store size above which new operations are
	// refused. Zero uses DefaultMaxStoreBytes.
	MaxStoreBytes int64
	// Audit receives every admission decision and state change, including
	// refusals that write nothing, for example to write a structured
	// system-journal line. It never receives request bytes. Nil discards.
	Audit func(AuditEvent)
}

// AuditEvent describes one decision or state change. It holds identity fields
// only.
type AuditEvent struct {
	// Event names what happened: "accepted", "known", "refused", "step",
	// "terminal", "outcome_unknown", "frozen", "quarantined".
	Event string
	// Ref names the operation, if any.
	Ref OpRef
	// VMUID is the VM, if any.
	VMUID string
	// Kind is the operation kind, if any.
	Kind providerv1.OperationKind
	// Actor is the caller named in the request, if any.
	Actor string
	// Reason qualifies the event: a refusal reason or a state reason.
	Reason string
	// State is the operation's state after the event.
	State OpState
}

// Errors returned by the store.
var (
	// ErrNotReady is returned before Recover has run.
	ErrNotReady = errors.New("operation store not ready: recovery has not run")
	// ErrNoEnrolment is returned when no enrolment is active.
	ErrNoEnrolment = errors.New("operation store has no active enrolment")
	// ErrUnknownOperation is returned for an operation the store has no
	// record of.
	ErrUnknownOperation = errors.New("operation unknown to the store")
	// ErrRunning is returned when an operation already has a runner.
	ErrRunning = errors.New("operation already has a runner")
)

// Store is the per-host agent's durable operation store. See the package
// documentation.
type Store struct {
	j     *journal.Store
	exec  executor.Executor
	opts  Options
	fsys  journal.FS
	audit func(AuditEvent)

	// mu serialises every read-decide-write sequence: admissions, step
	// transitions, enrolment changes and compaction batches.
	mu      sync.Mutex
	ready   bool
	running map[string]bool
	dirs    map[string]bool
}

// Open opens the journal and the store. The store starts not ready: call
// EstablishContinuity, then Recover, before admitting operations. A store
// whose journal is lost is returned too; it serves reads and refuses every
// mutation.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Executor == nil {
		return nil, errors.New("open operation store: no executor")
	}
	if opts.MaxStoreBytes == 0 {
		opts.MaxStoreBytes = DefaultMaxStoreBytes
	}
	jo := opts.Journal
	jo.Buckets = append(append([]string(nil), jo.Buckets...), buckets()...)
	j, err := journal.Open(ctx, jo)
	if err != nil {
		return nil, fmt.Errorf("open operation store: %w", err)
	}
	s := &Store{j: j, exec: opts.Executor, opts: opts, fsys: j.FS(), audit: opts.Audit,
		running: map[string]bool{}, dirs: map[string]bool{}}
	return s, nil
}

// Journal returns the underlying journal.
func (s *Store) Journal() *journal.Store { return s.j }

// Close closes the store. It writes nothing.
func (s *Store) Close() error { return s.j.Close() }

// EstablishContinuity establishes the journal's continuity (see
// journal.Store.EstablishContinuity). With the local store alone this is the
// set of checks Open made.
func (s *Store) EstablishContinuity(ctx context.Context) error {
	if err := s.j.EstablishContinuity(ctx); err != nil {
		return fmt.Errorf("operation store: %w", err)
	}
	return nil
}

func (s *Store) emit(ev AuditEvent) {
	if s.audit != nil {
		s.audit(ev)
	}
}

// auditKey is be64(seq) || be16(index).
func auditKey(seq uint64, i int) []byte {
	k := make([]byte, 10)
	binary.BigEndian.PutUint64(k, seq)
	binary.BigEndian.PutUint16(k[8:], uint16(i)) // #nosec G115 -- a transaction writes a handful of audit events
	return k
}

// auditTx writes ev into the audit bucket in tx and remembers it for the
// sink, which is told only after the commit.
type auditBuf struct {
	n      int
	events []AuditEvent
}

func (a *auditBuf) write(tx *journal.Tx, ev AuditEvent) error {
	rec := &pb.AuditEvent{
		Event:       ev.Event,
		OpId:        ev.Ref.OpID,
		EnrolmentId: ev.Ref.EnrolmentID,
		VmUid:       ev.VMUID,
		Kind:        int32(ev.Kind),
		Actor:       ev.Actor,
		Reason:      ev.Reason,
		State:       ev.State,
	}
	if err := put(tx, bucketAudit, auditKey(tx.Seq(), a.n), rec); err != nil {
		return err
	}
	a.n++
	a.events = append(a.events, ev)
	return nil
}

func (s *Store) flush(a *auditBuf) {
	for _, ev := range a.events {
		s.emit(ev)
	}
}

// Enrolment describes one enrolment of the host.
type Enrolment struct {
	// ID is the 16-byte enrolment identifier.
	ID []byte
	// ProviderUID is the Provider metadata.uid.
	ProviderUID string
	// InstallID is the installation identifier.
	InstallID []byte
	// StartEpoch is the epoch counter the enrolment starts at. It must be
	// greater than every earlier enrolment's.
	StartEpoch uint64
}

// ErrEnrolment reports an enrolment that cannot be made.
var ErrEnrolment = errors.New("enrolment refused")

func readAgentMeta(tx *journal.Tx) (*pb.AgentMeta, error) {
	m := &pb.AgentMeta{}
	if _, err := get(tx, bucketMeta, keyAgentMeta, m); err != nil {
		return nil, err
	}
	return m, nil
}

// activeEnrolment returns the active enrolment, or nil.
func activeEnrolment(tx *journal.Tx) (*pb.Enrolment, error) {
	m, err := readAgentMeta(tx)
	if err != nil {
		return nil, err
	}
	if len(m.GetCurrentEnrolmentId()) == 0 {
		return nil, nil
	}
	e := &pb.Enrolment{}
	ok, err := get(tx, bucketEnrolments, m.GetCurrentEnrolmentId(), e)
	if err != nil {
		return nil, err
	}
	if !ok || e.GetState() != pb.EnrolmentState_ENROLMENT_STATE_ACTIVE {
		return nil, nil
	}
	return e, nil
}

func (e Enrolment) validate(last uint64) error {
	switch {
	case len(e.ID) != idLen:
		return fmt.Errorf("%w: enrolment id must be %d bytes", ErrEnrolment, idLen)
	case e.ProviderUID == "":
		return fmt.Errorf("%w: no provider uid", ErrEnrolment)
	case e.StartEpoch == 0 || e.StartEpoch <= last:
		return fmt.Errorf("%w: start epoch %d must be above %d", ErrEnrolment, e.StartEpoch, last)
	}
	return nil
}

// activate writes e as the active enrolment.
func activate(tx *journal.Tx, e Enrolment) error {
	if tx.Get(bucketEnrolments, e.ID) != nil {
		return fmt.Errorf("%w: enrolment id already used", ErrEnrolment)
	}
	rec := &pb.Enrolment{EnrolmentId: e.ID, ProviderUid: e.ProviderUID, InstallId: e.InstallID,
		StartEpoch: e.StartEpoch, State: pb.EnrolmentState_ENROLMENT_STATE_ACTIVE}
	if err := put(tx, bucketEnrolments, e.ID, rec); err != nil {
		return err
	}
	m, err := readAgentMeta(tx)
	if err != nil {
		return err
	}
	m.CurrentEnrolmentId = append([]byte(nil), e.ID...)
	m.LastStartEpoch = e.StartEpoch
	return put(tx, bucketMeta, keyAgentMeta, m)
}

// Enrol makes the host's first enrolment. It fails when any enrolment
// exists; use Reenrol to replace one.
func (s *Store) Enrol(ctx context.Context, e Enrolment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		m, err := readAgentMeta(tx)
		if err != nil {
			return err
		}
		if m.GetLastStartEpoch() != 0 {
			return fmt.Errorf("%w: the host is already enrolled", ErrEnrolment)
		}
		if err := e.validate(0); err != nil {
			return err
		}
		return activate(tx, e)
	})
	if err != nil {
		return fmt.Errorf("enrol: %w", err)
	}
	return nil
}

// Lookup returns what the store knows about an operation: its record or, once
// compacted, its tombstone. Reads are served in every state, including a
// lost store that could be opened, so that status queries are answered.
func (s *Store) Lookup(ctx context.Context, ref OpRef) (OpView, bool, error) {
	var v OpView
	var found bool
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		k := opKey(ref.EnrolmentID, ref.OpID)
		rec := &pb.OpRecord{}
		ok, err := get(tx, bucketOps, k, rec)
		if err != nil {
			return err
		}
		if ok {
			v, found = viewOfRecord(rec), true
			return nil
		}
		t := &pb.Tombstone{}
		ok, err = get(tx, bucketTombstones, k, t)
		if err != nil {
			return err
		}
		if ok {
			v, found = viewOfTombstone(t), true
		}
		return nil
	})
	if err != nil {
		return OpView{}, false, fmt.Errorf("lookup %s: %w", ref, err)
	}
	return v, found, nil
}

// HasTombstone reports whether the operation has a tombstone.
func (s *Store) HasTombstone(ctx context.Context, ref OpRef) (bool, error) {
	var ok bool
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		ok = tx.Get(bucketTombstones, opKey(ref.EnrolmentID, ref.OpID)) != nil
		return nil
	})
	return ok, err
}

// VMDeleted reports whether a completed Delete of the VM placement is
// recorded in the enrolment, and its op_id. Only a VM tombstone, written in
// the Delete's terminal transaction, counts; the absence of a VM record never
// does.
func (s *Store) VMDeleted(ctx context.Context, enrolment []byte, vmUID string, binding []byte) ([]byte, bool, error) {
	var opID []byte
	var ok bool
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		t := &pb.VMTombstone{}
		found, err := get(tx, bucketVMTombstones, vmTombstoneKey(enrolment, vmUID, binding), t)
		if err != nil {
			return err
		}
		if found {
			opID, ok = t.GetDeleteOpId(), true
		}
		return nil
	})
	return opID, ok, err
}

// witnessDir is the witness directory of an enrolment.
func (s *Store) witnessDir(enrolment []byte) string {
	return filepath.Join(s.j.Dir(), witnessDirName, fmt.Sprintf("%x", enrolment))
}

// runKey names an operation in the running set.
func runKey(ref OpRef) string { return string(ref.EnrolmentID) + string(ref.OpID) }

// sameBytes is bytes.Equal, named for readability at call sites.
func sameBytes(a, b []byte) bool { return bytes.Equal(a, b) }
