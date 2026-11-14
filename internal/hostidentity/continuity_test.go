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
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func head(b byte) [32]byte {
	var h [32]byte
	h[0] = b
	return h
}

func headB(b byte) []byte { h := head(b); return h[:] }

// answerAt is a verified answer of store store at seq with head h.
func answerAt(store byte, enrol byte, seq uint64, h [32]byte, continuity []byte, cpSeq uint64, cpHead []byte) hostidentity.VerifiedAnswer {
	var sid [16]byte
	sid[0] = store
	return hostidentity.VerifiedAnswer{
		Position: hostidentity.ChainPosition{StoreID: sid, Seq: seq, Head: h},
		Context: &providerv1.AnswerContext{EnrolmentId: bytes.Repeat([]byte{enrol}, 16), ContinuityHead: continuity,
			CheckpointSeq: cpSeq, CheckpointHead: cpHead},
	}
}

func TestCheckContinuity(t *testing.T) {
	h5 := head(5)
	first := hostidentity.CheckContinuity(hostidentity.StoreRecord{}, answerAt(1, 1, 5, h5, nil, 0, nil))
	if first.Regressed || first.Next.Seq != 5 || first.Next.ContinuitySeq() != 5 {
		t.Fatalf("first answer: %+v", first)
	}
	snap := first.Next
	cp := head(9)
	cases := map[string]struct {
		v      hostidentity.VerifiedAnswer
		reason string
		seq    uint64
	}{
		"advances":                  {v: answerAt(1, 1, 8, head(8), h5[:], 0, nil), seq: 8},
		"same position":             {v: answerAt(1, 1, 5, h5, h5[:], 0, nil), seq: 5},
		"rolled back":               {v: answerAt(1, 1, 4, head(4), nil, 0, nil), reason: hostidentity.ContinuityRolledBack},
		"forked head":               {v: answerAt(1, 1, 9, head(9), headB(6), 0, nil), reason: hostidentity.ContinuityForked},
		"head dropped too soon":     {v: answerAt(1, 1, 9, head(9), nil, 0, nil), reason: hostidentity.ContinuityForked},
		"store changed":             {v: answerAt(2, 1, 9, head(9), nil, 0, nil), reason: hostidentity.ContinuityStoreChanged},
		"new store, new enrolment":  {v: answerAt(2, 2, 1, head(1), nil, 0, nil), seq: 1},
		"honest pruning far ahead":  {v: answerAt(1, 1, 5+hostidentity.CheckpointInterval, head(7), nil, 0, nil), seq: 5 + hostidentity.CheckpointInterval},
		"checkpoint recorded first": {v: answerAt(1, 1, 6, head(6), h5[:], 4, cp[:]), seq: 6},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := hostidentity.CheckContinuity(snap, tc.v)
			if c.Reason != tc.reason || c.Regressed != (tc.reason != "") {
				t.Fatalf("CheckContinuity = %+v, want reason %q", c, tc.reason)
			}
			if tc.reason == "" && c.Next.Seq != tc.seq {
				t.Fatalf("raised to %d, want %d", c.Next.Seq, tc.seq)
			}
		})
	}
	// A checkpoint that changes or goes down is a regression.
	withCP := hostidentity.CheckContinuity(snap, answerAt(1, 1, 6, head(6), h5[:], 4, cp[:])).Next
	other := head(10)
	if c := hostidentity.CheckContinuity(withCP, answerAt(1, 1, 7, head(7), headB(6), 4, other[:])); c.Reason != hostidentity.ContinuityCheckpointRegressed {
		t.Fatalf("changed checkpoint: %+v", c)
	}
	low := head(1)
	if c := hostidentity.CheckContinuity(withCP, answerAt(1, 1, 7, head(7), headB(6), 2, low[:])); c.Reason != hostidentity.ContinuityCheckpointRegressed {
		t.Fatalf("lower checkpoint: %+v", c)
	}
	// After honest pruning the next request names the checkpoint, whose head
	// must then be reproduced.
	pruned := hostidentity.CheckContinuity(withCP, answerAt(1, 1, 6+hostidentity.CheckpointInterval, head(7), nil, 4, cp[:])).Next
	if !pruned.UseCheckpoint && pruned.ContinuitySeq() != pruned.Seq {
		t.Fatalf("pruned record %+v", pruned)
	}
	if c := hostidentity.CheckContinuity(withCP, answerAt(1, 1, 6+hostidentity.CheckpointInterval, head(7), nil, 4, cp[:])); c.Regressed {
		t.Fatalf("honest pruning held: %+v", c)
	}
	// A concurrent raise is kept when merging.
	merged := hostidentity.MergeStoreRecords(hostidentity.StoreRecord{StoreID: snap.StoreID, Seq: 20, EnrolmentID: snap.EnrolmentID}, snap)
	if merged.Seq != 20 {
		t.Fatalf("merge lowered L to %d", merged.Seq)
	}
}

func caCert(t *testing.T, parent *x509.Certificate, parentKey any, name string) (*x509.Certificate, any) {
	t.Helper()
	key := hostidentitytest.NewKey(t)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func TestCheckHierarchySeparation(t *testing.T) {
	root, rootKey := caCert(t, nil, nil, "root")
	sub, _ := caCert(t, root, rootKey, "host sub-CA")
	grantIssuer, _ := caCert(t, root, rootKey, "grant issuer")
	separate, _ := caCert(t, nil, nil, "grant root")
	if err := hostidentity.CheckHierarchySeparation([]*x509.Certificate{sub}, []*x509.Certificate{separate}); err != nil {
		t.Fatalf("separate hierarchies refused: %v", err)
	}
	for name, grant := range map[string][]*x509.Certificate{
		"the root as grant anchor":       {root},
		"the sub-CA listed as an anchor": {separate, sub},
	} {
		t.Run(name, func(t *testing.T) {
			if err := hostidentity.CheckHierarchySeparation([]*x509.Certificate{sub}, grant); !errors.Is(err, hostidentity.ErrHierarchiesShared) {
				t.Fatalf("CheckHierarchySeparation = %v", err)
			}
		})
	}
	// A grant issuer that chains to the host anchor is refused too.
	if err := hostidentity.CheckHierarchySeparation([]*x509.Certificate{root}, []*x509.Certificate{grantIssuer}); err == nil {
		t.Fatal("a grant issuer under the host anchor was accepted")
	}
}

// certWith issues a certificate for a fresh key from parent (self-signed when
// parent is nil), with the given validity start and authority key id.
func certWith(t *testing.T, parent *x509.Certificate, parentKey any, notBefore time.Time, aki []byte) *x509.Certificate {
	t.Helper()
	key := hostidentitytest.NewKey(t)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "c"},
		NotBefore: notBefore, NotAfter: notBefore.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, AuthorityKeyId: aki}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A certificate signed by the other hierarchy is refused even when no chain
// verifies (here its validity starts before its issuer's), and so is one
// that names the other's key as its authority key.
func TestCheckHierarchySeparation_SignatureAndAuthorityKey(t *testing.T) {
	now := time.Now()
	hostCA, hostKey := caCert(t, nil, nil, "host sub-CA")
	early := certWith(t, hostCA, hostKey, now.Add(-48*time.Hour), nil)
	if err := hostidentity.CheckHierarchySeparation([]*x509.Certificate{hostCA}, []*x509.Certificate{early}); !errors.Is(err, hostidentity.ErrHierarchiesShared) {
		t.Fatalf("a grant anchor signed by the host CA was accepted: %v", err)
	}
	claiming := certWith(t, nil, nil, now.Add(-time.Minute), hostCA.SubjectKeyId)
	if len(hostCA.SubjectKeyId) == 0 {
		t.Skip("no subject key id generated")
	}
	if err := hostidentity.CheckHierarchySeparation([]*x509.Certificate{hostCA}, []*x509.Certificate{claiming}); !errors.Is(err, hostidentity.ErrHierarchiesShared) {
		t.Fatalf("a grant anchor naming the host CA's key as its authority was accepted: %v", err)
	}
}
