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

The contract also reserves the operation-grant messages (`OperationEnvelope` and
the others), an `operation_envelope` field on each mutating request and four
capability gates. They are shape only: no bundled provider uses them and the
manager never sets them
([`docs/operation-grants-wire-shape.md`](../docs/operation-grants-wire-shape.md)).
The same holds for the signed host answer messages in
[`provider/v1/host_answer.proto`](provider/v1/host_answer.proto) (same package and
Go package as `provider.proto`) and the request and response fields that carry
them
([`docs/operation-grants-wire-shape.md#signed-host-answers`](../docs/operation-grants-wire-shape.md#signed-host-answers)).

## The per-host agent service (internal)

[`agent/v1/agent.proto`](agent/v1/agent.proto) (package `agent.v1`, generated Go
bindings in `rpc/agent/v1`, package `agentv1`) is the service a per-host agent
serves to the Provider that routes to it. It is **internal** to VirtRigaud: it is
not part of the provider contract, external Provider implementations neither serve
nor call it, the manager never calls it, and there is no compatibility promise to
external consumers. It ships in this module so that the agent and the Provider can
share it, and it evolves additively under the same `buf lint` and `buf breaking`
checks as `provider.v1`. Nothing serves or calls it yet.

## Versions

From v0.4.1 on, each VirtRigaud release `vX.Y.Z` publishes this module as
`proto/vX.Y.Z` on the release commit:

```bash
go get github.com/projectbeskar/virtrigaud/proto@vX.Y.Z
```

Releases up to v0.4.0 have no `proto/` tag. How the modules are versioned and
released: [`docs/go-modules.md`](../docs/go-modules.md).
