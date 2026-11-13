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

package opstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// The clock floor record is read before continuity, committed only after it,
// checked against the stored value inside the commit, and is an ordinary
// record of the chain: scrubbed and verified with the rest.
func TestClockFloorRecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.open()

	rec, found, err := s.LoadClockFloor(ctx)
	require.NoError(t, err, "a read works before continuity")
	assert.False(t, found)
	assert.Nil(t, rec)
	err = s.UpdateClockFloor(ctx, func([]byte) ([]byte, error) { return []byte("v1"), nil })
	require.ErrorIs(t, err, journal.ErrUnattested, "no commit before continuity")
	_, found, _ = s.LoadClockFloor(ctx)
	assert.False(t, found, "a refused commit wrote nothing")

	require.NoError(t, s.EstablishContinuity(ctx))
	require.NoError(t, s.UpdateClockFloor(ctx, func(stored []byte) ([]byte, error) {
		assert.Nil(t, stored)
		return []byte("v1"), nil
	}))
	rec, found, err = s.LoadClockFloor(ctx)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, []byte("v1"), rec)

	// The transaction function sees the stored value and can refuse.
	errLower := errors.New("lower")
	err = s.UpdateClockFloor(ctx, func(stored []byte) ([]byte, error) {
		assert.Equal(t, []byte("v1"), stored)
		return nil, errLower
	})
	require.ErrorIs(t, err, errLower)
	rec, _, _ = s.LoadClockFloor(ctx)
	assert.Equal(t, []byte("v1"), rec, "a refused update leaves the record")
	require.Error(t, s.UpdateClockFloor(ctx, func([]byte) ([]byte, error) { return nil, nil }), "an empty record is refused")

	// It is part of the chain: it survives a restart, and the scrub and the
	// chain verify with it.
	require.NoError(t, s.UpdateClockFloor(ctx, func([]byte) ([]byte, error) { return []byte("v2"), nil }))
	h.restart()
	rec, found, err = h.s.LoadClockFloor(ctx)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, []byte("v2"), rec)
	require.NoError(t, h.s.Journal().Scrub(ctx))
	h.verifyChain()
}
