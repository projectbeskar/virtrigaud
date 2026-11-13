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
)

// ActiveEnrolment is the active enrolment as the store records it.
type ActiveEnrolment struct {
	// ID is the 16-byte enrolment identifier.
	ID []byte
	// ProviderUID is the Provider metadata.uid.
	ProviderUID string
	// HostID is the identity of the host.
	HostID string
	// InstallID is the installation identifier.
	InstallID []byte
	// StartEpoch is the epoch counter the enrolment started at.
	StartEpoch uint64
}

// HostView is what a host answer about the host itself is made from, read in
// one read transaction so that every part reflects the same store state.
type HostView struct {
	// Enrolment is the active enrolment; nil when none is active.
	Enrolment *ActiveEnrolment
	// Position is the committed chain position the view belongs to.
	Position journal.Position
	// CheckpointSeq is the newest checkpoint at or below Position.Seq; 0 when
	// the store has none.
	CheckpointSeq uint64
	// CheckpointHead is the head at CheckpointSeq.
	CheckpointHead [32]byte
	// ContinuityHead is the head at the continuity sequence number asked
	// for; nil when none was asked for, it is above Position.Seq, or the
	// store holds no head for it.
	ContinuityHead []byte
	// LastStartEpoch is the highest start epoch of any enrolment so far; 0
	// for a store that was never enrolled.
	LastStartEpoch uint64
}

// HostView reads the active enrolment, the committed chain position, the
// newest checkpoint at or below it and, when continuitySeq is not 0, the
// head at continuitySeq, in one read transaction. It is served in every
// state in which the journal could be opened.
func (s *Store) HostView(ctx context.Context, continuitySeq uint64) (HostView, error) {
	var v HostView
	err := s.j.View(ctx, func(tx *journal.Tx) error {
		var err error
		v, err = hostViewTx(tx, continuitySeq)
		return err
	})
	if err != nil {
		return HostView{}, fmt.Errorf("read host view: %w", err)
	}
	return v, nil
}

// hostViewTx reads a HostView in tx.
func hostViewTx(tx *journal.Tx, continuitySeq uint64) (HostView, error) {
	var v HostView
	pos, ok := tx.CommittedPosition()
	if !ok {
		return HostView{}, fmt.Errorf("store position unreadable")
	}
	v.Position = pos
	if cp, head, ok := tx.LastCheckpoint(pos.Seq); ok {
		v.CheckpointSeq, v.CheckpointHead = cp, head
	}
	if continuitySeq != 0 && continuitySeq <= pos.Seq {
		if h, ok := tx.HeadAt(continuitySeq); ok {
			v.ContinuityHead = append([]byte(nil), h[:]...)
		}
	}
	m, err := readAgentMeta(tx)
	if err != nil {
		return HostView{}, err
	}
	v.LastStartEpoch = m.GetLastStartEpoch()
	e, err := activeEnrolment(tx)
	if err != nil {
		return HostView{}, err
	}
	if e != nil {
		v.Enrolment = &ActiveEnrolment{
			ID:          append([]byte(nil), e.GetEnrolmentId()...),
			ProviderUID: e.GetProviderUid(),
			HostID:      e.GetHostId(),
			InstallID:   append([]byte(nil), e.GetInstallId()...),
			StartEpoch:  e.GetStartEpoch(),
		}
	}
	return v, nil
}

// Recovered reports whether recovery has run, so that the store accepts
// operations while its journal is ready. It does not wait for a recovery in
// progress.
func (s *Store) Recovered() bool { return s.recovered.Load() }
