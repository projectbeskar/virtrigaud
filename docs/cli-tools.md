# Command-line tools

VirtRigaud ships four small command-line programs next to the operator. They are separate from
the operator: they talk to the cluster with your kubeconfig (or, for `vrtg-provider`, only edit
local files), and nothing in the manager or in a provider image depends on them.

| Program | What it is for |
|---------|----------------|
| [`vrtg`](#vrtg) | Read VirtRigaud resources: list and describe `VirtualMachine`s, list `Provider`s |
| [`vcts`](#vcts) | Validate conformance test specifications; run the conformance suite against a `Provider` |
| [`virtrigaud-loadgen`](#virtrigaud-loadgen) | Create and delete `VirtualMachine`s at a steady rate and record how long the API calls take |
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

The tools need the VirtRigaud CRDs installed in the cluster and permission to read (`vrtg`,
`vcts`) or to create and delete (`virtrigaud-loadgen`) the resources they touch.

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
vcts validate [spec-dir]              # default spec-dir: test/conformance/specs
vcts list
vcts run --provider <name> [--output-dir ./conformance-results] [--skip <test>,...] [--timeout 30m]
```

- **`vcts validate`** checks every `*.yaml` file in the directory and exits **non-zero** when a
  file cannot be parsed or holds an invalid test (a bad test name, a missing description or step,
  an unknown step type, ...). It exits 0 only when every file is valid. The CI workflow uses it on
  `test/conformance/specs`.
- **`vcts list`** and **`vcts run`** read the specifications from `test/conformance/specs`
  **relative to the current directory**, so run them from the root of a VirtRigaud checkout.
- **`vcts run`** reads the named `Provider` from the namespace given by `-n`, selects the tests whose
  required capabilities that provider type has (today only `vsphere` and `libvirt` Providers select
  any test), and writes `results.json`, `junit.xml` and `report.md` to `--output-dir`. It exits
  non-zero when a test fails.

> **Limitation in this release.** The runner does not execute the steps of a specification yet:
> every `create`, `update`, `delete`, `wait` and `validate` step reports "not implemented", so
> `vcts run` reports the selected tests as failed. Use `vcts validate` and `vcts list` today.

## virtrigaud-loadgen

```
virtrigaud-loadgen run [--config loadtest.yaml] [-n <namespace>] [--output-dir ./loadgen-results] [--dry-run] [--verbose]
```

Each worker performs one randomly chosen operation per second for the configured duration. Without
`--config` the built-in defaults apply: 5 minutes, 2 workers, VMs created against a provider named
`test-provider` with class `test-class` and image `test-image`. `--config` reads a YAML file that
overrides the defaults; durations are Go duration strings (`30s`, `5m`).

```yaml
duration: 2m
concurrency: 4
providers: [libvirt-lab]          # a Provider in -n; one is chosen at random per operation
vmTemplate:
  classRef: small                 # an existing VMClass
  imageRef: ubuntu-22             # an existing VMImage
  labels:
    team: perf
operations:                       # relative weights, not percentages; at least one must be above 0
  create: 40
  delete: 20
  describe: 40
  power: 0
  reconfigure: 0
  snapshot: 0
  clone: 0
```

`test/performance/proxmox-loadtest.yaml` is a ready configuration in this format for a Provider
named `proxmox-perf` (edit the provider, class and image names for your cluster); a unit test loads
every YAML file in `test/performance` with the tool's own loader. The tool has no scenarios or
pass/fail thresholds: read `summary.md` and compare it with your own targets.

An unknown key, an unparsable duration or an unusable value (no providers, all weights zero, ...) is
an error and nothing is created. The keys `vmCount`, `rampUpTime`, `steadyState`, `rampDownTime`,
the `*Interval` keys, `vmTemplate.annotations` and `vmTemplate.resources` are accepted but not used
in this release.

- **What it does to the cluster.** `create` makes a `VirtualMachine` named
  `loadgen-vm-<run>-<worker>-<n>` with the label `generated-by: loadgen` plus your template labels.
  `delete` removes a random VM **that this run created**; it never touches other VMs in the
  namespace. `describe` lists the VMs in the namespace. `power`, `reconfigure`, `snapshot` and
  `clone` are **simulated**: they are recorded as successful without calling the API.
- **Cleanup.** VMs still present when the run ends are left in place. Remove them with
  `kubectl delete virtualmachine -n <namespace> -l generated-by=loadgen`.
- **`--dry-run`** does everything except the API calls; no VM is created or deleted.
- **Results.** `results.csv` (one row per operation) and `summary.md` (counts and p50/p95/p99/max
  per operation) in `--output-dir`.

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
  then run `vcts run --provider <name>`.
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
