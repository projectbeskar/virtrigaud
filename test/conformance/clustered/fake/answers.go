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

package fake

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/url"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// StepDeadline is the fake hosts' step deadline: a stalled operation's
// outcome is unknown once it has run this long.
const StepDeadline = 300 * time.Millisecond

// stepDeadlineReason is the outcome reason of an operation that ran past the
// step deadline.
const stepDeadlineReason = "step_deadline"

// rolledBackReason is the quarantine reason of a host whose store was rolled
// back.
const rolledBackReason = "rolled_back"

// hostKeys are the fake hosts' answer-signing keys under one issuer, each
// with a certificate of the agent profile: server authentication only, and
// the host's URI identity with its enrolment record as its only names.
type hostKeys struct {
	roots *x509.CertPool
	keys  map[string]*ecdsa.PrivateKey
	certs map[string][]byte
	pins  map[string]string
	uris  map[string]string
}

// newHostKeys makes an issuer and a signing certificate for each host,
// enrolled with enrolments[host], providerUID and installID.
func newHostKeys(hosts []string, trustDomain, namespace, providerUID string, installID []byte, enrolments map[string][]byte) (*hostKeys, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake host issuer"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	hk := &hostKeys{roots: x509.NewCertPool(), keys: map[string]*ecdsa.PrivateKey{}, certs: map[string][]byte{},
		pins: map[string]string{}, uris: map[string]string{}}
	hk.roots.AddCert(ca)
	for i, h := range hosts {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		hostURI, err := hostidentity.HostURI(trustDomain, namespace, h)
		if err != nil {
			return nil, err
		}
		rec := hostidentity.Record{EnrolmentID: enrolments[h], ProviderUID: providerUID, InstallID: installID, Mode: hostidentity.ModeIdentity}
		var uris []*url.URL
		for _, s := range hostidentity.RequestURIs(hostURI, rec) {
			u, err := url.Parse(s)
			if err != nil {
				return nil, err
			}
			uris = append(uris, u)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), NotBefore: now.Add(-time.Hour),
			NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, URIs: uris}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		if err != nil {
			return nil, err
		}
		pin, err := hostidentity.SPKIPin(&k.PublicKey)
		if err != nil {
			return nil, err
		}
		hk.keys[h], hk.certs[h], hk.pins[h], hk.uris[h] = k, der, pin, hostURI
	}
	return hk, nil
}

// answerState is a host's store-like state for its signed answers.
type answerState struct {
	storeID [16]byte
	seq     uint64
	// quarantine is the reason the host's store is quarantined; "" when it
	// is not.
	quarantine string
	// stallNext makes the next accepted operation stall.
	stallNext bool
	// attempts is the highest attempt accepted per VM and kind.
	attempts map[string]uint64
	// vmTombs names the Delete of each deleted placement (vm uid and binding
	// in hex), by op_id in hex.
	vmTombs map[string]string
	// lastOp is the op_id in hex of the last accepted operation.
	lastOp string
	// firstNonce is the nonce of the first signed answer (StaleAnswerNonce).
	firstNonce []byte
}

func newAnswerState() *answerState {
	a := &answerState{seq: 1, attempts: map[string]uint64{}, vmTombs: map[string]string{}}
	_, _ = rand.Read(a.storeID[:])
	return a
}

// headAt is the host's chain head at seq.
func (a *answerState) headAt(seq uint64) []byte {
	head := sha256.Sum256(binary.BigEndian.AppendUint64(append([]byte(nil), a.storeID[:]...), seq))
	return head[:]
}

// position is the host's chain position: store id, sequence number, head.
func (a *answerState) position() []byte {
	out := append([]byte(nil), a.storeID[:]...)
	out = binary.BigEndian.AppendUint64(out, a.seq)
	return append(out, a.headAt(a.seq)...)
}

// attemptKey names a VM and kind in answerState.attempts.
func attemptKey(vmUID string, kind providerv1.OperationKind) string {
	return fmt.Sprintf("%s/%d", vmUID, kind)
}

// vmTombKey names a placement in answerState.vmTombs.
func vmTombKey(vmUID string, binding []byte) string {
	return vmUID + "/" + hex.EncodeToString(binding)
}

// answerContext is the context of a signed answer of host for nonce. The
// caller holds w.mu.
func (p *provider) answerContext(host string, hs *hostState, nonce []byte, continuitySeq uint64) *providerv1.AnswerContext {
	a := hs.answers
	if a.firstNonce == nil {
		a.firstNonce = append([]byte(nil), nonce...)
	}
	if p.v(StaleAnswerNonce) {
		nonce = a.firstNonce
	}
	ctx := &providerv1.AnswerContext{
		Nonce: append([]byte(nil), nonce...), ProviderUid: p.ident.providerUID, HostId: host,
		InstallId: p.ident.signer.InstallID(), EnrolmentId: append([]byte(nil), p.ident.enrolments[host]...),
		ChainPosition: a.position(), AgentState: providerv1.AgentState_AGENT_STATE_READY,
		AgentTime: timestamppb.Now(),
	}
	if a.quarantine != "" {
		ctx.AgentState, ctx.StateReason = providerv1.AgentState_AGENT_STATE_QUARANTINED, a.quarantine
	}
	if continuitySeq > 0 && continuitySeq <= a.seq {
		ctx.ContinuityHead = a.headAt(continuitySeq)
	}
	return ctx
}

// signedHostInfo adds a signed HostAnswer to info for a GetHostInfo that
// carries answer_nonce. The caller holds w.mu.
func (p *provider) signedHostInfo(req *providerv1.GetHostInfoRequest, hs *hostState, info *providerv1.HostInfo) error {
	if len(req.GetAnswerNonce()) != answer.NonceLen {
		return status.Error(codes.InvalidArgument, "answer_nonce must be 16 bytes")
	}
	a := &providerv1.HostAnswer{Context: p.answerContext(req.GetHostId(), hs, req.GetAnswerNonce(), req.GetContinuitySeq()),
		AgentVersion: "fake", AgentApiVersion: answer.AgentAPIVersion, GrantVersions: []uint32{answer.GrantVersion},
		Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY}
	blob, err := p.sign(req.GetHostId(), a)
	if err != nil {
		return status.Errorf(codes.Internal, "sign: %v", err)
	}
	info.HostAnswer = blob
	return nil
}

// sign signs body with host's key.
func (p *provider) sign(host string, body proto.Message) (*providerv1.SignedBlob, error) {
	hk := p.ident.hostKeys
	return answer.Sign(hk.keys[host], hk.certs[host], body)
}

// recordState is the answer state of rec on hs. The caller holds w.mu.
func (p *provider) recordState(host string, hs *hostState, rec *opRecord) (providerv1.OperationRecordState, string) {
	if rec.task == "" {
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, ""
	}
	t := p.taskOf(host, hs, rec.task)
	if t == nil {
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_OUTCOME_UNKNOWN, "host_lost_record"
	}
	p.expire(t)
	if !t.done {
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_EXECUTING, ""
	}
	switch t.outcome {
	case providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, ""
	case providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN, t.reason
	case providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY, t.reason
	case providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_OUTCOME_UNKNOWN, t.reason
	}
	return providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED, t.reason
}

// expire ends a stalled task whose step deadline has passed: its outcome is
// unknown. The caller holds w.mu.
func (p *provider) expire(t *task) {
	if t.done || !t.stalled || p.v(StalledRunsForever) || time.Since(t.stallAt) < StepDeadline {
		return
	}
	t.done, t.typed, t.outcome, t.reason = true, true, providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN, stepDeadlineReason
}

// terminalState reports whether s is a terminal state, which a tombstone
// records.
func terminalState(s providerv1.OperationRecordState) bool {
	switch s {
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN,
		providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY, providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED:
		return true
	}
	return false
}

// signedTaskStatus answers a TaskStatus that carries answer_nonce: by op_id,
// with a signed OperationAnswer. The caller holds w.mu.
func (p *provider) signedTaskStatus(req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	if len(req.GetAnswerNonce()) != answer.NonceLen || req.GetTask() != nil || len(req.GetOpId()) != 16 || len(req.GetOpEnrolmentId()) != 16 {
		return nil, status.Error(codes.InvalidArgument, "a signed TaskStatus names the operation by op_id, op_enrolment_id and target_host_id")
	}
	host, hs, err := p.route(req.GetTargetHostId(), false)
	if err != nil {
		return nil, err
	}
	opHex := hex.EncodeToString(req.GetOpId())
	if p.v(AnswersAnotherOp) && hs.answers.lastOp != "" {
		opHex = hs.answers.lastOp
	}
	a := &providerv1.OperationAnswer{Context: p.answerContext(host, hs, req.GetAnswerNonce(), req.GetContinuitySeq()),
		OpId: append([]byte(nil), req.GetOpId()...), OpEnrolmentId: append([]byte(nil), req.GetOpEnrolmentId()...)}
	rec := p.ops(host, hs)[opHex]
	switch {
	case !bytes.Equal(req.GetOpEnrolmentId(), p.ident.enrolments[host]):
		a.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT
	case rec == nil:
		a.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD
	default:
		a.State, a.OutcomeReason = p.recordState(host, hs, rec)
		a.OpId, _ = hex.DecodeString(opHex)
		a.Tombstoned = terminalState(a.State)
		a.Kind, a.Attempt, a.VmUid = int32(rec.kind), rec.attempt, rec.vmUID
		a.BindingId, a.IntentDigest = append([]byte(nil), rec.bindingID...), append([]byte(nil), rec.digest...)
	}
	blob, err := p.sign(host, a)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign: %v", err)
	}
	if rec == nil {
		st := status.New(codes.NotFound, "this host has no record of the operation")
		info, _ := anypb.New(&errdetails.ErrorInfo{Reason: contracts.UnknownToAgentReason, Domain: contracts.ErrorInfoDomain})
		detail, _ := anypb.New(blob)
		pst := st.Proto()
		pst.Details = append(pst.Details, info, detail)
		return nil, status.FromProto(pst).Err()
	}
	t := p.taskOf(host, hs, rec.task)
	resp := &providerv1.TaskStatusResponse{Done: true, Outcome: providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED}
	if t != nil && t.typed {
		resp = p.typedStatus(t)
	}
	resp.HostAnswer = blob
	return resp, nil
}

// signedDescribe answers a Describe that carries answer_nonce: about the
// placement binding_id of the owner's VM, with a signed VMAnswer. The caller
// holds w.mu.
func (p *provider) signedDescribe(req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	if len(req.GetAnswerNonce()) != answer.NonceLen || len(req.GetBindingId()) != 16 || req.GetOwner().GetUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "a signed Describe names the owner and the placement")
	}
	host, hs, err := p.route(req.GetTargetHostId(), false)
	if err != nil {
		return nil, err
	}
	uid, binding := req.GetOwner().GetUid(), req.GetBindingId()
	a := &providerv1.VMAnswer{Context: p.answerContext(host, hs, req.GetAnswerNonce(), req.GetContinuitySeq()), VmUid: uid,
		BindingId: append([]byte(nil), binding...), Presence: providerv1.VMPresence_VM_PRESENCE_NO_RECORD}
	var owned *vm
	for _, v := range hs.vms {
		if v.owner.UID == uid {
			owned = v
		}
	}
	resp := &providerv1.DescribeResponse{}
	switch del, deleted := hs.answers.vmTombs[vmTombKey(uid, binding)]; {
	case deleted:
		a.Presence = providerv1.VMPresence_VM_PRESENCE_DELETED
		a.DeleteOpId, _ = hex.DecodeString(del)
		a.DeleteEnrolmentId, a.DeleteSeq = append([]byte(nil), p.ident.enrolments[host]...), hs.answers.seq
	case owned != nil && bytes.Equal(p.placements.Binding(host, uid), binding):
		a.Presence, resp.Exists, resp.PowerState = providerv1.VMPresence_VM_PRESENCE_PRESENT, true, powerName(owned.running)
	case owned != nil:
		a.Presence, resp.Exists = providerv1.VMPresence_VM_PRESENCE_OTHER_BINDING, true
	case p.v(DeletedWithoutTombstone):
		a.Presence, a.DeleteSeq = providerv1.VMPresence_VM_PRESENCE_DELETED, hs.answers.seq
		a.DeleteOpId, a.DeleteEnrolmentId = make([]byte, 16), append([]byte(nil), p.ident.enrolments[host]...)
	}
	blob, err := p.sign(host, a)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign: %v", err)
	}
	resp.HostAnswer = blob
	return resp, nil
}
