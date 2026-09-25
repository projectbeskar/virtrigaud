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

// Package assume is the manager's in-process record of placements the
// scheduler has chosen but whose durable record is not yet visible in the
// informer cache — kube-scheduler's "assume" step, for ADR-0007's clustered
// provider (Addendum A, scheduler-accuracy amendment).
//
// # The window it closes
//
// The VirtualMachine controller runs several reconciles at once. Each one
// schedules from the informer cache, then records the chosen host in
// status.placement.pendingHost with a checked status update (A2). Until that
// write is visible in the cache, a concurrent reconcile that reads the same
// cache does not see it: it could book the same capacity again, or put two VMs
// with mutual hard anti-affinity on one host. So the controller
//
//  1. takes the Provider's lock (Lock),
//  2. reads the committed placements from the cache plus the live assumptions
//     (List), schedules, and records its pick (Assume),
//  3. releases the lock, and then writes pendingHost.
//
// Every later schedule for that Provider sees the pick, either as an assumption
// or, once the cache has caught up, through the durable record.
//
// # When an assumption ends
//
//   - The cache shows the VM's durable record (placement.pendingHost or .host),
//     or no longer has the VM: List's settled callback reports it and List drops
//     it. From then on the record, not the assumption, counts.
//   - The pendingHost write failed, or the VM released the host (a name
//     conflict excluded it, the VM is being deleted): the controller calls
//     Forget.
//   - TTL passed: a safety net for a path that neither wrote nor forgot (a
//     reconcile that panicked in between). It never fires on the normal paths.
//
// # In-process is correct
//
// The manager runs under leader election, and only the leader runs
// reconcilers, so exactly one process schedules at a time and an in-process
// cache is authoritative. A new leader starts empty: it waits for its informer
// cache to sync before reconciling, and every placement the old leader made
// durable is then in that cache.
package assume

import (
	"context"
	"sync"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/scheduler"
)

// Assumption is one placement the scheduler has chosen for a VM whose durable
// record (status.placement.pendingHost) may not be visible in the informer
// cache yet.
type Assumption struct {
	// UID is the VM's Kubernetes UID; one VM has at most one assumption.
	UID string
	// Namespace and Name identify the VM (Namespace scopes affinity).
	Namespace string
	// Name is the VM's name.
	Name string
	// HostID is the chosen host (a Host CR name).
	HostID string
	// Labels are the VM's labels, for (anti-)affinity matching.
	Labels map[string]string
	// Resources is what the VM will hold on HostID.
	Resources scheduler.ResourceRequest
}

// entry is an Assumption with its Provider and expiry.
type entry struct {
	Assumption
	provider string
	expires  time.Time
}

// Cache holds the live assumptions of one manager, and one lock per Provider
// that serialises schedule-and-assume for that Provider. It is safe for
// concurrent use. It starts no goroutine: expired entries are dropped when the
// cache is next used.
type Cache struct {
	ttl time.Duration
	now func() time.Time

	// mu guards entries and locks. It is never held while a Provider lock is
	// being acquired, so the two cannot deadlock.
	mu      sync.Mutex
	entries map[string]*entry
	// locks holds one semaphore (capacity 1) per Provider: sending takes the
	// lock, receiving releases it. A channel, not a sync.Mutex, so a waiter
	// can give up (LockWithin).
	locks map[string]chan struct{}
}

// New returns an empty Cache whose assumptions expire ttl after they are made.
// now is the clock (nil means time.Now); tests pass a fake one.
func New(ttl time.Duration, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{
		ttl:     ttl,
		now:     now,
		entries: map[string]*entry{},
		locks:   map[string]chan struct{}{},
	}
}

// semaphore returns provider's lock, creating it on first use.
func (c *Cache) semaphore(provider string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[provider]
	if !ok {
		l = make(chan struct{}, 1)
		c.locks[provider] = l
	}
	return l
}

// Lock acquires provider's schedule lock, waiting as long as it takes, and
// returns the function that releases it. Hold it from reading the committed
// placements (List) to recording the pick (Assume) — in-memory work and
// informer-cache reads only — and release it before any API write, so
// concurrent reconciles of one Provider see each other's picks while no
// reconcile ever waits on another one's API call. Reconciles of different
// Providers never wait on each other. Controllers use LockWithin.
func (c *Cache) Lock(provider string) (unlock func()) {
	l := c.semaphore(provider)
	l <- struct{}{}
	return func() { <-l }
}

// LockWithin is Lock with a bounded wait: it gives up, returning ok == false
// and no unlock function, when the lock is not free within wait or ctx ends
// first. A reconcile that gets false requeues instead of parking its worker,
// so a busy Provider can never capture the controller's workers.
func (c *Cache) LockWithin(ctx context.Context, provider string, wait time.Duration) (unlock func(), ok bool) {
	l := c.semaphore(provider)
	select {
	case l <- struct{}{}:
		return func() { <-l }, true
	default:
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case l <- struct{}{}:
		return func() { <-l }, true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// Assume records a on provider, replacing any earlier assumption of the same
// VM. Call it with provider's lock held.
func (c *Cache) Assume(provider string, a Assumption) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[a.UID] = &entry{Assumption: a, provider: provider, expires: c.now().Add(c.ttl)}
}

// Forget drops the VM's assumption, if any, whatever its Provider.
func (c *Cache) Forget(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, uid)
}

// List returns provider's live assumptions, after dropping every expired
// assumption (of any Provider) and every one of provider's for which settled
// returns true. settled is called with c's internal lock held and must not use
// the Cache. Call List with provider's lock held, and decide settled from the
// same informer snapshot the committed placements were read from, so each
// placement is counted by its record or by its assumption — never by neither.
func (c *Cache) List(provider string, settled func(Assumption) bool) []Assumption {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var out []Assumption
	for uid, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, uid)
			continue
		}
		if e.provider != provider {
			continue
		}
		if settled != nil && settled(e.Assumption) {
			delete(c.entries, uid)
			continue
		}
		out = append(out, e.Assumption)
	}
	return out
}

// Len returns the number of assumptions held, expired ones included until the
// next List.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
