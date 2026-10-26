# Command-line tools

VirtRigaud ships four small command-line programs next to the operator. They are separate from
the operator: they talk to the cluster with your kubeconfig (or, for `vrtg-provider`, only edit
local files), and nothing in the manager or in a provider image depends on them.

| Program | What it is for |
|---------|----------------|
| [`vrtg`](#vrtg) | Read VirtRigaud resources: list and describe `VirtualMachine`s, list `Provider`s |
| [`vcts`](#vcts) | Validate conformance test specifications; run the conformance suite against a `Provider` |
| [`virtrigaud-loadgen`](#virtrigaud-loadgen) | Create, power, resize, snapshot, clone and delete `VirtualMachine`s at a steady rate and record how long the API calls (and, optionally, the provider) take |
| [`vrtg-provider`](#vrtg-provider) | Scaffold, check and catalog a new out-of-tree provider |

Each release publishes them as `<program>-<os>-<arch>` assets (`linux`, `darwin` and `windows`,
each for `amd64` and `arm64`; `.exe` on Windows). From a checkout, build one with
`go build -o bin/vrtg ./cmd/vrtg`. `<program> --version` prints the release it was built from
(`dev` for a source build).

`vrtg`, `vcts` and `virtrigaud-loadgen` share two flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--kubeconfig` | none | Path of the kubeconfig to use. Without it the usual resolution applies: `KUBECONFIG`, the in-cluster configuration, then `~/.kube/config`. A path that cannot be loaded is an error; the tool does not fall back to another cluster. |
| `-n`, `--namespace` | `default` | Namespace to read from or create in |

The tools need the VirtRigaud CRDs installed in the cluster. `vrtg` only reads (get, list).
`vcts run` and `virtrigaud-loadgen` need get, list, create, patch and delete on `VirtualMachine`s,
`VMSnapshot`s and `VMClone`s in the namespace (`vcts run` also on `VMClass`es and whatever other
VirtRigaud kinds a specification creates) and get on the `Provider` (`vcts run`); `vcts validate`
and `vcts list` need no cluster, nor does `virtrigaud-loadgen --dry-run`.

## vrtg

```
vrtg vm list                          # VirtualMachines
vrtg vm describe <name>               # one VirtualMachine
vrtg vm console-url <name>            # status.consoleURL, if the provider reports one
vrtg vm events <name>                 # events whose involved object is the VirtualMachine
vrtg provider list                    # Providers
vrtg provider status <name>           # one Provider: type, endpoint, status.healthy, conditions
```

```
$ vrtg vm list
NAMESPACE   NAME    PROVIDER      CLASS   IMAGE       IPS      AGE
default     web-1   libvirt-lab   small   ubuntu-22   <none>   13s
```

- **Namespaces.** The `list` commands show every namespace, with a `NAMESPACE` column, unless you
  pass `-n`. `describe`, `console-url`, `events` and `provider status` read the namespace given by
  `-n` (`default` if omitted).
- **Output.** `-o table` (default), `-o json` or `-o yaml` for `vm list`, `vm describe`,
  `provider list` and `provider status`. JSON and YAML carry `apiVersion` and `kind` and leave out
  `metadata.managedFields`. `--timeout` (default `30s`) bounds each request.
- **A VM with no `spec.imageRef`** (one that boots from an imported disk) shows `<none>` as its image.
- **`provider status`** prints `status.healthy` and the conditions; for everything else use
  `kubectl describe provider <name>`.

These subcommands exist in the command tree but are **not implemented** in this release. They exit
non-zero with an error that names the alternative: `vrtg provider logs` (use `kubectl logs`),
`vrtg snapshot create|list|revert` (use `VMSnapshot` resources and `spec.snapshot.revertToRef`),
`vrtg clone run|list` (use `VMClone`), `vrtg conformance run` (use [`vcts`](#vcts)),
`vrtg diag bundle` and `vrtg init` (use the Helm chart).

## vcts

```
vcts validate [spec-dir]                                     # default spec-dir: test/conformance/specs
vcts list [--spec-dir <dir>]
vcts run --provider <name> [-n <namespace>] --set IMAGE=<vmimage> [--set NAME=value ...]
         [--spec-dir <dir>] [--output-dir ./conformance-results] [--skip <test>,...]
         [--timeout 3h] [--parallel 1] [--keep] [--cleanup-timeout 10m] [-v]
```

- **`vcts validate`** checks every `*.yaml` file in the directory and exits **non-zero** when a
  file cannot be parsed or holds an invalid test (a bad test name, a missing description or step,
  an unknown step type, key, kind, capability or operator, a malformed path or variable, ...). It
  exits 0 only when every file is valid. The CI workflow uses it on `test/conformance/specs`.
- **`vcts list`** prints each test with its required capabilities and the variables it needs
  from `--set`.
- `--spec-dir` (default `test/conformance/specs`) is relative to the current directory, so run
  `vcts` from the root of a VirtRigaud checkout or pass the path.

### vcts run

`vcts run` runs the tests against a `Provider` that is deployed and healthy in the namespace given
by `-n`. Each test creates its objects in that namespace, waits for the operator and the provider to
act on them, checks their status, then deletes everything it created. The specification format is
described in [`test/conformance/README.md`](../test/conformance/README.md); the shipped tests are:

| Test | Requires | What it checks |
|------|----------|----------------|
| `vm-create-delete` | `vm-create`, `vm-delete` | A VM becomes `Ready` and powered on, with `status.id`, phase `Running`, `status.boundProvider` and `status.currentResources`; deleting it removes it |
| `vm-power-cycle` | `vm-create`, `vm-power`, `vm-delete` | `spec.powerState` Off then On is reflected in `status.powerState` and `status.phase` |
| `vm-reconfigure-offline` | `vm-create`, `vm-reconfigure`, `vm-delete` | A powered-off VM's `spec.resources` change is recorded in `status.currentResources` |
| `vm-snapshot-create-delete` | `vm-create`, `vm-snapshot`, `vm-delete` | A `VMSnapshot` of a powered-off VM becomes `Ready` with a `status.snapshotID`; deleting it removes it |
| `vm-clone-full` | `vm-create`, `vm-clone`, `vm-delete` | A full `VMClone` of a powered-off VM becomes `Ready` and its target VM is bound to a provider VM |

```
vcts run --provider libvirt-lab -n vcts --set IMAGE=ubuntu-22 -v
```

- **Variables.** Specifications refer to `${RUN}` (the run id: six lower-case letters and digits),
  `${TEST}`, `${PROVIDER}` and `${NAMESPACE}`, which `vcts` sets, and to variables you pass with
  `--set NAME=value`. The shipped tests need `--set IMAGE=<name>`: an existing `VMImage` in the
  namespace that the provider can boot. A test to run that refers to a variable you did not set
  stops the run before anything is created.
- **Names and labels.** Every object a test creates has `${RUN}` in its name, so runs never
  collide, and carries the labels `vcts.virtrigaud.io/run=<run id>`,
  `vcts.virtrigaud.io/test=<test>` and `app.kubernetes.io/managed-by=vcts`. The run id is printed
  and recorded in the results.
- **Only its own objects.** An `update` or `delete` step acts only on an object that carries the
  run's label; anything else in the namespace is never changed or deleted. Specifications cannot
  create, change or delete a `Provider`.
- **Capabilities.** A test lists `requiredCapabilities`. `vm-create`, `vm-delete`, `vm-power`,
  `vm-reconfigure` (a powered-off VM) and `vm-clone` (a full clone) are part of the provider
  contract and always offered. The others come from the Provider's
  `status.reportedCapabilities`: `vm-snapshot` (`supportsSnapshots`), `vm-snapshot-memory`
  (`supportsMemorySnapshots`), `vm-reconfigure-online` (`supportsReconfigureOnline`),
  `vm-clone-linked` (`supportsLinkedClones`), `image-import` (`supportsImageImport`),
  `disk-export` (`supportsDiskExport`) and `disk-import` (`supportsDiskImport`). A test needing a
  capability the Provider does not offer is reported as **skipped**, with the reason, not failed.
  So is a test named with `--skip`.
- **Timeouts.** `--timeout` (default 3h) bounds the whole run: a test still running when it ends
  fails, and a test not started yet counts as **failed** ("not run"), so an interrupted or
  timed-out run never exits 0. A test's `timeout` (default 15m) bounds its steps; a wait step's
  `waitFor.timeout` or `timeout` (default 5m) bounds the wait; other steps default to 1m. Waiting
  polls every 2 seconds. A wait (for anything but `Failed` or `Deleted`) fails when the object's
  `status.phase` has stayed `Failed` for 30 seconds; the operator retries some failed phases, such
  as a create in backoff.
- **Cleanup.** After its steps, whether they passed or not, each test runs its `cleanup` steps,
  then deletes the objects labelled with its run and test that it did not create itself (such as a
  clone's target VM), then the objects it created, newest first, and waits until each is gone.
  A `VMClone` whose provider clone may still be in flight is waited for first; one that does not
  settle is left in place with its source and target VMs (deleting them could leave the cloned VM
  on the hypervisor with nothing recording it) and the test fails. Cleanup also runs after
  `--timeout` or an interrupt (Ctrl-C; a second Ctrl-C exits at once), bounded by
  `--cleanup-timeout` per test; objects reached after that time still get their delete call, but
  are not waited for. A cleanup that fails fails the test. `--keep` skips the
  cleanup and lists what was left; remove it with
  `kubectl delete virtualmachines,vmsnapshots,vmclones,vmclasses -n <namespace> -l vcts.virtrigaud.io/run=<run id>`.
- **Results.** `results.json`, `junit.xml` (one test case per test, the step results in
  `system-out`) and `report.md` (a summary, and each test's steps and cleanup) in `--output-dir`,
  with passed, failed or skipped per test and per step. `-v` prints each step as it finishes.
  `vcts run` exits **non-zero** when a test fails.
- `--parallel N` runs up to N tests at once.

## virtrigaud-loadgen

```
virtrigaud-loadgen run [--config loadtest.yaml] [-n <namespace>] [--output-dir ./loadgen-results]
                       [--observe] [--observe-timeout 5m] [--cleanup] [--cleanup-timeout 10m]
                       [--dry-run] [--verbose]
```

Each worker performs one randomly chosen operation per second for the configured duration. Without
`--config` the built-in defaults apply: 5 minutes, 2 workers, at most 10 VMs alive at once, VMs
created against a provider named `test-provider` with class `test-class` and image `test-image`.
`--config` reads a YAML file that overrides the defaults; durations are Go duration strings (`30s`,
`5m`).

> **Size the run's storage before you start it.** `create` and `clone` make real VMs on the
> hypervisor, and on providers that copy disks (a Proxmox full clone, a libvirt volume copy) each
> one can occupy the whole size of its disk. A run without a cap fills the datastore (an earlier
> release, which accepted `vmCount` but ignored it, filled a small Proxmox thin pool to 100%). The
> run therefore keeps at most **`vmCount`** VMs alive at once (default 10), and the worst case is
> **`vmCount` x the disk size of the image or source VM**, plus snapshots, which `vmCount` does not
> limit. Pick `vmCount` so that figure fits in the datastore with room to spare, and run with
> `--cleanup` so the VMs do not outlive the run.

```yaml
duration: 2m
concurrency: 4
providers: [libvirt-lab]          # a Provider in -n; one is chosen at random per create
vmCount: 6                        # the most VMs the run has alive at once (default 10, at least 1)
vmTemplate:
  classRef: small                 # an existing VMClass
  imageRef: ubuntu-22             # an existing VMImage
  labels:
    team: perf
  resources:                      # optional; used by reconfigure only
    cpuRange: [1, 4]              # vCPUs (1..128); default: alternate between 1 and 2
    memoryRange: [1024, 4096]     # MiB (128..1048576); default: memory is not changed
operations:                       # relative weights, not percentages; at least one must be above 0
  create: 30
  delete: 10
  describe: 20
  power: 15
  reconfigure: 10
  snapshot: 10
  clone: 5
```

`test/performance/proxmox-loadtest.yaml` is a ready configuration in this format for a Provider
named `proxmox-perf` (edit the provider, class and image names for your cluster); a unit test loads
every YAML file in `test/performance` with the tool's own loader. The tool has no scenarios or
pass/fail thresholds: read `summary.md` and compare it with your own targets.

An unknown key, an unparsable duration or an unusable value (no providers, all weights zero,
`vmCount` below 1, a range that is not `[min, max]` within the bounds, ...) is an error and nothing
is created. The keys `rampUpTime`, `steadyState`, `rampDownTime`, the `*Interval` keys and
`vmTemplate.annotations` are accepted but not used in this release (the run starts all workers at
once, runs for `duration` and uses `operations` for the mix); the command prints a warning when a
file sets one of them.

**`vmCount`: the cap on VMs.** It is the most VMs the run has alive at once: the VMs it created and
has not deleted, the target VMs of the clones it requested, and the VMs whose delete it sent that
are not yet gone (they still hold their storage). A `create` or `clone` that would go beyond it is
**skipped**: no create or clone call is made (when the cap is reached the tool reads, at most once
per second, the VMs it deleted to see which are gone), and the operation is recorded as skipped
(not as a failure, and not in the totals of operations performed, success rates or latencies).
VMs that are not the run's, and snapshots, do not count.

A clone's target counts from the moment the `VMClone` is accepted. When the `VMClone` is `Ready`
and its target VM exists and carries the run's label, the target becomes one of the run's VMs:
`power`, `reconfigure`, `snapshot`, `clone` and `delete` can act on it, so `delete` frees its slot.
When the `VMClone` fails or disappears, or its target is being deleted, the slot stays taken for as
long as the target VM exists (the operator removes the target of a failed clone only later, and
keeps it in some cases, for example when it was bound); it is freed when the VM is gone, at once when there is none. A `Ready`
clone whose target does not carry the run's label frees its slot. The tool looks at the clones
when a `delete` runs (at most once per second, before the delete is timed), so a mix without
`delete` keeps every clone target counted.
Deleting frees room again; a run with `create` weighted above `delete` therefore reaches `vmCount`
quickly and then mostly skips creates, and you raise the `delete` weight if you want churn at a
steady VM count.

**What each operation does.** Every operation but `create` and `describe` acts on a VM **this run
created**, chosen at random among those no other operation is using; it never touches another VM in
the namespace. Patches and deletes carry the VM's UID, so they never reach another object created
later under the same name. `delete` never takes the source of a `VMClone` that has not ended
(is not `Ready`, `Failed` or gone): deleting it while the provider copies its disk could leave a VM on the
hypervisor that nothing records. With no such VM available the operation is recorded as failed
(`no VM created by this run is available`) and nothing is called.

| Operation | API call |
|-----------|----------|
| `create` | Creates a `VirtualMachine` `loadgen-vm-<run>-<worker>-<n>` (`spec.powerState: On`) from the template, with the labels `generated-by: loadgen` and `loadgen.virtrigaud.io/run: <run>` plus your template labels |
| `delete` | Deletes a VM of the run (its `VMSnapshot`s follow it, see below) |
| `describe` | Lists the `VirtualMachine`s in the namespace |
| `power` | Patches `spec.powerState` of a VM of the run to `Off`, or back to `On` |
| `reconfigure` | Patches `spec.resources.cpu` (and `spec.resources.memoryMiB` when `memoryRange` is set) of a VM of the run to a value from the range that differs from the one the run last set: the change the VirtualMachine controller sends to the provider as a Reconfigure |
| `snapshot` | Creates a `VMSnapshot` `loadgen-snap-<run>-<n>` of a VM of the run with `retentionPolicy.deleteOnVMDelete: true`, so the snapshot is deleted when its VM is |
| `clone` | Creates a `VMClone` `loadgen-clone-<run>-<n>` (full clone, target powered off) of a VM of the run, preferring one the run powered off (some providers, libvirt among them, hold a clone of a running source until it is powered off); its target VM, `loadgen-vm-<run>-clone-<n>`, is created by the operator with the run's labels |

**How long a run takes.** The workers start no operation once `duration` has passed. An operation
that is already in flight at that moment finishes, and so does its `--observe` wait (up to
`--observe-timeout`, 5 minutes by default), so a run lasts `duration` plus at most the longest
operation or observation still in flight, plus the `--cleanup`. An interrupt stops the wait.

**What is measured.** `Duration` is the time of the API call alone: from sending the request until
the apiserver answered (accepted or refused it). It does not include the provider's work. The
client-side rate limit of the Kubernetes client is disabled, so the time is the apiserver's, not
time spent queued in the tool. With **`--observe`**, after each accepted request the worker also
polls the object (every second, up to `--observe-timeout`) until its status reflects the request,
and records `ObservedDuration`, from sending the request until then:

| Operation | Observed when |
|-----------|---------------|
| `create` | `status.id` is set and `status.powerState` is `On` |
| `delete` | the VM is gone |
| `power` | `status.powerState` equals the requested state |
| `reconfigure` | `status.currentResources` holds the requested CPU (and memory), or the VM reports `Reconfiguring=True`, reason `RestartRequired`, for the patched generation (a change applied to the next boot only, such as a running VM's shrink, which `status.currentResources` does not show) |
| `snapshot` | the `VMSnapshot`'s `status.phase` is `Ready` |
| `clone` | the `VMClone`'s `status.phase` is `Ready` |

A VM, snapshot or clone that reaches phase `Failed`, or one not observed in time, is recorded in
`ObserveError` (the API call still counts as successful). A worker waits for the observation before
its next operation, so `--observe` lowers the request rate. Observation goes on past the end of the
run's duration for operations still in flight; an interrupt stops it.

- **Cleanup.** By default everything the run created is **left in place**, and the command prints
  how to remove it:
  `kubectl delete vmsnapshots,vmclones,virtualmachines -n <namespace> -l loadgen.virtrigaud.io/run=<run>`.
  With **`--cleanup`** the tool deletes, at the end (also after an interrupt), the run's
  `VMSnapshot`s and waits until they are gone, then its `VMClone`s, the clone target VMs that carry
  the run's label, its VMs and the VMs whose delete it issued during the run, and waits until they
  are gone, up to `--cleanup-timeout`. It also deletes any object carrying the run's label
  (`loadgen.virtrigaud.io/run`) that the tool did not record, which happens when a create's answer
  was lost after the apiserver stored the object. A `VMClone` is only an operation record: deleting it leaves
  its target VM, which is why the targets are deleted as well. A `VMClone` whose provider clone may
  still be in flight is waited for first; one that does not settle in time is left in place with
  its source and target VMs. The tool exits non-zero, naming what is left, when anything remains.
  The cleanup waits for clones to settle because deleting a `VMClone`, its target or its source
  while the provider is still copying the disk can leave a VM on the hypervisor that no
  `VirtualMachine` records, and the tool cannot see it. Deleting the target VM of a clone in
  flight makes the `VMClone` fail with a message naming the cloned VM left on the provider;
  deleting the `VMClone` itself only drops it (the manager logs `Deleting VMClone`). If you
  delete the objects of a run yourself, with `kubectl`, while clones are still in progress, check
  the hypervisor for VMs you did not expect afterwards.
- **Exit status.** `0` when the run reached the end of its duration and, with `--cleanup`, the
  cleanup completed. **Non-zero** when the run was interrupted (`SIGINT` or `SIGTERM`), like an
  interrupted `vcts run`: the results are saved and the `--cleanup` runs first (the output says
  `Cleanup complete` when it left nothing behind). A script can therefore tell an interrupted run
  from a finished one by the status alone. Non-zero also when the cleanup left objects behind or
  the configuration was unusable. A signal that arrives while the cleanup is running does not stop
  it (send a second one to terminate the process at once), and does not change the status.
- **`--dry-run`** makes no API call at all (it needs no kubeconfig): operations are recorded as
  successful with phase `DryRun`, and nothing is observed or cleaned up.
- **Results.** `results.csv` (one row per operation: `Operation`, `Provider`, `VMName`,
  `StartTime`, `Duration`, `Success`, `Error`, `Phase` — what was requested — and, with
  `--observe`, `ObservedDuration` and `ObserveError`, then `Skipped`: `true` for a create or clone
  skipped because of `vmCount`, which is also `Success=false` with no `Error`) and `summary.md`
  (counts, the number of skipped operations, and p50/p95/p99/max of the API call latency and, with
  `--observe`, of the observed time, per operation) in `--output-dir`. Earlier releases wrote the
  CSV without the `Skipped` column.

## vrtg-provider

`vrtg-provider` helps someone writing a provider outside this repository.

```
vrtg-provider init <name> [--type generic|vsphere|libvirt|proxmox|firecracker|qemu] [--remote] [--output <dir>] [--virtrigaud-path <checkout>] [--force]
vrtg-provider generate [--proto-only] [--clean]    # run inside the project
vrtg-provider verify [--skip-build] [--skip-tests] [--skip-conformance]
vrtg-provider publish --image <repo> --repo <url> --maintainer <email> [--name <n>] [--tag <t>] [--catalog <file>] [--dry-run] [--skip-verify]
vrtg-provider version
```

**`init`** creates `<output>/providers/<name>` (the name is lowercase letters, digits and hyphens):
`main.go`, `internal/provider/` (a provider whose operations all return `Unimplemented`, its
capabilities and a test), `go.mod`, `Makefile`, `Dockerfile`, `README.md`, a CI workflow and, with
`--remote`, Kubernetes manifests. The generated Go files are `gofmt`-clean.

`--type` (default `generic`) picks the platform the scaffold is written for: its README, the
credentials its remote Deployment reads and the capabilities it declares. The `proxmox` type declares
only the core profile and the `raw`/`qcow2` disk and `bridge`/`vlan` network types, and its test
asserts that no optional capability (snapshots, linked clones, online reconfigure) is advertised; add
each one as you implement the operation behind it. The `vsphere`, `libvirt` and `qemu` types still
declare optional capabilities (snapshots, and for `vsphere` and `libvirt` also linked clones) although
every operation of a fresh scaffold returns `Unimplemented`; the capabilities a scaffold advertises are
the ones `New()` builds in `internal/provider/provider.go`, so review them before you deploy one.

The SDK (`github.com/projectbeskar/virtrigaud/sdk`) and the protobuf bindings
(`github.com/projectbeskar/virtrigaud/proto`) are separate Go modules. From v0.4.1 on, every
release publishes them under their own tags (`sdk/vX.Y.Z`, `proto/vX.Y.Z`; see
[`docs/go-modules.md`](go-modules.md)). Where the generated `go.mod` gets them from depends on the
`vrtg-provider` binary and its flags:

- **A release binary** (downloaded from a GitHub release; `vrtg-provider version` prints a version
  such as `v0.4.1`) requires the VirtRigaud, SDK and proto modules at its own version, with no
  `replace` directives, also when the project is created inside a checkout. The `go` directive is
  the Go version the binary was built with, which for a release is the one the repository's
  `go.mod` names.
- **`--virtrigaud-path <checkout>`** overrides that, for local development: `go.mod` gets `replace`
  directives that resolve the three modules from the checkout (an absolute path, or a relative one
  when the project is inside the checkout), and the checkout's `go` directive.
- **A development build** (built from a checkout without a release version, for example with
  `go build ./cmd/vrtg-provider`) finds the checkout the project is created in and writes the
  `replace` directives with a relative path (for example `=> ../../sdk` for
  `<checkout>/providers/<name>`). Outside a checkout, `go.mod` requires placeholder versions
  (`v0.0.0`) and leaves a commented `replace` block: set the three versions to a release, or fill in
  the `replace` block, or pass `--virtrigaud-path`.

Then, in the project directory:

```
go mod tidy        # first time only: resolves the modules and writes go.sum
make build         # bin/provider-<name>
make test
vrtg-provider verify
```

- **`generate`** runs the project's `proto` make target (or `buf generate` in `proto/`) if it has one,
  then `go generate ./...` and the `generate` and `deepcopy` make targets if the Makefile defines
  them. A scaffolded project imports the generated bindings from the proto module, so there is
  nothing to generate and the command says so. `--clean` removes `*.pb.go` files under
  `internal/rpc`.
- **`verify`** runs the project's `build` and `test` make targets (or `go build` and `go test`),
  and reports `gofmt`, `go vet` and, when installed, `golangci-lint` findings without failing. It
  does **not** run the conformance suite, which needs the provider deployed in a cluster: deploy it,
  then run `vcts run --provider <name> --set IMAGE=<vmimage>`.
- **`publish`** builds and tests the project, then adds or updates its entry in a catalog file
  (default: `providers/catalog.yaml`, found by walking up from the current directory). It records
  the conformance profile `core` as `skip` (not run) and never claims a pass. `--dry-run` prints the
  entry without writing.

`generate`, `verify` and `publish` must be run in a provider project: a directory with a `Makefile`
and a `go.mod` that requires a `github.com/projectbeskar/virtrigaud` module, which is what `init`
creates.

**Building the image.** The generated `Dockerfile` builds from the project directory alone
(`go.mod` and `go.sum` first, then the source; run `go mod tidy` once so that `go.sum` exists). A
project that requires a release of the modules builds that way. A project whose `go.mod` has
`replace` directives to a checkout cannot: the checkout is not in the build context, and the
generated `Dockerfile` says so in a comment. Build such an image from a context that contains
both, or switch `go.mod` to a release before building.
