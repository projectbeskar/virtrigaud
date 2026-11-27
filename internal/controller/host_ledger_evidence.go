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

// The evidence a host ledger start is decided on (development builds only).
// A ledger is started only from hostidentity.LedgerEvidence, which only the
// answer verifier builds, and only from the latest evidence the ledger
// observed for the host: the evidence must name the store being started, the
// Provider, the host and its current enrolment, carry the nonce of the last
// verified ledger answer of the host, and be younger than ledgerEvidenceTTL.
// Evidence of another store, of an older request, or of another enrolment
// is refused before anything is read or written.

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// ledgerEvidenceTTL is how long ledger evidence may be acted on after its
// answer verified.
const ledgerEvidenceTTL = hostAnswerTTL

// errHostLedgerEvidence: the evidence passed to a ledger start is not
// acceptable (not verified, of another store, request, host or enrolment,
// or too old). Nothing was read or written.
var errHostLedgerEvidence = errors.New("the host ledger evidence is not acceptable")

// observedEvidence is the latest ledger evidence observed for a host.
type observedEvidence struct {
	nonce []byte
	at    time.Time
}

// Observe records ev, the evidence of a verified ledger answer of the host
// hostID of provider, as the host's latest: only that evidence may then
// start a ledger (Initialise). An invalid ev, or one of another Provider or
// host, is ignored.
func (l *HostLedger) Observe(provider *infravirtrigaudiov1beta1.Provider, hostID string, ev hostidentity.LedgerEvidence) {
	if l == nil || !ev.Valid() || ev.ProviderUID() != string(provider.UID) || ev.HostID() != hostID {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.observed == nil {
		l.observed = map[string]observedEvidence{}
	}
	l.observed[hostAnswerKey(provider, hostID)] = observedEvidence{nonce: ev.Nonce(), at: ev.VerifiedAt()}
}

// initEvidence is what a ledger start reads from accepted evidence.
type initEvidence struct {
	position, checkpoint HostLedgerPosition
	originEnrol          []byte
	enrolCount           uint32
	attestation          hostidentity.StoreAttestation
	continuitySeq        uint64
	continuityHead       [32]byte
	hasContinuity        bool
}

// checkEvidence accepts ev for starting the ledger of store for lh: valid
// (built by the verifier), of store, of lh's Provider, host and current
// enrolment, the latest evidence observed for lh (same nonce), and younger
// than ledgerEvidenceTTL.
func (l *HostLedger) checkEvidence(lh *ledgerHost, store [16]byte, ev hostidentity.LedgerEvidence) (initEvidence, error) {
	refuse := func(why string) (initEvidence, error) {
		return initEvidence{}, fmt.Errorf("store %s: %s: %w", storeIDPrefix(store), why, errHostLedgerEvidence)
	}
	switch {
	case !ev.Valid():
		return refuse("not evidence from a verified answer")
	case ev.StoreID() != store:
		return refuse("evidence of another store")
	case ev.ProviderUID() != string(lh.provider.UID) || ev.HostID() != lh.hostID:
		return refuse("evidence of another Provider or host")
	case !bytes.Equal(ev.EnrolmentID(), lh.enrolment):
		return refuse("evidence under another enrolment than the host's current one")
	}
	l.mu.Lock()
	obs, ok := l.observed[lh.key]
	l.mu.Unlock()
	nonce := ev.Nonce()
	switch {
	case !ok || len(obs.nonce) == 0 || len(nonce) != len(obs.nonce) || subtle.ConstantTimeCompare(nonce, obs.nonce) != 1:
		return refuse("evidence of an earlier request than the host's latest verified ledger answer")
	case l.now().Sub(ev.VerifiedAt()) > ledgerEvidenceTTL || ev.VerifiedAt().After(l.now().Add(time.Minute)):
		return refuse("evidence too old to act on")
	}
	pos := ev.Position()
	cpSeq, cpHead := ev.Checkpoint()
	origin, count := ev.Origin()
	out := initEvidence{position: HostLedgerPosition{Seq: pos.Seq, Head: pos.Head},
		checkpoint: HostLedgerPosition{Seq: cpSeq, Head: cpHead}, originEnrol: origin, enrolCount: count,
		attestation: ev.Attestation()}
	out.continuitySeq, out.continuityHead, out.hasContinuity = ev.Continuity()
	return out, nil
}
