/*
Copyright 2025.

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

package hostconn

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// registry is the default Registry implementation. It serves a fixed set of
// pre-built connections keyed by HostID.
//
// Today the libvirt provider constructs it with exactly one Conn (built from
// PROVIDER_ENDPOINT). ADR-0007 P1 changes only the call site that builds this —
// to pass N Conns projected from Host CRs, or a lazily-dialing variant — the
// registry type itself is unchanged. Access is mutex-guarded so a future
// hot-reload path (add/evict on Host CR change) is race-free.
type registry struct {
	mu    sync.RWMutex
	conns map[HostID]Conn
}

// NewRegistry returns a Registry serving the given connections, keyed by each
// Conn's HostID. It rejects a nil Conn or a duplicate HostID.
//
// This is the single constructor ADR-0007 P1 changes to move from one host to N:
// the libvirt provider passes exactly one Conn today; P1 passes the projected
// set. The Registry behaviour is identical for one host or many.
func NewRegistry(conns ...Conn) (Registry, error) {
	r := &registry{conns: make(map[HostID]Conn, len(conns))}
	for _, c := range conns {
		if c == nil {
			return nil, errors.New("hostconn: nil Conn passed to NewRegistry")
		}
		id := c.HostID()
		if _, dup := r.conns[id]; dup {
			return nil, fmt.Errorf("hostconn: duplicate HostID %q", id)
		}
		r.conns[id] = c
	}
	return r, nil
}

// ConnFor returns the connection for id, honoring context cancellation, or an
// error if the host is not configured.
func (r *registry) ConnFor(ctx context.Context, id HostID) (Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.conns[id]
	if !ok {
		return nil, fmt.Errorf("hostconn: no connection for host %q", id)
	}
	return c, nil
}

// Hosts returns the ids of every configured host, sorted for deterministic
// output.
func (r *registry) Hosts() []HostID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]HostID, 0, len(r.conns))
	for id := range r.conns {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Evict removes and closes the connection for id, if present. Any Close error is
// intentionally ignored: eviction removes the (already suspect) handle
// regardless, and callers re-dial via the constructor. A subsequent ConnFor(id)
// fails until the host is re-registered.
func (r *registry) Evict(id HostID) {
	r.mu.Lock()
	c, ok := r.conns[id]
	if ok {
		delete(r.conns, id)
	}
	r.mu.Unlock()
	if ok && c != nil {
		_ = c.Close()
	}
}

// Close closes every connection the Registry holds, empties it, and returns the
// joined errors (if any). It is safe to call more than once.
func (r *registry) Close() error {
	r.mu.Lock()
	conns := r.conns
	r.conns = make(map[HostID]Conn)
	r.mu.Unlock()

	var errs []error
	for id, c := range conns {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close host %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
