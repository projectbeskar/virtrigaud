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
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// verifyUnder verifies certPEM against ca alone, as the manager, the
// Provider and the agent do.
func verifyUnder(t *testing.T, ca *hostidentitytest.CA, certPEM []byte) error {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	leaf, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err
}

// The host-agent sub-CA carries a URI name constraint permitting only the
// trust domain. Every URI of an agent certificate has the trust domain as
// its host, so such a sub-CA issues certificates that verify; a URI under
// another trust domain, or one without a host (the former urn: record
// form), fails verification, which is what binds a stolen sub-CA key.
func TestConstrainedSubCAIssuesVerifiableAgentCertificates(t *testing.T) {
	const td = "cluster.test"
	ca := hostidentitytest.NewConstrainedCA(t, td)
	require.Equal(t, []string{td}, ca.Cert.PermittedURIDomains)
	key := hostidentitytest.NewKey(t)
	hostURI, err := hostidentity.HostURI(td, "infra", testHostName)
	require.NoError(t, err)
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(hostURI, testRecord())})

	later := testRecord()
	later.StartEpoch = 3
	for name, uris := range map[string][]string{
		"first epoch": hostidentity.RequestURIs(hostURI, testRecord()),
		"later epoch": hostidentity.RequestURIs(hostURI, later),
	} {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, verifyUnder(t, ca, ca.Issue(t, csr, hostidentitytest.IssueOptions{URIs: uris})))
		})
	}

	good := hostidentity.RequestURIs(hostURI, testRecord())
	for name, uris := range map[string][]string{
		"a host URI under another trust domain": append([]string{strings.Replace(good[0], td, "other.test", 1)}, good[1:]...),
		"a record URI under another trust domain": {good[0], strings.Replace(good[1], td, "other.test", 1), good[2], good[3],
			good[4]},
		"a URI without a host (the former urn form)": append(append([]string{}, good...), "urn:virtrigaud:mode:identity"),
	} {
		t.Run(name, func(t *testing.T) {
			err := verifyUnder(t, ca, ca.Issue(t, csr, hostidentitytest.IssueOptions{URIs: uris}))
			var invalid x509.CertificateInvalidError
			require.ErrorAs(t, err, &invalid)
			assert.Equal(t, x509.CANotAuthorizedForThisName, invalid.Reason)
		})
	}
}
