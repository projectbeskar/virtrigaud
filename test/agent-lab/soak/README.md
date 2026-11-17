# Agent kill-soak and store growth runs

Tooling for two measurements of the per-host agent (`cmd/vrtg-agent`, development
build) and its operation store. It is test tooling: it runs as root **inside a
disposable test VM**, never on a shared host. The driver is
`test/agent-lab/cmd/agent-soak`.

## Prerequisites

An unprivileged QEMU test bed on a machine of your own and one disposable VM
(any recent systemd distribution; 4 GiB of memory is enough for the kill
soak) with a second, disposable virtual disk for the agent's state:
`setup-host.sh` formats it. Never run the tool on a shared or production
host.

## Build and prepare

```sh
CGO_ENABLED=0 go build -tags virtrigaud_dev -trimpath -o vrtg-agent ./cmd/vrtg-agent
CGO_ENABLED=0 go build -tags virtrigaud_dev -trimpath -o agent-soak ./test/agent-lab/cmd/agent-soak
```

Copy both into `/opt/agent-soak/` of the VM, give the VM a dedicated block
device for the agent's state, and run `setup-host.sh DEVICE` there. It creates
two unprivileged users (the agent and the signer), formats and mounts the
device, and creates the directories the tool uses. The state must be on a real
disk-backed file system: the agent refuses tmpfs, network file systems and
loop devices.

## Kill soak

```sh
agent-soak soak --kills 1000 --workers 8 [--mode binary|wrapped] [--seed N]
```

The tool builds a fresh host (a test PKI, the trust configuration, the
provisioning marker, the host certificate, the signer's key), starts the
signer once and the agent repeatedly, and has `--workers` goroutines drive VM
lifecycles (Create, a few Power operations, Delete; a deleted VM is sometimes
created again with a new placement) through the agent's real gRPC path. Every
operation has a durable identity (op_id, binding, attempt), and its grant is
minted and signed as the manager would. The agent is killed with SIGKILL at
random points:

| Mode | When |
|---|---|
| `random_delay` | a random time (up to 1.5 s) into a run |
| `after_acknowledgement` | right after a worker received an acknowledgement |
| `after_effect` | within 3 ms after the fake executor appended an effect line |
| `during_startup` | 20-400 ms into a start, during recovery |
| `during_held_step` | while steps are held by the fake executor's hold file |

The workers redeliver (same op_id, fresh grant), poll TaskStatus and wait for
the agent to come back, as a caller does. In `binary` mode the agent is the
real `vrtg-agent run` with the fake executor. In `wrapped` mode the same
daemon code runs inside the driver binary with the fake executor wrapped so
that each step invocation is recorded, before the call, outside the agent's
state: the fake executor logs only effects it performs, so a second call of a
once or irreversible step that finds its effect present is invisible in its
own log.

The fake executor's directory is a link from the state directory to a
directory outside it, so nothing that happens to the state erases the effect
log.

Checks (every violation fails the run; the result is JSON, default
`WORK/result.json`):

- effect and intents logs: the fake executor names each effect in its
  intents log before performing it and in its effects log after. Each
  (operation, step) of a once or irreversible step appears at most once in
  either log; every effect and intent belongs to an issued operation and its
  VM; every effect line has an intent line; every completed operation has an
  effect line, or an intent line (a kill between the effect and its effects
  log line, counted separately); idempotent steps re-run after a crash are
  counted, not failed; effect lines without an intents log fail the run;
- step invocations (wrapped mode): a once or irreversible step is called at
  most once;
- acknowledged operations: after restarts (sampled every 25 kills, in full at
  the end) every acknowledged operation is known to the agent and no terminal
  outcome changed; at the end the state directory is opened independently by
  the agent's user (`verify-store`, which also runs a scrub) and every
  acknowledged operation is looked up;
- deadlines: an acknowledged operation must leave `RUNNING` within
  `--op-deadline` (default 30 s) of agent uptime; at the end nothing may still
  be running, nothing unacknowledged;
- VM files: for VMs whose operations all succeeded, the fake VM exists and is
  powered as the outcomes imply;
- the agent stops cleanly on SIGTERM and the store is not quarantined.

Outcomes `UNKNOWN` (a once step's witness exists without its effect, or an
irreversible step's goal was not observed) and `FAILED_DIRTY` are counted and
reported; they are the store's fail-closed answers, not violations.

A short run, as root in the VM after `setup-host.sh`:

```sh
/opt/agent-soak/agent-soak soak --kills 50 --workers 4
```

PASS: the tool prints a JSON result with `"verdict": "PASS"` and exits 0
when the run recorded no violation of the checks above; any violation sets
`"verdict": "FAIL"`, lists it under `violation_details`, and exits 1.

## Store growth

```sh
agent-soak growth --steps 0,10000,100000,1000000 --probe 200
```

The agent first enrols the empty store. At each count of finished operations
(each leaves a tombstone) the tool fills the store through `Admit` and
`Execute` (the store's own path; the mix is Create, three Power operations,
Delete per VM; the size threshold is lifted for the fill and the size is
sampled every 10,000 operations), then measures: state size on disk (apparent
and allocated, witness file count), open time of the store (open, continuity
checks, recovery, and a scrub) with the page cache dropped first and again
warm, the real agent's time from spawn to READY cold and warm, its resident
set, the store's `Admit` latency over the last 1,000 operations, and
the acknowledgement and TaskStatus latency over the agent's gRPC path
(`--probe` Create operations). Results: `WORK/growth.json`.

## Sustained load, temporary files

```sh
agent-soak load --duration 75m --workers 1 [--prefill N] [--memory-max 256M]
agent-soak soak --kills 1000 --plant-temps
agent-soak growth --bulk --memory-max 256M --steps 0,100000,445000,1000000
```

`load` keeps a create/delete/power load on a running agent without kills (optionally on a store prefilled with tombstones) and records the agent's compaction log lines, the database file size and the agent's memory. With `--plant-temps` the soak leaves stray temporary key files as an interrupted atomic write would, before random kills and before ten signer kill/start cycles, and fails if any remains at the end. `--memory-max` runs the agent in a transient systemd scope with that `MemoryMax` and reads the scope's `memory.peak` and `memory.events`.
