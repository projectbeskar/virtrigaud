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

package clockfloor_test

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
)

// TestState_RoundTrip: an encoded State decodes to itself.
func TestState_RoundTrip(t *testing.T) {
	in := clockfloor.State{Floor: t0.Add(123456789 * time.Nanosecond), BootID: "0a1b2c3d-0000-4444-8888-aabbccddeeff", BootTime: 90 * time.Minute}
	b, err := in.MarshalBinary()
	require.NoError(t, err)
	var out clockfloor.State
	require.NoError(t, out.UnmarshalBinary(b))
	assert.True(t, in.Floor.Equal(out.Floor))
	assert.Equal(t, in.BootID, out.BootID)
	assert.Equal(t, in.BootTime, out.BootTime)
}

// TestState_RefusesInvalid: values the floor never holds are not encoded.
func TestState_RefusesInvalid(t *testing.T) {
	for name, s := range map[string]clockfloor.State{
		"zero floor":    {BootID: "a"},
		"no boot id":    {Floor: t0},
		"negative boot": {Floor: t0, BootID: "a", BootTime: -1},
		"long boot id":  {Floor: t0, BootID: string(make([]byte, 65))},
	} {
		_, err := s.MarshalBinary()
		assert.Error(t, err, name)
	}
}

// TestState_RefusesOutOfRangeValues: a record with a valid checksum whose
// floor or boot time does not fit in int64 is corrupt, not a wrapped value.
func TestState_RefusesOutOfRangeValues(t *testing.T) {
	good, err := clockfloor.State{Floor: t0, BootID: "a", BootTime: time.Minute}.MarshalBinary()
	require.NoError(t, err)
	for name, off := range map[string]int{"floor": 5, "boot time": 13} {
		b := append([]byte(nil), good...)
		binary.BigEndian.PutUint64(b[off:], math.MaxUint64)
		binary.BigEndian.PutUint32(b[len(b)-4:], crc32.ChecksumIEEE(b[:len(b)-4]))
		var out clockfloor.State
		assert.ErrorIs(t, out.UnmarshalBinary(b), clockfloor.ErrCorrupt, name)
	}
}

// FuzzStateDecode: decoding arbitrary bytes never panics, and anything that
// decodes re-encodes to the same bytes.
func FuzzStateDecode(f *testing.F) {
	good, _ := clockfloor.State{Floor: t0, BootID: "abc", BootTime: time.Second}.MarshalBinary()
	f.Add(good)
	f.Add([]byte{})
	f.Add([]byte("CFL1"))
	f.Fuzz(func(t *testing.T, b []byte) {
		var s clockfloor.State
		if err := s.UnmarshalBinary(b); err != nil {
			assert.ErrorIs(t, err, clockfloor.ErrCorrupt)
			return
		}
		again, err := s.MarshalBinary()
		require.NoError(t, err)
		assert.Equal(t, b, again)
	})
}
