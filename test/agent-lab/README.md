# Per-host agent lab scripts

Test tooling for the per-host agent (`cmd/vrtg-agent`) on real systems:
scripts and small Go drivers that exercise the agent on disposable virtual
machines, where a test may format disks, cut power, kill processes, change
users and units, or install a throw-away Kubernetes cluster.

Every area needs an unprivileged QEMU test bed on a machine of your own and
disposable VMs that you destroy afterwards; the enrolment area also needs a
disposable Kubernetes cluster, which its setup script creates inside the
test VM. None of the scripts is meant for production or shared hosts, or for
a shared cluster. The agent under test is a development build
(`-tags virtrigaud_dev`).

| Area | What it exercises | Driver |
|---|---|---|
| [`journal/`](journal/README.md) | The agent journal's durability on real block devices: torn writes replayed entry by entry, power cuts, I/O errors. | `cmd/journal-torture` |
| [`systemd/`](systemd/README.md) | The shipped systemd units installed as a package would install them: start at boot, the shutdown unit's guard, the system call filter, rollback, cgroup v1 and persistent-state refusals, a system extension on an immutable host, the maintenance contract. | `cmd/agent-labpki` |
| [`enrolment/`](enrolment/README.md) | Enrolment end to end against a real cert-manager, with the shipped RBAC and admission samples: online and offline enrolment, revocation, renewal, re-key and fault checks. | `cmd/enrol-lab` |
| [`soak/`](soak/README.md) | The agent killed at random points under a VM lifecycle load, with every operation's effects checked against the executor's logs and the store; store growth and sustained load. | `cmd/agent-soak` |

Each area's README lists its prerequisites, how to run it and what PASS
means.
