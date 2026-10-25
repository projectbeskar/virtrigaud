# VirtRigaud provider contract

`github.com/projectbeskar/virtrigaud/proto` is the Go module of the gRPC
contract between the VirtRigaud manager and its providers:

- [`provider/v1/provider.proto`](provider/v1/provider.proto): the `Provider`
  service and its messages;
- `rpc/provider/v1`: the generated Go bindings
  (`import providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"`).

The module depends on no other VirtRigaud module. A provider written in Go
usually uses it together with the SDK,
[`github.com/projectbeskar/virtrigaud/sdk`](../sdk/README.md).

## Versions

From v0.4.1 on, each VirtRigaud release `vX.Y.Z` publishes this module as
`proto/vX.Y.Z` on the release commit:

```bash
go get github.com/projectbeskar/virtrigaud/proto@vX.Y.Z
```

Releases up to v0.4.0 have no `proto/` tag. How the modules are versioned and
released: [`docs/go-modules.md`](../docs/go-modules.md).
