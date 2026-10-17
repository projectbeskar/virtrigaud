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

package libvirt

import (
	"context"
	"errors"
	"sync"
)

// Landing-name serialisation (rc2 R7 security review).
//
// Three operations act on a migration's landed disk "<pool>/<domain>-migrated.qcow2"
// of a single-host provider: ImportDisk lands it (check, then convert and
// rename onto the name), Create attaches it in place (confinement and in-use
// check, then define), and DeleteLandedDisk removes it (in-use check, then
// rm). Each is a check followed by an act. landingLocks serialises them per
// landing volume name inside the provider process, so no two of them both
// pass their check before either acts. DeleteLandedDisk refuses (retryably)
// while an import of the same name is in flight instead of waiting for it:
// the import may be about to land the disk that the delete would report as
// NOT_FOUND. The lock is per provider process: run one replica of a
// single-host libvirt provider per host (the in-use scans see one host only).

// landingLocks is the process-wide landing-name lock set of the libvirt
// provider.
var landingLocks landingLockSet

// errLandingImportInFlight is DeleteLandedDisk's refusal while an ImportDisk
// of the same landing name is in flight in this provider process.
var errLandingImportInFlight = errors.New("an import of this landing name is in flight")

// landingLockSet is a hostLocks set over landing volume names plus the names an
// ImportDisk currently holds or waits for. The zero value is ready to use.
type landingLockSet struct {
	locks hostLocks

	mu        sync.Mutex
	importing map[string]int
}

// lock acquires name's lock (Create's in-place attach), or returns ctx's error.
// The returned func releases it and must be called exactly once.
func (s *landingLockSet) lock(ctx context.Context, name string) (func(), error) {
	return s.locks.lock(ctx, name)
}

// lockForImport marks name as being imported, then acquires its lock. The
// returned func releases the lock and the mark.
func (s *landingLockSet) lockForImport(ctx context.Context, name string) (func(), error) {
	s.mu.Lock()
	if s.importing == nil {
		s.importing = make(map[string]int)
	}
	s.importing[name]++
	s.mu.Unlock()
	unmark := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.importing[name]--
		if s.importing[name] <= 0 {
			delete(s.importing, name)
		}
	}
	unlock, err := s.locks.lock(ctx, name)
	if err != nil {
		unmark()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlock()
			unmark()
		})
	}, nil
}

// lockUnlessImporting acquires name's lock for DeleteLandedDisk: it answers
// errLandingImportInFlight at once — and again after waiting for the lock —
// when an import of name is in flight, and otherwise waits for a Create.
func (s *landingLockSet) lockUnlessImporting(ctx context.Context, name string) (func(), error) {
	if s.importInFlight(name) {
		return nil, errLandingImportInFlight
	}
	unlock, err := s.locks.lock(ctx, name)
	if err != nil {
		return nil, err
	}
	if s.importInFlight(name) {
		unlock()
		return nil, errLandingImportInFlight
	}
	return unlock, nil
}

// importInFlight reports whether an import of name holds or waits for its
// lock.
func (s *landingLockSet) importInFlight(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.importing[name] > 0
}
