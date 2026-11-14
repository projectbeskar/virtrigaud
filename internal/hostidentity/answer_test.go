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

package hostidentity_test

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// answerFixture is a host PKI, the manager's expectation of one request, and
// an answer body that meets it.
type answerFixture struct {
	pki   *trusttest.PKI
	cert  []byte
	nonce []byte
	pin   string
}

func newAnswerFixture(t *testing.T) *answerFixture {
	t.Helper()
	p := trusttest.New(t)
	block, _ := pem.Decode(p.HostCertPEM)
	pin, err := hostidentity.SPKIPin(&p.HostKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	n := make([]byte, 16)
	_, _ = rand.Read(n)
	return &answerFixture{pki: p, cert: block.Bytes, nonce: n, pin: pin}
}

func (f *answerFixture) exp(bodyType string) hostidentity.AnswerExpectation {
	return hostidentity.AnswerExpectation{
		BodyType: bodyType, Anchors: f.pki.HostCAPool(), HostURI: f.pki.HostURI, HostName: trusttest.HostName,
		HostID: trusttest.HostName, Pins: []string{f.pin}, ProviderUID: trusttest.ProviderUID,
		InstallID: f.pki.Record.InstallID, EnrolmentID: f.pki.Record.EnrolmentID, Nonce: f.nonce, Now: time.Now(),
	}
}

func position(seq uint64) []byte {
	out := bytes.Repeat([]byte{0xab}, 16)
	out = binary.BigEndian.AppendUint64(out, seq)
	return append(out, bytes.Repeat([]byte{0xcd}, 32)...)
}

func (f *answerFixture) ctx() *providerv1.AnswerContext {
	return &providerv1.AnswerContext{
		Nonce: f.nonce, ProviderUid: trusttest.ProviderUID, HostId: trusttest.HostName, InstallId: f.pki.Record.InstallID,
		EnrolmentId: f.pki.Record.EnrolmentID, ChainPosition: position(7), AgentState: providerv1.AgentState_AGENT_STATE_READY,
		AgentTime: timestamppb.Now(),
	}
}

func (f *answerFixture) opAnswer() *providerv1.OperationAnswer {
	return &providerv1.OperationAnswer{
		Context: f.ctx(), OpId: bytes.Repeat([]byte{1}, 16), OpEnrolmentId: f.pki.Record.EnrolmentID,
		State: providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, Tombstoned: true,
		Kind: int32(providerv1.OperationKind_OPERATION_KIND_CREATE), Attempt: 1, VmUid: "vm-uid-1",
		BindingId: bytes.Repeat([]byte{2}, 16), IntentDigest: bytes.Repeat([]byte{3}, 32),
	}
}

func (f *answerFixture) sign(t *testing.T, body proto.Message) *providerv1.SignedBlob {
	t.Helper()
	blob, err := answer.Sign(f.pki.HostKey, f.cert, body)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// signRaw signs raw body bytes of bodyType with key, as a host would, so a
// test can sign bytes the agent never builds.
func signRaw(t *testing.T, key *ecdsa.PrivateKey, cert []byte, bodyType string, body []byte) *providerv1.SignedBlob {
	t.Helper()
	m, err := hostidentity.AnswerSignatureInput(bodyType, body)
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(m)
	sig, err := key.Sign(rand.Reader, d[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if sig, err = grantverify.NormalizeLowS(sig, elliptic.P256()); err != nil {
		t.Fatal(err)
	}
	return &providerv1.SignedBlob{Body: body, Signature: sig, SignerCertificate: cert, BodyType: bodyType}
}

// TestAnswerSignatureInputVector reproduces the published framing vector.
func TestAnswerSignatureInputVector(t *testing.T) {
	body, _ := hex.DecodeString("120676302e302e31")
	m, err := hostidentity.AnswerSignatureInput(hostidentity.BodyTypeHostAnswer, body)
	if err != nil {
		t.Fatal(err)
	}
	if d := sha256.Sum256(m); hex.EncodeToString(d[:]) != "92b2ed124ec68b2661cef2f6188a1f937121db3f0d25bb0ae8b98319bd696cea" || len(m) != 62 {
		t.Fatalf("M = %x", m)
	}
	agentM, err := answer.SignatureInput(hostidentity.BodyTypeHostAnswer, body)
	if err != nil || !bytes.Equal(agentM, m) {
		t.Fatal("the agent frames the signature input differently from the verifier")
	}
}

// TestVerifyAnswerAcceptsAgentAnswers: what the agent signs verifies, for
// each body type.
func TestVerifyAnswerAcceptsAgentAnswers(t *testing.T) {
	f := newAnswerFixture(t)
	v, err := hostidentity.VerifyAnswer(f.sign(t, f.opAnswer()), f.exp(hostidentity.BodyTypeOperationAnswer))
	if err != nil {
		t.Fatal(err)
	}
	if v.Operation == nil || v.Position.Seq != 7 || !v.Ready() || v.SignerPin != f.pin {
		t.Fatalf("verified answer %+v", v)
	}
	vm := &providerv1.VMAnswer{Context: f.ctx(), VmUid: "vm-uid-1", BindingId: bytes.Repeat([]byte{2}, 16),
		Presence: providerv1.VMPresence_VM_PRESENCE_DELETED, DeleteOpId: bytes.Repeat([]byte{4}, 16),
		DeleteEnrolmentId: f.pki.Record.EnrolmentID, DeleteSeq: 6}
	if _, err := hostidentity.VerifyAnswer(f.sign(t, vm), f.exp(hostidentity.BodyTypeVMAnswer)); err != nil {
		t.Fatal(err)
	}
	ha := &providerv1.HostAnswer{Context: f.ctx(), AgentVersion: "v0.0.1", AgentApiVersion: 2, GrantVersions: []uint32{1},
		Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY}
	if _, err := hostidentity.VerifyAnswer(f.sign(t, ha), f.exp(hostidentity.BodyTypeHostAnswer)); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyAnswerRefuses: every check fails closed to "no answer".
func TestVerifyAnswerRefuses(t *testing.T) {
	f := newAnswerFixture(t)
	opType := hostidentity.BodyTypeOperationAnswer
	other := hostidentitytest.NewKey(t)
	otherCertPEM := f.pki.IssueHost(t, other, hostidentity.RequestURIs(f.pki.HostURI, f.pki.Record), hostidentitytest.IssueOptions{})
	otherBlock, _ := pem.Decode(otherCertPEM)
	foreignCA := hostidentitytest.NewCA(t)
	foreignCSR := hostidentitytest.NewCSR(t, f.pki.HostKey, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(f.pki.HostURI, f.pki.Record)})
	foreignBlock, _ := pem.Decode(foreignCA.Issue(t, foreignCSR, hostidentitytest.IssueOptions{}))
	otherHostURI, _ := hostidentity.HostURI(trusttest.TrustDomain, trusttest.HostNS, "host-b")
	wrongHostBlock, _ := pem.Decode(f.pki.IssueHost(t, f.pki.HostKey, hostidentity.RequestURIs(otherHostURI, f.pki.Record),
		hostidentitytest.IssueOptions{}))

	cases := map[string]struct {
		blob func() *providerv1.SignedBlob
		exp  func(e *hostidentity.AnswerExpectation)
	}{
		"stripped": {blob: func() *providerv1.SignedBlob { return nil }},
		"replayed nonce": {blob: func() *providerv1.SignedBlob { return f.sign(t, f.opAnswer()) },
			exp: func(e *hostidentity.AnswerExpectation) { e.Nonce = bytes.Repeat([]byte{9}, 16) }},
		"wrong body type for the RPC": {blob: func() *providerv1.SignedBlob { return f.sign(t, f.opAnswer()) },
			exp: func(e *hostidentity.AnswerExpectation) { e.BodyType = hostidentity.BodyTypeVMAnswer }},
		"body type outside the set": {blob: func() *providerv1.SignedBlob {
			raw, _ := proto.Marshal(f.opAnswer())
			return signRaw(t, f.pki.HostKey, f.cert, "provider.v1.TaskStatusResponse", raw)
		}, exp: func(e *hostidentity.AnswerExpectation) { e.BodyType = "provider.v1.TaskStatusResponse" }},
		"other Provider": {blob: func() *providerv1.SignedBlob { return f.sign(t, f.opAnswer()) },
			exp: func(e *hostidentity.AnswerExpectation) { e.ProviderUID = "0b7a1c3e-1111-4222-8333-000000000000" }},
		"other host": {blob: func() *providerv1.SignedBlob { return f.sign(t, f.opAnswer()) },
			exp: func(e *hostidentity.AnswerExpectation) { e.HostID = "host-b" }},
		"other enrolment": {blob: func() *providerv1.SignedBlob { return f.sign(t, f.opAnswer()) },
			exp: func(e *hostidentity.AnswerExpectation) { e.EnrolmentID = bytes.Repeat([]byte{7}, 16) }},
		"other installation": {blob: func() *providerv1.SignedBlob { return f.sign(t, f.opAnswer()) },
			exp: func(e *hostidentity.AnswerExpectation) { e.InstallID = bytes.Repeat([]byte{7}, 16) }},
		"key not pinned": {blob: func() *providerv1.SignedBlob {
			b, _ := answer.Sign(other, otherBlock.Bytes, f.opAnswer())
			return b
		}},
		"foreign CA": {blob: func() *providerv1.SignedBlob {
			b, _ := answer.Sign(f.pki.HostKey, foreignBlock.Bytes, f.opAnswer())
			return b
		}},
		"certificate of another host": {blob: func() *providerv1.SignedBlob {
			b, _ := answer.Sign(f.pki.HostKey, wrongHostBlock.Bytes, f.opAnswer())
			return b
		}},
		"expired at the manager's clock": {blob: func() *providerv1.SignedBlob { return f.sign(t, f.opAnswer()) },
			exp: func(e *hostidentity.AnswerExpectation) { e.Now = time.Now().Add(400 * 24 * time.Hour) }},
		"no anchors": {blob: func() *providerv1.SignedBlob { return f.sign(t, f.opAnswer()) },
			exp: func(e *hostidentity.AnswerExpectation) { e.Anchors = nil }},
		"tampered body": {blob: func() *providerv1.SignedBlob {
			b := f.sign(t, f.opAnswer())
			b.Body[len(b.Body)-1] ^= 1
			return b
		}},
		"high-S signature": {blob: func() *providerv1.SignedBlob {
			b := f.sign(t, f.opAnswer())
			s, _ := grantverify.ParseECDSASignature(b.Signature)
			s.S = new(big.Int).Sub(elliptic.P256().Params().N, s.S)
			b.Signature, _ = asn1.Marshal(s)
			return b
		}},
		"trailing bytes after the signature": {blob: func() *providerv1.SignedBlob {
			b := f.sign(t, f.opAnswer())
			b.Signature = append(b.Signature, 0)
			return b
		}},
		"unknown field in the signed blob": {blob: func() *providerv1.SignedBlob {
			b := f.sign(t, f.opAnswer())
			b.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
			return b
		}},
		"unknown field in the body": {blob: func() *providerv1.SignedBlob {
			raw, _ := proto.Marshal(f.opAnswer())
			raw = protowire.AppendVarint(protowire.AppendTag(raw, 99, protowire.VarintType), 1)
			return signRaw(t, f.pki.HostKey, f.cert, opType, raw)
		}},
		"unknown field in the context": {blob: func() *providerv1.SignedBlob {
			a := f.opAnswer()
			a.Context.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
			raw, _ := proto.Marshal(a)
			return signRaw(t, f.pki.HostKey, f.cert, opType, raw)
		}},
		"undefined state": {blob: func() *providerv1.SignedBlob {
			a := f.opAnswer()
			a.State = 42
			raw, _ := proto.Marshal(a)
			return signRaw(t, f.pki.HostKey, f.cert, opType, raw)
		}},
		"no record with record fields": {blob: func() *providerv1.SignedBlob {
			a := f.opAnswer()
			a.State, a.Tombstoned = providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD, false
			raw, _ := proto.Marshal(a)
			return signRaw(t, f.pki.HostKey, f.cert, opType, raw)
		}},
		"tombstone of a running operation": {blob: func() *providerv1.SignedBlob {
			a := f.opAnswer()
			a.State = providerv1.OperationRecordState_OPERATION_RECORD_STATE_EXECUTING
			raw, _ := proto.Marshal(a)
			return signRaw(t, f.pki.HostKey, f.cert, opType, raw)
		}},
		"quarantined without a reason": {blob: func() *providerv1.SignedBlob {
			a := f.opAnswer()
			a.Context.AgentState = providerv1.AgentState_AGENT_STATE_QUARANTINED
			raw, _ := proto.Marshal(a)
			return signRaw(t, f.pki.HostKey, f.cert, opType, raw)
		}},
		"no chain position": {blob: func() *providerv1.SignedBlob {
			a := f.opAnswer()
			a.Context.ChainPosition = position(0)
			raw, _ := proto.Marshal(a)
			return signRaw(t, f.pki.HostKey, f.cert, opType, raw)
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := f.exp(opType)
			if tc.exp != nil {
				tc.exp(&e)
			}
			if _, err := hostidentity.VerifyAnswer(tc.blob(), e); !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("VerifyAnswer = %v, want ErrAnswer", err)
			}
		})
	}
}

// TestVerifyHostAnswerRules: the HostAnswer field rules.
func TestVerifyHostAnswerRules(t *testing.T) {
	f := newAnswerFixture(t)
	base := func() *providerv1.HostAnswer {
		return &providerv1.HostAnswer{Context: f.ctx(), AgentApiVersion: 2, GrantVersions: []uint32{1, 2},
			Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY}
	}
	for name, edit := range map[string]func(*providerv1.HostAnswer){
		"no grant version":         func(a *providerv1.HostAnswer) { a.GrantVersions = nil },
		"grant versions unordered": func(a *providerv1.HostAnswer) { a.GrantVersions = []uint32{2, 1} },
		"grant version 0":          func(a *providerv1.HostAnswer) { a.GrantVersions = []uint32{0, 1} },
		"unspecified mode":         func(a *providerv1.HostAnswer) { a.Mode = providerv1.AgentMode_AGENT_MODE_UNSPECIFIED },
		"inventory not asked for":  func(a *providerv1.HostAnswer) { a.Inventory = &providerv1.HostInventory{} },
	} {
		t.Run(name, func(t *testing.T) {
			a := base()
			edit(a)
			raw, _ := proto.Marshal(a)
			blob := signRaw(t, f.pki.HostKey, f.cert, hostidentity.BodyTypeHostAnswer, raw)
			if _, err := hostidentity.VerifyAnswer(blob, f.exp(hostidentity.BodyTypeHostAnswer)); !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("VerifyAnswer = %v, want ErrAnswer", err)
			}
		})
	}
}
