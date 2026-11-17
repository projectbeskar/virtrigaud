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

package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// grantSigner is a grant-signing chain under the PKI's grant anchor: what the
// manager's authority would hold. It implements operation.Signer.
type grantSigner struct {
	installID []byte
	leafKey   *ecdsa.PrivateKey
	chain     [][]byte
}

var _ operation.Signer = (*grantSigner)(nil)

// InstallID implements operation.Signer.
func (g *grantSigner) InstallID() []byte { return g.installID }

// AuthorityGeneration implements operation.Signer.
func (g *grantSigner) AuthorityGeneration() uint64 { return 1 }

// Sign implements operation.Signer: ECDSA over SHA-256 with a low S value.
func (g *grantSigner) Sign(_ context.Context, body []byte, _ time.Time) (operation.Signature, error) {
	d := sha256.Sum256(body)
	sig, err := g.leafKey.Sign(rand.Reader, d[:], crypto.SHA256)
	if err != nil {
		return operation.Signature{}, fmt.Errorf("sign grant: %w", err)
	}
	if sig, err = grantverify.NormalizeLowS(sig, elliptic.P256()); err != nil {
		return operation.Signature{}, fmt.Errorf("normalise signature: %w", err)
	}
	return operation.Signature{Signature: sig, Chain: g.chain}, nil
}

// newGrantSigner issues a path length 0 issuer and a grant-signing leaf under
// the grant anchor of pki, valid for a day each side of now.
func newGrantSigner(pki *trusttest.PKI) (*grantSigner, error) {
	mk := func(tmpl, parent *x509.Certificate, pub any, key crypto.Signer) (*x509.Certificate, error) {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, key)
		if err != nil {
			return nil, fmt.Errorf("create certificate: %w", err)
		}
		return x509.ParseCertificate(der)
	}
	now := time.Now()
	ik, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate issuer key: %w", err)
	}
	issuer, err := mk(&x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "grant issuer"},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(7 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}, pki.GrantCA.Cert, &ik.PublicKey, pki.GrantCA.Key)
	if err != nil {
		return nil, err
	}
	lk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	leaf, err := mk(&x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "grant leaf"},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(7 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature},
		issuer, &lk.PublicKey, ik)
	if err != nil {
		return nil, err
	}
	return &grantSigner{installID: pki.Record.InstallID, leafKey: lk, chain: [][]byte{leaf.Raw, issuer.Raw}}, nil
}

// caller is the manager side of the operation path: it mints the grants the
// agent verifies and sends them.
type caller struct {
	pki    *trusttest.PKI
	grants *grantSigner
	epoch  operation.Epoch
}

func newCaller(pki *trusttest.PKI) (*caller, error) {
	g, err := newGrantSigner(pki)
	if err != nil {
		return nil, err
	}
	holder := make([]byte, 16)
	if _, err := rand.Read(holder); err != nil {
		return nil, fmt.Errorf("draw epoch holder: %w", err)
	}
	// The enrolment starts at epoch 2 (the development enrolment file).
	return &caller{pki: pki, grants: g, epoch: operation.Epoch{Counter: 2, Holder: holder}}, nil
}

// opSpec is one operation attempt, with everything a re-send needs: the same
// identity, request and digest salt.
type opSpec struct {
	id      operation.Identity
	salt    []byte
	request []byte
	kind    providerv1.OperationKind
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source failed: nothing can be tested
	}
	return b
}

func ownerOf(uid string) *providerv1.ObjectIdentity {
	return &providerv1.ObjectIdentity{Uid: uid, Namespace: "ns", Name: "vm"}
}

// newOp prepares one attempt of kind for vm with binding.
func (c *caller) newOp(kind providerv1.OperationKind, vm string, binding []byte, attempt uint64) (*opSpec, error) {
	row, ok := grantverify.RowFor(kind)
	if !ok {
		return nil, fmt.Errorf("no request type for %s", kind)
	}
	host := trusttest.HostName
	var req proto.Message
	switch kind {
	case providerv1.OperationKind_OPERATION_KIND_CREATE:
		req = &providerv1.CreateRequest{Name: "vm", TargetHostId: host, Owner: ownerOf(vm)}
	case providerv1.OperationKind_OPERATION_KIND_DELETE:
		req = &providerv1.DeleteRequest{Id: vm, TargetHostId: host, Owner: ownerOf(vm)}
	default:
		req = &providerv1.PowerRequest{Id: vm, Op: row.FixedPowerOp, TargetHostId: host, Owner: ownerOf(vm)}
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	return &opSpec{
		id: operation.Identity{ProviderUID: c.pki.Record.ProviderUID, HostID: host, EnrolmentID: c.pki.Record.EnrolmentID,
			Epoch: c.epoch, VMUID: vm, BindingID: binding, OpID: randBytes(16), Kind: kind, Attempt: attempt, MintEpoch: c.epoch},
		salt: randBytes(32), request: b, kind: kind,
	}, nil
}

// envelope mints a grant for o issued now and wraps it with the request.
func (c *caller) envelope(ctx context.Context, o *opSpec) (*providerv1.OperationEnvelope, error) {
	g, err := operation.MintOperationGrant(ctx, c.grants, operation.MintRequest{
		Identity: o.id, DigestSalt: o.salt, Request: o.request, IssuedAt: time.Now()})
	if err != nil {
		return nil, fmt.Errorf("mint grant: %w", err)
	}
	return &providerv1.OperationEnvelope{Request: o.request, Grant: g}, nil
}

// send delivers o on its method over cc and returns the task reference.
func (c *caller) send(ctx context.Context, cc grpc.ClientConnInterface, o *opSpec) (string, error) {
	env, err := c.envelope(ctx, o)
	if err != nil {
		return "", err
	}
	cl := providerv1.NewProviderClient(cc)
	switch o.kind {
	case providerv1.OperationKind_OPERATION_KIND_CREATE:
		r, err := cl.Create(ctx, &providerv1.CreateRequest{OperationEnvelope: env})
		if err != nil {
			return "", err
		}
		return r.GetTask().GetId(), nil
	case providerv1.OperationKind_OPERATION_KIND_DELETE:
		r, err := cl.Delete(ctx, &providerv1.DeleteRequest{OperationEnvelope: env})
		if err != nil {
			return "", err
		}
		return r.GetTask().GetId(), nil
	default:
		r, err := cl.Power(ctx, &providerv1.PowerRequest{OperationEnvelope: env})
		if err != nil {
			return "", err
		}
		return r.GetTask().GetId(), nil
	}
}

// taskStatus asks for the state of task.
func taskStatus(ctx context.Context, cc grpc.ClientConnInterface, task string) (*providerv1.TaskStatusResponse, error) {
	return providerv1.NewProviderClient(cc).TaskStatus(ctx, &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: task}})
}
