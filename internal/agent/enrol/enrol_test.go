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
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// fixture is one enrolment: a PKI whose host CA is the pinned sub-CA, a host
// key held only by the test (standing for the signer's), the manager's
// record URIs, and the request built from them.
type fixture struct {
	pki    *trusttest.PKI
	key    *ecdsa.PrivateKey
	rec    hostidentity.Record
	uris   []string
	csr    []byte
	pkiDir string
	cfg    *trust.Config
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pki := trusttest.New(t)
	rec := hostidentity.Record{
		EnrolmentID: bytes16(0xA0), ProviderUID: trusttest.ProviderUID, InstallID: bytes16(0xB0), Mode: hostidentity.ModeIdentity,
	}
	f := &fixture{pki: pki, key: hostidentitytest.NewKey(t), rec: rec, now: time.Now(),
		uris: hostidentity.RequestURIs(pki.HostURI, rec), pkiDir: filepath.Join(t.TempDir(), "pki")}
	require.NoError(t, os.Mkdir(f.pkiDir, 0o700))
	csr, err := enrol.BuildCSR(f.key, f.uris)
	require.NoError(t, err)
	f.csr = csr
	f.cfg = &trust.Config{HostCAPool: pki.HostCAPool()}
	return f
}

func bytes16(b byte) []byte {
	out := make([]byte, hostidentity.IDLen)
	for i := range out {
		out[i] = b + byte(i)
	}
	return out
}

func (f *fixture) install(cert []byte, replace bool) (enrol.Result, error) {
	return enrol.Install(enrol.InstallOptions{
		PKIDir: f.pkiDir, UID: signer.EUID(), CSR: f.csr, Certificate: cert, Trust: f.cfg, Signer: f.key, Now: f.now, Replace: replace,
	})
}

func (f *fixture) issue(t *testing.T, o hostidentitytest.IssueOptions) []byte {
	t.Helper()
	return f.pki.HostCA.Issue(t, f.csr, o)
}

func (f *fixture) hostCrt() string { return filepath.Join(f.pkiDir, trust.FileHostCert) }

// TestBuildCSRIsWhatTheManagerApproves round-trips the request through the
// manager's own authorisation rules.
func TestBuildCSRIsWhatTheManagerApproves(t *testing.T) {
	f := newFixture(t)
	policy := hostidentity.Policy{AdminGroup: "admins", HostName: trusttest.HostName, HostURI: f.pki.HostURI, Record: f.rec}
	req := hostidentity.Request{
		CSRPEM: f.csr, Usages: []string{hostidentity.UsageServerAuth}, Duration: hostidentity.MaxLifetime, Groups: []string{"admins"},
	}
	auth, err := hostidentity.Authorise(policy, req)
	require.NoError(t, err, "the manager authorises the agent's own request")
	pin, err := enrol.KeyPin(f.key.Public())
	require.NoError(t, err)
	assert.Equal(t, pin, auth.KeyPin)
	assert.True(t, auth.Record.Equal(f.rec))

	csr, err := hostidentity.ParseCSR(f.csr)
	require.NoError(t, err)
	assert.Equal(t, trusttest.HostName, csr.Subject.CommonName)
	assert.Empty(t, csr.DNSNames)
	assert.Empty(t, csr.IPAddresses)
	for _, ext := range csr.Extensions {
		assert.True(t, ext.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 17}), "only the subject alternative names are requested, got %s", ext.Id)
	}

	// A record the manager did not draw is refused by the manager.
	other := f.rec
	other.EnrolmentID = bytes16(0xC0)
	policy.Record = other
	_, err = hostidentity.Authorise(policy, req)
	require.Error(t, err)
}

func TestBuildCSRRefusals(t *testing.T) {
	f := newFixture(t)
	good := f.uris
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)

	drop := func(i int) []string { return append(append([]string{}, good[:i]...), good[i+1:]...) }
	cases := []struct {
		name string
		uris []string
		key  crypto.Signer
	}{
		{"no uris", nil, f.key},
		{"no host uri", good[1:], f.key},
		{"no enrolment uri", drop(1), f.key},
		{"no provider uri", drop(2), f.key},
		{"no install uri", drop(3), f.key},
		{"no mode uri", drop(4), f.key},
		{"an extra uri", append(append([]string{}, good...), "urn:virtrigaud:extra"), f.key},
		{"a repeated uri", append(append([]string{}, good...), good[1]), f.key},
		{"uppercase enrolment id", []string{good[0], "urn:virtrigaud:enrolment:A0A1A2A3A4A5A6A7A8A9AAABACADAEAF", good[2], good[3], good[4]}, f.key},
		{"an Ed25519 key", good, edKey},
		{"a P-384 key", good, p384},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := enrol.BuildCSR(tc.key, tc.uris)
			require.Error(t, err)
			assert.ErrorIs(t, err, enrol.ErrRequest)
		})
	}
	_, err = enrol.BuildCSR(nil, good)
	require.ErrorIs(t, err, enrol.ErrRequest)
}

func TestInstallWritesTheCertificateAndDerivesTheRecord(t *testing.T) {
	f := newFixture(t)
	res, err := f.install(f.issue(t, hostidentitytest.IssueOptions{}), false)
	require.NoError(t, err)
	assert.Equal(t, f.pki.HostURI, res.HostURI)
	assert.True(t, res.Record.Equal(f.rec), "the record is derived from the certificate's URI SANs")
	assert.False(t, res.Replaced)

	fi, err := os.Lstat(f.hostCrt())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	entries, err := os.ReadDir(f.pkiDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left behind")

	// The daemon's loader derives the same record from the file.
	id, err := trust.LoadIdentity(f.pkiDir, f.cfg, f.now, trust.KeyFileReader(signer.EUID()), f.key)
	require.NoError(t, err)
	assert.True(t, id.Record.Equal(f.rec))
	assert.Equal(t, f.pki.HostURI, id.HostURI)
}

func TestInstallKeepsOnlyTheLeaf(t *testing.T) {
	f := newFixture(t)
	chain := append(f.issue(t, hostidentitytest.IssueOptions{}), f.pki.HostCA.PEM...)
	_, err := f.install(chain, false)
	require.NoError(t, err)
	b, err := os.ReadFile(f.hostCrt())
	require.NoError(t, err)
	certs, err := hostidentity.ParseCertificates(b)
	require.NoError(t, err)
	assert.Len(t, certs, 1, "host.crt holds the host certificate only, as the agent loads it")
}

// TestInstallRefusals: each refusal wraps ErrInstall and writes nothing.
func TestInstallRefusals(t *testing.T) {
	otherCA := hostidentitytest.NewCA(t)
	otherKey := hostidentitytest.NewKey(t)
	tests := []struct {
		name  string
		cert  func(t *testing.T, f *fixture) []byte
		mutFn func(f *fixture)
	}{
		{name: "wrong chain: another CA", cert: func(t *testing.T, f *fixture) []byte {
			return otherCA.Issue(t, f.csr, hostidentitytest.IssueOptions{})
		}},
		{name: "SANs: another enrolment id", cert: func(t *testing.T, f *fixture) []byte {
			other := f.rec
			other.EnrolmentID = bytes16(0xC0)
			return f.issue(t, hostidentitytest.IssueOptions{URIs: hostidentity.RequestURIs(f.pki.HostURI, other)})
		}},
		{name: "SANs: another provider uid", cert: func(t *testing.T, f *fixture) []byte {
			other := f.rec
			other.ProviderUID = "11111111-2222-4333-8444-555555555555"
			return f.issue(t, hostidentitytest.IssueOptions{URIs: hostidentity.RequestURIs(f.pki.HostURI, other)})
		}},
		{name: "SANs: another host", cert: func(t *testing.T, f *fixture) []byte {
			uri, err := hostidentity.HostURI(trusttest.TrustDomain, trusttest.HostNS, "host-b")
			require.NoError(t, err)
			return f.issue(t, hostidentitytest.IssueOptions{URIs: hostidentity.RequestURIs(uri, f.rec)})
		}},
		{name: "SANs: enrolment URIs missing", cert: func(t *testing.T, f *fixture) []byte {
			return f.issue(t, hostidentitytest.IssueOptions{URIs: []string{f.pki.HostURI}})
		}},
		{name: "SANs: an extra URI", cert: func(t *testing.T, f *fixture) []byte {
			return f.issue(t, hostidentitytest.IssueOptions{URIs: append(append([]string{}, f.uris...), "urn:virtrigaud:extra")})
		}},
		{name: "other key", cert: func(t *testing.T, f *fixture) []byte {
			return f.issue(t, hostidentitytest.IssueOptions{PublicKey: &otherKey.PublicKey})
		}},
		{name: "CA bit", cert: func(t *testing.T, f *fixture) []byte {
			return f.issue(t, hostidentitytest.IssueOptions{IsCA: true})
		}},
		{name: "lifetime above the profile", cert: func(t *testing.T, f *fixture) []byte {
			return f.issue(t, hostidentitytest.IssueOptions{Lifetime: 31 * 24 * time.Hour})
		}},
		{name: "client authentication usage", cert: func(t *testing.T, f *fixture) []byte {
			return f.issue(t, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}})
		}},
		{name: "no server authentication usage", cert: func(t *testing.T, f *fixture) []byte {
			return f.issue(t, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		}},
		{name: "another subject", cert: func(t *testing.T, f *fixture) []byte {
			return f.issue(t, hostidentitytest.IssueOptions{Subject: &pkix.Name{CommonName: "someone-else"}})
		}},
		{name: "expired", cert: func(t *testing.T, f *fixture) []byte {
			f.now = f.now.Add(60 * 24 * time.Hour)
			return f.issue(t, hostidentitytest.IssueOptions{})
		}},
		{name: "not a certificate", cert: func(*testing.T, *fixture) []byte { return []byte("not pem") }},
		{name: "a request instead of a certificate", cert: func(_ *testing.T, f *fixture) []byte { return f.csr }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.install(tc.cert(t, f), false)
			require.Error(t, err)
			assert.ErrorIs(t, err, enrol.ErrInstall)
			_, statErr := os.Lstat(f.hostCrt())
			assert.True(t, os.IsNotExist(statErr), "nothing is installed")
			entries, rerr := os.ReadDir(f.pkiDir)
			require.NoError(t, rerr)
			assert.Empty(t, entries)
		})
	}
}

// TestInstallRefusesAnotherSignerKey: the signer's key must be the
// request's, and the certificate must certify it.
func TestInstallRefusesAnotherSignerKey(t *testing.T) {
	f := newFixture(t)
	cert := f.issue(t, hostidentitytest.IssueOptions{})
	other := hostidentitytest.NewKey(t)
	_, err := enrol.Install(enrol.InstallOptions{
		PKIDir: f.pkiDir, UID: signer.EUID(), CSR: f.csr, Certificate: cert, Trust: f.cfg, Signer: other, Now: f.now,
	})
	require.ErrorIs(t, err, enrol.ErrInstall)
	assert.Contains(t, err.Error(), "key other than the signer's")

	// A request signed by the other key, with the certificate for that key:
	// the signer still is not that key.
	csr, err := enrol.BuildCSR(other, f.uris)
	require.NoError(t, err)
	cert2 := f.pki.HostCA.Issue(t, csr, hostidentitytest.IssueOptions{})
	_, err = enrol.Install(enrol.InstallOptions{
		PKIDir: f.pkiDir, UID: signer.EUID(), CSR: csr, Certificate: cert2, Trust: f.cfg, Signer: f.key, Now: f.now,
	})
	require.ErrorIs(t, err, enrol.ErrInstall)
	_, statErr := os.Lstat(f.hostCrt())
	assert.True(t, os.IsNotExist(statErr))
}

func TestInstallRefusesABadRequestFile(t *testing.T) {
	f := newFixture(t)
	cert := f.issue(t, hostidentitytest.IssueOptions{})
	block, _ := pem.Decode(f.csr)
	broken := append([]byte{}, block.Bytes...)
	broken[len(broken)-3] ^= 1
	for name, csr := range map[string][]byte{
		"not pem":          []byte("x"),
		"broken signature": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: broken}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := enrol.Install(enrol.InstallOptions{
				PKIDir: f.pkiDir, UID: signer.EUID(), CSR: csr, Certificate: cert, Trust: f.cfg, Signer: f.key, Now: f.now,
			})
			require.ErrorIs(t, err, enrol.ErrInstall)
		})
	}
	// A request with a non-enrolment URI set is refused even if the CA would
	// have signed it.
	other, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		URIs: hostidentitytest.ParseURIs(t, []string{f.pki.HostURI}),
	}, f.key)
	require.NoError(t, err)
	_, err = enrol.Install(enrol.InstallOptions{
		PKIDir: f.pkiDir, UID: signer.EUID(), Certificate: cert, Trust: f.cfg, Signer: f.key, Now: f.now,
		CSR: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: other}),
	})
	require.ErrorIs(t, err, enrol.ErrInstall)
}

func TestInstallReplacement(t *testing.T) {
	f := newFixture(t)
	first := f.issue(t, hostidentitytest.IssueOptions{})
	_, err := f.install(first, false)
	require.NoError(t, err)
	before, err := os.ReadFile(f.hostCrt())
	require.NoError(t, err)

	_, err = f.install(first, false)
	require.ErrorIs(t, err, enrol.ErrExists, "an installed certificate is not replaced without --replace")
	_, err = f.install(f.issue(t, hostidentitytest.IssueOptions{}), true)
	require.ErrorIs(t, err, enrol.ErrExists, "the same enrolment is not installed again")
	after, err := os.ReadFile(f.hostCrt())
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refusal leaves the installed certificate")

	// A new enrolment record (re-enrolment) replaces it.
	next := f.rec
	next.EnrolmentID = bytes16(0xD0)
	f.uris = hostidentity.RequestURIs(f.pki.HostURI, next)
	f.csr, err = enrol.BuildCSR(f.key, f.uris)
	require.NoError(t, err)
	res, err := f.install(f.issue(t, hostidentitytest.IssueOptions{}), true)
	require.NoError(t, err)
	assert.True(t, res.Replaced)
	assert.True(t, res.Record.Equal(next))
	now, err := os.ReadFile(f.hostCrt())
	require.NoError(t, err)
	assert.NotEqual(t, before, now)
}

func TestInstallIncompleteOptions(t *testing.T) {
	_, err := enrol.Install(enrol.InstallOptions{})
	require.ErrorIs(t, err, enrol.ErrInstall)
}

func TestHostNameAndParseURIs(t *testing.T) {
	f := newFixture(t)
	name, err := enrol.HostName(f.pki.HostURI)
	require.NoError(t, err)
	assert.Equal(t, trusttest.HostName, name)
	_, err = enrol.HostName("urn:virtrigaud:mode:identity")
	require.ErrorIs(t, err, enrol.ErrRequest)
	parsed, host, err := enrol.ParseURIs(f.uris)
	require.NoError(t, err)
	assert.Len(t, parsed, 5)
	assert.Equal(t, f.pki.HostURI, host)
}
