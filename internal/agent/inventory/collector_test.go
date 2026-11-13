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

package inventory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func TestPendingBeforeTheFirstCollection(t *testing.T) {
	c, _ := newCollector(t, healthyTree(t), nil)
	inv := c.Inventory(1)
	require.NoError(t, Validate(inv, 1))
	assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_PENDING, inv.GetState())
	assert.Zero(t, inv.GetSnapshotAgeSeconds())
	assert.Empty(t, inv.GetComponents())
	assert.Equal(t, providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_UNSPECIFIED, inv.GetProcessIdentityTier())
}

func TestEchoAndMinRules(t *testing.T) {
	c, _ := newCollector(t, healthyTree(t), nil)
	c.CollectNow(context.Background())
	assert.Nil(t, c.Inventory(0), "requested 0 means no inventory")
	for _, requested := range []uint32{1, 2, 99, 1 << 31} {
		inv := c.Inventory(requested)
		require.NoError(t, Validate(inv, requested), requested)
		assert.Equal(t, requested, inv.GetRequestedVersion(), "echoed byte for byte")
		assert.EqualValues(t, SupportedVersion, inv.GetSupportedVersion())
		assert.EqualValues(t, 1, inv.GetInventoryVersion(), "min(requested, supported)")
		assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_COMPLETE, inv.GetState())
	}
}

func TestSnapshotAgeUsesTheBootClock(t *testing.T) {
	c, boot := newCollector(t, healthyTree(t), nil)
	c.CollectNow(context.Background())
	assert.EqualValues(t, 0, c.Inventory(1).GetSnapshotAgeSeconds())
	boot.advance(90*time.Second + 900*time.Millisecond)
	assert.EqualValues(t, 90, c.Inventory(1).GetSnapshotAgeSeconds(), "whole seconds, rounded down")
	boot.advance(-time.Hour) // a clock that went backwards cannot give a negative age
	assert.EqualValues(t, 0, c.Inventory(1).GetSnapshotAgeSeconds())
	boot.advance(time.Hour * 24 * 365 * 200)
	assert.EqualValues(t, 1<<32-1, c.Inventory(1).GetSnapshotAgeSeconds(), "clamped")

	boot.mu.Lock()
	boot.err = errors.New("no clock")
	boot.mu.Unlock()
	inv := c.Inventory(1)
	require.NoError(t, Validate(inv, 1))
	assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, inv.GetState(), "an undated report is not reported as fresh")
}

func TestRequestPathDoesNoIO(t *testing.T) {
	tr := healthyTree(t)
	var cfs *countingFS
	c, _ := newCollector(t, tr, func(o *Options) {
		cfs = &countingFS{FS: o.FS}
		o.FS = cfs
	})
	c.CollectNow(context.Background())
	before, _ := cfs.snapshot()
	require.Positive(t, before)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				require.NoError(t, Validate(c.Inventory(1), 1))
			}
		}()
	}
	wg.Wait()
	after, _ := cfs.snapshot()
	assert.Equal(t, before, after, "answering reads the snapshot pointer only: no manifest I/O, hashing or probing")
}

func TestSnapshotIsImmutable(t *testing.T) {
	c, _ := newCollector(t, healthyTree(t), nil)
	c.CollectNow(context.Background())
	a := c.Inventory(1)
	a.Components[0].Name = "tampered"
	a.Features = nil
	b := c.Inventory(1)
	assert.NotEqual(t, "tampered", b.GetComponents()[0].GetName(), "a caller cannot change the snapshot")
	assert.NotEmpty(t, b.GetFeatures())
}

// blockingFS blocks every directory read until released.
type blockingFS struct {
	FS
	release chan struct{}
	started atomic.Int32
}

func (b *blockingFS) ReadDir(p string, limit int) ([]DirEntry, bool, error) {
	b.started.Add(1)
	<-b.release
	return b.FS.ReadDir(p, limit)
}

func TestCollectionBudget(t *testing.T) {
	tr := healthyTree(t)
	var bfs *blockingFS
	c, _ := newCollector(t, tr, func(o *Options) {
		bfs = &blockingFS{FS: o.FS, release: make(chan struct{})}
		o.FS = bfs
		o.Budget = 50 * time.Millisecond
	})
	start := time.Now()
	c.CollectNow(context.Background())
	assert.Less(t, time.Since(start), 5*time.Second, "the budget ends the wait")
	inv := c.Inventory(1)
	require.NoError(t, Validate(inv, 1))
	assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, inv.GetState())
	assert.Equal(t, ReasonProbeFailed, inv.GetStateReason())
	assert.Empty(t, inv.GetComponents())

	// While the abandoned collection still holds the file system, no other one starts.
	c.CollectNow(context.Background())
	assert.EqualValues(t, 1, bfs.started.Load())
	assert.Equal(t, ReasonProbeFailed, c.Inventory(1).GetStateReason())

	// Once it has returned, collection works again.
	close(bfs.release)
	require.Eventually(t, func() bool {
		select {
		case <-c.stuck:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)
	c.CollectNow(context.Background())
	assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_COMPLETE, c.Inventory(1).GetState())
}

func TestBootClockFailureAtCollection(t *testing.T) {
	c, boot := newCollector(t, healthyTree(t), nil)
	boot.err = errors.New("no clock")
	c.CollectNow(context.Background())
	inv := c.Inventory(1)
	assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, inv.GetState())
}

// dirReads counts reads of the read-only manifest directory: one per
// collection.
type dirReads struct {
	FS
	n atomic.Int32
}

func (d *dirReads) ReadDir(p string, limit int) ([]DirEntry, bool, error) {
	if p == ReadOnlyManifestDir {
		d.n.Add(1)
	}
	return d.FS.ReadDir(p, limit)
}

func TestRunDebounceAndMinGap(t *testing.T) {
	tr := healthyTree(t)
	reads := &dirReads{}
	notify := make(chan struct{}, 64)
	c, _ := newCollector(t, tr, func(o *Options) {
		reads.FS = o.FS
		o.FS = reads
		o.Notify = notify
		o.Interval = time.Hour
		o.Debounce = 80 * time.Millisecond
		o.MinGap = 400 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	defer func() { cancel(); <-done }()

	require.Eventually(t, func() bool { return c.Inventory(1).GetState() == providerv1.InventoryState_INVENTORY_STATE_COMPLETE },
		5*time.Second, 5*time.Millisecond, "a collection at start")
	assert.EqualValues(t, 1, reads.n.Load())

	// A burst of changes makes one collection, after the quiet period and not
	// before the minimum gap since the last one began.
	began := time.Now()
	for range 20 {
		notify <- struct{}{}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // past the debounce, inside the minimum gap
	assert.EqualValues(t, 1, reads.n.Load(), "the minimum gap is honoured")
	require.Eventually(t, func() bool { return reads.n.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
	assert.GreaterOrEqual(t, time.Since(began), 350*time.Millisecond)
	time.Sleep(600 * time.Millisecond)
	assert.EqualValues(t, 2, reads.n.Load(), "a burst makes exactly one collection")

	// A later change, past the minimum gap, is collected after the quiet period.
	notify <- struct{}{}
	require.Eventually(t, func() bool { return reads.n.Load() == 3 }, 5*time.Second, 5*time.Millisecond)
}

func TestRunIntervalAndStop(t *testing.T) {
	tr := healthyTree(t)
	reads := &dirReads{}
	c, _ := newCollector(t, tr, func(o *Options) {
		reads.FS = o.FS
		o.FS = reads
		o.Interval = 30 * time.Millisecond
		o.Notify = make(chan struct{})
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	require.Eventually(t, func() bool { return reads.n.Load() >= 3 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}

func TestRunWatchesDirectories(t *testing.T) {
	tr := healthyTree(t)
	reads := &dirReads{}
	c, _ := newCollector(t, tr, func(o *Options) {
		reads.FS = o.FS
		o.FS = reads
		o.Interval = time.Hour
		o.Debounce = 30 * time.Millisecond
		o.MinGap = 30 * time.Millisecond
		o.WatchRoot = tr.root
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	defer func() { cancel(); <-done }()
	require.Eventually(t, func() bool { return reads.n.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	tr.manifest(ReadOnlyManifestDir, "extra.json", "{")
	require.Eventually(t, func() bool { return reads.n.Load() >= 2 }, 5*time.Second, 10*time.Millisecond, "a change in a manifest directory triggers a collection")
	require.Eventually(t, func() bool {
		for _, comp := range c.Inventory(1).GetComponents() {
			if comp.GetName() == hashedName("extra.json") {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
}

func TestNewRefuses(t *testing.T) {
	boot := &fakeBoot{}
	_, err := New(Options{Host: healthyHost(), Boot: boot})
	assert.Error(t, err)
	_, err = New(Options{FS: OSFS{}, Host: healthyHost(), Boot: boot, Interval: time.Second})
	assert.Error(t, err, "the interval is bounded")
	_, err = New(Options{FS: OSFS{}, Host: healthyHost(), Boot: boot, Interval: 601 * time.Second})
	assert.Error(t, err)
	_, err = New(Options{FS: OSFS{}, Host: healthyHost(), Boot: boot, Interval: MinInterval})
	assert.NoError(t, err)
	_, err = New(Options{FS: OSFS{}, Host: healthyHost(), Boot: boot, Catalog: []CatalogEntry{{Name: "manifest-x", Range: ">=1"}}})
	assert.Error(t, err, "a catalog name cannot use the reserved prefix")
	_, err = New(Options{FS: OSFS{}, Host: healthyHost(), Boot: boot, Catalog: []CatalogEntry{{Name: "x", Range: "nonsense"}}})
	assert.Error(t, err)
	assert.NoError(t, validateCatalog(DefaultCatalog()))
}
