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

package enrol_test

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// renewFixture is a host with an installed certificate and a pending
// renewal for a new key, in a private key material directory.
type renewFixture struct {
	pki    *trusttest.PKI
	dir    string
	cfg    *trust.Config
	cur    *x509.Certificate
	newCSR []byte
	newPEM []byte
}

func newRenewFixture(t *testing.T) *renewFixture {
	t.Helper()
	p := trusttest.New(t)
	dir := filepath.Join(t.TempDir(), "pki")
	p.WritePKI(t, dir)
	trustDir := filepath.Join(t.TempDir(), "trust")
	p.WriteTrust(t, trustDir)
	uid := uint32(os.Getuid()) // #nosec G115 -- a uid is 32 bits
	cfg, err := trust.LoadConfig(trustDir, trust.TrustFileReader(uid))
	require.NoError(t, err)
	b, _ := pem.Decode(p.HostCertPEM)
	cur, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	k := hostidentitytest.NewKey(t)
	csrPEM := hostidentitytest.NewCSR(t, k, hostidentitytest.CSROptions{CommonName: trusttest.HostName,
		URIs: hostidentity.RequestURIs(p.HostURI, p.Record)})
	cb, _ := pem.Decode(csrPEM)
	return &renewFixture{pki: p, dir: dir, cfg: cfg, cur: cur, newCSR: cb.Bytes, newPEM: p.HostCA.Issue(t, csrPEM, hostidentitytest.IssueOptions{})}
}

func reader() func(string) ([]byte, error) {
	return trust.KeyFileReader(uint32(os.Getuid())) // #nosec G115 -- a uid is 32 bits
}

func TestRenewalDue(t *testing.T) {
	f := newRenewFixture(t)
	at := enrol.RenewalAt(f.cur)
	life := f.cur.NotAfter.Sub(f.cur.NotBefore)
	assert.WithinDuration(t, f.cur.NotBefore.Add(2*life/3), at, time.Second)
	assert.False(t, enrol.RenewalDue(f.cur, at.Add(-time.Second)))
	assert.True(t, enrol.RenewalDue(f.cur, at))
}

func TestPendingFiles(t *testing.T) {
	f := newRenewFixture(t)
	p, err := enrol.LoadPending(f.dir, reader())
	require.NoError(t, err)
	assert.Nil(t, p.CSR)
	require.NoError(t, enrol.SavePendingCSR(f.dir, f.newCSR, f.cur, time.Now()))
	p, err = enrol.LoadPending(f.dir, reader())
	require.NoError(t, err)
	require.NotNil(t, p.CSR)
	assert.True(t, p.MatchesCurrent(f.cur))
	fi, err := os.Stat(filepath.Join(f.dir, enrol.FileRenewalCSR))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// A request for other URIs is refused.
	otherRec := f.pki.Record
	otherRec.EnrolmentID = make([]byte, 16)
	other := hostidentitytest.NewCSR(t, hostidentitytest.NewKey(t), hostidentitytest.CSROptions{
		URIs: hostidentity.RequestURIs(f.pki.HostURI, otherRec)})
	ob, _ := pem.Decode(other)
	assert.Error(t, enrol.SavePendingCSR(f.dir, ob.Bytes, f.cur, time.Now()))

	// A malformed file is an error, for the caller to discard.
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, enrol.FileRenewalCert), []byte("not a certificate"), 0o600))
	_, err = enrol.LoadPending(f.dir, reader())
	assert.Error(t, err)
	require.NoError(t, enrol.DiscardPending(f.dir))
	p, err = enrol.LoadPending(f.dir, reader())
	require.NoError(t, err)
	assert.Nil(t, p.CSR)
	assert.Nil(t, p.Staged)
}

func TestRecoverRenewal(t *testing.T) {
	newKeyOf := func(f *renewFixture) *x509.Certificate {
		b, _ := pem.Decode(f.newPEM)
		c, err := x509.ParseCertificate(b.Bytes)
		require.NoError(t, err)
		return c
	}
	t.Run("signer uses the new key: promoted", func(t *testing.T) {
		f := newRenewFixture(t)
		require.NoError(t, enrol.SavePendingCSR(f.dir, f.newCSR, f.cur, time.Now()))
		staged := newKeyOf(f)
		require.NoError(t, enrol.Stage(f.dir, staged.Raw))
		done, err := enrol.RecoverRenewal(f.dir, reader(), f.cfg, ecPub(t, staged.PublicKey), time.Now())
		require.NoError(t, err)
		assert.True(t, done)
		b, err := os.ReadFile(filepath.Join(f.dir, trust.FileHostCert))
		require.NoError(t, err)
		pb, _ := pem.Decode(b)
		assert.Equal(t, staged.Raw, pb.Bytes)
		assert.NoFileExists(t, filepath.Join(f.dir, enrol.FileRenewalCert))
		assert.NoFileExists(t, filepath.Join(f.dir, enrol.FileRenewalCSR))
	})
	t.Run("signer still uses the old key: nothing", func(t *testing.T) {
		f := newRenewFixture(t)
		require.NoError(t, enrol.SavePendingCSR(f.dir, f.newCSR, f.cur, time.Now()))
		require.NoError(t, enrol.Stage(f.dir, newKeyOf(f).Raw))
		done, err := enrol.RecoverRenewal(f.dir, reader(), f.cfg, ecPub(t, f.cur.PublicKey), time.Now())
		require.NoError(t, err)
		assert.False(t, done)
		assert.FileExists(t, filepath.Join(f.dir, enrol.FileRenewalCert))
	})
	t.Run("staged for another key: nothing", func(t *testing.T) {
		f := newRenewFixture(t)
		require.NoError(t, enrol.Stage(f.dir, newKeyOf(f).Raw))
		other := hostidentitytest.NewKey(t)
		done, err := enrol.RecoverRenewal(f.dir, reader(), f.cfg, &other.PublicKey, time.Now())
		require.NoError(t, err)
		assert.False(t, done)
	})
	t.Run("staged for another enrolment: nothing", func(t *testing.T) {
		f := newRenewFixture(t)
		k := hostidentitytest.NewKey(t)
		rec := f.pki.Record
		rec.EnrolmentID = make([]byte, 16)
		c := f.pki.IssueHost(t, k, hostidentity.RequestURIs(f.pki.HostURI, rec), hostidentitytest.IssueOptions{})
		cb, _ := pem.Decode(c)
		require.NoError(t, enrol.Stage(f.dir, cb.Bytes))
		done, err := enrol.RecoverRenewal(f.dir, reader(), f.cfg, &k.PublicKey, time.Now())
		require.NoError(t, err)
		assert.False(t, done)
	})
}

// ecPub returns k as an ECDSA public key, failing the test otherwise.
func ecPub(t testing.TB, k any) *ecdsa.PublicKey {
	t.Helper()
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("not an ECDSA public key: %T", k)
	}
	return pub
}

// TestPendingExpired: an unstaged request expires after half the lifetime
// left when it was made; a staged one never does.
func TestPendingExpired(t *testing.T) {
	f := newRenewFixture(t)
	since := time.Now()
	require.NoError(t, enrol.SavePendingCSR(f.dir, f.newCSR, f.cur, since))
	p, err := enrol.LoadPending(f.dir, reader())
	require.NoError(t, err)
	assert.WithinDuration(t, since, p.Since, time.Second)
	half := since.Add(f.cur.NotAfter.Sub(since) / 2)
	assert.False(t, p.PendingExpired(f.cur, half.Add(-time.Minute)))
	assert.True(t, p.PendingExpired(f.cur, half.Add(time.Minute)))
	b, _ := pem.Decode(f.newPEM)
	require.NoError(t, enrol.Stage(f.dir, b.Bytes))
	p, err = enrol.LoadPending(f.dir, reader())
	require.NoError(t, err)
	assert.False(t, p.PendingExpired(f.cur, half.Add(time.Hour)), "a staged renewal never expires")
	require.NoError(t, enrol.DiscardPending(f.dir))
	assert.NoFileExists(t, filepath.Join(f.dir, enrol.FileRenewalSince))
}
