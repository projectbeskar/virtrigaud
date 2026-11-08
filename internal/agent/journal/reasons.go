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

import (
	"errors"
	"fmt"
)

// LostReason says why a store lost its persistence guarantee. A store in a
// lost state refuses every commit; only reads are served. The reasons are
// stable strings: they appear in quarantine records and operator output.
type LostReason string

// The lost reasons.
const (
	// LostJournalCorrupt: the store does not open, or a read of it panics or
	// fails, or its chain does not verify.
	LostJournalCorrupt LostReason = "journal_corrupt"
	// LostRolledBack: the store is behind its anchor log.
	LostRolledBack LostReason = "rolled_back"
	// LostInconsistentStore: the store and the anchor log disagree in a way
	// a crash cannot explain, or a witness exists without its step intent.
	LostInconsistentStore LostReason = "inconsistent_store"
	// LostAnchorMissing: the store exists and its anchor log does not.
	LostAnchorMissing LostReason = "anchor_missing"
	// LostAnchorCorrupt: the anchor log has an invalid record before its end,
	// or no valid record at all.
	LostAnchorCorrupt LostReason = "anchor_corrupt"
	// LostJournalMissing: the store is missing while its anchor log or store
	// marker exists.
	LostJournalMissing LostReason = "journal_missing"
	// LostIOError: a write or fsync failed. The page-cache state after a
	// failed fsync is unknown, so the store is never trusted again.
	LostIOError LostReason = "io_error"
)

// reasonCodes gives each reason its anchor-record code. Codes are never
// reused.
var reasonCodes = map[LostReason]uint16{
	LostJournalCorrupt:    1,
	LostRolledBack:        2,
	LostInconsistentStore: 3,
	LostAnchorMissing:     4,
	LostAnchorCorrupt:     5,
	LostJournalMissing:    6,
	LostIOError:           7,
}

// code returns the reason's anchor-record code (0 for none).
func (r LostReason) code() uint16 { return reasonCodes[r] }

// reasonFromCode maps an anchor-record code back to its reason.
func reasonFromCode(c uint16) (LostReason, bool) {
	for r, rc := range reasonCodes {
		if rc == c {
			return r, true
		}
	}
	return "", false
}

// parseReason maps a stored reason string to a LostReason. An unknown string
// maps to LostJournalCorrupt: a quarantine is never lifted by garbage.
func parseReason(s string) LostReason {
	if _, ok := reasonCodes[LostReason(s)]; ok {
		return LostReason(s)
	}
	return LostJournalCorrupt
}

// Errors returned by the store.
var (
	// ErrLost is returned (wrapped in a *LostError) by every commit on a lost
	// store.
	ErrLost = errors.New("journal lost its persistence guarantee")
	// ErrUnattested is returned by a commit before continuity is established.
	ErrUnattested = errors.New("journal continuity not established")
	// ErrCrashed is returned after a test hook simulated a process crash.
	// Nothing more is written; the store must be closed and reopened.
	ErrCrashed = errors.New("journal crashed (injected)")
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("journal closed")
	// ErrSchemaTooNew is returned (wrapped) by Open when the store requires
	// a newer reader than this binary.
	ErrSchemaTooNew = errors.New("journal schema requires a newer agent")
	// ErrNoSync is returned by Open when the store would run without fsync.
	ErrNoSync = errors.New("journal refuses to run without fsync")
	// ErrLocked is returned (wrapped) by Open when another writer holds the
	// store.
	ErrLocked = errors.New("journal locked by another writer")
	// ErrNetworkFS is returned (wrapped) by Open for a state directory on a
	// network or user-space file system.
	ErrNetworkFS = errors.New("journal state directory is not on a local file system")
	// ErrInsecureDir is returned (wrapped) by Open for a state directory that
	// group or others may write.
	ErrInsecureDir = errors.New("journal state directory is not private")
)

// LostError carries the reason of a lost store.
type LostError struct {
	// Reason is the lost reason.
	Reason LostReason
}

// Error implements error.
func (e *LostError) Error() string { return fmt.Sprintf("journal lost{%s}", e.Reason) }

// Unwrap makes errors.Is(err, ErrLost) true.
func (e *LostError) Unwrap() error { return ErrLost }
