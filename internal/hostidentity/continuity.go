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

package hostidentity

import (
	"bytes"
	"crypto/subtle"
)

// Store continuity. For each host the manager remembers, for the host's
// current store, the last verified chain position (L) and the last verified
// checkpoint (C). Every verified answer is checked against what the manager
// held when it drew the request's nonce: a store that changed under the same
// enrolment, a sequence number that went down, a chain head that differs
// from one the manager saw, or a checkpoint that differs or went down means
// the host's store was replaced, rolled back or forked, and the host is held.

// StoreRecord is what the manager remembers of one host's store.
type StoreRecord struct {
	// StoreID identifies the store; all zero when nothing was seen yet.
	StoreID [16]byte
	// EnrolmentID is the enrolment under which StoreID was last seen.
	EnrolmentID []byte
	// Seq and Head are the last verified position (L).
	Seq  uint64
	Head [32]byte
	// CheckpointSeq and CheckpointHead are the last verified checkpoint (C);
	// CheckpointSeq is 0 when none was seen.
	CheckpointSeq  uint64
	CheckpointHead [32]byte
	// UseCheckpoint asks the next request to name CheckpointSeq instead of
	// Seq: the store pruned the head at Seq honestly.
	UseCheckpoint bool
}

// Empty reports whether nothing was seen of the host's store yet.
func (r StoreRecord) Empty() bool { return r.StoreID == [16]byte{} }

// ContinuitySeq is the continuity_seq a request drawn with r as its snapshot
// names: the checkpoint after an honest pruning, else the last position; 0
// when nothing was seen.
func (r StoreRecord) ContinuitySeq() uint64 {
	if r.Empty() {
		return 0
	}
	if r.UseCheckpoint && r.CheckpointSeq > 0 {
		return r.CheckpointSeq
	}
	return r.Seq
}

// headAt is the head r recorded at seq.
func (r StoreRecord) headAt(seq uint64) ([32]byte, bool) {
	switch {
	case seq == r.Seq:
		return r.Head, true
	case r.CheckpointSeq > 0 && seq == r.CheckpointSeq:
		return r.CheckpointHead, true
	}
	return [32]byte{}, false
}

// Continuity is the result of CheckContinuity.
type Continuity struct {
	// Regressed reports that the host's store was replaced, rolled back or
	// forked: the host is held and the answer is not acted on.
	Regressed bool
	// Reason names the check that failed: store_changed, rolled_back,
	// forked or checkpoint_regressed.
	Reason string
	// Next is the record after the answer: raised to the answer's position
	// and checkpoint when they are higher. It equals snapshot when
	// Regressed.
	Next StoreRecord
}

// Continuity reasons.
const (
	// ContinuityStoreChanged: another store under the same enrolment.
	ContinuityStoreChanged = "store_changed"
	// ContinuityRolledBack: the sequence number went down.
	ContinuityRolledBack = "rolled_back"
	// ContinuityForked: a head differs from one the manager saw.
	ContinuityForked = "forked"
	// ContinuityCheckpointRegressed: the checkpoint differs or went down.
	ContinuityCheckpointRegressed = "checkpoint_regressed"
)

// CheckContinuity checks a verified answer against snapshot, the record the
// manager held when it drew the request (whose continuity_seq was
// snapshot.ContinuitySeq()), and returns the record raised by it.
func CheckContinuity(snapshot StoreRecord, v VerifiedAnswer) Continuity {
	held := func(reason string) Continuity { return Continuity{Regressed: true, Reason: reason, Next: snapshot} }
	pos, ctx := v.Position, v.Context
	var cpHead [32]byte
	copy(cpHead[:], ctx.GetCheckpointHead())
	fresh := StoreRecord{StoreID: pos.StoreID, EnrolmentID: bytes.Clone(ctx.GetEnrolmentId()), Seq: pos.Seq, Head: pos.Head,
		CheckpointSeq: ctx.GetCheckpointSeq(), CheckpointHead: cpHead}
	if snapshot.Empty() {
		return Continuity{Next: fresh}
	}
	if pos.StoreID != snapshot.StoreID {
		if bytes.Equal(ctx.GetEnrolmentId(), snapshot.EnrolmentID) {
			return held(ContinuityStoreChanged)
		}
		// A new store under a new enrolment (a re-enrolment after a lost
		// store) starts a new record.
		return Continuity{Next: fresh}
	}
	if pos.Seq < snapshot.Seq {
		return held(ContinuityRolledBack)
	}
	next := snapshot
	next.EnrolmentID = bytes.Clone(ctx.GetEnrolmentId())
	if s := snapshot.ContinuitySeq(); s > 0 {
		want, _ := snapshot.headAt(s)
		got := ctx.GetContinuityHead()
		switch {
		case len(got) != 0 && subtle.ConstantTimeCompare(got, want[:]) != 1:
			return held(ContinuityForked)
		case len(got) == 0 && s == snapshot.CheckpointSeq:
			return held(ContinuityForked)
		case len(got) == 0 && pos.Seq-s < CheckpointInterval:
			return held(ContinuityForked)
		case len(got) == 0:
			next.UseCheckpoint = true
		}
	}
	if cp := ctx.GetCheckpointSeq(); snapshot.CheckpointSeq > 0 && cp > 0 {
		switch {
		case cp == snapshot.CheckpointSeq && cpHead != snapshot.CheckpointHead:
			return held(ContinuityCheckpointRegressed)
		case cp < snapshot.CheckpointSeq:
			return held(ContinuityCheckpointRegressed)
		}
	}
	if pos.Seq > next.Seq {
		next.Seq, next.Head, next.UseCheckpoint = pos.Seq, pos.Head, false
	}
	if cp := ctx.GetCheckpointSeq(); cp > next.CheckpointSeq {
		next.CheckpointSeq, next.CheckpointHead = cp, cpHead
	}
	return Continuity{Next: next}
}

// MergeStoreRecords returns the higher of two records of the same store (the
// one a concurrent answer may have raised meanwhile). A record of another
// store replaces cur: CheckContinuity returns one only for a new store under
// a new enrolment.
func MergeStoreRecords(cur, next StoreRecord) StoreRecord {
	if cur.Empty() || cur.StoreID != next.StoreID {
		return next
	}
	out := cur
	if next.Seq > out.Seq {
		out.Seq, out.Head, out.UseCheckpoint = next.Seq, next.Head, next.UseCheckpoint
	} else if next.Seq == out.Seq {
		out.UseCheckpoint = out.UseCheckpoint || next.UseCheckpoint
	}
	if next.CheckpointSeq > out.CheckpointSeq {
		out.CheckpointSeq, out.CheckpointHead = next.CheckpointSeq, next.CheckpointHead
	}
	out.EnrolmentID = bytes.Clone(next.EnrolmentID)
	return out
}
