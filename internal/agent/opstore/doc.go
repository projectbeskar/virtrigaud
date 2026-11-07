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
//     attempt seen per VM and kind, and a VM tombstone per completed Delete
//     of a placement, kept for the life of the enrolment.
//   - An audit bucket of accepted operations and state changes.
//
// # Guarantees
//
//   - At most one execution per op_id within an enrolment: a known op_id is
//     answered from its record or tombstone and never run again; the same
//     op_id with any other identity field or intent digest is refused
//     (OP_ID_CONFLICT). Concurrent deliveries are serialised.
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
//   - Recovery runs only after continuity is established, resolves every
//     in-doubt step by the recovery table (Classify) and never guesses.
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
//     and so is a re-execution that such a restore would allow. Detecting it
//     needs a party outside the host.
//   - The guarantees hold within one enrolment. Re-enrolment ends them for
//     the predecessor's operations, by design, after the freeze.
//   - The store trusts its caller to have verified the grant; it checks the
//     identity fields it is given, not signatures.
package opstore
