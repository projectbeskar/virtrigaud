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
// Every run ends when the operation reaches a final state or when the pool's
// context ends (the agent drains); a run stopped by the drain leaves its
// operation to the next start's recovery, which never guesses. It imports no
// Kubernetes package.
package runner

import (
	"context"
	"errors"
	"sync"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// Store is what the pool needs from the operation store.
type Store interface {
	// Execute runs an admitted operation to its end (opstore.Store.Execute).
	Execute(ctx context.Context, ref opstore.OpRef, payload []byte) (opstore.OpView, error)
}

// Pool runs operations. It is safe for concurrent use.
type Pool struct {
	ctx   context.Context
	store Store
	log   logr.Logger
	wg    sync.WaitGroup
}

// New returns a pool whose runs end when ctx ends.
func New(ctx context.Context, store Store, log logr.Logger) *Pool {
	return &Pool{ctx: ctx, store: store, log: log}
}

// Start runs the operation ref in a new goroutine. payload is the verified
// request, held in memory for the run only (nil when the operation is resumed
// after a restart). Starting an operation that already has a run is
// harmless: the store refuses the second run.
func (p *Pool) Start(ref opstore.OpRef, payload []byte) {
	if p.ctx.Err() != nil {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		v, err := p.store.Execute(p.ctx, ref, payload)
		switch {
		case err == nil:
			p.log.V(1).Info("operation run ended", "op", ref.String(), "state", v.State.String(), "reason", v.Reason)
		case errors.Is(err, opstore.ErrRunning):
		case p.ctx.Err() != nil:
			p.log.Info("operation run stopped by the drain; the next start's recovery resumes it", "op", ref.String())
		default:
			p.log.Error(err, "operation run failed; the operation is left to the store's recovery", "op", ref.String())
		}
	}()
}

// Wait waits for every run to end. Call it after the pool's context ended.
func (p *Pool) Wait() { p.wg.Wait() }
