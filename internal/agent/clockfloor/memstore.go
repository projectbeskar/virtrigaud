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
	"sync"
)

// MemStore is an in-memory Store for tests. It keeps the encoded record, so
// the codec is exercised, and exposes hooks to corrupt the record and to make
// either call fail. It is not durable.
type MemStore struct {
	mu   sync.Mutex
	data []byte
	// LoadErr, when set, is returned by Load.
	LoadErr error
	// SaveErr, when set, is returned by Save without changing the record.
	SaveErr error
	// Saves counts successful Save calls.
	Saves int
}

// Load decodes the stored record.
func (m *MemStore) Load(context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.LoadErr != nil {
		return State{}, m.LoadErr
	}
	if m.data == nil {
		return State{}, ErrNotFound
	}
	var s State
	if err := s.UnmarshalBinary(m.data); err != nil {
		return State{}, err
	}
	return s, nil
}

// Save encodes and stores s.
func (m *MemStore) Save(_ context.Context, s State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SaveErr != nil {
		return m.SaveErr
	}
	b, err := s.MarshalBinary()
	if err != nil {
		return err
	}
	m.data = b
	m.Saves++
	return nil
}

// Raw returns a copy of the stored record, nil when empty.
func (m *MemStore) Raw() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.data...)
}

// SetRaw replaces the stored record with b, for tests that corrupt it.
func (m *MemStore) SetRaw(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = append([]byte(nil), b...)
}
