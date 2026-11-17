# Per-host agent enrolment runs

Scripts that exercise per-host agent enrolment end to end against a real
cert-manager: online enrolment, offline enrolment by file copy, revocation
latency and certificate renewal. They are test tooling only: they run as root
**inside a disposable test VM**, never on a shared host or against an
existing cluster. The helper binary is `test/agent-lab/cmd/enrol-lab`.

What gets installed in the VM:

- a single-node k3s cluster (pinned version, checksum verified);
- cert-manager with its default `CertificateRequest` approver disabled
  (`--controllers=*,-certificaterequests-approver`), and cert-manager
  approver-policy;
- one of the repository's optional controls on the manager's own requests
  (`config/rbac/host-enrolment/admission/`): the approver-policy sample or the
  deny-only `ValidatingAdmissionPolicy` sample;
- a test root CA and, under it, the host-agent subordinate CA behind a
  `ClusterIssuer` (CA issuer), a separate Provider identity CA, a separate
  manager-to-Provider mTLS CA, the development operation signer key and an
  enrolment administrator client certificate (organisation = the enrolment
  administrator group);
- the VirtRigaud CRDs, `config/rbac` (manager) and `config/rbac/host-enrolment`,
  the Provider inventory-watch Role the chart would render, and an
  administrator Role (create requests, `enrol`/`rekey` on Hosts, patch Hosts);
- the development-built manager as a systemd service on the node, out of
  cluster, with a token kubeconfig of its own ServiceAccount (so the API
  server authorises it exactly as the RBAC says);
- the clustered Cloud Hypervisor Provider as the Deployment the manager
  renders, unpatched, from a local single-layer image of the development
  binary; its identity certificate (URI ending with the Provider UID) issued
  by a cert-manager `Certificate` into the Secret its `credentialSecretRef`
  names, from a second `ClusterIssuer`, approved by the repository's sample
  `config/rbac/host-enrolment/admission/provider-identity-approver-policy.yaml`
  (the default approver is off);
- per-host agents (`vrtg-agent run` with the fake executor, and `vrtg-agent
  signer`) as systemd units generated from `packaging/systemd` (only the
  per-instance places change), with the packaged sysusers/tmpfiles fragments
  and `vrtg-agent-setup.service`, several instances per VM, each with its own
  state, key directory, socket directory and port.

## Build

```sh
for c in manager provider-cloudhypervisor vrtg vrtg-agent; do
  CGO_ENABLED=0 go build -trimpath -tags virtrigaud_dev -o bin/$c ./cmd/$c
done
CGO_ENABLED=0 go build -trimpath -tags virtrigaud_dev -o bin/enrol-lab ./test/agent-lab/cmd/enrol-lab
```

Copy `bin/*` to `$LAB/bin`, the repository's `config/` and
`packaging/systemd/` to `$LAB/repo/`, and the scripts to `$LAB/scripts/`
(`LAB` defaults to `/root/lab`).

## Scripts

| Script | What it does |
|--------|--------------|
| `setup-cluster.sh` | k3s, cert-manager (default approver disabled), Helm and approver-policy; records versions and sha256 in `$LAB/versions.txt`. |
| `pki.sh` | The test PKI (see above). With `PROVIDER_UID` set it can also issue a Provider identity certificate by hand (the install uses a cert-manager `Certificate` instead). |
| `install-virtrigaud.sh` | CRDs, RBAC, the host-agent and Provider identity `ClusterIssuer`s, the control selected by `ADMISSION` (`approver-policy`, `admission` or `none`, via `admission.sh`), kubeconfigs, the Provider image, the manager service, the `Provider`/`HostPool` and the Provider identity `Certificate`. |
| `admission.sh MODE` | Applies one of the repository's samples in `config/rbac/host-enrolment/admission/` (placeholders replaced) and removes the other: `approver-policy`, `admission` or `none`. |
| `host-provision.sh HOST PORT` | One agent instance: users (packaged sysusers file), trust configuration, directories, provisioning marker, units. Does not start the signer. |
| `enrol-online.sh HOST PORT [DURATION]` | Online enrolment, step by step as documented: Host with an identity, `enroll csr` as the signer's user, `vrtg host enroll`, `--certificate-out` once pinned, signer restart, `enroll install` as the agent's user, agent start; waits for a verified signed host answer. |
| `enrol-offline.sh HOST PORT` | Offline enrolment by file copy: the host and the administrator workstation steps run in an empty network namespace (`unshare -n`), the workstation with no kubeconfig (`--manifest-out`); files move between the three roles by copy, checked by sha256; `kubectl create -f` and `--certificate-out` run where the cluster is reachable. |
| `revoke-latency.sh FIRST LAST [BASEPORT]` | Per sample: a fresh host enrolled online, then `vrtg host revoke` while `enrol-lab revoke-probe` calls `GetHostInfo` for it through the Provider every 10 ms; one JSON line per sample. |
| `provider-cert-nouid.sh HOST [SECONDS] [INTERVAL]` | Re-issues the Provider identity certificate without the Provider UID in its URI, restarts the Provider, logs how the Host reports it and the agent's refusal lines, then restores it. |
| `manager-sa-requests.sh` | With the manager's token, creates requests for a made-up host (not renewal-shaped, CA, renewal-shaped, renewal-shaped for an existing Host, and a self-approval attempt) and reports what admission refused and how each admitted one ended. |
| `provider-identity-certs.sh` | Creates Provider identity `Certificate`s (the documented shape, another Provider's URI, 721h, an extra usage, an RSA key) and reports how approver-policy decided each request. |
| `renewal-restart.sh HOST midflight\|completed\|withdraw\|withdraw-wait` | Restarts the manager while a renewal is Issued and again after it completed, or revokes the Host while the renewal is Issued (with a manager restart after, or watching for 5 minutes without one), and prints the renewal request's conditions. |
| `handshake-faults.sh HOST PORT [SECONDS]` | Faults that are not a certificate refusal: the host key signer stopped, and TCP resets on the agent port (iptables), each with a Provider restart; reports the Host's cause and the Provider's per-call causes. |
| `pins-conflict.sh HOST [INTERVAL] [TIMEOUT]` | During a host's renewal, rewrites an annotation on its pins Secret every INTERVAL seconds (optimistic-lock conflicts for the manager's writes), then prints the renewal events and the manager's ERROR lines for the host. |
| `rekey-expired.sh HOST` | Re-keys a Host as documented (window, new key, request, certificate, install `--replace`), for example after its certificate expired, and waits for a verified signed answer. |
| `migration-volumes.sh [SECONDS]` | A Bound migration PVC and one VMMigration naming the CH Provider and one naming a control libvirt Provider; logs which Provider pod templates mount the PVC and whether the CH pod keeps running. |
| `expiry-watch.sh HOST [SECONDS]` | Logs, once a second, every change of a Host's Ready, AgentEnrolled, AgentAnswerVerified and AgentInventoryVerified conditions next to its certificate expiry. |
| `withdraw-stalled.sh HOST` | Stalls the host-agent issuer, revokes the Host while its renewal request is approved but unissued, restores the issuer and logs how the request is marked (no manager restart). |
| `first-cert-stalled.sh HOST PORT [HOLD]` | Enrols a host while the issuer is stalled for HOLD seconds, then completes it; logs the manager's `GetHostInfo failed` lines per minute while the Host waited, when the key was pinned and when a signed answer verified. |
| `renewal-slow-issuer.sh HOST [HOLD]` | Stalls the issuer for HOLD seconds as soon as the agent made its renewal request, then restores it; logs the renewal events and whether the agent drew a second key. |
| `watch-renewal.sh HOST [SECONDS] [INTERVAL]` | Logs every change of the Host's `status.agent`, the renewal `CertificateRequest`s, the agent's installed certificate and renewal files, and renewal Events. |

## enrol-lab

- `enrol-lab anchor -signer-key FILE`: the development operation signer's
  grant anchor certificate (for the agent's `grant-anchors.pem`).
- `enrol-lab image -binary FILE -name REF -out TAR [-arg A]...`: a
  single-layer image archive (docker save layout) for `ctr images import`.
- `enrol-lab revoke-probe ...`: probes the Provider over mTLS, runs the
  revocation command, and reports (milliseconds after the command started)
  when it returned, when the Host showed `AgentEnrolled=False/Revoked`, when
  the inventory Secret stopped listing the host and when the Provider first
  refused a call for it.

## Lab arrangement notes

- The manager runs out of cluster, so the scripts add the Provider Service's
  name to the node's `/etc/hosts` (the manager dials
  `<service>.<namespace>.svc.cluster.local`).
- All "hosts" share one VM; the Provider pod reaches each agent on the node
  address.
