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
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// TestPinnedOnHost: --certificate-out's check needs both the key pin and the
// enrolment id of the Host's status.agent; either alone is refused.
func TestPinnedOnHost(t *testing.T) {
	ca := hostidentitytest.NewCA(t)
	key := hostidentitytest.NewKey(t)
	hostURI, err := hostidentity.HostURI(testTrustDomain, "infra", "host-a")
	require.NoError(t, err)
	rec := hostidentity.Record{EnrolmentID: bytes.Repeat([]byte{0xa1}, 16), ProviderUID: "0b7a1c3e-1111-4222-8333-944455556666",
		InstallID: bytes.Repeat([]byte{7}, 16), Mode: hostidentity.ModeIdentity}
	csr := hostidentitytest.NewCSR(t, key, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(hostURI, rec)})
	certs, err := hostidentity.ParseCertificates(ca.Issue(t, csr, hostidentitytest.IssueOptions{}))
	require.NoError(t, err)
	leaf := certs[0]
	pin, err := hostidentity.SPKIPin(&key.PublicKey)
	require.NoError(t, err)
	enrolment := hex.EncodeToString(rec.EnrolmentID)
	other := hex.EncodeToString(bytes.Repeat([]byte{0xb2}, 16))

	for name, c := range map[string]struct {
		agent *infrav1beta1.HostAgentStatus
		want  bool
	}{
		"pinned":                     {&infrav1beta1.HostAgentStatus{EnrolmentID: enrolment, KeyPins: []string{pin}}, true},
		"pin matches, enrolment not": {&infrav1beta1.HostAgentStatus{EnrolmentID: other, KeyPins: []string{pin}}, false},
		"enrolment matches, pin not": {&infrav1beta1.HostAgentStatus{EnrolmentID: enrolment, KeyPins: []string{"00" + pin[2:]}}, false},
		"no agent status":            {nil, false},
		"not enrolled":               {&infrav1beta1.HostAgentStatus{KeyPins: []string{pin}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := pinnedOnHost(c.agent, leaf)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}
