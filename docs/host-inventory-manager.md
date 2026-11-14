# Host inventory and readiness: manager side (development builds only)

This page describes what the manager does with the host inventory that a per-host agent
puts in its signed host answer. It is used only by development builds (built with the
`virtrigaud_dev` build tag) and only for the hosts of a Provider whose type runs a
per-host agent (`cloudhypervisor`). Every other Provider, and every release build, is
unchanged.

The agent side (what is collected and how) is in
[`per-host-agent-development.md`](per-host-agent-development.md#host-inventory). The wire
shape, field rules and closed value sets are in
[`operation-grants-wire-shape.md`](operation-grants-wire-shape.md#host-inventory).

## What it is, and what it is not

The inventory is a **detective control**. It is signed inside the host answer, so the
Provider relaying it cannot alter it, replay an older one, present one host's inventory as
another's, or lower the inventory version the manager asked for. It is a statement by the
host's agent, not a proof: a host whose agent, signer or root account is compromised can
report any inventory, including ready components with allow-listed digests, because the
digests are computed by the code being checked.

Also note:

- **Manifest authenticity.** A component manifest in the writable host-configuration
  directory cannot override one in the read-only image directory, but it can add a
  declaration the image does not make. Restrict the accepted manifest origins
  (`--host-inventory-accepted-origins=read-only`) and rely on the allow-list's exact
  digest match to counter that.
- **Time of use.** Files can change between the agent's collection and their use; the
  inventory detects drift at the next collection only.
- **Unsigned host information.** The Provider's unsigned `HostInfo` (capacity, labels,
  CPU features, health) can still steer which ready host a VM lands on, and so which VMs
  share a host. It cannot make a host ready: it can only make it not ready.
- **Status.** `Host.status` can be written by anyone with `hosts/status` access, so the
  manager never reads its own inventory mirror back to decide anything, and the scheduler
  refuses hosts whose last successful sync is too old (below).

## Asking

Every heartbeat of such a host (every 60 seconds, 15 seconds after a failure) is a
`GetHostInfo` that asks for a signed host answer and for the host inventory
(`inventory_version` 1, the highest version this manager implements), whatever version of
agent the host last reported.

An answer without the inventory (an agent that does not report one, or a Provider that
dropped the request field) is still a valid answer for everything else: grant versions,
the host answer gate, certificate renewal and store continuity keep working. Only the
inventory is missing, and the host is not ready.

The manager verifies the inventory after every other check of the answer has passed
(`hostidentity.VerifyInventory`, independently of the agent's own checks):

- **Echo.** The inventory is present only when it was asked for, and its
  `requested_version` is exactly the version the manager sent. Only a relay that changed
  the request can break this, so the answer is no answer for that request and nothing
  else: the next heartbeat asks again, with no backoff.
- **Field rules.** The agent's `supported_version` is at least 1, `inventory_version` is
  the lower of the two and one the manager implements, every list is strictly ascending by
  name and within its bound, the states are consistent, and every enum value, reason and
  probe name is in the closed sets of that version. A failure means the agent signed
  malformed content: the whole answer is refused, and the manager stops asking for the
  inventory for 1 minute, then 2, 4, and at most 5 minutes after each further failure, so
  the rest of the host answer keeps working. The backoff ends at once when the host's
  enrolment or agent version changes, or when an inventory verifies.

## Conditions

Two Host conditions are written from the verified answer of the heartbeat that just
happened, never from status and never carried over a failed heartbeat:

**`AgentInventoryVerified`** is True (`InventoryVerified`) when the heartbeat got a
verified signed host answer under the host's current enrolment, carrying an inventory that
passed every check, in state `COMPLETE`, and whose age is at most the maximum inventory
age. The age is the agent's `snapshot_age_seconds` plus the time since the manager
verified the answer. False reasons:

| Reason | Meaning |
|---|---|
| `NoSignedAnswer` | No verified answer: `GetHostInfo` failed (any error), the answer was missing, unsigned or failed verification, the inventory did not echo the request, the host is held for a store regression, or no enrolment is configured |
| `InventoryMissing` | The verified answer carries no inventory |
| `InventoryInvalid` | The agent signed an inventory that breaks a field rule (and, during the backoff that follows, the inventory is not asked for) |
| `InventoryPending` | The agent has not finished its first collection |
| `InventoryStale` | The inventory is older than the maximum age |
| `InventoryOverLimit` | The agent observed more than an inventory can hold |
| `InventoryUnreadable` | The agent could not read its manifests or finish its probes |

**`ComponentsReady`** is checked in this order; the first that applies is reported:

| Reason | Meaning |
|---|---|
| `AgentInventoryNotVerified` | `AgentInventoryVerified` is not True |
| `AgentNotReady` | The agent's state in the answer is not `READY` |
| `NoComponentsReported` | The inventory lists no component at all |
| `UnexpectedExtension` | A system extension no component declares is merged on the host |
| `ComponentUnmanifested` | A known component is present without a manifest |
| `ManifestInvalid` | A component manifest cannot be used |
| `ManifestOriginNotAccepted` | A component is declared by a manifest from a directory that is not accepted |
| `ComponentMissing` | A declared or required component is not present |
| `ComponentIncompatible` | A component's version is outside the range the agent requires |
| `ComponentUnhealthy` | A component failed a check on the host |
| `AllowListUnverified` | The configured allow-list could not be put in force (below) |
| `AllowListNotConfigured` | The manager has no allow-list (fail closed) |
| `DigestNotAllowListed` | A component's digests match no allow-list alternative for its version, or its version has no entry |
| `DigestsNotReported` | A component reports neither an artifact digest nor files, so it cannot be compared |
| `RequiredComponentAbsent` | A component the allow-list requires is not reported ready |
| `FeatureMissing` | A required feature probe is not present |
| `IdentityTierNotAccepted` | The agent's process identity tier is not accepted |

It is True with `ComponentsVerified`, or with `AllowListNotConfigured` when the manager runs
with `--insecure-skip-component-allow-list`. Every `ComponentsReady` message names the
SHA-256 of the allow-list in force.

## Readiness and scheduling

In the same status write that records the Provider's host information, the Host's health
is `Ready` only when the Provider reports the host ready **and** both conditions are True.
Otherwise it is `NotReady` with `Ready=False/AgentInventoryNotReady`, whose message names
the failing condition and its reason. A heartbeat that ends without a verified answer, on
any path (including every `GetHostInfo` error), sets both conditions False with
`NoSignedAnswer` in that same write, so a Provider that stops relaying answers takes the
host out of placement on the next heartbeat, not after the maximum age.

The scheduler, HostPool readiness and clone placement read the health as before. In
addition, for the hosts of such a Provider only, the scheduler rejects a host whose
`status.lastHeartbeatTime` is absent or older than the maximum inventory age (rejection
`host heartbeat stale`), so a `Ready` health left by a Host controller that stopped
writing (or by an earlier manager) does not outlive it. After a manager restart a host is
not ready until its first verified heartbeat.

A host turning `NotReady` only removes it from **new** placement. A VM with an open
operation attempt on the host is never re-placed, re-bound or given a new binding because
of it: the attempt keeps being asked about by `op_id` on that host. Because one failed
heartbeat makes a host not ready, schedulability follows the Provider's error rate; with a
healthy Provider and agent expect well under one change per host per hour. Alert on the
`AgentInventoryVerified` Events (below) above about six transitions per host per hour.

## The status mirror

`Host.status.agentInventory` is the **last verified** inventory, for operators and audit;
`observedTime` says when it was verified. It carries the SHA-256 of the signed answer and
of the allow-list, the enrolment it was verified under, the versions, state, collection
age, identity tier, and each component (with its allow-list result `Allowed`,
`NotAllowed`, `NotListed` or `NotChecked`), feature and unexpected extension. Its schema
bounds match the signed inventory's. It is an **output only**: no controller, the
scheduler included, reads it back. It is kept across failed heartbeats under the same
enrolment (the conditions say whether it is current), dropped when the host's enrolment
changes, and removed, with both conditions, when the Host's Provider no longer runs a
per-host agent.

## The component allow-list

The allow-list is a JSON file the manager reads **once, at start**:

```json
{
  "version": 1,
  "required": ["cloud-hypervisor"],
  "components": [
    {
      "name": "cloud-hypervisor",
      "version": "1.0.0",
      "alternatives": [
        {
          "artifactDigest": "sha256:<64 lowercase hex>",
          "files": [
            {"name": "cloud-hypervisor", "digest": "sha256:<64 lowercase hex>"}
          ]
        }
      ]
    }
  ]
}
```

An entry is one component at one observed version; it lists one or more alternatives
(builds). An alternative is an artifact digest, or `"none"` for a component delivered
without one, and the **complete** set of the component's file names with one digest each.
A reported component is `Allowed` only if its artifact digest equals an alternative's
(both absent for `"none"`), its file names are exactly the alternative's (no extra, none
missing) and every file digest is equal. An entry with no matching alternative gives
`NotAllowed`; no entry for the component and version gives `NotListed`; a component that
reports no digest at all gives `NotChecked`. Every component named in `required` must be
reported ready; an allow-list must require at least one component (so that a host
reporting nothing cannot pass). Unknown fields, repeated entries or file names, malformed names, versions
or digests, and files larger than 1 MiB are refused.

**Digest pin.** The file's SHA-256 is pinned by a manager flag. Render the file into an
immutable object whose name carries its content hash, mount it into the manager, and put
its SHA-256 in `--component-allow-list-sha256`. Changing the allow-list then needs a new
flag value, which is a change of the manager Deployment and a rollout; recreating the
object with other content under the same name changes nothing a running or restarted
manager accepts. Compute the pin with `sha256sum allow-list.json`.

**Signature.** With `--component-allow-list-signers-file` (a PEM file of ECDSA P-256 or
Ed25519 public keys: for example the release signing key and an operator key for a site
allow-list), the file must also carry a detached signature
(`--component-allow-list-signature-file`, base64; for ECDSA an ASN.1 DER signature over
the SHA-256 of the file, as `cosign sign-blob --key` writes it; for Ed25519 a signature
over the file). It is verified offline against those keys only; no network is used. The
fingerprint of the key that verified is logged. A signers file without a signature file,
a signers file that does not parse, or a digest pin that is not 64 lowercase hexadecimal
characters stops the manager at start.

**When it cannot be put in force.** A configured allow-list that is missing, has no pin,
does not have the pinned digest, or whose signature does not verify does not stop the
manager. The manager logs why (with both digests when they differ), every Provider whose
type runs a per-host agent reports `Ready=False` with `AllowListDigestMismatch` or
`AllowListUnverified`, and none of their hosts is ready (`ComponentsReady=False/
AllowListUnverified`). Restart the manager with a matching file and pin.

**No allow-list.** Without one, every such host is not ready
(`ComponentsReady=False/AllowListNotConfigured`). Only
`--insecure-skip-component-allow-list` lets hosts be ready without a comparison; it is
logged as a warning at start and the condition and its Event say so.

## Flags

All are refused by a release build, which exits at start when any is set.

| Flag | Default | Meaning |
|---|---|---|
| `--component-allow-list-file` | none | The allow-list file |
| `--component-allow-list-sha256` | none | Its pinned SHA-256 (required with the file) |
| `--component-allow-list-signers-file` | none | Keys one of which must sign the allow-list; requires the signature file |
| `--component-allow-list-signature-file` | none | The detached signature |
| `--insecure-skip-component-allow-list` | false | Hosts may be ready without an allow-list comparison; cannot be combined with an allow-list |
| `--host-inventory-max-age` | `5m` | Maximum inventory age, from `1m` to `1h`; also the scheduler's heartbeat limit for these hosts |
| `--host-inventory-required-features` | `cgroup_v2,clone_into_cgroup,initial_cgroup_namespace,initial_pid_namespace,initial_time_namespace,kvm,pidfd,tun` | Feature probes that must be present, or `none` |
| `--host-inventory-accepted-origins` | `read-only,writable` | Manifest origins accepted for a ready component |
| `--host-inventory-accepted-tiers` | `pidfd-store,pidfs-handle,pidfs-inode,start-time` | Process identity tiers accepted |

Every flag set to other than its default is logged at start with its effective value.
`kvm` and `tun` are required by default because a Cloud Hypervisor host needs both
devices; a test or CI host without them overrides `--host-inventory-required-features`. A
stricter profile accepts `read-only` origins only, the `pidfd-store` and `pidfs-handle`
tiers only, adds `secure_boot`, `measured_boot` and `extension_image_policy_signed` to the
required features, and signs the allow-list. The current agent reports
`hypervisor_pause_resume` and `extension_image_policy_signed` as not evaluated, so
requiring either keeps every host not ready until the agent evaluates them.

## Audit and metrics

On every change of either condition the manager writes one log line with the condition,
its reason, the component it is about, and the SHA-256 of the signed answer and of the
allow-list. The whole signed answer (base64; it carries no secret) is added to such a
line once per answer and at most once per Host every 10 minutes, so a flapping Provider
does not flood the log; the other lines carry its SHA-256 only.

Each condition records an Event on the Host when its reason changes, at most one per
condition per minute. Changes within that minute are counted and, at the first heartbeat
after it, reported in one `InventoryTransitionsSuppressed` Event that gives their number,
their reasons and the current reason, so recovering and failing again within a minute is
never shown as one outage.

At start the manager logs the allow-list's SHA-256 (with `audit=component-allow-list`)
and publishes `virtrigaud_component_allow_list{state, sha256}` (state `loaded`,
`not_in_force`, `not_configured` or `skipped`). Per Host it publishes
`virtrigaud_host_inventory_condition{namespace, host, condition, reason}`: 1 while the
condition is True, 0 otherwise, one series per condition with its current reason (from
the closed set above, anything else as `Other`), removed with the Host. Alert on changes
of its `AgentInventoryVerified` series.

When the Host is deleted, the manager also drops its inventory backoff and audit state.

## Not done yet

- The allow-list signature is over the file's bytes (or their SHA-256) with no domain
  separation of its own: a signing key used for allow-lists should not sign anything else.
- There is no single stricter profile that requires a signers file and read-only manifest
  origins; set the flags explicitly.
- The Helm chart does not render the allow-list object or its digest pin.
