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
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Timing of the background collector.
const (
	// DefaultInterval is the period between collections.
	DefaultInterval = 60 * time.Second
	// MinInterval and MaxInterval bound the configurable period.
	MinInterval = 30 * time.Second
	// MaxInterval is the longest configurable period.
	MaxInterval = 600 * time.Second
	// DefaultDebounce is the quiet period after a change in a watched
	// directory before a collection starts.
	DefaultDebounce = 5 * time.Second
	// DefaultMinGap is the shortest time between two change-triggered
	// collections.
	DefaultMinGap = 30 * time.Second
	// DefaultBudget is the longest a collection may take; one that exceeds it
	// is abandoned and the snapshot becomes UNREADABLE/probe_failed.
	DefaultBudget = 120 * time.Second
)

// Options configures New.
type Options struct {
	// FS reads the host. Required.
	FS FS
	// Host answers the feature probes. Required.
	Host Host
	// Boot is the CLOCK_BOOTTIME source for the snapshot's age. Required.
	Boot clockfloor.BootClock
	// Layout names the directories to read; the zero value uses
	// DefaultLayout.
	Layout Layout
	// Catalog replaces DefaultCatalog (tests).
	Catalog []CatalogEntry
	// AgentVersion is the agent's build version, the version of the agent
	// component.
	AgentVersion string
	// Log receives one line when the snapshot's state changes; never file
	// contents or paths.
	Log logr.Logger
	// Interval, Debounce, MinGap and Budget replace the defaults; zero uses
	// the default. Interval must be within MinInterval and MaxInterval unless
	// AllowShortTimings is set.
	Interval, Debounce, MinGap, Budget time.Duration
	// AllowShortTimings lifts the lower limit of Interval (tests).
	AllowShortTimings bool
	// MaxInventoryBytes replaces MaxInventoryBytes (tests); zero uses it.
	MaxInventoryBytes int
	// Notify replaces the directory watch with a channel of change
	// notifications (tests).
	Notify <-chan struct{}
	// probes replaces the feature probes (tests).
	probes []probe
	// hashFile and hashTotal replace the hashing limits (tests).
	hashFile, hashTotal int64
}

// snapshot is one finished collection: immutable once published.
type snapshot struct {
	state  providerv1.InventoryState
	reason string
	core   *collected
	// at is CLOCK_BOOTTIME when the collection finished.
	at time.Duration
}

// Collector builds the host inventory in the background and answers requests
// from its latest snapshot.
type Collector struct {
	env       *env
	o         Options
	snap      atomic.Pointer[snapshot]
	lastKey   string
	stuck     <-chan struct{}
	lastBegan time.Time
}

// New returns a collector. It starts nothing; Run does.
func New(o Options) (*Collector, error) {
	if o.FS == nil || o.Host == nil || o.Boot == nil {
		return nil, errors.New("inventory collector: file system, host and boot clock are required")
	}
	if o.Layout.ReadOnlyDir == "" {
		l := DefaultLayout()
		l.AgentExecutable = o.Layout.AgentExecutable
		o.Layout = l
	}
	if o.Catalog == nil {
		o.Catalog = DefaultCatalog()
	}
	if err := validateCatalog(o.Catalog); err != nil {
		return nil, fmt.Errorf("inventory collector: %w", err)
	}
	if o.Interval == 0 {
		o.Interval = DefaultInterval
	}
	if !o.AllowShortTimings && (o.Interval < MinInterval || o.Interval > MaxInterval) {
		return nil, fmt.Errorf("inventory collector: interval %v is outside %v to %v", o.Interval, MinInterval, MaxInterval)
	}
	if o.Debounce == 0 {
		o.Debounce = DefaultDebounce
	}
	if o.MinGap == 0 {
		o.MinGap = DefaultMinGap
	}
	if o.Budget == 0 {
		o.Budget = DefaultBudget
	}
	if o.MaxInventoryBytes == 0 {
		o.MaxInventoryBytes = MaxInventoryBytes
	}
	e := &env{
		fs: o.FS, host: o.Host, layout: o.Layout, catalog: o.Catalog, byName: map[string]*CatalogEntry{},
		agentVersion: o.AgentVersion, maxBytes: o.MaxInventoryBytes, probes: o.probes,
		hashFile: o.hashFile, hashTotal: o.hashTotal,
	}
	if e.hashFile == 0 {
		e.hashFile = MaxHashFileBytes
	}
	if e.hashTotal == 0 {
		e.hashTotal = MaxHashTotalBytes
	}
	if e.probes == nil {
		e.probes = defaultProbes()
	}
	for i := range e.catalog {
		e.byName[e.catalog[i].Name] = &e.catalog[i]
	}
	return &Collector{env: e, o: o}, nil
}

// Inventory returns the inventory that answers a request for inventory
// version requested: nil when it is 0 (nothing was asked for), PENDING before
// the first collection has finished, otherwise the latest snapshot, with its
// age measured now on CLOCK_BOOTTIME. It reads the snapshot pointer and the
// boot clock only; it never touches the file system.
func (c *Collector) Inventory(requested uint32) *providerv1.HostInventory {
	if requested == 0 {
		return nil
	}
	inv := &providerv1.HostInventory{State: providerv1.InventoryState_INVENTORY_STATE_PENDING}
	bind(inv, requested)
	s := c.snap.Load()
	if s == nil {
		return inv
	}
	now, err := c.o.Boot.BootTime()
	if err != nil {
		// Without a clock the age is unknown: the report cannot be dated.
		inv.State, inv.StateReason = providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, ReasonProbeFailed
		return inv
	}
	age := max(now-s.at, 0) / time.Second
	inv.State, inv.StateReason = s.state, s.reason
	inv.SnapshotAgeSeconds = uint32(min(age, math.MaxUint32)) // #nosec G115 -- clamped
	if s.core != nil {
		for _, comp := range s.core.components {
			inv.Components = append(inv.Components, proto.Clone(comp).(*providerv1.HostComponent))
		}
		for _, f := range s.core.features {
			inv.Features = append(inv.Features, proto.Clone(f).(*providerv1.HostFeature))
		}
		for _, x := range s.core.extensions {
			inv.UnexpectedExtensions = append(inv.UnexpectedExtensions, proto.Clone(x).(*providerv1.HostExtension))
		}
		inv.ProcessIdentityTier = s.core.tier
	}
	return inv
}

// CollectNow makes one collection and publishes it, subject to the budget. It
// is what Run does on every trigger.
func (c *Collector) CollectNow(ctx context.Context) {
	if c.stuck != nil {
		select {
		case <-c.stuck:
			c.stuck = nil
		default:
			// An abandoned collection still holds the file system; start no
			// other one on top of it.
			c.publishFailure(ReasonProbeFailed)
			return
		}
	}
	cctx, cancel := context.WithTimeout(ctx, c.o.Budget)
	defer cancel()
	type result struct {
		core *collected
		err  error
	}
	done := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		core, err := c.env.collect(cctx)
		done <- result{core, err}
	}()
	select {
	case r := <-done:
		c.publish(ctx, r.core, r.err)
	case <-cctx.Done():
		cancel()
		if ctx.Err() != nil {
			return
		}
		c.stuck = finished
		c.o.Log.Error(nil, "the host inventory collection ran out of time and was abandoned")
		c.publishFailure(ReasonProbeFailed)
	}
}

// publish turns a finished collection into the current snapshot.
func (c *Collector) publish(ctx context.Context, core *collected, err error) {
	var ab *abort
	switch {
	case err == nil:
	case errors.As(err, &ab):
	case ctx.Err() != nil:
		return
	default:
		ab = unreadable(ReasonProbeFailed, err)
	}
	at, berr := c.o.Boot.BootTime()
	if berr != nil {
		c.publishFailure(ReasonProbeFailed)
		return
	}
	s := &snapshot{at: at}
	if ab != nil {
		s.state, s.reason = ab.state, ab.reason
		c.logChange(s, ab.cause)
	} else {
		s.state, s.core = providerv1.InventoryState_INVENTORY_STATE_COMPLETE, core
		c.logChange(s, nil)
	}
	c.snap.Store(s)
}

// publishFailure publishes an UNREADABLE snapshot with reason.
func (c *Collector) publishFailure(reason string) {
	at, err := c.o.Boot.BootTime()
	if err != nil {
		at = 0
	}
	s := &snapshot{state: providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, reason: reason, at: at}
	c.logChange(s, nil)
	c.snap.Store(s)
}

// logChange logs when the state of the published snapshot changes.
func (c *Collector) logChange(s *snapshot, cause error) {
	key := s.state.String() + "/" + s.reason
	if key == c.lastKey {
		return
	}
	c.lastKey = key
	if s.state == providerv1.InventoryState_INVENTORY_STATE_COMPLETE {
		c.o.Log.Info("host inventory collected", "components", len(s.core.components), "unexpectedExtensions", len(s.core.extensions))
		return
	}
	c.o.Log.Info("host inventory is not complete", "state", s.state.String(), "reason", s.reason, "cause", fmt.Sprint(cause))
}

// Run collects until ctx ends: at once, then every interval, and after a
// change in a manifest or extension directory once it has been quiet for the
// debounce period, but not sooner than the minimum gap after the last
// collection began. It returns when ctx is done.
func (c *Collector) Run(ctx context.Context) {
	notify, closeWatch := c.watch()
	defer closeWatch()
	c.begin(ctx)
	interval := time.NewTicker(c.o.Interval)
	defer interval.Stop()
	due := time.NewTimer(time.Hour)
	if !due.Stop() {
		<-due.C
	}
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-interval.C:
			if pending {
				pending = false
				stopTimer(due)
			}
			c.begin(ctx)
		case <-notify:
			pending = true
			wait := c.o.Debounce
			if gap := c.o.MinGap - time.Since(c.lastBegan); gap > wait {
				wait = gap
			}
			stopTimer(due)
			due.Reset(wait)
		case <-due.C:
			if pending {
				pending = false
				c.begin(ctx)
			}
		}
	}
}

// begin starts one collection and records when it began.
func (c *Collector) begin(ctx context.Context) {
	c.lastBegan = time.Now()
	c.CollectNow(ctx)
}

// stopTimer stops t and drains its channel.
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// watchedDirs are the directories whose change triggers a collection.
func (c *Collector) watchedDirs() []string {
	l := c.o.Layout
	dirs := []string{l.ReadOnlyDir, l.WritableDir}
	return append(dirs, l.ExtensionDirs...)
}

// watch returns the channel of change notifications and a function that
// stops watching. With Options.Notify it is that channel. Otherwise it is a
// file system watch on the directories that exist under the file system's
// root; a directory that does not exist is picked up by the periodic
// collection.
func (c *Collector) watch() (<-chan struct{}, func()) {
	if c.o.Notify != nil {
		return c.o.Notify, func() {}
	}
	osfs, ok := c.o.FS.(OSFS)
	if !ok {
		return nil, func() {}
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		c.o.Log.V(1).Info("no directory watch; the periodic collection still runs")
		return nil, func() {}
	}
	for _, d := range c.watchedDirs() {
		if p, rerr := osfs.resolve(d); rerr == nil {
			_ = w.Add(p) // a directory that does not exist yet is covered by the interval
		}
	}
	out := make(chan struct{}, 1)
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-stop:
				return
			case _, ok := <-w.Events:
				if !ok {
					return
				}
				select {
				case out <- struct{}{}:
				default:
				}
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return out, func() {
		close(stop)
		_ = w.Close()
		<-finished
	}
}
