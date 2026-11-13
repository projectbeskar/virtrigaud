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
	"context"
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Reenrol replaces the active enrolment with e, in two transactions:
//
//  1. freeze: every non-terminal operation of the predecessor enrolment
//     with no step intent at all is retired (Retired{provider_changed}, with
//     its tombstone); every other one becomes
//     OutcomeUnknown{provider_changed} and keeps holding its VM; the
//     predecessor enrolment becomes frozen and no enrolment is active;
//  2. activate: e becomes the active enrolment.
//
// A crash between the two leaves the host with no active enrolment, which
// refuses every operation; calling Reenrol again completes it. The frozen
// enrolment's tombstones, VM tombstones, attempt marks and witnesses are kept,
// read-only, until ConfirmFreeze, and their op_ids are answered from them.
// e.StartEpoch must exceed every earlier start epoch. No runner may be
// active. The freeze is one transaction over every open operation of the
// predecessor.
func (s *Store) Reenrol(ctx context.Context, e Enrolment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.running) > 0 {
		return fmt.Errorf("re-enrol: %w", ErrRunning)
	}
	var last uint64
	var prev []byte
	if err := s.j.View(ctx, func(tx *journal.Tx) error {
		m, err := readAgentMeta(tx)
		if err != nil {
			return err
		}
		last, prev = m.GetLastStartEpoch(), m.GetCurrentEnrolmentId()
		// Refuse a used id before freezing anything.
		retired, err := isRetired(tx, e.ID)
		if err != nil {
			return err
		}
		used, err := has(tx, bucketEnrolments, e.ID)
		if err != nil {
			return err
		}
		if retired || used {
			return fmt.Errorf("%w: enrolment id already used", ErrEnrolment)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("re-enrol: %w", err)
	}
	if last == 0 {
		return fmt.Errorf("re-enrol: %w: the host was never enrolled", ErrEnrolment)
	}
	if err := e.validate(last); err != nil {
		return fmt.Errorf("re-enrol: %w", err)
	}
	if len(prev) > 0 {
		buf := &auditBuf{}
		if _, err := s.j.Update(ctx, func(tx *journal.Tx) error { return freeze(tx, prev, buf) }); err != nil {
			return fmt.Errorf("re-enrol: freeze: %w", err)
		}
		s.flush(buf)
		if err := s.j.CrashPoint(PointMidReenrolment); err != nil {
			return err
		}
	}
	if err := s.activateBound(ctx, e, prev); err != nil {
		return fmt.Errorf("re-enrol: %w", err)
	}
	return nil
}

// freeze freezes the enrolment prev and its open operations.
func freeze(tx *journal.Tx, prev []byte, buf *auditBuf) error {
	var open []*pb.OpRecord
	if err := tx.ForEach(bucketOps, prev, func(_, v []byte) error {
		rec := &pb.OpRecord{}
		if err := unmarshalRecord(v, rec); err != nil {
			return err
		}
		if rec.GetState() == StateAccepted || rec.GetState() == StateExecuting {
			open = append(open, rec)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, rec := range open {
		id := rec.GetIdentity()
		key := opKey(id.GetEnrolmentId(), id.GetOpId())
		rec.Reason = ReasonProviderChanged
		if anyIntent(rec) {
			rec.State = StateOutcomeUnknown
			if err := put(tx, bucketOps, key, rec); err != nil {
				return err
			}
			if err := buf.write(tx, AuditEvent{Event: "frozen", Ref: OpRef{EnrolmentID: id.GetEnrolmentId(), OpID: id.GetOpId()},
				VMUID: id.GetVmUid(), Kind: providerv1.OperationKind(id.GetKind()), Reason: rec.GetReason(), State: rec.GetState()}); err != nil {
				return err
			}
			continue
		}
		rec.State = StateRetired
		if err := put(tx, bucketOps, key, rec); err != nil {
			return err
		}
		if err := terminalWrites(tx, rec, buf); err != nil {
			return err
		}
	}
	e := &pb.Enrolment{}
	if _, err := get(tx, bucketEnrolments, prev, e); err != nil {
		return err
	}
	e.State = pb.EnrolmentState_ENROLMENT_STATE_FROZEN
	if err := put(tx, bucketEnrolments, prev, e); err != nil {
		return err
	}
	m, err := readAgentMeta(tx)
	if err != nil {
		return err
	}
	m.CurrentEnrolmentId = nil
	return put(tx, bucketMeta, keyAgentMeta, m)
}

func anyIntent(rec *pb.OpRecord) bool {
	for _, st := range rec.GetSteps() {
		if st.GetIntent() {
			return true
		}
	}
	return false
}

// ConfirmFreeze discards a frozen enrolment's records, once the manager has
// confirmed that it holds every operation of that enrolment: the operation
// records, op_id tombstones, attempt marks, the enrolment record and the
// witness directory. VM tombstones are kept. VM records that the discarded
// operations held are released (the manager now holds those operations). The
// enrolment id is remembered as retired: it is never activated again, and a
// grant naming it is refused UNKNOWN_TO_AGENT, never as definitely not
// performed. It fails unless prev is frozen and another enrolment is active.
func (s *Store) ConfirmFreeze(ctx context.Context, prev []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.j.Update(ctx, func(tx *journal.Tx) error {
		e := &pb.Enrolment{}
		ok, err := get(tx, bucketEnrolments, prev, e)
		if err != nil {
			return err
		}
		if !ok || e.GetState() != pb.EnrolmentState_ENROLMENT_STATE_FROZEN {
			return fmt.Errorf("%w: enrolment is not frozen", ErrEnrolment)
		}
		act, err := activeEnrolment(tx)
		if err != nil {
			return err
		}
		if act == nil {
			return fmt.Errorf("%w: no successor enrolment is active", ErrEnrolment)
		}
		for _, b := range []string{bucketOps, bucketTombstones, bucketAttempts} {
			var keys [][]byte
			if err := tx.ForEach(b, prev, func(k, _ []byte) error {
				keys = append(keys, k)
				return nil
			}); err != nil {
				return err
			}
			for _, k := range keys {
				if err := tx.Delete(b, k); err != nil {
					return err
				}
			}
		}
		var held []*pb.VMRecord
		if err := tx.ForEach(bucketVMs, nil, func(_, v []byte) error {
			vm := &pb.VMRecord{}
			if err := unmarshalRecord(v, vm); err != nil {
				return err
			}
			if sameBytes(vm.GetBusyEnrolmentId(), prev) {
				held = append(held, vm)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, vm := range held {
			vm.BusyOpId, vm.BusyEnrolmentId = nil, nil
			if err := put(tx, bucketVMs, []byte(vm.GetVmUid()), vm); err != nil {
				return err
			}
		}
		m, err := readAgentMeta(tx)
		if err != nil {
			return err
		}
		m.RetiredEnrolmentIds = append(m.RetiredEnrolmentIds, append([]byte(nil), prev...))
		if err := put(tx, bucketMeta, keyAgentMeta, m); err != nil {
			return err
		}
		return tx.Delete(bucketEnrolments, prev)
	})
	if err != nil {
		return fmt.Errorf("confirm freeze: %w", err)
	}
	dir := s.witnessDir(prev)
	if err := s.fsys.RemoveAll(dir); err != nil {
		return fmt.Errorf("confirm freeze: remove witnesses: %w", err)
	}
	return nil
}
