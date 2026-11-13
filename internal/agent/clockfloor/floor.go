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

package clockfloor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
)

// Defaults of Config.
const (
	// DefaultPersistInterval is how far the floor may advance beyond the
	// persisted one before Observe saves it again. It bounds the write rate
	// (one save per interval at most, apart from the first and the last) and
	// how much of a raise a crash can lose.
	DefaultPersistInterval = 10 * time.Second
	// DefaultMaxAhead is how far the floor may be ahead of the wall clock
	// before the Floor reports StateClockFloorAhead.
	DefaultMaxAhead = grantverify.DefaultMaxClockSkew
)

// ReasonClockFloorAhead is the state reason a host reports while the floor is
// ahead of its wall clock.
const ReasonClockFloorAhead = "ClockFloorAhead"

// StateKind is the health of a Floor.
type StateKind string

const (
	// StateOK means the floor is usable and Now returns a time.
	StateOK StateKind = "Ok"
	// StateClockFloorAhead means the floor is ahead of the wall clock by more
	// than Config.MaxAhead. Now returns the zero time, so every grant is
	// refused, until the wall clock catches up. Re-enrolment is the only other
	// recovery.
	StateClockFloorAhead StateKind = ReasonClockFloorAhead
	// StateFailed means the floor could not be persisted or a clock source
	// failed. It is sticky: Now returns the zero time until the process
	// restarts and Open succeeds again.
	StateFailed StateKind = "Failed"
)

// Config configures Open.
type Config struct {
	// Store persists the floor. Required.
	Store Store
	// Wall is the wall clock; nil means grantverify.WallClock.
	Wall grantverify.Clock
	// Boot reads CLOCK_BOOTTIME; nil means SystemBootClock.
	Boot BootClock
	// BootID reads the boot_id; nil means ProcBootID with the default path.
	BootID BootIDSource
	// TimeNS checks that the process is not in a time namespace with a clock
	// offset; nil means ProcTimeNS with the default path.
	TimeNS TimeNamespaceCheck
	// FirstUse permits Open to create the floor when none is stored. Only
	// the code that has just created an enrolment sets it; every later start
	// leaves it false, so a deleted floor is refused (ErrFloorMissing) and is
	// not mistaken for a new one.
	FirstUse bool
	// MaxAhead is how far the floor may be ahead of the wall clock before
	// StateClockFloorAhead; 0 means DefaultMaxAhead.
	MaxAhead time.Duration
	// PersistInterval is how far the floor advances before Observe saves it;
	// 0 means DefaultPersistInterval. A restart in another boot loses the
	// boot-time credit accumulated since the last successful save: this
	// interval plus the period at which the agent calls Observe, and all of
	// the time since the last save if the process is killed without Flush or
	// the host loses power. The agent must call Observe at a period no longer
	// than this interval, and Flush on graceful shutdown.
	PersistInterval time.Duration
}

// Status is a snapshot of a Floor for a host answer.
type Status struct {
	// State is the health of the floor.
	State StateKind
	// Floor is the current floor (zero when State is StateFailed).
	Floor time.Time
	// Wall is the wall clock at the snapshot.
	Wall time.Time
	// Ahead is how far Floor is ahead of Wall; 0 when it is not.
	Ahead time.Duration
	// Err is the failure that made State StateFailed.
	Err error
}

// Floor is an agent's clock floor. It implements grantverify.Clock through
// Now and is safe for concurrent use.
type Floor struct {
	cfg    Config
	bootID string

	// saveMu serialises saves, so that mu is not held across Store.Save: a
	// slow fsync does not block Now, and therefore verification.
	saveMu sync.Mutex
	mu     sync.Mutex
	// floor is the highest time observed, in memory.
	floor time.Time
	// anchor is the (floor, boot time) pair boot-time credit is computed
	// from: the last one saved, or the one Open established.
	anchor State
	// saved is the floor of the last successful Save, or the floor Open read
	// from the store before any Save (zero when the store held none).
	saved time.Time
	// persisting is set by StartPersisting and cleared by StopPersisting:
	// only while it is set does the Floor call Store.Save.
	persisting bool
	failed     error
}

// Open reads the persisted floor and raises it from the agent's own clocks,
// anchored in the current boot. It only reads: the Floor then lives in memory
// and writes nothing until StartPersisting, because the agent's store must
// not commit before its continuity is established. Until then, and
// whatever the store holds, the in-memory floor is never lower than the
// stored one (it starts from it).
//
// It returns an error, and the caller must not run, when the floor is
// unreadable (any Load error other than ErrNotFound), is missing and
// cfg.FirstUse is false (ErrFloorMissing), a clock source fails, or the boot
// clock regressed within the persisted boot (ErrBootTimeRegressed). Errors
// wrap the cause; use errors.Is.
func Open(ctx context.Context, cfg Config) (*Floor, error) {
	if cfg.Store == nil {
		return nil, errors.New("open clock floor: no store")
	}
	if cfg.Wall == nil {
		cfg.Wall = grantverify.WallClock{}
	}
	if cfg.Boot == nil {
		cfg.Boot = SystemBootClock{}
	}
	if cfg.BootID == nil {
		cfg.BootID = ProcBootID{}
	}
	if cfg.MaxAhead == 0 {
		cfg.MaxAhead = DefaultMaxAhead
	}
	if cfg.PersistInterval == 0 {
		cfg.PersistInterval = DefaultPersistInterval
	}
	if cfg.TimeNS == nil {
		cfg.TimeNS = ProcTimeNS{}
	}
	if err := cfg.TimeNS.CheckTimeNamespace(); err != nil {
		return nil, fmt.Errorf("open clock floor: %w", err)
	}
	bootID, err := cfg.BootID.BootID()
	if err != nil {
		return nil, fmt.Errorf("open clock floor: %w", err)
	}
	if bootID == "" || len(bootID) > maxBootIDLen {
		return nil, errors.New("open clock floor: boot_id is empty or too long")
	}
	boot, err := cfg.Boot.BootTime()
	if err != nil {
		return nil, fmt.Errorf("open clock floor: %w", err)
	}
	wall := cfg.Wall.Now().UTC()
	if wall.After(maxFloor) {
		// Never raise the floor to a reading beyond the bound; a stepped
		// clock is a source failure, and the persisted floor is untouched.
		return nil, fmt.Errorf("open clock floor: wall clock: %w", ErrClockOutOfRange)
	}

	persisted, err := cfg.Store.Load(ctx)
	var floor, saved time.Time
	switch {
	case errors.Is(err, ErrNotFound):
		if !cfg.FirstUse {
			return nil, fmt.Errorf("open clock floor: %w", ErrFloorMissing)
		}
	case err != nil:
		return nil, fmt.Errorf("open clock floor: load: %w", err)
	default:
		if verr := persisted.validate(); verr != nil {
			return nil, fmt.Errorf("open clock floor: %w: %w", ErrCorrupt, verr)
		}
		floor, saved = persisted.Floor, persisted.Floor
		if persisted.BootID == bootID {
			// Same boot (the agent restarted): credit the boot time that
			// passed since the floor was anchored. Another boot_id earns no
			// credit, so a boot-time reading is never replayed across boots.
			if boot < persisted.BootTime {
				return nil, fmt.Errorf("open clock floor: %w", ErrBootTimeRegressed)
			}
			if credited := persisted.Floor.Add(boot - persisted.BootTime); credited.After(floor) {
				if credited.After(maxFloor) {
					return nil, fmt.Errorf("open clock floor: boot-time credit: %w", ErrClockOutOfRange)
				}
				floor = credited
			}
		}
	}
	if wall.After(floor) {
		floor = wall
	}
	anchor := State{Floor: floor, BootID: bootID, BootTime: boot}
	return &Floor{cfg: cfg, bootID: bootID, floor: floor, anchor: anchor, saved: saved}, nil
}

// StartPersisting saves the floor, raised from the clocks as of now, and from
// then on lets Observe and Flush save it. The agent calls it as the first
// write after its store's continuity is established. It is the first Save of
// a Floor: the value saved is at least the floor Open read, so no earlier
// stored value is replaced by a lower one, and a Store that checks that
// (UpdateClockFloor's transaction) holds the same line. A Save error fails
// the Floor for good (StateFailed), like any later one. Calling it again
// saves again.
func (f *Floor) StartPersisting(ctx context.Context) error {
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	f.mu.Lock()
	f.persisting = true
	f.mu.Unlock()
	return f.observe(ctx, true)
}

// StopPersisting makes Observe and Flush raise the floor in memory only. The
// agent calls it when its store can no longer commit (it is quarantined), so
// that the floor keeps protecting grant verification without failing.
func (f *Floor) StopPersisting() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.persisting = false
}

// observeLocked raises the in-memory floor from the wall clock and from the
// boot-time credit, and returns the wall clock and boot time it read. It
// fails the Floor when the boot clock errors or regresses. f.mu is held.
func (f *Floor) observeLocked() (wall time.Time, boot time.Duration, ok bool) {
	wall = f.cfg.Wall.Now().UTC()
	boot, err := f.cfg.Boot.BootTime()
	if err != nil {
		f.failed = fmt.Errorf("read boot clock: %w", err)
		return wall, 0, false
	}
	if boot < f.anchor.BootTime {
		f.failed = fmt.Errorf("read boot clock: %w", ErrBootTimeRegressed)
		return wall, 0, false
	}
	if wall.After(maxFloor) {
		f.failed = fmt.Errorf("read wall clock: %w", ErrClockOutOfRange)
		return wall, 0, false
	}
	if credited := f.anchor.Floor.Add(boot - f.anchor.BootTime); credited.After(f.floor) {
		if credited.After(maxFloor) {
			f.failed = fmt.Errorf("boot-time credit: %w", ErrClockOutOfRange)
			return wall, 0, false
		}
		f.floor = credited
	}
	if wall.After(f.floor) {
		f.floor = wall
	}
	return wall, boot, true
}

// Now implements grantverify.Clock. It returns the greater of the wall clock
// and the floor, which is the floor after it has been raised from both. It
// returns the zero time, which the verifier refuses, when the Floor has failed
// or is ahead of the wall clock by more than MaxAhead. Now reads memory and
// two clocks; it does no I/O (Observe persists).
func (f *Floor) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed != nil {
		return time.Time{}
	}
	wall, _, ok := f.observeLocked()
	if !ok || f.floor.Sub(wall) > f.cfg.MaxAhead {
		return time.Time{}
	}
	return f.floor
}

// Observe raises the floor from the agent's clocks and saves it when it has
// advanced PersistInterval beyond the saved one. An agent calls it from a
// periodic task. A Save error fails the Floor for good (StateFailed): the
// floor is never retried and continued from.
func (f *Floor) Observe(ctx context.Context) error {
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	return f.observe(ctx, false)
}

// Flush is Observe that always saves (while the Floor is persisting), for use
// at shutdown so a restart loses no credit. Before StartPersisting it saves
// nothing.
func (f *Floor) Flush(ctx context.Context) error {
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	return f.observe(ctx, true)
}

// observe raises the floor and, while persisting, saves it when force is set
// or it advanced PersistInterval beyond the saved one. f.saveMu is held.
func (f *Floor) observe(ctx context.Context, force bool) error {
	f.mu.Lock()
	if f.failed != nil {
		err := f.failed
		f.mu.Unlock()
		return err
	}
	_, boot, ok := f.observeLocked()
	if !ok {
		err := f.failed
		f.mu.Unlock()
		return err
	}
	if !f.persisting && !guard.Off(guard.ClockFloorReadOnlyOpen) {
		f.mu.Unlock()
		return nil
	}
	if !force && f.floor.Sub(f.saved) < f.cfg.PersistInterval {
		f.mu.Unlock()
		return nil
	}
	next := State{Floor: f.floor, BootID: f.bootID, BootTime: boot}
	f.mu.Unlock()

	err := f.cfg.Store.Save(ctx, next) // without f.mu: Now keeps answering
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.failed = fmt.Errorf("save clock floor: %w", err)
		return f.failed
	}
	f.anchor, f.saved = next, next.Floor
	return nil
}

// Status returns the floor's health after raising it from the clocks. A host
// reports StateClockFloorAhead as its ClockFloorAhead state reason.
func (f *Floor) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed != nil {
		return Status{State: StateFailed, Err: f.failed}
	}
	wall, _, ok := f.observeLocked()
	if !ok {
		return Status{State: StateFailed, Err: f.failed}
	}
	st := Status{State: StateOK, Floor: f.floor, Wall: wall}
	if ahead := f.floor.Sub(wall); ahead > f.cfg.MaxAhead {
		st.State, st.Ahead = StateClockFloorAhead, ahead
	}
	return st
}
