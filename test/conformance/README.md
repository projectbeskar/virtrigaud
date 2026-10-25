# Conformance test specifications

The `*.yaml` files in [`specs/`](specs/) are the VirtRigaud conformance tests. `vcts` validates,
lists and runs them; see [`docs/cli-tools.md`](../../docs/cli-tools.md#vcts) for the command and
for what a run does to the cluster.

```
vcts validate test/conformance/specs
vcts list
vcts run --provider <name> -n <namespace> --set IMAGE=<vmimage>
```

## File format

A file is a YAML list of tests. A key the format does not define is an error.

```yaml
- name: vm-power-cycle               # lower-case letters, digits and '-'; unique across the files
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
| `update` | JSON-merge-patches the named object with the resource's other fields (`spec`, `metadata.labels`, `metadata.annotations`). |
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

While waiting for anything but `Failed`, an object whose `status.phase` becomes `Failed` fails the
step at once. An object that does not exist yet is waited for (it may be one the operator creates,
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
create itself, then everything it created, newest first, waiting until each is gone. To have an
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
