# Per-host agent (development builds only)

`vrtg-agent` is the agent a clustered Provider with one agent per host
(`spec.type: cloudhypervisor`) will route to: one process per host, serving
that Provider over mutually authenticated TLS. This page describes the agent
as it is today: a process skeleton that starts, checks its host, opens and
recovers its operation store, answers reads with host-signed answers and
**refuses every mutating call**. It executes no operation, creates no VM,
scope, cgroup or process, and holds no Linux capability. It does not hold
its host private key either: a separate signer process, run as its own user,
does (see [Host key signer](#host-key-signer)).

Nothing here is part of a release. No release workflow, container image or
make target builds `vrtg-agent` (a test in `cmd/vrtg-agent` checks the
release workflow, the CI workflow and the `Makefile`), and no Provider routes
to it yet.

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
| `vrtg-agent enroll` | Not available yet; exits 1. A development build enrols at start-up instead (see [Enrolment](#development-enrolment)). |
| `vrtg-agent version` | Prints the version. |

`run` and `shutdown-vms` take the same flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--state-dir` | `/var/lib/virtrigaud-agent` | State directory: clean absolute path. |
| `--listen-address` | `:9460` | Address served to the Provider. |
| `--drain-timeout` | `30s` | How long a stop waits for running calls. |
| `--scrub-interval` | `1h` | How often the operation store reads every record and verifies it against its live-state digest. Must be positive; the scrub cannot be switched off. A scrub that finds damage quarantines the store (`journal_corrupt`), and the service status shows it. |
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
| `…/clock.floor` | agent user, `0600` | The clock floor (see [Start-up](#start-up-order)). |
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
| 4 | The clock floor is read and raised from the host's own clocks. | A missing floor for an enrolled store, or an unreadable one, quarantines the store (`inconsistent_store`); one that cannot be written quarantines it (`io_error`); clocks the floor cannot use refuse the start. |
| 5 | The server starts serving, unattested. Nothing is committed before step 6. | |
| 6 | Continuity is established. A development build with `--dev-enrolment-file` enrols a store that was never enrolled. | |
| 7 | Recovery of interrupted operations. An operation left runnable stays as it is: this agent starts no runner. | |
| 8 | systemd is notified `READY=1`; the agent's trust state is READY. | |

A quarantined store also sends `READY=1`, with `STATUS=AGENT_STATE_QUARANTINED
(<reason>)`, so that systemd keeps the agent running to answer reads. A store
stays quarantined until the host is enrolled again.

The clock floor is a file beside the store, not a record in it, because it is
raised before continuity is established, when the store must not commit; a
restore of the state directory restores it too.

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
  at once and at most 2 from one source. The slot is given back when the
  handshake ends. A source that opens connections and stays silent therefore
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
address ranges.

Connections are closed after 30 minutes (and a minute of grace for running
calls), so each is authenticated again with a new handshake. A handshake must
finish within 10 seconds, a connection carries at most 64 concurrent calls,
and a client may not send keepalive pings more often than every 30 seconds.

## What the agent serves

| Method | Answer |
|--------|--------|
| `provider.v1.Provider/GetHostInfo` | `HostInfo` with `id` (the Host name) and `health` (`READY` once the agent is ready). With `answer_nonce`, a signed `HostAnswer` in `host_answer`. `host_id` naming another host, a nonce that is not 16 bytes, or `continuity_seq` without a nonce: `InvalidArgument`. No inventory fields yet. |
| `agent.v1.AgentService/Hello` | A signed `HostAnswer`. The nonce (16 bytes) and `caller_api_version` (at least 1) are required. |
| `agent.v1.AgentService/AgentStatus` | A signed `HostAnswer`. The nonce is required. |
| `agent.v1.AgentService/InstallRenewedCertificate` | `Unimplemented`. |
| Every mutating Provider method (the operation kind table) | `Unavailable` with `UNATTESTED` before the agent is ready; `FailedPrecondition` with `QUARANTINED` while the store is quarantined; `Unimplemented` once ready. |
| Every other Provider method | `Unimplemented`. |

A signed answer is made only for a request with a nonce, and only while the
store's active enrolment is the one the host certificate names; otherwise the
read answers without `host_answer`. Its body carries the nonce, the enrolment,
the store's chain position, newest checkpoint and trust state (read together),
the agent's API version (1), grant versions (`[1]`), mode (`identity`), store
schema version and clock floor. It is signed with the host key, by the signer process, over
`"virtrigaud.host-signed.v1" 0x00 ‖ be16(len(body_type)) ‖ body_type ‖ be32(len(body)) ‖ body`
(ECDSA P-256, SHA-256, low-S), as defined in
[`operation-grants-wire-shape.md`](operation-grants-wire-shape.md#the-signature-input); a test
reproduces the published test vector.

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
| SIGHUP | `RELOADING=1`; the key material and trust configuration are checked (the `pki/`, host key and trust checks above), the signer is asked for its public key again and the host certificate is loaded again. A reload that fails, or whose certificate names another host or enrolment, is refused and the running credentials stay. New handshakes and every later call use the new credentials. `READY=1` follows, with a status that starts with the trust state, then the result: for example `STATUS=AGENT_STATE_QUARANTINED (rolled_back); credentials reloaded` or `STATUS=AGENT_STATE_READY; reload refused: <reason>` (`IdentityInvalid`, `EnrolmentChanged`, or a host-check reason such as `PKIInsecure`; a signer that cannot be reached is `IdentityInvalid` with the signer named in the log), never a path or a secret. |

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

## Development enrolment

Host enrolment by the agent arrives with a later change. Until then a
development build enrols a store that was never enrolled, at step 6, from:

- the host certificate's enrolment record (enrolment id, Provider UID,
  installation id), and
- the file named by `--dev-enrolment-file`:

```json
{"startEpoch": 2, "predecessors": ["<32 hex>"]}
```

`startEpoch` (at least 1) must equal the epoch counter of the manager's
development enrolment file, so the manager's operation ids are not refused as
minted before the enrolment started. `predecessors` (optional) are earlier
enrolment ids of the host that this store never held. Unknown fields are
refused. A store that is already enrolled is never changed; if its enrolment
is not the one the certificate names, reads answer unsigned.

## Limits

- The agent executes no operation; every mutating call is refused.
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
  those nodes appear with the node's address). The pre-handshake throttle
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
- The clock floor file is outside the chain-anchored store: a rollback of
  that file alone is not detected. This must be fixed before grant expiry is
  enforced on the host.
- The shutdown unit is not ordered against the agent's unit; the order is
  decided when the agent runs real VMs.
- The release-artefact test checks the release workflow, the CI workflow and
  the `Makefile` only.
- The per-call check of the Provider's certificate uses the host clock, not
  the clock floor.
- The provisioning marker is checked with `lstat` and then read with
  `O_NOFOLLOW`, not checked on the open descriptor; it holds no secret.
- `GetHostInfo` reports no inventory (CPU, memory, storage, components).
- Certificate renewal (`InstallRenewedCertificate`) and `vrtg-agent enroll`
  are not available.
- The Provider identity CA is a development stand-in.
