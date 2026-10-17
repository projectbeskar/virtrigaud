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
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// defaultDatacenterTTL bounds how long the provider reuses the default
// datacenter it looked up. The datacenter is effectively immutable for the life
// of a provider process, but a renamed or re-created one (a new managed object
// ID) must not stay pinned: the TTL is short, and noteInventoryError drops the
// entry as soon as a lookup through a cached datacenter reports "not found".
const defaultDatacenterTTL = 60 * time.Second

// errVSphereClientNotInitialised is returned by dcFinder when the provider has
// no vCenter client (the connection failed at startup and Validate has not yet
// reconnected).
var errVSphereClientNotInitialised = errors.New("vSphere client not initialized")

// datacenterCache holds the default datacenter looked up for one vCenter client.
// The zero value is an empty cache.
type datacenterCache struct {
	mu     sync.Mutex
	client *govmomi.Client
	dc     *object.Datacenter
	at     time.Time
	// now is the clock; nil means time.Now (tests substitute their own).
	now func() time.Time
}

// clock returns the current time from c's clock.
func (c *datacenterCache) clock() time.Time {
	c.mu.Lock()
	now := c.now
	c.mu.Unlock()
	if now != nil {
		return now()
	}
	return time.Now()
}

// invalidate empties the cache.
func (c *datacenterCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client, c.dc, c.at = nil, nil, time.Time{}
}

// get returns a private copy of the cached datacenter when it was looked up
// through client and is younger than defaultDatacenterTTL as of now.
func (c *datacenterCache) get(client *govmomi.Client, now time.Time) (*object.Datacenter, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dc == nil || c.client != client || now.Sub(c.at) >= defaultDatacenterTTL {
		return nil, false
	}
	dc := *c.dc
	return &dc, true
}

// put records dc as the default datacenter of client.
func (c *datacenterCache) put(client *govmomi.Client, dc *object.Datacenter, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *dc
	c.client, c.dc, c.at = client, &cp, now
}

// dcFinder returns a finder of the caller's own, scoped to the default
// datacenter, and that datacenter.
//
// The provider used to keep one shared *find.Finder and re-scope it with
// SetDatacenter at the start of every RPC. A govmomi Finder is not safe for
// concurrent use (SetDatacenter rewrites it, and it lazily fills its own folder
// cache), so concurrent RPCs raced on it, and a path that used the finder before
// scoping it ran against whatever datacenter the previous RPC had left behind.
// Every RPC path now takes its own finder from here and passes it to the helpers
// it calls; nothing mutates a finder another goroutine can reach.
//
// Finding the default datacenter is a vCenter round trip. The datacenter object
// is an immutable reference (managed object ID, inventory path and the client it
// was found through), so it is cached for defaultDatacenterTTL and for the
// current vCenter client only: a reconnect (Validate) builds a new client, which
// makes the entry miss. A finder or inventory call made through a cached
// datacenter that fails with "not found" drops the entry (noteInventoryError),
// so a renamed or re-created datacenter costs at most the one failed call. Each
// caller gets its own copy of the object, so the cache cannot be altered through
// it. Building the finder itself does no I/O.
//
// A failure to find the datacenter (none, or more than one without a default)
// is returned as govmomi reports it, for the caller to wrap; it is never cached.
func (p *Provider) dcFinder(ctx context.Context) (*find.Finder, *object.Datacenter, error) {
	client := p.client
	if client == nil {
		return nil, nil, errVSphereClientNotInitialised
	}
	finder := find.NewFinder(client.Client, true)

	if dc, ok := p.dcCache.get(client, p.dcCache.clock()); ok {
		return finder.SetDatacenter(dc), dc, nil
	}
	dc, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, nil, err
	}
	p.dcCache.put(client, dc, p.dcCache.clock())
	return finder.SetDatacenter(dc), dc, nil
}

// noteInventoryError drops the cached datacenter when err, from a finder or
// inventory call made through a dcFinder finder, says an object was not found
// (the finder's NotFoundError, or vCenter's ManagedObjectNotFound fault): a
// datacenter that was renamed or re-created makes every relative lookup fail
// that way, and the next dcFinder then looks the default datacenter up afresh.
// A genuinely missing object (a mistyped folder) costs only that one extra
// lookup. It never changes err; a nil err is ignored.
func (p *Provider) noteInventoryError(err error) {
	if err == nil {
		return
	}
	var notFound *find.NotFoundError
	if errors.As(err, &notFound) || fault.Is(err, &types.ManagedObjectNotFound{}) {
		p.dcCache.invalidate()
	}
}
