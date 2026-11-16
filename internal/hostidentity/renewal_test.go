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
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// renewalFixture is an answer fixture plus a new key and its certificate
// request carrying the current certificate's URIs.
type renewalFixture struct {
	*answerFixture
	newKey *ecdsa.PrivateKey
	csrDER []byte
}

func newRenewalFixture(t *testing.T) *renewalFixture {
	t.Helper()
	f := &renewalFixture{answerFixture: newAnswerFixture(t), newKey: hostidentitytest.NewKey(t)}
	f.csrDER = f.csr(t, f.newKey, hostidentity.RequestURIs(f.pki.HostURI, f.pki.Record))
	return f
}

func (f *renewalFixture) csr(t *testing.T, key *ecdsa.PrivateKey, uris []string) []byte {
	t.Helper()
	block, _ := pem.Decode(hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{CommonName: trusttest.HostName, URIs: uris}))
	return block.Bytes
}

func (f *renewalFixture) body(t *testing.T) *providerv1.RenewalRequest {
	t.Helper()
	b, err := answer.RenewalRequest(answer.Renewal{Requested: 1, HostID: trusttest.HostName, Record: f.pki.Record,
		CertificateDER: f.cert, CSRDER: f.csrDER})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *renewalFixture) renewalExp() hostidentity.RenewalExpectation {
	return hostidentity.RenewalExpectation{
		Anchors: f.pki.HostCAPool(), HostURI: f.pki.HostURI, HostName: trusttest.HostName, HostID: trusttest.HostName,
		Pins: []string{f.pin}, ProviderUID: trusttest.ProviderUID, InstallID: f.pki.Record.InstallID,
		EnrolmentID: f.pki.Record.EnrolmentID, Version: 1, Now: time.Now(),
	}
}

func (f *renewalFixture) signBody(t *testing.T, body *providerv1.RenewalRequest) *providerv1.SignedBlob {
	t.Helper()
	blob, err := answer.SignRenewalRequest(f.pki.HostKey, f.cert, body)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func TestVerifyRenewalAccepts(t *testing.T) {
	f := newRenewalFixture(t)
	v, err := hostidentity.VerifyRenewal(f.signBody(t, f.body(t)), f.renewalExp())
	if err != nil {
		t.Fatalf("VerifyRenewal: %v", err)
	}
	wantPin, _ := hostidentity.SPKIPin(&f.newKey.PublicKey)
	if v.KeyPin != wantPin || v.SignerPin != f.pin || !bytes.Equal(v.CSR, f.csrDER) || !v.Record.Equal(f.pki.Record) {
		t.Fatalf("verified renewal = %+v", v)
	}
	var back providerv1.SignedBlob
	if err := proto.Unmarshal(v.Blob, &back); err != nil || back.GetBodyType() != hostidentity.BodyTypeRenewalRequest {
		t.Fatalf("stored blob does not decode: %v", err)
	}
	// It verifies again from its stored bytes.
	if _, err := hostidentity.VerifyRenewal(&back, f.renewalExp()); err != nil {
		t.Fatalf("stored blob does not verify again: %v", err)
	}
}

func TestVerifyRenewalRefuses(t *testing.T) {
	f := newRenewalFixture(t)
	otherKey := hostidentitytest.NewKey(t)
	otherCert := f.pki.IssueHost(t, otherKey, hostidentity.RequestURIs(f.pki.HostURI, f.pki.Record), hostidentitytest.IssueOptions{})
	otherBlock, _ := pem.Decode(otherCert)
	otherRec := f.pki.Record
	otherRec.EnrolmentID = bytes.Repeat([]byte{0x77}, 16)
	epochRec := f.pki.Record
	epochRec.StartEpoch = 9
	expired := f.pki.IssueHost(t, f.pki.HostKey, hostidentity.RequestURIs(f.pki.HostURI, f.pki.Record),
		hostidentitytest.IssueOptions{Lifetime: time.Second})
	expiredBlock, _ := pem.Decode(expired)

	cases := []struct {
		name string
		blob func() *providerv1.SignedBlob
		exp  func(*hostidentity.RenewalExpectation)
	}{
		{"wrong body type", func() *providerv1.SignedBlob {
			b := f.signBody(t, f.body(t))
			b.BodyType = hostidentity.BodyTypeHostAnswer
			return b
		}, nil},
		{"not asked for", func() *providerv1.SignedBlob { return f.signBody(t, f.body(t)) },
			func(e *hostidentity.RenewalExpectation) { e.Version = 0 }},
		{"co-signed by an unpinned key", func() *providerv1.SignedBlob {
			raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(f.body(t))
			return signRaw(t, otherKey, otherBlock.Bytes, hostidentity.BodyTypeRenewalRequest, raw)
		}, nil},
		{"co-signature by another key than the certificate's", func() *providerv1.SignedBlob {
			raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(f.body(t))
			return signRaw(t, otherKey, f.cert, hostidentity.BodyTypeRenewalRequest, raw)
		}, nil},
		{"current certificate expired", func() *providerv1.SignedBlob {
			b := f.body(t)
			sum := sha256.Sum256(expiredBlock.Bytes)
			b.CurrentCertificateSha256 = sum[:]
			raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(b)
			return signRaw(t, f.pki.HostKey, expiredBlock.Bytes, hostidentity.BodyTypeRenewalRequest, raw)
		}, func(e *hostidentity.RenewalExpectation) { e.Now = time.Now().Add(time.Hour) }},
		{"tampered body", func() *providerv1.SignedBlob {
			b := f.signBody(t, f.body(t))
			b.Body = append(bytes.Clone(b.Body[:len(b.Body)-1]), b.Body[len(b.Body)-1]^1)
			return b
		}, nil},
		{"unknown field in the body", func() *providerv1.SignedBlob {
			raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(f.body(t))
			raw = protowire.AppendVarint(protowire.AppendTag(raw, 99, protowire.VarintType), 1)
			return signRaw(t, f.pki.HostKey, f.cert, hostidentity.BodyTypeRenewalRequest, raw)
		}, nil},
		{"other enrolment", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.EnrolmentId = otherRec.EnrolmentID
			return f.signBody(t, b)
		}, nil},
		{"other host", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.HostId = "host-b"
			return f.signBody(t, b)
		}, nil},
		{"other Provider", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.ProviderUid = "0b7a1c3e-1111-4222-8333-000000000000"
			return f.signBody(t, b)
		}, nil},
		{"wrong certificate digest", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.CurrentCertificateSha256 = bytes.Repeat([]byte{1}, 32)
			return f.signBody(t, b)
		}, nil},
		{"wrong version", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.RenewalVersion = 2
			return f.signBody(t, b)
		}, nil},
		{"changed SANs: other enrolment record", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.CertificateRequest = f.csr(t, f.newKey, hostidentity.RequestURIs(f.pki.HostURI, otherRec))
			return f.signBody(t, b)
		}, nil},
		{"changed SANs: other epoch", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.CertificateRequest = f.csr(t, f.newKey, hostidentity.RequestURIs(f.pki.HostURI, epochRec))
			return f.signBody(t, b)
		}, nil},
		{"changed SANs: other host URI", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.CertificateRequest = f.csr(t, f.newKey, hostidentity.RequestURIs(strings.Replace(f.pki.HostURI, "host-a", "host-b", 1), f.pki.Record))
			return f.signBody(t, b)
		}, nil},
		{"broken proof of possession", func() *providerv1.SignedBlob {
			b := f.body(t)
			csr := bytes.Clone(f.csrDER)
			csr[len(csr)-3] ^= 1
			b.CertificateRequest = csr
			return f.signBody(t, b)
		}, nil},
		{"same key as the current one", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.CertificateRequest = f.csr(t, f.pki.HostKey, hostidentity.RequestURIs(f.pki.HostURI, f.pki.Record))
			return f.signBody(t, b)
		}, nil},
		{"trailing bytes after the request", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.CertificateRequest = append(bytes.Clone(f.csrDER), 0)
			return f.signBody(t, b)
		}, nil},
		{"request too large", func() *providerv1.SignedBlob {
			b := f.body(t)
			b.CertificateRequest = bytes.Repeat([]byte{0x30}, hostidentity.MaxRenewalCSRBytes+1)
			raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(b)
			return signRaw(t, f.pki.HostKey, f.cert, hostidentity.BodyTypeRenewalRequest, raw)
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := f.renewalExp()
			if c.exp != nil {
				c.exp(&exp)
			}
			if _, err := hostidentity.VerifyRenewal(c.blob(), exp); !errors.Is(err, hostidentity.ErrRenewal) {
				t.Fatalf("VerifyRenewal = %v, want ErrRenewal", err)
			}
		})
	}
}

// TestVerifyAnswerPendingRenewal checks the pending renewal inside a host
// answer: verified only when asked for, and only when signed with the
// answer's own certificate.
func TestVerifyAnswerPendingRenewal(t *testing.T) {
	f := newRenewalFixture(t)
	hostAnswer := func(pr *providerv1.SignedBlob) *providerv1.HostAnswer {
		return &providerv1.HostAnswer{Context: f.ctx(), AgentApiVersion: answer.AgentAPIVersion, GrantVersions: []uint32{1},
			Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY, PendingRenewal: pr}
	}
	exp := f.exp(hostidentity.BodyTypeHostAnswer)
	exp.RenewalVersion = 1
	v, err := hostidentity.VerifyAnswer(f.sign(t, hostAnswer(f.signBody(t, f.body(t)))), exp)
	if err != nil || v.Renewal == nil {
		t.Fatalf("VerifyAnswer = %v, renewal %v", err, v.Renewal)
	}
	if v, err := hostidentity.VerifyAnswer(f.sign(t, hostAnswer(nil)), exp); err != nil || v.Renewal != nil {
		t.Fatalf("an answer without a renewal: %v, %v", err, v.Renewal)
	}

	// Not asked for: the whole answer is refused.
	notAsked := f.exp(hostidentity.BodyTypeHostAnswer)
	if _, err := hostidentity.VerifyAnswer(f.sign(t, hostAnswer(f.signBody(t, f.body(t)))), notAsked); !errors.Is(err, hostidentity.ErrAnswer) {
		t.Fatalf("unasked renewal: %v", err)
	}
	// Asked for a version above the implemented one.
	above := exp
	above.RenewalVersion = hostidentity.RenewalVersion + 1
	if _, err := hostidentity.VerifyAnswer(f.sign(t, hostAnswer(f.signBody(t, f.body(t)))), above); !errors.Is(err, hostidentity.ErrAnswer) {
		t.Fatalf("renewal version above: %v", err)
	}
	// Co-signed by another certificate of the same host and enrolment (a
	// pinned key, but not the answer's).
	otherKey := hostidentitytest.NewKey(t)
	otherCert := f.pki.IssueHost(t, otherKey, hostidentity.RequestURIs(f.pki.HostURI, f.pki.Record), hostidentitytest.IssueOptions{})
	ob, _ := pem.Decode(otherCert)
	otherPin, _ := hostidentity.SPKIPin(&otherKey.PublicKey)
	body := f.body(t)
	sum := sha256.Sum256(ob.Bytes)
	body.CurrentCertificateSha256 = sum[:]
	other, err := answer.SignRenewalRequest(otherKey, ob.Bytes, body)
	if err != nil {
		t.Fatal(err)
	}
	both := exp
	both.Pins = []string{f.pin, otherPin}
	if _, err := hostidentity.VerifyAnswer(f.sign(t, hostAnswer(other)), both); !errors.Is(err, hostidentity.ErrAnswer) {
		t.Fatalf("renewal co-signed by another certificate: %v", err)
	}
}

func TestCheckRenewalRequestSpec(t *testing.T) {
	f := newRenewalFixture(t)
	v, err := hostidentity.VerifyRenewal(f.signBody(t, f.body(t)), f.renewalExp())
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: f.csrDER})
	ok := hostidentity.RenewalRequestSpec{CSRPEM: csrPEM, Usages: []string{hostidentity.UsageServerAuth, hostidentity.UsageDigitalSignature},
		Duration: 24 * time.Hour}
	if err := hostidentity.CheckRenewalRequestSpec(ok, v); err != nil {
		t.Fatalf("CheckRenewalRequestSpec: %v", err)
	}
	other := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST",
		Bytes: f.csr(t, hostidentitytest.NewKey(t), hostidentity.RequestURIs(f.pki.HostURI, f.pki.Record))})
	for name, mut := range map[string]func(*hostidentity.RenewalRequestSpec){
		"CA":            func(s *hostidentity.RenewalRequestSpec) { s.IsCA = true },
		"client auth":   func(s *hostidentity.RenewalRequestSpec) { s.Usages = append(s.Usages, hostidentity.UsageClientAuth) },
		"no lifetime":   func(s *hostidentity.RenewalRequestSpec) { s.Duration = 0 },
		"long lifetime": func(s *hostidentity.RenewalRequestSpec) { s.Duration = hostidentity.MaxLifetime + time.Hour },
		"other request": func(s *hostidentity.RenewalRequestSpec) { s.CSRPEM = other },
		"not PEM":       func(s *hostidentity.RenewalRequestSpec) { s.CSRPEM = f.csrDER },
	} {
		s := ok
		s.Usages = append([]string(nil), ok.Usages...)
		mut(&s)
		if err := hostidentity.CheckRenewalRequestSpec(s, v); err == nil {
			t.Errorf("%s: accepted", name)
		} else if _, isDenial := hostidentity.AsDenial(err); !isDenial {
			t.Errorf("%s: %v is not a denial", name, err)
		}
	}
	block, _ := pem.Decode(f.pki.HostCertPEM)
	cur, err := hostidentity.ParseCertificates(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))
	if err != nil {
		t.Fatal(err)
	}
	if d := hostidentity.RenewalLifetime(cur[0]); d <= 0 || d > hostidentity.MaxLifetime || d%time.Hour != 0 {
		t.Errorf("RenewalLifetime = %s", d)
	}
}
