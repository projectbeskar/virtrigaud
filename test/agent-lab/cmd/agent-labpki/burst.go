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
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// grantSigner is a grant-signing chain under the lab grant anchor: what the
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

// loadGrantSigner reads the anchor gen wrote and issues a path length 0
// issuer and a grant-signing leaf under it.
func loadGrantSigner(pdir string, installID []byte) (*grantSigner, error) {
	certPEM, err := os.ReadFile(filepath.Join(pdir, fileGrantCA)) // #nosec G304 -- a lab path
	if err != nil {
		return nil, fmt.Errorf("read the grant anchor: %w", err)
	}
	keyPEMBytes, err := os.ReadFile(filepath.Join(pdir, fileGrantKey)) // #nosec G304 -- a lab path
	if err != nil {
		return nil, fmt.Errorf("read the grant anchor key: %w", err)
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEMBytes)
	if cb == nil || kb == nil {
		return nil, errors.New("the grant anchor files are not PEM")
	}
	anchor, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the grant anchor: %w", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the grant anchor key: %w", err)
	}
	anchorKey, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("the grant anchor key cannot sign")
	}
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
	issuer, err := mk(&x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "lab grant issuer"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}, anchor, &ik.PublicKey, anchorKey)
	if err != nil {
		return nil, err
	}
	lk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	leaf, err := mk(&x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "lab grant leaf"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature},
		issuer, &lk.PublicKey, ik)
	if err != nil {
		return nil, err
	}
	return &grantSigner{installID: installID, leafKey: lk, chain: [][]byte{leaf.Raw, issuer.Raw}}, nil
}

// burstResult is burst's output line.
type burstResult struct {
	// Lifecycles is the number of VM lifecycles sent.
	Lifecycles int `json:"lifecycles"`
	// Sent is the number of operations sent.
	Sent int64 `json:"sent"`
	// Succeeded counts tasks that ended successfully.
	Succeeded int64 `json:"succeeded"`
	// Failed counts calls or tasks that failed.
	Failed int64 `json:"failed"`
	// Seconds is the wall time.
	Seconds float64 `json:"seconds"`
	// FirstError is the first failure seen, if any.
	FirstError string `json:"firstError,omitempty"`
}

// burster sends operations for one enrolment.
type burster struct {
	rec    labRecord
	grants *grantSigner
	epoch  operation.Epoch
	cl     providerv1.ProviderClient
	res    burstResult
	errMu  sync.Mutex
}

// burst sends -n VM lifecycles over -workers workers.
func burst(args []string) error {
	fs := flag.NewFlagSet("burst", flag.ContinueOnError)
	dir := fs.String("dir", "", "the directory gen wrote")
	addr := fs.String("addr", "127.0.0.1:9460", "the agent's address")
	n := fs.Int("n", 1000, "VM lifecycles (3 operations each)")
	workers := fs.Int("workers", 8, "concurrent lifecycles")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pdir := filepath.Join(*dir, dirProvider)
	raw, err := os.ReadFile(filepath.Join(pdir, fileRecord)) // #nosec G304 -- a lab path
	if err != nil {
		return fmt.Errorf("read the record: %w", err)
	}
	b := &burster{}
	if err := json.Unmarshal(raw, &b.rec); err != nil {
		return fmt.Errorf("decode the record: %w", err)
	}
	if b.grants, err = loadGrantSigner(pdir, b.rec.InstallID); err != nil {
		return err
	}
	b.epoch = operation.Epoch{Counter: 1, Holder: randomID()}
	cc, err := dial(*dir, *addr)
	if err != nil {
		return err
	}
	defer func() { _ = cc.Close() }()
	b.cl = providerv1.NewProviderClient(cc)
	start := time.Now()
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				b.lifecycle(i)
			}
		}()
	}
	for i := 0; i < *n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	b.res.Lifecycles = *n
	b.res.Seconds = time.Since(start).Seconds()
	line, err := json.Marshal(&b.res)
	if err != nil {
		return fmt.Errorf("encode the result: %w", err)
	}
	fmt.Println(string(line))
	return nil
}

// lifecycle creates, powers off and deletes one VM.
func (b *burster) lifecycle(i int) {
	vm := fmt.Sprintf("burst-%d-%x", i, randomID()[:4])
	binding := randomID()
	for _, kind := range []providerv1.OperationKind{providerv1.OperationKind_OPERATION_KIND_CREATE,
		providerv1.OperationKind_OPERATION_KIND_POWER_OFF, providerv1.OperationKind_OPERATION_KIND_DELETE} {
		if err := b.op(kind, vm, binding); err != nil {
			atomic.AddInt64(&b.res.Failed, 1)
			b.errMu.Lock()
			if b.res.FirstError == "" {
				b.res.FirstError = err.Error()
			}
			b.errMu.Unlock()
			return
		}
		atomic.AddInt64(&b.res.Succeeded, 1)
	}
}

// op sends one operation and waits for its task to end.
func (b *burster) op(kind providerv1.OperationKind, vm string, binding []byte) error {
	row, ok := grantverify.RowFor(kind)
	if !ok {
		return fmt.Errorf("no request type for %s", kind)
	}
	owner := &providerv1.ObjectIdentity{Uid: vm, Namespace: "ns", Name: "vm"}
	var req proto.Message
	switch kind {
	case providerv1.OperationKind_OPERATION_KIND_CREATE:
		req = &providerv1.CreateRequest{Name: "vm", TargetHostId: b.rec.Host, Owner: owner}
	case providerv1.OperationKind_OPERATION_KIND_DELETE:
		req = &providerv1.DeleteRequest{Id: vm, TargetHostId: b.rec.Host, Owner: owner}
	default:
		req = &providerv1.PowerRequest{Id: vm, Op: row.FixedPowerOp, TargetHostId: b.rec.Host, Owner: owner}
	}
	reqBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g, err := operation.MintOperationGrant(ctx, b.grants, operation.MintRequest{
		Identity: operation.Identity{ProviderUID: b.rec.ProviderUID, HostID: b.rec.Host, EnrolmentID: b.rec.EnrolmentID,
			Epoch: b.epoch, VMUID: vm, BindingID: binding, OpID: randomID(), Kind: kind, Attempt: 1, MintEpoch: b.epoch},
		DigestSalt: append(randomID(), randomID()...), Request: reqBytes, IssuedAt: time.Now(), GrantVersion: operation.LedgerGrantVersion,
	})
	if err != nil {
		return fmt.Errorf("mint grant: %w", err)
	}
	env := &providerv1.OperationEnvelope{Request: reqBytes, Grant: g}
	atomic.AddInt64(&b.res.Sent, 1)
	var task string
	switch kind {
	case providerv1.OperationKind_OPERATION_KIND_CREATE:
		r, err := b.cl.Create(ctx, &providerv1.CreateRequest{OperationEnvelope: env})
		if err != nil {
			return fmt.Errorf("create: %w", err)
		}
		task = r.GetTask().GetId()
	case providerv1.OperationKind_OPERATION_KIND_DELETE:
		r, err := b.cl.Delete(ctx, &providerv1.DeleteRequest{OperationEnvelope: env})
		if err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		task = r.GetTask().GetId()
	default:
		r, err := b.cl.Power(ctx, &providerv1.PowerRequest{OperationEnvelope: env})
		if err != nil {
			return fmt.Errorf("power: %w", err)
		}
		task = r.GetTask().GetId()
	}
	return b.wait(ctx, task)
}

// wait polls the task until it ends.
func (b *burster) wait(ctx context.Context, task string) error {
	for {
		st, err := b.cl.TaskStatus(ctx, &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: task}})
		if err != nil {
			return fmt.Errorf("task status: %w", err)
		}
		switch st.GetOutcome() {
		case providerv1.TaskOutcome_TASK_OUTCOME_RUNNING:
		case providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED:
			return nil
		default:
			return fmt.Errorf("task %s ended %s: %s", task, st.GetOutcome(), st.GetOutcomeReason())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("task %s: %w", task, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
