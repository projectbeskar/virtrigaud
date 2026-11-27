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

// Operation receipts and the host ledger (development builds only).
//
// A send signed at grant version 2 gets a host-signed receipt in the
// Provider's reply. The manager verifies it against the attempt's
// authenticated intent and the grant of that very call, and against the
// host ledger: the receipt's store must be the ledger's and its session the
// ledger's adopted one. A verified receipt moves the attempt to Accepted
// with the receipt recorded on its marker, bound to the attempt by a tag
// under the intent key, and raises the ledger, then the agent's ledger mark,
// so that the agent may run the operation. A receipt that does not verify
// is "no receipt": the attempt stays Sent and the heartbeat's raise covers
// the operation later.
//
// A recorded receipt counts as ledgered (covered by the ledger the manager
// holds) when its session is the ledger's current or a pending one and its
// position is at or below the ledger's, or its session is an earlier one
// and its position is at or below where that session was fenced. A session
// older than the ledger remembers counts as ledgered: the safe direction.
// A re-send of an attempt whose receipt counts as ledgered sets
// expect_known, so an agent that holds no record of the operation refuses
// it rather than run it as new.
//
// Every decision on the host ledger an attempt was prepared under reads the
// store and generation covered by the attempt's authenticated intent, never
// a marker or a timestamp: an attempt prepared under another store or
// generation, or without one (an intent authenticated under the first
// intent version counts as generation 0), is never continuous with the
// current ledger.

import (
	"bytes"
	"context"
	"encoding/hex"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// receiptMessage is the message a marker receipt's tag covers: the
// attempt's op_id, kind and number, the VM's UID, and every field of the
// receipt but the tag.
func receiptMessage(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt,
	rc *infravirtrigaudiov1beta1.OperationAttemptReceipt) []byte {
	return operation.ReceiptMessage([]byte(a.OpID), []byte(a.Kind), []byte(strconv.FormatInt(a.Attempt, 10)), []byte(vm.UID),
		[]byte(rc.StoreID), []byte(strconv.FormatInt(rc.Seq, 10)), []byte(rc.Head), []byte(rc.AttestationID))
}

// receiptTag is the hex tag binding rc to attempt a of vm.
func (r *VirtualMachineReconciler) receiptTag(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt,
	rc *infravirtrigaudiov1beta1.OperationAttemptReceipt) string {
	return hex.EncodeToString(r.OperationIntentAuth.SealIntent(receiptMessage(vm, a, rc)))
}

// receiptBound reports whether a's recorded receipt is one the manager
// recorded for a: its tag verifies over a's op_id, kind, attempt, vm's UID
// and the receipt.
func (r *VirtualMachineReconciler) receiptBound(vm *infravirtrigaudiov1beta1.VirtualMachine, a *infravirtrigaudiov1beta1.OperationAttempt) bool {
	if r.OperationIntentAuth == nil || a.Receipt == nil {
		return false
	}
	tag, err := hex.DecodeString(a.Receipt.ReceiptTag)
	return err == nil && r.OperationIntentAuth.OpenIntent(receiptMessage(vm, a, a.Receipt), tag)
}

// ledgerContinuous reports whether the attempt prepared as p, read from its
// authenticated intent, is continuous with the ledger rec: prepared under
// rec's store and current generation, with its enrolment among the
// generation's enrolments. An attempt prepared without a ledger
// (generation 0) never is.
func ledgerContinuous(p contracts.PreparedOperation, rec *HostLedgerRecord) bool {
	return rec != nil && p.LedgerGeneration != 0 && bytes.Equal(p.LedgerStoreID, rec.StoreID[:]) &&
		p.LedgerGeneration == rec.Generation && rec.HasEnrolment(p.Identity.EnrolmentID)
}

// receiptLedgered reports whether the marker receipt rc counts as ledgered
// under the ledger rec (see the file comment). A receipt of another store,
// or one that does not parse, counts as ledgered.
func receiptLedgered(rec *HostLedgerRecord, rc *infravirtrigaudiov1beta1.OperationAttemptReceipt) bool {
	store, err1 := hex.DecodeString(rc.StoreID)
	session, err2 := hex.DecodeString(rc.AttestationID)
	if rec == nil || err1 != nil || err2 != nil || len(session) != 16 || !bytes.Equal(store, rec.StoreID[:]) || rc.Seq < 1 {
		return true
	}
	seq := uint64(rc.Seq) // #nosec G115 -- checked positive above
	id := [16]byte(session)
	if id == rec.AttestationID || rec.HasPendingSession(id) {
		return seq <= rec.Position.Seq
	}
	for _, f := range rec.SessionHistory {
		if f.ID == id {
			return seq <= f.FencedAt
		}
	}
	return true
}

// expectKnown reports whether a re-send of attempt a (prepared as p) sets
// expect_known: a's receipt counts as ledgered, or cannot be vouched for (a
// receipt whose tag fails, an unreadable ledger, an attempt not continuous
// with the ledger): an agent with no record of the operation then refuses
// it instead of running it as new.
func (r *VirtualMachineReconciler) expectKnown(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p contracts.PreparedOperation) bool {
	if a.Receipt == nil {
		return false
	}
	if !r.receiptBound(vm, a) || r.HostAnswers.Ledger == nil {
		return true
	}
	store, err := hex.DecodeString(a.Receipt.StoreID)
	if err != nil || len(store) != 16 {
		return true
	}
	rec, err := r.HostAnswers.Ledger.Load(ctx, ia.providerCR, a.HostID, [16]byte(store))
	if err != nil || !ledgerContinuous(p, rec) {
		return true
	}
	return receiptLedgered(rec, a.Receipt)
}

// verifiedReceipt is a receipt verified for an attempt, with its marker
// form.
type verifiedReceipt struct {
	receipt hostidentity.VerifiedReceipt
	marker  *infravirtrigaudiov1beta1.OperationAttemptReceipt
	// generation is the ledger generation it was verified under.
	generation uint64
}

// acceptReceipt verifies the receipt of a send of attempt a (prepared as
// p, signed at grant version 2) that answer carried: against the host's
// trust, the attempt's intent, the grant of that call, and the host
// ledger's store and adopted session. It returns nil when there is no
// receipt that verifies.
func (r *VirtualMachineReconciler) acceptReceipt(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p contracts.PreparedOperation, answer contracts.OperationAnswer) *verifiedReceipt {
	l := r.HostAnswers.Ledger
	if l == nil || answer.Receipt == nil || p.GrantVersion != grantverify.LedgerGrantVersion {
		return nil
	}
	logger := log.FromContext(ctx).WithValues("host", a.HostID)
	t, err := r.HostAnswers.Trust.HostTrust(ctx, ia.providerCR, a.HostID)
	if err != nil {
		return nil
	}
	wire, ok := wireKinds[p.Kind]
	if !ok {
		return nil
	}
	id := p.Identity
	vr, err := hostidentity.VerifyReceipt(blobOf(answer.Receipt), hostidentity.ReceiptExpectation{
		Anchors: t.Anchors, HostURI: t.HostURI, HostName: t.HostName, HostID: a.HostID, Pins: t.Pins,
		ProviderUID: string(ia.providerCR.UID), InstallID: t.InstallID, EnrolmentID: id.EnrolmentID,
		OpID: id.OpID, Kind: wire, Attempt: id.Attempt, VMUID: id.VMUID, BindingID: id.BindingID,
		IntentDigest: operation.IntentDigest(p.DigestSalt, p.RequestBytes), GrantDigest: answer.GrantDigest, Now: r.now(),
	})
	if err != nil {
		logger.Info("The operation receipt did not verify; it is not recorded", "check", err.Error())
		return nil
	}
	rec, err := l.Load(ctx, ia.providerCR, a.HostID, vr.StoreID)
	if err != nil || rec.AttestationState != infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested ||
		rec.AttestationID != vr.AttestationID {
		logger.Info("The operation receipt is not of the host ledger's store and adopted session; it is not recorded")
		return nil
	}
	return &verifiedReceipt{receipt: vr, generation: rec.Generation, marker: &infravirtrigaudiov1beta1.OperationAttemptReceipt{
		StoreID: hex.EncodeToString(vr.StoreID[:]), Seq: int64(vr.Seq), // #nosec G115 -- a chain sequence number
		Head: hex.EncodeToString(vr.Head[:]), AttestationID: hex.EncodeToString(vr.AttestationID[:]),
	}}
}

// recordReceipt returns sent with the verified receipt vr recorded and
// tagged, in phase Accepted (keeping its transition time once delivered).
func (r *VirtualMachineReconciler) recordReceipt(vm *infravirtrigaudiov1beta1.VirtualMachine, sent infravirtrigaudiov1beta1.OperationAttempt,
	vr *verifiedReceipt) infravirtrigaudiov1beta1.OperationAttempt {
	if vr == nil {
		return sent
	}
	rc := *vr.marker
	rc.ReceiptTag = r.receiptTag(vm, &sent, &rc)
	sent.Receipt = &rc
	if sent.Phase != infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted {
		if !attemptDelivered(sent.Phase) {
			sent.LastTransitionTime = metav1.NewTime(r.now())
		}
		sent.Phase = infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted
	}
	if vr.generation > 0 {
		sent.LedgerGeneration = int64(vr.generation) // #nosec G115 -- a generation is a Unix time or a small counter
	}
	return sent
}

// signedSend sends attempt a, prepared as p, at the grant version the host
// takes now (sendVersion): grant version 2 for a host the host ledger
// drives, only while it is attested in the ledger's adopted session
// (errLedgerNotAttested otherwise, and nothing is sent), with expect_known
// on a re-send whose receipt counts as ledgered; the original version for
// any other host, as before. p records the version and flag sent.
func (r *VirtualMachineReconciler) signedSend(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ia identityAttempt,
	a *infravirtrigaudiov1beta1.OperationAttempt, p *contracts.PreparedOperation, first bool) (contracts.OperationAnswer, error) {
	version, ok := r.HostAnswers.sendVersion(ia.providerCR, a.HostID)
	if !ok {
		return contracts.OperationAnswer{}, errLedgerNotAttested
	}
	p.GrantVersion, p.ExpectKnown = version, false
	if version == grantverify.LedgerGrantVersion && !first {
		p.ExpectKnown = r.expectKnown(ctx, vm, ia, a, *p)
	}
	return ia.sender.SendOperation(ctx, *p)
}

// ledgerAccepted raises the host's ledger and mark over an operation a
// verified operation answer v shows accepted, with its acceptance position,
// in the ledger's adopted session (an answer equivalent to the operation's
// receipt).
func (h *HostAnswers) ledgerAccepted(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, reader contracts.HostAnswerReader,
	hostID string, v hostidentity.VerifiedAnswer) {
	op := v.Operation
	session, ok := sessionOf(v.Context)
	if h.Ledger == nil || op == nil || !ok || op.GetAcceptedSeq() == 0 || len(op.GetAcceptedHead()) != 32 ||
		op.GetState() != providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED {
		return
	}
	vr := hostidentity.VerifiedReceipt{StoreID: v.Position.StoreID, Seq: op.GetAcceptedSeq(), AttestationID: session}
	copy(vr.Head[:], op.GetAcceptedHead())
	h.ledgerReceipt(ctx, provider, reader, hostID, vr)
}

// ledgerReceipt raises the host's ledger to a verified receipt and then the
// agent's ledger mark (operation traffic), so the agent runs the operation.
// A step that cannot be taken now is taken by the heartbeat's raise.
func (h *HostAnswers) ledgerReceipt(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, reader contracts.HostAnswerReader,
	hostID string, vr hostidentity.VerifiedReceipt) {
	l := h.Ledger
	auth, ok := reader.(contracts.HostAuthorityCaller)
	if l == nil || !ok || !h.buckets.allow(trafficOperation, h.now()) {
		return
	}
	logger := log.FromContext(ctx).WithValues("host", hostID)
	rec, err := l.Raise(ctx, provider, hostID, vr.StoreID, []HostLedgerRaise{{Session: vr.AttestationID,
		Position: HostLedgerPosition{Seq: vr.Seq, Head: vr.Head}}})
	if err != nil {
		logger.Info("The host's ledger was not raised from the receipt; the heartbeat raises it", "error", err.Error())
		return
	}
	t, err := h.Trust.HostTrust(ctx, provider, hostID)
	if err != nil {
		return
	}
	if err := h.markHost(ctx, provider, auth, hostID, t, rec, trafficOperation); err != nil {
		logger.Info("The host's ledger mark was not raised from the receipt; the heartbeat raises it", "error", err.Error())
	}
}
