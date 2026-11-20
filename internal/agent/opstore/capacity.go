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

package opstore

// Capacity is how full the store is: the measure admission compares with the
// threshold, and the threshold.
type Capacity struct {
	// LiveBytes is the bytes the store's live pages use
	// (journal.Store.LiveSize): admission refuses a new operation when it is
	// above MaxBytes. 0 when the store is closed.
	LiveBytes int64
	// MaxBytes is the store size above which new operations are refused
	// (MaxStoreBytes). 0 means not measured.
	MaxBytes int64
}

// Full reports whether admission refuses new operations at this capacity
// (RefusalStoreFull): LiveBytes above MaxBytes.
func (c Capacity) Full() bool { return c.MaxBytes > 0 && c.LiveBytes > c.MaxBytes }

// Capacity returns how full the store is. It reads the same live size
// admission compares with the threshold, which is the store's size and free
// page count as of its last committed transaction: two counters, not a scan
// of the store, so it is cheap enough for every host answer and never older
// than the last commit.
func (s *Store) Capacity() Capacity {
	return Capacity{LiveBytes: s.j.LiveSize(), MaxBytes: s.opts.MaxStoreBytes}
}
