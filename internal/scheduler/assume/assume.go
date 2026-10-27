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
// An assumption ends only on evidence that its allocation is counted, or no
// longer owed:
//
//   - The cache shows the VM's durable record naming the assumed host
//     (placement.pendingHost or .host): Observe drops it, and from then on the
//     record counts. A VM missing from the cache settles it only once the cache
//     has shown that VM since the assumption was made (a VM created a moment
//     ago may not be in the cache yet).
//   - The checked write recording the pick was refused by the API server, and
//     no earlier write of the same pick may still land (WriteRejected), or the
//     controller resolved an ambiguous write from an authoritative read
//     (ForgetIf).
//   - The VM released the host (a name conflict excluded it, the VM is being
//     deleted): the controller calls Forget.
//   - TTL passed and no write was ever sent for the pick (a reconcile that
//     stopped between choosing and writing): nothing can land, so it is dropped.
//
// TTL alone never ends an assumption whose write was sent: the write may have
// landed while the cache has not shown it yet, so the assumption keeps counting
// and is reported as stale (SetStaleHook) instead.
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
	"maps"
	"slices"
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
	// Resize marks an admitted resize of a VM already on HostID (its record
	// names the host from the start); Resources is its new size. The
	// controller settles it once the VM's recorded size reaches Resources, not
	// when its record names the host.
	Resize bool
	// Write is what is known of the checked write recording the pick. It is
	// set by the Cache (BeginWrite, WriteStored, WriteAmbiguous); a value
	// passed to Assume is ignored.
	Write WriteState
	// Observed reports that the snapshot the pick was made from (or the
	// informer cache the caller read the VM from) has the VM. Until a snapshot
	// has shown the VM, its absence does not settle the assumption: a VM
	// created a moment ago may not have reached the cache yet.
	Observed bool
}

// WriteState is what is known of the checked status write that records an
// assumption's pick durably (status.placement.pendingHost).
type WriteState int

const (
	// WriteNotSent: no write was sent for the pick. Nothing durable can exist,
	// so the assumption may be replaced, and it is dropped when its TTL passes.
	WriteNotSent WriteState = iota
	// WriteInFlight: a write was sent and its outcome is not known: it is in
	// progress, or it failed ambiguously (a timeout, a 5xx, a broken
	// connection) and may have landed.
	WriteInFlight
	// WriteStored: the API server confirmed the write; the assumption counts
	// until the cache shows the record.
	WriteStored
)

// Observation is what one informer snapshot says about an assumption's VM
// (Observe).
type Observation struct {
	// Present reports that the snapshot has the VM among the Provider's VMs
	// holding a placement.
	Present bool
	// Settled reports that the snapshot's record of the VM supersedes the
	// assumption (it names the assumed host; for a resize, the VM left the host
	// or its recorded size reached the admitted one). Only read when Present.
	Settled bool
}

// entry is an Assumption with its Provider, TTL, expiry and write record.
type entry struct {
	Assumption
	provider string
	ttl      time.Duration
	expires  time.Time
	// token identifies this pick: a later pick of the same VM gets another,
	// so a caller resolving its own write never touches a newer assumption.
	token uint64
	// observed is set once a snapshot has shown the VM since the pick.
	observed bool
	// ambiguous holds the resourceVersion preconditions of the writes for
	// this pick that failed ambiguously and may still land. They are compared
	// for equality with an authoritative read only (Pending).
	ambiguous []string
	// staleReported is set once the stale hook has reported this entry.
	staleReported bool
}

// unresolved reports whether a write was sent for the pick and its outcome is
// not yet reflected in the informer cache.
func (e *entry) unresolved() bool { return e.Write != WriteNotSent }

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
	// nextToken is the token of the next pick.
	nextToken uint64
	// onStale is called, once per assumption, when its TTL has passed while
	// its write is unresolved (SetStaleHook).
	onStale func(provider string, a Assumption, age time.Duration)
	// established holds, per VM UID and admission kind, the durable
	// admissions this process admitted or revalidated itself (Establish).
	established map[string]map[AdmissionKind]Admission
}

// AdmissionKind is the kind of a durable capacity admission.
type AdmissionKind string

// The durable capacity admissions a VM can hold.
const (
	// AdmissionCreate is a scheduled create's pendingHost with its size.
	AdmissionCreate AdmissionKind = "create"
	// AdmissionClone is a clustered clone target's pendingHost with its size.
	AdmissionClone AdmissionKind = "clone"
	// AdmissionResize is a resize reservation.
	AdmissionResize AdmissionKind = "resize"
)

// Admission identifies one durable capacity admission exactly as it is
// recorded on its VM: what it is for, where, at what size, and the operation
// it belongs to. Two admissions are the same only if every field is.
type Admission struct {
	Kind     AdmissionKind
	Provider string
	Host     string
	// Resources is the recorded size (a pending size, or a reservation).
	Resources scheduler.ResourceRequest
	// MemoryCeilingMiB is the recorded memory ceiling, -1 when none is
	// recorded.
	MemoryCeilingMiB int64
	// Ref names the operation the admission belongs to beyond the VM (a
	// clone's UID), "" when none.
	Ref string
}

// SetStaleHook installs fn, called once for each assumption whose TTL passed
// while its write was unresolved (the assumption keeps counting). fn is called
// with no lock of the Cache held.
func (c *Cache) SetStaleHook(fn func(provider string, a Assumption, age time.Duration)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onStale = fn
}

// New returns an empty Cache whose assumptions expire ttl after they are made.
// now is the clock (nil means time.Now); tests pass a fake one.
func New(ttl time.Duration, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{
		ttl:         ttl,
		now:         now,
		entries:     map[string]*entry{},
		locks:       map[string]chan struct{}{},
		established: map[string]map[AdmissionKind]Admission{},
	}
}

// Establish records that this process admitted adm for the VM uid, or
// revalidated it against its own accounting: the operation adm authorizes
// may be executed without checking it again, for as long as the VM records
// exactly adm (Established). A durable admission this process did not
// establish — one a previous manager instance recorded, possibly with a write
// that landed after this one started leading — is checked first.
func (c *Cache) Establish(uid string, adm Admission) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.established[uid]
	if m == nil {
		m = map[AdmissionKind]Admission{}
		c.established[uid] = m
	}
	m[adm.Kind] = adm
}

// Established reports whether this process established exactly adm for the
// VM uid. Any difference (another host, size, ceiling, Provider or operation)
// is a different admission.
func (c *Cache) Established(uid string, adm Admission) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	got, ok := c.established[uid][adm.Kind]
	return ok && got == adm
}

// Disestablish drops every admission established for the VM uid.
func (c *Cache) Disestablish(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.established, uid)
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

// Assume records a on provider with the Cache's TTL and returns the pick's
// token. Call it with provider's lock held.
//
// An earlier assumption of the same VM is replaced only when no write was sent
// for it. One whose write is unresolved is kept: a pick of the same host and
// Provider continues it (same token and write record, at the larger size), and
// a pick of another host or Provider is refused (ok == false), because the
// earlier write may have recorded the first host durably.
func (c *Cache) Assume(provider string, a Assumption) (token uint64, ok bool) {
	return c.AssumeFor(provider, a, c.ttl)
}

// AssumeFor is Assume with a TTL of its own, for an assumption that must
// outlive a longer operation than the Cache's default covers (an admitted
// resize outlives its Reconfigure call). A TTL below the Cache's is raised to
// it. Touch restarts it with the same TTL.
func (c *Cache) AssumeFor(provider string, a Assumption, ttl time.Duration) (token uint64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl = max(ttl, c.ttl)
	a.Labels = maps.Clone(a.Labels) // the caller's map may be shared with the informer cache
	if e, found := c.entries[a.UID]; found && e.unresolved() {
		if e.provider != provider || e.HostID != a.HostID || e.Resize != a.Resize {
			return 0, false
		}
		e.Labels = a.Labels
		e.observed = e.observed || a.Observed
		e.Resources.CPU = max(e.Resources.CPU, a.Resources.CPU)
		e.Resources.MemoryMiB = max(e.Resources.MemoryMiB, a.Resources.MemoryMiB)
		e.ttl = ttl
		e.expires = c.now().Add(ttl)
		e.staleReported = false
		return e.token, true
	}
	c.nextToken++
	a.Write = WriteNotSent
	c.entries[a.UID] = &entry{Assumption: a, provider: provider, ttl: ttl, expires: c.now().Add(ttl),
		token: c.nextToken, observed: a.Observed}
	return c.nextToken, true
}

// TokenFor returns the token of the VM's assumption on host, ok == false when
// it has none there (it was settled, forgotten or replaced meanwhile). A
// caller about to write the pick revalidates it with this.
func (c *Cache) TokenFor(uid, host string) (token uint64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[uid]
	if !found || e.HostID != host {
		return 0, false
	}
	return e.token, true
}

// Lookup returns the VM's assumption and its token.
func (c *Cache) Lookup(uid string) (a Assumption, token uint64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[uid]
	if !found {
		return Assumption{}, 0, false
	}
	a = e.Assumption
	a.Labels = maps.Clone(e.Labels)
	return a, e.token, true
}

// Unresolved reports whether the VM has an assumption whose write was sent
// and is not yet reflected in the cache: the VM must not be placed again
// until that write is resolved.
func (c *Cache) Unresolved(uid string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[uid]
	return found && e.unresolved()
}

// BeginWrite marks the pick token as having its checked write sent. It
// reports false, changing nothing,
// when the VM's assumption is no longer that pick: the caller must not send
// the write then.
func (c *Cache) BeginWrite(uid string, token uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[uid]
	if !found || e.token != token {
		return false
	}
	if e.Write != WriteStored {
		e.Write = WriteInFlight
	}
	return true
}

// WriteStored records that a write for the pick token was stored by the API
// server: the assumption counts until the cache shows the record, whatever
// its TTL, and no earlier write of the pick can land any more (the object has
// moved past their preconditions).
func (c *Cache) WriteStored(uid string, token uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, found := c.entries[uid]; found && e.token == token {
		e.Write = WriteStored
		e.ambiguous = nil
	}
}

// WriteAmbiguous records that a write for the pick token, sent with the
// resourceVersion precondition, failed without telling whether it was
// stored. The assumption keeps counting until the write is resolved.
func (c *Cache) WriteAmbiguous(uid string, token uint64, precondition string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, found := c.entries[uid]; found && e.token == token && e.Write != WriteStored {
		e.Write = WriteInFlight
		if !slices.Contains(e.ambiguous, precondition) {
			e.ambiguous = append(e.ambiguous, precondition)
		}
	}
}

// WriteRejected records that a write for the pick token was refused by the
// API server outright, so that write certainly did not land. The assumption
// is dropped (released == true) unless an earlier write of the same pick
// failed ambiguously or was stored: that one may hold the host durably, and it
// keeps counting until it is resolved.
func (c *Cache) WriteRejected(uid string, token uint64) (released bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[uid]
	if !found || e.token != token {
		return false
	}
	if e.Write == WriteStored || len(e.ambiguous) > 0 {
		return false
	}
	delete(c.entries, uid)
	return true
}

// Pending reports whether a write of the pick token that failed ambiguously
// can still land, given resourceVersion: the VM's resourceVersion read from
// the API server itself (never from the informer cache, which may lag). A
// checked write lands only on the version it names, so one can land only while
// the stored version is still its precondition; resourceVersions are compared
// for equality only.
func (c *Cache) Pending(uid string, token uint64, resourceVersion string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[uid]
	return found && e.token == token && slices.Contains(e.ambiguous, resourceVersion)
}

// SettleVisible drops the VM's assumption when it names one of hosts: hosts
// are the hosts the VM's record names in the informer cache the caller read
// the VM from. The cache only moves forward, so every later snapshot counts
// that record (or a newer one the controller wrote itself). Call it with the
// VM's Provider's lock held.
func (c *Cache) SettleVisible(uid string, hosts []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, found := c.entries[uid]; found && slices.Contains(hosts, e.HostID) {
		delete(c.entries, uid)
	}
}

// Narrow lowers the resources of the pick token to r, per resource (it never
// raises them). The caller has established — from its provider's report,
// recorded durably — that the VM holds no more than r on its host; the
// assumption keeps covering r until a snapshot shows that record.
func (c *Cache) Narrow(uid string, token uint64, r scheduler.ResourceRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, found := c.entries[uid]; found && e.token == token {
		e.Resources.CPU = min(e.Resources.CPU, r.CPU)
		e.Resources.MemoryMiB = min(e.Resources.MemoryMiB, r.MemoryMiB)
	}
}

// ForgetIf drops the VM's assumption if it is still the pick token.
func (c *Cache) ForgetIf(uid string, token uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, found := c.entries[uid]; found && e.token == token {
		delete(c.entries, uid)
		return true
	}
	return false
}

// Touch restarts the TTL of the VM's assumption, if any: a caller whose
// operation is still in flight (an asynchronous resize task) keeps it alive
// without changing it.
func (c *Cache) Touch(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[uid]; ok {
		e.expires = c.now().Add(e.ttl)
	}
}

// Forget drops the VM's assumption, if any, whatever its Provider.
func (c *Cache) Forget(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, uid)
}

// List is Observe with a callback that only reports whether an assumption is
// settled (every VM taken as present). nil settles nothing.
func (c *Cache) List(provider string, settled func(Assumption) bool) []Assumption {
	var observe func(Assumption) Observation
	if settled != nil {
		observe = func(a Assumption) Observation {
			return Observation{Present: true, Settled: settled(a)}
		}
	}
	return c.Observe(provider, observe)
}

// Observe returns provider's live assumptions, after dropping
//
//   - every expired assumption (of any Provider) for which no write was sent,
//   - every one of provider's whose snapshot record settles it, and every one
//     whose VM the snapshot no longer has although an earlier snapshot showed
//     it after the pick.
//
// An expired assumption whose write was sent is kept, and reported once to the
// stale hook. observe is called with c's internal lock held and must not use
// the Cache. Call Observe with provider's lock held, and answer observe from
// the same informer snapshot the committed placements were read from, so each
// placement is counted by its record or by its assumption — never by neither.
func (c *Cache) Observe(provider string, observe func(Assumption) Observation) []Assumption {
	type staleReport struct {
		provider string
		a        Assumption
		age      time.Duration
	}
	var stale []staleReport
	c.mu.Lock()
	now := c.now()
	var out []Assumption
	for uid, e := range c.entries {
		if !now.Before(e.expires) {
			if !e.unresolved() {
				delete(c.entries, uid)
				continue
			}
			if !e.staleReported && c.onStale != nil {
				e.staleReported = true
				stale = append(stale, staleReport{provider: e.provider, a: e.Assumption, age: now.Sub(e.expires) + e.ttl})
			}
		}
		if e.provider != provider {
			continue
		}
		if observe != nil {
			o := observe(e.Assumption)
			if o.Present {
				e.observed = true
				if o.Settled {
					delete(c.entries, uid)
					continue
				}
			} else if e.observed {
				delete(c.entries, uid)
				continue
			}
		}
		out = append(out, e.Assumption)
	}
	hook := c.onStale
	c.mu.Unlock()
	for _, r := range stale {
		hook(r.provider, r.a, r.age)
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
