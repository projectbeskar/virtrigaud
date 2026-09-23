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

package libvirt

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// These tests pin the ADR-0007 P1 clustered create-on-host binding: a clustered
// libvirt provider routes Create onto the host named by target_host_id, always
// releasing the per-borrow connection lease (non-severing), and rejects an empty
// target_host_id with a typed error rather than defaulting to some host. They
// reuse the fakeHostConn / fakeDialer / newClusteredProviderForTest harness in
// hostinfo_test.go so lease accounting runs the real ClusterRegistry code path.
//
// The create pipeline itself (storage + cloud-init + domain define) needs a live
// libvirtd, so these tests inject the p.createOnHostFn seam — mirroring the
// describeNativeFn/listNativeFn seams — to assert host SELECTION and lease
// RELEASE without a host. The narrowing that the production createOnHostFn
// performs (lease -> *virshConn -> VirshProvider) is pinned separately by
// TestVirshConnFrom_UnwrapsLeaseToVirshConn.

func twoHostInventory() hostsecret.Inventory {
	return hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts: []hostsecret.Host{
			{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system"},
			{ID: "host-b", Endpoint: "qemu+ssh://virt@host-b/system"},
		},
	}
}

func TestCreate_Clustered_RoutesToTargetHostAndReleasesLeaseOnError(t *testing.T) {
	conns := map[hostconn.HostID]*fakeHostConn{
		"host-a": {id: "host-a"},
		"host-b": {id: "host-b"},
	}
	p, reg := newClusteredProviderForTest(t, twoHostInventory(), fakeDialer(conns, nil))

	var gotHost hostconn.HostID
	boom := errors.New("create failed on host")
	p.createOnHostFn = func(_ context.Context, lease hostconn.Conn, _ contracts.CreateRequest) (contracts.CreateResponse, error) {
		gotHost = lease.HostID()
		return contracts.CreateResponse{}, boom
	}

	_, err := p.Create(context.Background(), contracts.CreateRequest{Name: "vm1", TargetHostID: "host-b"})
	require.ErrorIs(t, err, boom, "the create error must propagate unchanged")
	require.Equal(t, hostconn.HostID("host-b"), gotHost, "create must be routed to the target host")

	// Lease released even on the error path: Close releases the lease but does not
	// close the shared connection, so Evict (idle) closes it inline. A leaked
	// lease would park host-b in draining and leave it unclosed.
	require.Equal(t, 0, conns["host-b"].closeCount(), "lease Close must not close the shared connection")
	reg.Evict("host-b")
	require.Equal(t, 1, conns["host-b"].closeCount(), "released lease lets Evict close the connection")
	require.Equal(t, 0, conns["host-a"].closeCount(), "a non-target host is never dialed")
}

func TestCreate_Clustered_RoutesToTargetHostOnSuccess(t *testing.T) {
	conns := map[hostconn.HostID]*fakeHostConn{
		"host-a": {id: "host-a"},
		"host-b": {id: "host-b"},
	}
	p, reg := newClusteredProviderForTest(t, twoHostInventory(), fakeDialer(conns, nil))

	var gotHost hostconn.HostID
	p.createOnHostFn = func(_ context.Context, lease hostconn.Conn, req contracts.CreateRequest) (contracts.CreateResponse, error) {
		gotHost = lease.HostID()
		return contracts.CreateResponse{ID: req.Name}, nil
	}

	resp, err := p.Create(context.Background(), contracts.CreateRequest{Name: "vm2", TargetHostID: "host-a"})
	require.NoError(t, err)
	require.Equal(t, "vm2", resp.ID)
	require.Equal(t, hostconn.HostID("host-a"), gotHost)

	require.Equal(t, 0, conns["host-a"].closeCount())
	reg.Evict("host-a")
	require.Equal(t, 1, conns["host-a"].closeCount(), "released lease lets Evict close the connection")
}

func TestCreate_Clustered_EmptyTargetHostIDTypedError(t *testing.T) {
	conns := map[hostconn.HostID]*fakeHostConn{"host-a": {id: "host-a"}}
	inv := hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts:         []hostsecret.Host{{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system"}},
	}
	p, _ := newClusteredProviderForTest(t, inv, fakeDialer(conns, nil))

	called := false
	p.createOnHostFn = func(_ context.Context, _ hostconn.Conn, _ contracts.CreateRequest) (contracts.CreateResponse, error) {
		called = true
		return contracts.CreateResponse{}, nil
	}

	// Whitespace-only is treated as empty (TrimSpace) — the operator must supply a
	// real host, never a blank that silently defaults.
	_, err := p.Create(context.Background(), contracts.CreateRequest{Name: "vm", TargetHostID: "   "})
	require.Error(t, err)

	var pe *contracts.ProviderError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, contracts.ErrorTypeInvalidSpec, pe.Type, "empty target_host_id must be a typed InvalidSpec error")
	require.Contains(t, err.Error(), "target_host_id")

	require.False(t, called, "must not lease or create on any host when target_host_id is empty")
	require.Equal(t, 0, conns["host-a"].closeCount(), "no host is dialed when target_host_id is empty")
}

// TestVirshConnFrom_UnwrapsLeaseToVirshConn pins the production narrowing the
// clustered create path relies on: a ClusterRegistry lease Unwraps to the
// underlying *virshConn (and thus its VirshProvider), a bare *virshConn passes
// through, and a non-virsh connection is a clean error rather than a nil-deref.
func TestVirshConnFrom_UnwrapsLeaseToVirshConn(t *testing.T) {
	vp := NewVirshProvider(&ProviderConfig{Spec: ProviderSpec{Endpoint: "qemu+ssh://virt@host-a/system"}})
	vc := newClusteredVirshConn("host-a", vp, nil)

	inv := hostsecret.Inventory{
		SchemaVersion: hostsecret.SchemaVersion,
		Hosts:         []hostsecret.Host{{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system"}},
	}
	dial := func(_ context.Context, _ hostsecret.Host) (hostconn.Conn, error) { return vc, nil }
	reg, err := hostconn.NewClusterRegistry(inv, dial, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })

	lease, err := reg.ConnFor(context.Background(), "host-a")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })

	got, err := virshConnFrom(lease)
	require.NoError(t, err)
	require.Same(t, vc, got, "virshConnFrom must unwrap the lease to the underlying *virshConn")
	require.Same(t, vp, got.virsh, "the unwrapped virshConn must carry the target host's VirshProvider")

	// A bare *virshConn (not behind a lease) passes through unchanged.
	gotBare, err := virshConnFrom(vc)
	require.NoError(t, err)
	require.Same(t, vc, gotBare)

	// A non-virsh connection is an error, not a nil-deref.
	_, err = virshConnFrom(&fakeHostConn{id: "x"})
	require.Error(t, err)
}

// TestCreate_SingleHost_IgnoresTargetHostID proves a single-host provider
// (clusterReg nil) ignores target_host_id: it takes the single-host branch and
// fails with "virsh provider not initialized" (retryable) rather than the
// clustered "requires target_host_id" InvalidSpec — so the field is inert in
// single-host mode (D9, byte-for-byte unchanged).
func TestCreate_SingleHost_IgnoresTargetHostID(t *testing.T) {
	p := &Provider{} // clusterReg nil => clustered() == false, virshProvider nil
	require.False(t, p.clustered())

	_, err := p.Create(context.Background(), contracts.CreateRequest{Name: "vm", TargetHostID: "host-x"})
	require.Error(t, err)

	var pe *contracts.ProviderError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, contracts.ErrorTypeRetryable, pe.Type)
	require.NotContains(t, err.Error(), "target_host_id", "single-host mode must ignore target_host_id, not demand it")
}
