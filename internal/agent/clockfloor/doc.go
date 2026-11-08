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

// Package clockfloor keeps a per-host agent's own clock floor: a persisted
// lower bound on the time that never moves backwards. The agent verifies
// operation grants against the floor's reading (Floor.Now, a
// grantverify.Clock), so rolling the wall clock back past the expiry of a
// grant-signing key does not make the expired key valid again.
//
// # Rules
//
//   - The floor is raised only from the agent's own clocks: the wall clock
//     and CLOCK_BOOTTIME. It is never raised from a timestamp inside a grant
//     or any other message, so no peer can push it forward.
//   - Within one boot, the floor also advances with CLOCK_BOOTTIME, which a
//     wall-clock change cannot move and which keeps counting across suspend.
//     That credit is keyed on the kernel's boot_id: a persisted
//     (floor, boot time) pair is credited only when the current boot_id equals
//     the persisted one, so a boot-time reading from another boot is never
//     replayed as elapsed time.
//   - The floor is persisted. After a restart or a reboot it holds even when
//     the wall clock is behind it. Within the new boot the floor keeps
//     advancing with CLOCK_BOOTTIME, so a wall clock that restarted behind
//     stays behind: the host refuses grants (floor ahead of the wall clock
//     beyond Config.MaxAhead, or issued_at beyond the verifier's skew from the
//     floor) until the wall clock is corrected past the floor.
//   - The floor is bounded: it is after the epoch and not after the start of
//     MaxFloorYear. A wall-clock or boot-credit reading beyond the bound is a
//     source failure (ErrClockOutOfRange): the floor is not raised to it, the
//     Floor fails closed (StateFailed, Now returns the zero time) and the
//     persisted floor is untouched, so no encoding can wrap and roll it back.
//   - There is no reset path. If the floor is ahead of the wall clock by more
//     than Config.MaxAhead (the clock jumped forward and was corrected), the
//     host refuses every grant and reports StateClockFloorAhead until the wall
//     clock catches up. Recovery is re-enrolment, which starts a new floor;
//     that is a documented residual, not something this package works around.
//     Status.Ahead should be exported as a metric with an alert, so a forward
//     jump is visible before the clock is corrected.
//   - It fails closed. A floor that cannot be read, that does not decode, that
//     is missing without Config.FirstUse, or whose clock sources fail makes
//     Open return an error, and a floor that fails to persist, whose boot
//     clock regresses or whose clock reading is out of range turns the Floor
//     to StateFailed. In a failed or ahead state Floor.Now returns the zero
//     time, which grantverify refuses.
//   - Open refuses a process in a time namespace with a non-zero clock offset
//     (ProcTimeNS reads /proc/self/timens_offsets, which needs no privilege):
//     the namespace's creator could otherwise inflate the CLOCK_BOOTTIME
//     credit, which is keyed on boot_id and boot_id is not namespaced.
//
// # Persistence
//
// The floor is stored through the Store interface. A Store must replace the
// floor atomically and durably (write a temporary file, fsync it, rename it,
// fsync the directory; or one committed transaction) before a Save returns,
// and must return an error rather than an older value when it cannot read
// what it stored. MemStore is an in-memory Store for tests. The adapter that
// keeps the floor in the agent's operation store comes with that store; this
// package does not depend on it.
//
// The record carries a CRC-32 that detects accidental corruption only. The
// floor's resistance to rollback of the whole store (an older, consistent
// copy restored) is that of the Store: it is defended by the future store
// adapter's anchoring and by the manager-side check that a host's reported
// position does not go backwards, not by this package.
//
// # Calling pattern
//
// The agent calls Observe at a period no longer than Config.PersistInterval
// and Flush on graceful shutdown. A restart in another boot loses the
// boot-time credit accumulated since the last successful save: the
// PersistInterval plus the Observe period, or all of the time since the last
// save when the process is killed without Flush or the host loses power. The
// floor itself is never lost. Floor.Now does no I/O and never blocks on a
// Save.
//
// This package is not used by any binary yet. It must not import
// controller-runtime, the API types or Kubernetes client packages
// (importboundary_test.go).
package clockfloor
