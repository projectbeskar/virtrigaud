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
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"time"
)

// Errors a Store, Open or a clock source reports. They are matched with
// errors.Is.
var (
	// ErrNotFound is what Store.Load returns when no floor was ever saved.
	ErrNotFound = errors.New("clock floor not found")
	// ErrCorrupt means the stored floor does not decode or fails its
	// checksum.
	ErrCorrupt = errors.New("clock floor corrupt")
	// ErrFloorMissing is Open's refusal when no floor is stored and
	// Config.FirstUse is false: a deleted floor must not read as a new
	// enrolment.
	ErrFloorMissing = errors.New("clock floor missing for an enrolment that has used it")
	// ErrBootTimeRegressed means CLOCK_BOOTTIME read lower than the value
	// persisted in the same boot, which a healthy kernel never does.
	ErrBootTimeRegressed = errors.New("boot time regressed within one boot")
	// ErrUnsupported is returned by the boot clock and boot_id sources on a
	// platform without them.
	ErrUnsupported = errors.New("boot clock is not supported on this platform")
	// ErrClockOutOfRange means a clock reading, or a floor derived from one,
	// is outside the range the floor represents (after MaxFloor). It is a
	// source failure: the floor is never raised to such a value.
	ErrClockOutOfRange = errors.New("clock reading is outside the range the floor represents")
	// ErrTimeNamespace means the process runs in a time namespace with a
	// non-zero clock offset, which would let the namespace's creator inflate
	// the floor.
	ErrTimeNamespace = errors.New("process runs in a time namespace with a clock offset")
)

// MaxFloorYear is the year after which no floor is represented. The floor is
// bounded (after the epoch and not after the start of this year) so that no
// encoding can wrap: a reading beyond the bound is a source failure
// (ErrClockOutOfRange), not a value to raise the floor to.
const MaxFloorYear = 2200

// minFloor and maxFloor are the bounds of State.Floor.
var (
	minFloor = time.Unix(1, 0).UTC()
	maxFloor = time.Date(MaxFloorYear, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// inRange reports whether t is a floor the package represents.
func inRange(t time.Time) bool { return !t.Before(minFloor) && !t.After(maxFloor) }

// State is the persisted floor: the highest time known and the boot-time
// reading it was anchored at.
type State struct {
	// Floor is the persisted lower bound of time, in UTC.
	Floor time.Time
	// BootID is the boot_id of the boot Floor was anchored in.
	BootID string
	// BootTime is the CLOCK_BOOTTIME reading taken when Floor was anchored.
	BootTime time.Duration
}

// Store persists the floor. Implementations must be safe for use by one
// Floor at a time.
//
// Rollback resistance: the floor is only as hard to roll back as the Store
// is. The record's CRC-32 detects accidental corruption and nothing else;
// anyone who can write the record can forge a floor, and replacing the whole
// store with an older consistent copy is not detected here. That is defended
// by the Store the agent will use (a hash-chained, anchored operation store)
// together with the manager checking that a host's reported position never
// goes backwards, not by this package.
type Store interface {
	// Load returns the persisted State. It returns an error that matches
	// ErrNotFound when none was ever saved and any other error (ErrCorrupt
	// for undecodable content) when the floor cannot be read. A Load error
	// other than ErrNotFound is never treated as "no floor".
	Load(ctx context.Context) (State, error)
	// Save atomically and durably replaces the persisted State: a reader
	// after any crash sees the whole old value or the whole new one, never a
	// torn mix (write a temporary file, fsync it, rename it over the old one,
	// fsync the directory; or one committed transaction). It returns nil only
	// after the new value would survive power loss. After an error the caller
	// does not assume either value is stored.
	Save(ctx context.Context, s State) error
}

const (
	stateMagic   = "CFL1"
	stateVersion = 1
	maxBootIDLen = 64
	// stateFixed is the encoded size without the boot_id bytes: magic,
	// version, floor, boot time, boot_id length and checksum.
	stateFixed = len(stateMagic) + 1 + 8 + 8 + 1 + 4
)

// MarshalBinary encodes s as a self-checking record for a Store to write:
// magic, version, floor and boot time as big-endian nanosecond integers, the
// boot_id, and a CRC-32 over everything before it.
func (s State) MarshalBinary() ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("encode clock floor: %w", err)
	}
	buf := make([]byte, 0, stateFixed+len(s.BootID))
	buf = append(buf, stateMagic...)
	buf = append(buf, stateVersion)
	buf = binary.BigEndian.AppendUint64(buf, uint64(s.Floor.UnixNano()))
	buf = binary.BigEndian.AppendUint64(buf, uint64(s.BootTime))
	buf = append(buf, byte(len(s.BootID)))
	buf = append(buf, s.BootID...)
	buf = binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
	return buf, nil
}

// UnmarshalBinary decodes a record MarshalBinary wrote. Anything else, a
// short or long record, a wrong magic, version or checksum, or an invalid
// value, is an error that matches ErrCorrupt. It never panics.
func (s *State) UnmarshalBinary(b []byte) error {
	if len(b) < stateFixed || string(b[:len(stateMagic)]) != stateMagic {
		return fmt.Errorf("%w: bad header", ErrCorrupt)
	}
	if b[len(stateMagic)] != stateVersion {
		return fmt.Errorf("%w: unknown version %d", ErrCorrupt, b[len(stateMagic)])
	}
	idLen := int(b[len(stateMagic)+1+16])
	if len(b) != stateFixed+idLen {
		return fmt.Errorf("%w: bad length", ErrCorrupt)
	}
	body, sum := b[:len(b)-4], binary.BigEndian.Uint32(b[len(b)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}
	off := len(stateMagic) + 1
	out := State{
		Floor:    time.Unix(0, int64(binary.BigEndian.Uint64(b[off:]))).UTC(),
		BootTime: time.Duration(binary.BigEndian.Uint64(b[off+8:])),
		BootID:   string(b[off+17 : off+17+idLen]),
	}
	if err := out.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	*s = out
	return nil
}

// validate checks the values a State may hold.
func (s State) validate() error {
	if !inRange(s.Floor) {
		return fmt.Errorf("floor is outside (epoch, year %d]", MaxFloorYear)
	}
	if s.BootTime < 0 {
		return errors.New("boot time is negative")
	}
	if s.BootID == "" || len(s.BootID) > maxBootIDLen {
		return errors.New("boot_id is empty or too long")
	}
	return nil
}
