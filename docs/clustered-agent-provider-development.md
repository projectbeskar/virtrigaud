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
| `--hosts-secret` | (none) | Name of the host inventory Secret to follow through the Kubernetes API instead of the file (see [Following the inventory Secret](#following-the-inventory-secret)). Excludes `--hosts-file`. Needs `get` and `watch` on that Secret; without them the Provider stops at start with a 403 (`Forbidden`) error. A Provider Deployment the manager renders sets it to `<provider>-hosts` only for a Provider listed in the manager's `--dev-agent-provider-inventory-watch`. |
| `--hosts-secret-namespace` | `$PROVIDER_NAMESPACE`, then the pod's namespace | Namespace of `--hosts-secret`. |
| `--inventory-fail-closed-after` | `5m` (minimum `1m`) | With `--hosts-secret`: once no valid inventory was applied for this long, every host is refused (see [Fail-closed](#fail-closed)). |
| `--agent-client-cert`, `--agent-client-key` | (required) | PEM certificate and key the Provider presents to every agent: its Provider identity certificate. |
| `--provider-uid` | (none) | UID of the Provider resource. When set, an agent whose certificate records an enrolment with another Provider is refused. Optional for now, with a warning in the log when unset; a Provider Deployment the manager renders sets it. |

The manager-facing TLS is the one of the other Providers (the mounted
provider TLS material and its environment contract).

## Host inventory

The hosts come from the host inventory document
(`internal/clustered/hostsecret`), read either from `--hosts-file` and
followed as the file changes (`internal/clustered/inventoryfile`, shared with
clustered libvirt), or, with `--hosts-secret`, from the inventory Secret
through the Kubernetes API (next section). Every entry
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

An entry may also carry `freezeConfirmation` (see
[Freeze confirmations](#freeze-confirmations)) and `renewedCertificate` (see
[Renewed certificates](#renewed-certificates)); a malformed one is dropped and
the entry stays routed.

An entry that fails these rules, or shares its id with another entry, is not
routed. It is logged when it is first refused, or refused for another
reason, not at every reload; a malformed id is logged only as a short digest.
A reload that removes a host, or changes its
endpoint or TLS material, closes its connection before the reload returns
(calls in flight on it fail); a change of labels alone keeps the connection.
A file that turns unreadable or invalid keeps the last good host set. The
file is re-read every 30 seconds on its own, besides the watch on its
directory; when the watch cannot be set up, the Provider logs it at error
level and says it in its `Validate` answer, and the periodic re-read still
follows the file.

## Following the inventory Secret

With `--hosts-secret`, the Provider reads its inventory Secret by name and
watches it with a `metadata.name` field selector, using its pod's
ServiceAccount token. Each change is applied as soon as the watch delivers
it: a host the manager removed from the inventory (a revoked Host, a deleted
Host, or one whose pins were removed) has its connection closed and every
call for it refused (`HOST_UNAVAILABLE`, cause `HostNotFronted`) before the next
call can reach it, without waiting for the kubelet to refresh a mounted
Secret (which takes up to about a minute or more). The tests measure the
time from the write that removes a host to its connection closed and its
calls refused against a real API server, as the Provider's ServiceAccount
with only the RBAC below: a few milliseconds (p50 about 6 ms, max under
10 ms over 20 rounds on a development workstation), held to a bound of
2 seconds. In a cluster, add the manager's re-render of the Secret after the
Host change (one reconcile) and the API server's watch delivery.

A watch cycle lasts at most 30 seconds and every cycle starts from a fresh
read of the Secret, so a watch that goes silent without failing delays a
change by at most about 31 seconds. A failed read or watch is retried with a
backoff from 250 ms up to 30 seconds. A Secret that cannot be read or parsed
at start stops the Provider. A deleted Secret fronts no host. Secret content
is never logged: log lines name the Secret and give a host count or a coarse
reason.

The inventory's age is the time since the Provider last **applied** a valid
inventory. Only that refreshes it: a watch bookmark, or an update whose
document is invalid (a schema version this Provider does not understand, a
corrupt write), proves that the API answers and nothing more, and never
clears the failure. Within the fail-closed bound, while no valid inventory
can be applied, the last good host set is kept; once the age passes three
reload intervals (90 seconds) the `Validate` answer says so, with the cause.

### Fail-closed

Once no valid inventory was applied for `--inventory-fail-closed-after`
(default 5 minutes, at least 1 minute; whether the API server is
unreachable or the Secret's document is invalid), the Provider can no longer
tell which hosts are still authorised, and it refuses every host: their
connections are closed, and every call is refused at once, without dialing,
with `HOST_UNAVAILABLE` and cause `InventoryStale` (no host is named).
`Validate` answers not Ok and says why. The next valid inventory applied
restores routing on its own; the hosts are verified again on new
connections.

Running VMs are not affected: the agents keep them running, and only the
manager's calls (creating, deleting, power and reconfiguration operations,
reads) wait until the inventory is applied again. The manager treats
`HOST_UNAVAILABLE` as host-scoped and outside its circuit breaker, as for an
unreachable host: a VM waiting on its pending host stays there (it is never
re-placed and the host is not excluded) and its operation is sent again,
with the same identity, once the host is routed.

A call already in flight to an agent when the Provider fails closed is cut
by the closed connection and ends with a transport error, not with
`InventoryStale`. That is safe for the same reason as a revocation: the agent
journals every operation and performs it at most once, and the manager asks
about the operation by its identity before it decides anything, so a cut
call is never taken as "not performed".

### Metrics

On the health port's `/metrics`, labelled `provider_type="cloudhypervisor"`:

| Metric | Meaning |
|--------|---------|
| `virtrigaud_provider_host_inventory_seconds_since_apply` | Seconds since a valid inventory was last applied. |
| `virtrigaud_provider_host_inventory_stale` | 1 once that age passes three reload intervals, else 0. |
| `virtrigaud_provider_host_inventory_failed_closed` | 1 while every host is refused (fail-closed), else 0. |

Alert on these gauges (`virtrigaud_provider_host_inventory_stale == 1`, and
`virtrigaud_provider_host_inventory_failed_closed == 1`), not on the
Provider's readiness: `Validate` stays Ok while the inventory is stale and
only turns not Ok once the Provider fails closed. A revocation made while
the inventory is stale is not applied until a valid inventory is (or until
the Provider fails closed).

### RBAC

The Provider needs exactly `get` and `watch` on that one Secret and nothing
else: no `list` (the watch names the Secret, which a `resourceNames` rule
authorises), no other Secret and no write. The chart grants it, for the
Providers you list, to each Provider's own ServiceAccount
(`virtrigaud-provider-<namespace>-<name>`, created by the manager), with a
Role and RoleBinding in the Provider's namespace:

```yaml
experimental:
  agentProviderInventoryWatch:
    enabled: true
    developmentBuild: true   # explicit: the Provider image is a development build
    providers:
      - namespace: infra
        name: ch
```

which renders, per Provider:

```yaml
rules:
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "watch"]
  resourceNames: ["ch-hosts"]
```

Rendering fails without `developmentBuild: true`, without a Provider, or with
an entry that is not a valid namespace and name. The manager grants no RBAC
itself (it holds no `roles` or `rolebindings` write).

The same list also starts the manager with
`--dev-agent-provider-inventory-watch=<namespace>/<name>,...` (honoured
only by a development build; a release manager refuses to start with it).
Only for a clustered `cloudhypervisor` Provider in that list does the
Deployment the manager renders mount the pod's ServiceAccount token (every
other Provider pod still runs without one), pass `--hosts-secret` and
`--provider-uid`, and no longer mount the inventory Secret as a volume, so
the material is not held twice. Every other Provider, listed nowhere, keeps
the mounted file and no token. If you set the manager flag by hand, list
only Providers granted the RBAC above: a listed Provider without it is
refused its first read (403, `secrets "<provider>-hosts" is forbidden`) and
stops at start, so its pod restarts until the grant exists.

What the token can reach is that Secret: the agent TLS material of the
Provider's hosts (CA bundle, expected host URIs and key pins). It holds no
private key; the inventory a per-host agent Provider reads carries no SSH
material (`hostsecret.ValidateAgentHost`).

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
and naming the host. The Provider speaks API version 3 and routes to an agent
of version 3 or 2. An agent outside that window, or one that does not serve
`Hello`, is not routed until its connection is replaced (cause
`AgentIncompatible`); the Provider never adapts a request to an older agent.
An answer that is missing or does not verify leaves the host unavailable
(cause `AgentUnverified`), and the next call tries again. The agent signs at
most 1 `Hello` answer per second (burst 5) for one Provider identity, apart
from its other answers; several Provider replicas that share one identity
may therefore briefly get `AgentUnverified` after an agent restart, which
clears on its own as the budget refills.

A client connection carries one transport only: once a transport began, its
dialer refuses to dial again, so that no call, not even one gRPC retries
transparently, reaches a new transport (a restarted agent, possibly of
another version) without a new `Hello`. A `Hello` is bound to the connection
it was made on, and so to that one transport, whatever its addresses; it is
dropped when the transport ends or the connection is replaced. Before a call
is sent, the Provider checks that the connection it holds is still the
host's connection and that its transport had the `Hello`; otherwise it makes
a new `Hello` first. A caller that waits for another caller's `Hello` of the
same host gives up when its own context ends.

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

## Freeze confirmations

An entry may carry the manager's `freezeConfirmation`: the host's current
enrolment id and up to 16 predecessor enrolment ids whose freeze the manager
confirmed. Before the next call to that host, the Provider relays it
unchanged with `agent.v1 AgentStatus` on the host's verified connection,
once per connection and again when it changes; a relay never fails the call
itself. A failed relay is logged and tried again with a later call, waiting
10 seconds after the first failure and doubling up to 10 minutes per host. An
agent that answers `Unimplemented` (it predates confirmations) is not asked
again on that connection. A malformed confirmation (a bad id, more than 16,
or the current enrolment among them) is dropped with a log line; the entry
stays routable and nothing is relayed. The Provider does
not judge the confirmation: the agent applies it only to its own frozen
predecessors of the named current enrolment
([details](host-agent-enrolment.md#confirming-a-predecessors-freeze)).

## Renewed certificates

An entry may carry `renewedCertificate`: the DER certificate the manager had
issued for the host's certificate renewal. Before the next call to that host,
the Provider delivers it with `agent.v1 InstallRenewedCertificate` on the
host's verified connection: with `activate` false while the certificate's key
is not among the entry's key pins (the agent stages it), and with the same
bytes and `activate` true once the inventory lists its key among the pins
(the manager pins it only after the host showed, signed, that it staged
exactly that certificate). Each phase is delivered once per connection; a
relay never fails the call itself. A failed relay is logged and tried again
after 10 seconds, doubling up to 10 minutes per host; an agent that answers
`Unimplemented` is not asked again on that connection. A malformed
certificate (not one DER certificate, or larger than 8 KiB) is dropped with a
log line and the entry stays routable. The Provider does not judge the
certificate and does not need to verify the answer: the agent accepts only a
certificate for its own pending key and its unchanged identities, and the
connection is authenticated by a pinned key. During the overlap both keys are
pinned, so connections to the agent succeed before and after it switches;
a pin change recycles the host's connection
([details](host-agent-enrolment.md#certificate-renewal)).

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

- With `--hosts-file`, a removed or revoked host stops being routed when the
  file changes on the Provider's disk, which for a mounted Secret is bounded
  by the kubelet's sync period; use `--hosts-secret` for a bound in seconds.
- With `--hosts-secret`, the bound holds while the API server is reachable.
  During an outage shorter than the fail-closed bound the last good host set
  is kept (reported by `Validate` and the metrics), so a revocation made
  meanwhile takes effect when the Provider reads the Secret again, or when it
  fails closed at the bound, whichever comes first.
- The chart's optional NetworkPolicies give provider pods no egress to the
  Kubernetes API server; with them enabled, a Provider started with
  `--hosts-secret` needs an additional egress rule to the API server.
- A Provider Deployment the manager renders does not yet carry the
  Provider's identity certificate (`--agent-client-cert`); deploy the
  Provider with it by other means until the Provider identity issuer exists.
- `ListVMs` is not served: a per-host agent does not list VMs yet.
- The Provider verifies only the answer to its own `Hello`. It relays every
  other signed answer unchanged; verifying them is the manager's job.
- The breaker thresholds, the connection and call timeouts, the keepalive
  and the maximum connection age are fixed (not flags).
