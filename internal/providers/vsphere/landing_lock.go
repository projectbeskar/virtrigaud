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
	"sync"
)

// keyedMutex is a set of mutexes, one per key, created on demand and dropped
// when unused. The zero value is ready to use. Provider.landingLocks uses it
// to serialise the work on one migration landing folder.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

// keyedLock is one key's lock: a one-slot channel (held while full) and the
// number of holders and waiters, so it is dropped when nobody needs it.
type keyedLock struct {
	slot chan struct{}
	refs int
}

// lock acquires key's lock, or returns ctx's error if ctx ends first. The
// returned func releases it and must be called exactly once.
func (k *keyedMutex) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*keyedLock)
	}
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{slot: make(chan struct{}, 1)}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	select {
	case l.slot <- struct{}{}:
	case <-ctx.Done():
		k.release(key, l)
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.slot
			k.release(key, l)
		})
	}, nil
}

// release drops one holder or waiter of key's lock l.
func (k *keyedMutex) release(key string, l *keyedLock) {
	k.mu.Lock()
	defer k.mu.Unlock()
	l.refs--
	if l.refs == 0 {
		delete(k.locks, key)
	}
}
