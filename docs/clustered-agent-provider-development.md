# Clustered Provider for per-host agents (development builds only)

`provider-cloudhypervisor` is the clustered Provider that fronts hosts which
each run a [per-host agent](per-host-agent-development.md) (`vrtg-agent`).
It routes every per-VM call to the agent of the host the call names and
forwards the call's request unchanged, so that the agent, which holds the
pinned grant anchors, is the only party that judges an operation grant. The
Provider holds no grant key and verifies no grant.

Nothing here is part of a release. A build without the `virtrigaud_dev` build
tag links none of the Provider (`internal/providers/cloudhypervisor`): the
binary prints that it is available in development builds only and exits 1.
No release workflow, container image or make target builds it; a test in
`cmd/provider-cloudhypervisor` checks the release workflow, the CI workflow
and the `Makefile`, and that neither build of the binary links the `testing`
package.

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `--port` | `9443` | gRPC port the manager connects to. |
| `--health-port` | `8080` | HTTP health port. |
| `--hosts-file` | `/etc/virtrigaud/hosts/hosts.json` | The mounted host inventory. |
| `--agent-client-cert`, `--agent-client-key` | (required) | PEM certificate and key the Provider presents to every agent: its Provider identity certificate. |
| `--provider-uid` | (none) | UID of the Provider resource. When set, an agent whose certificate records an enrolment with another Provider is refused. Optional for now, with a warning in the log when unset; a deployment the manager renders must set it. |

The manager-facing TLS is the one of the other Providers (the mounted
provider TLS material and its environment contract).

## Host inventory

The hosts come from the host inventory document
(`internal/clustered/hostsecret`), read from `--hosts-file` and followed as
the file changes (`internal/clustered/inventoryfile`, shared with clustered
libvirt): the Provider reads a file, never the Kubernetes API. Every entry
must carry agent TLS material and nothing else
(`hostsecret.ValidateAgentHost`):

- `id`: a host name (a DNS-1123 subdomain of at most 253 characters);
- `endpoint`: `grpc://host:port`;
- `credentials.agentTLS.caBundle`: PEM CA certificates the agent's
  certificate must chain to;
- `credentials.agentTLS.expectedSAN`: the host's URI identity,
  `spiffe://<trust domain>/virtrigaud/host/<namespace>/<host id>`, whose last
  segment must be the entry's own id;
- `credentials.agentTLS.keyPins`: the SHA-256 of each pinned key's DER
  SubjectPublicKeyInfo, as 64 lowercase hexadecimal characters.

An entry that fails these rules, or shares its id with another entry, is not
routed and is logged by id only. A reload that removes a host, or changes its
endpoint or TLS material, closes its connection before the reload returns
(calls in flight on it fail); a change of labels alone keeps the connection.
A file that turns unreadable or invalid keeps the last good host set. The
file is re-read every 30 seconds on its own, besides the watch on its
directory; when the watch cannot be set up, the Provider logs it at error
level and says it in its `Validate` answer, and the periodic re-read still
follows the file.

## Connections to agents

One client connection per host, created lazily on the first call. The
connection is TLS 1.3 with the Provider's identity certificate. The agent's
certificate is accepted only when all of these hold, checked on every
handshake:

- it chains to the entry's CA bundle for server authentication, at the
  current time;
- it has the host certificate profile (not a CA, digital signature,
  server authentication only, a lifetime of at most the profile's maximum,
  no DNS, IP or e-mail name);
- its URI subject alternative names are exactly the entry's expected host URI
  and one enrolment record (and, with `--provider-uid`, the record names this
  Provider);
- its key is one of the entry's pins.

A host whose agent fails any of these is unavailable (`HOST_UNAVAILABLE`,
cause `AgentCertificateRefused`), and the agent receives no call.

On every new connection the Provider first calls `agent.v1.AgentService/Hello`
with a fresh nonce and its API version, and verifies the host-signed answer:
a `HostAnswer` signed with the key of the certificate the handshake just
verified (the signer certificate must be that certificate), echoing the nonce
and naming the host. The Provider speaks API version 2 and routes to an agent
of version 2 or 1. An agent outside that window, or one that does not serve
`Hello`, is not routed until its connection is replaced (cause
`AgentIncompatible`); the Provider never adapts a request to an older agent.
An answer that is missing or does not verify leaves the host unavailable
(cause `AgentUnverified`), and the next call tries again.

A `Hello` is bound to the transport it was made on (by its local address).
A client connection carries one transport only: once a transport began, its
dialer refuses to dial again, so that no call, not even one gRPC retries
transparently, reaches a new transport (a restarted agent, possibly of
another version) without a new `Hello`. Before a call is sent, its transport
is checked against the host's `Hello`; after it, a call found to have run on
another transport drops that `Hello`. A caller that waits for another
caller's `Hello` of the same host gives up when its own context ends.

A connection is replaced before the next call when its transport ended, is
older than 30 minutes, or carries an agent certificate that has expired, so
that the agent is verified again by a new handshake (the agent also closes
its connections after 30 minutes). Connections send keepalive pings every 20
seconds, with or without calls in flight, and are closed when a ping is not
answered within 10 seconds.

A call waits at most 2 seconds for a connection. A connection that failed is
retried at once by the next call, rather than after its backoff, so that a
host that came back is reached by that call. Every forwarded call has its own
deadline: 20 seconds, or the caller's deadline less half a second when that
is sooner, so that the Provider answers before the caller gives up.

## Routing

| Method | How it is routed | What is forwarded |
|--------|------------------|-------------------|
| `Create`, `Delete`, `Power`, `Reconfigure` | By the `target_host_id` of a decoded copy of the request inside `operation_envelope`. | The outer request, byte for byte as it arrived. |
| `Describe` | By `target_host_id`. | The request, byte for byte. |
| `GetHostInfo` | By `host_id`. | The request, byte for byte. |
| `TaskStatus` with `answer_nonce` | By `target_host_id` (a request that also names a task is `InvalidArgument`). | The request, byte for byte. |
| `TaskStatus` naming a task | By the host encoded in the task reference (`host-task/v1/<host>/<task>`). | A request naming the agent's own task reference. |
| `ListHosts` | Answered by the Provider from its inventory. | Nothing; the hosts without a Hello on their current transport get one first, all at once, within 5 seconds in all. |
| `GetCapabilities`, `Validate` | Answered by the Provider. | Nothing. |
| Every other method (`ListVMs`, snapshots, clones, disk export and import, image preparation, ...) | Not served: `Unimplemented`. | Nothing. |

A mutating call without `operation_envelope` is refused `FailedPrecondition`
with the `IDENTITY_REQUIRED` reason, and one that sets any other field (or an
unknown field) beside it `InvalidArgument`, before any host is dialed. An
envelope without a grant, or whose grant has no body, signature or signer
chain, is refused `FailedPrecondition` with `GRANT_UNAUTHENTICATED`, as the
agent would refuse it. An envelope whose request does not decode, or names no
host, is `InvalidArgument`. Everything inside the envelope, the grant included, is
judged by the agent. A read without a host is `InvalidArgument`; a host the
Provider does not front is `HOST_UNAVAILABLE` (cause `HostNotFronted`, and the
id is not echoed); a task of a host it does not front is `NotFound`.

The forwarded methods read the request as raw bytes: the Provider's service
description (`cloudhypervisor.ServiceDesc`) decodes each request into a
message without fields, which keeps all of its bytes, and sends those bytes
on. The Provider must be registered with that description
(`Provider.Register`), not with the generated `RegisterProviderServer`, which
would decode and re-encode the request. An agent's answer to a read is
relayed byte for byte too. The answer to a mutating call is decoded only to
host-encode its task reference; every other field, signed answers included,
is kept.

The agent's own answers, refusals included, are relayed unchanged. A call the
agent never received is `HOST_UNAVAILABLE` (not performed). A mutating call
that was sent and got no answer (the connection dropped, or the Provider's
call deadline expired) is `Unavailable` with `OUTCOME_UNKNOWN_RETRY`: it may
have been performed, and repeating it with the same grant is safe because the
agent performs an operation at most once. A read the agent did not answer
within the call deadline is `HOST_UNAVAILABLE` (cause `AgentTimeout`). All of
these are host-scoped, so the manager keeps them out of its Provider-wide
breaker; only the caller's own cancellation or deadline is answered as such.
An answer to a mutating call whose host-encoded task reference would exceed
the reference length limit is replaced by `OUTCOME_UNKNOWN_RETRY` (the
operation is then asked about by `op_id`).

`GetCapabilities` reports `supports_operation_identity`,
`requires_operation_identity` and `supports_clustering`, and nothing else.

## Per-host circuit breaker

Each host has its own breaker (5 counted failures open it for 30 seconds). A
failure counts only when the agent could not be reached: no answer
(connection refused or reset, a certificate refused in the handshake, the
connection dropped, the Provider's own call deadline, an agent that went
silent on an open connection), unless the caller gave up;
or an answer of `Unavailable` without a reason of the agent contract. An
answer with such a reason (`UNATTESTED`, `VM_BUSY`, `UNKNOWN_TO_AGENT`, the
grant refusals and the other reasons in `contracts.PartAReasons`) and every
other answer never count. An open breaker answers `HOST_UNAVAILABLE` (cause
`BreakerOpen`) for its host only, without dialing; `ListHosts` reports the
host `NOT_READY`.

## Conformance

The clustered conformance suite runs against the Provider in front of real
agent processes with the fake executor
(`go test -tags virtrigaud_dev ./internal/providers/cloudhypervisor/`), and the
`identity.*` cases run end to end through it in
`test/conformance/clustered/agenttarget`. The deviations are listed in
`chExpectedFailures` and in
[the suite's README](../test/conformance/README.md#findings-recorded-against-the-clustered-provider-for-per-host-agents).

## Limits

- The inventory is a mounted file: a removed or revoked host stops being
  routed when the file changes on the Provider's disk, which for a mounted
  Secret is bounded by the kubelet's sync period. Watching the inventory
  Secret through the API for a faster bound is a later change.
- `ListVMs` is not served: a per-host agent does not list VMs yet.
- The Provider verifies only the answer to its own `Hello`. It relays every
  other signed answer unchanged; verifying them is the manager's job.
- The breaker thresholds, the connection and call timeouts, the keepalive
  and the maximum connection age are fixed (not flags).
- A transport replaced in the instant between the check of a call's
  transport and its send is caught only after the call (its `Hello` is then
  dropped); a client connection never dials a second transport, which keeps
  that window to a transport the same connection had verified.
