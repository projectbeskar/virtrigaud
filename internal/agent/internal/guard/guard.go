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
	// AuditTrimOnlyAudit: audit retention removes audit events only, never
	// an op_id tombstone or a VM tombstone.
	AuditTrimOnlyAudit Name = "audit-trim-only-audit"
	// AuditTrimEmittedOnly: audit retention removes only events at or below
	// the persisted audit cursor (emitted to the sink).
	AuditTrimEmittedOnly Name = "audit-trim-emitted-only"
	// AuditReplayCeiling: after a failed replay at open, the audit cursor
	// never passes the last event emitted contiguously, and stays where it
	// is when it could not be read.
	AuditReplayCeiling Name = "audit-replay-ceiling"
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
	// FreezeResumeRecover: recovery finishes a re-enrolment freeze that a
	// crash left incomplete, before it classifies any operation.
	FreezeResumeRecover Name = "freeze-resume-recover"
	// FreezeResumeReenrol: Reenrol finishes an incomplete freeze before it
	// activates the new enrolment.
	FreezeResumeReenrol Name = "freeze-resume-reenrol"
	// ClockFloorNeverLower: the clock floor's commit refuses a value below
	// the one stored.
	ClockFloorNeverLower Name = "clock-floor-never-lower"
	// ClockFloorReadOnlyOpen: the clock floor writes nothing until the agent
	// starts persisting it after continuity is established.
	ClockFloorReadOnlyOpen Name = "clock-floor-read-only-open"
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
	// DeletedOnlyFromVMTombstone: a VM answer says a placement was deleted
	// only from the VM tombstone of its completed Delete, never from the
	// absence of a VM record.
	DeletedOnlyFromVMTombstone Name = "deleted-only-from-vm-tombstone"
	// LookupBeforeEnrolment: the agent's operation path answers a known op_id
	// from the store before it refuses a grant naming another Provider or
	// enrolment, and never refuses an enrolment the store has no record of as
	// definitely not performed.
	LookupBeforeEnrolment Name = "lookup-before-enrolment"
	// RecoveredOpsRun: the agent starts a runner for every operation that
	// recovery left runnable, so that no operation stays running without one.
	RecoveredOpsRun Name = "recovered-ops-run"
	// InventoryValidate: a host inventory is checked against the inventory
	// field rules before it is put in an answer and again before it is
	// signed.
	InventoryValidate Name = "inventory-validate"
	// InventoryOnlyWhenAsked: an inventory is never put in the answer to a
	// request that did not ask for one.
	InventoryOnlyWhenAsked Name = "inventory-only-when-asked"
	// EnrolKeyNeverReplaced: enrolment never replaces an existing host key.
	EnrolKeyNeverReplaced Name = "enrol-key-never-replaced"
	// EnrolCSRKeyType: the enrolment request is built only for an ECDSA
	// P-256 key.
	EnrolCSRKeyType Name = "enrol-csr-key-type"
	// EnrolCSRURIs: the enrolment request carries exactly the host URI and
	// the manager's enrolment record URIs.
	EnrolCSRURIs Name = "enrol-csr-uris"
	// EnrolRequestKey: a certificate is installed only for a request made
	// with the signer's key, refused before anything is written.
	EnrolRequestKey Name = "enrol-request-key"
	// EnrolNoReplace: an installed host certificate is replaced only on
	// request.
	EnrolNoReplace Name = "enrol-no-replace"
	// EnrolSameEnrolment: a certificate for the enrolment that is already
	// installed is not installed again.
	EnrolSameEnrolment Name = "enrol-same-enrolment"
	// EnrolLoaderCheck: the installed file is confirmed with the daemon's own
	// loader, and removed or restored when the loader refuses it.
	EnrolLoaderCheck Name = "enrol-loader-check"
	// EnrolEpochAhead: a replacing certificate is installed only when its
	// start epoch is after the host's last start epoch.
	EnrolEpochAhead Name = "enrol-epoch-ahead"
	// ReenrolEpochAhead: the daemon re-enrols only at a certificate's start
	// epoch that is after the store's last start epoch.
	ReenrolEpochAhead Name = "reenrol-epoch-ahead"
	// ConfirmForActive: a relayed freeze confirmation is applied only when it
	// names the store's active enrolment.
	ConfirmForActive Name = "confirm-for-active"
	// ConfirmOnlyFrozen: a freeze confirmation discards only a frozen
	// enrolment, never the active one.
	ConfirmOnlyFrozen Name = "confirm-only-frozen"
	// RetiredNotHeld: a retired enrolment's operations are answered as
	// discarded (admission, lookups, answers) while a confirmed discard is
	// still removing their records.
	RetiredNotHeld Name = "retired-not-held"
	// DiscardChunked: one transaction of a confirmed discard removes at most
	// the discard chunk's records.
	DiscardChunked Name = "discard-chunked"
	// DiscardResume: compaction finishes a confirmed discard that a crash
	// interrupted.
	DiscardResume Name = "discard-resume"
	// DiscardPrompt: the agent compacts at its next check while a confirmed
	// discard has records left, whatever the compaction backoff.
	DiscardPrompt Name = "discard-prompt"
	// DiscardOnlyRetired: compaction removes the records of an enrolment the
	// discard marker names only when it is retired.
	DiscardOnlyRetired Name = "discard-only-retired"
	// FreezeRefusalReported: a freeze confirmation refused because too many
	// discards are pending is reported as refused over capacity, never as
	// applied, so that the Provider relays it again.
	FreezeRefusalReported Name = "freeze-refusal-reported"
	// SignerRenewalURIs: the signer builds a renewal certificate request only
	// for a host URI identity and an enrolment record.
	SignerRenewalURIs Name = "signer-renewal-uris"
	// SignerActivatePin: the signer puts in use only the pending key the
	// caller names.
	SignerActivatePin Name = "signer-activate-pin"
	// RenewChain: a renewed certificate is staged only when it chains to the
	// pinned host identity issuer and is valid now.
	RenewChain Name = "renew-chain"
	// RenewProfile: a renewed certificate is staged only when it meets the
	// agent certificate profile.
	RenewProfile Name = "renew-profile"
	// RenewMinLifetime: a renewed certificate is staged only when its lifetime
	// is at least hostidentity.MinLifetime (it could not finish a renewal).
	RenewMinLifetime Name = "renew-min-lifetime"
	// RenewSameIdentity: a renewed certificate is staged only when it carries
	// exactly the current certificate's identities (same enrolment, same
	// epoch).
	RenewSameIdentity Name = "renew-same-identity"
	// RenewPendingKey: a renewed certificate is staged only when it certifies
	// the key of the host's own pending renewal.
	RenewPendingKey Name = "renew-pending-key"
	// RenewActivateStaged: only the staged certificate, byte for byte, is put
	// in use.
	RenewActivateStaged Name = "renew-activate-staged"
	// RenewalOnlyWhenAsked: a pending renewal request is put only in the
	// answer of a GetHostInfo that asked for it.
	RenewalOnlyWhenAsked Name = "renewal-only-when-asked"
	// EnrolStatePersistent: every enrolment step refuses, before it writes
	// anything, when the agent's state directory is not on persistent
	// storage.
	EnrolStatePersistent Name = "enrol-state-persistent"
	// SignerKeyDirPersistent: the host key is created, and the signer starts,
	// only when the key file's directory is on persistent storage.
	SignerKeyDirPersistent Name = "signer-key-dir-persistent"
	// BootStartMarker: boot-start starts nothing when the marker of the
	// current boot (the userspace the boot or the last soft reboot started)
	// exists.
	BootStartMarker Name = "boot-start-marker"
	// BootStartMarkerExcl: boot-start starts nothing when its exclusive
	// create of the current boot's marker finds that another run created it
	// first (after this run's existence check).
	BootStartMarkerExcl Name = "boot-start-marker-excl"
	// BootStartStartedSince: boot-start starts nothing when the agent was
	// already started since the current boot (or soft reboot) began.
	BootStartStartedSince Name = "boot-start-started-since"
	// StoreCapacityOnlyWhenAsked: the store capacity is put only in the
	// answer of a GetHostInfo that asked for it (an older manager refuses an
	// answer that carries a field it does not know).
	StoreCapacityOnlyWhenAsked Name = "store-capacity-only-when-asked"
	// OperationLedgerFields: an operation answer's ledger fields (the
	// acceptance position and awaiting_request) are signed only in the
	// answer to a request that asked for the ledger protocol, the head and
	// awaiting_request only for an accepted operation.
	OperationLedgerFields Name = "operation-ledger-fields"
	// LedgerFieldsOnlyWhenAsked: the host ledger fields of an answer (the
	// attestation id and ledger version echo of its context, the ledger
	// state of a host answer) are signed only in the answer to a request that
	// asked for the ledger protocol, or in a host authority answer.
	LedgerFieldsOnlyWhenAsked Name = "ledger-fields-only-when-asked"
	// LedgerReasonsGated: a quarantine an attestation recorded is reported
	// with its own reason only to a request that asked for the ledger
	// protocol (or in a host authority answer), and as rolled_back to any
	// other.
	LedgerReasonsGated Name = "ledger-reasons-gated"
	// OriginOnlyFirstEnrolment: a store records its origin enrolment only in
	// the activation of its first enrolment, never for a store enrolled
	// before origin evidence existed.
	OriginOnlyFirstEnrolment Name = "origin-only-first-enrolment"
	// OriginNeverRewritten: a store's origin enrolment, once recorded, is
	// never replaced.
	OriginNeverRewritten Name = "origin-never-rewritten"
	// LedgerRetentionFloor: a checkpoint prunes no chain entry at or above
	// the position of the last attestation.
	LedgerRetentionFloor Name = "ledger-retention-floor"
	// AttestStoreMatch: an attestation that names another store quarantines
	// the store.
	AttestStoreMatch Name = "attest-store-match"
	// AttestNotAhead: an attestation that names a position beyond the store's
	// chain quarantines the store.
	AttestNotAhead Name = "attest-not-ahead"
	// AttestHeadCompare: an attestation passes only when the store holds the
	// attested head (or a matching checkpoint) and quarantines it otherwise.
	AttestHeadCompare Name = "attest-head-compare"
	// AttestMarkMax: a re-attestation never lowers the ledger mark.
	AttestMarkMax Name = "attest-mark-max"
	// ChallengeSingleUse: an attestation challenge is consumed by the first
	// attestation that names it.
	ChallengeSingleUse Name = "challenge-single-use"
	// ChallengePurpose: an attestation is applied only with a challenge of
	// the purpose the agent's state allows (a first attestation while
	// awaiting one, a re-attestation while attested).
	ChallengePurpose Name = "challenge-purpose"
	// ChallengeExpiry: an attestation challenge is refused once its validity
	// has passed.
	ChallengeExpiry Name = "challenge-expiry"
	// MutationsAfterAttestation: every mutating call is refused until the
	// agent is attested.
	MutationsAfterAttestation Name = "mutations-after-attestation"
	// NoCommitBeforeAttest: the agent starts recovery, and the commits that
	// come with it, only after its first attestation.
	NoCommitBeforeAttest Name = "no-commit-before-attest"
	// LedgerGate: an accepted operation commits its first step intent only
	// once the ledger mark of the current attestation session covers its
	// acceptance.
	LedgerGate Name = "ledger-gate"
	// AwaitRequest: an accepted operation the ledger mark covers starts
	// without its request bytes only when the executor runs its kind without
	// them.
	AwaitRequest Name = "await-request"
	// LedgerBehindCount: a new operation is refused while the accepted
	// operations above the ledger mark are at their bound.
	LedgerBehindCount Name = "ledger-behind-count"
	// LedgerBehindBytes: the request bytes held for parked operations never
	// exceed their budget; a new operation whose request would is refused.
	LedgerBehindBytes Name = "ledger-behind-bytes"
	// LedgerMarkSession: a ledger mark is applied only for the current
	// attestation session.
	LedgerMarkSession Name = "ledger-mark-session"
	// LedgerMarkStoreMatch: a ledger mark that names another store
	// quarantines the store.
	LedgerMarkStoreMatch Name = "ledger-mark-store-match"
	// LedgerMarkNotAhead: a ledger mark beyond the store's chain quarantines
	// the store.
	LedgerMarkNotAhead Name = "ledger-mark-not-ahead"
	// LedgerMarkHeadCompare: a ledger mark is applied only when the store's
	// head at its position equals the mark's head, and quarantines it
	// otherwise.
	LedgerMarkHeadCompare Name = "ledger-mark-head-compare"
	// ExpectKnownRefused: an op_id the store has no record or tombstone of
	// is never accepted when its grant sets expect_known.
	ExpectKnownRefused Name = "expect-known-refused"
	// ReceiptOnlyAccepted: an operation receipt is signed only for an
	// operation whose record is ACCEPTED, in the session the store held when
	// it decided.
	ReceiptOnlyAccepted Name = "receipt-only-accepted"
	// AttestRecordFirst: a store's first attestation commits the store's
	// attestation record before it creates the attestation session, and
	// creates none when that commit fails.
	AttestRecordFirst Name = "attest-record-first"
	// AttestRecordNeverRewritten: a store's attestation record, once
	// committed, is never rewritten by a later attestation.
	AttestRecordNeverRewritten Name = "attest-record-never-rewritten"
	// AttestRecordReported: every host ledger state of version 2 or higher
	// reports the store's attestation record.
	AttestRecordReported Name = "attest-record-reported"
)

// All lists every guard, in a fixed order.
func All() []Name {
	return []Name{
		TombstoneLookup, TombstoneKept, Witness, WitnessKept, AuditTrimOnlyAudit, AuditTrimEmittedOnly, AuditReplayCeiling, AnchorBeforeAck, AnchorFsync,
		CommitAfterContinuity, VMBusy, Binding, Attempt, MintEpoch, QuarantinePersist, RefuseNoSync,
		LockFile, MarkerCheck, QuarantineRewrite, WitnessScan, CommitErrorQuarantine, KnownBeforeMismatch,
		CloseAfterIOError, UnknownEnrolmentNotDefinite, FreezeResumeRecover, FreezeResumeReenrol,
		ClockFloorNeverLower, ClockFloorReadOnlyOpen, SignerPeerCheck, SignerClientPeerCheck, SignerDigestCheck,
		SignerRateLimit, SignerVerify, HostKeyUnreadable, SourceThrottle, HandshakeCap,
		DeletedOnlyFromVMTombstone, LookupBeforeEnrolment, RecoveredOpsRun, InventoryValidate, InventoryOnlyWhenAsked,
		EnrolKeyNeverReplaced, EnrolCSRKeyType, EnrolCSRURIs, EnrolRequestKey, EnrolNoReplace, EnrolSameEnrolment,
		EnrolLoaderCheck, EnrolEpochAhead, ReenrolEpochAhead, ConfirmForActive, ConfirmOnlyFrozen,
		RetiredNotHeld, DiscardChunked, DiscardResume, DiscardPrompt, DiscardOnlyRetired, FreezeRefusalReported,
		RenewMinLifetime, RenewChain, RenewProfile, RenewSameIdentity, RenewPendingKey, RenewActivateStaged,
		RenewalOnlyWhenAsked, SignerRenewalURIs, SignerActivatePin,
		EnrolStatePersistent, SignerKeyDirPersistent, BootStartMarker, BootStartMarkerExcl,
		BootStartStartedSince, StoreCapacityOnlyWhenAsked, OperationLedgerFields,
		LedgerFieldsOnlyWhenAsked, LedgerReasonsGated, OriginOnlyFirstEnrolment, OriginNeverRewritten,
		LedgerRetentionFloor, AttestStoreMatch, AttestNotAhead, AttestHeadCompare, AttestMarkMax,
		ChallengeSingleUse, ChallengePurpose, ChallengeExpiry, MutationsAfterAttestation, NoCommitBeforeAttest,
		LedgerGate, AwaitRequest, LedgerBehindCount, LedgerBehindBytes, LedgerMarkSession, LedgerMarkStoreMatch,
		LedgerMarkNotAhead, LedgerMarkHeadCompare, ExpectKnownRefused, ReceiptOnlyAccepted,
		AttestRecordFirst, AttestRecordNeverRewritten, AttestRecordReported,
	}
}
