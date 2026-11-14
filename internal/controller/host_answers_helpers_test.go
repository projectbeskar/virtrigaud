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
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// testTrustDomain is the trust domain of the test host identities.
const testTrustDomain = "example.test"

// testHost is a host agent stand-in for the controller tests: a host key and
// certificate under a test host-agent CA, an enrolment, and a store whose
// position advances with every answer. It signs answers exactly as the agent
// does (internal/agent/answer.Sign).
type testHost struct {
	mu          sync.Mutex
	key         *ecdsa.PrivateKey
	cert        []byte
	trust       HostTrust
	providerUID string
	hostID      string
	storeID     [16]byte
	seq         uint64
	state       providerv1.AgentState
}

// newTestHost enrols host hostID of provider (namespace = the Provider's)
// with enrolment id enrol and grant epoch epoch.
func newTestHost(t testing.TB, provider *infravirtrigaudiov1beta1.Provider, hostID string, enrol []byte, epoch operation.Epoch) *testHost {
	t.Helper()
	ca := hostidentitytest.NewCA(t)
	key := hostidentitytest.NewKey(t)
	uri, err := hostidentity.HostURI(testTrustDomain, provider.Namespace, hostID)
	require.NoError(t, err)
	install := bytes.Repeat([]byte{0x22}, 16)
	rec := hostidentity.Record{EnrolmentID: enrol, ProviderUID: string(provider.UID), InstallID: install, Mode: hostidentity.ModeIdentity}
	uris := hostidentity.RequestURIs(uri, rec)
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: uris})
	block, _ := pem.Decode(ca.Issue(t, csr, hostidentitytest.IssueOptions{URIs: uris}))
	pin, err := hostidentity.SPKIPin(&key.PublicKey)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	h := &testHost{key: key, cert: block.Bytes, providerUID: string(provider.UID), hostID: hostID, seq: 1,
		state: providerv1.AgentState_AGENT_STATE_READY,
		trust: HostTrust{Enrolment: operation.Enrolment{EnrolmentID: enrol, Epoch: epoch}, InstallID: install, Pins: []string{pin},
			Anchors: pool, HostURI: uri, HostName: hostID}}
	_, _ = rand.Read(h.storeID[:])
	return h
}

// headAt is the test store's chain head at seq.
func headAt(seq uint64) []byte {
	head := make([]byte, 32)
	binary.BigEndian.PutUint64(head, seq)
	return head
}

// context is the answer context of the next answer for nonce, with the
// store's head at continuitySeq: the store advances by one commit.
func (h *testHost) context(nonce []byte, continuitySeq uint64) *providerv1.AnswerContext {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	pos := append(append(append([]byte(nil), h.storeID[:]...), binary.BigEndian.AppendUint64(nil, h.seq)...), headAt(h.seq)...)
	c := &providerv1.AnswerContext{Nonce: nonce, ProviderUid: h.providerUID, HostId: h.hostID, InstallId: h.trust.InstallID,
		EnrolmentId: h.trust.Enrolment.EnrolmentID, ChainPosition: pos, AgentState: h.state, AgentTime: timestamppb.Now()}
	if continuitySeq > 0 && continuitySeq <= h.seq {
		c.ContinuityHead = headAt(continuitySeq)
	}
	return c
}

// sign signs body.
func (h *testHost) sign(t testing.TB, body proto.Message) *contracts.SignedAnswer {
	blob, err := answer.Sign(h.key, h.cert, body)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return &contracts.SignedAnswer{BodyType: blob.GetBodyType(), Body: blob.GetBody(), Signature: blob.GetSignature(),
		SignerCertificate: blob.GetSignerCertificate()}
}

// rollBack moves the host's store back to an earlier position (a store
// restored with its anchor).
func (h *testHost) rollBack(by uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq -= min(by, h.seq-1)
}

// staticTrust is a HostTrustSource over test hosts, with store states in
// memory.
type staticTrust struct {
	mu     sync.Mutex
	hosts  map[string]*testHost
	stores map[string]HostStoreState
	writes int
}

func (s *staticTrust) HostTrust(_ context.Context, _ *infravirtrigaudiov1beta1.Provider, hostID string) (HostTrust, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hosts[hostID]
	if !ok {
		return HostTrust{}, operation.ErrNoEnrolment
	}
	return h.trust, nil
}

func (s *staticTrust) LoadStoreState(_ context.Context, _ *infravirtrigaudiov1beta1.Provider, hostID string) (HostStoreState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stores[hostID], nil
}

func (s *staticTrust) UpdateStoreState(_ context.Context, _ *infravirtrigaudiov1beta1.Provider, hostID string, update func(*HostStoreState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stores == nil {
		s.stores = map[string]HostStoreState{}
	}
	st := s.stores[hostID]
	update(&st)
	s.stores[hostID] = st
	s.writes++
	return nil
}

// signedHostStub answers the signed reads of a Provider client for one test
// host, from the operations the client accepted: it implements
// contracts.HostAnswerReader. Tests change what it signs through the hooks.
type signedHostStub struct {
	t    testing.TB
	host *testHost

	amu sync.Mutex
	// accepted are the operations the client accepted, by op_id (hex).
	accepted map[string]contracts.PreparedOperation
	// taskOf is the task reference each accepted operation was answered
	// with, by op_id (hex).
	taskOf map[string]string
	// unsigned makes every read carry no signed answer (a Provider that
	// strips them).
	unsigned bool
	// editAnswer, when set, edits every OperationAnswer before it is signed.
	editAnswer func(a *providerv1.OperationAnswer)
	// replayNonce, when set, signs every answer with this nonce instead of
	// the request's.
	replayNonce []byte
	// vmDeleted makes a VMAnswer DELETED by the given op_id.
	vmDeleted []byte
	// answers counts the signed reads.
	answers int
}

func newSignedHostStub(t testing.TB, h *testHost) *signedHostStub {
	return &signedHostStub{t: t, host: h, accepted: map[string]contracts.PreparedOperation{}, taskOf: map[string]string{}}
}

// accept records an operation the client accepted.
func (s *signedHostStub) accept(p contracts.PreparedOperation, taskRef string) {
	s.amu.Lock()
	defer s.amu.Unlock()
	k := hex.EncodeToString(p.Identity.OpID)
	s.accepted[k] = p
	if taskRef != "" {
		s.taskOf[k] = taskRef
	}
}

func drawNonce() []byte {
	n := make([]byte, 16)
	_, _ = rand.Read(n)
	return n
}

// recordStates maps a scripted task outcome to the host's record state.
func recordStateOf(st contracts.TaskStatus) providerv1.OperationRecordState {
	switch st.Outcome {
	case contracts.TaskOutcomeRunning:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_EXECUTING
	case contracts.TaskOutcomeSucceeded:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED
	case contracts.TaskOutcomeFailedClean, contracts.TaskOutcomeCancelled:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN
	case contracts.TaskOutcomeRetired:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED
	case contracts.TaskOutcomeFailedDirty:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY
	case contracts.TaskOutcomeUnknown:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_OUTCOME_UNKNOWN
	}
	switch {
	case !st.IsCompleted:
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_EXECUTING
	case st.Error == "":
		return providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED
	}
	return providerv1.OperationRecordState_OPERATION_RECORD_STATE_OUTCOME_UNKNOWN
}

// operationAnswer answers q from the accepted operations; task scripts the
// state of an accepted operation from its task reference.
func (s *signedHostStub) operationAnswer(q contracts.OperationAnswerQuery,
	task func(ref string) (contracts.TaskStatus, error)) (contracts.TaskStatus, contracts.SignedRead, error) {
	nonce := drawNonce()
	read := contracts.SignedRead{Nonce: nonce}
	s.amu.Lock()
	s.answers++
	k := hex.EncodeToString(q.OpID)
	p, ok := s.accepted[k]
	ref := s.taskOf[k]
	s.amu.Unlock()
	signNonce := nonce
	if s.replayNonce != nil {
		signNonce = s.replayNonce
	}
	a := &providerv1.OperationAnswer{Context: s.host.context(signNonce, q.ContinuitySeq), OpId: q.OpID, OpEnrolmentId: q.OpEnrolmentID}
	var plain contracts.TaskStatus
	var err error
	switch {
	case !bytes.Equal(q.OpEnrolmentID, s.host.trust.Enrolment.EnrolmentID):
		a.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT
		err = contracts.NewNotFoundError("no record", errors.Join(contracts.ErrUnknownToAgent))
	case !ok:
		a.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD
		err = providerErr(codes.NotFound, contracts.UnknownToAgentReason)
	default:
		st := contracts.TaskStatus{Outcome: contracts.TaskOutcomeSucceeded, IsCompleted: true}
		if task != nil && ref != "" {
			var terr error
			st, terr = task(ref)
			if terr != nil {
				if contracts.IsUnknownToAgent(terr) {
					a.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT
					err = terr
					break
				}
				return contracts.TaskStatus{}, read, terr
			}
		}
		plain = st
		a.State, a.OutcomeReason = recordStateOf(st), st.OutcomeReason
		if a.State == providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT {
			break
		}
		wire := wireKinds[p.Kind]
		a.Kind, a.Attempt, a.VmUid, a.BindingId = int32(wire), p.Identity.Attempt, p.Identity.VMUID, p.Identity.BindingID
		a.IntentDigest = operation.IntentDigest(p.DigestSalt, p.RequestBytes)
		switch a.State {
		case providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN,
			providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY, providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED:
			a.Tombstoned = true
		}
	}
	if a.State == providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT ||
		a.State == providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD {
		a.Kind, a.Attempt, a.VmUid, a.BindingId, a.IntentDigest, a.OutcomeReason, a.Tombstoned = 0, 0, "", nil, nil, "", false
	}
	if s.editAnswer != nil {
		s.editAnswer(a)
	}
	if !s.unsigned {
		read.Answer = s.host.sign(s.t, a)
	}
	return plain, read, err
}

// vmAnswer answers a signed Describe.
func (s *signedHostStub) vmAnswer(q contracts.VMAnswerQuery) (contracts.DescribeResponse, contracts.SignedRead, error) {
	nonce := drawNonce()
	read := contracts.SignedRead{Nonce: nonce}
	a := &providerv1.VMAnswer{Context: s.host.context(nonce, q.ContinuitySeq), VmUid: q.VM.Owner.UID, BindingId: q.BindingID,
		Presence: providerv1.VMPresence_VM_PRESENCE_NO_RECORD}
	if s.vmDeleted != nil {
		a.Presence, a.DeleteOpId, a.DeleteEnrolmentId, a.DeleteSeq = providerv1.VMPresence_VM_PRESENCE_DELETED, s.vmDeleted,
			s.host.trust.Enrolment.EnrolmentID, 1
	}
	if !s.unsigned {
		read.Answer = s.host.sign(s.t, a)
	}
	return contracts.DescribeResponse{}, read, nil
}

// hostAnswer answers a signed GetHostInfo.
func (s *signedHostStub) hostAnswer(hostID string, continuitySeq uint64) (contracts.HostInfo, contracts.SignedRead, error) {
	nonce := drawNonce()
	read := contracts.SignedRead{Nonce: nonce}
	a := &providerv1.HostAnswer{Context: s.host.context(nonce, continuitySeq), AgentVersion: "test", AgentApiVersion: 2, GrantVersions: []uint32{1},
		Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY}
	if !s.unsigned {
		read.Answer = s.host.sign(s.t, a)
	}
	return contracts.HostInfo{ID: hostID, Health: contracts.HostHealthReady}, read, nil
}

// withHostAnswers gives r signed host answers for host of provider, through
// the reader stub, and a fresh verified host answer for it.
func withHostAnswers(t testing.TB, r *VirtualMachineReconciler, provider *infravirtrigaudiov1beta1.Provider, h *testHost,
	reader contracts.HostAnswerReader) *staticTrust {
	t.Helper()
	trust := &staticTrust{hosts: map[string]*testHost{h.hostID: h}}
	r.HostAnswers = &HostAnswers{Trust: trust}
	_, _, verdict, err := r.HostAnswers.RefreshHostAnswer(context.Background(), provider, reader, h.hostID)
	require.NoError(t, err)
	require.Equal(t, answerVerified, verdict, "the test host's answer verifies")
	return trust
}

// unreadableTrust is a HostTrustSource whose state cannot be read.
type unreadableTrust struct{}

func (unreadableTrust) HostTrust(context.Context, *infravirtrigaudiov1beta1.Provider, string) (HostTrust, error) {
	return HostTrust{}, errors.New("the pins Secret cannot be read")
}

func (unreadableTrust) LoadStoreState(context.Context, *infravirtrigaudiov1beta1.Provider, string) (HostStoreState, error) {
	return HostStoreState{}, errors.New("the pins Secret cannot be read")
}

func (unreadableTrust) UpdateStoreState(context.Context, *infravirtrigaudiov1beta1.Provider, string, func(*HostStoreState)) error {
	return errors.New("the pins Secret cannot be read")
}
