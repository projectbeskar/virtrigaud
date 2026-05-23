# Changelog

All notable changes to VirtRigaud will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.3.3] - 2026-03-17

### Changed
- Changelog organization with versioned release headers

---

## [2026-05-23 13:31] - feat(obs): wire ProviderRPCMetrics into gRPC client middleware (closes #90)
**Author:** @williamrizzo (William Rizzo)

### Added
- `internal/transport/grpc/client.go`: New `providerRPCMetricsInterceptor(providerType)` returns a `grpc.UnaryClientInterceptor` that wraps every outbound provider RPC. On each call it records `virtrigaud_provider_rpc_requests_total{provider_type,method,code}` and `virtrigaud_provider_rpc_latency_seconds{provider_type,method}` via the existing `metrics.NewRPCTimer` API.
- `internal/transport/grpc/client.go`: New `shortRPCMethod(fullMethod)` helper that extracts the RPC name from gRPC's full path (`/provider.v1.Provider/Validate` → `Validate`).
- `internal/transport/grpc/client.go`: New `grpcCodeString(err)` helper that returns the canonical gRPC status code string (`"OK"`, `"Unavailable"`, `"DeadlineExceeded"`, etc.) for the metric `code` label.
- `internal/transport/grpc/client_metrics_test.go`: New file. 5 tests / 15 sub-cases covering: `shortRPCMethod` parsing (6 edge cases), `grpcCodeString` mapping (5 codes incl. nil and non-gRPC errors), and 4 bufconn-based integration tests exercising the interceptor against an in-process gRPC server (successful call, error propagation, latency histogram, deadline-exceeded). No real network.

### Changed
- `internal/transport/grpc/client.go`: `NewClient` signature now takes a `providerType string` parameter inserted between `endpoint` and `tlsConfig`. The new parameter populates the `provider_type` metric label on every RPC made through this client. Empty string is permitted (label will be empty) but production callers should pass `provider.Spec.Type`.
- `internal/runtime/remote/resolver.go`: Updated the sole caller of `NewClient` to pass `string(provider.Spec.Type)` (the only callsite repo-wide; verified by `grep -rn "transport/grpc\".NewClient" --include="*.go"`).

### Why
Fourth in the G-track. Per-RPC latency + error code is the most operationally valuable single signal for the remote-provider architecture: it answers "which RPC method is slow?", "which provider type is flapping?", "are gRPC errors concentrated on specific endpoints?", and "what's our SLO baseline?" — questions that per-Kind reconcile counters (G1-G3) can't.

The interceptor is the natural insertion point — every gRPC call goes through it without per-callsite changes. The 17+ `c.client.XXX(...)` callsites in `client.go` need no edits.

Coordinates with ADR-0001 follow-up F3 (interceptor coverage audit) — this PR establishes the metrics interceptor; future PRs can layer logging/tracing interceptors using the same pattern.

### Impact
- [ ] Breaking change to public API (the `NewClient` signature change is internal-only; only `internal/runtime/remote/resolver.go` uses it; verified)
- [x] Requires cluster rollout (new manager image needed to emit the additional families)
- [ ] Config change only
- [ ] Documentation only

Targeted for **v0.3.5**.

### Verification
```bash
go test -v -count=1 -run "TestShortRPCMethod|TestGrpcCodeString|TestProviderRPCMetricsInterceptor" ./internal/transport/grpc/
# PASS: 5 tests / 15 sub-cases
make test  # transport/grpc pkg coverage 0% -> 9.2% (first tests in this package)
make test-integration  # still green
```

Post-deploy (after v0.3.5-rc1):
```bash
curl /metrics | grep '^virtrigaud_provider_rpc_requests_total'
# expect samples with provider_type=libvirt|vsphere|proxmox, method=Validate|Create|Describe|..., code=OK|Unavailable|...
curl /metrics | grep '^virtrigaud_provider_rpc_latency_seconds_bucket'
# expect populated histogram per provider_type x method combination
```

### References
- Closes #90
- Umbrella: #86
- Pattern: PRs #101 (G1), #103 (G2), #106 (G3 + K5)
- Coordinates with ADR-0001 F3

---

## [2026-05-23 12:46] - feat(obs): instrument remaining 6 reconcilers + fix double-count bug (closes #89, #105)
**Author:** @williamrizzo (William Rizzo)

Two related changes in one PR (both touch the same files / pattern):

### Fixed
- `internal/controller/vmmigration_controller.go` and `internal/controller/vmsnapshot_controller.go`: **Double-count bug** (#105). Prior `Reconcile` used `defer timer.Finish(metrics.OutcomeSuccess)` — the argument was evaluated and captured at defer-time — alongside explicit `timer.Finish(metrics.OutcomeError)` on error paths. Errored reconciles recorded TWO samples (one error from the explicit call, one success from the deferred call) because the deferred Finish always ran. `RecordReconcile` is not idempotent. Inflated success counters and made error-rate alerts under-report. Refactored both files to the G1 pattern (named return values + single deferred outcome-inference block that uses the actual return state).

### Added
Six reconcilers now instrumented (closes #89):
- `internal/controller/vmmigration_controller.go`: reconcile timer + `errReasonGetMigration` constant + `RecordError` on get/add-finalizer paths
- `internal/controller/vmsnapshot_controller.go`: reconcile timer + `errReasonGetSnapshot` constant + `RecordError` on get/add-finalizer/get-VM paths
- `internal/controller/vmadoption_controller.go`: reconcile timer + 3 new constants (`adoption-discover-failed`, `adoption-status-update`, `adoption-invalid-filter`) + `RecordError` at 4 sites
- `internal/controller/vmclass_controller.go`: reconcile timer only (stub Reconciler, no error paths)
- `internal/controller/vmimage_controller.go`: same
- `internal/controller/vmnetworkattachment_controller.go`: same

After this PR, all 8 reconcilers emit `virtrigaud_manager_reconcile_total{kind=...,outcome=...}` and `virtrigaud_manager_reconcile_duration_seconds{kind=...}`:
- `VirtualMachine` (G1, #87, PR #101)
- `Provider` (G2, #88, PR #103)
- `VMMigration`, `VMSnapshot`, `VMAdoption`, `VMClass`, `VMImage`, `VMNetworkAttachment` (G3, this PR)

### Added — tests
- `internal/controller/g3_metrics_test.go`: 4 tests covering:
  - `TestVMMigrationReconcile_NoDoubleCountOnSuccessfulNotFound` — regression canary for #105
  - `TestVMSnapshotReconcile_NoDoubleCountOnSuccessfulNotFound` — same
  - `TestG3_StubReconcilersEmitTimer` — table-driven across VMClass, VMImage, VMNetworkAttachment stubs; asserts outcome=success emits on no-op reconcile
  - `TestVMAdoptionReconcile_NotFoundIsSuccessOutcome` — IsNotFound contract for VMAdoption

### Why
G3 was the bulk pass over the remaining reconcilers per the G-track umbrella (#86) — meant to be mostly mechanical after G1 and G2 established the pattern. The audit step (recommended in #89's body) turned up the double-count bug in the 2 reconcilers that already had partial instrumentation. Filed as #105 and bundled into the same PR because both fixes touch the same files and the refactor was the same.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image needed to emit the additional families AND to fix the double-count)
- [ ] Config change only
- [ ] Documentation only

Targeted for **v0.3.5**.

### Verification
```bash
go test -v -count=1 -run "TestG3|TestVMMigrationReconcile_NoDouble|TestVMSnapshotReconcile_NoDouble|TestVMAdoptionReconcile" ./internal/controller/
# PASS: 6 sub-tests (4 top-level)
make test  # controller pkg coverage 22.4% -> 24.0%
make test-integration  # still green
```

Post-deploy (after v0.3.5-rc1 deploys):
```bash
curl /metrics | awk '/^virtrigaud_manager_reconcile_total/{print $1}' | sort -u
# expect samples across all 8 reconcilers' kinds
```

### References
- Closes #89 (G3)
- Closes #105 (K5 double-count fix)
- Umbrella: #86
- Pattern: PRs #101 (G1), #103 (G2)
- Pre-#105 bug shipping in: every release with `vmmigration` and `vmsnapshot` reconcilers that called the metrics package (at least v0.3.0+)

---

## [2026-05-23 12:24] - fix(ci): pin Build matrix checkout to v4 to mitigate K4 intermittent failure (refs #102)
**Author:** @williamrizzo (William Rizzo)

### Fixed
- `.github/workflows/ci.yml`: Pinned the `build` matrix job's `actions/checkout` from v6.0.2 (`de0fac2e...`) back to v4 (`34e114876b...`). Added a `DO NOT bump without resolving #102` comment so future contributors don't silently re-introduce the issue.

### Why
The `Build (...)` matrix in `ci.yml` (4 components: manager + 3 providers) has been intermittently failing during the `actions/checkout@v6.0.2` step with `fatal: could not read Username for 'https://github.com': terminal prompts disabled` (3 observed occurrences in 24 hours; tracked as #102). The same `v6.0.2` works fine on every other job in the same workflow (test, lint, security, build-tools, build-images). The asymmetry suggests something specific to this matrix's checkout invocation — possibly the parallel-fetch of the `pull/<N>/merge` ref racing against GitHub-side ref availability, or a regression introduced when PR #74 bumped checkout from v4 → v6 (PR #74 only touched ci.yml, not release.yml).

This is a **partial fix** targeted at the most common K4 occurrence shape:
- ✅ Addresses 2 of 3 observed occurrences (both in `ci.yml`'s `build` matrix)
- ⚠️ Does NOT address release-time K4 (`release.yml`'s `build-and-push` matrix was already on v4 when occurrence 1 hit during the failed v0.3.3.1 release attempt; root cause likely different)
- ⚠️ Does NOT pin `ci.yml`'s `build-images` matrix (no observed failures there; pinning preventively is over-fitting)

If K4 continues to fire after this pin lands, the next move is to wrap the affected checkout in a retry action (e.g., `nick-fields/retry`) as a belt-and-suspenders measure regardless of root cause.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only (CI workflow)
- [ ] Documentation only

CI-only change.

### Verification
- [x] YAML syntax is valid (will be confirmed by CI's own self-load on this PR)
- [ ] **CI on this PR runs cleanly** — if green on first try, the pin works AND we've already broken K4's per-PR pattern.
- [ ] **Next ~5 PRs in the G-track also run cleanly on first try** — that's the real evidence the pin solved the per-cycle friction.

If K4 still hits a future PR's `build` matrix despite this pin, that disproves the v6-regression hypothesis and we widen the search (retry wrapper, GitHub Actions infra investigation, etc.).

### References
- Refs #102 (not Closes — see "partial fix" caveat above; close #102 after we see ~10 consecutive K4-free CI runs across the next few PRs)
- Original v4→v6 bump: PR #74
- Affected runs: PR #101 CI (run 26331518215, succeeded on rerun), post-#101 main CI (run 26331986052, succeeded on rerun), failed v0.3.3.1 release attempt (run 26329718832, never re-attempted; re-cut as v0.3.4)

---

## [2026-05-23 11:58] - feat(obs): instrument ProviderReconciler with reconcile timer + error counter (closes #88)
**Author:** @williamrizzo (William Rizzo)

### Added
- `internal/controller/provider_controller.go`: `Reconcile` now records `virtrigaud_manager_reconcile_total{kind="Provider",outcome=...}` and `virtrigaud_manager_reconcile_duration_seconds{kind="Provider"}` via a single deferred block that infers outcome from named return values. Mirrors the G1 pattern shipped for `VirtualMachineReconciler` in PR #101.
- `internal/controller/provider_controller.go`: 6 `metrics.RecordError(reason, ComponentManager)` calls at the documented error sites in `Reconcile`, `handleDeletion`, and `reconcileRemoteRuntime`. New `errReason*` constants at the top of the file: `get-provider`, `runtime-spec-invalid`, `service-reconcile-failed`, `deployment-reconcile-failed`, `cleanup-failed`. No collisions with the VirtualMachine reconciler's taxonomy.
- `internal/controller/provider_metrics_test.go`: New file. 3 focused unit tests using `client/fake` (no envtest needed):
  - `TestProviderReconcile_Metrics_NotFoundIsSuccessOutcome` — IsNotFound is outcome=success
  - `TestProviderReconcile_Metrics_MissingRuntimeIsErrorOutcome` — missing `spec.runtime` is outcome=error + `errors_total{reason="runtime-spec-invalid"}`
  - `TestProviderReconcile_Metrics_DurationHistogramFires` — smoke that the histogram observes at least one sample under `kind="Provider"`

### Why
Second in the G-track sequence (after G1 / PR #101). `ProviderReconciler` reconciles Provider CRs into per-Provider Deployments + Services + Secrets, and its failure modes (missing runtime spec, Service/Deployment reconcile errors) are operationally distinct from VM reconcile failures. Per-Kind reconcile counters and a dedicated reason taxonomy let operators dashboard "Provider lifecycle health" separately from "VM lifecycle health."

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image needed to emit)
- [ ] Config change only
- [ ] Documentation only

Targeted for **v0.3.5** alongside G1 (already merged), G3, G4, G5.

### Verification
Locally:
```bash
go test -v -count=1 -run TestProviderReconcile_Metrics ./internal/controller/
# PASS: 3 tests
make test  # controller coverage 21.6% -> 22.4%
make test-integration  # still green
```

Post-deploy (after the full G-track lands + v0.3.5-rc1 deploys):
```bash
curl /metrics | grep '^virtrigaud_manager_reconcile_total{kind="Provider"'
# expect non-zero samples for outcome=success|requeue|error
```

### References
- Closes #88
- Umbrella: #86
- Pattern established by: #101 (G1)
- PROJECT_CONTEXT.md G-track item G2

---

## [2026-05-23 11:26] - feat(obs): instrument VirtualMachineReconciler with reconcile timer + structured error counter (closes #87)
**Author:** @williamrizzo (William Rizzo)

### Added
- `internal/controller/virtualmachine_controller.go`: Reconcile entry now records a per-call timer to `virtrigaud_manager_reconcile_total{kind="VirtualMachine",outcome=...}` + `virtrigaud_manager_reconcile_duration_seconds{kind="VirtualMachine"}` via a single deferred block that infers outcome from named return values (`success` / `error` / `requeue`).
- `internal/controller/virtualmachine_controller.go`: 11 `metrics.RecordError(reason, ComponentManager)` calls at the documented error sites in Reconcile + reconcileVM + handleDeletion, emitting `virtrigaud_errors_total{reason=...,component="manager"}`. Structured reason taxonomy declared as `errReason*` constants at the top of the file: `get-vm`, `add-finalizer`, `remove-finalizer`, `deps-not-found`, `deps-error`, `provider-resolve`, `provider-validate`, `provider-describe`, `provider-task-status`, `provider-delete`.
- `internal/controller/virtualmachine_metrics_test.go`: New file. 3 focused unit tests using `client/fake` (no envtest needed):
  - `TestReconcile_Metrics_NotFoundIsSuccessOutcome` — reconciling a non-existent VM (IsNotFound) records outcome=success
  - `TestReconcile_Metrics_MissingDepsIsRequeueOutcome` — reconciling a VM whose Provider is missing records outcome=requeue + `errors_total{reason="deps-not-found"}` (Provider-not-found is recoverable transient state, NOT outcome=error noise)
  - `TestReconcile_Metrics_DurationHistogramFires` — smoke that the duration histogram observes at least one sample per reconcile

### Why
v0.3.3 wired up the metrics infrastructure (registry binding PR #83, SetupMetrics PR #84, accurate labels PR #85, integration-test CI gate PR #98) but only `virtrigaud_build_info` emitted samples. This change starts populating the 11 other registered metric families by instrumenting the primary work loop — `VirtualMachineReconciler` — which is expected to be the loudest emitter under normal operation. After this PR deploys, operators scraping `/metrics` will see non-zero `virtrigaud_manager_reconcile_total{kind="VirtualMachine",...}` samples and a populated `virtrigaud_errors_total` histogram from real reconcile activity.

The dual-channel design (outcome timer + structured error counter) was deliberate: the outcome label answers "was this reconcile a success/error/requeue from controller-runtime's perspective", and the error reason answers "WHY". A Provider-not-found situation is `outcome=requeue` (we asked to come back in 30s, not an error) AND `errors_total{reason="deps-not-found"}` (we want to dashboard the volume of missing-dep blocking states). These don't always overlap — a `deps-error` from a k8s API issue produces `outcome=requeue + reason=deps-error`, while a finalizer add failure produces `outcome=error + reason=add-finalizer`.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image needed for the metrics to start emitting)
- [ ] Config change only
- [ ] Documentation only

Targeted for **v0.3.5** alongside G2-G5 (other reconcilers + gRPC middleware + circuit breaker).

### Verification
Locally:
```bash
go test -v -count=1 -run TestReconcile_Metrics ./internal/controller/
# PASS: 3 tests
make test  # full unit suite still green; controller coverage 20.8% -> 21.6%
make test-integration  # still green; no integration regressions
```

Post-deploy (after merging the G-track sequence and cutting v0.3.5-rc1):
```bash
kubectl -n virtrigaud-system port-forward deploy/virtrigaud-manager 8080:8080 &
curl -s :8080/metrics | grep '^virtrigaud_manager_reconcile_total{kind="VirtualMachine"'
# expect one or more samples with outcome=success|error|requeue
curl -s :8080/metrics | grep '^virtrigaud_errors_total{component="manager"'
# expect samples for whichever error reasons actually fired during cluster operation
```

### References
- Closes #87
- Umbrella: #86
- PROJECT_CONTEXT.md G-track item G1
- Companion PRs (G2-G5) coming next

---

## [2026-05-23 11:05] - Fix circuit-breaker half-open transition count + harden timing-fragile test (closes #96, #97)
**Author:** @williamrizzo (William Rizzo)

### Fixed
- `internal/resilience/circuitbreaker.go`: `allowCall()` now counts the Open→HalfOpen transition itself as the first half-open call. Previously the transition reset `halfOpenCalls=0` and returned `true` without incrementing, so a circuit configured with `HalfOpenMaxCalls=N` actually required `N+1` successful calls to transition back to Closed instead of `N`. Implementation: `fallthrough` from the StateOpen case into the StateHalfOpen case so the same `halfOpenCalls++` logic runs uniformly. Closes #96.
- `test/integration/observability_test.go`: `TestCombinedResiliencePolicy` now uses `ResetTimeout: 30 * time.Second` (was `50 * time.Millisecond`). The old value was shorter than the test's own retry-loop runtime, so by the time the "Next call should be fast-failed" assertion ran, the reset timeout had already elapsed and the circuit had correctly transitioned to half-open — invalidating the test's premise. The fix decouples the test from wall-clock timing; it was a test bug, not a behavioral bug in the circuit breaker. Closes #97.

### Added
- `internal/resilience/circuitbreaker_test.go`: New file. 4 focused unit tests covering the half-open contract:
  - `TestHalfOpenTransitionCountsAsCall` — the regression canary for #96
  - `TestOpenStateRejectsCallsBeforeResetTimeout` — pins "fast-fail when open" contract, asserts the returned error is a retryable ProviderError
  - `TestHalfOpenFailureReOpensCircuit` — a single failure in half-open re-opens the circuit, regardless of HalfOpenMaxCalls
  - `TestHalfOpenRejectsAfterMaxCalls` — once HalfOpenMaxCalls is reached, additional calls are rejected until recordSuccess transitions to Closed
- `test/integration/observability_test.go`: Removed `t.Skip("blocked by #96")` on `TestCircuitBreakerIntegration` and `t.Skip("blocked by #97")` on `TestCombinedResiliencePolicy`. Both now run in `make test-integration` (which is gated in CI as of PR #98).

### Why
Both issues were discovered when `test/integration/observability_test.go` was wired into CI in PR #98 (which closed #93). #96 is a real code bug — minor production impact (circuits stay half-open one call longer than configured before transitioning to closed) but worth fixing for correctness and to make the codebase match its documented behavior. #97 is a pure test bug with zero production impact — the circuit breaker correctly enforces open-state for the configured `ResetTimeout` window; the test was just timing-fragile.

Bundled into a single PR because both touch the same subsystem (`internal/resilience/circuitbreaker.go` + its integration tests), the fixes are small, and the new unit-test suite covers behavior shared by both.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (next manager image will include the corrected half-open transition behavior — circuits recover one call faster than before, otherwise unchanged)
- [ ] Config change only
- [ ] Documentation only

Not a security fix. Targeted for **v0.3.5** alongside the G-track metrics instrumentation.

### Verification
```bash
go test -v -run "TestHalfOpen|TestOpenState" ./internal/resilience/
# PASS: 4 tests
go test -v -run "TestCircuitBreakerIntegration|TestCombinedResiliencePolicy" ./test/integration/
# PASS: 2 tests (formerly skipped)
make test-integration
# ok ... all tests pass, no skips remaining for these two
```

### References
- Closes #96, Closes #97
- Discovered by PR #98 / commit `3f49026` (CI wiring)
- PROJECT_CONTEXT.md tracks K2 + K3

---

## [2026-05-23 09:23] - SECURITY: fix logging.Redact to mask values, not field names (closes #95)
**Author:** @williamrizzo (William Rizzo)

### Security
- `internal/obs/logging/logging.go`: `Redactor.Redact` (and the convenience wrappers `RedactString`, `RedactMap`) previously replaced the FIRST capture group of every matched pattern. For two-group kv-pair patterns like `(password|token|...)\s*[:=]\s*([^\s]+)`, this meant the field NAME (group 1) was masked while the SECRET VALUE (group 2) survived in cleartext. Input `"password=hunter2"` produced `"[REDACTED]=hunter2"`.
- Fix: replace the LAST capture group, by documented convention "the value is always the last group". Works correctly for both 1-group patterns (URL passwords) and 2-group patterns (kv-pairs); 0-group patterns continue to replace the entire match.

### Added
- `internal/obs/logging/logging_test.go`: New file. 6 tests with 22+ sub-cases covering:
  - `TestRedactStringValuesNotKeys`: 13 sub-cases (equals/colon separators, `api_key` underscore + hyphen variants, case-insensitivity, all `password|passwd|pwd|token|secret` aliases, quoted values, multiple kv-pairs in one string, URL-embedded passwords incl. `qemu+ssh://`).
  - `TestRedactStringNonSensitiveInputUntouched`: confirms harmless content passes through unchanged.
  - `TestRedactMap`: confirms `RedactMap` masks whole values for sensitive keys, recursively masks values for non-sensitive keys, leaves harmless map entries alone.
  - `TestRedactStringSSHPublicKey`, `TestIsSensitiveKey`, `TestRedactStringIdempotent`.

### Changed
- `test/integration/observability_test.go`: Removed the `t.Skip("blocked by #95 ...")` on `TestObservabilityIntegration`. The integration assertion `assert.NotContains(t, redacted, "secret123")` now passes against the fixed implementation.

### Why
Discovered during PR #98 (wiring `test/integration/` into CI) on 2026-05-23. `TestObservabilityIntegration` had been failing silently in the disabled CI job since the function shipped. The bug is real but has **zero production callsites today** — no code in `internal/controller/`, `internal/providers/`, or `internal/transport/` calls any redaction function. So no operator's v0.3.3 logs contain leaked secrets via this path. Still shipping as a security fix because (a) the function name implies behavior the implementation didn't deliver, giving any future caller a false sense of security; (b) banking-compliance posture demands rapid response to security findings even when current exposure is dormant; (c) the fix is small and isolated.

Cut as v0.3.3.1 patch release (not rolled into v0.3.4 alongside the G-track) for clean per-incident release auditability.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image for the redaction behavior to take effect — only matters once a code path actually calls Redact)
- [ ] Config change only
- [ ] Documentation only

### Verification
Locally:
```bash
go test -v -run TestRedact ./internal/obs/logging/
# PASS: 13 sub-tests
go test -v -run TestObservabilityIntegration ./test/integration/
# PASS: now un-skipped; log output proves the fix:
#   Original: password=secret123 and api_key=abcdef
#   Redacted: password=[REDACTED] and api_key=[REDACTED]
```

### References
- Closes #95
- Discovered by PR #98 (wiring observability_test into CI)
- PROJECT_CONTEXT.md track K1 (renamed from previous "Active TODO" entry)

---

## [2026-05-23 09:07] - Wire observability integration test into CI (closes #93)
**Author:** @williamrizzo (William Rizzo)

### Added
- `Makefile`: New `test-integration` target — `go test -race -coverprofile=cover-integration.out ./test/integration/...`. Distinct from `make test` (unit, excludes integration) and `make test-e2e` (kind cluster + ginkgo).
- `.github/workflows/ci.yml`: The existing `integration` job now actually runs `make test-integration` instead of `echo "Skipping..."`. Removed the stale Kind cluster + CRD install steps (the only active test under `test/integration/` is `observability_test.go`, which has no Kubernetes API dependency). Added codecov upload tagged `integration`.

### Fixed
- `.github/workflows/ci.yml`: Replaced the no-op `echo "Skipping integration tests in CI due to libvirt dependencies"` step. The "libvirt dependencies" claim was stale — `test/integration/observability_test.go` only imports `internal/obs/*` + `internal/providers/contracts` + `internal/resilience` (no cgo, no libvirt). The disabled file (`vm_lifecycle_test.go.disabled`) was the libvirt-dependent one but Go ignores files not ending in `.go`.

### Security
- This PR surfaces, but does not fix, a critical pre-existing security bug in `internal/obs/logging.RedactString`: the function redacts field NAMES rather than VALUES, so `"password=secret123"` becomes `"[REDACTED]=secret123"` with the secret in cleartext. Tracked as **#95 (P0)**. `TestObservabilityIntegration` is skipped in this PR pending #95's fix.

### Test infrastructure
- `test/integration/observability_test.go`: Marked 3 pre-existing failing tests with `t.Skip(...)` and explicit issue references:
  - `TestObservabilityIntegration` — blocked by #95 (security: RedactString redacts keys not values)
  - `TestCircuitBreakerIntegration` — blocked by #96 (state assertion mismatch)
  - `TestCombinedResiliencePolicy` — blocked by #97 (circuit-open not enforced — possible behavioral bug)
- 4 tests now run in CI on every PR: `TestMetricsIntegration`, `TestHealthSystem`, `TestRetryIntegration`, `TestObservabilityEndToEnd`. Each follow-up PR (#95, #96, #97) un-skips its test as it lands.

### Why
The v0.3.0 → v0.3.2 → v0.3.3-rc1 metrics regression went undetected for three minor releases because the test that catches it (`TestMetricsIntegration`) lives under `test/integration/` and `make test` explicitly excludes that path. CI ran a no-op for the integration job. This PR closes that gap so future metric-binding (and broader observability) regressions are caught at PR time.

Discovering 3 pre-existing failures the moment we wired the gate up — including a security-relevant one — validates the choice to land H3 before the G-track instrumentation work begins.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

Affects CI only. Future PRs gain integration-test gating; the 3 skipped tests are tracked by their own issues and will un-skip in their fix PRs.

### References
- Closes #93
- Discovered: #95 (security, P0), #96, #97
- PROJECT_CONTEXT.md track H3

---

## [2026-05-22 18:27] - Inject VERSION and GIT_SHA into manager binary via ldflags
**Author:** @williamrizzo (William Rizzo)

### Fixed
- `build/Dockerfile.manager`: Added `ARG VERSION=dev` and `ARG GIT_SHA=unknown` plus `-ldflags="-s -w -X 'github.com/projectbeskar/virtrigaud/internal/version.Version=${VERSION}' -X 'github.com/projectbeskar/virtrigaud/internal/version.GitSHA=${GIT_SHA}'"` on the `go build` step. The container image now reports accurate version metadata at runtime.
- `.github/workflows/release.yml`: Added `build-args: VERSION / GIT_SHA` to the `docker/build-push-action` step so the release tag and commit SHA are passed into the container build. Dockerfiles that don't declare these ARGs (kubectl image, current provider images) silently ignore them — no impact.

### Why
v0.3.3-rc3 smoke test on `vr1.lab.k8` showed `virtrigaud_build_info{component="manager",git_sha="unknown",go_version="go1.25.10",version="dev"} 1` on `/metrics` instead of `version="v0.3.3-rc3"` and the real SHA. The metrics pipeline (registry binding from PR #83 + SetupMetrics call from PR #84) was working — the binary itself had never been built with ldflags to populate `internal/version`. Latent bug since the project's inception, but only became visible once `virtrigaud_build_info` was actually exposed. `virtrigaud_build_info` is the canonical way for operators to know which manager version is running; reporting `version="dev"` to Prometheus dashboards is actively misleading during incident response, which matters for the project's banking-grade operational posture.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image needed for labels to update)
- [ ] Config change only
- [ ] Documentation only

### Verification
After deploying the new image:
```bash
kubectl -n virtrigaud-system port-forward deploy/virtrigaud-manager 8080:8080 &
curl -s http://localhost:8080/metrics | grep '^virtrigaud_build_info'
# expect:
# virtrigaud_build_info{component="manager",git_sha="<rc4-commit-sha>",go_version="...",version="v0.3.3-rc4"} 1
```

Local sanity check:
```bash
go build -ldflags="-s -w \
  -X 'github.com/projectbeskar/virtrigaud/internal/version.Version=v0.3.3-rc4-sanity' \
  -X 'github.com/projectbeskar/virtrigaud/internal/version.GitSHA=abc1234'" \
  -o /tmp/manager ./cmd/manager/
strings /tmp/manager | grep -E 'v0\.3\.3-rc4-sanity|abc1234'
# both literals present in the resulting binary
```

---

## [2026-05-22 15:29] - Emit virtrigaud_build_info on manager startup
**Author:** @williamrizzo (William Rizzo)

### Fixed
- `cmd/manager/main.go`: Call `metrics.SetupMetrics(version.Version, version.GitSHA, metrics.ComponentManager)` immediately after logger setup. Without this call the build_info `GaugeVec` from `internal/obs/metrics` stays empty after package init, and the `virtrigaud_build_info` family does NOT appear in the manager's `/metrics` output even with the PR #83 registry-binding fix correctly applied.

### Why
v0.3.3-rc2 deploy + smoke test on `vr1.lab.k8` exposed that PR #83 was a necessary-but-not-sufficient fix. The registry binding is structurally correct (verified by `internal/obs/metrics/metrics_test.go`) but Prometheus Vec metrics with zero observations do not appear in `/metrics` output. The only metric that wouldn't depend on reconcile activity is `virtrigaud_build_info`, which is populated by `SetupMetrics`. Nothing in production called `SetupMetrics`, so the family stayed empty and the smoke test continued to report `0 virtrigaud_* metric families` after upgrading to rc2 — same as rc1.

This change makes the manager emit a `virtrigaud_build_info{version,git_sha,go_version,component}` sample on startup, providing the canary metric that release smoke tests can rely on, and serving as proof end-to-end that the registry binding works in production. Per-reconciler instrumentation (VirtualMachineReconciler, ProviderReconciler) for full operational visibility is deferred to v0.3.4.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager pod restart picks up the new SetupMetrics call)
- [ ] Config change only
- [ ] Documentation only

### Verification
After deploying the fix:
```bash
curl -s http://<manager>:8080/metrics | grep '^virtrigaud_build_info'
# expect: virtrigaud_build_info{component="manager",git_sha="<sha>",go_version="go1.25.x",version="v0.3.3-rc3"} 1
```

---

## [2026-05-22 06:38] - Fix virtrigaud_* metrics not exposed on /metrics endpoint
**Author:** @williamrizzo (William Rizzo)

### Fixed
- `internal/obs/metrics/metrics.go`: Bind all 12 virtrigaud Prometheus metric vectors to controller-runtime's `metrics.Registry` (the registry served by the manager on `/metrics`) instead of promauto's default global registry. Previously the metrics were created in-process but never exposed because the manager's `/metrics` endpoint serves controller-runtime's registry, not promauto's default. Use `promauto.With(ctrlmetrics.Registry).New*` for: `virtrigaud_build_info`, `virtrigaud_manager_reconcile_total`, `virtrigaud_manager_reconcile_duration_seconds`, `virtrigaud_queue_depth`, `virtrigaud_vm_operations_total`, `virtrigaud_provider_rpc_requests_total`, `virtrigaud_provider_rpc_latency_seconds`, `virtrigaud_provider_tasks_inflight`, `virtrigaud_errors_total`, `virtrigaud_ip_discovery_duration_seconds`, `virtrigaud_circuit_breaker_state`, `virtrigaud_circuit_breaker_failures_total`.

### Removed
- `internal/obs/metrics/metrics.go`: Removed dead `Init()` function which had a misleading comment claiming promauto registered metrics with the controller-runtime registry; it had zero callers anywhere in the codebase.

### Added
- `internal/obs/metrics/metrics_test.go`: New unit tests `TestMetricsRegisteredInControllerRuntimeRegistry` and `TestSetupMetricsEmitsBuildInfo` asserting all 12 metric families appear in `GetRegistry().Gather()` after one observation each, and that `virtrigaud_build_info` carries the correct label values. These run under `make test` and serve as a regression canary.
- `test/integration/observability_test.go`: Added missing `metrics.SetupMetrics(...)` call at the top of `TestMetricsIntegration` so the test is self-contained rather than relying on side effects from other tests. The integration test now passes when run directly (`go test ./test/integration/...`), though `make test` continues to exclude that path.

### Why
v0.3.3-rc1 smoke test on `vr1.lab.k8` confirmed that scraping `/metrics:8080` returned 41 metric families — all controller-runtime/workqueue/certwatcher/go_/process_ — with zero `virtrigaud_*` series. Root cause: `promauto.NewGaugeVec(...)` etc. bind to `prometheus.DefaultRegisterer`, which the manager does not serve. The bug shipped in v0.3.0 through v0.3.2 (originally introduced in commit `a849271 implement observability extensions`) and would have shipped in v0.3.3 without this fix. Observability is load-bearing for the project's banking-compliance posture; releasing without exposed metrics is a regression on every downstream operator that relies on Prometheus scraping. This change blocks v0.3.3 release until merged (cut v0.3.3-rc2 from this commit).

### Impact
- [ ] Breaking change (no public API change; vector variables are unexported and callers use the helper structs)
- [x] Requires cluster rollout (`virtrigaud_*` metrics begin appearing once the new manager image is deployed)
- [ ] Config change only
- [ ] Documentation only

### Verification
After deploying the fix:
```bash
kubectl -n virtrigaud-system port-forward deploy/virtrigaud-controller-manager 8080:8080 &
curl -s http://localhost:8080/metrics | grep -c '^virtrigaud_'
# expect: 12 or more (one line per metric family + samples)
curl -s http://localhost:8080/metrics | grep '^virtrigaud_build_info'
# expect: virtrigaud_build_info{component="manager",git_sha="...",go_version="go...",version="v0.3.3-rc2"} 1
```

---

## [2026-03-17 22:10] - Add SCSI Controller Configuration for Additional Disks
**Author:** @firestoned (Erick Bourgeois)

### Added
- `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`: New `SCSIControllerSpec` type for SCSI controller configuration
  - `controller`: Specify SCSI bus number (0-3) for the disk
  - `sharedBus`: Configure bus sharing mode (noSharing, virtualSharing, physicalSharing)
  - `controllerType`: Select controller type (lsilogic, buslogic, lsilogic-sas, pvscsi)
- `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`: Added optional `SCSI` field to `DiskSpec`
- `internal/providers/vsphere/server.go`: New `createSCSIController()` function to create SCSI controllers dynamically
- `internal/providers/vsphere/server.go`: Enhanced `attachAdditionalDisk()` to support multiple SCSI controllers
  - Automatically creates new SCSI controllers if specified controller doesn't exist
  - Tracks controllers by bus number for precise disk placement
  - Supports up to 4 SCSI controllers (bus 0-3) with 15 disks each (60 disks total)

### Changed
- `internal/providers/vsphere/server.go`: `AdditionalDiskSpec` now includes SCSI configuration fields
- `internal/providers/vsphere/server.go`: `parseCreateRequest()` parses SCSI configuration from DisksJson
- `internal/providers/vsphere/server.go`: Disk attachment logic refactored to support controller selection

### Why
Users need to attach disks to specific SCSI controllers with custom configurations
(e.g., pvscsi with virtualSharing for RDM disks, or separate controllers for different
disk types). Previously, all disks were attached to the first available SCSI controller
(bus 0), limiting flexibility for advanced storage configurations.

### Usage
```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: multi-controller-vm
spec:
  classRef:
    name: large
  imageRef:
    name: rhel9-template
  providerRef:
    name: vsphere
  disks:
    # Disks on default controller (bus 0)
    - name: data-disk-1
      sizeGiB: 100
      type: thin

    # Disks on dedicated pvscsi controller (bus 1)
    - name: db-disk-1
      sizeGiB: 500
      type: eagerzeroedthick
      scsi:
        controller: 1
        controllerType: pvscsi
        sharedBus: noSharing

    - name: db-disk-2
      sizeGiB: 500
      type: eagerzeroedthick
      scsi:
        controller: 1  # Same controller as db-disk-1
```

This automatically creates SCSI controller 1 (pvscsi, noSharing) if it doesn't exist,
then attaches both db disks to it.

### Impact
- [ ] Breaking change
- [x] New feature - SCSI controller configuration
- [ ] Requires cluster rollout
- [x] CRD update required - includes new `scsi` field in DiskSpec

## [2026-03-17 21:55] - Add 'vm' ShortName Alias for VirtualMachine CRD
**Author:** @firestoned (Erick Bourgeois)

### Added
- `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`: Added `+kubebuilder:resource:shortName=vm` marker
- `config/crd/bases/infra.virtrigaud.io_virtualmachines.yaml`: Generated CRD now includes `vm` as a shortName

### Changed
- VirtualMachine CRD now supports `kubectl get vm` as an alias for `kubectl get virtualmachines`

### Why
Users frequently need to query VirtualMachines and typing `virtualmachines` is verbose. Following
Kubernetes conventions (like `po` for pods, `svc` for services), adding a `vm` shortName improves
user experience and command-line efficiency.

### Usage
After applying the updated CRD:
```bash
kubectl get vm              # Instead of kubectl get virtualmachines
kubectl get vm -A           # List all VMs across namespaces
kubectl describe vm foo     # Describe a specific VM
kubectl delete vm bar       # Delete a VM
```

### Impact
- [ ] Breaking change
- [x] CRD update required - run `kubectl apply -f config/crd/bases/infra.virtrigaud.io_virtualmachines.yaml`
- [ ] Requires cluster rollout
- [ ] Config change only

## [2026-03-17 21:30] - Fix Datastore Name Resolution for Additional Disks
**Author:** @firestoned (Erick Bourgeois)

### Fixed
- `internal/providers/vsphere/server.go`: Additional disk attachment failing with "Invalid configuration for device '0'"
  - Issue: `resolveDatastoreFromStoragePod()` returned `object.NewDatastore()` which doesn't populate the Name property
  - When `datastore.Name()` was called in `attachAdditionalDisk()`, it returned empty string
  - Resulted in invalid disk path: `[] vm-name/disk.vmdk` instead of `[datastore-name] vm-name/disk.vmdk`
  - Fixed by using `p.finder.Datastore(ctx, best.Name)` which properly fetches datastore with all properties

### Why
Additional disks specified in VirtualMachine `spec.disks` were being parsed and attachment was attempted,
but all attachments failed with "Invalid configuration for device '0'" because the datastore path was malformed.
The datastore name was empty in the path string, causing vSphere to reject the disk configuration.

### Impact
- [x] Bug fix - additional disks can now attach successfully
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only

## [2026-03-17 19:00] - Add Comprehensive Disk Debugging and VM Existence Check
**Author:** @firestoned (Erick Bourgeois)

### Added
- `internal/controller/virtualmachine_controller.go`: Verbose-level logging for disk configuration
  - Logs count and details of all additional disks being sent to provider using `log.V(1).Info()`
  - Logs image source resolution details (Libvirt, vSphere, Proxmox) at verbose level
  - Helps identify if disk specs are properly reaching the create request
- `internal/transport/grpc/client.go`: Debug output for DisksJson transmission
  - Prints DisksJson being sent over gRPC to provider
  - Useful for troubleshooting marshaling issues
- `internal/providers/vsphere/server.go`: VM existence check before creation
  - Checks if VM already exists before attempting creation
  - Returns existing VM ID if found, avoiding "name already exists" errors
  - Useful for recovering from failed creation attempts

### Changed
- `internal/providers/vsphere/server.go`: Enhanced debug-level logging throughout disk operations
  - Logs DisksJson receipt in Create method using `p.logger.Debug()`
  - Logs parsed VMSpec with disk count using Debug log level
  - Detailed per-disk logging during attachment with index, name, size, and type
  - Separate logging for success (Debug) vs failure (Error) of each disk attachment
  - Uses proper slog Debug/Error methods
- `internal/controller/virtualmachine_controller.go`: Uses logr verbose logging pattern
  - Converted debug logging from `log.Info("DEBUG ...")` to proper `log.V(1).Info()`
  - Follows controller-runtime's logging conventions for verbose/debug output
  - Verbose logs only appear when log level is set to 1 or higher

### Why
Users were experiencing "name already exists" errors when VM creation failed mid-process,
leaving a partially-created VM that blocked subsequent reconciliation attempts. Additionally,
there was insufficient visibility into whether disk specifications were being properly
transmitted and processed through the controller → gRPC → provider pipeline.

Debug logging was using Info level with "DEBUG" prefix strings, which made logs noisy and
couldn't be controlled by log level settings. Now uses proper debug/verbose logging that
can be enabled/disabled via log level configuration.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Debugging improvements - better error recovery and visibility
- [ ] Config change only

**To enable verbose logging:**
- Controller: Set `--zap-log-level=1` or higher
- Provider: Uses slog Debug level (enabled via LOG_LEVEL=debug environment variable)

---

## [2026-03-17 16:00] - Implement Additional Disks Support in vSphere Provider
**Author:** @firestoned (Erick Bourgeois)

### Added
- `internal/providers/vsphere/server.go`: Complete support for additional disks beyond root disk
  - Added `AdditionalDisks` field to `VMSpec` struct to store additional disk specifications
  - Added `AdditionalDiskSpec` type to define disk name, size, and provisioning type
  - Implemented `attachAdditionalDisk()` helper function to attach disks to existing VMs
  - Added parsing of `DisksJson` from gRPC CreateRequest in `parseCreateRequest()`
  - Integrated disk attachment logic into `createVirtualMachine()` workflow

### Changed
- `internal/providers/vsphere/server.go`: Additional disks now attached after root disk resize, before power-on
  - Automatically finds available SCSI controller slots (unit numbers 0-15, excluding 7)
  - Supports thin, thick, and eager-zeroed-thick provisioning types
  - Creates disk files with naming convention: `vm-name_N.vmdk` (N = disk index)
  - Logs detailed information about each disk attachment operation

### Fixed
- **CRITICAL BUG**: `spec.disks` in VirtualMachine CRD was completely ignored by vSphere provider
  - DisksJson was sent via gRPC but never parsed by the provider
  - Users could specify additional disks in YAML but they were silently not created
  - Now all disks specified in `spec.disks` are properly created and attached

### Why
The VirtualMachine CRD has always supported additional disks via `spec.disks` (up to 20 disks),
but the vSphere provider implementation was incomplete. DisksJson was marshaled and sent by the
controller, transmitted via gRPC, but never parsed or acted upon by the provider. This meant:
- VMs were created with only the root disk (from VMClass.DiskDefaults)
- Additional storage requirements were silently ignored
- No error or warning was generated, making it appear the feature worked

This fix completes the implementation by parsing DisksJson and attaching each specified disk
to the VM using vSphere's Reconfigure API after VM creation.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Feature completion - additional disks now work as documented
- [ ] Config change only

**Example Usage:**
```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: database-server
spec:
  classRef:
    name: standard  # Root disk from class defaults (e.g., 20GB)
  disks:
    - name: data-disk
      sizeGiB: 100
      type: thin
    - name: logs-disk
      sizeGiB: 50
      type: thick
```

Result: VM will have 3 disks (root + 2 additional) attached to SCSI controller.

---

## [2026-01-21 18:00] - Align MetaData Structure with UserData Pattern
**Author:** @firestoned (Erick Bourgeois)

### Changed
- `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`: Restructured MetaData type to match UserData pattern
  - Changed from `spec.metaData.inline` to `spec.metaData.cloudInit.inline`
  - Added new `CloudInitMetaData` type with `inline` and `secretRef` fields
  - Updated `MetaData` type to nest `CloudInit` configuration
- `internal/controller/virtualmachine_controller.go`: Updated metaData access to use nested structure
- `docs/CLOUD_INIT.md`: Updated all examples to use `metaData.cloudInit.inline` syntax
- `CHANGELOG.md`: Updated example YAML to reflect new structure

### Why
Provides consistent API pattern between userData and metaData fields. Both now follow the same structure:
- `spec.userData.cloudInit.inline`
- `spec.metaData.cloudInit.inline`

This makes the API more intuitive and easier to learn.

### Impact
- [x] Breaking change (requires updating existing VirtualMachine manifests)
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

**Migration:** Users must update their VirtualMachine manifests from:
```yaml
metaData:
  inline: |
    instance-id: vm-001
```
To:
```yaml
metaData:
  cloudInit:
    inline: |
      instance-id: vm-001
```

---

## [2026-01-21 17:30] - Fix Duplicate ProviderReconciler Registration
**Author:** @firestoned (Erick Bourgeois)

### Fixed
- `cmd/main.go`: Removed duplicate ProviderReconciler registration (lines 258-263)
  - ProviderReconciler was registered twice, causing "controller with name provider already exists" error
  - First registration at line 232 is correct and retained
  - Duplicate registration removed to allow manager to start successfully

### Why
The manager was failing to start with error: "controller with name provider already exists. Controller names must be unique to avoid multiple controllers reporting the same metric."

### Impact
- [x] Breaking change (fixes manager startup failure)
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

---

## [2026-01-21] - Add MetaData Support for Cloud-Init Metadata
**Author:** @firestoned (Erick Bourgeois)

### Added
- `VirtualMachine.spec.metaData`: New field for cloud-init metadata configuration
  - Supports inline YAML format (similar to userData)
  - Sent to vSphere via `guestinfo.metadata`
  - Falls back to default `{"instance-id": "<vm-name>"}` if not provided
- `MetaData` type in CRD API with `inline` and `secretRef` support
- `MetaData` support in contracts and controller
- vSphere provider now accepts custom cloud-init metadata
- **Documentation**:
  - New comprehensive guide: [docs/CLOUD_INIT.md](docs/CLOUD_INIT.md)
  - Updated CRD reference: [docs/CRDs.md](docs/CRDs.md)
  - New example: [docs/examples/cloud-init-with-metadata.yaml](docs/examples/cloud-init-with-metadata.yaml)
  - Updated main README with Cloud-Init Configuration link

### Changed
- vSphere `addCloudInitToConfigSpec` now accepts metadata parameter
- Default metadata encoding is JSON, custom metadata uses YAML encoding
- `VMCreateSpec` includes `CloudInitMetaData` field
- CRD documentation now includes `metaData` field specification

### Why
Allows users to customize cloud-init metadata beyond the default instance-id. This is useful for:
- Custom instance configuration
- Network configuration via metadata
- Vendor data and other cloud-init metadata features
- Organizing VMs with custom metadata fields (region, environment, project, etc.)

### Documentation
Complete guide now available at [docs/CLOUD_INIT.md](docs/CLOUD_INIT.md) with:
- Explanation of UserData vs MetaData
- Basic and advanced examples
- Kubernetes Secret integration
- VMware vSphere implementation details
- Best practices and troubleshooting
- Network configuration examples

### Example
```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: my-vm
spec:
  metaData:
    cloudInit:
      inline: |
        instance-id: custom-id
        local-hostname: my-custom-hostname
        network:
          version: 2
          ethernets:
            eth0:
              dhcp4: true
  userData:
    cloudInit:
      inline: |
        #cloud-config
        users:
          - name: admin
```

## [0.3.0] [2026-01-10 19:00] - VM Adoption Feature and Infrastructure Improvements
**Author:** @wrkode (William Rizzo)

### Added

#### VM Adoption
- VM adoption controller for discovering and managing existing VMs not created by VirtRigaud
- Annotation-driven adoption via `virtrigaud.io/adopt-vms: "true"`
- Flexible filtering system via `virtrigaud.io/adopt-filter` annotation with support for:
  - Name pattern matching (regex)
  - Power state filtering
  - CPU and memory range filtering
- Automatic VirtualMachine CR creation for discovered unmanaged VMs
- Automatic VMClass generation based on discovered VM properties
- Adoption status tracking in Provider status with discovery time, counts, and error messages
- Provider interface extension with `ListVMs()` method implemented across all providers (vSphere, Libvirt, Proxmox, Mock)
- gRPC API extension with `ListVMs` RPC and VMInfo message types

#### Provider Status Enhancements
- `ConnectedVMs` count field in Provider status showing number of managed VMs
- `Healthy` status field indicating provider availability
- Status fields now properly populate in `kubectl get providers` output

### Fixed

#### GitHub Pages Conflict
- Resolved conflict between documentation and Helm chart hosting
- Documentation now served from `/docs` subdirectory
- Helm charts remain at root for backward compatibility
- Both coexist in `gh-pages` branch without conflicts

### Changed

#### Documentation
- Updated docs workflow to publish to `gh-pages/docs/` directory
- Updated release workflow to preserve docs directory when publishing charts
- Added comprehensive VM Adoption guide (`docs/src/VM_ADOPTION.md`)
- Added VM adoption examples (`docs/src/examples/vm-adoption-example.yaml`)
- Updated installation guides with correct GitHub Pages URLs

#### API Changes
- Added `AdoptionStatus` field to `ProviderStatus` with tracking fields
- New Provider annotations: `virtrigaud.io/adopt-vms` and `virtrigaud.io/adopt-filter`

### Why
- Enables onboarding of existing VMs into VirtRigaud management without manual CR creation
- Provides flexible filtering to adopt only specific VMs based on criteria
- Resolves GitHub Pages hosting conflict allowing both docs and charts to coexist
- Improves Provider status visibility for operational monitoring

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] New feature (opt-in via annotations)

---

## [2025-12-16 22:30] - Add Documentation Workflow and CRD API Reference Generation
**Author:** @ebourgeois (Erick Bourgeois)

### Added
- `.github/workflows/docs.yaml`: GitHub Actions workflow for building and deploying documentation
  - Automatically builds documentation on pushes to `main` and PRs
  - Generates CRD API reference from Go types
  - Installs mdBook and mdbook-mermaid preprocessor
  - Deploys to GitHub Pages on main branch
- `docs/crd-ref-docs-config.yaml`: Configuration for CRD API reference generation
- `Makefile`: Enhanced `docs` target to generate CRD API reference and build mdBook
  - Now generates API reference from `api/infra.virtrigaud.io/v1beta1` types
  - Uses `crd-ref-docs` tool for automatic API documentation
  - `docs-build` is now an alias for the comprehensive `docs` target

### Changed
- Documentation workflow now mirrors bindy's approach with mdBook + API reference generation
- `docs` target is now the primary documentation build command (replaces basic `docs-build`)

### Why
- Automates documentation deployment to GitHub Pages for easier access
- Generates CRD API reference directly from Go types (single source of truth)
- Ensures documentation stays in sync with code changes
- Provides CI/CD validation for documentation builds
- Improves developer experience with automatic API documentation

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

---

## [2025-12-05 15:00] - Standardize Project Name Capitalization
**Author:** @eribourg (Erick Bourgeois)

### Changed
- `docs/`: All occurrences of "Virtrigaud" changed to "VirtRigaud" (correct capitalization)
- Applied to all Markdown and TOML files in documentation

### Why
The project name is "VirtRigaud" with capital V and R. Inconsistent capitalization was causing confusion and looked unprofessional in documentation.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

---

## [2025-12-04 14:50] - Add Makefile Targets for Documentation
**Author:** @ebourgeois (Erick Bourgeois)

### Added
- `Makefile`: Documentation build targets
  - `make docs-build`: Build documentation using mdbook
  - `make docs-serve`: Serve documentation with live reload at http://localhost:3000
  - `make docs-clean`: Clean documentation build artifacts
  - `make docs-watch`: Alias for docs-serve

### Changed
- Makefile now includes `##@ Documentation` section with mdbook integration

### Why
Simplifies documentation workflow by providing standard make targets. Users can easily build, serve, and clean documentation without remembering mdbook commands.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

---

## [2025-12-03 14:45] - Add mdbook Configuration
**Author:** @ebourgeois (Erick Bourgeois)

### Added
- `docs/book.toml`: mdbook configuration file for documentation site
- `docs/src/SUMMARY.md`: Table of contents generated from README.md structure
- `docs/.gitignore`: Ignore mdbook build output directory

### Changed
- Documentation structure now supports mdbook for better browsing and navigation
- All documentation files symlinked into `docs/src/` for mdbook compatibility

### Why
mdbook provides a better reading experience for documentation with navigation, search, and a clean interface. This allows users to browse documentation locally or deploy it as a website.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

## [0.3.0] - 2025-11-16

### Major Release: Cross-Provider VM Migration and Advanced Lifecycle Management

This release introduces VM migration capabilities, multi-VM management, and advanced placement policies. VirtRigaud v0.3.0 enables VM migrations between different hypervisor platforms (currently tested: vSphere to Libvirt/KVM) and provides enterprise-grade VM lifecycle management features.

### Added

#### VM Migration (VMMigration CRD)
- **Cross-Provider Migration**: VM migration support between different hypervisor providers (currently tested: vSphere to Libvirt/KVM)
- **PVC-Based Storage**: Kubernetes PersistentVolumeClaims (PVCs) as intermediate storage for migration transfers
- **Automatic Storage Management**: Auto-creation and cleanup of migration PVCs with ReadWriteMany access mode
- **Format Conversion**: Automatic disk format conversion (qcow2, VMDK, raw) during migration
- **Progress Tracking**: Real-time migration progress with phase tracking and percentage completion
- **Checksum Validation**: SHA256 checksum verification for data integrity
- **Migration Phases**: Comprehensive phase tracking (Pending, Validating, Snapshotting, Exporting, Transferring, Converting, Importing, Creating, Validating-Target, Ready, Failed)
- **Retry Policies**: Configurable retry behavior with exponential backoff
- **Validation Checks**: Optional validation checks for disk size, checksum, boot success, and connectivity
- **Snapshot Integration**: Automatic snapshot creation before migration with optional snapshot references
- **Provider Restart Management**: Automatic provider pod restart to mount migration PVCs with graceful shutdown
- **Storage URL Format**: PVC-based storage URLs for provider communication
- **Migration Metadata**: Purpose tracking, project identification, and environment tagging
- **Cleanup Policies**: Configurable cleanup behavior (Always, OnSuccess, Never)
- **Documentation**: Complete migration guide with examples and troubleshooting

#### Multi-VM Management (VMSet CRD)
- **Replica Management**: Declarative management of multiple VM instances with replica count
- **Rolling Updates**: Rolling update strategy for VM sets with configurable max surge and max unavailable
- **OnDelete Strategy**: OnDelete update strategy for manual control
- **Recreate Strategy**: Complete replacement strategy for major updates
- **MinReadySeconds**: Configurable minimum ready time before considering VM ready
- **Revision History**: Configurable retention of old VMSet revisions
- **PVC Retention**: PersistentVolumeClaim retention policies for VM disks
- **Ordinal Management**: Sequential ordering of VM indices with configurable start offset
- **Service Integration**: Service name reference for VM set management
- **Volume Claim Templates**: Template-based PVC creation for VM sets
- **Label Selectors**: Label-based VM selection and matching
- **Status Tracking**: Comprehensive status tracking with ready replicas, updated replicas, and conditions

#### Advanced Placement Policies (VMPlacementPolicy CRD)
- **Hard Constraints**: Mandatory placement constraints for clusters, datastores, hosts, folders, resource pools, networks, zones, and regions
- **Soft Constraints**: Preferred placement constraints with weight-based scoring
- **Anti-Affinity Rules**: VM anti-affinity rules to prevent co-location
- **Affinity Rules**: VM affinity rules to encourage co-location
- **Resource Constraints**: Resource-based placement constraints (CPU, memory, storage)
- **Security Constraints**: Security-based placement constraints for compliance
- **Priority and Weight**: Configurable priority and weight for policy evaluation
- **Label Selectors**: Label-based policy matching for VMs
- **Topology Spread**: Topology spread constraints for distribution across zones
- **Placement Scoring**: Weighted scoring system for placement decisions
- **Policy References**: VM-level placement policy references via PlacementRef

#### ImportedDisk Support
- **ImportedDisk Field**: New VirtualMachine spec field for referencing pre-imported disks from migrations
- **Migration References**: Automatic migration reference tracking in imported disks
- **Disk Metadata**: Format, source, and size metadata for imported disks
- **Type Safety**: Type-safe validation for imported disk references
- **Separation of Concerns**: Clear separation between template-based and disk-based VM creation

#### Provider Enhancements

**vSphere Provider:**
- **Migration Export**: VMDK export support for migration operations
- **Migration Import**: VMDK import support with format conversion
- **Disk Path Fixes**: Corrected disk path handling for migration storage
- **Enhanced Error Handling**: Improved error messages for migration operations

**Libvirt Provider:**
- **Migration Export**: qcow2 export support for migration operations
- **Migration Import**: qcow2 import support with in-place detection
- **Disk Path Fixes**: Fixed disk copy path to use pool directory instead of /tmp
- **In-Place Detection**: Intelligent detection of disks already in pool directory
- **SCP Transfer**: Secure copy protocol support for disk transfers
- **Format Conversion**: qemu-img-based format conversion support

**Proxmox Provider:**
- **Migration Export**: Disk export support for migration operations
- **Migration Import**: Disk import support with format conversion
- **Storage Integration**: Enhanced storage pool integration for migrations

#### Controller Enhancements
- **Migration Controller**: Complete VMMigration controller implementation
- **VMSet Controller**: VMSet controller for multi-VM management
- **Placement Policy Controller**: VMPlacementPolicy controller for advanced placement
- **Provider Restart Coordination**: Automatic provider pod restart coordination for PVC mounting
- **PVC Management**: Automatic PVC creation, mounting, and cleanup
- **Status Reconciliation**: Enhanced status reconciliation for all CRDs
- **Event Recording**: Comprehensive event recording for all operations

#### Storage Layer
- **PVC Storage Backend**: PVC-based storage backend for migrations
- **Storage URL Parsing**: PVC URL parsing and path resolution
- **Mount Path Management**: Automatic mount path management for provider pods
- **Storage Discovery**: Automatic discovery of migration PVCs
- **Volume Mount Management**: Dynamic volume mount management for providers

### Fixed

#### Migration Fixes
- **Disk Path Issue**: Fixed disk copy path in Libvirt provider to use pool directory instead of /tmp
- **In-Place Detection**: Fixed in-place disk detection logic to correctly identify migrated disks
- **VM Creation**: Fixed VM creation to use imported disks instead of creating fresh template copies
- **Data Preservation**: Ensured migrated VM data is preserved during migration
- **PVC Mount Path**: Fixed PVC mount path resolution for provider pods
- **Provider Restart**: Fixed provider restart timing and coordination
- **Storage URL Format**: Corrected storage URL format to include PVC name

#### Provider Fixes
- **Libvirt Disk Import**: Fixed disk import to correctly handle in-place detection
- **vSphere Export**: Fixed VMDK export path handling
- **Proxmox Import**: Fixed disk import path resolution
- **Connection Management**: Improved gRPC connection management during provider restarts
- **Error Handling**: Enhanced error handling for migration operations

#### Controller Fixes
- **PVC Creation**: Fixed PVC creation timing and error handling
- **Provider Reconciliation**: Fixed provider reconciliation trigger mechanism
- **Status Updates**: Fixed status update timing and consistency
- **Event Recording**: Fixed event recording for migration operations

### Enhanced

#### Documentation
- **Migration Guide**: Complete VM migration guide with examples and troubleshooting
- **Migration Architecture**: Detailed migration storage architecture documentation
- **VMSet Documentation**: VMSet usage and examples documentation
- **Placement Policy Guide**: VMPlacementPolicy configuration guide
- **Advanced Lifecycle**: Enhanced advanced lifecycle management documentation
- **API Reference**: Updated API reference with new CRDs

#### Examples
- **Migration Examples**: Complete migration examples for all provider combinations
- **VMSet Examples**: VMSet examples with rolling updates
- **Placement Policy Examples**: VMPlacementPolicy configuration examples
- **Multi-Provider Examples**: Enhanced multi-provider examples

### Technical Details

#### New CRDs
- **VMMigration**: Cross-provider VM migration resource
- **VMSet**: Multi-VM management resource
- **VMPlacementPolicy**: Advanced placement policy resource

#### API Changes
- **VirtualMachine**: Added ImportedDisk field and PlacementRef field
- **Backward Compatibility**: Maintains full backward compatibility with v0.2.x
- **CRD Schemas**: Enhanced CRD schemas with comprehensive validation

#### Storage Architecture
- **PVC-Based Storage**: Per-migration PVC approach with automatic provider restart
- **ReadWriteMany Access**: Requires StorageClass with ReadWriteMany access mode
- **Automatic Cleanup**: Automatic PVC cleanup on migration completion or deletion
- **Provider Restart**: Brief (5-15 second) provider pod restart for PVC mounting

### Deployment Notes

#### Container Images
Updated provider images are available from GitHub Container Registry:
- **Manager**: `ghcr.io/projectbeskar/virtrigaud/manager:v0.3.0`
- **vSphere Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.3.0`
- **LibVirt Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:v0.3.0`
- **Proxmox Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.3.0`
- **Mock Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-mock:v0.3.0`

#### Helm Charts
- **Main Chart**: `virtrigaud/virtrigaud:0.3.0`
- **Provider Runtime Chart**: `virtrigaud/virtrigaud-provider-runtime:0.3.0`

#### Prerequisites
- **StorageClass**: Requires StorageClass with ReadWriteMany access mode for migrations
- **Kubernetes**: Kubernetes 1.24+ recommended
- **Provider Versions**: All providers updated to v0.3.0

#### Upgrade Path
- Direct upgrade from v0.2.x with no manual intervention required
- Existing deployments will automatically benefit from new features
- Migration features require StorageClass with ReadWriteMany access mode
- No configuration changes needed for standard deployments

### Known Limitations

#### Migration
- **Testing Status**: Currently only tested from vSphere to Libvirt/KVM. Other provider combinations (Libvirt to vSphere, Proxmox migrations, etc.) are not yet fully tested
- **Provider Restart**: Brief (5-15 second) provider pod restart required for PVC mounting
- **Concurrent Migrations**: Multiple migrations may cause multiple provider restarts
- **Storage Requirements**: Requires StorageClass with ReadWriteMany access mode
- **Network Bandwidth**: Migration speed depends on network bandwidth and storage performance

#### VMSet
- **Rolling Updates**: Rolling updates may cause brief VM unavailability during updates
- **Replica Limits**: Maximum 1000 replicas per VMSet

#### Placement Policies
- **Provider Support**: Placement policies require provider-specific implementation
- **Policy Evaluation**: Policy evaluation occurs during VM creation only


---

## [0.2.3] - 2025-10-13

### Added

#### vSphere Provider
- **VM Reconfiguration Support**: Complete implementation of dynamic VM reconfiguration
  - Online CPU adjustment for running VMs (hot-add when supported by guest OS)
  - Online memory adjustment for running VMs (hot-add when supported)
  - Disk resizing with safety checks to prevent data loss (shrinking prevented)
  - Intelligent change detection to avoid unnecessary reconfigurations
  - Memory parsing support for multiple units (Mi, Gi, MiB, GiB)
  - Automatic fallback to offline changes when online modification is not supported
- **Asynchronous Task Tracking**: Full TaskStatus RPC implementation
  - Real-time monitoring of vSphere async operations via govmomi
  - Task state reporting (queued, running, success, error)
  - Error information extraction from failed tasks
  - Progress tracking with percentage completion
  - Integration with vSphere task manager for reliable operation status
- **VM Cloning Operations**: Complete clone functionality
  - Full clone support for independent VM copies
  - Linked clone support for space-efficient template-based deployments
  - Automatic snapshot creation for linked clones when no snapshot exists
  - Proper disk relocation and storage configuration
  - Clone naming and folder placement control
- **Console URL Generation**: Web-based VM console access
  - Automatic vSphere web client console URL generation
  - Direct browser-based VM console access via vCenter
  - URL includes VM instance UUID for reliable identification
  - Integration with vCenter endpoint for proper routing

#### Libvirt Provider
- **VM Reconfiguration Support**: Complete virsh-based reconfiguration
  - Online CPU adjustment via `virsh setvcpus --live` for running VMs
  - Online memory adjustment via `virsh setmem --live` for running VMs
  - Offline configuration updates for stopped VMs via `virsh setvcpus/setmem --config`
  - Disk volume resizing via storage provider integration
  - Automatic VM info parsing to extract current CPU and memory settings
  - Graceful handling of operations requiring VM restart
  - Memory unit conversion and validation (bytes, KiB, MiB, GiB)
- **VNC Console URL Generation**: Remote console access support
  - Automatic VNC port extraction from domain XML configuration
  - VNC console URL generation for direct viewer connections
  - Support for standard VNC clients and web-based VNC viewers
  - Integration with libvirt graphics configuration

#### Proxmox Provider
- **Guest Agent IP Detection**: Enhanced network information retrieval
  - QEMU guest agent integration for accurate IP address detection
  - Extraction of all network interfaces from running VMs
  - Automatic filtering of loopback and link-local addresses
  - Support for both IPv4 and IPv6 address reporting
  - Real-time IP information when guest agent is installed and running
- **Template Cloning Support**: Full VM cloning from Proxmox templates
  - Automatic template detection from VMImage CRD `templateID` or `templateName`
  - Full clone and linked clone support via `fullClone` parameter
  - Proper storage pool selection during clone operation
  - Clone task monitoring with async operation support
- **Intelligent Boot Order Configuration**:
  - Auto-detection of primary boot disk from cloned template
  - Support for multiple disk types: scsi, virtio, sata, ide
  - Automatic boot order generation (e.g., `boot=order=scsi0;ide2`)
  - Exclusion of CD-ROM devices from boot order
- **Cloud-Init Integration**:
  - Complete cloud-init configuration via `ide2:cloudinit` device
  - SSH key injection with proper URL encoding
  - User creation and authentication setup
  - Network configuration (static IP and DHCP)
  - Package installation and custom commands via runcmd
- **Complete CRD Support**:
  - ProxmoxImageSource integration for template-based deployments
  - ProxmoxNetworkConfig for bridge and VLAN configuration
  - Controller parsing of Proxmox-specific fields
  - Provider RPC implementation for all VM operations
- **Production Features**:
  - VM creation, power management, deletion
  - Status reporting with IP address detection
  - Task-based async operations with progress tracking
  - Comprehensive error handling and logging

### Fixed

#### vSphere Provider

**Reconfigure Type Mismatch**:
- **Root Issue**: Memory comparison in Reconfigure function caused compilation error due to type mismatch between int64 and int32
- **Cause**: govmomi's `VirtualMachineConfigInfo.Hardware.MemoryMB` field is int32, but comparison was using int64
- **Fix**: Added explicit type casting `int64(vmMo.Config.Hardware.MemoryMB)` to ensure type compatibility
- **Impact**: Reconfigure operations now compile and execute correctly without type errors

#### Libvirt Provider

**Missing Standard Library Imports**:
- **Root Issue**: Reconfigure and Describe functions referenced undefined packages causing compilation failures
- **Cause**: Implementation added `strconv` usage for integer conversion and `net/url` for URL parsing without importing packages
- **Fix**: Added missing imports:
  - `import "strconv"` for string-to-integer conversions in CPU/memory parsing
  - `import "net/url"` for VNC console URL construction
- **Impact**: Provider now compiles successfully and all reconfiguration and console URL features work correctly

#### Proxmox Provider

**SSH Keys Encoding**:
- **Root Issue**: Proxmox API's `sshkeys` parameter requires **double URL encoding** due to its internal decoding behavior
**Template Cloning and Boot Order**:
- **Root Issue**: Provider was creating new VMs instead of cloning from templates, causing boot failures
- **Fix**: Implemented proper template detection and cloning workflow:
  1. Parse `TemplateName` from VMImage CRD (via controller)
  2. Call CloneVM API with template ID and storage configuration
  3. Wait for clone task completion
  4. Detect primary boot disk from cloned VM config
  5. Reconfigure VM with cloud-init and correct boot order
- **Boot Order Detection**: Added intelligent disk detection that:
  - Scans VM config for disk attachments (virtio, scsi, sata, ide)
  - Filters out CD-ROM devices
  - Prioritizes disk types (virtio > scsi > sata > ide)
  - Constructs proper boot parameter (e.g., `boot=order=scsi0;ide2`)


**Controller Image Source Parsing**:
- **Root Issue**: Controller was sending empty `TemplateName` to provider, causing fallback to VM creation
- **Cause**: JSON field name mismatch - controller uses `TemplateName` (capital T) but provider expected `template_name` (snake_case)
- **Fix**: Updated provider to parse `TemplateName` field correctly from contracts.VMImage JSON
- **Debug Process**: Added comprehensive debug logging in both controller and provider to trace data flow
- **Impact**: Template ID from VMImage CRD is now correctly transmitted to Proxmox provider

#### Release Workflow

**Helm Chart Image Tag Updates**:
- **Root Issue**: Manager and provider images were not being updated during Helm releases, staying pinned to v0.2.0
- **Cause**: sed patterns in release workflow used incorrect range expressions that stopped before reaching the `tag:` lines
  - Manager pattern: `/^manager:/,/^  image:/` stopped at line 16, but `tag:` is on line 18
  - Provider patterns: Nested ranges didn't reach `tag:` lines which appear after provider names
- **Fix**: Replaced fragile regex ranges with precise line-number-based patterns:
  - Manager: `MANAGER_START` to `MANAGER_START+10`
  - LibVirt: `LIBVIRT_START` to `LIBVIRT_START+15`
  - vSphere: `VSPHERE_START` to `VSPHERE_START+15`
  - Proxmox: `PROXMOX_START` to `PROXMOX_START+15`
- **Impact**: All component images (manager, providers, kubectl) now update correctly to match release version


## [0.2.2] - 2025-10-13

### Added (Continued)

#### Nested Virtualization Support
- **VMClass PerformanceProfile**: Added `nestedVirtualization` field to enable nested virtualization capabilities in VMs, allowing VMs to run their own hypervisors and nested virtual machines
- **vSphere Provider Implementation**:
  - Automatically configures `vhv.enable=TRUE` for hardware-assisted virtualization
  - Enables `vhv.allowNestedPageTables=TRUE` for improved nested VM performance
  - Compatible with VM hardware version 9+ (version 14+ recommended)
- **LibVirt Provider Implementation**:
  - Configures CPU mode with required virtualization extensions (vmx for Intel VT-x, svm for AMD-V)
  - Automatically passes through host CPU virtualization features to guest VMs
  - Compatible with QEMU/KVM hypervisors with nested virtualization enabled
- **VT-d/AMD-Vi Support**: Added `vtdEnabled` field in SecurityProfile for Intel VT-d or AMD IOMMU support, improving I/O performance for nested environments
- **CPU/Memory Hot-Add**: Added `cpuHotAddEnabled` and `memoryHotAddEnabled` in PerformanceProfile for dynamic resource scaling without VM restart
- **Virtualization Based Security**: Added `virtualizationBasedSecurity` field in PerformanceProfile for Windows VBS features

#### Security Features
- **TPM (Trusted Platform Module) Support**:
  - Added `tpmEnabled` and `tpmVersion` fields in VMClass SecurityProfile
  - vSphere Provider: Full TPM 2.0 device support (requires vSphere 6.7+ and VM hardware version 14+)
  - LibVirt Provider: TPM emulator support with tpm-tis model and version 2.0
  - Automatically enforces UEFI firmware requirement when TPM is enabled
  - Enables Windows 11 support and BitLocker encryption capabilities
- **Secure Boot Support**:
  - Added `secureBoot` field in SecurityProfile for UEFI Secure Boot functionality
  - vSphere Provider: Configures EFI Secure Boot through VM boot options
  - LibVirt Provider: Uses OVMF firmware with Secure Boot capabilities
  - Automatically forces UEFI firmware when enabled
  - Protects against rootkits and bootkits at firmware level
- **Comprehensive Documentation**:
  - Added `docs/NESTED_VIRTUALIZATION.md` with detailed configuration guide
  - Added `docs/examples/nested-virtualization.yaml` with complete working examples
  - Includes verification steps, troubleshooting guidance, and performance recommendations

#### Use Cases Enabled
- Development and testing of virtualization platforms (e.g., Proxmox, OpenStack, vSphere)
- Running Kubernetes clusters with nested container runtimes
- Creating isolated lab environments for security testing
- Educational scenarios for learning virtualization technologies

#### VM Snapshot Management
- **Complete VMSnapshot CRD**: Full-featured API for VM snapshot lifecycle management
  - Snapshot creation with memory state and filesystem quiescing options
  - Snapshot deletion with proper cleanup
  - Snapshot revert for rollback scenarios
  - Retention policies (maxAge, deleteOnVMDelete, maxCount)
  - Automated scheduling support via cron expressions
  - Snapshot metadata and tagging
- **vSphere Provider Implementation**:
  - Full govmomi-based snapshot operations (Create, Delete, Revert)
  - Memory snapshot support for powered-on VMs
  - Filesystem quiescing with VMware Tools integration
  - Automatic power state handling during revert
  - Hierarchical snapshot tree navigation
  - Synchronous operations for immediate completion
- **LibVirt Provider Implementation**:
  - Full virsh-based snapshot operations (Create, Delete, Revert)
  - Memory snapshot support for running VMs with qcow2 storage
  - Disk-only snapshots for VMs with incompatible storage backends
  - Atomic snapshot creation with --atomic flag
  - Automatic power state preservation during revert
  - Snapshot existence validation before operations
  - Synchronous operations with immediate feedback
  - Snapshot name sanitization for virsh compatibility
  - Helper methods for snapshot listing and querying
- **Proxmox Provider Implementation**:
  - Complete snapshot lifecycle support
  - Memory state inclusion (vmstate)
  - Async task handling with status tracking
  - Full VM creation from templates with cloud-init
  - Intelligent boot order configuration
  - SSH key injection with proper encoding
  - Network configuration with bridge support
  - Storage pool management
- **Controller Integration**:
  - Real provider RPC calls (no more simulation)
  - Proper task status polling for async operations
  - Comprehensive error handling and reporting
  - Event recording for observability
  - Finalizer-based cleanup
- **Transport Layer**:
  - Added snapshot methods to gRPC client
  - TaskStatus RPC for async operation tracking
  - Proper request/response type mapping
- **Use Cases**:
  - Pre-maintenance backups with quick rollback
  - CI/CD testing with snapshot-based environments
  - Disaster recovery and point-in-time restore
  - Development environment versioning

### Fixed

#### vSphere Provider
- **Placement Override Bug**: Fixed critical bug where VirtualMachine `spec.placement.folder`, `spec.placement.datastore`, and `spec.placement.cluster` overrides were not being respected by the vSphere provider. The provider was always using the default values from the Provider CRD instead of honoring the per-VM placement overrides specified in the VirtualMachine manifest. VMs are now correctly created in the specified folder, datastore, and cluster when placement overrides are provided.

## [0.2.1] - 2025-09-29

### Patch Release: Critical Fixes and Documentation Updates

This patch release addresses several critical issues discovered in v0.2.0, including linter compliance fixes, documentation improvements, and enhanced provider capabilities. VirtRigaud v0.2.1 ensures improved stability and usability for production deployments.

### Fixed

#### Code Quality and Compliance
- **Error Handling**: Fixed unchecked error return values in vSphere provider fmt.Sscanf calls
- **Linting Compliance**: Resolved golangci-lint errcheck violations that were causing CI failures
- **CRD Validation**: Fixed CRD validation conflicts for OffGraceful powerState transitions

#### Documentation and Examples
- **Broken Links**: Corrected broken documentation links in README.md
- **Example Updates**: Consolidated and enhanced examples with v0.2.1 features
- **CLI Documentation**: Added comprehensive CLI documentation and reference guides

#### Provider Enhancements
- **VMClass Disk Settings**: Fixed VMClass disk size settings to be properly respected across all providers
- **CRD Schema Sync**: Synchronized Helm chart CRDs with latest schema fixes for consistency

### Added

#### Infrastructure Improvements
- **Build Artifacts**: Enhanced .gitignore to properly exclude dist/ and build artifacts
- **Automated CRD Sync**: Implemented automated CRD synchronization workflow for improved consistency
- **Field Test Exclusions**: Added fieldTest exclusions to .gitignore for cleaner repository

#### vSphere Provider Features
- **Hardware Version Management**: Added VM hardware version management support with version comparison logic
- **Graceful Shutdown**: Implemented graceful shutdown capabilities for virtual machines
- **Enhanced Power States**: Improved power state management with better error handling

### Enhanced

#### Documentation
- **README Updates**: Comprehensive updates to project README with corrected examples and links
- **CLI Reference**: Complete CLI documentation covering all available commands and options
- **Provider Guides**: Enhanced provider-specific documentation with updated examples

#### Development Workflow
- **Release Preparation**: Streamlined release preparation process with automated documentation sync
- **CI/CD Pipeline**: Improved continuous integration with better linting and validation checks

### Technical Details

#### API Stability
- Maintains full backward compatibility with v0.2.0
- No breaking changes to existing APIs or configurations
- CRD schemas remain stable with validation improvements

#### Provider Compatibility
- All existing provider configurations continue to work without modification
- Enhanced error handling improves provider reliability
- VMClass configurations now properly enforce disk size settings

### Deployment Notes

#### Container Images
Updated provider images are available from GitHub Container Registry:
- **Manager**: `ghcr.io/projectbeskar/virtrigaud/manager:v0.2.1`
- **vSphere Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.2.1`
- **LibVirt Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:v0.2.1`
- **Proxmox Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.2.1`
- **Mock Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-mock:v0.2.1`

#### Helm Charts
- **Main Chart**: `virtrigaud/virtrigaud:0.2.1`
- **Provider Runtime Chart**: `virtrigaud/virtrigaud-provider-runtime:0.2.1`

#### Upgrade Path
- Direct upgrade from v0.2.0 with no manual intervention required
- Existing deployments will automatically benefit from the fixes
- No configuration changes needed for standard deployments

### Acknowledgments

This release includes important fixes identified by the community and addresses issues reported in production environments. Thanks to all contributors who helped identify and resolve these issues.

---

## [0.2.0] - 2025-09-15

### Major Release: Production-Ready Provider Architecture

This release marks a significant milestone for VirtRigaud with production-ready vSphere and LibVirt providers, comprehensive documentation, and a complete CLI toolset. VirtRigaud v0.2.0 delivers enterprise-grade virtual machine management across multiple hypervisor platforms.

### Added

#### Core Features
- **Remote Provider Architecture**: Complete implementation of the remote provider model with gRPC communication
- **Production-Ready vSphere Provider**: Full VMware vSphere integration with enterprise features
- **Production-Ready LibVirt Provider**: Comprehensive KVM/QEMU support via virsh-based implementation
- **Advanced Storage Management**: Storage pools, volume operations, and cloud image handling
- **Enhanced Cloud-Init Support**: NoCloud datasource implementation with ISO generation
- **QEMU Guest Agent Integration**: Enhanced guest OS monitoring and communication

#### CLI Tools Suite
- **vrtg**: Complete virtual machine management CLI with resource operations
- **vcts**: Conformance testing suite for provider validation
- **vrtg-provider**: Provider development toolkit for scaffolding and code generation
- **virtrigaud-loadgen**: Load testing and performance benchmarking tool

#### Provider Capabilities

**vSphere Provider:**
- VM creation from templates, OVA/OVF files, and content libraries
- Power management with suspend/resume support
- Advanced networking with distributed switches and port groups
- Snapshot management with memory state preservation
- Template and content library integration
- High availability and DRS configuration support
- Storage policy management and vSAN integration
- Comprehensive error handling and async task monitoring

**LibVirt Provider:**
- VM creation from cloud images with automatic download
- Storage pool and volume management with multiple backends
- Network configuration with bridges and virtual networks
- Cloud-init integration via NoCloud ISO generation
- QEMU Guest Agent support for enhanced monitoring
- Snapshot operations with storage-dependent features
- Resource configuration and management
- Performance optimization with virtio drivers

#### Documentation
- **Comprehensive Provider Documentation**: Detailed guides for each supported provider
- **CLI Reference Manual**: Complete documentation for all command-line tools
- **Provider Capabilities Matrix**: Feature comparison and implementation status
- **Architecture Documentation**: Remote provider design and configuration flows
- **Examples and Tutorials**: Real-world configuration examples and best practices

### Enhanced

#### Core Improvements
- **Provider Registry**: Centralized provider discovery and capability reporting
- **Error Handling**: Improved error classification and retry logic
- **Resource Management**: Enhanced VM lifecycle management with proper cleanup
- **Network Configuration**: Advanced networking features across all providers
- **Monitoring Integration**: Comprehensive metrics and observability features

#### CI/CD Pipeline
- **Automated Testing**: Enhanced test coverage with conformance testing
- **Release Automation**: Streamlined build and release processes
- **Documentation Generation**: Automated API reference and capability documentation
- **Quality Assurance**: Comprehensive linting and static analysis

### Fixed

#### Stability and Reliability
- **Connection Management**: Robust connection handling with automatic retry
- **Resource Cleanup**: Proper cleanup of VM resources and associated storage
- **Memory Management**: Improved memory usage in provider implementations
- **Concurrent Operations**: Thread-safe operations and proper synchronization
- **Error Recovery**: Enhanced error recovery and graceful degradation

#### Provider-Specific Fixes
- **vSphere**: Resolved template deployment and network configuration issues
- **LibVirt**: Fixed storage pool management and cloud-init generation
- **Cross-Platform**: Improved compatibility across different hypervisor versions

### Technical Details

#### API Changes
- **Stable v1beta1 API**: Production-ready API with comprehensive resource definitions
- **Provider Contract**: Standardized provider interface with capability discovery
- **Resource Schemas**: Enhanced CRD schemas with validation and defaults
- **Backward Compatibility**: Seamless upgrade path from previous versions

#### Performance Improvements
- **Async Operations**: Non-blocking VM operations with progress tracking
- **Connection Pooling**: Efficient resource utilization in provider connections
- **Caching**: Intelligent caching of templates, images, and metadata
- **Batch Operations**: Support for bulk VM operations where applicable

#### Security Enhancements
- **Credential Management**: Secure handling of hypervisor credentials
- **Network Isolation**: Provider network isolation with configurable policies
- **RBAC Integration**: Fine-grained role-based access control
- **Audit Logging**: Comprehensive audit trail for all operations

### Provider Feature Matrix

| Feature | vSphere | LibVirt | Status |
|---------|---------|---------|---------|
| VM Lifecycle | Complete | Complete | Production |
| Power Management | Complete | Complete | Production |
| Storage Management | Complete | Complete | Production |
| Network Configuration | Complete | Complete | Production |
| Snapshot Operations | Complete | Storage-dependent | Production |
| Template Management | Complete | Cloud Images | Production |
| Guest Integration | VMware Tools | QEMU Guest Agent | Production |
| High Availability | Complete | Planned | vSphere Only |
| Live Migration | Complete | Planned | vSphere Only |
| Hot Reconfiguration | Complete | Restart Required | Mixed |

### Deployment and Operations

#### Container Images
All provider images are available from GitHub Container Registry:
- **Manager**: `ghcr.io/projectbeskar/virtrigaud/manager:v0.2.0`
- **vSphere Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.2.0`
- **LibVirt Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:v0.2.0`
- **Proxmox Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.2.0`
- **Mock Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-mock:v0.2.0`

#### Helm Charts
- **Main Chart**: `virtrigaud/virtrigaud:0.2.0`
- **Provider Runtime Chart**: `virtrigaud/virtrigaud-provider-runtime:0.2.0`

#### Installation Methods
- Helm charts with comprehensive configuration options
- Kustomize overlays for different deployment scenarios
- Direct YAML manifests for custom deployments
- CLI-based installation and management

### Upgrade Notes

#### Breaking Changes
- None. This release maintains full backward compatibility with v0.1.x deployments.

#### Migration Guide
- Existing v0.1.x deployments can be upgraded in-place
- Provider configurations are automatically migrated
- No manual intervention required for standard deployments

#### Deprecations
- None in this release. All APIs remain stable and supported.

### Known Issues

#### Current Limitations
- **LibVirt Hot Reconfiguration**: CPU and memory changes require VM restart
- **LibVirt Memory Snapshots**: Not supported on all storage backends
- **Cross-Provider Migration**: Not yet implemented between different provider types

#### Workarounds
- Detailed workarounds are documented in the provider-specific guides
- Community support available for deployment-specific issues

### Acknowledgments

This release includes contributions from the VirtRigaud development team and community feedback from early adopters. Special thanks to all contributors who helped shape this production-ready release.

### What's Next

#### Roadmap for v0.3.0
- **Enhanced LibVirt Features**: Live migration and hot reconfiguration support
- **Proxmox VE Provider**: Production-ready Proxmox integration
- **Multi-Cloud Providers**: AWS EC2, Azure, and GCP provider implementations
- **Advanced Networking**: Service mesh integration and network policies
- **Backup and Recovery**: Integrated backup solutions and disaster recovery

#### Community
- Join our community discussions on GitHub
- Contribute to provider development and documentation
- Report issues and feature requests through GitHub Issues

For detailed upgrade instructions and deployment guides, see the [Installation Documentation](docs/install-helm-only.md).

For provider-specific configuration and capabilities, see the [Provider Documentation](docs/providers/).

---

**Full Changelog**: https://github.com/projectbeskar/virtrigaud/compare/v0.2.0...v0.2.1
#### Proxmox Provider CRD Integration (Completed)

The Proxmox provider now has full CRD integration for template-based VM deployment:

**VMImage CRD - ProxmoxImageSource**:
- Template ID or template name references
- Storage pool selection for cloned VMs
- Node specification for template location
- Full clone vs linked clone selection
- Disk format configuration (qcow2, raw, vmdk)

**VMNetworkAttachment CRD - ProxmoxNetworkConfig**:
- Linux bridge selection (vmbr0, vmbr1, etc.)
- Network card model selection (virtio, e1000, rtl8139, vmxnet3)
- VLAN tagging support
- Proxmox firewall integration
- Bandwidth rate limiting
- MTU configuration

**Controller Integration**:
- Full parsing of Proxmox-specific fields from CRDs
- Conversion to provider contracts and gRPC messages
- Proper JSON field mapping (TemplateName, Bridge, Model, etc.)

**Documentation**:
- Complete provider documentation in `docs/providers/PROXMOX.md`
- Working examples in `examples/proxmox/`
- Troubleshooting guides for common issues

**Impact**: The Proxmox provider now has feature parity with vSphere and LibVirt providers, enabling production-ready VM management on Proxmox VE clusters via Kubernetes CRDs.
