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

// Package guard names the safety guards of the per-host agent's operation
// store, so that a mutation test build can switch one of them off.
//
// In every ordinary build Off is a constant false and the compiler removes the
// checks. Only a test binary built with the virtrigaud_mutation build tag reads
// the VIRTRIGAUD_MUTATION environment variable and reports the named guard as
// switched off. hack/agent-mutation.sh runs the agent tests once per guard and
// reports every guard whose removal no test notices.
package guard

// Name identifies one guard.
type Name string

// The guards. Each one is a check the store's safety argument relies on.
const (
	// TombstoneLookup: admission consults op_id tombstones.
	TombstoneLookup Name = "tombstone-lookup"
	// TombstoneKept: compaction keeps tombstones.
	TombstoneKept Name = "tombstone-kept"
	// Witness: a once step creates its witness before its effect.
	Witness Name = "witness"
	// WitnessKept: compaction keeps witness files.
	WitnessKept Name = "witness-kept"
	// AnchorBeforeAck: a commit returns only after its anchor record is
	// appended.
	AnchorBeforeAck Name = "anchor-before-ack"
	// AnchorFsync: the anchor append is fsynced.
	AnchorFsync Name = "anchor-fsync"
	// CommitAfterContinuity: no commit happens before continuity is
	// established.
	CommitAfterContinuity Name = "commit-after-continuity"
	// VMBusy: admission refuses a second non-terminal operation for a VM.
	VMBusy Name = "vm-busy"
	// Binding: admission checks the VM binding.
	Binding Name = "binding"
	// Attempt: admission refuses an attempt that does not increase.
	Attempt Name = "attempt"
	// MintEpoch: admission refuses a first delivery minted before the
	// enrolment started.
	MintEpoch Name = "mint-epoch"
	// QuarantinePersist: a quarantine is written durably.
	QuarantinePersist Name = "quarantine-persist"
	// RefuseNoSync: opening a store refuses unsynchronised bbolt options.
	RefuseNoSync Name = "refuse-nosync"
	// LockFile: Open takes the single-writer lock file before touching any
	// other file.
	LockFile Name = "lock-file"
	// MarkerCheck: a missing or mismatched store marker is a loss, not
	// rewritten.
	MarkerCheck Name = "marker-check"
	// QuarantineRewrite: an open that finds a loss re-writes every missing
	// quarantine record.
	QuarantineRewrite Name = "quarantine-rewrite"
	// WitnessScan: recovery treats a witness of an unknown operation as an
	// inconsistent store.
	WitnessScan Name = "witness-scan"
	// CommitErrorQuarantine: a bbolt commit error that did not come from the
	// transaction function quarantines the store.
	CommitErrorQuarantine Name = "commit-error-quarantine"
	// CloseAfterIOError: after an I/O failure the store stops serving reads
	// through bbolt's in-memory state.
	CloseAfterIOError Name = "close-after-io-error"
	// UnknownEnrolmentNotDefinite: a grant naming an enrolment the store has
	// no record of is not refused as definitely not performed.
	UnknownEnrolmentNotDefinite Name = "unknown-enrolment-not-definite"
	// KnownBeforeMismatch: admission answers a known op_id from its record
	// or tombstone before refusing on the enrolment.
	KnownBeforeMismatch Name = "known-before-mismatch"
	// SignerPeerCheck: the signer serves only the agent's uid.
	SignerPeerCheck Name = "signer-peer-check"
	// SignerClientPeerCheck: the agent talks only to the signer's uid.
	SignerClientPeerCheck Name = "signer-client-peer-check"
	// SignerDigestCheck: the signer signs only a SHA-256 digest of exactly
	// 32 bytes.
	SignerDigestCheck Name = "signer-digest-check"
	// SignerRateLimit: the signer refuses signatures above its rate limit.
	SignerRateLimit Name = "signer-rate-limit"
	// SignerVerify: the agent verifies the signature the signer returns.
	SignerVerify Name = "signer-verify"
	// SourceThrottle: connections above a source address's rate are closed
	// before the TLS handshake.
	SourceThrottle Name = "source-throttle"
	// HandshakeCap: at most a fixed number of TLS handshakes run at once.
	HandshakeCap Name = "handshake-cap"
	// HostKeyUnreadable: the agent refuses to start when it can read the
	// host key file.
	HostKeyUnreadable Name = "host-key-unreadable"
)

// All lists every guard, in a fixed order.
func All() []Name {
	return []Name{
		TombstoneLookup, TombstoneKept, Witness, WitnessKept, AnchorBeforeAck, AnchorFsync,
		CommitAfterContinuity, VMBusy, Binding, Attempt, MintEpoch, QuarantinePersist, RefuseNoSync,
		LockFile, MarkerCheck, QuarantineRewrite, WitnessScan, CommitErrorQuarantine, KnownBeforeMismatch,
		CloseAfterIOError, UnknownEnrolmentNotDefinite, SignerPeerCheck, SignerClientPeerCheck, SignerDigestCheck,
		SignerRateLimit, SignerVerify, HostKeyUnreadable, SourceThrottle, HandshakeCap,
	}
}
