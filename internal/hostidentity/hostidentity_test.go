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
	"crypto/x509"
	"crypto/x509/pkix"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

const (
	testAdminGroup  = "virtrigaud:enrolment-admins"
	testHostName    = "host-a"
	testProviderUID = "0b9a3f2e-5d1c-4f7a-9e6b-2c8d1a4b7e90"
)

func testRecord() hostidentity.Record {
	return hostidentity.Record{
		EnrolmentID: bytes.Repeat([]byte{0xa1}, 16),
		ProviderUID: testProviderUID,
		InstallID:   bytes.Repeat([]byte{0x5c}, 16),
		Mode:        hostidentity.ModeIdentity,
	}
}

func testHostURI(t *testing.T) string {
	t.Helper()
	u, err := hostidentity.HostURI("cluster.test", "infra", testHostName)
	require.NoError(t, err)
	return u
}

func testPolicy(t *testing.T) hostidentity.Policy {
	return hostidentity.Policy{AdminGroup: testAdminGroup, HostName: testHostName, HostURI: testHostURI(t), Record: testRecord()}
}

func goodRequest(csr []byte) hostidentity.Request {
	return hostidentity.Request{
		CSRPEM:   csr,
		Usages:   []string{hostidentity.UsageServerAuth, hostidentity.UsageDigitalSignature},
		Duration: 30 * 24 * time.Hour,
		Username: "alice",
		Groups:   []string{"system:authenticated", testAdminGroup},
	}
}

func TestHostURIShape(t *testing.T) {
	assert.Equal(t, "spiffe://cluster.test/virtrigaud/host/infra/host-a", testHostURI(t))
	assert.NoError(t, hostidentity.ValidateHostURI(testHostURI(t)))
	for _, bad := range []string{"spiffe://Cluster/virtrigaud/host/a/b", "https://x/virtrigaud/host/a/b",
		"spiffe://x/virtrigaud/host/a", "spiffe://x/virtrigaud/host/a/b?q=1", "spiffe://x/virtrigaud/host/a/b/c"} {
		assert.Error(t, hostidentity.ValidateHostURI(bad), bad)
	}
	_, err := hostidentity.HostURI("cluster.test", "infra", "Bad_Name")
	assert.Error(t, err)
}

func TestRecordRoundTripsThroughURIs(t *testing.T) {
	rec := testRecord()
	uris := hostidentity.RequestURIs(testHostURI(t), rec)
	got, err := hostidentity.RecordFromURIs(hostidentitytest.ParseURIs(t, uris), testHostURI(t))
	require.NoError(t, err)
	assert.True(t, rec.Equal(got))
	assert.Equal(t, "urn:virtrigaud:enrolment:a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", uris[1])
	assert.Equal(t, "urn:virtrigaud:mode:identity", uris[4])
}

func TestRecordFromURIsRefusesAnythingButTheExactSet(t *testing.T) {
	host := testHostURI(t)
	base := hostidentity.RequestURIs(host, testRecord())
	cases := map[string][]string{
		"missing host URI":     append([]string{"urn:virtrigaud:other"}, base[1:]...),
		"other host URI":       append([]string{strings.Replace(host, testHostName, "host-b", 1)}, base[1:]...),
		"extra URI":            append(append([]string{}, base...), "urn:virtrigaud:extra"),
		"missing mode":         base[:4],
		"repeated enrolment":   {host, base[1], base[1], base[2], base[3]},
		"uppercase enrolment":  {host, strings.ToUpper(base[1]), base[2], base[3], base[4]},
		"short enrolment id":   {host, "urn:virtrigaud:enrolment:abcd", base[2], base[3], base[4]},
		"provider uid garbage": {host, base[1], "urn:virtrigaud:provider:nope", base[3], base[4]},
	}
	for name, uris := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := hostidentity.RecordFromURIs(hostidentitytest.ParseURIs(t, uris), host)
			assert.ErrorIs(t, err, hostidentity.ErrRecord)
		})
	}
}

func TestAuthoriseAcceptsAWellFormedRequest(t *testing.T) {
	key := hostidentitytest.NewKey(t)
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{CommonName: testHostName, URIs: hostidentity.RequestURIs(testHostURI(t), testRecord())})
	auth, err := hostidentity.Authorise(testPolicy(t), goodRequest(csr))
	require.NoError(t, err)
	pin, err := hostidentity.SPKIPin(&key.PublicKey)
	require.NoError(t, err)
	assert.Equal(t, pin, auth.KeyPin)
	assert.NoError(t, hostidentity.ValidatePin(pin))
	assert.True(t, auth.Record.Equal(testRecord()))
}

func TestAuthoriseDeniesEachBrokenRule(t *testing.T) {
	host := testHostURI(t)
	uris := hostidentity.RequestURIs(host, testRecord())
	key := hostidentitytest.NewKey(t)
	good := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: uris})
	otherRecord := testRecord()
	otherRecord.EnrolmentID = bytes.Repeat([]byte{0x01}, 16)

	cases := []struct {
		name   string
		mutate func(*hostidentity.Request)
		reason string
	}{
		{"requester outside the admin group", func(r *hostidentity.Request) { r.Groups = []string{"system:authenticated"} }, hostidentity.ReasonRequesterNotAuthorised},
		{"no groups at all", func(r *hostidentity.Request) { r.Groups = nil }, hostidentity.ReasonRequesterNotAuthorised},
		{"missing proof of possession", func(r *hostidentity.Request) {
			r.CSRPEM = hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: uris, BreakSignature: true})
		}, hostidentity.ReasonProofOfPossession},
		{"SAN for another host", func(r *hostidentity.Request) {
			other, err := hostidentity.HostURI("cluster.test", "infra", "host-b")
			require.NoError(t, err)
			r.CSRPEM = hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(other, testRecord())})
		}, hostidentity.ReasonSubjectAltNames},
		{"a DNS name besides the URIs", func(r *hostidentity.Request) {
			r.CSRPEM = hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: uris, DNSNames: []string{"host-a.example.com"}})
		}, hostidentity.ReasonSubjectAltNames},
		{"a record the manager did not draw", func(r *hostidentity.Request) {
			r.CSRPEM = hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(host, otherRecord)})
		}, hostidentity.ReasonSubjectAltNames},
		{"lifetime over thirty days", func(r *hostidentity.Request) { r.Duration = 31 * 24 * time.Hour }, hostidentity.ReasonLifetime},
		{"no lifetime", func(r *hostidentity.Request) { r.Duration = 0 }, hostidentity.ReasonLifetime},
		{"CA requested in the spec", func(r *hostidentity.Request) { r.IsCA = true }, hostidentity.ReasonCARequested},
		{"CA bit in the CSR", func(r *hostidentity.Request) {
			r.CSRPEM = hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: uris, RequestCA: true})
		}, hostidentity.ReasonCARequested},
		{"usages without server auth", func(r *hostidentity.Request) { r.Usages = []string{hostidentity.UsageDigitalSignature} }, hostidentity.ReasonUsages},
		{"client auth only", func(r *hostidentity.Request) { r.Usages = []string{hostidentity.UsageClientAuth} }, hostidentity.ReasonUsages},
		{"client auth with server auth", func(r *hostidentity.Request) {
			r.Usages = []string{hostidentity.UsageClientAuth, hostidentity.UsageServerAuth}
		}, hostidentity.ReasonUsages},
		{"an extra usage", func(r *hostidentity.Request) { r.Usages = append(r.Usages, "cert sign") }, hostidentity.ReasonUsages},
		{"default usages", func(r *hostidentity.Request) { r.Usages = nil }, hostidentity.ReasonUsages},
		{"a subject other than the host name", func(r *hostidentity.Request) {
			r.CSRPEM = hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: uris, CommonName: "host-b"})
		}, hostidentity.ReasonSubject},
		{"not a CSR", func(r *hostidentity.Request) { r.CSRPEM = []byte("garbage") }, hostidentity.ReasonRequestInvalid},
		{"trailing data", func(r *hostidentity.Request) { r.CSRPEM = append(append([]byte{}, good...), good...) }, hostidentity.ReasonRequestInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := goodRequest(good)
			tc.mutate(&req)
			_, err := hostidentity.Authorise(testPolicy(t), req)
			d, ok := hostidentity.AsDenial(err)
			require.True(t, ok, "want a denial, got %v", err)
			assert.Equal(t, tc.reason, d.Reason)
			assert.NotContains(t, d.Message, "BEGIN")
		})
	}
}

func TestValidateIssued(t *testing.T) {
	host := testHostURI(t)
	ca := hostidentitytest.NewCA(t)
	roots, err := hostidentity.RootPool(ca.PEM)
	require.NoError(t, err)
	key := hostidentitytest.NewKey(t)
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(host, testRecord())})
	auth, err := hostidentity.Authorise(testPolicy(t), goodRequest(csr))
	require.NoError(t, err)

	issued, err := hostidentity.ValidateIssued(auth, ca.Issue(t, csr, hostidentitytest.IssueOptions{}), roots, time.Now())
	require.NoError(t, err)
	assert.Equal(t, auth.KeyPin, issued.KeyPin)
	assert.True(t, issued.Record.Equal(testRecord()), "the enrolment record is derived from the certificate's SANs")
	_, err = hostidentity.ValidateIssued(auth, ca.Issue(t, csr, hostidentitytest.IssueOptions{Subject: &pkix.Name{CommonName: testHostName}}), roots, time.Now())
	require.NoError(t, err, "a common name equal to the host name is allowed")

	otherCA := hostidentitytest.NewCA(t)
	otherHost, err := hostidentity.HostURI("cluster.test", "infra", "host-b")
	require.NoError(t, err)
	cases := map[string][]byte{
		"another CA":             otherCA.Issue(t, csr, hostidentitytest.IssueOptions{}),
		"another key":            ca.Issue(t, csr, hostidentitytest.IssueOptions{PublicKey: &hostidentitytest.NewKey(t).PublicKey}),
		"another host's SAN":     ca.Issue(t, csr, hostidentitytest.IssueOptions{URIs: hostidentity.RequestURIs(otherHost, testRecord())}),
		"lifetime too long":      ca.Issue(t, csr, hostidentitytest.IssueOptions{Lifetime: 40 * 24 * time.Hour}),
		"client and server auth": ca.Issue(t, csr, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}),
		"server auth and any":    ca.Issue(t, csr, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageAny}}),
		"client auth only":       ca.Issue(t, csr, hostidentitytest.IssueOptions{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}),
		"a CA":                   ca.Issue(t, csr, hostidentitytest.IssueOptions{IsCA: true}),
		"another subject":        ca.Issue(t, csr, hostidentitytest.IssueOptions{Subject: &pkix.Name{CommonName: "host-b"}}),
		"extra subject RDNs":     ca.Issue(t, csr, hostidentitytest.IssueOptions{Subject: &pkix.Name{CommonName: testHostName, Organization: []string{"x"}}}),
		"not PEM":                []byte("nope"),
	}
	for name, chain := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := hostidentity.ValidateIssued(auth, chain, roots, time.Now())
			assert.ErrorIs(t, err, hostidentity.ErrIssued)
		})
	}
	_, err = hostidentity.ValidateIssued(auth, ca.Issue(t, csr, hostidentitytest.IssueOptions{}), roots, time.Now().Add(60*24*time.Hour))
	assert.ErrorIs(t, err, hostidentity.ErrIssued, "an expired certificate is refused")
	_, err = hostidentity.RootPool(ca.Issue(t, csr, hostidentitytest.IssueOptions{}))
	assert.Error(t, err, "a CA bundle holding a leaf is refused")
}
