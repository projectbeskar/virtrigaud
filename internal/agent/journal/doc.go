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

// Package journal is the durable journal of the per-host agent's operation
// store: a bbolt store opened with full fsync, a hash chain inside it, and a
// separately fsynced, append-only anchor log beside it. It is a library; no
// binary uses it yet. It imports no Kubernetes package.
//
// # Commit path
//
// Every write transaction is one chain step: seq = previous + 1 (counting
// from 1) and head = SHA-256("vrtg-journal-v2" || previous head || be64(seq)
// || digest || state), where digest is SHA-256 over the transaction's client
// (bucket, key, value) writes, sorted, as written, and state is the
// live-state digest after the commit (see below). The chain entry stored for
// the step is digest || state || head, and a checkpoint stores the head and
// the head before it, so that the newest step can still be recomputed from a
// checkpoint after older entries are pruned. The order is: bbolt commit (fsync), then
// append (seq, head, store_id) to the anchor log and fsync it, then return.
// A caller makes a change observable only after Update returns, so nothing
// acknowledged is ever ahead of the anchor log.
//
// # Record trailers and the live-state digest
//
// Every value in a client bucket is stored as value || trailer, where the
// trailer is a format version byte (1) || be64(seq of the commit that wrote
// it) || a 32-byte SHA-256 over "vrtg-journal-rec-v1" || store_id ||
// be32(len bucket) || bucket || be32(len key) || key || be64(seq) || value.
// Tx.Put writes it; Tx.Get and Tx.ForEach verify and strip it, so a value
// cannot be read under another key, bucket or store, nor with a changed byte.
// A mismatch, an unknown version, a value shorter than its trailer, or a seq
// above the committed position is an error that wraps ErrCorruptRecord, never
// a missing key, and quarantines the store with LostJournalCorrupt (the
// detail names the bucket and the key, never the value).
//
// The meta bucket also holds a live-state digest: the sum, modulo 2^256, of
// the digests of all live client records. Every commit updates it
// incrementally (the old record's digest is subtracted on an overwrite or a
// delete, the new one added) and writes it as part of the commit. The chain
// head of that commit hashes the state digest (the meta write itself is not
// repeated in the transaction digest), so a recorded digest that was edited,
// even together with the records, no longer matches the chain: Open checks
// that the newest chain entry's state is the recorded one, and that the chain
// recomputes. Open also recomputes the state from a full scan, after the
// chain check and before anything is served; Store.Scrub does the same, and
// the chain check, on demand, and the agent daemon runs it periodically.
// Together with the per-record trailers this catches a stale page version of
// a record, a lost record and a resurrected delete. The scan also verifies
// every trailer.
//
// # Guarantees
//
//   - An acknowledged commit survives a crash or power loss on storage that
//     honours fsync.
//   - At open, before any commit, the store first checks min_reader_version
//     (a store that needs a newer reader refuses to open, with the required
//     version in the error, read through a read-only handle before the store
//     is opened for writing or any file of the state directory is changed, so
//     an older binary never quarantines or alters a newer store), then reads its identity, position and
//     quarantine record, so that any loss names the store and the position.
//     A store whose schema predates record verification, or whose meta has no
//     live-state digest, is lost{journal_corrupt}: it is not migrated. The
//     chain is recomputed from its newest checkpoint, or from the one before
//     it when the entries between them are retained (which authenticates the
//     newest checkpoint), including the state digest it committed to, and the
//     position is compared with the anchor log (the head compared in the
//     one-commit-ahead case is the one the recomputation produced):
//     store behind the anchor ⇒ rolled_back; store more than one ahead, or
//     at the same position with another head, or of another store ⇒
//     inconsistent_store; store missing ⇒ journal_missing; anchor missing ⇒
//     anchor_missing; an invalid anchor record before the last ⇒
//     anchor_corrupt. Exactly one unacknowledged commit (the process died
//     between the bbolt commit and the anchor append) is accepted and
//     re-anchored. A zero-filled, partial or otherwise invalid last anchor
//     record is a torn, unacknowledged append and is cut off. An anchor log
//     larger than 64 MiB is anchor_corrupt: its size is checked before it
//     is read, and the read stops one byte past the bound. This detects a
//     bbolt meta-page fallback after an acknowledged commit, a restore of the
//     store file alone, and a lost or damaged anchor log.
//   - A store that fails a check, or whose commit, write or fsync fails, is
//     lost: it refuses every commit and serves reads only. Any error of a
//     bbolt commit that did not come from the transaction function counts
//     (bbolt returns a failed fdatasync as a bare errno). A loss is recorded
//     in a quarantine marker file, in the anchor log and in the store; any
//     one of them keeps it quarantined across a restart, and every open that
//     finds a loss writes the missing ones again (the store record is not
//     written into a rolled-back, corrupt or inconsistent store, nor one
//     whose anchor log is missing, which are kept as evidence; a missing
//     anchor log is re-created holding only the quarantine record). The same
//     rule applies when a loss is found while the store runs: a store found
//     corrupt gets no record written into it, and the marker file and the
//     anchor log record the loss. Only a corruption is found again by a later
//     open (its scan and chain check): a journal_corrupt loss does not depend on
//     the records alone. A reason a client reports (rolled_back,
//     inconsistent_store, anchor_missing, io_error) is kept only by those
//     records, so when neither the marker file nor the anchor log took the
//     record, it is written into the store as the last resort (the evidence
//     cost is accepted). A failed fsync is never retried.
//   - A store marker binds the store_id to the host and its enrolment
//     (written by the operation store). A store that has committed and whose
//     marker is missing, or names another store, is inconsistent_store.
//   - No commit happens before continuity is established
//     (EstablishContinuity); schema migrations run after it, so a migration
//     never hides a rollback.
//   - A damaged data page, or a record edited in place, that still decodes is
//     detected: on read through its trailer, and at open and by Store.Scrub
//     through the live-state digest, which also catches a record that was lost,
//     restored to an older version, or deleted and brought back.
//   - bbolt is never opened without fsync; a corrupt store surfaces as
//     journal_corrupt, never as a panic of the caller. A memory fault on
//     bbolt's mapping (unreadable blocks, a truncated file) is turned into such
//     a panic (the runtime's panic-on-fault setting is switched on, and then
//     restored, on the calling goroutine in every call that reads or writes
//     the store). After a panic inside bbolt's own start of a transaction, the
//     store drops the database and releases its file lock, so a later open in
//     the same process reports the loss instead of failing with ErrLocked; the
//     memory mapping stays until the process exits.
//   - A first open that died after bbolt created the store file and before the
//     journal committed anything (no anchor log, marker, quarantine record,
//     anchor-rotation file or bucket exists) is started again, like one that
//     died before its anchor log was complete. The old store file is set aside
//     as state.db.unfinished-<unix nanoseconds> (linked, then unlinked, and the
//     directory fsynced; a name is never overwritten), not removed. A second
//     unfinished first open, with such a file already there, is an Open error
//     for the operator to look at: nothing piles up and nothing is written.
//     Any such record, or any committed data, keeps the store lost.
//   - One writer at a time: Open takes an exclusive flock on the state
//     directory itself and then on state.db.lock (re-checked against the
//     file at the path) before it reads or changes any other file, and holds
//     both until Close; a second Open fails with ErrLocked and changes
//     nothing, also when the lock file was deleted. bbolt's own file lock is
//     a third guard. A state directory writable by group or others is
//     refused.
//   - The state directory must be on a local file system. On Linux, Open
//     refuses, best-effort by statfs magic number, NFS, SMB/CIFS, Ceph, AFS,
//     Coda, 9p, FUSE (which includes virtiofs), Lustre, GPFS, GFS2, OCFS2,
//     BeeGFS and OrangeFS, whose fsync, flock and O_EXCL semantics the journal
//     cannot rely on; on other systems, and for file systems the list does not
//     name, this is the operator's responsibility. Shared volumes in some
//     development setups (virtiofs, WSL2 drive mounts) are refused.
//   - The state directory must not be renamed or moved while a Store holds
//     it: the lock follows the directory, but files are opened by path.
//   - After an I/O failure the store serves no more reads (bbolt's in-memory
//     state may show a commit that never reached the disk); a reopen reads
//     what is on disk and stays quarantined.
//   - Position.MarshalBinary gives (store_id, seq, head) in the fixed form an
//     agent can sign and a manager can compare.
//
// # Limits
//
//   - A simultaneous restore of both the store and the anchor log to an
//     earlier consistent pair (a disk snapshot revert, a re-imaged host) is
//     undetectable locally. So is the loss of the whole state set (store,
//     anchor log, markers and quarantine record together): it looks like a
//     fresh host. Only a party outside the host that remembers a later chain
//     position, or this store's store_id, can detect either.
//   - An attacker with root on the host can rewrite both files.
//   - Storage that acknowledges a flush it did not perform defeats the
//     durability guarantee.
//   - The checks rest on the file-system semantics of fsync on the file and
//     on its directory. bbolt checksums its meta pages only; the chain is
//     verified from its stored digests, not against the live buckets. Damage
//     to a data page is found by the record trailers and the live-state
//     digest, and only when it is read, at open or by Store.Scrub: nothing
//     checks a page at write time, and a record that is never read is found by
//     the next open or scrub.
//   - The live-state digest is an additive (sum modulo 2^256) multiset digest.
//     It resists corruption, not an adversary: root on the host can rewrite the
//     records, the digest and the chain, and the anchor log. A simultaneous,
//     consistent rollback of the store and the anchor log to an earlier pair is
//     still undetectable locally.
//   - After a panic inside bbolt, a call that was already waiting on bbolt's
//     locks in another goroutine is not released. The goroutine that faulted
//     never waits for the store lock: if a writer holds it, the quarantine is
//     applied by the next call that takes it. After the fault no call enters
//     bbolt again; every call returns the loss.
//   - A record whose bytes vanish, or revert, after the last open or scrub
//     reads as absent (or as the older version, which its trailer cannot tell)
//     until the next scrub finds the live-state digest wrong. Reads verify what
//     they find, not what they fail to find.
//   - Any panic inside a caller's transaction function quarantines the store
//     with journal_corrupt: the journal cannot tell a bbolt fault from the
//     caller's own, and a loss is the safe answer. A Tx must not be used from
//     any other goroutine than the one running the function: the runtime's
//     panic-on-fault setting is per goroutine and does not cover them.
//   - Size and cost. The store holds the live records of one host's
//     operations, one chain entry (96 bytes) for each of the last two
//     checkpoint intervals (2048 commits by default) and a 64 byte checkpoint
//     for every interval, for the life of the store; it is expected to stay in
//     the order of megabytes. Open reads every live byte once, so its cost is
//     linear in the live bytes; it is bounded by the context given to Open,
//     and a context that ends first fails the open with the context's error
//     (the store is not served). Open's full scan does not run for a store
//     that is already lost: such a store serves reads, each of which still
//     verifies its trailer, but nothing verifies the whole. Store.Scrub holds
//     one read transaction for its whole run: commits proceed, but a writer
//     that needs the file to grow waits until the scrub ends.
//   - Options.Events is called with the store lock held: it must not call
//     back into the store.
package journal
