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
package journal

// Point names a place in the commit, recovery or maintenance path where a test
// may simulate a process crash. Production code passes no hooks.
type Point string

// Journal crash points. The operation store defines more in its own package.
const (
	// PointAfterStoreCommit: the bbolt transaction is committed (fsynced);
	// its anchor record is not yet appended. Nothing was acknowledged.
	PointAfterStoreCommit Point = "after-store-commit"
	// PointAfterAnchor: the anchor record is appended and fsynced; the
	// commit has not returned, so nothing was acknowledged yet.
	PointAfterAnchor Point = "after-anchor"
	// PointMidAnchorRotation: the new anchor file is written and fsynced;
	// it has not replaced the old one.
	PointMidAnchorRotation Point = "mid-anchor-rotation"
)

// Hooks are fault-injection hooks for tests. Every field may be nil.
type Hooks struct {
	// Crash is consulted at every crash point. Returning true simulates the
	// process dying at that point: the store writes nothing more and every
	// later call fails with ErrCrashed until it is closed and reopened.
	Crash func(p Point) bool
	// StoreCommitError, when it returns an error, makes the next commit fail
	// at the end of the transaction function, before bbolt writes anything.
	// The journal treats it as a failed commit and quarantines the store with
	// LostIOError. It does not reproduce what a real fdatasync failure leaves
	// behind (written pages, bbolt's in-memory meta); a test that needs that
	// makes bbolt's own fdatasync fail (see commit_linux_test.go).
	StoreCommitError func() error
}

// Event is a journal event reported to Options.Events, for example to write a
// structured system-journal line. It never carries record contents.
type Event struct {
	// Kind is "quarantine".
	Kind string
	// Reason is the lost reason.
	Reason LostReason
	// Seq is the chain position at the event.
	Seq uint64
	// Detail is a short explanation.
	Detail string
}

// EventQuarantine is the Kind of a quarantine Event.
const EventQuarantine = "quarantine"
