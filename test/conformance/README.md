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
configuration the two `async-tasks` cases are skipped. The `identity.*` cases are listed
separately, under [Operation identity cases](#operation-identity-cases).

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
| `tasks.forged-inner-ref` | Required | a well-formed reference to a reachable host it fronts, but to a task that host never issued, is NotFound with the `UNKNOWN_TO_AGENT` ErrorInfo: never done, never "not performed"; the manager maps it to a NotFound marked `UNKNOWN_TO_AGENT` | - | pass | pass |
| `tasks.host-down-ref` | Required | TaskStatus for a well-formed reference naming a host the provider fronts but cannot reach is the retryable, host-scoped Unavailable that names the host (ErrorInfo metadata `host`): not NotFound, not a task result | - | pass | pass |
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

Clustered libvirt: 41 Required cases pass and 5 are skipped, each with a reason (no asynchronous
calls; no `VM_NOT_OWNED` reason, which the contract does not require of a clustered provider;
Create runs through a seam, so the owner rebind is left to the real-pipeline tests; no safe way to
make the Provider itself unready). 3 Recommended cases: 2 pass and 1 fails (recorded below).
The 25 `identity.*` cases are skipped for clustered libvirt and for the default fake: neither
offers `operation-identity`.

## Operation identity cases

A Provider that reports `supports_operation_identity` accepts a mutating call carried in an
`operation_envelope`: the request's exact bytes and a signed grant that binds them
([`docs/operation-grants-wire-shape.md`](../../docs/operation-grants-wire-shape.md)). Its hosts
verify the grant before they act and execute each operation (each `op_id`) at most once. No
built-in provider reports it, and only a development build of the manager can sign a grant.

The `identity.*` cases run only against a Target that declares the `operation-identity` feature and
implements `clustered.OperationIdentityTarget`, which hands the suite a signer whose grants chain to
the anchor the Provider's hosts pinned, and the Provider, enrolment and epoch values the hosts
expect. For every other Target they are skipped and listed as not applicable, so the results of the
cases above do not change. The suite mints grants with `internal/operation` and sends them on the
raw gRPC client, so it can also send grants that are forged, re-signed with other fields, bound
elsewhere or paired with other request bytes.

| Case | Level | Rule | Needs | identity fake | per-host agent |
|------|-------|------|-------|---------------|----------------|
| `identity.capabilities` | Required | a Provider whose hosts verify operation grants reports supports_operation_identity, and reports requires_operation_identity exactly when it refuses a mutating call that carries no grant | `operation-identity` | pass | pass |
| `identity.unsigned-refused` | Required | a Provider that reports requires_operation_identity refuses a mutating call with no operation_envelope with IDENTITY_REQUIRED, and the VM is untouched | `operation-identity`, `operation-identity-required` | pass | pass |
| `identity.signed-power` | Required | a correctly signed Power is routed by the request inside the envelope, performed on that host, and its task reports TASK_OUTCOME_SUCCEEDED once the host finished it | `operation-identity` | pass | pass |
| `identity.signed-create-delete` | Required | a correctly signed Create makes the VM on the host its request names, stamped with its owner, and a correctly signed Delete removes it | `operation-identity`, `create` | pass | pass |
| `identity.forged-grant-refused` | Required | a grant whose signature or certificate chain does not verify to the anchor the host pinned is refused with GRANT_UNAUTHENTICATED, and nothing is performed | `operation-identity` | pass | pass |
| `identity.wrong-kind-refused` | Required | an authentic grant whose operation kind does not fit the call (another PowerRequest.op, including REBOOT against SHUTDOWN_GRACEFUL, or another RPC) is refused with GRANT_MISMATCH, and nothing is performed | `operation-identity` | pass | pass |
| `identity.outer-fields-refused` | Required | a request that sets any field beside operation_envelope is refused with INVALID_ARGUMENT, and nothing is performed | `operation-identity` | pass | pass |
| `identity.digest-bound` | Required | request bytes that are not the ones the grant's intent_digest covers are refused with GRANT_MISMATCH, and neither request is performed | `operation-identity` | pass | pass |
| `identity.binding-refused` | Required | an authentic grant for another Provider, another enrolment or another host than the one the request names is refused, and nothing is performed: with GRANT_MISMATCH, except that a host that keeps durable operation records may refuse an enrolment it holds no record of with UNKNOWN_TO_AGENT, which is never definite | `operation-identity` | pass | pass |
| `identity.duplicate-op-id-once` | Required | a re-delivery of an op_id the host accepted, even after the operation is over, is answered as the first delivery was and never performed again: a re-sent Delete does not delete a VM made after it | `operation-identity` | pass | pass |
| `identity.duplicate-after-restart` | Required | the host's record of an accepted op_id survives a restart of the Provider: a re-delivery after the restart is never performed again | `operation-identity` | pass | pass |
| `identity.op-id-conflict` | Required | a known op_id that arrives with other identity fields (another attempt, another intent_digest, another VM) is refused with OP_ID_CONFLICT, and nothing is performed | `operation-identity` | pass | pass |
| `identity.vm-busy` | Required | while an operation on a VM is not over, a new operation (a new op_id) for the same VM is refused with VM_BUSY; once the first is over the same attempt is accepted | `operation-identity`, `async-tasks` | pass | pass |
| `identity.mint-epoch-first-delivery` | Recommended | the first delivery of an op_id minted before the host's enrolment started is refused, and nothing is performed: the host cannot know what an earlier enrolment did with it | `operation-identity` | pass | pass |
| `identity.outcome-retired` | Required | a task that never started reports TASK_OUTCOME_RETIRED with done=true and a "not performed" error, the manager reads it as not performed, and it had no effect | `operation-identity`, `scripted-outcomes`, `async-tasks` | pass | expected failure |
| `identity.outcome-unknown` | Required | a task whose outcome the host cannot tell reports TASK_OUTCOME_UNKNOWN and is never done, and the VM stays busy for a new operation | `operation-identity`, `scripted-outcomes`, `async-tasks` | pass | pass |
| `identity.outcome-failed-dirty` | Required | a task that failed after an irreversible step reports TASK_OUTCOME_FAILED_DIRTY with done=true and an error, and the manager reads it as a failure, never as not performed | `operation-identity`, `scripted-outcomes`, `async-tasks` | pass | expected failure |
| `identity.tombstone-after-restart` | Required | a host's record of a finished op_id survives a crash of its agent: a re-delivery after the restart is answered as the first one was and never performed again, and a signed TaskStatus names it as tombstoned | `operation-identity`, `host-answers` | pass | pass |
| `identity.taskstatus-bound` | Required | a signed TaskStatus answers for exactly the op_id asked about (its kind, attempt, VM and binding), and an op_id the host never saw is NO_RECORD, NotFound with UNKNOWN_TO_AGENT, never done | `operation-identity`, `host-answers` | pass | pass |
| `identity.deleted-answer` | Required | a signed Describe answers DELETED, naming the Delete, only after that placement's Delete completed; a placement the host never had is NO_RECORD, never DELETED | `operation-identity`, `host-answers` | pass | pass |
| `identity.answer-fresh` | Required | every signed answer echoes the nonce of the request it answers, so an answer replayed for another request is detected | `operation-identity`, `host-answers` | pass | pass |
| `identity.attempt-regression` | Required | a new op_id whose attempt is not above an attempt the host already accepted for the VM and kind is refused with OP_ID_CONFLICT, and nothing is performed | `operation-identity`, `host-answers` | pass | pass |
| `identity.inner-unknown-field` | Required | an authentic grant over request bytes that carry a field the host does not know is refused with GRANT_MISMATCH, and nothing is performed | `operation-identity`, `host-answers` | pass | pass |
| `identity.store-quarantined` | Required | a host whose operation store was rolled back refuses every mutation with QUARANTINED, never as definitely not performed, performs nothing, and still answers signed reads with its quarantine | `operation-identity`, `host-answers` | pass | pass |
| `identity.no-running-forever` | Required | an operation whose step never ends on its own leaves the running state by the host's step deadline: it ends, or its outcome is unknown, and it is never reported running past it | `operation-identity`, `host-answers`, `async-tasks` | pass | pass |

The identity fake is the instrumented fake made with `fake.NewIdentity`, with a test-only signer
that exists only in the suite's test binary. It is run three ways: accepting unsigned calls too,
asynchronous and synchronous, against the whole suite (every case above passes; the synchronous run
skips the `async-tasks` cases, and both skip `identity.unsigned-refused`); and requiring a grant on
every mutating call, against the `identity.*` cases only (`clustered.OnlyCases("identity.")`),
because such a Provider refuses the unsigned calls of the other groups by design. A development
build (`go test -tags virtrigaud_dev ./test/conformance/clustered/`) also runs the `identity.*`
cases with the manager's development signer in place of the test-only one. The Recommended
`identity.mint-epoch-first-delivery` rule is not yet written in `provider.proto`, which is why it is
Recommended.

What the identity cases do not cover: the snapshot family, Clone, the disk calls and the other
mutating RPCs under a grant; `expect_known`; epochs beyond the identity-only values (no ordering,
no fencing); and host-signed receipts. The fake keeps its operation records in memory, on its
hosts, so they survive a restart of the Provider but are not durable.

### Host answers and the per-host agent target

The eight cases that need `host-answers` check what a per-host agent answers about itself: signed
`TaskStatus` and `Describe` answers (`OperationAnswer`, `VMAnswer` in
`proto/provider/v1/host_answer.proto`) that name exactly what was asked, echo the request's nonce
and verify against the issuer of the hosts' identity certificates (`HostAnswerTarget`); records
that survive a crash of the agent; refusals of a rolled-back store; and the step deadline. They
verify each answer as a manager would: body type, the signer's chain, the signature over the
domain-separated body, no unknown field, the nonce and the host. A Target that tracks VM
placements (`PlacementTracker`) makes `NewOp` use each VM's placement binding and the next attempt
per VM and kind, as a manager does; the identity fake and the agent target both do. These cases
run only on a Target that declares `host-answers`; for every other Target they are skipped and
listed as not applicable.

[`clustered/agenttarget`](clustered/agenttarget/) (development builds only:
`go test -tags virtrigaud_dev ./test/conformance/clustered/agenttarget/`) runs every `identity.*`
case against real per-host agent processes: two hosts, each a separate process of the test binary
that runs the agent's start-up sequence, operation store, mutual TLS server and the development
fake executor, behind a thin in-test router that routes and forwards only. The agent's results
are the last column above. Two cases are recorded as expected failures of that target, not of the
agent: a power operation cannot reach `RETIRED` (only a re-enrolment freeze produces it) or
`FAILED_DIRTY` (only an irreversible step does) at the agent, so the target cannot script them. A
per-host agent keys a VM by its owner's UID: two VMs of one owner on one host are one VM to it, and
the target maps each seeded VM's name to that UID. The same package holds the crash-point tests:
the agent process is killed with SIGKILL at each crash point of its operation path, restarted, and
the operation is shown to end with its once-only effect performed exactly once.

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
| `operation-identity` | The Provider reports `supports_operation_identity` and its hosts verify operation grants (`OperationIdentityTarget`). Needed by every `identity.*` case. |
| `operation-identity-required` | The Provider also reports `requires_operation_identity` and refuses a mutating call with no grant. |
| `scripted-outcomes` | The Target can make the next operation a host accepts end with a chosen typed outcome (`OutcomeScripter`). |
| `host-answers` | The Target's hosts answer a `TaskStatus` or `Describe` that carries `answer_nonce` with a host-signed answer, keep their records across a crash of the agent, and can be crashed, rolled back and made to stall a step (`HostAnswerTarget`). |

## Writing an adapter for another clustered provider

Implement `clustered.Target` (and, for the features you declare, `TaskController`,
`DialObserver`, `CreateTemplater`, `HostBlackholer`, `HostFaulter`, `HostRemover`,
`ProviderReadiness`, `OperationIdentityTarget`, `OutcomeScripter`, `PlacementTracker` or
`HostAnswerTarget`) and run the suite from a test in your provider's package:

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
and can be told to break any one of them (`fake.WithViolations(...)`: 86 violations, such as
`AllowEmptyHost`, `IgnoreOwnerMutate`, `ListDropsUnreachable`, `TaskWrongHost`). In its
asynchronous mode its effects happen when the host finishes the task, as a real asynchronous
provider's do. 25 of the violations need its operation-identity mode (`fake.IdentityViolations()`),
such as `AcceptsUnsigned`, `AcceptsWrongKindGrant`, `IgnoresDigest`, `ExecutesDuplicateOpID` and
`DropsTombstones`; the mutation test runs them against the identity fake that requires a grant. In
that mode the fake uses the shared verifier (`internal/operation`), the one a per-host agent is to
use, rather than verification of its own; a violation makes it act on a call the verifier refused
under one rule.

`TestEveryCaseCatchesItsSeededViolation` runs, for each violation, the cases that guard the broken
rule against a fake that breaks it, and requires each of them to **fail**, for some of them with a
pinned failure message. It also requires that every case is guarded by at least one violation and
that every violation is used. It shows that **every case is able to fail** for the reason it
states; it does not show that the cases are complete. When you add a case, add the violation that
breaks its rule (`fake/violation.go`, `fake/provider.go`) and its entry in `clustered/meta_test.go`.

## Findings recorded against clustered libvirt

A Recommended case, not fixed by the change that added the suite. It is listed in
`libvirtExpectedFailures` with a pointer and a pinned message.

- `create.unavailable-names-host`: a routed `Create` answered `HOST_UNAVAILABLE` does not set the
  `host` metadata of its ErrorInfo (`contracts.HostUnavailableHostKey`), where `Describe`, `Delete`,
  `Power`, `Reconfigure` and `TaskStatus` do. The manager reads the key only on the Delete path, so
  nothing reads it for a Create today.

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
