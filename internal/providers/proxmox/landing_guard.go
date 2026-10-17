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

package proxmox

import "sync"

// Import-stage exclusion (rc2 R7 security review).
//
// ImportDisk writes a stage to a temporary file and renames it onto the stage
// name at the end; DeleteLandedDisk removes the stage. A delete that ran while
// an import of the same stage was in flight would answer NOT_FOUND (nothing
// renamed yet) and leave the stage the import then lands. stageGuards makes
// the two exclusive per stage name inside the provider process, without
// waiting: a delete is refused (retryably) while an import of the name runs,
// and an import is refused (retryably) while a delete of the name runs.
// `qm importdisk` in a Create needs no exclusion: it keeps reading a stage it
// has opened after the stage is unlinked, and one that has not opened it yet
// fails its stage check and removes its VM shell (staged_disk.go). The guard
// is per provider process: run one replica of a Proxmox provider per PVE
// cluster.

// stageGuards is the process-wide import-stage guard of the Proxmox provider.
var stageGuards stageGuardSet

// stageGuardSet records the stage names an import or a delete is working on.
// The zero value is ready to use.
type stageGuardSet struct {
	mu        sync.Mutex
	importing map[string]int
	deleting  map[string]bool
}

// beginImport marks name as being imported and returns its release, or false
// when a delete of name is running.
func (s *stageGuardSet) beginImport(name string) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleting[name] {
		return nil, false
	}
	if s.importing == nil {
		s.importing = make(map[string]int)
	}
	s.importing[name]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.importing[name]--
			if s.importing[name] <= 0 {
				delete(s.importing, name)
			}
		})
	}, true
}

// beginDelete marks name as being deleted and returns its release, or false
// when an import or another delete of name is running.
func (s *stageGuardSet) beginDelete(name string) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.importing[name] > 0 || s.deleting[name] {
		return nil, false
	}
	if s.deleting == nil {
		s.deleting = make(map[string]bool)
	}
	s.deleting[name] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.deleting, name)
		})
	}, true
}
