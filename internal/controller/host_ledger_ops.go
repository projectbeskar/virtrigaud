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

// The host ledger writes (development builds only), in the order an
// attestation driver uses them:
//
//   - Initialise: the first ledger object of a store, from the host's last
//     verified position once the host confirmed it in a signed answer, or,
//     for a store the host's agent says was created under the current
//     enrolment, never held another and never ran an operation, from first
//     contact. A first contact archives the ledger of the Host's previous
//     store.
//   - Rebind: a ledger object that served an earlier Host object of the same
//     machine moves to its replacement, once the earlier Host is deleted or
//     revoked, quiet, and no other Host carries the identity.
//   - BeginSession: an attestation session (a challenge nonce) is added to
//     the pending sessions.
//   - Raise: the ledger position is raised from verified positions of the
//     current session (receipts and answers), in batches.
//   - Adopt: a pending session is made current once a nonce-bound signed
//     answer proves the agent holds it.
//
// Every session-changing write, and every raise from receipts, records the
// ledger's write counter in the host's tagged ledger index before it
// returns; a raise made only from heartbeat answers does so at most every
// 30 seconds.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// HostLedgerSessionPurpose is why an attestation session is begun.
type HostLedgerSessionPurpose int

const (
	// LedgerSessionAttest: the host's agent awaits attestation (after a
	// start); the ledger becomes Pending.
	LedgerSessionAttest HostLedgerSessionPurpose = iota + 1
	// LedgerSessionReattest: the agent is attested in a session the ledger
	// does not hold; the current session stays current and Attested until
	// the new one is adopted.
	LedgerSessionReattest
)

// HostLedgerAdoptEvidence is the kind of nonce-bound signed answer that
// proves the agent holds a pending session. An operation receipt is not
// nonce-bound and is never evidence; there is no value for it.
type HostLedgerAdoptEvidence int

const (
	// LedgerEvidenceHostAnswer: a verified HostAnswer.
	LedgerEvidenceHostAnswer HostLedgerAdoptEvidence = iota + 1
	// LedgerEvidenceOperationAnswer: a verified OperationAnswer.
	LedgerEvidenceOperationAnswer
	// LedgerEvidenceVMAnswer: a verified VMAnswer.
	LedgerEvidenceVMAnswer
	// LedgerEvidenceChallengeAnswer: a verified ChallengeAnswer.
	LedgerEvidenceChallengeAnswer
	// LedgerEvidenceAuthorityAnswer: a verified AuthorityAnswer.
	LedgerEvidenceAuthorityAnswer
)

// valid reports whether e is one of the defined kinds.
func (e HostLedgerAdoptEvidence) valid() bool {
	return e >= LedgerEvidenceHostAnswer && e <= LedgerEvidenceAuthorityAnswer
}

// HostLedgerOrigin is the store-origin evidence of a verified
// ChallengeAnswer (its HostLedgerState): the enrolment under which the
// store was created and how many enrolments it ever held.
type HostLedgerOrigin struct {
	// OriginEnrolmentID is the store's origin enrolment (16 bytes); empty
	// for a store that predates the record.
	OriginEnrolmentID []byte
	// EnrolmentCount is how many enrolments were ever activated in the
	// store; 0 for a store that predates the record.
	EnrolmentCount uint32
}

// HostLedgerContinuity is the confirmation of a verified host answer to a
// request whose continuity_seq named Seq: the head the host signed at Seq.
type HostLedgerContinuity struct {
	// Seq is the request's continuity_seq.
	Seq uint64
	// Head is the answer's continuity_head.
	Head [32]byte
}

// HostLedgerInit is the input of Initialise.
type HostLedgerInit struct {
	// StoreID is the host's store (from the verified answer).
	StoreID [16]byte
	// Position and Checkpoint are the store's chain position and checkpoint
	// in the verified ChallengeAnswer the origin evidence came from: a first
	// contact starts the ledger there, and only when Position is at most
	// three (a store that never ran an operation).
	Position, Checkpoint HostLedgerPosition
	// Origin is the store-origin evidence of that verified ChallengeAnswer;
	// nil when none.
	Origin *HostLedgerOrigin
	// Continuity is the host's confirmation of the last verified position
	// (see Initialise); nil when none.
	Continuity *HostLedgerContinuity
}

// HostLedgerInitSource says where Initialise took a ledger from.
type HostLedgerInitSource string

const (
	// LedgerInitExisting: the store already had a ledger object.
	LedgerInitExisting HostLedgerInitSource = "Existing"
	// LedgerInitLastPosition: started from the host's last verified
	// position, confirmed by the host.
	LedgerInitLastPosition HostLedgerInitSource = "LastPosition"
	// LedgerInitFirstContact: started from the agent's signed position for
	// a store created under the current enrolment that never ran an
	// operation.
	LedgerInitFirstContact HostLedgerInitSource = "FirstContact"
)

// HostLedgerInitResult is the result of Initialise.
type HostLedgerInitResult struct {
	// Record is the store's ledger.
	Record *HostLedgerRecord
	// Source says where it came from.
	Source HostLedgerInitSource
	// Archived are the ledgers of the Host's earlier stores that a first
	// contact archived: the caller holds the open attempts of their
	// enrolments as lost with the store.
	Archived []*HostLedgerRecord
}

// HostLedgerRaise is one verified position of a store that Raise may raise
// the ledger to.
type HostLedgerRaise struct {
	// Session is the attestation session the position was signed in;
	// positions of any session other than the ledger's current one are
	// dropped.
	Session [16]byte
	// Position is the verified chain position (a receipt's accepted
	// position, or an answer's chain position).
	Position HostLedgerPosition
	// Checkpoint is the verified checkpoint beside it; Seq 0 when none.
	Checkpoint HostLedgerPosition
	// Heartbeat reports a position from a heartbeat answer rather than a
	// receipt: a batch of heartbeat positions only raises the ledger at
	// most once per 30 seconds per host.
	Heartbeat bool
}

// nextGeneration returns the generation of a ledger started afresh: above
// the predecessor's (pred, 0: none) and the Host's high-water mark in its
// index (indexMax), and never below the current Unix time in seconds, so a
// generation is never handed out twice for a store even when an earlier
// ledger object or index of it was lost.
func (l *HostLedger) nextGeneration(pred, indexMax uint64) uint64 {
	now := l.now().Unix()
	if now < 1 {
		now = 1
	}
	return max(pred+1, indexMax+1, uint64(now))
}

// headOf returns the head a store record holds at seq.
func headOf(r hostidentity.StoreRecord, seq uint64) ([32]byte, bool) {
	if seq == 0 {
		return [32]byte{}, false
	}
	switch seq {
	case r.Seq:
		return r.Head, true
	case r.CheckpointSeq:
		return r.CheckpointHead, true
	}
	return [32]byte{}, false
}

// validPosition checks a position and its checkpoint.
func validPosition(pos, cp HostLedgerPosition) error {
	switch {
	case pos.Seq < 1:
		return fmt.Errorf("%w: a position needs a sequence number of at least 1", errHostLedgerInput)
	case cp.Seq%hostidentity.CheckpointInterval != 0 || cp.Seq > pos.Seq:
		return fmt.Errorf("%w: a checkpoint must be a multiple of %d not above its position", errHostLedgerInput,
			hostidentity.CheckpointInterval)
	}
	return nil
}

// Initialise returns the ledger of store in.StoreID for the host hostID of
// provider, starting it when the store has none:
//
//   - an existing ledger object is returned (Source Existing); one that
//     serves another Host object is ErrHostLedgerNotBound (Rebind) and an
//     archived one ErrHostLedgerArchived, both with no record;
//   - another store with a ledger under the host's current enrolment holds
//     the host (ledger_store_mismatch, persistent): one enrolment has one
//     store;
//   - a store the host's ledger index names, but whose ledger object is
//     gone, holds the host (ledger_missing): a lost ledger is never
//     restarted;
//   - when the host's last verified position (the pins Secret's store
//     state) is of this store, the ledger starts there, but only once the
//     host has confirmed it: in.Continuity must be the verified answer to a
//     request whose continuity_seq named that position (or its checkpoint,
//     after an honest pruning) carrying the same head; otherwise the host
//     is held (ledger_unconfirmed);
//   - otherwise only a first contact may start it: in.Origin must show the
//     store was created under the current enrolment and held no other, and
//     in.Position must be at most three, so the store never ran an
//     operation. The ledgers of the Host's earlier stores are then archived
//     (Archived). Any other case holds the host (ledger_missing).
//
// The store is reserved in the host's ledger index before its object is
// created, and its write counter recorded after; a new ledger's generation
// is above every generation the index recorded for the Host.
func (l *HostLedger) Initialise(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
	in HostLedgerInit) (HostLedgerInitResult, error) {
	if in.StoreID == ([16]byte{}) {
		return HostLedgerInitResult{}, fmt.Errorf("%w: no store id", errHostLedgerInput)
	}
	if err := validPosition(in.Position, in.Checkpoint); err != nil {
		return HostLedgerInitResult{}, err
	}
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return HostLedgerInitResult{}, err
	}
	m := l.storeLock(in.StoreID)
	m.Lock()
	defer m.Unlock()
	for range maxLedgerWriteAttempts {
		res, retry, err := l.initialiseOnce(ctx, lh, in)
		if retry {
			continue
		}
		if err == nil {
			l.clearHold(lh)
		}
		return res, err
	}
	return HostLedgerInitResult{}, fmt.Errorf("store %s: %w", storeIDPrefix(in.StoreID), errHostLedgerConflict)
}

// initialiseOnce is one attempt of Initialise; retry reports that a
// concurrent writer created the object first.
func (l *HostLedger) initialiseOnce(ctx context.Context, lh *ledgerHost, in HostLedgerInit) (HostLedgerInitResult, bool, error) {
	prefix := storeIDPrefix(in.StoreID)
	st, err := l.storeState(ctx, lh, in.StoreID)
	if err != nil {
		return HostLedgerInitResult{}, false, err
	}
	obj, err := l.readRecord(ctx, lh, in.StoreID, st.LedgerIndex)
	switch {
	case err == nil:
		l.remember(lh, obj.rec, false)
		if err := requireBound(lh, obj.rec); err != nil {
			return HostLedgerInitResult{}, false, err
		}
		return HostLedgerInitResult{Record: obj.rec, Source: LedgerInitExisting}, false, nil
	case !errors.Is(err, ErrHostLedgerNotFound):
		return HostLedgerInitResult{}, false, err
	}
	others, err := l.relatedLedgers(ctx, lh, in.StoreID, st.LedgerIndex)
	if err != nil {
		return HostLedgerInitResult{}, false, err
	}
	enrolmentLabel := hex.EncodeToString(lh.enrolment)
	for _, o := range others {
		if !o.rec.Archived && (o.enrolmentLabel == enrolmentLabel || o.rec.HasEnrolment(lh.enrolment)) {
			return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonStoreMismatch, prefix, true,
				"another store already has a ledger under the host's current enrolment; re-enrol the host with a fresh store",
				&ledgerObject{rec: o.rec, sec: o.sec})
		}
		if o.rec.HostUID == lh.uid() && st.LedgerIndex == nil {
			return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonTampered, prefix, false,
				"a ledger serves this Host but the Host's ledger index is gone", nil)
		}
	}
	if st.LedgerIndex.Names(in.StoreID) {
		return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonMissing, prefix, false,
			"the store had a ledger and its ledger object is gone; a lost ledger is never restarted", nil)
	}
	now := l.now().UTC().Truncate(time.Second)
	rec := &HostLedgerRecord{StoreID: in.StoreID, HostIdentityMAC: lh.identityMAC, HostUID: lh.uid(),
		HostKey: hostKey(lh.host.Namespace, lh.host.Name), ProviderUID: string(lh.provider.UID), GenerationStartedAt: now,
		GenerationEnrolments: [][]byte{bytes.Clone(lh.enrolment)}}
	var indexMax uint64
	for _, s := range st.LedgerIndex.storesOrNil() {
		rec.WriteCounter = max(rec.WriteCounter, s.Counter)
	}
	if st.LedgerIndex != nil {
		indexMax = st.LedgerIndex.MaxGeneration
	}
	res := HostLedgerInitResult{}
	if last := st.Current; !last.Empty() && last.StoreID == in.StoreID {
		want := last.ContinuitySeq()
		head, ok := headOf(last, want)
		c := in.Continuity
		if !ok || c == nil || c.Seq != want || c.Head != head {
			return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonUnconfirmed, prefix, false,
				"the host has not confirmed, in a signed answer, the last position the ledger would start from", nil)
		}
		rec.Position = HostLedgerPosition{Seq: last.Seq, Head: last.Head}
		rec.Checkpoint = HostLedgerPosition{Seq: last.CheckpointSeq, Head: last.CheckpointHead}
		rec.Generation = l.nextGeneration(0, indexMax)
		res.Source = LedgerInitLastPosition
	} else {
		o := in.Origin
		switch {
		case o == nil || o.EnrolmentCount != 1 || !bytes.Equal(o.OriginEnrolmentID, lh.enrolment):
			return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonMissing, prefix, false,
				"the store has no ledger and its agent does not show it was created under the current enrolment alone", nil)
		case in.Position.Seq > hostLedgerFirstContactMaxSeq:
			return HostLedgerInitResult{}, false, l.hold(ctx, lh, LedgerReasonMissing, prefix, false,
				"the store has no ledger and its chain shows it may have run operations; use the fresh-store procedure", nil)
		}
		var pred uint64
		for _, p := range others {
			if p.rec.Archived || p.rec.HostIdentityMAC != lh.identityMAC {
				continue
			}
			if p.rec.HostUID != lh.uid() {
				if reason, detail := l.boundHostReleased(ctx, p.rec); reason != "" {
					return HostLedgerInitResult{}, false, l.hold(ctx, lh, reason, p.rec.StorePrefix(), false, detail, nil)
				}
			}
			archived, err := l.archive(ctx, lh, p.rec.StoreID, in.StoreID)
			if err != nil {
				return HostLedgerInitResult{}, false, err
			}
			res.Archived = append(res.Archived, archived)
			pred = max(pred, archived.Generation)
		}
		rec.Position, rec.Checkpoint = in.Position, in.Checkpoint
		rec.Generation = l.nextGeneration(pred, indexMax)
		res.Source = LedgerInitFirstContact
	}
	// Reserve the store first: if the object is created and the counter
	// write fails, the store is still named, so its object is never lost
	// unseen; if the creation fails, a reserved store with no object is
	// started again.
	if err := l.persistIndex(ctx, lh, indexChange{stores: map[[16]byte]uint64{in.StoreID: 0}, generation: rec.Generation}); err != nil {
		return HostLedgerInitResult{}, false, fmt.Errorf("reserve store %s in the host's ledger index: %w", prefix, err)
	}
	data, err := l.prepare(rec)
	if err != nil {
		return HostLedgerInitResult{}, false, err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hostLedgerSecretName(in.StoreID), Namespace: l.Config.Namespace},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{hostLedgerDataKey: data}}
	setLedgerLabels(sec, rec, lh.enrolment)
	if err := l.Client.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return HostLedgerInitResult{}, true, nil
		}
		return HostLedgerInitResult{}, false, fmt.Errorf("create the host ledger of store %s: %w", prefix, err)
	}
	got, err := l.written(ctx, lh, sec, in.StoreID, false)
	if err != nil {
		return HostLedgerInitResult{}, false, err
	}
	res.Record = got
	if l.Recorder != nil {
		l.Recorder.Event(lh.host, corev1.EventTypeNormal, eventReasonLedgerInitialised,
			fmt.Sprintf("the ledger of store %s was started (%s) at generation %d", prefix, res.Source, got.Generation))
	}
	if err := l.recordCounter(ctx, lh, got, nil, false); err != nil {
		return res, false, err
	}
	return res, false, nil
}

// storesOrNil returns the index's stores (nil for a nil index).
func (x *HostLedgerIndex) storesOrNil() []HostLedgerIndexStore {
	if x == nil {
		return nil
	}
	return x.Stores
}

// relatedLedger is a verified ledger object related to a Host.
type relatedLedger struct {
	rec            *HostLedgerRecord
	sec            *corev1.Secret
	enrolmentLabel string
}

// relatedLedgers returns the verified ledger objects of stores other than
// store that are labelled with lh's current enrolment or identity MAC, and
// those lh's index names (read by name). Labels only find objects; each is
// decided on its verified body. An object that fails its checks, a store
// the index names that the labels do not find, or a confirmed store the
// index names whose object is gone, holds the host.
func (l *HostLedger) relatedLedgers(ctx context.Context, lh *ledgerHost, store [16]byte, idx *HostLedgerIndex) ([]relatedLedger, error) {
	prefix := storeIDPrefix(store)
	seen := map[string]bool{}
	var out []relatedLedger
	add := func(sec *corev1.Secret) error {
		rec, err := l.verifySecret(ctx, sec, [16]byte{})
		if err != nil {
			var tamper *ledgerTamper
			if errors.As(err, &tamper) {
				return l.hold(ctx, lh, LedgerReasonTampered, prefix, false, "a related ledger object fails its checks: "+tamper.detail, nil)
			}
			return err
		}
		if reason, detail := l.checkGuard(rec); reason != "" {
			return l.hold(ctx, lh, reason, rec.StorePrefix(), true, detail, nil)
		}
		if e, ok := idx.entry(rec.StoreID); ok && rec.WriteCounter < e.Counter {
			return l.hold(ctx, lh, LedgerReasonRegressed, rec.StorePrefix(), true,
				"a related ledger's write counter is below the highest the manager recorded", nil)
		}
		out = append(out, relatedLedger{rec: rec, sec: sec, enrolmentLabel: sec.Labels[LabelHostLedgerEnrolment]})
		return nil
	}
	for _, sel := range []client.MatchingLabels{
		{k8sLabelComponent: hostLedgerComponent, LabelHostLedgerEnrolment: hex.EncodeToString(lh.enrolment)},
		{k8sLabelComponent: hostLedgerComponent, LabelHostLedgerIdentityMAC: hex.EncodeToString(lh.identityMAC[:])[:hostLedgerIdentityLabelLen]},
	} {
		var list corev1.SecretList
		if err := l.APIReader.List(ctx, &list, client.InNamespace(l.Config.Namespace), sel); err != nil {
			return nil, fmt.Errorf("list host ledgers: %w", err)
		}
		for i := range list.Items {
			sec := &list.Items[i]
			if seen[sec.Name] || sec.Name == hostLedgerSecretName(store) {
				continue
			}
			seen[sec.Name] = true
			if err := add(sec); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range idx.storesOrNil() {
		name := hostLedgerSecretName(e.ID)
		if e.ID == store || seen[name] {
			continue
		}
		sec := &corev1.Secret{}
		err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: l.Config.Namespace, Name: name}, sec)
		switch {
		case apierrors.IsNotFound(err) && e.Counter == 0:
			continue // reserved, never created
		case apierrors.IsNotFound(err):
			return nil, l.hold(ctx, lh, LedgerReasonTampered, storeIDPrefix(e.ID), false,
				"a store the Host's ledger index names has lost its ledger object", nil)
		case err != nil:
			return nil, fmt.Errorf("get host ledger of store %s: %w", storeIDPrefix(e.ID), err)
		}
		return nil, l.hold(ctx, lh, LedgerReasonTampered, storeIDPrefix(e.ID), false,
			"a store the Host's ledger index names is not labelled for this Host or its machine", nil)
	}
	return out, nil
}

// archive marks the ledger of store as replaced by successor and records
// its counter in lh's index. Its labels are kept.
func (l *HostLedger) archive(ctx context.Context, lh *ledgerHost, store, successor [16]byte) (*HostLedgerRecord, error) {
	m := l.storeLock(store)
	m.Lock()
	defer m.Unlock()
	for range maxLedgerWriteAttempts {
		sec := &corev1.Secret{}
		if err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: l.Config.Namespace, Name: hostLedgerSecretName(store)}, sec); err != nil {
			return nil, fmt.Errorf("get host ledger of store %s: %w", storeIDPrefix(store), err)
		}
		rec, err := l.verifySecret(ctx, sec, store)
		if err != nil {
			var tamper *ledgerTamper
			if errors.As(err, &tamper) {
				return nil, l.hold(ctx, lh, LedgerReasonTampered, storeIDPrefix(store), false, tamper.detail, nil)
			}
			return nil, err
		}
		if reason, detail := l.checkGuard(rec); reason != "" {
			return nil, l.hold(ctx, lh, reason, rec.StorePrefix(), true, detail, nil)
		}
		if rec.Archived {
			return rec, nil
		}
		next := rec.clone()
		next.Archived, next.SuccessorStoreID, next.ArchivedAt = true, successor, l.now().UTC().Truncate(time.Second)
		got, err := l.writeObject(ctx, lh, sec, next, nil, false)
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("archive the host ledger of store %s: %w", storeIDPrefix(store), err)
		}
		if got.HostUID == lh.uid() {
			if err := l.persistIndex(ctx, lh, indexChange{stores: map[[16]byte]uint64{store: got.WriteCounter}}); err != nil {
				return nil, fmt.Errorf("record archived store %s in the host's ledger index: %w", got.StorePrefix(), err)
			}
		}
		if l.Recorder != nil {
			l.Recorder.Event(lh.host, corev1.EventTypeNormal, eventReasonLedgerArchived,
				fmt.Sprintf("the ledger of store %s was archived: the host has a new store %s", got.StorePrefix(), storeIDPrefix(successor)))
		}
		return got, nil
	}
	return nil, fmt.Errorf("store %s: %w", storeIDPrefix(store), errHostLedgerConflict)
}

// boundHostReleased reports whether the Host object a ledger serves has
// released it: deleted (gone, replaced by another object of the same name,
// or deleting) or revoked, with no verified answer of that object younger
// than hostAnswerTTL, and seen released by this process for at least
// hostAnswerTTL (so a restart waits again). It returns the hold reason and
// detail when not.
func (l *HostLedger) boundHostReleased(ctx context.Context, rec *HostLedgerRecord) (reason, detail string) {
	ns, name, ok := strings.Cut(rec.HostKey, "/")
	if !ok {
		return LedgerReasonStoreBound, "the ledger's Host reference is malformed"
	}
	h := &infravirtrigaudiov1beta1.Host{}
	err := l.APIReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, h)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return LedgerReasonStoreBound, "the Host the ledger serves cannot be read"
	case string(h.UID) == rec.HostUID && h.DeletionTimestamp.IsZero() && !h.Spec.Revoked:
		l.mu.Lock()
		delete(l.goneSince, rec.HostUID)
		l.mu.Unlock()
		return LedgerReasonStoreBound, "the Host the ledger serves is live and not revoked"
	}
	now := l.now()
	if l.lastAnswer != nil {
		if at := l.lastAnswer(rec.ProviderUID, name, rec.HostUID); !at.IsZero() && now.Sub(at) <= hostAnswerTTL {
			return LedgerReasonStoreBound, "the Host the ledger serves answered recently"
		}
	}
	l.mu.Lock()
	if l.goneSince == nil {
		l.goneSince = map[string]time.Time{}
	}
	since, seen := l.goneSince[rec.HostUID]
	if !seen {
		since = now
		l.goneSince[rec.HostUID] = now
	}
	l.mu.Unlock()
	if now.Sub(since) < hostAnswerTTL {
		return LedgerReasonStoreBound, "the Host the ledger serves was released less than the answer lifetime ago"
	}
	return "", ""
}

// Rebind moves the ledger of store to the host hostID of provider, a new
// Host object of the same machine, keeping its position, generation and
// sessions (never a fresh start) and making it Pending: the agent must be
// attested again, and the next adoption is the new Host's. It is allowed
// only when the ledger is bound to the same identity MAC (else the host is
// held, ledger_store_foreign), the Host object it serves is deleted or
// revoked, has no verified answer younger than hostAnswerTTL (answers of a
// replacement with the same name do not count) and was seen released by
// this process at least that long, and no Host other than this one that is
// not revoked carries the identity (else ledger_store_bound). The store is
// reserved in the new Host's ledger index first and its counter recorded
// after. A ledger already bound to the Host is returned unchanged.
func (l *HostLedger) Rebind(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store [16]byte) (*HostLedgerRecord, error) {
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	var reserved bool
	rec, err := l.update(ctx, lh, store, updateOptions{enrolment: lh.enrolment}, func(r *HostLedgerRecord, _ *ledgerObject) (bool, error) {
		switch {
		case r.Archived:
			return false, fmt.Errorf("store %s: %w", r.StorePrefix(), ErrHostLedgerArchived)
		case r.HostIdentityMAC != lh.identityMAC:
			return false, l.hold(ctx, lh, LedgerReasonStoreForeign, r.StorePrefix(), false, "the ledger is bound to another machine identity", nil)
		case r.HostUID == lh.uid():
			return false, nil
		}
		if reason, detail := l.boundHostReleased(ctx, r); reason != "" {
			return false, l.hold(ctx, lh, reason, r.StorePrefix(), false, detail, nil)
		}
		others, err := l.otherIdentityHolders(ctx, lh.host)
		if err != nil {
			return false, err
		}
		if others > 0 {
			return false, l.hold(ctx, lh, LedgerReasonStoreBound, r.StorePrefix(), false,
				"another Host that is not revoked carries the same identity", nil)
		}
		if !reserved {
			if err := l.persistIndex(ctx, lh, indexChange{stores: map[[16]byte]uint64{store: 0}, generation: r.Generation}); err != nil {
				return false, fmt.Errorf("reserve store %s in the host's ledger index: %w", r.StorePrefix(), err)
			}
			reserved = true
		}
		r.HostUID, r.HostKey, r.ProviderUID = lh.uid(), hostKey(lh.host.Namespace, lh.host.Name), string(lh.provider.UID)
		if r.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested {
			r.AttestationState = infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending
		}
		return true, nil
	})
	if err != nil {
		return rec, err
	}
	if l.Recorder != nil && reserved {
		l.Recorder.Event(lh.host, corev1.EventTypeNormal, eventReasonLedgerRebound,
			fmt.Sprintf("the ledger of store %s now serves this Host (generation %d kept)", rec.StorePrefix(), rec.Generation))
	}
	l.clearHold(lh)
	return rec, nil
}

// otherIdentityHolders counts the Hosts other than host that carry its
// identity and that block a rebind (hostsWithIdentity, read live).
func (l *HostLedger) otherIdentityHolders(ctx context.Context, host *infravirtrigaudiov1beta1.Host) (int, error) {
	others, err := hostsWithIdentity(ctx, l.APIReader, nil, host)
	return len(others), err
}

// BeginSession adds attestation session session (the challenge nonce) to
// the pending sessions of the ledger of store, evicting the oldest beyond
// four. tip is the store's chain tip in the verified answer the challenge
// came with: a ledger above it holds the host (ledger_ahead) and nothing is
// added. For LedgerSessionAttest the ledger becomes Pending; for
// LedgerSessionReattest it must be Attested and stays so, with its current
// session, until Adopt. A session already pending is not added again.
func (l *HostLedger) BeginSession(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store, session [16]byte,
	purpose HostLedgerSessionPurpose, tip HostLedgerPosition) (*HostLedgerRecord, error) {
	if session == ([16]byte{}) || (purpose != LedgerSessionAttest && purpose != LedgerSessionReattest) {
		return nil, fmt.Errorf("%w: a session needs an id and a purpose", errHostLedgerInput)
	}
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	rec, err := l.update(ctx, lh, store, updateOptions{enrolment: lh.enrolment}, func(r *HostLedgerRecord, _ *ledgerObject) (bool, error) {
		if err := requireBound(lh, r); err != nil {
			return false, err
		}
		if tip.Seq < r.Position.Seq {
			return false, l.hold(ctx, lh, LedgerReasonAhead, r.StorePrefix(), false, "the ledger is above the host's chain tip", nil)
		}
		if purpose == LedgerSessionReattest && r.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested {
			return false, fmt.Errorf("store %s: a re-attestation needs an adopted session: %w", r.StorePrefix(), ErrHostLedgerNotAttested)
		}
		changed := false
		current := r.AttestationID == session && r.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested
		if !r.HasPendingSession(session) && !current {
			r.PendingSessions = append(r.PendingSessions, HostLedgerPendingSession{ID: session, BegunAt: l.now().UTC().Truncate(time.Second)})
			if n := len(r.PendingSessions); n > maxLedgerPendingSessions {
				r.PendingSessions = r.PendingSessions[n-maxLedgerPendingSessions:]
			}
			changed = true
		}
		if purpose == LedgerSessionAttest && r.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending {
			r.AttestationState = infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending
			changed = true
		}
		return changed, nil
	})
	if err != nil {
		return rec, err
	}
	l.clearHold(lh)
	return rec, nil
}

// Raise raises the ledger of store from a batch of verified positions: to
// the highest position, and the checkpoint to the highest checkpoint, of
// those signed in the ledger's current session (the others are dropped),
// and adds the host's current enrolment to the generation's enrolments. The
// ledger must be Attested. Two positions with one sequence number and
// different heads (among the batch, or against the ledger's position or
// checkpoint) hold the host (ledger_fork, persistent). A batch of heartbeat
// positions only raises the ledger at most once per 30 seconds per host,
// and records its counter in the index at most as often. Nothing is
// written when nothing changes.
func (l *HostLedger) Raise(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store [16]byte,
	batch []HostLedgerRaise) (*HostLedgerRecord, error) {
	for _, in := range batch {
		if err := validPosition(in.Position, in.Checkpoint); err != nil {
			return nil, err
		}
	}
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	heartbeatOnly := len(batch) > 0
	for _, in := range batch {
		heartbeatOnly = heartbeatOnly && in.Heartbeat
	}
	if heartbeatOnly {
		l.mu.Lock()
		at := l.heartbeatAt[lh.key]
		l.mu.Unlock()
		if !at.IsZero() && l.now().Sub(at) < ledgerHeartbeatRaiseInterval {
			return l.Load(ctx, provider, hostID, store)
		}
	}
	rec, err := l.update(ctx, lh, store, updateOptions{enrolment: lh.enrolment, lazyIndex: heartbeatOnly},
		func(r *HostLedgerRecord, obj *ledgerObject) (bool, error) {
			if err := requireBound(lh, r); err != nil {
				return false, err
			}
			if r.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested {
				return false, fmt.Errorf("store %s: %w", r.StorePrefix(), ErrHostLedgerNotAttested)
			}
			heads := map[uint64][32]byte{r.Position.Seq: r.Position.Head}
			if r.Checkpoint.Seq > 0 {
				heads[r.Checkpoint.Seq] = r.Checkpoint.Head
			}
			fork := func(p HostLedgerPosition) bool {
				if h, ok := heads[p.Seq]; ok && h != p.Head {
					return true
				}
				heads[p.Seq] = p.Head
				return false
			}
			pos, cp := r.Position, r.Checkpoint
			for _, in := range batch {
				if in.Session != r.AttestationID {
					continue
				}
				if fork(in.Position) || (in.Checkpoint.Seq > 0 && fork(in.Checkpoint)) {
					return false, l.hold(ctx, lh, LedgerReasonFork, r.StorePrefix(), true,
						"two verified positions of one sequence number carry different heads", obj)
				}
				if in.Position.Seq > pos.Seq {
					pos = in.Position
				}
				if in.Checkpoint.Seq > cp.Seq {
					cp = in.Checkpoint
				}
			}
			changed := pos != r.Position || cp != r.Checkpoint
			r.Position, r.Checkpoint = pos, cp
			if changed && !r.HasEnrolment(lh.enrolment) {
				r.GenerationEnrolments = append(r.GenerationEnrolments, bytes.Clone(lh.enrolment))
				if n := len(r.GenerationEnrolments); n > maxLedgerGenerationEnrolments {
					r.GenerationEnrolments = r.GenerationEnrolments[n-maxLedgerGenerationEnrolments:]
				}
			}
			return changed, nil
		})
	if err != nil {
		return rec, err
	}
	if heartbeatOnly {
		l.mu.Lock()
		if l.heartbeatAt == nil {
			l.heartbeatAt = map[string]time.Time{}
		}
		l.heartbeatAt[lh.key] = l.now()
		l.mu.Unlock()
	}
	l.clearHold(lh)
	return rec, nil
}

// Adopt makes pending session session the current session of the ledger of
// store, on the evidence of a verified nonce-bound answer of kind evidence
// that carries it (never an operation receipt): the previous current
// session, if any, moves to the session history fenced at the ledger's
// position now (at most eight are kept), the ledger becomes Attested and
// the pending sessions are cleared. A session that is not pending is
// ErrHostLedgerSessionNotPending; the current, adopted session is returned
// unchanged.
func (l *HostLedger) Adopt(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, store, session [16]byte,
	evidence HostLedgerAdoptEvidence) (*HostLedgerRecord, error) {
	if session == ([16]byte{}) || !evidence.valid() {
		return nil, fmt.Errorf("%w: adoption needs a session id and nonce-bound answer evidence", errHostLedgerInput)
	}
	lh, err := l.resolve(ctx, provider, hostID)
	if err != nil {
		return nil, err
	}
	rec, err := l.update(ctx, lh, store, updateOptions{enrolment: lh.enrolment, adopt: true}, func(r *HostLedgerRecord, _ *ledgerObject) (bool, error) {
		if err := requireBound(lh, r); err != nil {
			return false, err
		}
		if r.AttestationID == session && r.AttestationState == infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested {
			return false, nil
		}
		if !r.HasPendingSession(session) {
			return false, fmt.Errorf("store %s: %w", r.StorePrefix(), ErrHostLedgerSessionNotPending)
		}
		if r.AttestationID != ([16]byte{}) && r.AttestationID != session {
			r.SessionHistory = append(r.SessionHistory, HostLedgerFencedSession{ID: r.AttestationID, FencedAt: r.Position.Seq})
			if n := len(r.SessionHistory); n > maxLedgerSessionHistory {
				r.SessionHistory = r.SessionHistory[n-maxLedgerSessionHistory:]
			}
		}
		r.AttestationID, r.AttestationState, r.PendingSessions = session, infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested, nil
		return true, nil
	})
	if err != nil {
		return rec, err
	}
	l.clearHold(lh)
	return rec, nil
}
