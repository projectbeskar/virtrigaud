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

// Package opstore is the per-host agent's durable operation store: it decides
// whether a signed operation may run on this host, runs its steps at most once
// through an executor.Executor, and remembers the outcome for the life of the
// host's enrolment. It is a library; no binary uses it yet. It imports no
// Kubernetes package and never stores request bytes.
//
// # What it keeps
//
//   - An operation record per (enrolment_id, op_id): the identity fields of
//     the verified grant (Provider, enrolment, op_id, VM, binding, kind,
//     attempt, salted intent digest, mint epoch), the state, the irrev flag
//     and the step journal.
//   - An op_id tombstone, written in the same transaction as an operation's
//     terminal state, never dropped while the enrolment lasts.
//   - A witness file per once step, created with O_EXCL and made durable
//     before the step's effect, never removed while the enrolment lasts.
//   - A VM record per VM (binding, the operation that holds it), the highest
//     accepted attempt per VM and kind (for the life of the enrolment), and a
//     VM tombstone per completed Delete of a placement, kept across
//     re-enrolments.
//   - The store marker's binding to the host and the active enrolment, and
//     the ids of discarded enrolments, never activated again.
//   - An audit bucket of accepted operations and state changes.
//
// # Guarantees
//
//   - At most one execution per op_id within an enrolment: a known op_id is
//     answered from its record or tombstone, whatever the state of its
//     enrolment, and never run again; the same op_id with any other identity
//     field or intent digest is refused (OP_ID_CONFLICT). Concurrent
//     deliveries are serialised.
//   - Only GRANT_MISMATCH, PROVIDER_MISMATCH and a full store are definite
//     not-performed refusals, as in the provider contract, and only for an
//     op_id the store has no trace of in an enrolment whose records it still
//     holds. An op_id of a discarded enrolment is refused UNKNOWN_TO_AGENT.
//   - At most one operation holds a VM at a time (VM_BUSY); an operation
//     whose outcome is unknown, and a dirty failure, keep holding it.
//   - A non-Create operation needs the VM's record with the same binding;
//     a Create is refused for another binding or for a placement whose Delete
//     completed. Attempts per VM and kind only increase. A first delivery
//     minted before the enrolment started is refused.
//   - An operation is acknowledged accepted only after its record is
//     committed and anchored. A once step never runs twice: after a crash
//     with its witness present the effect is decided by inspection, and the
//     absence of evidence gives OutcomeUnknown, never "not performed". An
//     irreversible step is never run twice: its goal state decides.
//   - Recovery runs once, only after continuity is established. It first
//     checks every witness file: a witness of an operation or enrolment the
//     store has no record of quarantines it (inconsistent_store). It then
//     resolves every in-doubt step by the recovery table (Classify) and never
//     guesses. A writer never overwrites a record that left the runnable
//     states behind its back.
//   - A store whose marker names another host or enrolment is quarantined at
//     open.
//   - Compaction drops only the step journal of a finished operation whose
//     tombstone is present; it never drops a tombstone, a witness or a VM
//     tombstone.
//   - Re-enrolment freezes the predecessor's open operations (retired when
//     no step intent exists, otherwise OutcomeUnknown) before the new
//     enrolment becomes active, and discards the predecessor's records only
//     when the freeze is confirmed.
//
// # Limits
//
//   - The store's continuity checks are local (package journal): a joint
//     restore of the store and its anchor log is undetectable on the host,
//     and so is a re-execution that such a restore would allow, except that
//     a once step whose witness survived is still never run again, and a
//     surviving witness of a lost operation is detected at recovery.
//   - The loss of the whole state set (store, anchor log, markers, quarantine
//     record and witnesses) looks like a fresh host. Enrol cannot know that
//     an enrolment id was used before; enrolling a fresh store with an old
//     enrolment id would accept that enrolment's op_ids again. The defence is
//     outside the host: a manager that remembers the host's store_id and
//     last chain position (Position) and holds a host whose store changes.
//   - The guarantees hold within one enrolment. Re-enrolment ends them for
//     the predecessor's operations, by design, after the freeze.
//   - The store trusts its caller to have verified the grant; it checks the
//     identity fields it is given, not signatures.
//   - A Create of a placement that has no record (for example after a
//     confirmed freeze released it) relies on the executor not to overwrite
//     an existing VM.
//   - The audit sink is called with the store lock held; it must not call
//     back into the store.
package opstore
