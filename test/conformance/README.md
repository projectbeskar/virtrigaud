# Conformance test specifications

The `*.yaml` files in [`specs/`](specs/) are the VirtRigaud conformance tests. `vcts` validates,
lists and runs them; see [`docs/cli-tools.md`](../../docs/cli-tools.md#vcts) for the command and
for what a run does to the cluster. The directory also holds a Go suite that checks a clustered
Provider's gRPC contract without a cluster: see [Clustered-provider gRPC conformance
suite](#clustered-provider-grpc-conformance-suite) below.

```
vcts validate test/conformance/specs
vcts list
vcts run --provider <name> -n <namespace> --set IMAGE=<vmimage>
```

## File format

A file is a YAML list of tests. A key the format does not define is an error.

```yaml
- name: vm-power-cycle               # lower-case letters, digits and '-', at most 63; unique across the files
  description: What the test checks  # required
  requiredCapabilities: [vm-create, vm-power, vm-delete]
  timeout: 20m                       # bounds the steps (default 15m)
  labels: {category: basic}          # informational
  steps: [...]                       # at least one
  cleanup: [...]                     # optional; run after the steps, whatever happened
```

### Steps

Every step has a `name`, a `type` and a `resource` that names the object it acts on or reads:

```yaml
resource:
  apiVersion: infra.virtrigaud.io/v1beta1    # the only API group allowed
  kind: VirtualMachine                       # any kind of that group
  metadata:
    name: vcts-${RUN}-vm                     # no namespace: the run's namespace (-n) is used
```

| Type | What it does |
|------|--------------|
| `create` | Creates the resource, adding the labels `vcts.virtrigaud.io/run`, `vcts.virtrigaud.io/test` and `app.kubernetes.io/managed-by: vcts`. Its `metadata.name` must contain `${RUN}`. The object is deleted by the cleanup. |
| `update` | JSON-merge-patches the named object with the resource's other fields (`spec`, `metadata.labels`, `metadata.annotations`); the run labels cannot be changed. |
| `delete` | Deletes the named object (it does not wait: follow it with a `wait` for `Deleted`). |
| `wait` | Reads the object every 2 seconds until `waitFor.condition` holds **and** every `validate` entry passes (at least one of the two is required), or until the timeout. |
| `validate` | Reads the object once and checks every `validate` entry. |

`update` and `delete` act only on objects that carry the run's label (`vcts.virtrigaud.io/run`);
no step may create, change or delete a `Provider`. A `create` or `update` step may have `validate`
entries, checked once right after the step; a `delete` step may not.

Other step keys: `timeout` (default 1m; for a `wait`, `waitFor.timeout` wins and the default is 5m),
`optional: true` (a failure is recorded but the test goes on) and `description`. After a step that
is not optional fails, the remaining steps are reported as skipped.

### Wait conditions

| `waitFor.condition` | Holds when |
|---------------------|------------|
| `Ready`, `Available` | The status condition of that type is `True` and, when it records an `observedGeneration`, it is not older than `metadata.generation` (so a `Ready` from before an update does not count) |
| `Running`, `Stopped` | A VirtualMachine's `status.powerState` is `On` / `Off` |
| `Provisioned` | A VirtualMachine's `status.id` is set |
| `Failed` | `status.phase` is `Failed` |
| `Deleted` | The object does not exist |

While waiting for anything but `Failed` or `Deleted`, an object whose `status.phase` stays `Failed`
for 30 seconds fails the step (the operator retries some failed phases, such as a create in
backoff). An object that does not exist yet is waited for (it may be one the operator creates,
such as a clone's target). On a timeout the error says what was last seen.

### Assertions

```yaml
validate:
  - path: .status.conditions[?(@.type=="Ready")].status   # kubectl JSONPath, starting with '.' or '$'
    operator: eq                                          # default eq
    value: "True"
```

| `operator` | Passes when |
|------------|-------------|
| `eq`, `ne` | The value equals (does not equal) `value`: numerically when both are numbers, otherwise as text |
| `gt`, `lt`, `gte`, `lte` | Both are numbers or Kubernetes quantities (`2Gi`) and compare so |
| `contains` | A list holds `value`, or a string contains it |
| `matches` | The value, as text, matches the regular expression `value` |
| `exists` | The path is set (`value: false`: it is not set) |

Every operator but `exists` and `contains` needs the path to resolve to exactly one value. Quote
`"On"`, `"Off"`, `"True"` and `"False"` so they stay strings. Escape dots in a map key with `\`
(`.metadata.labels.vcts\.virtrigaud\.io/run`).

### Variables

`${NAME}` in any string value of a resource or of an assertion's `value` is replaced before the
step runs. `vcts` sets `RUN` (the run id), `TEST` (the test name), `PROVIDER` (`--provider`) and
`NAMESPACE` (`-n`); every other name comes from `--set NAME=value`. The shipped tests use `IMAGE`,
the name of an existing `VMImage` the provider can boot.

### Capabilities

`requiredCapabilities` names what the Provider must offer for the test to run; otherwise the test
is reported as skipped. `vm-create`, `vm-delete`, `vm-power`, `vm-reconfigure` and `vm-clone` are
always offered. `vm-snapshot`, `vm-snapshot-memory`, `vm-reconfigure-online`, `vm-clone-linked`,
`image-import`, `disk-export` and `disk-import` come from the Provider's
`status.reportedCapabilities`. Any other name is a validation error.

### Cleanup

After the steps, `vcts` runs the test's `cleanup` steps (a `delete` of an object that is already gone
is not an error there), then deletes the objects labelled with the run and test that it did not
create itself, then everything it created, newest first, waiting until each is gone. A `VMClone`
with a provider clone still in flight is waited for first and, if it does not settle, left in
place with its source and target VMs. To have an
object the operator creates on the test's behalf cleaned up, give it both labels; for a clone's
target VM:

```yaml
spec:
  target:
    name: vcts-${RUN}-dst
    labels:
      vcts.virtrigaud.io/run: ${RUN}
      vcts.virtrigaud.io/test: ${TEST}
```

---

# Clustered-provider gRPC conformance suite

The Go package [`clustered/`](clustered/) is a reusable conformance suite for a **clustered remote
Provider**: a Provider that fronts several hypervisor hosts and is told, on every per-VM call,
which host to act on ([ADR-0007](../../docs/adr/0007-clustered-orchestrator-provider.md) and its
Addenda A and B). It is separate from the `vcts` specs above: it needs no cluster, no hypervisor
and no network beyond loopback, and it tests the **gRPC contract**
([`proto/provider/v1/provider.proto`](../../proto/provider/v1/provider.proto)) from the outside.

```
make test-conformance-clustered     # the suite with -race: the fake's mutation meta-test, then clustered libvirt
go test ./test/conformance/clustered/...                                         # the fake and the meta-test
go test ./internal/providers/libvirt/ -run TestClusteredLibvirt_Conformance -v   # clustered libvirt, every case listed
```

Both are also part of `make test`. On a development machine the fake with its mutation test takes
about 15 s and clustered libvirt about 10 s (several cases wait on a deadline, for a host that never
answers), plus the compile; `-race` adds compile time only.

## How it works

A case starts the Provider's **real gRPC server** on a loopback listener and talks to it with two
clients:

- a raw `providerv1.ProviderClient`, which sees the wire exactly as the Provider sent it: status
  codes and `google.rpc.ErrorInfo` reasons (`HOST_UNAVAILABLE`, `VM_OPERATION_FAILED`, ...);
- the manager's own transport client (`internal/transport/grpc`), which shows how those answers
  are mapped (`contracts.IsHostUnavailable`, `IsNotFound`, `IsInvalidSpec`, ...) and whether they
  count toward the Provider's circuit breaker.

A case never reaches into a Provider's internals. What it needs from the Provider's world, it asks
of the provider's adapter, a `clustered.Target`: the host ids, a way to place a VM on a host out
of band (owned by a given identity, owned by another, or unstamped), a way to read the ground truth
of a VM from its host (state, size, owner uid; so a refused call is proven to have changed
nothing), a way to make a host unreachable, and a way to restart the Provider process while the
hosts keep their state. When a call returns a task, a case completes it through the Target
(`TaskController`) before it looks at the effect.

## Required and Recommended

Every case cites where its rule comes from (`Case.Source`: a proto comment, an ADR section, a
`contracts` doc comment).

- **Required** cases check a rule the proto, the `contracts` package or an ADR states. A failing
  Required case fails the run.
- **Recommended** cases check good practice the contract does not state. A failing Recommended case
  is reported in the run's summary (`RECOMMENDED FAILED`), with what was observed, and does not
  fail the run.

A case that needs an optional feature (below) the Target does not offer is skipped, and the skip
and its reason are listed when the suite ends. A Target can also record a known deviation with
`clustered.WithExpectedFailures`: each entry has a pointer and a substring the failure output must
contain, and the run fails if the case starts passing or fails in some other way.

## Cases

"fake" is the instrumented reference provider (below), asynchronous: in its synchronous
configuration the two `async-tasks` cases are skipped.

| Case | Level | Rule | Needs | fake | clustered libvirt |
|------|-------|------|-------|------|-------------------|
| `route.missing-host` | Required | an empty or blank target_host_id is InvalidArgument on every per-VM call; the provider never picks a host, and the VM is not touched | - | pass | pass |
| `route.unknown-host` | Required | a host id the provider does not front is a retryable, host-scoped Unavailable (HOST_UNAVAILABLE ErrorInfo); it is never dialed and never answered NotFound | - | pass | pass |
| `route.invalid-host-id` | Required | a malformed or differently-cased host id is never routed: InvalidArgument or the host-scoped Unavailable, never success, never NotFound, never dialed | - | pass | pass |
| `route.correct-host` | Required | a call acts on the VM at the named host only: the same VM name on another host is a different VM, and a VM that is not on the named host is not found there (no lookup on other hosts) | - | pass | pass |
| `owner.describe-foreign` | Required | Describe of a VM whose stamp is missing or records another owner is exists=false and returns none of its state | - | pass | pass |
| `owner.mutate-foreign` | Required | Power (every op), Delete and Reconfigure of a VM whose stamp is missing or records another owner are NotFound, and the VM is not started, stopped, rebooted, resized or destroyed | - | pass | pass |
| `owner.request-without-owner` | Required | a request whose owner identity is missing, or has no uid (even with the right namespace and name), never operates on a VM, stamped or not: Describe says exists=false; Power, Reconfigure and Delete are NotFound | - | pass | pass |
| `owner.unreadable-stamp` | Required | a VM whose owner stamp is unreadable or records more than one owner is never started, stopped, resized or destroyed: Power, Reconfigure and Delete are NotFound, and Describe does not return its state | `unreadable-stamp` | pass | pass |
| `owner.refusal-discloses-nothing` | Required | a refusal for a VM of another owner names neither that owner (uid, namespace, name) nor leaks its state | - | pass | pass |
| `owner.vm-not-owned-reason` | Required | a Provider that adds the VM_NOT_OWNED reason does so for a refusal for another owner's VM, never for a VM that is absent; the manager marks it (contracts.IsVMNotOwned) | `vm-not-owned-reason` | pass | skip |
| `hostdown.per-vm` | Required | a per-VM call routed to an unreachable host is codes.Unavailable with a HOST_UNAVAILABLE ErrorInfo naming that host: never NotFound (the VM is not gone), never a task result | - | pass | pass |
| `hostdown.isolated` | Required | one unreachable host does not affect calls on the other hosts | - | pass | pass |
| `hostdown.recovery` | Required | when the host comes back its VMs are found again, unchanged, and calls succeed: nothing was re-created or dropped meanwhile | - | pass | pass |
| `hostdown.not-counted-against-provider` | Required | a host-scoped unavailability is not an infrastructure failure of the Provider: the manager's circuit breaker stays closed and calls on healthy hosts still go through | - | pass | pass |
| `hostdown.blackholed-list` | Required | a host that accepts connections and never answers does not stall ListVMs: the call answers inside the caller's deadline and reports that host unreachable | `host-blackhole` | pass | pass |
| `hostdown.blackholed-per-vm` | Recommended | a per-VM call to a host that never answers fails inside the caller's deadline with the host-scoped Unavailable, not a bare deadline error | `host-blackhole` | pass | pass |
| `hostdown.operation-failed` | Required | an operation that reaches its host and fails there carries VM_OPERATION_FAILED, is not NotFound or host-unavailable, changes nothing, and does not count toward the Provider's circuit breaker | `host-faults` | pass | pass |
| `hostdown.connection-lost` | Required | a host whose connection dies while it runs an operation that changes a VM is answered with HOST_CONNECTION_LOST together with VM_OPERATION_FAILED (never NotFound, never success); repeating the operation once the host answers succeeds | `host-faults` | pass | pass |
| `hostdown.removed-host` | Required | a host removed from the Provider's registry while VMs are bound to it is an unavailable host, not a missing VM: calls to it are the host-scoped Unavailable, its VMs are untouched, the other hosts are unaffected, and a task naming it is NotFound | `host-removal` | pass | pass |
| `tasks.malformed-ref` | Required | TaskStatus refuses a reference that is not host-encoded with InvalidArgument, and a host-encoded one with a malformed host id with InvalidArgument or NotFound: never a task result (done or failed), never a dial | - | pass | pass |
| `tasks.unknown-host` | Required | TaskStatus for a well-formed reference naming a host outside the provider's registry is NotFound: never done, never an endpoint, never dialed | - | pass | pass |
| `tasks.forged-inner-ref` | Recommended | a well-formed reference to a reachable host it fronts, but to a task it never issued, is refused (NotFound or InvalidArgument), not reported done | - | pass | **recommended fail** |
| `tasks.host-down-ref` | Required | TaskStatus for a well-formed reference naming a host the provider fronts but cannot reach is the retryable, host-scoped Unavailable that names the host: not NotFound, not a task result | - | pass | pass |
| `tasks.async-routing` | Required | an asynchronous call returns a host-encoded task reference naming the host it ran on; TaskStatus is routed to that host (done only when that host finished it) and a task of an unreachable host is host-scoped Unavailable while other hosts' tasks still answer | `async-tasks` | pass | skip |
| `tasks.synchronous-effect` | Required | a routed call that returns no task has completed: its effect is already in place when the response arrives; a task it does return is host-encoded with the request's host | - | pass | pass |
| `list.all-hosts` | Required | ListVMs answers for every host in one call; every VM is tagged with the host it is on, and a VM is identified by (host id, VM id): the same name on two hosts is two VMs | - | pass | pass |
| `list.unreachable-host` | Required | an unreachable host is reported in unreachable_host_ids and never silently dropped; the call still succeeds and returns the VMs of the hosts that answered | - | pass | pass |
| `list.all-unreachable` | Required | when no host answers, ListVMs still succeeds, lists every host as unreachable and invents no VM | - | pass | pass |
| `list.recovery` | Required | once a host answers again it is no longer reported unreachable and its VMs are listed | - | pass | pass |
| `list.not-counted-against-provider` | Required | an unreachable host in ListVMs never counts toward the manager's circuit breaker (the call succeeds) | - | pass | pass |
| `caps.clustering` | Required | a clustered provider reports supports_clustering, and the same capabilities on every call | - | pass | pass |
| `caps.advertised-rpcs-are-routed` | Required | an RPC the provider advertises is served and requires a host: the same otherwise valid request is refused InvalidArgument about the host when it names none, and is not Unimplemented or refused for its host when it names one | - | pass | pass |
| `errinfo.host-unavailable-wire-and-mapping` | Required | the wire carries google.rpc.ErrorInfo{reason: HOST_UNAVAILABLE, domain: provider.virtrigaud.io}, with metadata.host naming a known host that is unreachable; the manager maps it to a retryable HostUnavailable that is not NotFound, and keeps that host id | - | pass | pass |
| `errinfo.provider-level-unavailable` | Required | when the Provider itself cannot serve, its Unavailable is not host-scoped (no HOST_UNAVAILABLE), so the manager counts it as the Provider's failure | `provider-not-ready` | pass | skip |
| `idempotency.reads` | Required | Describe and ListVMs are read-only: repeated calls give the same answer and change nothing | - | pass | pass |
| `idempotency.power` | Recommended | a power operation repeated for a state the VM is already in succeeds (the manager re-sends after a lost answer) | - | pass | pass |
| `idempotency.delete` | Required | a repeated Delete is a success or NotFound (the VM is gone), never another error; and it never deletes a different VM that has since taken the name | - | pass | pass |
| `idempotency.host-unavailable` | Required | repeated calls to an unreachable host keep giving the same host-scoped answer and change nothing | - | pass | pass |
| `restart.state-lives-on-hosts` | Required | after the provider restarts, its VMs are found on their hosts unchanged, ListVMs lists them, other tenants' VMs are still refused, and the capabilities are the same | - | pass | pass |
| `restart.no-resurrection` | Required | a VM deleted before a restart is still gone after it: the provider has no memory that could bring it back | - | pass | pass |
| `restart.host-down-across-restart` | Required | a host that is unreachable when the provider restarts is reported unreachable, not dropped, and its VMs are found again when it returns | - | pass | pass |
| `restart.task-routing` | Required | after a restart, task references are routed against the same host registry as before: a host it fronts that is unreachable is the host-scoped Unavailable, and a host it does not front is NotFound | - | pass | pass |
| `restart.task-ref` | Required | a task reference issued before a restart is still routed afterwards: it names its host, and the provider keeps no task table that a restart could lose | `async-tasks` | pass | skip |
| `providerdown.outage-and-recovery` | Required | while the Provider is down a call fails as a retryable Provider-level Unavailable (not host-scoped, not NotFound); when it is back, its VMs answer as before | - | pass | pass |
| `hosts.inventory` | Required | ListHosts reports exactly the hosts the provider fronts, Ready; an unreachable host is still reported, NotReady, and GetHostInfo refuses an unknown host | `host-inventory` | pass | pass |
| `create.requires-routable-host` | Required | Create without a host id is InvalidArgument and with an unknown or unreachable host is the host-scoped Unavailable; no VM is made in either case, and once the host is back Create works | `create` | pass | pass |
| `create.unavailable-names-host` | Recommended | the HOST_UNAVAILABLE answer of a Create to a known host that is unreachable names that host in its ErrorInfo metadata, as the per-VM calls' answers do | `create` | pass | **recommended fail** |
| `create.routed-to-target-host` | Required | Create lands the VM on the host named by target_host_id and stamps the requester as its owner; the VM is then found there by that owner and by no other | `create` | pass | pass |
| `create.owner-rebind` | Required | a repeated Create by the same owner is an idempotent success (the same VM), and a Create by another owner for a taken name fails AlreadyExists and leaves the VM and its stamp alone | `create`, `create-owner-rebind` | pass | skip |

Clustered libvirt: 40 Required cases pass and 5 are skipped, each with a reason (no asynchronous
calls; no `VM_NOT_OWNED` reason, which the contract does not require of a clustered provider;
Create runs through a seam, so the owner rebind is left to the real-pipeline tests; no safe way to
make the Provider itself unready). 4 Recommended cases: 2 pass and 2 fail (recorded below).

## Optional features

| Feature | Meaning |
|---------|---------|
| `async-tasks` | Routed calls (Power, Delete) may return a task that `TaskStatus` later reports done. Clustered libvirt's routed calls are all synchronous. |
| `vm-not-owned-reason` | A refusal for a VM that is not the requester's carries the `VM_NOT_OWNED` reason. The contract requires only NotFound. |
| `host-inventory` | The Provider serves `ListHosts` and `GetHostInfo`. |
| `create` | `Create` can be driven through the gRPC server (and its VM observed). |
| `create-owner-rebind` | A repeated Create by the same owner is idempotent; another owner's Create of a taken name is `AlreadyExists`. |
| `provider-not-ready` | The Target can make the Provider itself, not one host, unable to serve. |
| `host-blackhole` | The Target can make a host accept connections and never answer (`HostBlackholer`). Clustered libvirt's adapter gives the host a real SSH host connection to a server that never completes the handshake, so the Provider's own bound on a host that does not answer is what ends the call. |
| `host-faults` | The Target can make a host fail an operation cleanly, or lose its connection during one (`HostFaulter`). |
| `host-removal` | The Target can remove a host from the Provider's registry while VMs are bound to it (`HostRemover`). |
| `unreadable-stamp` | `SeedSpec.Stamp` can make a VM's owner stamp unreadable or ambiguous. |

## Writing an adapter for another clustered provider

Implement `clustered.Target` (and, for the features you declare, `TaskController`,
`DialObserver`, `CreateTemplater`, `HostBlackholer`, `HostFaulter`, `HostRemover` or
`ProviderReadiness`) and run the suite from a test in your provider's package:

```go
func TestMyProvider_Conformance(t *testing.T) {
	clustered.Run(t, func(t clustered.T) clustered.Target { return newMyAdapter(t) })
}
```

No case is changed or copied. `Target.Server()` returns the provider's gRPC service exactly as its
binary serves it; build whatever fake host layer your provider already has underneath it, as
`internal/providers/libvirt/conformance_clustered_test.go` does with its fake `virsh`. Declare the
optional features you offer, and for each you do not, a reason (`Capabilities.Reasons`). A
`DialObserver` makes the "a refused host id is never dialed" checks real; without one the suite logs
that its coverage is reduced.

## The instrumented fake and the mutation test

[`clustered/fake`](clustered/fake/) is an in-memory clustered Provider that obeys every rule above
and can be told to break any one of them (`fake.WithViolations(...)`: 60 violations, such as
`AllowEmptyHost`, `IgnoreOwnerMutate`, `ListDropsUnreachable`, `TaskWrongHost`). In its
asynchronous mode its effects happen when the host finishes the task, as a real asynchronous
provider's do.

`TestEveryCaseCatchesItsSeededViolation` runs, for each violation, the cases that guard the broken
rule against a fake that breaks it, and requires each of them to **fail**, for some of them with a
pinned failure message. It also requires that every case is guarded by at least one violation and
that every violation is used. It shows that **every case is able to fail** for the reason it
states; it does not show that the cases are complete. When you add a case, add the violation that
breaks its rule (`fake/violation.go`, `fake/provider.go`) and its entry in `clustered/meta_test.go`.

## Findings recorded against clustered libvirt

Both are Recommended cases; neither is fixed by the change that added the suite. They are listed
in `libvirtExpectedFailures` with a pointer and a pinned message.

- `create.unavailable-names-host`: a routed `Create` answered `HOST_UNAVAILABLE` does not set the
  `host` metadata of its ErrorInfo (`contracts.HostUnavailableHostKey`), where `Describe`, `Delete`,
  `Power`, `Reconfigure` and `TaskStatus` do. The manager reads the key only on the Delete path, so
  nothing reads it for a Create today.
- `tasks.forged-inner-ref`: `TaskStatus` for a well-formed reference to a host it fronts, naming a
  task it never issued, answers `done`. Every routed libvirt call is synchronous, so it issues no
  task and cannot tell.

## Not covered

- The snapshot family, Clone, ExportDisk, ImportDisk and TransferOwner, beyond "an advertised RPC
  refuses a request that names no host".
- `ListVMs` with the owner filter, and the A6 previous-incarnation and disk guards.
- A draining host, and an excluded host: both are decided by the operator's `Host` objects, not by
  the Provider, and need no seam in the Provider's gRPC surface.
- The `OWNER_STAMP_UNREADABLE` reason: it is a single-host Proxmox refinement. For a clustered
  provider the contract is NotFound, which `owner.unreadable-stamp` checks.
- The real Create pipeline's owner rebind at the gRPC boundary (clustered libvirt runs Create through
  the `createOnHostFn` seam; the pipeline needs libvirtd).
- Hosts that fail over a real SSH session: libvirt's host faults are scripted in a fake `virsh`
  (a command that dies without an exit status, one that hangs, one that fails).
