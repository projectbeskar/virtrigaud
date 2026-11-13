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

// Package runner runs the per-host agent's accepted operations: one goroutine
// per operation, which drives the operation store's step journal
// (opstore.Store.Execute) to the operation's end. The store serialises the
// runs of one operation, and admits at most one operation per VM at a time,
// so the runs of one VM are serialised too.
//
// A run ends when the operation reaches a final state, when the pool's
// context ends (the agent drains; the operation is left to the next start's
// recovery, which never guesses), or when the store is quarantined (nothing
// more is committed; every answer carries the quarantine). A run that fails
// on any other store error is resumed with a bounded backoff for at least
// Options.GiveUpAfter (by default the store's step deadline, minutes) and at
// least Options.Resumes times, so that a short store fault does not end the
// operation; only then is its outcome recorded as unknown
// (opstore.ReasonRunFailed), never as a definite outcome, so that no
// operation stays running without a runner. An unknown outcome keeps the
// operation's VM busy: clearing it is an operator path that a later change
// adds.
//
// A panic in a run (for example in an executor) is recovered, logged,
// counted durably for its operation (opstore.Store.RecordPanic) and reported
// to Options.OnPanic; the operation is left to recovery. An operation whose
// runs panicked Options.MaxPanics times is not run again: its outcome is
// recorded as unknown (opstore.ReasonRunPanicked), so that an operation that
// always panics cannot keep the agent restarting. The agent
// treats it as fatal: it drains and exits, and its next start's recovery
// resolves the interrupted step by its evidence. Quarantining instead would
// end the host's service until it is enrolled again for what may be a
// transient fault, and carrying on would leave a run whose state nothing
// knows. It imports no Kubernetes package.
package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// Defaults of Options.
const (
	// DefaultResumes is the least number of times a failed run is resumed.
	DefaultResumes = 5
	// DefaultGiveUpAfter is the least time a failed run is resumed for: the
	// store's default step deadline.
	DefaultGiveUpAfter = opstore.DefaultStepDeadline
	// DefaultInitialBackoff is the pause before the first resumption.
	DefaultInitialBackoff = 200 * time.Millisecond
	// DefaultMaxPanics is the number of panicked runs after which an
	// operation is given up.
	DefaultMaxPanics = 3
	// DefaultMaxBackoff bounds the pause between resumptions.
	DefaultMaxBackoff = 10 * time.Second
)

// Store is what the pool needs from the operation store.
type Store interface {
	// Execute runs an admitted operation to its end (opstore.Store.Execute).
	Execute(ctx context.Context, ref opstore.OpRef, payload []byte) (opstore.OpView, error)
	// ResolveUnknown records a runnable operation's outcome as unknown
	// (opstore.Store.ResolveUnknown).
	ResolveUnknown(ctx context.Context, ref opstore.OpRef, reason string) (opstore.OpView, error)
	// Lost reports whether the store is quarantined (opstore.Store.Lost).
	Lost() bool
	// RecordPanic durably counts a run of ref that panicked
	// (opstore.Store.RecordPanic).
	RecordPanic(ctx context.Context, ref opstore.OpRef) (int, error)
	// Panics returns how many runs of ref panicked (opstore.Store.Panics).
	Panics(ctx context.Context, ref opstore.OpRef) (int, error)
}

// Options configures New. Zero fields take their defaults.
type Options struct {
	// Resumes is the least number of times a run that failed is resumed
	// before the operation's outcome is recorded as unknown.
	Resumes int
	// GiveUpAfter is the least time, from the first failure, a run that
	// failed is resumed for before the operation's outcome is recorded as
	// unknown. Set it to at least the longest step deadline.
	GiveUpAfter time.Duration
	// InitialBackoff is the pause before the first resumption; it doubles
	// up to MaxBackoff.
	InitialBackoff time.Duration
	// MaxBackoff bounds the pause between resumptions.
	MaxBackoff time.Duration
	// OnPanic is called, once per panic, after a run panicked and the panic
	// was counted; nil ignores it.
	OnPanic func(recovered any)
	// MaxPanics is the number of panicked runs after which an operation is
	// given up without running it again: its outcome is recorded unknown
	// (opstore.ReasonRunPanicked).
	MaxPanics int
}

// ErrDraining is returned by Start once the pool drains.
var ErrDraining = errors.New("the runner pool is draining")

// Pool runs operations. It is safe for concurrent use.
type Pool struct {
	ctx   context.Context
	store Store
	log   logr.Logger
	opts  Options

	mu       sync.Mutex
	draining bool
	wg       sync.WaitGroup
}

// New returns a pool whose runs stop when ctx ends.
func New(ctx context.Context, store Store, log logr.Logger, opts Options) *Pool {
	if opts.Resumes <= 0 {
		opts.Resumes = DefaultResumes
	}
	if opts.InitialBackoff <= 0 {
		opts.InitialBackoff = DefaultInitialBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultMaxBackoff
	}
	if opts.MaxPanics <= 0 {
		opts.MaxPanics = DefaultMaxPanics
	}
	if opts.GiveUpAfter <= 0 {
		opts.GiveUpAfter = DefaultGiveUpAfter
	}
	return &Pool{ctx: ctx, store: store, log: log, opts: opts}
}

// Start runs the operation ref in a new goroutine. payload is the verified
// request, held in memory for the run only (nil when the operation is resumed
// after a restart). Starting an operation that already has a run is
// harmless: the store refuses the second run. It returns ErrDraining once the
// pool drains or its context ended.
func (p *Pool) Start(ref opstore.OpRef, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.draining || p.ctx.Err() != nil {
		return ErrDraining
	}
	p.wg.Add(1)
	go p.run(ref, payload)
	return nil
}

// run drives one operation, resuming it on store errors.
func (p *Pool) run(ref opstore.OpRef, payload []byte) {
	defer p.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			p.log.Error(fmt.Errorf("panic: %v", r), "an operation run panicked; the operation is left to recovery", "op", ref.String())
			// Counted durably before the agent stops: a run that always
			// panics is given up after MaxPanics restarts.
			if n, err := p.store.RecordPanic(context.WithoutCancel(p.ctx), ref); err != nil {
				p.log.Error(err, "the panic could not be counted", "op", ref.String())
			} else {
				p.log.Info("panicked runs of the operation", "op", ref.String(), "count", n, "limit", p.opts.MaxPanics)
			}
			if p.opts.OnPanic != nil {
				p.opts.OnPanic(r)
			}
		}
	}()
	if n, err := p.store.Panics(p.ctx, ref); err == nil && n >= p.opts.MaxPanics {
		v, err := p.store.ResolveUnknown(p.ctx, ref, opstore.ReasonRunPanicked)
		if err != nil {
			p.log.Error(err, "an operation whose runs panicked could not be given up", "op", ref.String(), "panics", n)
			return
		}
		p.log.Error(nil, "the operation's runs panicked repeatedly; it is not run again and its outcome is recorded as unknown",
			"op", ref.String(), "panics", n, "state", v.State.String())
		return
	}
	backoff := p.opts.InitialBackoff
	var firstFailure time.Time
	for failures := 0; ; failures++ {
		v, err := p.store.Execute(p.ctx, ref, payload)
		switch {
		case err == nil:
			p.log.V(1).Info("operation run ended", "op", ref.String(), "state", v.State.String(), "reason", v.Reason)
			return
		case errors.Is(err, opstore.ErrRunning):
			return
		case p.ctx.Err() != nil:
			p.log.Info("operation run stopped by the drain; the next start's recovery resumes it", "op", ref.String())
			return
		case p.store.Lost():
			p.log.Error(err, "operation run stopped: the store is quarantined", "op", ref.String())
			return
		}
		if firstFailure.IsZero() {
			firstFailure = time.Now()
		}
		if failures >= p.opts.Resumes && time.Since(firstFailure) >= p.opts.GiveUpAfter {
			p.giveUp(ref, err)
			return
		}
		p.log.Error(err, "operation run failed; resuming it", "op", ref.String(), "failures", failures+1, "in", backoff.String())
		t := time.NewTimer(backoff)
		select {
		case <-p.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, p.opts.MaxBackoff)
	}
}

// giveUp records the operation's outcome as unknown after its last failed
// resumption.
func (p *Pool) giveUp(ref opstore.OpRef, last error) {
	v, err := p.store.ResolveUnknown(p.ctx, ref, opstore.ReasonRunFailed)
	if err != nil {
		p.log.Error(errors.Join(last, err), "operation run failed and its outcome could not be recorded; the operation is left to recovery", "op", ref.String())
		return
	}
	p.log.Error(last, "operation run failed repeatedly; its outcome is recorded as unknown", "op", ref.String(), "state", v.State.String())
}

// Drain refuses further runs and waits for the running ones until ctx ends.
// Cancel the pool's context first, so that runs stop between steps. It
// reports whether every run ended; a run still going at ctx's end is
// abandoned (the next start's recovery resolves its operation).
func (p *Pool) Drain(ctx context.Context) bool {
	p.mu.Lock()
	p.draining = true
	p.mu.Unlock()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
