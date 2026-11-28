# Host ledger: how it is verified

The host ledger protocol (development builds only, build tag `virtrigaud_dev`) is
described in [Host ledger protocol](operation-grants-wire-shape.md#host-ledger-protocol-reserved)
and [Host ledger state](host-agent-enrolment.md#host-ledger-state). This page says what
evidence backs its safety and liveness claims, and how to run it.

## The whole-protocol model test

`TestHostLedgerProtocolModel` in `internal/controller/host_ledger_model_dev_test.go`
runs a seeded, deterministic schedule of interleaved events against one host, across
at least two manager processes per run, and checks the protocol's invariants after
every step.

### What runs for real

- The agent's operation store (`opstore.Store` with the ledger gate), on an in-memory
  file system for its anchor log and a memory-backed directory for its database. An
  in-process executor counts every effect; no rollback undoes that count. The real
  code covers admission, the ledger gate in `Execute`, attestation and re-attestation,
  ledger marks, retire-if-unknown with its trace scan, recovery, and re-enrolment with
  its freeze.
- The manager's host ledger library (`HostLedger`), over a controller-runtime fake
  API server. The real code covers starting a ledger (first contact, and archiving an
  earlier store), moving it to a replaced Host, beginning, raising, adopting and
  re-asserting sessions, the HMAC tags, the per-process guard, the write counter, the
  ledger index, and every hold.
- The ledger evidence a ledger is started from: a stand-in host key signs a host
  answer with the store's real position, origin and attestation record, and the real
  answer verifier checks it.
- The manager's only signing points for attestation, ledger mark and
  retire-if-unknown grants, its retirement decision, and its ledger raise over a
  retirement.

### What is modelled

- **The wire.** A grant the manager signs reaches the agent's store as the order it
  names. Grant signing, the grant verifier and the transport have their own tests;
  here the agent's grant check is reduced to the enrolment the grant names. A message
  can be delivered at once, delivered later in any order, or lost.
- **The agent server's challenge registry.** At most four challenges are outstanding
  per purpose, held in memory. The server refuses operations while the store awaits
  attestation, is freezing, or is lost.
- **The attestation driver and the VirtualMachine controller's attempts.** This
  covers the attempt markers and their receipts, the intent's ledger stamp, re-sends
  with `expect_known`, and polling. They follow the real driver's steps, but call the
  real library above.
- **etcd.** A snapshot, and a restore of all of it together: the fake API server's
  objects (ledger Secrets, Hosts), the pins state (enrolment, store state, ledger
  index, holds) and the attempt markers.
- **Manager processes.** A downgrade window (an older build signs nothing at grant
  version 2 and never touches the ledger), leader changes with two writers, and
  restarts (a new process has no guard, no own writes and no holds in memory).

### The events

Each run starts with a first contact. It then draws about 90 random events and ends
with a quiescent phase. The events are:

- operation delivery: prepare, send, re-send, poll, and lost or delayed replies;
- the attestation driver: begin a session, attest, adopt, re-attest;
- heartbeat raises and ledger marks;
- agent restarts, and rollbacks of the agent's store (with its anchor, which the host
  cannot detect, or without);
- etcd snapshots and restores (with a manager restart, without one, or of the ledger
  object alone);
- a deleted ledger object;
- leader changes with two writers, manager restarts and downgrade windows;
- re-enrolment on the same store (the frozen-enrolment closure);
- reinstallation of the agent with a new store under a new enrolment (archive);
- replacement of the Host object (rebind);
- retire-if-unknown, and advancing the clock.

Each run turns on a random subset of the faults; about a third of the runs turn on
none.

### The invariants

1. **Retired means no effect.** An attempt the manager ever closed `Retired` never
   has an effect. A run that injects both an etcd restore and a rollback of the
   agent's store with its anchor is outside the protocol's claim. Such runs are
   counted and reported, never failed.
2. **Nothing starts before the ledger covers it.** When a step runs, the agent is
   attested and its ledger mark covers the operation's acceptance. The mark must also
   be a ledger position the API server acknowledged.
3. **At most one effect per operation.** Runs that roll the agent's store back with
   its anchor are exempt (counted and reported).
4. **Nothing is signed for an unattested or unbound host.** Every attestation, ledger
   mark, retirement and operation grant names a session and a ledger position of an
   acknowledged ledger object. That object was bound to the Host object, not archived,
   and Attested in that session; an attestation names a pending session instead.
5. **No permanent wedge.** Once the adversary stops, every run reaches a quiescent
   state. In that state every attempt is closed, or the host is held for a documented
   reason (see [Holds](host-agent-enrolment.md#holds)). A run that injected no fault
   able to hold a host for good must end with the ledger Attested in the agent's
   session and every attempt closed. The faults that can are a rollback of the
   agent's store, a restore while the manager runs, a partial restore, a restore
   across an enrolment or store change, and a deleted ledger.

A failing run prints its seed, the shortest prefix of its schedule that still fails,
and that prefix's trace.

### Running it

```sh
# Default: 1000 seeds (200 with -short), a few seconds on a workstation.
go test -tags virtrigaud_dev -run '^TestHostLedgerProtocolModel$' ./internal/controller

# The full run: one million seeds (about 40 minutes on 14 cores).
VIRTRIGAUD_PROTOCOL_MODEL_SEEDS=1000000 VIRTRIGAUD_PROTOCOL_MODEL_WORKERS=14 \
  go test -tags virtrigaud_dev -timeout 0 -run '^TestHostLedgerProtocolModel$' ./internal/controller

# One seed, with its trace on failure.
VIRTRIGAUD_PROTOCOL_MODEL_SEED=42 \
  go test -tags virtrigaud_dev -run '^TestHostLedgerProtocolModel$' ./internal/controller
```

`VIRTRIGAUD_PROTOCOL_MODEL_NO_START_CHECK=1` turns invariant 2 off, to see whether the
other invariants alone notice a broken gate.

### Mutation

`hack/agent-mutation.sh` runs the model in every round, besides the agent and manager
tests. Each round switches off one safety guard. Switching off the ledger gate in
`Store.Execute` (guard `ledger-gate`) fails the model on its first runs (invariant 2).
It also fails the agent's ledger gate unit tests.

## Other evidence

- **Agent tests against real agent processes** (`TestAgentIdentity_*`, in
  `internal/controller`) run the real agent, Provider and transport. They cover these
  cases:
  - a receipt is ledgered;
  - a lost reply is decided from an operation answer;
  - an operation that was never delivered is retired;
  - a frozen enrolment is closed;
  - a replaced Host is rebound;
  - an etcd restore alone recovers by re-attestation, with exactly one effect;
  - a ledger mark lost across a manager restart is sent again.
- **Envtest specs** (`Host ledger (envtest)`) run the ledger library against a real
  API server. They cover:
  - every write and every hold;
  - tag and key rotation;
  - regression and partial-restore detection;
  - resourceVersion conflicts;
  - two managers' sessions both pending;
  - a delayed attestation adopted;
  - an unproven re-attestation that keeps the working session;
  - each rebind condition.
- **Scale.** `TestLedgerBuckets_TenThousandHosts` simulates 10,000 hosts over the real
  token buckets and the real per-host heartbeat throttle. Heartbeat raises never take
  a token from receipt raises or attestation, and every host gets a heartbeat raise
  within about five minutes.
- **Write cost.** Run `go test -run '^$' -bench BenchmarkHostLedger ./internal/controller`.
  It reports the time and the API calls (ledger writes and reads, pins writes) per
  receipt raise, per receipt raise in a batch of 64, per heartbeat raise, per ledger
  mark through a re-asserted ledger, and per uncached bound read.

## Not covered here

- The control-plane measurements against a real cluster: write rates and conflicts,
  API latency, etcd growth and KMS calls. They need a cluster.
- Ledger garbage collection by an operator command, which does not exist yet.
