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

package vsphere

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// testFinder returns a finder of the test's own, scoped to p's default datacenter.
func testFinder(t *testing.T, p *Provider) *find.Finder {
	t.Helper()
	finder, _, err := p.dcFinder(context.Background())
	require.NoError(t, err)
	return finder
}

// testDatacenter returns p's default datacenter.
func testDatacenter(t *testing.T, p *Provider) *object.Datacenter {
	t.Helper()
	_, dc, err := p.dcFinder(context.Background())
	require.NoError(t, err)
	return dc
}

// TestDCFinder_NoClient: a provider whose vCenter connection failed has no
// client; dcFinder says so instead of dereferencing nil.
func TestDCFinder_NoClient(t *testing.T) {
	p := &Provider{}
	finder, dc, err := p.dcFinder(context.Background())
	require.ErrorIs(t, err, errVSphereClientNotInitialised)
	assert.Nil(t, finder)
	assert.Nil(t, dc)
}

// TestDCFinder_FinderPerCall: every call gets a finder of its own, scoped to the
// default datacenter, and its own copy of the datacenter, so nothing one RPC
// does to them reaches another RPC or the cache.
func TestDCFinder_FinderPerCall(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	f1, dc1, err := p.dcFinder(ctx)
	require.NoError(t, err)
	f2, dc2, err := p.dcFinder(ctx)
	require.NoError(t, err)

	assert.NotSame(t, f1, f2, "each call builds its own finder")
	assert.NotSame(t, dc1, dc2, "each call gets its own datacenter object")
	assert.Equal(t, dc1.Reference(), dc2.Reference())

	// The finder is scoped: a relative datastore name resolves with no further setup.
	ds, err := f1.Datastore(ctx, simDatastore)
	require.NoError(t, err)
	assert.Equal(t, simDatastore, ds.Name())

	// Changing the copy a caller holds does not change what the next caller gets.
	dc1.InventoryPath = "/tampered"
	_, dc3, err := p.dcFinder(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, "/tampered", dc3.InventoryPath)
}

// TestDCFinder_AmbiguousDatacenterIsNotCached: with several datacenters and no
// default, the error govmomi reports is returned each time and nothing is cached.
func TestDCFinder_AmbiguousDatacenterIsNotCached(t *testing.T) {
	model := simulator.VPX()
	model.Datacenter = 2
	require.NoError(t, model.Create())
	server := model.Service.NewServer()
	u := server.URL
	pw, _ := u.User.Password()
	cfg := &Config{
		Endpoint:           (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String(),
		Username:           u.User.Username(),
		Password:           pw,
		InsecureSkipVerify: true,
	}
	client, err := createVSphereClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = client.Logout(context.Background())
		server.Close()
		model.Remove()
	})
	p := &Provider{client: client, config: cfg}
	ctx := context.Background()

	for range 2 {
		finder, dc, err := p.dcFinder(ctx)
		require.Error(t, err)
		assert.Nil(t, finder)
		assert.Nil(t, dc)
	}
	_, cached := p.dcCache.get(p.client, time.Now())
	assert.False(t, cached, "a failed lookup is not cached")
}

// TestDatacenterCache covers the cache rules: a hit only for the client the
// datacenter was found through and only while younger than the TTL.
func TestDatacenterCache(t *testing.T) {
	clientA, clientB := &govmomi.Client{}, &govmomi.Client{}
	dc := object.NewDatacenter(nil, types.ManagedObjectReference{Type: "Datacenter", Value: "datacenter-1"})
	dc.InventoryPath = "/DC0"
	now := time.Now()

	tests := []struct {
		name   string
		client *govmomi.Client
		at     time.Time
		want   bool
	}{
		{"same client, fresh", clientA, now.Add(defaultDatacenterTTL - time.Second), true},
		{"same client, expired", clientA, now.Add(defaultDatacenterTTL), false},
		{"reconnected client misses", clientB, now.Add(time.Second), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c datacenterCache
			_, ok := c.get(clientA, now)
			require.False(t, ok, "an empty cache misses")
			c.put(clientA, dc, now)

			got, ok := c.get(tc.client, tc.at)
			assert.Equal(t, tc.want, ok)
			if tc.want {
				assert.Equal(t, dc.Reference(), got.Reference())
				assert.Equal(t, "/DC0", got.InventoryPath)
				assert.NotSame(t, dc, got, "callers get a copy")
			}
		})
	}
}

// TestDCFinder_ConcurrentRPCs runs the RPC paths that used to share and
// re-scope one *find.Finder (Describe, ListVMs, ImportDisk target resolution,
// the ImagePrepare template lookup) from several goroutines at once. It is
// meaningful under -race: with the old shared finder, SetDatacenter in one RPC
// raced with the finder reads of the others.
func TestDCFinder_ConcurrentRPCs(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	id := seededVMID(t, p, simUnstamped)
	prepare := &providerv1.ImagePrepareRequest{
		ImageJson:  fmt.Sprintf(`{"source":{"vsphere":{"templateName":%q}}}`, simTemplate),
		TargetName: "concurrent-prepare",
	}

	const workers, rounds = 6, 3
	errs := make(chan error, workers*rounds*4)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				if _, err := p.Describe(ctx, &providerv1.DescribeRequest{Id: id}); err != nil {
					errs <- fmt.Errorf("Describe: %w", err)
				}
				if _, err := p.ListVMs(ctx, &providerv1.ListVMsRequest{}); err != nil {
					errs <- fmt.Errorf("ListVMs: %w", err)
				}
				if _, err := p.resolveImportTarget(ctx, ""); err != nil {
					errs <- fmt.Errorf("resolveImportTarget: %w", err)
				}
				if _, err := p.ImagePrepare(ctx, prepare); err != nil {
					errs <- fmt.Errorf("ImagePrepare: %w", err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// countingRT counts the round trips a client makes, so a test can tell a cached
// datacenter (none) from a lookup (at least one).
type countingRT struct {
	soap.RoundTripper
	n atomic.Int64
}

// RoundTrip counts the call and forwards it.
func (c *countingRT) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	c.n.Add(1)
	return c.RoundTripper.RoundTrip(ctx, req, res)
}

// countTrips installs a counter on p's client and returns it.
func countTrips(p *Provider) *countingRT {
	rt := &countingRT{RoundTripper: p.client.RoundTripper}
	p.client.RoundTripper = rt
	return rt
}

// fakeClock is a settable clock for the datacenter cache.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

// TestDCFinder_CacheExpiresThroughDCFinder: dcFinder reuses the datacenter
// without a vCenter round trip until defaultDatacenterTTL has passed, then
// looks it up again.
func TestDCFinder_CacheExpiresThroughDCFinder(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	clk := &fakeClock{t: time.Now()}
	p.dcCache.now = clk.now
	p.dcCache.invalidate() // newOwnershipSim's setup already looked the datacenter up
	trips := countTrips(p)

	_, _, err := p.dcFinder(ctx)
	require.NoError(t, err)
	assert.Positive(t, trips.n.Load(), "the first call looks the datacenter up")

	for _, step := range []time.Duration{0, defaultDatacenterTTL - time.Second} {
		clk.t = clk.t.Add(step)
		before := trips.n.Load()
		_, _, err = p.dcFinder(ctx)
		require.NoError(t, err)
		assert.Equal(t, before, trips.n.Load(), "a young entry is reused without a round trip")
	}

	clk.t = clk.t.Add(time.Second)
	before := trips.n.Load()
	_, _, err = p.dcFinder(ctx)
	require.NoError(t, err)
	assert.Greater(t, trips.n.Load(), before, "an entry at the TTL is looked up again")

	before = trips.n.Load()
	_, _, err = p.dcFinder(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, trips.n.Load(), "and the fresh entry is reused")
}

// TestDCFinder_ClientSwapMissesThroughDCFinder: after Validate reconnects (a new
// client), dcFinder does not hand out the datacenter found through the old one.
func TestDCFinder_ClientSwapMissesThroughDCFinder(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	_, oldDC, err := p.dcFinder(ctx)
	require.NoError(t, err)

	newClient, err := createVSphereClient(p.config)
	require.NoError(t, err)
	t.Cleanup(func() { _ = newClient.Logout(context.Background()) })
	p.client = newClient
	trips := countTrips(p)

	_, dc, err := p.dcFinder(ctx)
	require.NoError(t, err)
	assert.Positive(t, trips.n.Load(), "a new client misses the cache")
	assert.Equal(t, oldDC.Reference(), dc.Reference())

	before := trips.n.Load()
	_, _, err = p.dcFinder(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, trips.n.Load(), "then the new client's entry is reused")
}

// TestNoteInventoryError: a "not found" from a finder or inventory call drops
// the cached datacenter (a renamed or re-created datacenter makes every relative
// lookup fail that way); any other error, or none, leaves it.
func TestNoteInventoryError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		invalidate bool
	}{
		{"no error", nil, false},
		{"finder not found", fmt.Errorf("find folder: %w", new(find.NotFoundError)), true},
		{"ManagedObjectNotFound fault", fmt.Errorf("read: %w", soap.WrapVimFault(&types.ManagedObjectNotFound{})), true},
		{"other fault", soap.WrapVimFault(&types.NotAuthenticated{}), false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newOwnershipSim(t)
			_, _, err := p.dcFinder(context.Background())
			require.NoError(t, err)
			_, cached := p.dcCache.get(p.client, time.Now())
			require.True(t, cached)

			p.noteInventoryError(tc.err)

			_, cached = p.dcCache.get(p.client, time.Now())
			assert.Equal(t, !tc.invalidate, cached)
		})
	}
}

// TestDCFinder_NotFoundThroughFinderForcesLookup: a lookup that really fails
// with NotFound through a dcFinder finder (here a missing folder) drops the
// entry, so the next call looks the datacenter up afresh.
func TestDCFinder_NotFoundThroughFinderForcesLookup(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	finder, _, err := p.dcFinder(ctx)
	require.NoError(t, err)
	trips := countTrips(p)

	_, err = finder.Folder(ctx, "no-such-folder")
	require.Error(t, err)
	p.noteInventoryError(err)

	before := trips.n.Load()
	_, _, err = p.dcFinder(ctx)
	require.NoError(t, err)
	assert.Greater(t, trips.n.Load(), before, "the datacenter is looked up again after a not-found")
}
