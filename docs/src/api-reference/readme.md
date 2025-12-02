# API Reference

_Auto-generated Go API documentation for VirtRigaud_

This section contains automatically generated documentation from Go source code.
The documentation is extracted from GoDoc comments and regenerated on every build.

## Available References

- **[API Types](api-types.md)** - Custom Resource Definitions (CRDs)
- **[Provider Contract](provider-contract.md)** - Provider interface specification
- **[SDK Reference](sdk.md)** - Provider SDK documentation
- **[Utilities](utilities.md)** - Internal utility packages

## Manual API References

- **[CLI Reference](cli.md)** - Command-line tools
- **[Metrics](metrics.md)** - Observability metrics

---

## Documentation Source

All API documentation is automatically generated from:

- **CRD Types**: `api/infra.virtrigaud.io/v1beta1/*_types.go`
- **Provider Contract**: `internal/providers/contracts/*.go`
- **SDK**: `sdk/provider/`
- **Utilities**: `internal/k8s/`, `internal/resilience/`, `internal/util/`

The source code GoDoc comments are the authoritative documentation.

---

_Last generated: 2025-12-02 01:05:57 UTC_

## Generating Documentation

To regenerate this documentation, run:

```bash
make docs-build
```
