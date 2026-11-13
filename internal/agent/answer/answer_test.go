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

package answer_test

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestSignatureInputVector reproduces the published test vector of the
// signature input framing.
func TestSignatureInputVector(t *testing.T) {
	body, _ := hex.DecodeString("120676302e302e31")
	m, err := answer.SignatureInput(answer.BodyTypeHostAnswer, body)
	if err != nil {
		t.Fatal(err)
	}
	want := "766972747269676175642e686f73742d7369676e65642e763100" + "0016" +
		"70726f76696465722e76312e486f7374416e73776572" + "00000008" + "120676302e302e31"
	if hex.EncodeToString(m) != want || len(m) != 62 {
		t.Fatalf("M = %x (%d bytes)", m, len(m))
	}
	if d := sha256.Sum256(m); hex.EncodeToString(d[:]) != "92b2ed124ec68b2661cef2f6188a1f937121db3f0d25bb0ae8b98319bd696cea" {
		t.Fatalf("SHA-256(M) = %x", d)
	}
	if m[0] != 0x76 {
		t.Fatal("first byte is not 0x76")
	}
	if err := proto.Unmarshal(m, &providerv1.HostAnswer{}); err == nil {
		t.Fatal("the signature input parses as a protobuf message")
	}
}

func leafDER(t *testing.T, p *trusttest.PKI) []byte {
	t.Helper()
	block, _ := pem.Decode(p.HostCertPEM)
	return block.Bytes
}

// verify checks a SignedBlob the way a verifier does.
func verify(t *testing.T, blob *providerv1.SignedBlob, msg proto.Message) {
	t.Helper()
	cert, err := x509.ParseCertificate(blob.GetSignerCertificate())
	if err != nil {
		t.Fatal(err)
	}
	m, err := answer.SignatureInput(blob.GetBodyType(), blob.GetBody())
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(m)
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatal("signer key is not ECDSA")
	}
	if !ecdsa.VerifyASN1(pub, d[:], blob.GetSignature()) {
		t.Fatal("signature does not verify")
	}
	if !grantverify.IsLowS(blob.GetSignature(), elliptic.P256()) {
		t.Fatal("signature is not low-S")
	}
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(blob.GetBody(), msg); err != nil {
		t.Fatal(err)
	}
}

// highSSigner returns high-S signatures, to prove Sign normalises.
type highSSigner struct{ k *ecdsa.PrivateKey }

func (h highSSigner) Public() crypto.PublicKey { return h.k.Public() }

func (h highSSigner) Sign(r io.Reader, d []byte, o crypto.SignerOpts) ([]byte, error) {
	sig, err := h.k.Sign(r, d, o)
	if err != nil {
		return nil, err
	}
	s, err := grantverify.ParseECDSASignature(sig)
	if err != nil {
		return nil, err
	}
	n := elliptic.P256().Params().N
	if s.S.Cmp(new(bigInt).Rsh(n, 1)) <= 0 {
		s.S = new(bigInt).Sub(n, s.S)
	}
	return marshalSig(s)
}

func TestSign(t *testing.T) {
	p := trusttest.New(t)
	body := &providerv1.HostAnswer{AgentVersion: "test-build", AgentApiVersion: 1, GrantVersions: []uint32{1}}
	for i := range 8 {
		blob, err := answer.Sign(highSSigner{p.HostKey}, leafDER(t, p), body)
		if err != nil {
			t.Fatalf("sign %d: %v", i, err)
		}
		if blob.GetBodyType() != answer.BodyTypeHostAnswer {
			t.Fatalf("body type %q", blob.GetBodyType())
		}
		got := &providerv1.HostAnswer{}
		verify(t, blob, got)
		if !proto.Equal(got, body) {
			t.Fatal("body round trip differs")
		}
	}
	// Only the closed set of body types is signed.
	if _, err := answer.Sign(p.HostKey, leafDER(t, p), &providerv1.GrantBody{}); !errors.Is(err, answer.ErrSign) {
		t.Fatalf("grant body signed: %v", err)
	}
	// The signer must be the certificate's key.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), randReader)
	if _, err := answer.Sign(other, leafDER(t, p), body); !errors.Is(err, answer.ErrSign) {
		t.Fatalf("foreign key signed: %v", err)
	}
	// Oversized body.
	big := &providerv1.HostAnswer{AgentVersion: string(bytes.Repeat([]byte("x"), answer.MaxBodyBytes))}
	if _, err := answer.Sign(p.HostKey, leafDER(t, p), big); !errors.Is(err, answer.ErrSign) {
		t.Fatalf("oversized body signed: %v", err)
	}
}

func testView() opstore.HostView {
	v := opstore.HostView{
		Enrolment: &opstore.ActiveEnrolment{ProviderUID: trusttest.ProviderUID, HostID: trusttest.HostName, StartEpoch: 1},
		Position:  journal.Position{Seq: 7},
	}
	v.Position.StoreID[0], v.Position.Head[0] = 1, 2
	return v
}

func testHost(p *trusttest.PKI) answer.Host {
	v := testView()
	v.Enrolment.ID, v.Enrolment.InstallID = p.Record.EnrolmentID, p.Record.InstallID
	return answer.Host{
		Nonce: bytes.Repeat([]byte{9}, 16), View: v, HostID: trusttest.HostName, Record: p.Record,
		State: providerv1.AgentState_AGENT_STATE_READY, AgentVersion: "test", JournalSchemaVersion: 1,
		ClockFloor: time.Unix(1700000000, 0), Now: time.Now(),
	}
}

func TestHostAnswer(t *testing.T) {
	p := trusttest.New(t)
	h := testHost(p)
	a, err := answer.HostAnswer(h)
	if err != nil {
		t.Fatal(err)
	}
	c := a.GetContext()
	pos, _ := h.View.Position.MarshalBinary()
	if !bytes.Equal(c.GetNonce(), h.Nonce) || c.GetProviderUid() != trusttest.ProviderUID || c.GetHostId() != trusttest.HostName ||
		!bytes.Equal(c.GetEnrolmentId(), p.Record.EnrolmentID) || !bytes.Equal(c.GetInstallId(), p.Record.InstallID) ||
		!bytes.Equal(c.GetChainPosition(), pos) || len(c.GetChainPosition()) != 56 ||
		c.GetAgentState() != providerv1.AgentState_AGENT_STATE_READY || c.GetCheckpointSeq() != 0 || c.GetCheckpointHead() != nil {
		t.Fatalf("context = %v", c)
	}
	if a.GetMode() != providerv1.AgentMode_AGENT_MODE_IDENTITY || a.GetAgentApiVersion() != answer.AgentAPIVersion ||
		len(a.GetGrantVersions()) != 1 || a.GetGrantVersions()[0] != 1 || a.GetClockFloor().AsTime().Unix() != 1700000000 {
		t.Fatalf("answer = %v", a)
	}
	blob, err := answer.Sign(p.HostKey, leafDER(t, p), a)
	if err != nil {
		t.Fatal(err)
	}
	verify(t, blob, &providerv1.HostAnswer{})

	for name, edit := range map[string]func(h *answer.Host){
		"no nonce":             func(h *answer.Host) { h.Nonce = nil },
		"short nonce":          func(h *answer.Host) { h.Nonce = h.Nonce[:15] },
		"no enrolment":         func(h *answer.Host) { h.View.Enrolment = nil },
		"other enrolment":      func(h *answer.Host) { h.View.Enrolment.ID = bytes.Repeat([]byte{1}, 16) },
		"other provider":       func(h *answer.Host) { h.View.Enrolment.ProviderUID = "other" },
		"other installation":   func(h *answer.Host) { h.View.Enrolment.InstallID = bytes.Repeat([]byte{1}, 16) },
		"other host":           func(h *answer.Host) { h.HostID = "host-b" },
		"unsupported mode":     func(h *answer.Host) { h.Record.Mode = "fencing" },
		"empty store position": func(h *answer.Host) { h.View.Position = journal.Position{} },
	} {
		h := testHost(p)
		edit(&h)
		if _, err := answer.HostAnswer(h); !errors.Is(err, answer.ErrNoAnswer) {
			t.Errorf("%s: err = %v, want ErrNoAnswer", name, err)
		}
	}
	for name, edit := range map[string]func(h *answer.Host){
		"quarantine without a loss reason": func(h *answer.Host) { h.State = providerv1.AgentState_AGENT_STATE_QUARANTINED },
		"quarantine with another reason": func(h *answer.Host) {
			h.State, h.StateReason = providerv1.AgentState_AGENT_STATE_QUARANTINED, "something_else"
		},
		"reason without a quarantine": func(h *answer.Host) { h.StateReason = string(journal.LostIOError) },
		"unspecified state":           func(h *answer.Host) { h.State = providerv1.AgentState_AGENT_STATE_UNSPECIFIED },
	} {
		h := testHost(p)
		edit(&h)
		if _, err := answer.HostAnswer(h); err == nil || errors.Is(err, answer.ErrNoAnswer) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
	h = testHost(p)
	h.State, h.StateReason = providerv1.AgentState_AGENT_STATE_QUARANTINED, string(journal.LostRolledBack)
	if a, err := answer.HostAnswer(h); err != nil || a.GetContext().GetStateReason() != "rolled_back" {
		t.Fatalf("quarantined answer: %v, %v", a, err)
	}
	h = testHost(p)
	h.View.CheckpointSeq, h.View.CheckpointHead[0] = 4, 3
	if a, err := answer.HostAnswer(h); err != nil || len(a.GetContext().GetCheckpointHead()) != 32 {
		t.Fatalf("checkpoint: %v, %v", a, err)
	}
}

func TestStoreState(t *testing.T) {
	for _, tc := range []struct {
		st        journal.State
		lost      journal.LostReason
		recovered bool
		want      providerv1.AgentState
	}{
		{journal.StateUnattested, "", false, providerv1.AgentState_AGENT_STATE_UNATTESTED},
		{journal.StateReady, "", false, providerv1.AgentState_AGENT_STATE_UNATTESTED},
		{journal.StateReady, "", true, providerv1.AgentState_AGENT_STATE_READY},
		{journal.StateLost, journal.LostIOError, true, providerv1.AgentState_AGENT_STATE_QUARANTINED},
	} {
		got, reason := answer.StoreState(tc.st, tc.lost, tc.recovered)
		if got != tc.want || (got == providerv1.AgentState_AGENT_STATE_QUARANTINED) != (reason != "") {
			t.Errorf("%v/%v: %v %q", tc.st, tc.recovered, got, reason)
		}
	}
}
