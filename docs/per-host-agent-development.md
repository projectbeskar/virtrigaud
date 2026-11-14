# Per-host agent (development builds only)

`vrtg-agent` is the agent a clustered Provider with one agent per host
(`spec.type: cloudhypervisor`) will route to: one process per host, serving
that Provider over mutually authenticated TLS. This page describes the agent
as it is today: a process that starts, checks its host, opens and recovers
its operation store and answers reads with host-signed answers. A
**development build** also executes signed operations, at most once each,
through its development fake executor, and answers `TaskStatus` and
`Describe` from its store with signed, request-bound answers (see
[Operations](#operations)); a release build refuses every mutating call. It
creates no VM, scope, cgroup or process, and holds no Linux capability. It
does not hold its host private key either: a separate signer process, run as
its own user, does (see [Host key signer](#host-key-signer)).

Nothing here is part of a release. No release workflow, container image or
make target builds `vrtg-agent` (a test in `cmd/vrtg-agent` checks the
release workflow, the CI workflow and the `Makefile`). The Provider that
routes to it, also for development builds only, is described in
[`clustered-agent-provider-development.md`](clustered-agent-provider-development.md).

## Builds

| Build | Command | Executor | Development flags |
|-------|---------|----------|-------------------|
| Development | `go build -tags virtrigaud_dev -o bin/vrtg-agent ./cmd/vrtg-agent` | `none` (default) or `fake` | accepted |
| Release (no tag) | `go build -o bin/vrtg-agent ./cmd/vrtg-agent` | `none` only | `--executor=fake`, `--dev-trust-dir`, `--dev-enrolment-file` refuse the start (exit 2) |

The `none` executor implements no operation kind and performs nothing. The
`fake` executor (`internal/agent/executor/fake`) keeps small JSON files under
`<state dir>/fake-executor/` that stand in for VMs; it is compiled only with
the `virtrigaud_dev` tag, so a release binary does not contain it (a test
checks the release build's dependency list). Neither build links Kubernetes or
the VirtRigaud API packages.

## Subcommands and flags

| Command | What it does |
|---------|--------------|
| `vrtg-agent run` | Runs the agent until SIGTERM or SIGINT. |
| `vrtg-agent signer` | Runs the host key signer until SIGTERM or SIGINT: a separate process and user that holds the host private key and signs for the agent over a Unix socket (see [Host key signer](#host-key-signer)). The same in a release and a development build; it has no development-only setting. |
| `vrtg-agent shutdown-vms` | Stops every VM of the executor, only while the host is shutting down (see [Units](#systemd-units)). |
| `vrtg-agent enroll csr` / `enroll install` | Development builds only (a release build prints that it is not available and exits 1). The two steps of host enrolment: `csr`, run as the signer's user, creates the host key and writes the certificate signing request; `install`, run as the agent's user, checks and installs the issued certificate (see [Enrolment](#enrolment)). |
| `vrtg-agent version` | Prints the version. |

`run` and `shutdown-vms` take the same flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--state-dir` | `/var/lib/virtrigaud-agent` | State directory: clean absolute path. |
| `--listen-address` | `:9460` | Address served to the Provider. |
| `--drain-timeout` | `30s` | How long a stop waits for running calls. |
| `--scrub-interval` | `1h` | How often the operation store reads every record and verifies it against its live-state digest. Must be positive; the scrub cannot be switched off. A scrub that finds damage quarantines the store (`journal_corrupt`), and the service status shows it. |
| `--inventory-interval` | `60s` | How often the agent collects its host inventory, from `30s` to `600s` (see [Host inventory](#host-inventory)); a change in a manifest or extension directory also triggers a collection. |
| `--signer-socket` | `/run/virtrigaud-signer/signer.sock` | The signer's Unix socket: clean absolute path. The agent signs only through it. |
| `--signer-user` | `virtrigaud-signer` | The signer's user, a name or a uid. The agent talks to no other peer on the socket. |
| `--signer-key-file` | `/var/lib/virtrigaud-signer/host.key` | The host key file the signer reads. The agent never opens it; it checks that it cannot read it (see [Host checks](#host-checks)). |
| `--executor` | `none` | `none`, or `fake` in a development build. |
| `--dev-trust-dir` | (empty: `/usr/share/virtrigaud/trust`) | Development builds only: another trust configuration directory. The start-up checks still apply to it. |
| `--dev-enrolment-file` | (empty) | Development builds only: the development enrolment file. |

`shutdown-vms` also takes `--timeout` (default `5m`).

`signer` takes its own flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--socket` | `/run/virtrigaud-signer/signer.sock` | The Unix socket to serve: clean absolute path. Its directory must be owned by the signer's user and grant nothing to others and no write to its group. |
| `--key-file` | `/var/lib/virtrigaud-signer/host.key` | The host private key: a regular file (not a symbolic link) owned by the signer's user with no permission for group or others, ECDSA P-256, PEM. |
| `--agent-user` | `virtrigaud-agent` | The only user served, a name or a uid. It must not be the signer's own user. |
| `--max-signs-per-second` | `50` | The sustained rate limit of the answer budget (signed host answers). |
| `--sign-burst` | `100` | The answer budget's burst size. |
| `--max-handshake-signs-per-second` | `20` | The sustained rate limit of the handshake budget (TLS handshakes). |
| `--handshake-sign-burst` | `40` | The handshake budget's burst size. |
| `--log-interval` | `1m` | How often the signature counters are logged (only while they change). |

The signer refuses to run as root.

## Host layout

| Path | Owner, mode | Content |
|------|-------------|---------|
| `/var/lib/virtrigaud-agent/` | agent user, `0700` | State directory, on persistent block-device-backed storage. |
| `…/state.db`, `anchor.log`, `store.marker`, `state.db.lock`, `quarantine`, `witness/` | agent user | The operation store (journal, anchor log, markers, witness files). |
| `…/host-id` | agent user, `0600` | Provisioning marker: the host's URI identity, `spiffe://<trust domain>/virtrigaud/host/<namespace>/<host>`, written when the host is provisioned. |
| `…/pki/` | agent user, `0700` | `host.crt` only: the host identity certificate, PEM, the leaf only, `0600`. The host private key is **not** here, and the agent refuses to start when a file of that name is. |
| `/var/lib/virtrigaud-signer/` | signer user, `0700` | The signer's state directory. `host.key`: the host private key, ECDSA P-256, PEM, PKCS #8 or SEC 1, `0600`, owned by the signer's user. The agent's user cannot read it. Never put it in a backup. |
| `/run/virtrigaud-signer/` | signer user, group agent, `2750` | The socket directory; `signer.sock` (`0660`) takes the agent's group from the setgid directory. No access for others, and the agent's group cannot write the directory, so it cannot replace the socket. |
| `…/fake-executor/` | agent user, `0700` | Fake VMs (development builds with `--executor=fake`). |
| `/usr/share/virtrigaud/trust/` | root, not writable by the agent | Trust configuration, meant to come from the host's signed, read-only system image (see [Limits](#limits): today the agent checks only that it cannot write it). |

The trust configuration directory holds four files, all required:

| File | Content |
|------|---------|
| `host-ca.pem` | The CA certificate(s) that issue host identity certificates. |
| `provider-ca.pem` | The CA certificate(s) that issue Provider identity certificates. This issuer is a development stand-in until the Provider identity path is defined. |
| `grant-anchors.pem` | The operation grant anchors. |
| `provider-sans` | The Providers the agent serves, one per line: `spiffe://<trust domain>/virtrigaud/provider/<namespace>/<name>`. Blank lines and `#` comments are ignored. |

The grant anchors, the host identity CA and the Provider identity CA must be
three separate hierarchies: the agent refuses a trust configuration in which
any two of them share a certificate, a key or an issuer, or in which one
signed, or chains to, the other. Whoever holds the key of one of them must
not be able to issue a credential of another role (for example, the host
identity CA's key holder must not be able to issue a grant signer).

Every file of the trust configuration and of `pki/` (and the signer's key file) is opened without
following a symbolic link and checked on the open descriptor before it is
read: a regular file of at most 1 MiB, owned by root (trust configuration,
not writable by group or others) or by the agent's user (`pki/`, no
permission for group or others).

The host certificate must chain to `host-ca.pem`, certify the host key (the
public key the signer reports), carry
the server-authentication usage, not be a CA, be valid now, and carry exactly
these URI subject alternative names (see
[`host-agent-enrolment.md`](host-agent-enrolment.md#the-enrolment-record)):
the host's URI identity, `urn:virtrigaud:enrolment:<32 hex>`,
`urn:virtrigaud:provider:<Provider UID>`, `urn:virtrigaud:install:<32 hex>`,
`urn:virtrigaud:mode:identity`.

## Start-up order

| Step | What happens | On failure |
|------|--------------|------------|
| 0 | Host checks (below); the signer is reached and its public key fetched (if the signer is not there yet, the agent waits for it without a time limit, serving nothing (no listener, no store opened), and sends `READY=1` with `STATUS=waiting for the host key signer`, so that a signer that is down for minutes neither holds up the boot (`multi-user.target`) nor `systemctl start`, nor uses up the agent's start rate limit; here "ready" means started and waiting, and every call fails until the signer arrives; `READY=1` follows again, with the trust state, when start-up ends; a stop during the wait exits 0); then the trust configuration and the host identity are loaded, the host certificate must certify the signer's key, and the provisioning marker must name the host. | The start is refused (exit 1) with a reason; nothing on the host changes. |
| 1 | The store's single-writer lock. | A second agent on the same state directory exits. |
| 2 | The store opens with full fsync and checks its schema. | A store written for a newer agent refuses the start, naming the schema version it needs. |
| 3 | Local continuity checks: chain, anchor log, store marker, quarantine record. | The store is quarantined: reads still answer, every mutation is refused. |
| 4 | The clock floor record is read from the store and raised in memory from the host's own clocks. Nothing is written. | A missing floor for an enrolled store, or an unreadable one, quarantines the store (`inconsistent_store`); clocks the floor cannot use refuse the start. |
| 5 | The server starts serving, unattested. Nothing is committed before step 6. | |
| 6 | Continuity is established. The clock floor is committed as the first write. The store's active enrolment is brought to the one the host certificate carries: in a development build a store that was never enrolled is enrolled, and a store with another (or no) active enrolment is re-enrolled with the [freeze](#re-enrolment-freeze) (see [Enrolment](#enrolment)); a release build does neither. | A floor that cannot be committed quarantines the store (`io_error`). |
| 7 | Recovery of interrupted operations. A development build starts a runner for every operation that recovery left runnable, so that no operation stays running without one. | |
| 8 | systemd is notified `READY=1`; the agent's trust state is READY. A development build now runs signed operations. | |

A quarantined store also sends `READY=1`, with `STATUS=AGENT_STATE_QUARANTINED
(<reason>)`, so that systemd keeps the agent running to answer reads. A store
stays quarantined until the host is enrolled again.

### The clock floor

The clock floor is one record in the operation store, so it is covered by
the store's record trailers and its live-state digest, and rolling the floor
back is rolling the store back, which the anchor checks of step 3 detect
(`rolled_back` or `inconsistent_store`). A `clock.floor` file left in the
state directory by an earlier build is not read and not written.

The store must not commit before its continuity is established, so the floor
has two phases:

- Steps 4 to 5: the agent only reads the stored floor and raises it in memory
  from the wall clock and `CLOCK_BOOTTIME`. The in-memory floor starts from the
  stored one and only goes up, so nothing is ever verified against a lower
  floor than the store holds, whatever the store holds (including a rolled-back
  copy that step 6 then quarantines).
- Step 6: right after continuity is established, and before the enrolment or
  any recovery write, the raised floor is committed. Every later commit is of a
  floor at least that high, and the commit's own transaction reads the stored
  record and refuses a lower one. No window exists in which a lower floor is
  used or written.

After that the floor is committed again only when it has advanced by one
minute since the last commit, at most once per observe period (5 seconds)
even if the clock jumps, and once at drain. A crash loses at most an unsaved
advance of about one minute plus an observe period, never a committed value.
Within the same boot, the boot-time credit restores that advance after a
restart. After a power loss, or a crash followed by a reboot (another
`boot_id`), no credit is possible. A clean drain therefore commits the floor
marked as drained (and the first write of the next run clears the mark); when
the agent opens the store in a new boot and the last commit was not a clean
drain, it raises the stored floor by that bound (one minute plus the observe
period, 65 seconds), whatever the wall clock reads, but never beyond the wall
clock plus the floor's ahead bound (5 minutes) and never below the stored
floor. The raise lives in memory until the first commit, so it does not stack
across reboots that never commit; nothing is added after a clean drain. The
residual ratchet is repeated unclean fast reboots that each commit: each can
add up to 65 seconds, capped at the wall clock plus the ahead bound. The bound
against grant expiry: after a power loss an expired grant-signing key stays
expired unless the wall clock was rolled back below the lost floor by more
than the 65 second raise and the host then ran on that clock.

A quarantined store cannot commit; the floor then keeps running in memory (a
commit that fails because the store was quarantined stops persisting, it does
not fail the floor).

The caveat of every store-held value applies: a joint restore of the store and
its anchor log is not detected on the host (the manager-side check that a
host's reported chain position does not go backwards covers it), and that
restore brings back an older floor with it.

**Floor ahead of the clock.** If the floor is ahead of the wall clock by more
than 5 minutes, the floor is `ClockFloorAhead` (it is left only when the lead
has fallen 10 seconds below that bound, so a clock jittering at the edge does
not flip the state): grants are refused and the host
refuses every mutating call. Export the floor's ahead state as a metric and
alert on it: it is the primary control, because the state can have two very
different causes. A wall clock that was rolled back heals without a restart as
soon as it is corrected. A clock that jumped forward and was corrected leaves
the floor at the jump: it persists within a boot, and across restarts and
reboots, until the wall clock reaches it, so the host stays refused for the
length of the jump. The only ways out are to wait for the clock to reach the
floor, to power the host off for longer than the lead, or to enrol the host
again (which starts a new floor).

While the floor is ahead, failed or missing (and while a re-enrolment's freeze
is incomplete) the host reports `AGENT_STATE_UNATTESTED` with no state reason
(the signed answer's contract is unchanged), reads (`Hello`, `GetHostInfo`)
keep answering, and every mutating call is refused with the reserved
`UNATTESTED` reason and the cause in the `ErrorInfo` metadata
(`cause: clock_floor_ahead`, `clock_floor_unavailable` or `freeze_incomplete`)
and in the message. The cause is also in the service status
(`STATUS=AGENT_STATE_UNATTESTED (clock_floor_ahead)`, refreshed when it
changes) and in a log line when it appears or goes. The per-call check of the
Provider's certificate uses the later of the host clock and the floor only
while the floor is healthy.

### Re-enrolment freeze

Re-enrolling freezes every operation of the previous enrolment that is still
open (retired when it has no step intent, otherwise outcome unknown) before the
new enrolment becomes active. The freeze commits in chunks of at most 128
operations, so one transaction's size and latency do not depend on how many
operations were open, and one transaction reads at most 8 times that many
records (open or finished: a freeze scan over a predecessor with very many
finished operations advances its cursor over them in several transactions, and
may freeze none in some of them). The first chunk marks the old enrolment frozen (no
enrolment is active from then on, so the host refuses every operation) and
writes a freeze-in-progress marker that each chunk advances. A crash between
chunks leaves the marker: recovery finishes the freeze at the next start, or
the next re-enrolment does, before the new enrolment is activated. Every
operation is frozen once. A freeze that stops partway without a crash keeps
the host refusing every operation, and the host reports `AGENT_STATE_UNATTESTED`
(cause `freeze_incomplete`, as above). A marker that cannot be read quarantines the
store (`inconsistent_store`) and keeps the host refusing; reads still answer.

### Host checks

| Check | Refusal reason |
|-------|----------------|
| The agent does not run as root (effective uid 0). | `RunsAsRoot` |
| The agent runs in the host's initial PID, cgroup, user and time namespaces (a kernel without time namespaces passes that one). | `UnsupportedHost` |
| `/sys/fs/cgroup` is a unified cgroup v2 hierarchy. | `UnsupportedHost` |
| The state directory is a directory (not a symbolic link), owned by the agent's user, with no permission for group or others. | `StateDirInsecure` |
| The state directory is on ext4, ext3, XFS, Btrfs, F2FS (mounted from a `/dev/` device that is not a loop device) or ZFS; never tmpfs, ramfs, overlay, a loop device or a network file system. | `StateNotPersistent` |
| `host-id` exists, is a regular file owned by the agent's user that group and others cannot write, and names the host of the certificate. | `StateNotPersistent` |
| `pki/` and every file in it are owned by the agent's user, grant nothing to group or others, and are not symbolic links. | `PKIInsecure` |
| The agent is shown not to be able to read the host private key: an attempt to open `--signer-key-file` for reading (never read from) is refused by permission (`EACCES`). A file that opens, and also a missing path, a symbolic link or any other error, refuses the start, so a misspelled key path cannot pass for a protected one. `pki/` holds no file named `host.key` at all, readable or not. | `HostKeyReadable` |
| The signer's socket directory is a directory (not a symbolic link), owned by the signer's user, with nothing for others and no write permission for its group. | `SignerSocketInsecure` |
| Every directory from `/` to the trust directory, and every file in it, is owned by root, not writable by group or others, not a symbolic link; and the agent's process cannot write the directory or its files. | `TrustConfigInsecure` |
| The trust configuration and the host identity load, including the checks on the open files (see above), and the host certificate certifies the signer's key. | `IdentityInvalid` |
| The signer's user (`--signer-user`) is neither root nor the agent's own user, and the signer answers on its socket, run by that user (checked with `SO_PEERCRED`), with an ECDSA P-256 key. A signer that is not there yet is waited for (see [Start-up order](#start-up-order)); a signer of the wrong user or with an unusable key is refused at once: the agent exits 1, and `Restart=always` starts it again every 2 seconds until the unit's start limit is reached and it stays failed, which is intended (a configuration that waiting cannot cure fails closed and is visible, not retried for ever). | `SignerUnavailable` |

A release build also refuses to start when a caller of the agent's Go
packages sets one of the daemon's test overrides (another trust directory,
host probe, uid, listener, file owner, file system, clock sources, clock or
hooks); the agent binary never sets them.

## Peer authentication

The agent speaks TLS 1.3 only, with its host certificate; the signature of
each handshake is made by the signer process (see
[Host key signer](#host-key-signer)). A
connection is accepted only from a client certificate that chains to
`provider-ca.pem`, carries exactly one URI subject alternative name, and
whose name is `spiffe://<trust domain>/virtrigaud/provider/<namespace>/<name>/<Provider UID>`
with the trust domain, namespace and name in `provider-sans`. Anything else
fails the handshake. Every call then verifies the client certificate again,
against the current `provider-ca.pem` and the current time, so that a reload
that replaces the Provider identity CA, or a certificate that expires while
its connection is open, cuts the connection off (`PermissionDenied`). It then
compares the Provider UID with the Provider the host certificate is enrolled
with, and refuses another one with `FailedPrecondition` and the ErrorInfo
reason `PROVIDER_MISMATCH` before any other check.

Before the TLS handshake, a throttle limits what a peer that can reach the
port can cost. This matters because in TLS 1.3 the server signs its
CertificateVerify before it looks at the client certificate: every
ClientHello from any peer that can connect would otherwise cost the host key
one signature.

- A source address may open 2 new connections per second, with a burst of 10
  (IPv6 sources are counted per /64). The table of sources keeps the most
  recently seen ones (up to 8192) and forgets the others in constant time; a
  forgotten source starts again with a full bucket.
- A new connection must send its ClientHello within 2 seconds or it is closed;
  until then it holds nothing. When the ClientHello arrives, the connection
  takes a handshake slot, before anything is signed: at most 16 handshakes run
  at once and at most 2 from one source. After the hello the client has 2.5
  seconds for each next flight (its certificate and Finished) or the
  connection is closed. The slot is given back when the handshake ends. A source that opens connections and stays silent therefore
  cannot use the slots up, and one source cannot hold more than 2 of them.
- A refused connection or handshake never reaches the signer. Refusals are
  logged once per 30 seconds with the count held back, as is the "no signed
  answer" error.
- Signed answers are limited per Provider identity (the verified client
  certificate's URI): 10 per second, burst 20; a read above that is answered
  without a signed answer, and the signer is not asked.

This bounds what a peer that can connect costs; it authenticates nobody, and
the signer's handshake budget (below) is shared with every peer that gets
through it, so a determined set of sources can still use that budget up and
make the Provider's reconnects fail until they stop. **Only the Provider's
pods may reach the agent's port.** A host firewall on the agent's address is
not enough when the Provider runs in a cluster: traffic from other pods on
the Provider's node can leave that node with the node's address, which is
inside the firewall. A Kubernetes NetworkPolicy (or an equivalent control)
that lets only the Provider's pods reach the agent's address and port is a
requirement, in addition to firewalling the port to the Provider's node
address ranges. A pod NetworkPolicy does not cover pods that use the node's
network (`hostNetwork`) or traffic from the node itself, so the host firewall
is still what restricts those.

Connections are closed after 30 minutes (and a minute of grace for running
calls), so each is authenticated again with a new handshake. A handshake must
finish within 10 seconds, a connection carries at most 64 concurrent calls,
and a client may not send keepalive pings more often than every 30 seconds.

## What the agent serves

| Method | Answer |
|--------|--------|
| `provider.v1.Provider/GetHostInfo` | `HostInfo` with `id` (the Host name) and `health` (`READY` once the agent is ready). With `answer_nonce`, a signed `HostAnswer` in `host_answer`; with `inventory_version` of at least 1 as well, that answer carries the host inventory (see [Host inventory](#host-inventory)). `host_id` naming another host, a nonce that is not 16 bytes, `continuity_seq` without a nonce, or `inventory_version` without a nonce: `InvalidArgument`. |
| `agent.v1.AgentService/Hello` | A signed `HostAnswer`. The nonce (16 bytes) and `caller_api_version` (at least 1) are required. |
| `agent.v1.AgentService/AgentStatus` | A signed `HostAnswer`. The nonce is required. |
| `agent.v1.AgentService/InstallRenewedCertificate` | `Unimplemented`. |
| Every mutating Provider method (the operation kind table) | `Unavailable` with `UNATTESTED` before the agent is ready; `FailedPrecondition` with `QUARANTINED` while the store is quarantined. Once ready: a release build answers `Unimplemented`; a development build runs the operation path (see [Operations](#operations)). |
| `provider.v1.Provider/TaskStatus` | Development builds: the operation's state from its record or tombstone, by task reference, or by `op_id` with a signed `OperationAnswer` (see [Operations](#operations)). Release builds: `Unimplemented`. |
| `provider.v1.Provider/Describe` | Development builds: whether the owner's VM has a placement on the host, and with `answer_nonce` a signed `VMAnswer` about one placement (see [Operations](#operations)). Release builds: `Unimplemented`. |
| Every other Provider method | `Unimplemented`. |

A signed answer is made only for a request with a nonce, and only while the
store's active enrolment is the one the host certificate names; otherwise the
read answers without `host_answer`. Its body carries the nonce, the enrolment,
the store's chain position, newest checkpoint and trust state (read together),
the agent's API version (2), grant versions (`[1]`), mode (`identity`), store
schema version and clock floor. It is signed with the host key, by the signer process, over
`"virtrigaud.host-signed.v1" 0x00 ‖ be16(len(body_type)) ‖ body_type ‖ be32(len(body)) ‖ body`
(ECDSA P-256, SHA-256, low-S), as defined in
[`operation-grants-wire-shape.md`](operation-grants-wire-shape.md#the-signature-input); a test
reproduces the published test vector.

## Operations

A development build (`-tags virtrigaud_dev`) executes signed operations of
the seven kinds a manager marks with operation identity (Create, Delete, the
four power kinds and Reconfigure) with the executor it is started with: the
`fake` executor performs them on its stand-in VMs; the `none` executor
implements no kind, so every such call is refused `Unimplemented` after its
grant is verified. A release build has no operation path at all.

**Check order of a mutating call.** Each call is checked in this order; the
first check that fails answers it, and nothing after it runs:

1. the caller is the Provider the host is enrolled with (mutual TLS,
   `PROVIDER_MISMATCH` otherwise);
2. the store is ready: not quarantined (`QUARANTINED`), continuity
   established, recovery run, no incomplete re-enrolment freeze and a
   healthy clock floor (`UNATTESTED`, with the cause in the `ErrorInfo`
   metadata). The floor is read again, once, for the call: a floor that
   stopped being healthy since the gate refuses the call the same way, never
   with a definite reason;
3. the grant, with the shared verifier, at that one reading of the floor: an envelope (`IDENTITY_REQUIRED`
   without one), no other field of the outer request (`INVALID_ARGUMENT`),
   the signature and certificate chain to a pinned grant anchor, the
   certificate and grant validity judged at the later of the host clock and
   the clock floor (`GRANT_UNAUTHENTICATED`), the body rules, the kind
   against the method and request type, the request decoded with no
   unknown field, the intent digest, and the installation and host
   (`GRANT_MISMATCH`);
4. the store is read for the `op_id`. A known `op_id` (record or tombstone)
   goes straight to the store, which answers it from its record: the same
   identity fields give the same task reference and nothing runs again; other
   fields give `OP_ID_CONFLICT`. Only an `op_id` the store does not know is
   checked further:
   - the grant's VM and Provider UIDs have the shape of a UID, and the
     decoded request names this host and, as its owner, the grant's VM
     (`GRANT_MISMATCH`);
   - a kind the executor does not implement: `Unimplemented`;
   - the store's active enrolment is the one the host certificate names
     (`UNATTESTED`, cause `enrolment_mismatch`, never definite);
5. the operation store decides, under its lock. The grant's Provider and
   enrolment are compared with the active enrolment: another Provider is `GRANT_MISMATCH`; an enrolment the store
   holds no record of is `UNKNOWN_TO_AGENT`, never a definite refusal,
   because it may be a predecessor whose records were lost. Then the
   first-delivery rule for `mint_epoch` (`GRANT_MISMATCH`), the VM's other
   open operation (`VM_BUSY`), the placement binding (`VM_BINDING_STALE`,
   `VM_NOT_OWNED`), an attempt that does not increase for the VM and kind
   (`OP_ID_CONFLICT`) and a full store (`RESOURCE_EXHAUSTED`);
6. an accepted operation is answered with its task reference
   (`op-<enrolment id><op_id>` in hex) once its record is durable and
   anchored, and a runner drives its steps.

Every decision is logged as one structured line (`operation decision` and
`operation event`) with identity fields only: the decision, the reason, the
method, `op_id`, enrolment, VM, kind, attempt and the caller's identity.
Request bytes, cloud-init user data and grant bytes are never written to the
store, the anchor log, the logs, an error or an answer.

**Runners and step deadlines.** One runner per operation drives its steps
through the store's step journal: the intent is durable before the effect, a
once-only step's witness file before its effect, and the terminal state, its
tombstone and, for a completed Delete, the placement's VM tombstone in one
transaction. Every step has a deadline (10 minutes per kind by default); a
step that has not ended by then is resolved by inspection: its effect found
is done, its definite absence a failure (dirty after an irreversible step),
no evidence an unknown outcome, so a step that never returns ends the
operation instead of leaving it running. A drain stops runners between
steps; the next start's recovery resumes them. A run that fails on a store
error that neither quarantines nor stops it is resumed with a bounded
backoff for at least the longest step deadline; only then is its outcome
recorded unknown (`run_failed`). A run that panics is counted durably, and
the agent stops so that its next start's recovery resolves the step; after
three panicked runs the operation is not run again (`run_panicked`).

**TaskStatus.** A request with a task reference is answered from the
operation's record or tombstone: running while it is accepted or executing,
then its typed outcome (`SUCCEEDED`, `FAILED_CLEAN`, `FAILED_DIRTY`,
`UNKNOWN`, `RETIRED`). A request with `answer_nonce` names the operation by
`op_id`, `op_enrolment_id` and `target_host_id` (this host; no task) and is
answered with a signed `OperationAnswer` made from the same store read as
the unsigned fields: the operation's state number for number, its kind,
attempt, VM, binding and intent digest, `tombstoned` when it is over. An
operation the store holds no record of is `NotFound` with `UNKNOWN_TO_AGENT`,
never done, with the signed answer (`NO_RECORD` when the store holds the
enrolment, `UNKNOWN_TO_AGENT` when it does not) as a status detail.

**Outcome reasons.** An operation's `outcome_reason` (in `TaskStatus` and in
the signed `OperationAnswer`) names why it ended as it did. None is ever a
secret, a path or an address.

| Reason | State | Means |
|--------|-------|-------|
| `not_performed` | `FAILED_CLEAN`, or `FAILED_DIRTY` after an irreversible step | The executor reported a step definitely not performed. |
| `step_deadline` | `FAILED_CLEAN`, `FAILED_DIRTY` or `OUTCOME_UNKNOWN` | A step ran past its deadline; inspection found its effect absent (a failure) or could not decide (unknown). |
| `effect_error` | as `step_deadline` | A step failed in a way that left its effect in doubt, and inspection decided as above. |
| `witness_no_effect` | `OUTCOME_UNKNOWN` | A once-only step's witness exists and inspection did not find its effect: it is never run again. |
| `goal_not_observed` | `FAILED_DIRTY` or `OUTCOME_UNKNOWN` | An irreversible step's intent was durable and its goal state was not observed after a restart. |
| `probe_error` | `OUTCOME_UNKNOWN` | An inspection or goal check failed. |
| `inconsistent_store` | `OUTCOME_UNKNOWN` | A witness exists without its step intent; the store is quarantined. |
| `provider_changed` | `RETIRED` or `OUTCOME_UNKNOWN` | A re-enrolment froze the operation. |
| `run_failed` | `OUTCOME_UNKNOWN` | Its runs failed on store errors for longer than the longest step deadline. |
| `run_panicked` | `OUTCOME_UNKNOWN` | Its runs panicked three times, each stopping the agent; it is not run again. |

An `OUTCOME_UNKNOWN` operation keeps its VM busy. Clearing it, once an operator
has established what happened, is an operator path that a later change adds.

**Describe.** The owner's UID is required. Without a nonce, the VM exists
while the store records a placement of it, and the executor reports its power
state. With `answer_nonce` and `binding_id`, the signed `VMAnswer` says
`DELETED`, naming the Delete, only when that placement's Delete completed (its
VM tombstone); otherwise `CREATING`, `PRESENT`, `OTHER_BINDING` or
`NO_RECORD`. The absence of a record never says deleted.

**Crash hook.** A development build reads `VRTG_AGENT_DEV_CRASH_AT`, a
comma-separated sequence of crash points of the journal and the operation
store (for example `before-accept,after-store-commit`), and kills its own
process with SIGKILL when the last point is reached after the earlier ones;
`VRTG_AGENT_DEV_ANCHOR_ROTATE_BYTES` shrinks the anchor log's rotation size.
They exist for the crash-point tests (`test/conformance/clustered/agenttarget`);
a release build ignores both.

## Host inventory

The agent reports what it observes of the components the host runs virtual
machines with, in the signed `HostAnswer` of `GetHostInfo`
(`HostAnswer.inventory`; the field rules and the closed value sets are in
[`operation-grants-wire-shape.md`](operation-grants-wire-shape.md#host-inventory)).
It is a **detective control**: signed so that the Provider cannot alter,
replay or downgrade it, but a host whose agent is compromised can report
anything.

### When it is sent

Only `GetHostInfo`, only when the request carries `answer_nonce` and an
`inventory_version` of at least 1. `agent.v1` answers (`Hello`,
`AgentStatus`) and reads without a nonce never carry it. The answer echoes the
requested version in `requested_version`, states the highest version the agent
implements in `supported_version` (1) and uses the lower of the two as
`inventory_version`; a request for version 0 gets no inventory. An inventory
that breaks a field rule is never signed: the agent answers without it (still a
valid signed answer, for every other decision), or, when only its size is the
problem, with an `OVER_LIMIT` inventory that lists nothing.

### Collection

A background collector builds one immutable snapshot at start, every
`--inventory-interval`, and after a change in a manifest or extension
directory once it has been quiet for 5 seconds (at least 30 seconds after the
previous collection began). A collection that takes longer than 120 seconds is
abandoned and the snapshot is `UNREADABLE` with `probe_failed`; no second
collection starts while the abandoned one still holds the file system. Before
the first collection has finished the inventory is `PENDING`. Answering reads
the latest snapshot only: no file access, hashing or probing happens per
request. `snapshot_age_seconds` is the time since the collection finished on
`CLOCK_BOOTTIME`, which does not jump and counts suspended time.

### Component manifests

A component is declared by a JSON manifest, a file whose name ends in `.json`,
in one of two directories:

| Directory | Origin reported | Notes |
|-----------|-----------------|-------|
| `/usr/share/virtrigaud/host-components.d/` | `READ_ONLY` | Provided with the host image. |
| `/etc/virtrigaud/host-components.d/` | `WRITABLE` | Host configuration. |

```json
{
  "schemaVersion": 1,
  "component": "cloud-hypervisor",
  "version": "1.2.3",
  "source": "sysext",
  "artifact": {"name": "cloud-hypervisor.raw", "sha256": "sha256:<64 hex digits>"},
  "files": {"cloud-hypervisor": "sha256:<64 hex digits>"}
}
```

`schemaVersion`, `component` and `version` are required. `source` says how the
component was delivered (a short lower-case word such as `sysext` or
`package`). `artifact` names the system extension (a name in an extension
directory, never a path) and may declare its digest; `files` may declare the
digest of each file the agent hashes. Digests are written `sha256:` and 64
lower-case hex digits; any other form makes the manifest invalid
(`unsupported_digest`). Unknown fields, trailing data, a repeated key, a key whose case differs from
the field's, invalid UTF-8, nesting deeper than a manifest needs, a file larger
than 16 KiB or one that is not a regular file make the manifest invalid. A declared digest
is checked against the digest the agent computed; it is never copied into the
report.

A component declared in both directories, or twice in one, is
`MANIFEST_INVALID` with `duplicate_declaration`, reported with the read-only
origin when the image declares it: a manifest in the writable directory cannot
silently override the image's. It can still **add** a declaration the image
does not make, so the writable directory must be as protected as the
component files; a manager can accept only `READ_ONLY` manifests. A manifest
that names no component of the agent's catalog is reported under the name
`manifest-` and the first 16 hex digits of the SHA-256 of its file name. That
hides the characters of the name, not its content: a short, guessable file
name can be recovered by brute force, so file names must not carry anything
sensitive.

### What is collected

The agent's built-in catalog names the components it knows, the files it
executes or loads from each (hashed on every collection), and the version range
it requires. The catalog's paths are the `/usr/bin` ones listed below: a
component installed elsewhere is `MISSING` (or, without a manifest, not seen)
until the catalog learns its path.

| Component | Required | Files | Version read from |
|-----------|----------|-------|-------------------|
| `cloud-hypervisor` | yes | `cloud-hypervisor`, optional `ch-remote` | the extension's release file, then the executable's packaging note, then the package database |
| `guest-kernel` | no | `vmlinux` | the same sources |
| `hypervisor-firmware` | no | `hypervisor-fw` | the same sources |
| `virtrigaud-agent` | yes | `vrtg-agent` (the running image, read through `/proc/self/exe`) | the agent's own build |

Each reported component has one state, the first that applies:
`MANIFEST_INVALID`, `MISSING` (declared but not installed, or required and
neither declared nor present: `not_installed`, `not_declared`),
`INCOMPATIBLE` (`version_out_of_range`; `version_unreadable` when no source gave a
version or the version has no numeric part, such as `dev`), `UNHEALTHY`
(`unexpected_extension`, `artifact_digest_mismatch`, `file_digest_mismatch`,
`digest_limit_exceeded`), `UNMANIFESTED` (present, no manifest declares it),
else `READY`. A component without a manifest is `UNMANIFESTED` when present and
`MISSING` when required and absent; its version and digests are still reported.
The reasons `socket_unanswered`, `self_test_failed` and `not_content_addressed`
are in the closed set but not produced yet.

`artifact_sha256` is the digest of the delivered system extension image;
a directory extension has none. `files` are listed by name with the digest the
agent computed. Hashing reads at most 1 GiB of a file and 4 GiB per
collection; a file above a limit makes its component `UNHEALTHY`
(`digest_limit_exceeded`).

**Unexpected extensions.** Every entry of the system extension directories
(`/etc/extensions`, `/run/extensions`, `/var/lib/extensions`,
`/usr/lib/extensions`, `/usr/local/lib/extensions`, in that order; the first
directory that holds a name wins, and `x` and `x.raw` are one name, as for the
system extension tool) that no manifest declares is listed. A `.raw` regular
file (or a link to one) is an undeclared image with its digest. A link to
`/dev/null` masks an extension and is not listed. Everything else is listed as
a directory extension, without a digest, so that nothing unusual in these
directories is hidden: a directory, a dangling link, a device node, a FIFO, a
versioned (`.v`) directory, a file without the `.raw` suffix, and an image
that cannot be hashed (over the limit, unreadable). Two entries of one directory that name the
same extension (`x` and `x.raw`) make the report `UNREADABLE`
(`probe_failed`): which one the tool takes is not defined, and neither may be
dropped. Entries whose name starts with a dot are skipped: the tool's image
discovery ignores them, so they are never merged (stated from the tool's
source, not checked against a local manual). One such entry never makes
the whole report an error; the manager sees an unexpected extension and keeps
the host out of service. Entries are listed whether or not the extension is
merged. A directory extension that holds a catalog file of a component, and
that the component does not declare itself, makes that component `UNHEALTHY`
(`unexpected_extension`); an image that overlays a component's files is not
detected. The search order follows the current system extension manual; the
older manual pages list only the first three directories.

**Feature probes.** `cgroup_v2`, `clone_into_cgroup`, `pidfd`, `pidfs_handle`,
`pidfd_info_exit`, `initial_pid_namespace`, `initial_cgroup_namespace`,
`initial_time_namespace`, `hypervisor_pause_resume`, `secure_boot`,
`measured_boot`, `extension_image_policy_signed`, `kvm` and `tun`, each
`PRESENT`, `ABSENT` or `ERROR` with a reason (`probe_failed`, `probe_timeout`,
`permission_denied`, `not_evaluated`). They are data reads and calls on the
agent's own descriptors: `pidfd_open`; a file handle taken from its own process
descriptor and opened again (`name_to_handle_at`, `open_by_handle_at`), which
must give the same process; `statfs` of the cgroup root; the namespace links;
whether the host exposes `/dev/kvm` and `/dev/net/tun`, read from sysfs
(`/sys/class/misc/kvm/dev` must read `10:232` and `/sys/class/misc/tun/dev`
`10:200`); the agent never opens either device, and the sysfs files stay
readable when the service hides device nodes. A `tun` module that is not loaded
yet (it is often loaded on first use) has registered nothing and reads
`ABSENT`; and the firmware's Secure Boot variable. A missing Secure
Boot variable means `ABSENT` only when the host has no EFI firmware, or
efivarfs is mounted and does not hold it; EFI firmware with efivarfs not
mounted is `ERROR`. `measured_boot` says whether a TPM firmware event log is
present, and is `ERROR` unless securityfs is mounted. `clone_into_cgroup` and
`pidfd_info_exit` are inferred from the kernel release: they cannot see a
feature a distribution backported, so they err towards `ABSENT`. Two probes
cannot be decided without running something and are reported `ERROR` with
`not_evaluated`: `hypervisor_pause_resume` (it needs a self-test of the
hypervisor in a sandbox) and `extension_image_policy_signed` (the policy is not
in a file the agent can read).

**Process identity tier.** The strongest mechanism this host supports for
identifying a process the agent starts, as the probes show: `PIDFS_HANDLE` when
a file handle of a process descriptor opens the same process again,
`PIDFS_INODE` when the agent's own process descriptor lives on the process file
system, else `START_TIME`. The service manager's descriptor store is not used yet, so
`PIDFD_STORE` is not reported.

### Bounds

The report is never truncated. When an observation would exceed a bound, the
inventory is `OVER_LIMIT` and lists nothing, with a reason: more than 32
components or 64 entries in a manifest directory (`too_many_components`), more
than 8 files in a component or 64 in all (`too_many_files`), more than 32
features (`too_many_features`), more than 16 unexpected extensions or 256
entries in an extension directory (`too_many_extensions`), or more than 32 KiB
encoded (`inventory_too_large`). A manifest directory that cannot be read, a
manifest or component file that cannot be read, or a collection that cannot
finish makes it `UNREADABLE` (`manifest_dir_unreadable`, `io_error`,
`probe_failed`). Every reason is a member of a closed set; no error text, path,
file content or environment value is ever copied into the report.

### What the collector never does

It never runs a program found on the host: no component binary with a version
flag, no package tool, no self-test. Versions are read as data: an
`extension-release` file (`SYSEXT_VERSION_ID`, else `SYSEXT_IMAGE_VERSION`;
the unprefixed `VERSION_ID` is the host release the extension fits and is never
used), the packaging metadata note of an ELF file (only the note section, at
most 64 KiB; the ELF header and section table are read to find it), or a Debian
or Alpine package database (a line longer than 64 KiB is skipped; a read error or a database over 32 MiB gives no version, never a half-read answer). A package
database in a binary format is not read. Every parser is Go code with a bounded
input and is fuzzed; the collector links no C code. Files are looked at before
they are opened, and a device node or FIFO is never opened. Logs carry a class
(permission, not found, an errno number), never an error's text, a path or a
file name. A test checks that the collector's sources import
nothing that starts a process.

### Limits

- A static binary, and firmware or a kernel image, usually carry no packaging
  note and no package entry, so their version reads `version_unreadable` (the
  component is `INCOMPATIBLE`) until the version a manifest declares can be
  attested. The catalog's paths are the `/usr/bin` ones above.
- The collector runs inside the agent process, which also holds the signer's
  interface. A compromised agent can report any inventory, and a hostile
  manifest or component file reaches the agent's own parsers. Moving the
  collector into a separate unprivileged process is required before the
  regulated profile.
- Digests are computed when the collector runs, not when a component starts:
  a file can change between collection and use. The inventory detects drift at
  the next collection; it does not prevent it.
- The agent trusts manifests delivered by the same provisioning path as the
  components. A manager's policy of accepted origins, and an allow-list of
  digests, are the counters; neither is in the agent.

## Host key signer

The host private key lives in a separate process, `vrtg-agent signer`, that
runs as its own user (`virtrigaud-signer`) from its own unit. The agent, which
terminates TLS and parses everything a Provider sends, holds a
`crypto.Signer` (`signer.Client`) whose `Public()` is the key the signer
reports, which must be the host certificate's key, and whose `Sign` sends the
digest to the signer. The agent never reads or holds the private key bytes
(the host check `HostKeyReadable` refuses a host where it could), so a
compromise of the agent process cannot copy the key out, and cannot use it
after that process is gone.

**What it does not do.** The signer is a *digest oracle*. Go's `crypto/tls`
hands `Sign` a digest, not the message, so the signer cannot tell a TLS 1.3
handshake transcript from a host answer, or from any other 32 bytes. A
compromised agent process can therefore still obtain a signature over any
digest, online, for as long as it can reach the signer, and so can still make
host answers that verify. The separation prevents exfiltration of the key and
its later, offline use; it does not prevent online forgery by the TLS-facing
process. A separate answer key, held by a process that never terminates TLS
and so sees the whole message it signs, is a later requirement for the
regulated profile; until it exists, a signed host answer is no stronger than
the agent process that relays it.

**Protocol** (package `internal/agent/signer`). One request and one response
per connection, each a frame of a 2-byte big-endian length (at most 256 bytes;
a longer announcement closes the connection unread) and that many payload
bytes. A request is the version byte (1), an operation byte and its body:

| Operation | Body | Response body |
|-----------|------|---------------|
| `1` public key | none | the public key, PKIX DER |
| `2` sign | hash byte `1` (SHA-256), purpose byte (`1` handshake, `2` answer), then exactly 32 digest bytes | the signature, ASN.1 DER |

A response is the version byte, a status byte (`0` ok, `1` bad request,
`2` rate limited, `3` internal error) and the body. The only hash is SHA-256
and the only digest size 32 bytes, which is what `crypto/tls` signs for an
ECDSA P-256 key in TLS 1.3 and what the host answer signing uses; any other
operation, hash, size or version is refused. The agent also refuses locally
any `Sign` whose options are not `crypto.SHA256` or whose digest is not 32
bytes, and verifies every signature it gets back against the public key before
using it, so a wrong or stale signer yields an error, never a bad signature.

**Authentication.** Both ends read the peer's uid with `SO_PEERCRED` on every
connection: the signer serves only `--agent-user`'s uid and closes any other
connection without a response; the agent talks only to `--signer-user`'s uid.
The socket is in a directory that only the signer's user and the agent's
group can enter, and the signer refuses to start when that directory is
wrong. There is no network socket: the signer's unit has no network namespace
and allows `AF_UNIX` only.

**Bounds.** At most 32 connections are served at once (more are closed at
once); each has a 5-second deadline for the whole exchange; every client call
has a 5-second deadline for connect, request and response. The client opens a
connection per call, pools nothing and starts no goroutine; the signer starts
one goroutine per connection, bounded by the connection limit, plus one for
its log, and all end with the process's context.

**Restart and failure.** Because each call connects afresh, a signer that
restarts is reached again by the next call with no action by the agent. While
the signer is down or refuses, every `Sign` fails: new TLS handshakes fail,
and a read with a nonce is answered without `host_answer` (no answer, which a
manager treats as no evidence). The agent never signs for itself. At start-up
the agent waits for the signer without a time limit (see
[Start-up order](#start-up-order)); a signer of the wrong user or with an
unusable key is refused at once (`SignerUnavailable`). SIGHUP on the agent
fetches the signer's key again (a failed fetch keeps the running
credentials); SIGHUP on the signer loads a replaced key file (a key that does
not load keeps the old one), which is how a renewed certificate with a new
key is brought in: replace the key file, signal the signer, replace the
certificate, signal the agent. Between the signer's SIGHUP and the agent's
there is a brief window in which the signer holds the new key and the agent
still has the old certificate and public key: every signature the agent asks
for fails verification, so TLS handshakes and signed answers fail closed
until the agent's SIGHUP loads the new certificate. Do the two signals one
after the other.

**Memory.** The signer overwrites the buffer the key file was read into and
the decoded key bytes right after parsing, and its unit forbids core dumps and
swap (`MemorySwapMax=0` does not cover hibernation: a host that hibernates
writes all memory, the key included, to disk). Go gives no zeroization
guarantee for the parsed key it keeps, nor for the old key after a reload (it
stays in memory until the garbage collector frees it), nor for copies made
by `crypto/x509`, `encoding/asn1` and the runtime, so none is claimed.

**Installing.** On a development host, create the users and directories before
starting any unit: `systemd-sysusers` and `systemd-tmpfiles --create` with the
fragments in `packaging/systemd/` (the socket directory is
`/run/virtrigaud-signer`, owned by the signer's user, group the agent's,
`2750`, and is not made by the signer unit). Put `host.key` in
`/var/lib/virtrigaud-signer/` (`0600`, owned by the signer's user) and only
`host.crt` in the agent's `pki/`. The `vrtg-agent` binary, which both units
run, must be owned by root and writable by no one else (`root:root`, `0755`),
and so must the directories above it: the agent's user must not be able to
replace the program that holds the key.

**What the separation assumes.** The claim that a compromise of the agent
process cannot copy the key out assumes that the compromise does not also
reach root, or the signer's own uid: a process with either can read the
signer's memory or its key file.

**Rate limits and logging.** The signer limits signatures with two token
buckets, one per purpose: 20 per second (burst 40) for TLS handshakes and
50 per second (burst 100) for signed answers (flags above). The agent's TLS
handshakes use the first and its host answers the second, so a flood of
handshakes, which any peer that reaches the agent's port can start, cannot
starve the answers a Provider asks for. The purpose is declared by the agent
and taken on trust, since the signer cannot see what is signed: a compromised
agent can lie about it, so the split protects against outside floods, not
against that. A limit cannot stop a compromised agent process from asking;
it bounds how fast, keeps a stolen oracle from working as a bulk signing
service, caps the signer's CPU and shows an abuse as a rate in the log. The
agent needs about one signature per TLS handshake and per signed answer, a few
per second at most on a host with a few Providers, so the defaults leave a
wide margin (and the agent's pre-handshake throttle keeps unauthenticated
connections from using the handshake budget up in the first place). The signer logs, at most once per
`--log-interval` and only while something changed, the counts of signatures,
rate-limited requests, malformed requests, refused peers and refused
connections, and its totals when it stops; it never logs a digest, a
signature or key material.

## Signals

| Signal | Effect |
|--------|--------|
| SIGTERM, SIGINT | `STOPPING=1`; the server stops accepting calls and waits up to `--drain-timeout`; the clock floor is saved; the store is closed; exit 0. During recovery, recovery is cancelled and resumes at the next start. |
| SIGHUP | `RELOADING=1`; the key material and trust configuration are checked (the `pki/`, host key and trust checks above), the signer is asked for its public key again and the host certificate is loaded again. A reload that fails, or whose certificate names another host, is refused and the running credentials stay. In a development build, a certificate that names a new enrolment of this host re-enrols the store first (with the freeze) and then replaces the credentials (a release build refuses it as `EnrolmentChanged`); a new enrolment id the store has used before is refused (`ReenrolmentRefused`) and the running credentials stay. New handshakes and every later call use the new credentials. `READY=1` follows, with a status that starts with the trust state, then the result: for example `STATUS=AGENT_STATE_QUARANTINED (rolled_back); credentials reloaded` or `STATUS=AGENT_STATE_READY; reload refused: <reason>` (`IdentityInvalid`, `EnrolmentChanged`, `ReenrolmentRefused`, or a host-check reason such as `PKIInsecure`; a signer that cannot be reached is `IdentityInvalid` with the signer named in the log), never a path or a secret. |

## systemd units

`packaging/systemd/` holds reference units and `sysusers.d` / `tmpfiles.d`
fragments; they are not installed by anything.

`vrtg-agent.service`: `Type=notify`, `Restart=always`, `RestartSec=2`,
`StartLimitIntervalSec=300`, `StartLimitBurst=10`, `KillMode=process`,
`User=virtrigaud-agent`, `StateDirectory=virtrigaud-agent` (`0700`),
`RestartPreventExitStatus=2` (a configuration error is not retried),
`NoNewPrivileges=yes`, an empty capability bounding set, `ProtectSystem=strict`,
`ProtectProc=invisible`, `PrivateIPC=yes`, `LimitNOFILE=4096`, `TasksMax=128`,
`MemoryMax=256M` and further sandboxing. It sets no `Delegate=` and creates no
scope. No other unit upholds it, so it can be stopped for maintenance.

`vrtg-agent-signer.service`: the host key signer. `Type=notify` (`READY=1`
once it serves), `Restart=always`, the same start-rate limit and sandboxing as
the agent, and its own user and group (`virtrigaud-signer`) and, beyond the
agent's, a private network namespace with `RestrictAddressFamilies=AF_UNIX`,
a `MemoryMax` of `64M`, `MemorySwapMax=0` and `LimitCORE=0` (the key is never
never dumped, and swapped out neither, hibernation aside), its state directory (`/var/lib/virtrigaud-signer`,
`0700`, made by `tmpfiles.d`) read-only (`ReadOnlyPaths=`; the signer never
writes there, so the unit has no `StateDirectory=`) and read-write access to
`/run/virtrigaud-signer` only. It sets no
`PrivateUsers=`, which would hide the agent's uid from `SO_PEERCRED`.
`ExecReload` sends SIGHUP (reload the key file). The `sysusers.d` fragment
declares both users, and the `tmpfiles.d` fragment the signer's state
directory and the socket directory.

The agent's unit has `Wants=` and `After=vrtg-agent-signer.service`: starting
the agent starts the signer first, and as the signer is `Type=notify`,
`After=` waits until it serves. If the signer is still not up, the agent waits
(serving nothing, with `READY=1` and a status line, so that the boot and
`systemctl start` are not held up) instead of exiting, so it recovers when the signer
does, with no `systemctl reset-failed`. It is `Wants=`, not `Requires=`, on purpose.
With `Requires=`, every restart of the signer (a crash, or a key reload done
as a restart) would restart the agent too and take down its store and its
Provider connections, while the agent already fails closed on its own while the
signer is away (see [Host key signer](#host-key-signer)) and signs again when
the signer returns. A test asserts that the agent's unit does not bind to,
require or part-of the signer.

`vrtg-vm-shutdown.service`: a `oneshot` unit with `RemainAfterExit=yes` whose
stop action is `vrtg-agent shutdown-vms`. That command stops the VMs only
when `/usr/bin/systemctl is-system-running` reports `stopping` or a start job
of `shutdown.target`, `reboot.target`, `soft-reboot.target`, `halt.target`,
`poweroff.target` or `kexec.target` is queued; a maintenance stop of the unit
stops nothing. When it cannot read the host's state it stops nothing either,
and exits 1. The unit never wants, requires or binds to `vrtg-agent.service`,
so starting it never starts an agent stopped for maintenance; it is not
ordered against the agent's unit either (`After=` would only order the two,
never pull the agent in). That ordering is decided when the agent runs real
VMs.

`hack/verify-systemd-units.sh` runs `systemd-analyze verify` on every unit
(with the binary path replaced, since it does not exist on a build machine);
the CI lint job runs it.

## Enrolment

Enrolment gives the host its certificate and the agent its enrolment record.
**This is for development builds only** (`virtrigaud_dev` build tag): until the
manager's re-enrolment flow lands, a release build has no `enroll` steps (it
exits 1), derives no enrolment from the host certificate, never re-enrols (a
reload with another enrolment is refused as `EnrolmentChanged`), and leaves a
store that was never enrolled unenrolled, so its answers stay unsigned.

Enrolment needs two users, because the host private key belongs to the signer's user
and the agent's user must never read it, and it needs an administrator of the
manager side (see [`host-agent-enrolment.md`](host-agent-enrolment.md)):

1. **The Host is prepared on the manager.** The Host has `spec.identity`, and
   the manager publishes `status.agent.requestURIs`: the exact URI subject
   alternative names the request must carry (the host's URI identity and the
   enrolment record URIs).
2. **`vrtg-agent enroll csr`, as the signer's user.** It creates the host key
   when there is none and writes the request:

   ```
   sudo -u virtrigaud-signer vrtg-agent enroll csr \
       --uri spiffe://<td>/virtrigaud/host/<ns>/<name> --uri urn:virtrigaud:enrolment:<hex> \
       --uri urn:virtrigaud:provider:<uid> --uri urn:virtrigaud:install:<hex> \
       --uri urn:virtrigaud:mode:identity --out - > host.csr
   ```

   `--out -` writes the request to standard output, so the shell redirection
   (run as you, not as the signer's user) decides where it lands; a path given
   to `--out` is created by the signer's user, who must be able to write there.

   (`--uris-file` reads the same URIs one per line.) The key is ECDSA P-256,
   written as PKCS #8 PEM with mode `0600` through a private temporary file and
   a no-replace link into `--key-file` (default
   `/var/lib/virtrigaud-signer/host.key`), in a directory that must be owned by
   the signer's user with nothing for group or others. An existing key is
   never replaced: running the step again re-signs a new request with the same
   key (a lost request file, a new enrolment record). To enrol with a new key,
   remove the key file yourself first and restart the signer. The step refuses
   to run as root, as any user but `--signer-user`, or when `--agent-user` is
   the signer's user; it refuses URIs that are not exactly the host URI and one
   of each record URI, before a key is made; it never overwrites `--out`. The
   request is public (`0644`): subject common name the Host's name, the URIs as
   subject alternative names, no other extension, signed with the host key
   (proof of possession). It is exactly what the manager's authorisation rules
   accept. The log line names the key's pin.
3. **The request goes to an administrator, with files only.** `vrtg host
   enroll --host <name> --csr host.csr` (see the manager page) has it
   approved and signed by the host-agent sub-CA; the issued certificate is
   copied back to the host. Put `host.csr`, the certificate and the list of
   request URIs in a hand-off directory that only root or you can write to,
   created for the purpose and readable by the agent's user: as root, `install
   -d -m 0755 -o root -g root /run/vrtg-enrol`, or as an operator, `mktemp -d`
   and `chmod 0755` on it. Not a shared, world-writable directory such as
   `/var/tmp`. The steps read these files only when each is a regular file
   owned by root or by the user running the step and not writable by group or
   others (not reached through a symbolic link), so a file planted or replaced
   by another user is refused; neither file is secret.
4. **The signer is (re)started**, so it loads the key (`systemctl restart
   vrtg-agent-signer`); the signer serves only a key that exists.
5. **`vrtg-agent enroll install`, as the agent's user:**

   ```
   sudo -u virtrigaud-agent vrtg-agent enroll install --csr /run/vrtg-enrol/host.csr \
       --certificate /run/vrtg-enrol/host-cert.pem --uris-file /run/vrtg-enrol/request-uris
   ```

   The Host's **current** request URIs are required (`--uri`, repeatable, or
   `--uris-file`, one per line): its `status.agent.requestURIs` as the manager
   publishes them now. The request file must carry exactly these, so a request
   made before the manager drew another enrolment record is refused instead of
   being installed for a record the manager no longer expects.

   It takes the run flags that name places (`--state-dir`, `--signer-socket`,
   `--signer-user`, `--signer-key-file`, `--dev-trust-dir`). It refuses to run
   as root, runs the host checks on `pki/`, the trust directory and the key
   file (the agent's user must be refused the key), asks the running signer
   for its public key over the socket (it never opens the key file), and
   installs `pki/host.crt` (`0600`, temporary file, sync, rename) only when
   all of these hold:

   - the request file is one PEM request with a valid signature, whose URIs are
     exactly the host URI and one enrolment record and exactly the Host's
     current request URIs, and whose key is the signer's key;
   - the certificate chains directly to a pinned host identity issuer
     (`host-ca.pem` of the trust configuration) for server authentication,
     is valid now, is not a CA, has no certificate or CRL signing usage and the
     digital signature usage, has server authentication as its only usage,
     lives at most 30 days, has an empty subject or the Host's name, carries no
     DNS, address or e-mail name, carries exactly the request's URIs (so the
     enrolment record the manager drew) and certifies the signer's key;
   - the daemon's own loader accepts the installed file (otherwise the
     previous file is put back, or the new one removed).

   Only the host certificate is kept (`host.crt` holds one certificate); anything
   after it in the file is ignored. An installed certificate is replaced only
   with `--replace`, and never with the same enrolment.
6. **The agent starts, or reloads (SIGHUP).** The state directory's provisioning
   marker (`host-id`) must name the same host; `install` does not check it, the
   agent does at start.

### The enrolment record

(Development builds only; see above for what a release build does.) The agent
loads the host certificate with the same certificate profile the manager
enforces before it pins the key (no CA bit, no certificate or CRL signing
usage, digital signature, server authentication only, at most 30 days, empty
subject or the Host's name, no DNS, address or e-mail name). The agent keeps no
separate enrolment file. At start (step 6) and on reload it
derives its enrolment record from the host certificate: the enrolment id, the
Provider UID, the install id and the mode are in the URI subject alternative
names the sub-CA signed, so they are as authentic as the certificate. The
store's active enrolment is then brought to it:

- The store's active enrolment is the certificate's: nothing happens.
- The store was never enrolled: it is enrolled at start epoch 1, or at the
  epoch of `--dev-enrolment-file` when one is given (see below).
- Any other case: a **re-enrolment**. This means the certificate names an
  enrolment id the store does not have as active, whether another is active or
  none is (a freeze cut short, a re-provisioned host). The previous
  enrolment's open operations are frozen in bounded chunks (see
  [Re-enrolment freeze](#re-enrolment-freeze)), then the new enrolment is
  activated at the next start epoch (the highest so far plus one). An enrolment
  id the store has used before (active, frozen or retired) is refused: the
  store keeps its state, the error is logged and answers stay unsigned. A store
  that belongs to another host is never touched: the active enrolment's host,
  or with none active the store marker's and the newest enrolment record's
  host, must be the certificate's (otherwise the log says `StoreOfOtherHost`
  and answers stay unsigned). A store that names no host at all, whose
  marker names none and whose enrolment records were all discarded, belongs
  to nobody and can be adopted by whichever host presents a certificate: the
  store itself holds nothing that says otherwise, and the provisioning marker
  in the state directory (`host-id`) is what ties a disk to a host.

On reload, a certificate of the same host with a new enrolment re-enrols the
store first and only then replaces the running credentials, so no answer names
an enrolment that is not active. The reload handler runs the freeze: calls are refused while it runs (the host
reports `freeze_incomplete`), and a very long freeze delays the next reload and
the housekeeping of the same loop.

### Development enrolment file

`--dev-enrolment-file` (development builds only) keeps its role as a
stand-in for what the certificate does not carry, for a store's *first*
enrolment only:

```json
{"startEpoch": 2, "predecessors": ["<32 hex>"]}
```

`startEpoch` (at least 1) must equal the epoch counter the manager's
development operation signer starts at, so the manager's operation ids are not
refused as minted before the enrolment started. `predecessors` (optional) are
earlier enrolment ids of the host that this store never held. Unknown fields
are refused. Without the file, the first enrolment starts at epoch 1. A store
that is already enrolled is never changed by this file.

## Limits

- A release build executes no operation; every mutating call is refused. A
  development build executes operations only with the fake executor, which
  stands in for VMs with files; no real VM is created.
- The agent's continuity checks are local: a joint restore of the store and
  its anchor log is not detected on the host, and recovery may then run an
  operation again whose effect survived. The fake executor recognises its own
  effect (by the placement) and reports the step done, never a definite
  failure; a manager that tracks the host's chain position narrows the rest.
- A run that recovery resumes, after a restart, has no request payload: the
  store never writes request bytes. The fake executor needs none; a real
  executor must resolve such a step by inspection only, or park it with its
  effect in doubt, and never perform an effect it cannot reconstruct
  (documented on `executor.Operation.Payload`). How a real executor parks a
  step and how the manager re-delivers the request for it is a follow-up.
- The re-enrolment freeze, compaction and certificate installation are not
  reachable from the agent process yet, so their crash points are tested in
  the store library only.
- The host key is held by a separate signer process, but the signer is a
  digest oracle: `crypto/tls` hands it a digest, so it cannot check what it
  signs. A compromised agent process cannot exfiltrate the key or use it later,
  offline, but can still obtain signatures over digests of its choosing, online,
  and so can make host answers that verify. A separate answer key, held by a
  process that never terminates TLS, is a later requirement for the regulated
  profile; until then a signed host answer is no stronger than the agent
  process that relays it. The signer's rate limits bound how fast, not
  whether, and the handshake/answer split is declared by the agent, so it
  protects against outside floods only. The key's protection also assumes the
  attacker has not reached root or the signer's uid.
- Only the Provider's pods may reach the agent's port (a NetworkPolicy, and
  the port firewalled to the Provider's node address ranges: other pods on
  those nodes appear with the node's address, and a NetworkPolicy does not
  cover `hostNetwork` pods or the node itself). The pre-handshake throttle
  bounds the cost of a peer that can connect but authenticates nobody, and
  the signer's handshake budget is shared with every peer the throttle lets
  through.
- The agent's checks that it cannot read the key file (`access(2)`) are made
  at start-up and on reload, not continuously; a later change of the file's
  permissions is not noticed until then.
- The trust configuration's provenance is not proved: the agent checks only
  that the trust path is owned by root and that it cannot write it. Positive
  provenance (a verity-protected system extension or a signed bundle) is
  required before the agent leaves development builds.
- The clock floor is as hard to roll back as the store: a joint restore of
  the store and its anchor log is not detected on the host and restores an
  older floor too, and a power loss can lose the last minute or so of the
  floor's advance (see [The clock floor](#the-clock-floor)); repeated unclean
  fast reboots that each commit can leave the floor up to the wall clock plus
  5 minutes ahead.
- The shutdown unit is not ordered against the agent's unit; the order is
  decided when the agent runs real VMs.
- The release-artefact test checks the release workflow, the CI workflow and
  the `Makefile` only.
- A failed or missing clock floor (the latter when the store could not give
  the agent one, which also quarantines it) reads as no floor: the per-call
  check of the Provider's certificate, which uses the later of the host clock
  and a healthy floor, falls back to the host clock for reads, and every
  mutating call is refused (`clock_floor_unavailable`); grants are refused
  too. So while the floor is not OK, a rolled-back clock can re-admit an
  expired Provider certificate for read-only calls (`Hello`, `GetHostInfo`);
  nothing commits, and the answers say UNATTESTED.
- The provisioning marker is checked with `lstat` and then read with
  `O_NOFOLLOW`, not checked on the open descriptor; it holds no secret.
- `GetHostInfo` reports the component inventory but no CPU, memory or storage capacity, and the inventory is a detective control (see [Host inventory](#host-inventory)).
- Certificate renewal (`InstallRenewedCertificate`) and rotation are not
  available; nor is revocation handling beyond what the start-up checks do
  (an expired certificate is refused at start). Enrolment needs the manager
  side's flow by hand (copying files).
- The Provider identity CA is a development stand-in.
