# Per-host agent on real systemd hosts

Scripts that install the per-host agent's reference units
(`packaging/systemd/`) on real systemd hosts and check how they behave:
`systemd-analyze` results, the shutdown unit's guard, an agent rollback below
the store's reader version, the cgroup v1 refusal, the positive
persistent-state check, and start at boot from a system extension on an
immutable host. They are test tooling only: they run as root **inside a
disposable test VM**, never on a shared host. Every script prints `PASS` /
`FAIL` lines and a final `VERDICT`.

The agent is a development build (`-tags virtrigaud_dev`) and, unless a script
says otherwise, runs the development fake executor, whose "VMs" are JSON files
under `/var/lib/virtrigaud-agent/fake-executor/vms/`. Scripts seed powered-on
fake VMs there and compare their content digest and modification time before
and after each action: any difference is a VM touched.

## Prerequisites

- An unprivileged QEMU test bed on a machine of your own, and disposable
  VMs that you can destroy afterwards: Ubuntu 24.04 or Rocky Linux 9 for the
  package-style runs, a Kairos image for the system extension runs. The
  scripts change the VM's users, units, kernel command line and file
  systems; never run them on a shared or production host.
- A Go toolchain on the build machine; `systemd-repart` and `erofs-utils` in
  the VM that builds the system extension.

## Build

```sh
CGO_ENABLED=0 go build -trimpath -tags virtrigaud_dev -o vrtg-agent ./cmd/vrtg-agent
CGO_ENABLED=0 go build -trimpath -o agent-labpki ./test/agent-lab/cmd/agent-labpki
agent-labpki gen -out id -host <host-name>
```

`agent-labpki gen` writes a throw-away identity: the trust configuration
(host, Provider and grant issuers and the Provider allow-list), the host key
for the signer, a host certificate carrying a first-epoch enrolment record,
the `host-id` provisioning marker and a Provider client identity.
`agent-labpki hello -dir id -addr 127.0.0.1:9460` calls `Hello` as that
Provider and prints the decoded, signed `HostAnswer`; `agent-labpki hostinfo`
does the same with `GetHostInfo`, whose answer carries the host inventory.

Stage directory copied into the VM (`STAGE`): `bin/vrtg-agent`,
`bin/agent-labpki`, `units/` (a copy of `packaging/systemd/`), `id/`.

## Scripts

| Script | What it does |
|---|---|
| `install.sh STAGE` | Installs exactly what `packaging/systemd` ships, like a package: binary at `/usr/lib/virtrigaud/vrtg-agent`, the five units and the `multi-user.target.d` drop-in in `/usr/lib/systemd/system`, the sysusers.d and tmpfiles.d fragments (applied by the shipped `vrtg-agent-setup.service`), trust configuration root-owned in `/usr/share/virtrigaud/trust`, key, certificate and marker in place; enables the units with an `[Install]` section and starts them. `EXECUTOR=fake` adds `/etc` drop-ins for the fake executor (the only change, and only when VMs must be observable). |
| `analyze.sh [REPO]` | `systemd-analyze verify` of the installed units, `systemd-analyze security` (table and overall exposure) per unit, effective properties, the processes' credentials, seccomp mode and namespaces, and a Hello probe. With `REPO` (a copy of `hack/` and `packaging/`) also runs `hack/verify-systemd-units.sh`. Then waits past the first inventory collection and reports SIGSYS exits, audit `SECCOMP` records and the inventory's feature probes (`agent-labpki hostinfo`). |
| `shutdown-guard.sh run [N]` | Seeds N powered-on fake VMs, then stops, starts, restarts and try-restarts the shutdown unit, performs a package-style upgrade (unit files reinstalled, `daemon-reload`, `try-restart` of all three units), and repeats the shutdown-unit actions with the agent stopped for maintenance: no VM may change and the stopped agent must stay stopped. |
| `shutdown-guard.sh after-reboot` | Positive control after `systemctl reboot`: the previous boot's journal shows the guard acting and every fake VM is off. |
| `rollback-reader-version.sh OLD_BINARY` | Downgrades the binary in place and restarts the unit: the older agent must fail to start, name the store's reader version, and leave the store files and fake VMs byte-identical; a control run shows the older binary starting on a fresh store; the roll forward starts READY on the same store. |
| `cgroup-v1-refusal.sh set unified\|hybrid\|legacy` / `check refuse\|start` | Sets the cgroup hierarchy on the kernel command line for the next boot (grubby, or `/etc/default/grub.d`), then checks the agent's `UnsupportedHost` refusal (or READY on unified). |
| `persistent-state.sh CASE...` | Puts the state directory on tmpfs, on an overlay whose upper is on tmpfs, on an ext4 loop device backed by tmpfs, or removes the provisioning marker, and expects `StateNotPersistent`; `as-is` expects READY (or a refusal with `EXPECT_AS_IS=refuse`). |
| `build-sysext.sh STAGE OUT` | Builds an erofs + dm-verity system extension image (no signature partition) carrying exactly the shipped files: binary, every unit and the `multi-user.target.d` drop-in, sysusers.d and tmpfiles.d fragments, and a trust configuration. |
| `sysext-host.sh` | Immutable-host side (Kairos): `persist on\|off` (bind-mount rule for the state directories), `activate IMAGE` (install and enable for every boot entry), `provision STAGE`, `report` (one block of boot evidence: setup-unit ordering, user ids, units, Hello), `maint` (a maintenance-stopped agent must stay stopped through shutdown-unit stop/start/try-restart, `daemon-reload` and an extension refresh), `kicker-stop` (stopping the once-per-boot start unit by hand must not start a maintenance-stopped agent). |
| `maint-contract.sh CMD` | The maintenance contract with the shipped units: `kicker-stopped` (a maintenance-stopped agent stays stopped through restarts of the boot-start unit and a package-style reinstall), `boot-state` (after a boot: enablement, unit states, the boot-start unit's log), `uid-move USER` / `uid-restore USER` (a service user moved to another uid: the setup unit fails and the agent and signer stay down; restored: all start), `maint-burst` and `restart-burst N` (routine maintenance in quick succession never puts the shutdown unit, or with `restart-burst` the setup unit, in `start-limit-hit`), `foreign-entries DIR [pki]` (entries of another owner under a state directory fail the setup unit, which names them), `seccomp-log` (the filter's EPERM and log channel on a transient unit). |
| `crash-temps.sh` | Plants the temporaries an interrupted atomic write leaves in `pki/` and in the signer's key directory, kills agent and signer with SIGKILL, and checks that the restarted processes removed them (and kept look-alikes). |

## Running

A minimal run on a fresh VM, as root in the VM, with `STAGE` copied in:

```sh
EXECUTOR=fake ./install.sh "$STAGE"   # install as shipped, plus the fake-executor drop-ins
./analyze.sh                          # unit analysis, filter and inventory probe
./shutdown-guard.sh run 8             # no VM touched by maintenance actions
./maint-contract.sh kicker-stopped    # a maintenance-stopped agent stays stopped
systemctl reboot
# after the reboot:
./shutdown-guard.sh after-reboot      # the guard stopped every VM at the reboot
./maint-contract.sh boot-state        # the agent started once at boot
```

The other scripts are independent checks run the same way on a VM prepared
by `install.sh`.

## What PASS means

Scripts with checks print one `PASS` or `FAIL` line per check and end with
`VERDICT PASS` (exit code 0) only when every check passed. `install.sh`,
`analyze.sh`, `build-sysext.sh` and the `report` command of `sysext-host.sh`
print evidence (unit states, `systemd-analyze` output, a `Hello` answer)
for a reader to check: every unit active or, for the oneshots, exited
successfully, no `SIGSYS` exit or seccomp record, and a signed `Hello`
answer whose agent state is `READY`.

## Older agent for the rollback check

The store written by the current agent carries `min_reader_version` 2. The
older binary is a development build of `cmd/vrtg-agent` from commit
`1f7a0f87`, the last commit whose journal reads only schema 1
(`internal/agent/journal/store.go`: `ReaderMax = 1`):

```sh
git worktree add --detach /tmp/old 1f7a0f87
(cd /tmp/old && CGO_ENABLED=0 go build -trimpath -tags virtrigaud_dev -o vrtg-agent-old ./cmd/vrtg-agent)
```

That build predates the separate signer and reads the host key from the
agent's own `pki/` directory, so `rollback-reader-version.sh` also places the key there for
the rollback (as a host running that build would have had it) and removes it
for the roll forward.

## Deviations from the shipped units

None in the images or the installed unit files. The only optional change is
`EXECUTOR=fake` (or `fake_dropins` in `common.sh`): drop-ins in `/etc` that run
the agent and the shutdown unit with the development fake executor, so that
the guard and rollback checks have VMs to observe.
