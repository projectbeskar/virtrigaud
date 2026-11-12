# Per-host agent (development builds only)

`vrtg-agent` is the agent a clustered Provider with one agent per host
(`spec.type: cloudhypervisor`) will route to: one process per host, serving
that Provider over mutually authenticated TLS. This page describes the agent
as it is today: a process skeleton that starts, checks its host, opens and
recovers its operation store, answers reads with host-signed answers and
**refuses every mutating call**. It executes no operation, creates no VM,
scope, cgroup or process, and holds no Linux capability.

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
| `vrtg-agent shutdown-vms` | Stops every VM of the executor, only while the host is shutting down (see [Units](#systemd-units)). |
| `vrtg-agent enroll` | Not available yet; exits 1. A development build enrols at start-up instead (see [Enrolment](#development-enrolment)). |
| `vrtg-agent version` | Prints the version. |

`run` and `shutdown-vms` take the same flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--state-dir` | `/var/lib/virtrigaud-agent` | State directory: clean absolute path. |
| `--listen-address` | `:9460` | Address served to the Provider. |
| `--drain-timeout` | `30s` | How long a stop waits for running calls. |
| `--executor` | `none` | `none`, or `fake` in a development build. |
| `--dev-trust-dir` | (empty: `/usr/share/virtrigaud/trust`) | Development builds only: another trust configuration directory. The start-up checks still apply to it. |
| `--dev-enrolment-file` | (empty) | Development builds only: the development enrolment file. |

`shutdown-vms` also takes `--timeout` (default `5m`).

## Host layout

| Path | Owner, mode | Content |
|------|-------------|---------|
| `/var/lib/virtrigaud-agent/` | agent user, `0700` | State directory, on persistent block-device-backed storage. |
| `…/state.db`, `anchor.log`, `store.marker`, `state.db.lock`, `quarantine`, `witness/` | agent user | The operation store (journal, anchor log, markers, witness files). |
| `…/host-id` | agent user, `0600` | Provisioning marker: the host's URI identity, `spiffe://<trust domain>/virtrigaud/host/<namespace>/<host>`, written when the host is provisioned. |
| `…/pki/` | agent user, `0700` | `host.key` (ECDSA P-256 private key, PEM, PKCS #8 or SEC 1, `0600`) and `host.crt` (the host identity certificate, PEM, the leaf only, `0600`). Never put it in a backup. |
| `…/clock.floor` | agent user, `0600` | The clock floor (see [Start-up](#start-up-order)). |
| `…/fake-executor/` | agent user, `0700` | Fake VMs (development builds with `--executor=fake`). |
| `/usr/share/virtrigaud/trust/` | root, not writable by the agent | Trust configuration, from the host's signed, read-only system image. |

The trust configuration directory holds four files, all required:

| File | Content |
|------|---------|
| `host-ca.pem` | The CA certificate(s) that issue host identity certificates. |
| `provider-ca.pem` | The CA certificate(s) that issue Provider identity certificates. This issuer is a development stand-in until the Provider identity path is defined. |
| `grant-anchors.pem` | The operation grant anchors. |
| `provider-sans` | The Providers the agent serves, one per line: `spiffe://<trust domain>/virtrigaud/provider/<namespace>/<name>`. Blank lines and `#` comments are ignored. |

The agent refuses a trust configuration in which the grant anchors and the
host identity CA share a certificate, a key or an issuer, or in which one
signed, or chains to, the other: whoever holds the host identity CA's key
must not be able to issue a grant signer.

The host certificate must chain to `host-ca.pem`, certify the host key, carry
the server-authentication usage, not be a CA, be valid now, and carry exactly
these URI subject alternative names (see
[`host-agent-enrolment.md`](host-agent-enrolment.md#the-enrolment-record)):
the host's URI identity, `urn:virtrigaud:enrolment:<32 hex>`,
`urn:virtrigaud:provider:<Provider UID>`, `urn:virtrigaud:install:<32 hex>`,
`urn:virtrigaud:mode:identity`.

## Start-up order

| Step | What happens | On failure |
|------|--------------|------------|
| 0 | Host checks (below), then the trust configuration and the host identity are loaded, then the provisioning marker must name the host. | The start is refused (exit 1) with a reason; nothing on the host changes. |
| 1 | The store's single-writer lock. | A second agent on the same state directory exits. |
| 2 | The store opens with full fsync and checks its schema. | A store written for a newer agent refuses the start, naming the schema version it needs. |
| 3 | Local continuity checks: chain, anchor log, store marker, quarantine record. | The store is quarantined: reads still answer, every mutation is refused. |
| 4 | The clock floor is read and raised from the host's own clocks. | A missing floor for an enrolled store, or an unreadable one, quarantines the store (`journal_corrupt`); one that cannot be written quarantines it (`io_error`); clocks the floor cannot use refuse the start. |
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
| The agent runs in the host's initial PID, cgroup, user and time namespaces (a kernel without time namespaces passes that one). | `UnsupportedHost` |
| `/sys/fs/cgroup` is a unified cgroup v2 hierarchy. | `UnsupportedHost` |
| The state directory is a directory (not a symbolic link), owned by the agent's user, with no permission for group or others. | `StateDirInsecure` |
| The state directory is on ext4, ext3, XFS, Btrfs, F2FS (mounted from a `/dev/` device) or ZFS; never tmpfs, ramfs, overlay or a network file system. | `StateNotPersistent` |
| `host-id` exists, is a regular file owned by the agent's user that group and others cannot write, and names the host of the certificate. | `StateNotPersistent` |
| `pki/` and every file in it are owned by the agent's user, grant nothing to group or others, and are not symbolic links. | `PKIInsecure` |
| Every directory from `/` to the trust directory, and every file in it, is owned by root, not writable by group or others, not a symbolic link; and the agent's process cannot write the directory or its files. | `TrustConfigInsecure` |
| The trust configuration and the host identity load (see above). | `IdentityInvalid` |

## Peer authentication

The agent speaks TLS 1.3 only, with its host key and certificate. A
connection is accepted only from a client certificate that chains to
`provider-ca.pem`, carries exactly one URI subject alternative name, and
whose name is `spiffe://<trust domain>/virtrigaud/provider/<namespace>/<name>/<Provider UID>`
with the trust domain, namespace and name in `provider-sans`. Anything else
fails the handshake. Every call then compares the Provider UID with the
Provider the host certificate is enrolled with, and refuses another one with
`FailedPrecondition` and the ErrorInfo reason `PROVIDER_MISMATCH` before any
other check.

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
schema version and clock floor. It is signed with the host key over
`"virtrigaud.host-signed.v1" 0x00 ‖ be16(len(body_type)) ‖ body_type ‖ be32(len(body)) ‖ body`
(ECDSA P-256, SHA-256, low-S), as defined in
[`operation-grants-wire-shape.md`](operation-grants-wire-shape.md#the-signature-input); a test
reproduces the published test vector.

## Signals

| Signal | Effect |
|--------|--------|
| SIGTERM, SIGINT | `STOPPING=1`; the server stops accepting calls and waits up to `--drain-timeout`; the clock floor is saved; the store is closed; exit 0. During recovery, recovery is cancelled and resumes at the next start. |
| SIGHUP | `RELOADING=1`; the key material and trust configuration are checked (the `pki/` and trust checks above) and loaded again. A reload that fails, or whose certificate names another host or enrolment, is refused and the running credentials stay. New handshakes and every later call use the new credentials. `READY=1` follows. |

## systemd units

`packaging/systemd/` holds reference units and `sysusers.d` / `tmpfiles.d`
fragments; they are not installed by anything.

`vrtg-agent.service`: `Type=notify`, `Restart=always`, `RestartSec=2`,
`StartLimitIntervalSec=300`, `StartLimitBurst=10`, `KillMode=process`,
`User=virtrigaud-agent`, `StateDirectory=virtrigaud-agent` (`0700`),
`NoNewPrivileges=yes`, an empty capability bounding set, `ProtectSystem=strict`
and further sandboxing. It sets no `Delegate=` and creates no scope. No other
unit upholds it, so it can be stopped for maintenance.

`vrtg-vm-shutdown.service`: a `oneshot` unit with `RemainAfterExit=yes` whose
stop action is `vrtg-agent shutdown-vms`. That command stops the VMs only
when `systemctl is-system-running` reports `stopping` or a start job of
`shutdown.target`, `reboot.target`, `halt.target`, `poweroff.target` or
`kexec.target` is queued; a maintenance stop of the unit stops nothing, and
so does a state it cannot read. The unit neither wants, requires nor orders
itself after `vrtg-agent.service`, so starting it never starts an agent
stopped for maintenance.

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
- The host key is used in the agent's own process, behind Go's
  `crypto.Signer`; it is not yet held by a separate signer process.
- `GetHostInfo` reports no inventory (CPU, memory, storage, components).
- Certificate renewal (`InstallRenewedCertificate`) and `vrtg-agent enroll`
  are not available.
- The Provider identity CA is a development stand-in.
