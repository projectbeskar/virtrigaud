//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// renewalInventory is the agent's entry with a renewed certificate for
// newPin's key, pinned or not.
func renewalInventory(t *testing.T, a *fakeAgent, renewed []byte, pins ...string) hostsecret.Inventory {
	t.Helper()
	h := a.entry()
	h.RenewedCertificate = renewed
	h.Credentials.AgentTLS.KeyPins = append(h.Credentials.AgentTLS.KeyPins, pins...)
	return hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{h}}
}

// TestRenewedCertificateRelayed: a renewed certificate in the inventory is
// staged (activate=false) before the next call while its key is not pinned,
// once per connection; once the inventory pins its key as well, the same
// bytes are activated (activate=true), once; the calls succeed throughout.
func TestRenewedCertificateRelayed(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	newKey := hostidentitytest.NewKey(t)
	renewed := hostCert(t, p.ca, "host-a", testProviderUID, newKey).Raw
	newPin, err := hostidentity.SPKIPin(&newKey.PublicKey)
	require.NoError(t, err)
	prov := newProvider(t, p, Options{}, a)
	client := providerv1.NewProviderClient(served(t, prov))
	describe := func() {
		t.Helper()
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err)
	}
	describe()
	assert.Empty(t, a.installRequests(), "no renewed certificate, nothing relayed")

	require.NoError(t, prov.Reconcile(renewalInventory(t, a, renewed)))
	describe()
	describe()
	got := a.installRequests()
	require.Len(t, got, 1, "staged once per connection")
	assert.False(t, got[0].GetActivate())
	assert.Equal(t, renewed, got[0].GetCertificate())
	assert.Len(t, got[0].GetAnswerNonce(), nonceLen)

	require.NoError(t, prov.Reconcile(renewalInventory(t, a, renewed, newPin)))
	require.Eventually(t, func() bool {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		return err == nil
	}, testTimeout, 10*time.Millisecond)
	describe()
	got = a.installRequests()
	require.Len(t, got, 2, "activated once")
	assert.True(t, got[1].GetActivate())
	assert.Equal(t, renewed, got[1].GetCertificate(), "the byte-identical certificate")

	// An invalid renewed certificate is dropped; the host stays routed.
	require.NoError(t, prov.Reconcile(renewalInventory(t, a, []byte("not a certificate"))))
	require.Eventually(t, func() bool {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		return err == nil
	}, testTimeout, 10*time.Millisecond, "the host stays routed")
	assert.Len(t, a.installRequests(), 2, "an invalid certificate is not relayed")
}

// TestRenewedCertificateToAnAgentThatServesNone: an agent that answers
// Unimplemented is not asked again on that connection, and calls to it
// succeed without tripping its breaker.
func TestRenewedCertificateToAnAgentThatServesNone(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	a.installMode.Store(1)
	renewed := hostCert(t, p.ca, "host-a", testProviderUID, hostidentitytest.NewKey(t)).Raw
	prov := newProvider(t, p, Options{})
	require.NoError(t, prov.Reconcile(renewalInventory(t, a, renewed)))
	client := providerv1.NewProviderClient(served(t, prov))
	for i := 0; i < 2*DefaultBreakerThreshold+1; i++ {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err, "call %d", i)
	}
	assert.EqualValues(t, 1, a.installCalls.Load(), "Unimplemented: not asked again on the same connection")
}

// TestRenewedCertificateRelayBacksOff: a failed relay is tried again only
// after a growing backoff; the calls go through.
func TestRenewedCertificateRelayBacksOff(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	a.installMode.Store(2)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	renewed := hostCert(t, p.ca, "host-a", testProviderUID, hostidentitytest.NewKey(t)).Raw
	prov := newProvider(t, p, Options{Now: clock})
	require.NoError(t, prov.Reconcile(renewalInventory(t, a, renewed)))
	client := providerv1.NewProviderClient(served(t, prov))
	describe := func() {
		t.Helper()
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err)
	}
	describe()
	describe()
	assert.EqualValues(t, 1, a.installCalls.Load(), "not again before the backoff")
	advance(renewalRetryMin + time.Second)
	describe()
	assert.EqualValues(t, 2, a.installCalls.Load())
	advance(renewalRetryMin + time.Second)
	describe()
	assert.EqualValues(t, 2, a.installCalls.Load(), "the backoff doubled")
	a.installMode.Store(0)
	advance(2 * renewalRetryMin)
	describe()
	describe()
	assert.EqualValues(t, 3, a.installCalls.Load(), "delivered, then not again on this connection")
}
