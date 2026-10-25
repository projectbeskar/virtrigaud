# Project Instructions for Claude Code

> VirtRigaud - Kubernetes Operator for Multi-Hypervisor VM Management
> Language: Go | Framework: controller-runtime / kubebuilder

---

## 🚨 Critical TODOs

### Code Quality: Use Global Constants for Repeated Strings
**Status:** 🔄 Ongoing
**Impact:** Code maintainability and consistency

When a string literal appears in multiple places across the codebase, it MUST be defined as a global constant and referenced consistently.

**Why:**
- **Single Source of Truth**: Changes only need to be made in one place
- **Consistency**: Prevents typos and inconsistencies across the codebase
- **Maintainability**: Easier to refactor and update values
- **Type Safety**: Compiler catches usage errors

**When to Create a Global Constant:**
- String appears 2+ times in the same file
- String appears in multiple files
- String represents a configuration value (paths, filenames, keys, etc.)
- String is part of an API contract or protocol

**Examples:**
```go
// ✅ GOOD - Use constants
const (
    FinalizerName        = "virtualmachine.infra.virtrigaud.io/finalizer"
    ConditionTypeReady   = "Ready"
    ConditionTypeError   = "Error"
    AnnotationLastApplied = "virtrigaud.io/last-applied-config"
)

func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    if controllerutil.ContainsFinalizer(vm, FinalizerName) {
        // ...
    }
}

// ❌ BAD - Hardcoded strings
func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    if controllerutil.ContainsFinalizer(vm, "virtualmachine.infra.virtrigaud.io/finalizer") {
        // ...
    }
}
```

**Where to Define Constants:**
- Package-level constants: At the top of the file for file-specific use
- Shared constants: In a dedicated package (e.g., `pkg/constants/constants.go`) for cross-package use
- API constants: In `api/infra.virtrigaud.io/v1beta1/` for CRD-related constants
- Group related constants together with documentation

**Verification:**
Before committing, search for repeated string literals:
```bash
# Find potential duplicate strings in Go files
grep -rn '"[^"]\{10,\}"' internal/ api/ | sort | uniq -d
```

### High Priority: CRD Code Generation
**Status:** ✅ Fully Automated
**Impact:** CRD YAMLs are auto-generated from Go types with multiple safety layers

The Go types in `api/infra.virtrigaud.io/v1beta1/` are the **source of truth**. CRD YAML files in `/config/crd/bases/` are **auto-generated** from these types using controller-gen.

#### **Automated Workflow** (Recommended - One-time Setup):

**1. Install Git Hooks (One-time setup):**
```bash
make setup-git-hooks
```

After installation, the pre-commit hook will **automatically**:
- Detect changes to `*_types.go` files
- Regenerate CRD YAMLs and DeepCopy methods
- Sync CRDs to Helm chart
- Add generated files to your commit
- Run formatting and linting checks

**2. Modify CRD Types:**
```bash
# Edit your CRD type definitions
vim api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go

# Commit - the hook handles the rest automatically
git add api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go
git commit -m "Add new field to VirtualMachine spec"
# ✅ Hook automatically regenerates CRDs, syncs to Helm, and adds to commit
```

#### **Manual Workflow** (If hooks not installed):

1. Edit Go types in `api/infra.virtrigaud.io/v1beta1/*_types.go`
2. Regenerate code and CRDs:
   - `make generate` - Generate Go code
   - `make gen-crds` - Generate CRDs for config directory
3. **CRITICAL**: Update examples in `examples/` to match new schema
4. **CRITICAL**: Update documentation in `docs/` that references the CRDs
5. Validate all examples: `kubectl apply --dry-run=client -f examples/`
6. Run `make fmt lint test` to ensure code quality
7. Deploy with `make install`

#### **CRD Generation Commands:**

```bash
# Generate CRDs for local development (config/crd/bases/)
make gen-crds

# Generate CRDs for Helm chart (charts/virtrigaud/crds/)
make gen-helm-crds

# Note: Helm chart CRDs are generated during packaging and not checked into git
```

#### **CI/CD Protection:**

The following CI checks ensure CRDs stay synchronized:
1. **`verify-crds.yml` workflow** - Runs on every PR that touches CRD files
2. **`generate` job in CI** - Verifies generated files are up-to-date
3. **Automatic PR comments** - Bot comments with fix instructions if verification fails

⚠️ **IMPORTANT**: Examples and documentation MUST stay in sync with CRD schemas. After ANY CRD change, you MUST update:
- `examples/*.yaml` - Ensure all examples can be applied successfully
- `docs/` - Update any documentation that references the CRD fields
- Quickstart guide - Verify all YAML snippets are valid

---

## 🔒 Compliance & Security Context

This codebase operates in a **regulated banking environment**. All changes must be:
- Auditable with clear documentation
- Traceable to a business or technical requirement
- Compliant with zero-trust security principles

**Never commit**:
- Secrets, tokens, or credentials (even examples)
- Internal hostnames or IP addresses
- Customer or transaction data in any form

---

## 📝 Documentation Requirements

### Mandatory: Documentation Updates for Code Changes

**CRITICAL: After ANY code change in the `internal/`, `api/`, `proto/`, `sdk/`, `charts/` or `cmd/` directories, you MUST update all relevant documentation.**

This is a **mandatory step** that must be completed before considering any task complete. Documentation must always reflect the current state of the code.

#### Documentation Update Workflow

When adding, removing, or changing any feature in the Go source code:

1. **Analyze the Change**:
   - What functionality was added/removed/changed?
   - What are the user-facing impacts?
   - What are the architectural implications?
   - Are there new APIs, configuration options, or behaviors?

2. **Update Documentation** (in this order):
   - **`CHANGELOG.md`** - Document the change (see format below)
   - **`docs/`** - Update all affected documentation pages:
     - User guides that reference the changed functionality
     - Quickstart guides with examples of the changed code
     - Configuration references for new/changed options
     - Troubleshooting guides if behavior changed
   - **`examples/`** - Update YAML examples to reflect changes
   - **Architecture diagrams** - Update if structure/flow changed
   - **API documentation** - Regenerate if CRDs changed (`make gen-crds`)
   - **README.md** - Update if getting started steps or features changed

3. **Verify Documentation Accuracy**:
   - Read through updated docs as if you're a new user
   - Ensure all code examples compile and run
   - Verify all YAML examples validate: `kubectl apply --dry-run=client -f examples/`
   - Check that diagrams match current architecture
   - Confirm API docs reflect current CRD schemas

4. **Add Missing Documentation**:
   - If architecture changed, add/update architecture diagrams
   - If new public APIs were added, document them
   - If new configuration options exist, document them with examples
   - If new error conditions exist, document troubleshooting steps
   - If new dependencies were added, document version requirements

#### What Documentation to Update

**For Controller/Reconciler Changes** (`internal/controller/`):
- Update reconciliation flow diagrams
- Document new behaviors in user guides
- Update troubleshooting guides for new error conditions
- Add examples showing the new functionality

**For CRD Changes** (`api/infra.virtrigaud.io/v1beta1/`):
- Run `make generate gen-crds gen-helm-crds` to regenerate DeepCopy and the CRD YAMLs
- Update ALL examples in `examples/` that use the changed CRD
- Update quickstart guides with new field examples
- Update configuration reference documentation

**For Provider Changes** (`internal/providers/`):
- Update provider-specific documentation in `docs/` (for example `docs/libvirt-clones.md`, `docs/proxmox-networks.md`)
- Update architecture documentation explaining the change
- Add code examples for new provider capabilities
- Update troubleshooting guides for new behaviors

**For gRPC/Remote Provider Changes** (`internal/transport/grpc/`, `internal/providers/`, `proto/`):
- Update remote provider documentation
- Document protocol changes
- Update deployment examples

**For New Features**:
- Add feature documentation to `docs/`
- Update feature list in README.md
- Add usage examples
- Create architecture diagrams showing how the feature works
- Document configuration options
- Add troubleshooting section

**For Bug Fixes**:
- Update troubleshooting guides with the fix
- Document workarounds (if applicable) in known issues
- Update behavior documentation if expectations changed

#### Documentation Quality Standards

- **Completeness**: All user-visible changes must be documented
- **Accuracy**: Documentation must match the actual code behavior
- **Examples**: Include working examples for all features
- **Clarity**: Write for users who haven't seen the code
- **Diagrams**: Use Mermaid diagrams for complex flows
- **Versioning**: Date all changes in CHANGELOG.md

#### Validation Checklist

Before considering a task complete, verify:
- [ ] CHANGELOG.md updated with change details
- [ ] All affected documentation pages updated
- [ ] All YAML examples validate successfully
- [ ] API documentation regenerated (if CRDs changed)
- [ ] Architecture diagrams updated (if structure changed)
- [ ] Code examples compile and run
- [ ] README.md updated (if getting started or features changed)
- [ ] No broken links in documentation
- [ ] Documentation reviewed as if reading for the first time

### Mandatory: Update Changelog on Every Code Change

After **ANY** code modification, update `CHANGELOG.md` with the following format:

```markdown
## [YYYY-MM-DD HH:MM] - Brief Title
**Author:** @github-username (Full Name)

### Changed
- `path/to/file.go`: Description of the change

### Why
Brief explanation of the business or technical reason.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only
```

**Author Attribution Rules:**
- **ALWAYS** include the `**Author:**` line immediately after the heading
- Format: `**Author:** @github-username (Full Name)`
- For changes by Erick Bourgeois: `**Author:** @firestoned (Erick Bourgeois)`
- For changes by William Rizzo: `**Author:** @williamrizzo (William Rizzo)`
- For automated changes: `**Author:** github-actions[bot]`
- This ensures proper attribution and traceability in regulated environments

### Code Comments

All exported functions, types, and constants **must** have GoDoc comments:

```go
// Reconcile handles the reconciliation of VirtualMachine resources.
// It ensures the actual state matches the desired state defined in the CR.
//
// The reconciler:
//   - Creates/updates VMs on the target hypervisor
//   - Updates status conditions reflecting the current state
//   - Manages finalizers for cleanup on deletion
//
// Returns ctrl.Result with requeue settings and any error encountered.
func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
```

### Architecture Decision Records (ADRs)

For significant design decisions, create `docs/adr/NNNN-title.md`:

```markdown
# ADR-NNNN: Title

## Status
Proposed | Accepted | Deprecated | Superseded by ADR-XXXX

## Context
What is the issue we're facing?

## Decision
What have we decided to do?

## Consequences
What are the trade-offs?
```

---

## 🐹 Go Workflow

### After Modifying Any `.go` File

**CRITICAL: At the end of EVERY task that modifies Go files, ALWAYS run these commands in order:**

```bash
# 1. Format code
make fmt
# Or directly: go fmt ./...

# 2. Run linter with strict settings
make lint
# Or directly: golangci-lint run ./...

# 3. Run tests
make test
# Or directly: go test -v -race ./...

# 4. Verify builds
make build

# 5. If CRD types changed, regenerate manifests
make generate manifests
```

**IMPORTANT:**
- This is MANDATORY at the end of every task involving Go code changes
- Fix ALL linter warnings before considering the task complete
- Do NOT skip these steps - they catch bugs and ensure code quality
- If lint or tests fail, the task is NOT complete

**CRITICAL: After ANY Go code modification, you MUST verify:**

1. **Function documentation is accurate**:
   - Check GoDoc comments match what the function actually does
   - Verify all parameters are documented
   - Verify return values are documented
   - Verify error conditions are described
   - Update examples in doc comments if behavior changed

2. **Unit tests are accurate and passing**:
   - Check test assertions match the new behavior
   - Update test expectations if behavior changed
   - Ensure all tests compile and run successfully
   - Add new tests for new behavior/edge cases

3. **End-user documentation is updated**:
   - Update relevant files in `docs/` directory
   - Update examples in `examples/` directory
   - Ensure `CHANGELOG.md` reflects the changes
   - Verify example YAML files validate successfully

### Unit Testing Requirements

**CRITICAL: When modifying ANY Go code, you MUST update, add, or delete unit tests accordingly:**

1. **Adding New Functions/Methods:**
   - MUST add unit tests for ALL new exported functions
   - Test both success and failure scenarios
   - Include edge cases and boundary conditions

2. **Modifying Existing Functions:**
   - MUST update existing tests to reflect changes
   - Add new tests if new behavior or code paths are introduced
   - Ensure ALL existing tests still pass

3. **Deleting Functions:**
   - MUST delete corresponding unit tests
   - Remove or update integration tests that depended on deleted code

4. **Refactoring Code:**
   - Update test names and assertions to match refactored code
   - Verify test coverage remains the same or improves
   - If refactoring changes function signatures, update ALL tests

5. **Test Quality Standards:**
   - Use descriptive test names (e.g., `TestReconcile_CreatesVM_WhenMissing`)
   - Follow table-driven tests pattern for multiple scenarios
   - Use testify/assert or gomega for assertions
   - Mock external dependencies (k8s API, hypervisor APIs)
   - Test error conditions, not just happy paths
   - Ensure tests are deterministic (no flaky tests)

6. **Test File Organization:**
   - Tests go in `*_test.go` files alongside the source
   - Use `_test` package suffix for black-box testing where appropriate
   - Integration tests go in `/tests/` directory

**VERIFICATION:**
- After ANY Go code change, run `make test`
- ALL tests MUST pass before the task is considered complete
- If you add code but cannot write a test, document WHY in the code comments

**Example:**
If you modify `internal/controller/virtualmachine_controller.go`:
1. Update/add tests in `internal/controller/virtualmachine_controller_test.go`
2. Run `go test -v ./internal/controller/...` to verify
3. Ensure ALL tests pass before moving on

### Go Style Guidelines

- Follow [Effective Go](https://go.dev/doc/effective_go) and [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)
- Use `context.Context` as the first parameter for all functions that do I/O
- Return `error` as the last return value
- Use structured errors with `fmt.Errorf("context: %w", err)` for wrapping
- Prefer explicit error handling - avoid panic in library code
- Use `slog` or `logr` for structured logging
- All k8s API calls must have timeout and retry logic
- Use controller-runtime's `client.Client` for Kubernetes operations

### Error Handling

```go
// ✅ GOOD - Wrapped errors with context
if err := r.Client.Get(ctx, req.NamespacedName, vm); err != nil {
    if apierrors.IsNotFound(err) {
        return ctrl.Result{}, nil
    }
    return ctrl.Result{}, fmt.Errorf("failed to get VirtualMachine: %w", err)
}

// ❌ BAD - Bare error return without context
if err := r.Client.Get(ctx, req.NamespacedName, vm); err != nil {
    return ctrl.Result{}, err
}
```

### Dependency Management

Before adding a new dependency:
1. Check if existing deps solve the problem
2. Verify the module is actively maintained (commits in last 6 months)
3. Prefer modules from well-known authors or the Go/Kubernetes ecosystem
4. Document why the dependency was added in `CHANGELOG.md`
5. Run `go mod tidy` after adding dependencies

---

## ☸️ Kubernetes Operator Patterns

### CRD Development - Go Types as Source of Truth

**CRITICAL: Go types in `api/infra.virtrigaud.io/v1beta1/` are the source of truth.**

CRD YAML files in `/config/crd/bases/` are **AUTO-GENERATED** from the Go types using controller-gen. This ensures:
- Type safety enforced at compile time
- CRDs deployed to clusters match what the operator expects
- Schema validation in Kubernetes matches Go types
- No drift between deployed CRDs and operator code

#### Workflow for CRD Changes:

1. **Edit the Go types** in `api/infra.virtrigaud.io/v1beta1/*_types.go`
2. **Add kubebuilder markers** for validation and printing:
   ```go
   // +kubebuilder:validation:Required
   // +kubebuilder:validation:MinLength=1
   ```
3. **Regenerate code and manifests**:
   ```bash
   make generate manifests
   ```
4. **Verify generated YAMLs** look correct
5. **Update `CHANGELOG.md`** documenting the CRD change
6. **Update examples** in `examples/`
7. **Deploy updated CRDs**:
   ```bash
   make install
   ```

#### Adding New CRDs:

1. Create the new type file in `api/infra.virtrigaud.io/v1beta1/`:
   ```go
   // +kubebuilder:object:root=true
   // +kubebuilder:subresource:status
   // +kubebuilder:resource:scope=Namespaced,shortName=vms
   // +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
   // +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

   // VirtualMachine is the Schema for the virtualmachines API
   type VirtualMachine struct {
       metav1.TypeMeta   `json:",inline"`
       metav1.ObjectMeta `json:"metadata,omitempty"`

       Spec   VirtualMachineSpec   `json:"spec,omitempty"`
       Status VirtualMachineStatus `json:"status,omitempty"`
   }
   ```

2. Add to `api/infra.virtrigaud.io/v1beta1/groupversion_info.go` scheme registration

3. Regenerate:
   ```bash
   make generate manifests
   ```

#### CI/CD Integration:

Add this to your CI pipeline to ensure CRDs stay in sync:

```bash
# Generate manifests
make generate gen-crds

# Check if any files changed
if ! git diff --quiet config/crd/bases/; then
  echo "ERROR: CRD YAML files are out of sync with api/infra.virtrigaud.io/v1beta1/"
  echo "Run: make generate gen-crds"
  exit 1
fi
```

### Controller Best Practices

- Always set `ownerReferences` for child resources using `controllerutil.SetControllerReference`
- Use finalizers for cleanup logic with `controllerutil.AddFinalizer/RemoveFinalizer`
- Implement exponential backoff for retries using `ctrl.Result{RequeueAfter: time.Second * n}`
- Log reconciliation start/end with resource name and namespace using structured logging
- Use `client.FieldIndexer` for efficient lookups
- Handle deletion gracefully with finalizers before removing the finalizer

### Status Conditions

Always update status conditions following Kubernetes conventions using `meta.SetStatusCondition`:

```go
import "k8s.io/apimachinery/pkg/api/meta"

meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
    Type:               "Ready",
    Status:             metav1.ConditionTrue,
    Reason:             "ReconcileSucceeded",
    Message:            "VM synchronized successfully",
    LastTransitionTime: metav1.Now(),
    ObservedGeneration: vm.Generation,
})
```

### Provider Interface Pattern

VirtRigaud uses a provider interface for hypervisor abstraction. The manager talks to
each provider through `contracts.Provider`
([`internal/providers/contracts/provider.go`](../internal/providers/contracts/provider.go)),
implemented over gRPC by `internal/transport/grpc`; each provider binary serves the
gRPC contract in `proto/provider/v1/provider.proto`:

```go
// Abridged; see contracts.Provider for the full interface.
type Provider interface {
    Validate(ctx context.Context) error
    Create(ctx context.Context, req CreateRequest) (CreateResponse, error)
    Delete(ctx context.Context, vm VMRef) (taskRef string, err error)
    Power(ctx context.Context, vm VMRef, op PowerOp) (taskRef string, err error)
    Reconfigure(ctx context.Context, vm VMRef, desired CreateRequest) (ReconfigureResult, error)
    Describe(ctx context.Context, vm VMRef) (DescribeResponse, error)
    TaskStatus(ctx context.Context, taskRef string) (TaskStatus, error)
    SnapshotCreate(ctx context.Context, req SnapshotCreateRequest) (SnapshotCreateResponse, error)
    // ...snapshot delete/revert, clone, disk export/import, image prepare
}
```

When adding new providers, use the `add-provider` workflow: a `cmd/provider-<name>/` binary, an
`internal/providers/<name>/` package implementing the gRPC server, and capabilities that say
what it really supports. A new RPC must be implemented, or return `Unimplemented` and be
declared in the capabilities, in every provider. Document the provider in `docs/`.

---

## 🔄 FluxCD / GitOps Integration

### Kustomization Structure

```
config/
├── default/           # Default kustomization
├── crd/               # CRD definitions
├── manager/           # Manager deployment
├── rbac/              # RBAC resources
└── samples/           # Sample CRs
```

### HelmRelease Changes

When modifying Helm chart or deployment manifests:
1. Bump the chart version or values checksum
2. Add suspend annotation for breaking changes
3. Document rollback procedure in `CHANGELOG.md`

---

## 🧪 Testing Requirements

### Unit Tests

**MANDATORY: Every exported function MUST have corresponding unit tests.**

#### Test File Organization

Tests are placed in `*_test.go` files alongside the source code:

```go
// internal/controller/virtualmachine_controller.go
package controller

// internal/controller/virtualmachine_controller_test.go
package controller

import (
    "testing"
    "context"
    
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcile_CreatesVM_WhenMissing(t *testing.T) {
    // Arrange
    ctx := context.Background()
    client := fake.NewClientBuilder().
        WithScheme(scheme.Scheme).
        WithObjects(testVM).
        Build()
    
    reconciler := &VirtualMachineReconciler{
        Client: client,
        Scheme: scheme.Scheme,
    }
    
    // Act
    result, err := reconciler.Reconcile(ctx, ctrl.Request{
        NamespacedName: types.NamespacedName{
            Name:      "test-vm",
            Namespace: "default",
        },
    })
    
    // Assert
    require.NoError(t, err)
    assert.Equal(t, ctrl.Result{RequeueAfter: time.Minute}, result)
}

func TestReconcile_HandlesProviderError(t *testing.T) {
    // Arrange
    ctx := context.Background()
    mockProvider := &mocks.Provider{}
    mockProvider.On("CreateVM", mock.Anything, mock.Anything).
        Return(nil, errors.New("provider unavailable"))
    
    // Act
    result, err := reconciler.Reconcile(ctx, req)
    
    // Assert
    require.Error(t, err)
    assert.Contains(t, err.Error(), "provider unavailable")
}
```

**Table-Driven Tests Pattern:**

```go
func TestVMSpec_Validate(t *testing.T) {
    tests := []struct {
        name    string
        spec    VMSpec
        wantErr bool
        errMsg  string
    }{
        {
            name: "valid spec",
            spec: VMSpec{
                CPUs:   2,
                Memory: "4Gi",
            },
            wantErr: false,
        },
        {
            name: "invalid - zero CPUs",
            spec: VMSpec{
                CPUs:   0,
                Memory: "4Gi",
            },
            wantErr: true,
            errMsg:  "CPUs must be greater than 0",
        },
        {
            name: "invalid - empty memory",
            spec: VMSpec{
                CPUs:   2,
                Memory: "",
            },
            wantErr: true,
            errMsg:  "memory is required",
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := tt.spec.Validate()
            if tt.wantErr {
                require.Error(t, err)
                assert.Contains(t, err.Error(), tt.errMsg)
            } else {
                require.NoError(t, err)
            }
        })
    }
}
```

**Test Coverage Requirements:**
- **Success path:** Test the primary expected behavior
- **Failure paths:** Test error handling for each possible error type
- **Edge cases:** Empty strings, nil values, boundary conditions
- **State changes:** Verify correct state transitions
- **Concurrent operations:** Test race conditions where applicable

**When to Update Tests:**
- **ALWAYS** when adding new functions → Add new tests
- **ALWAYS** when modifying functions → Update existing tests
- **ALWAYS** when deleting functions → Delete corresponding tests
- **ALWAYS** when refactoring → Verify tests still cover the same behavior

### Integration Tests

Place in `/tests/e2e/` directory:
- Use envtest for controller integration tests
- Use real or mocked hypervisor APIs for provider tests
- Test failure scenarios, not just happy path
- Test end-to-end workflows (create → update → delete)
- Verify finalizers and cleanup logic

### Test Execution

**Before committing ANY Go changes:**
```bash
# Run all tests
make test

# Run tests with race detection
go test -v -race ./...

# Run tests for a specific package
go test -v ./internal/controller/...

# Run tests with coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Run integration tests (requires cluster)
make test-e2e
```

**ALL tests MUST pass before code is considered complete.**

---

## 📁 File Organization

```
api/
└── infra.virtrigaud.io/v1beta1/      # CRD type definitions (source of truth)
    ├── groupversion_info.go          # Scheme registration
    ├── virtualmachine_types.go       # VirtualMachine CRD
    ├── vmclass_types.go              # VMClass CRD
    ├── vmimage_types.go              # VMImage CRD
    ├── vmnetworkattachment_types.go  # VMNetworkAttachment CRD
    ├── provider_types.go             # Provider CRD
    ├── vmsnapshot_types.go           # VMSnapshot CRD
    ├── vmclone_types.go              # VMClone CRD
    ├── vmmigration_types.go          # VMMigration CRD
    ├── vmset_types.go                # VMSet CRD
    ├── vmplacementpolicy_types.go    # VMPlacementPolicy CRD
    ├── host_types.go, hostpool_types.go  # Host and HostPool CRDs (clustered libvirt)
    └── zz_generated.deepcopy.go      # Auto-generated DeepCopy
cmd/
├── manager/                          # Manager entry point
├── provider-vsphere/                 # vSphere provider binary
├── provider-libvirt/                 # Libvirt provider binary
├── provider-proxmox/                 # Proxmox provider binary
├── provider-mock/                    # Mock provider binary
└── vrtg/, vrtg-provider/, vcts/, ... # CLIs
internal/
├── controller/                       # Reconcilers (one flat package; *_controller.go + *_test.go)
├── providers/                        # gRPC server implementations
│   ├── vsphere/, libvirt/, proxmox/, mock/
│   └── contracts/                    # Provider interface and capability types
├── transport/grpc/                   # Manager-side gRPC client
├── scheduler/                        # Clustered placement
└── obs/, resilience/, storage/, k8s/, runtime/, scaffold/, util/  # Cross-cutting
proto/                                # Separate Go module: provider.proto + generated code
sdk/                                  # Separate Go module: SDK for external provider authors
charts/virtrigaud/                    # Helm chart (CRDs generated into crds/, gitignored)
config/
├── crd/bases/                        # Generated CRD YAMLs (checked in)
├── manager/                          # Manager deployment
├── rbac/                             # RBAC definitions
└── samples/                          # Sample CRs
examples/                             # User-facing example YAMLs (dry-run by `make test`)
docs/
├── adr/                              # Architecture Decision Records
└── release-notes/                    # Release notes
```

**Test File Pattern:**
- Every `foo.go` has a corresponding `foo_test.go` in the same package
- Test files use the same package name for white-box testing
- Use `package foo_test` suffix for black-box testing where appropriate

---

## 🚫 Things to Avoid

- **Never** use `panic()` in production code - return errors properly
- **Never** hardcode namespaces - make them configurable
- **Never** use `time.Sleep()` for synchronization - use proper k8s watch/informers
- **Never** ignore errors in finalizers - this blocks resource deletion
- **Never** store state outside of Kubernetes - operators must be stateless
- **Never** use `interface{}` without type assertions - prefer generics or typed interfaces
- **Never** shadow the `err` variable - use explicit error variable names

---

## 💡 Helpful Commands

```bash
# Generate CRD manifests and DeepCopy methods
make generate manifests

# Install CRDs to cluster
make install

# Uninstall CRDs from cluster
make uninstall

# Run the controller locally
make run

# Build the manager binary
make build

# Build container images
make docker-build

# Push container images
make docker-push

# Run all tests
make test

# Run linter
make lint

# Format code
make fmt

# Validate CRD manifests
kubectl apply --dry-run=server -f config/crd/bases/

# Validate example manifests
for file in examples/*.yaml; do
  echo "Checking $file"
  kubectl apply --dry-run=client -f "$file"
done

# Generate mocks (if using mockery)
go generate ./...

# Update dependencies
go mod tidy
go mod vendor  # if vendoring
```

---

## 📋 PR/Commit Checklist

**MANDATORY: Run this checklist at the end of EVERY task before considering it complete.**

Before committing:

- [ ] **If ANY `.go` file was modified**:
  - [ ] **Unit tests updated/added/deleted** to match code changes (REQUIRED)
  - [ ] All new exported functions have corresponding tests (REQUIRED)
  - [ ] All modified functions have updated tests (REQUIRED)
  - [ ] All deleted functions have tests removed (REQUIRED)
  - [ ] `make fmt` passes (REQUIRED)
  - [ ] `make lint` passes (REQUIRED - fix ALL warnings)
  - [ ] `make test` passes (REQUIRED - ALL tests must pass)
  - [ ] **Documentation updated** for code changes (REQUIRED - see Documentation Requirements section):
    - [ ] GoDoc comments on ALL exported items (functions, types, constants)
    - [ ] Function documentation matches actual behavior (parameters, returns, errors)
    - [ ] `docs/` updated for user-facing changes
    - [ ] Architecture diagrams updated if structure changed
    - [ ] Examples added for new features
    - [ ] Troubleshooting docs updated for new error conditions
- [ ] **If `api/infra.virtrigaud.io/v1beta1/*_types.go` was modified**:
  - [ ] Run `make generate gen-crds gen-helm-crds` to regenerate DeepCopy and the CRDs
  - [ ] **Update `examples/*.yaml` to match new schema** (CRITICAL)
  - [ ] **Update `docs/` documentation** that references the CRDs (CRITICAL)
  - [ ] Run `kubectl apply --dry-run=client -f examples/` to verify all examples (REQUIRED)
  - [ ] Run `make fmt lint test` to ensure everything passes
- [ ] **If `internal/controller/` was modified**:
  - [ ] Update reconciliation flow diagrams in `docs/`
  - [ ] Document new behaviors in user guides
  - [ ] Update troubleshooting guides for new error conditions
  - [ ] Add examples showing the new functionality
  - [ ] Verify all examples still work with the changes
- [ ] **If `internal/providers/` was modified**:
  - [ ] Update provider documentation in `docs/`
  - [ ] Test against actual hypervisor (if possible)
  - [ ] Document any new configuration options
- [ ] **Documentation verification** (CRITICAL):
  - [ ] `CHANGELOG.md` updated with detailed change description
  - [ ] All affected documentation pages reviewed and updated
  - [ ] All YAML examples validate: `kubectl apply --dry-run=client -f examples/`
  - [ ] Code examples in docs compile and run
  - [ ] Architecture diagrams match current implementation
  - [ ] API documentation reflects current CRD schemas
  - [ ] README.md updated if getting started or features changed
  - [ ] No broken links in documentation
- [ ] CRD YAML files validate: `kubectl apply --dry-run=client -f config/crd/bases/`
- [ ] No secrets or sensitive data
- [ ] Error handling uses proper error wrapping (no bare returns)
- [ ] `go mod tidy` has been run

**A task is NOT complete until all of the above items pass successfully.**

**Documentation is NOT optional** - it is a critical requirement equal in importance to the code itself.

---

## 🔗 Project References

- [controller-runtime documentation](https://pkg.go.dev/sigs.k8s.io/controller-runtime)
- [kubebuilder book](https://book.kubebuilder.io/)
- [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
- [Operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- [Effective Go](https://go.dev/doc/effective_go)
- [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)
- Internal: k0rdent platform docs (check Confluence)

---

## 📦 Provider-Specific Notes

### vSphere Provider
- Uses govmomi for vCenter API interaction
- Supports async task tracking via TaskStatus RPC
- Cloud-init via guestinfo properties
- Documentation: `docs/vm-ownership.md` (owner stamp, required vCenter privilege)

### Libvirt Provider
- Runs `virsh` and `qemu-img` on the host over an in-process SSH client (`qemu+ssh://`); host keys are verified against `known_hosts` (ADR-0004)
- Cloud-init via nocloud ISO
- VNC console URL
- Documentation: `docs/libvirt-domain-ownership.md`, `docs/libvirt-clones.md`, `docs/clustered-provider-inventory.md`

### Proxmox Provider
- Uses the REST API for Proxmox VE, plus SSH to the node for migration disk transfers
- Applies only the user name and SSH keys of cloud-init user-data
- Documentation: `docs/proxmox-networks.md`, `docs/proxmox-cloud-init-storage.md`

See `docs/README.md` for the documentation index.

---

## 🏗️ Remote Provider Architecture

VirtRigaud uses a Remote Provider architecture for scalability:

```
┌─────────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                        │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐   │
│  │  VirtRigaud  │    │   vSphere    │    │   Libvirt    │   │
│  │   Manager    │───▶│   Provider   │    │   Provider   │   │
│  │              │    │     Pod      │    │     Pod      │   │
│  └──────────────┘    └──────────────┘    └──────────────┘   │
│         │                   │                   │            │
│         │              gRPC/TLS            gRPC/TLS          │
│         │                   │                   │            │
└─────────┼───────────────────┼───────────────────┼────────────┘
          │                   │                   │
          ▼                   ▼                   ▼
    ┌──────────┐       ┌──────────┐       ┌──────────┐
    │  CRDs    │       │ vCenter  │       │  KVM     │
    │ (Source  │       │  Server  │       │  Host    │
    │ of Truth)│       │          │       │          │
    └──────────┘       └──────────┘       └──────────┘
```

**Benefits:**
- **Scalability**: Each provider runs as independent pods
- **Reliability**: Provider failures don't affect the manager
- **Security**: Provider credentials are isolated
- **Flexibility**: Scale providers independently
- **Maintainability**: Update providers without manager downtime

When modifying gRPC communication:
1. Update protobuf definitions in `proto/provider/v1/provider.proto`
2. Regenerate Go code: `make proto`
3. Update both client and server implementations
4. Test thoroughly with integration tests