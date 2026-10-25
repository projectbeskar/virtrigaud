# VirtRigaud provider SDK

`github.com/projectbeskar/virtrigaud/sdk` is the Go module for writing a
VirtRigaud provider: a gRPC server that implements the provider contract of
[`github.com/projectbeskar/virtrigaud/proto`](../proto/README.md).

| Package | Contents |
|---|---|
| `provider/server` | gRPC server bootstrapping: TLS and mTLS, health checks, middleware |
| `provider/middleware` | gRPC interceptors: logging, authentication, rate limiting, recovery, timeouts |
| `provider/capabilities` | Building and reporting the provider's capabilities |
| `provider/errors` | Typed errors mapped to gRPC status codes |
| `provider/client` | gRPC client for the provider contract, with retries and typed error mapping |

See the package documentation in [`provider/doc.go`](provider/doc.go) for usage.

## Versions

From v0.4.1 on, each VirtRigaud release `vX.Y.Z` publishes this module as
`sdk/vX.Y.Z` (and the proto module as `proto/vX.Y.Z`) on the release commit:

```bash
go get github.com/projectbeskar/virtrigaud/sdk@vX.Y.Z github.com/projectbeskar/virtrigaud/proto@vX.Y.Z
```

Use the same version for both. Releases up to v0.4.0 have no `sdk/` tag. The
fastest start is `vrtg-provider init` from a release, which writes a project
that requires both modules at its own version (see
[`docs/cli-tools.md`](../docs/cli-tools.md#vrtg-provider)). How the modules are
versioned and released: [`docs/go-modules.md`](../docs/go-modules.md).
