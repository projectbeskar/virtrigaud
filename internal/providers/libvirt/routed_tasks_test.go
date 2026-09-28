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
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the host-encoded task references of a clustered provider
// (ADR-0007 Addendum A, A1, slice 3): the format, the refusal of any reference
// the provider did not issue, and that TaskStatus is routed to the encoded
// host — and never dials anything else.

func TestHostTaskRef_RoundTrip(t *testing.T) {
	ref := encodeHostTaskRef("host-a.rack-1", "job/42:x")
	assert.Equal(t, "host-task/v1/host-a.rack-1/job/42:x", ref)
	host, inner, err := parseHostTaskRef(ref)
	require.NoError(t, err)
	assert.Equal(t, hostconn.HostID("host-a.rack-1"), host)
	assert.Equal(t, "job/42:x", inner, "the host-local reference is kept verbatim, '/' included")

	assert.Empty(t, encodeHostTaskRef("host-a", ""), "a synchronous call has no task, and no reference")
}

func TestHostTaskRef_ParseRefusesMalformed(t *testing.T) {
	for name, ref := range map[string]string{
		"empty":                    "",
		"not host-encoded":         "task-123",
		"other version":            "host-task/v2/host-a/t",
		"no task":                  "host-task/v1/host-a",
		"empty task":               "host-task/v1/host-a/",
		"empty host":               "host-task/v1//t",
		"uppercase host":           "host-task/v1/Host-A/t",
		"host with a port":         "host-task/v1/host-a:22/t",
		"host is a path":           "host-task/v1/../t",
		"host is an endpoint":      "host-task/v1/qemu+ssh:/t",
		"host with leading hyphen": "host-task/v1/-host/t",
		"too long":                 "host-task/v1/host-a/" + strings.Repeat("x", hostTaskRefMaxLen),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseHostTaskRef(ref)
			assert.Error(t, err)
		})
	}
}

// taskCluster is a clustered provider over host-a, host-b and host-down (whose
// dial fails), recording every host it dials.
func taskCluster(t *testing.T) (*Provider, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var dialed []string
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{
		{ID: "host-a", Endpoint: "qemu+ssh://virt@host-a/system"},
		{ID: "host-b", Endpoint: "qemu+ssh://virt@host-b/system"},
		{ID: "host-down", Endpoint: "qemu+ssh://virt@host-down/system"},
	}}
	dial := func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) {
		mu.Lock()
		dialed = append(dialed, h.ID)
		mu.Unlock()
		if h.ID == "host-down" {
			return nil, errors.New("dial tcp: connection refused")
		}
		return newClusteredVirshConn(hostconn.HostID(h.ID), localHostVP(h.ID), func() {}), nil
	}
	p, _ := newClusteredProviderForTest(t, inv, dial)
	p.virshProvider = newUnroutableVirshProvider()
	return p, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), dialed...)
	}
}

func TestClustered_TaskStatus_RoutedToTheEncodedHost(t *testing.T) {
	p, dialed := taskCluster(t)
	resp, err := NewServer(p).TaskStatus(context.Background(), &providerv1.TaskStatusRequest{
		Task: &providerv1.TaskRef{Id: encodeHostTaskRef("host-b", "job-1")},
	})
	require.NoError(t, err)
	assert.True(t, resp.Done)
	assert.Empty(t, resp.Error)
	assert.Equal(t, []string{"host-b"}, dialed(), "only the encoded host is leased")
	assert.Zero(t, p.virshProvider.unroutableHits.Load())
}

// TestClustered_TaskStatus_RefusesForgedReferences: a reference the provider
// did not issue is refused as a gRPC error — never reported as a task result —
// and NOTHING is dialed for it: a host id not in the registry never becomes an
// endpoint.
func TestClustered_TaskStatus_RefusesForgedReferences(t *testing.T) {
	for name, tc := range map[string]struct {
		ref  *providerv1.TaskRef
		code codes.Code
	}{
		"single-host style reference": {&providerv1.TaskRef{Id: "task-123"}, codes.InvalidArgument},
		"no task at all":              {nil, codes.InvalidArgument},
		"malformed host id":           {&providerv1.TaskRef{Id: "host-task/v1/Evil:22/t"}, codes.InvalidArgument},
		"unknown host":                {&providerv1.TaskRef{Id: "host-task/v1/host-zzz/t"}, codes.NotFound},
		"attacker-chosen host name":   {&providerv1.TaskRef{Id: "host-task/v1/attacker.example.com/t"}, codes.NotFound},
	} {
		t.Run(name, func(t *testing.T) {
			p, dialed := taskCluster(t)
			resp, err := NewServer(p).TaskStatus(context.Background(), &providerv1.TaskStatusRequest{Task: tc.ref})
			require.Error(t, err)
			assert.Nil(t, resp)
			assert.Equal(t, tc.code, status.Code(err), "got %v", err)
			assert.Empty(t, dialed(), "a refused reference never dials anything")
			assert.Zero(t, p.virshProvider.unroutableHits.Load(), "nor falls through to the single-host placeholder")
		})
	}
}

// TestClustered_TaskStatus_UnreachableHostIsHostScoped: a host of the registry
// that cannot be reached is the retryable, host-scoped Unavailable the manager
// keeps out of its circuit breaker — not a task failure.
func TestClustered_TaskStatus_UnreachableHostIsHostScoped(t *testing.T) {
	p, dialed := taskCluster(t)
	_, err := NewServer(p).TaskStatus(context.Background(), &providerv1.TaskStatusRequest{
		Task: &providerv1.TaskRef{Id: encodeHostTaskRef("host-down", "job-1")},
	})
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.True(t, hasHostUnavailableInfo(st), "an unreachable task host is host-scoped")
	assert.NotContains(t, st.Message(), "qemu+ssh", "the host's endpoint is never disclosed")
	assert.Equal(t, []string{"host-down"}, dialed())
}

// TestSingleHost_TaskStatus_ReferencesUnchanged: a single-host provider never
// parses a reference — even one that looks host-encoded — so its answers are
// exactly the historical ones (also pinned by the slice 3 golden file).
func TestSingleHost_TaskStatus_ReferencesUnchanged(t *testing.T) {
	p := &Provider{virshProvider: localHostVP("single")}
	for _, ref := range []string{"task-123", "host-task/v1/host-zzz/t", ""} {
		done, err := p.IsTaskComplete(context.Background(), ref)
		require.NoError(t, err)
		assert.True(t, done, ref)
	}
}
