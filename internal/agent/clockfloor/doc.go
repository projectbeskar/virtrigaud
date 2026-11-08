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
//     the wall clock is behind it; verification then refuses grants (the
//     time the verifier sees is the floor, so a grant issued in real time is
//     issued in the verifier's future) until the clock passes the floor.
//   - There is no reset path. If the floor is ahead of the wall clock by more
//     than Config.MaxAhead (the clock jumped forward and was corrected), the
//     host refuses every grant and reports StateClockFloorAhead until the wall
//     clock catches up. Recovery is re-enrolment, which starts a new floor;
//     that is a documented residual, not something this package works around.
//   - It fails closed. A floor that cannot be read, that does not decode, that
//     is missing without Config.FirstUse, or whose clock sources fail makes
//     Open return an error, and a floor that fails to persist or whose boot
//     clock regresses turns the Floor to StateFailed. In a failed or ahead
//     state Floor.Now returns the zero time, which grantverify refuses.
//
// # Persistence
//
// The floor is stored through the Store interface. A Store must make a
// successful Save durable (fsync of the data and of the directory entry, or a
// committed and anchored transaction) before returning, and must return an
// error rather than an older value when it cannot read what it stored.
// MemStore is an in-memory Store for tests. The adapter that keeps the floor
// in the agent's operation store comes with that store; this package does not
// depend on it.
//
// How often the floor is persisted is bounded by Config.PersistInterval: a
// reboot loses at most that much boot-time credit, never the floor itself.
//
// This package is not used by any binary yet. It must not import
// controller-runtime, the API types or Kubernetes client packages
// (importboundary_test.go).
package clockfloor
