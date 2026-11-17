#!/usr/bin/env bash
# Offline enrolment of one host by file copy, as docs/host-agent-enrolment.md
# (CLI, --manifest-out) describes it:
#   enrol-offline.sh HOST PORT
# Three roles, each with its own directory; files move between them only by
# copying (each copy is checked by sha256):
#   host         the agent machine: no cluster access. Its steps run in an
#                empty network namespace (unshare -n), so they cannot reach
#                anything.
#   workstation  the administrator's offline workstation: renders the
#                CertificateRequest with --manifest-out, also under unshare -n
#                and with no kubeconfig.
#   kubectl      wherever "kubectl create -f" runs (the administrator's
#                cluster access); also reads requestURIs and, once pinned,
#                writes the certificate with --certificate-out.
# The host must be provisioned (host-provision.sh). Run as root inside a
# disposable test VM only.
set -euo pipefail

H=${1:?host}; P=${2:?port}
LAB=${LAB:-/root/lab}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
ENROL_NS=${ENROL_NS:-host-agent-pki}
ISSUER=${ISSUER:-host-agent-ca}
AK="$LAB/kube/admin.kubeconfig"
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
AG=/var/lib/vrtg-agent-$H
SG=/var/lib/vrtg-signer-$H
SOCK=/run/vrtg-signer-$H/signer.sock
BIN=/usr/lib/virtrigaud/vrtg-agent
HOSTDIR=/run/vrtg-enrol-$H            # host hand-off directory
WS=$LAB/offline/$H/workstation        # administrator workstation
KC=$LAB/offline/$H/kubectl            # cluster-access machine
log() { printf '%s [%s] %s\n' "$(date -u +%FT%T.%3NZ)" "$H" "$*"; }
copy() { install -m 0644 -o root -g root "$1" "$2"; [ "$(sha256sum < "$1")" = "$(sha256sum < "$2")" ]; log "copied $(basename "$1") -> $2 ($(sha256sum < "$2" | cut -c1-16))"; }
offline() { unshare -n -- "$@"; }
nodeip=$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
install -d -m 0755 -o root -g root "$HOSTDIR"
install -d -m 0700 "$WS" "$KC"

# kubectl machine: the Host and its request URIs.
$K apply -f - <<EOF
apiVersion: infra.virtrigaud.io/v1beta1
kind: Host
metadata: {name: $H, namespace: $PROVIDER_NS}
spec:
  providerRef: {name: $PROVIDER_NAME}
  poolRef: {name: pool}
  endpoint: grpc://$nodeip:$P
  identity: uuid:$(cat /proc/sys/kernel/random/uuid)
  schedulable: true
EOF
for _ in $(seq 1 120); do
  n=$(kubectl --kubeconfig "$AK" -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.requestURIs}' | tr -cd ',' | wc -c)
  [ "$n" -ge 4 ] && break; sleep 0.5
done
kubectl --kubeconfig "$AK" -n "$PROVIDER_NS" get host "$H" -o jsonpath='{range .status.agent.requestURIs[*]}{@}{"\n"}{end}' > "$KC/request-uris"
log "kubectl: request URIs read"
copy "$KC/request-uris" "$HOSTDIR/request-uris"

# host (no network): key and request.
( cd /tmp && offline sudo -u virtrigaud-signer "$BIN" enroll csr --uris-file "$HOSTDIR/request-uris" --key-file "$SG/host.key" --out - ) > "$HOSTDIR/host.csr"
chmod 0644 "$HOSTDIR/host.csr"
log "host: CSR written offline"
copy "$HOSTDIR/host.csr" "$WS/host.csr"

# workstation (no network, no kubeconfig): render the request.
rm -f "$WS/certificaterequest.yaml"
( cd "$WS" && HOME=$WS KUBECONFIG=/nonexistent offline "$LAB/bin/vrtg" -n "$PROVIDER_NS" host enroll --host "$H" --csr "$WS/host.csr" \
    --issuer-namespace "$ENROL_NS" --issuer "ClusterIssuer/$ISSUER" --manifest-out "$WS/certificaterequest.yaml" )
log "workstation: manifest written offline"
copy "$WS/certificaterequest.yaml" "$KC/certificaterequest.yaml"

# kubectl machine: create it as the administrator.
out=$(kubectl --kubeconfig "$AK" create -f "$KC/certificaterequest.yaml")
echo "$out"
CR=$(echo "$out" | sed -n 's|^certificaterequest.cert-manager.io/\([^ ]*\) created|\1|p')
echo "$CR" > "$KC/request-name"
log "kubectl: CertificateRequest $CR created"
for _ in $(seq 1 240); do
  if "$LAB/bin/vrtg" --kubeconfig "$AK" -n "$PROVIDER_NS" host enroll --host "$H" --issuer-namespace "$ENROL_NS" \
      --request "$CR" --certificate-out "$KC/host-cert.pem" >/dev/null 2>"$KC/cert-out.err"; then break; fi
  sleep 0.5
done
[ -s "$KC/host-cert.pem" ] || { cat "$KC/cert-out.err"; exit 1; }
log "kubectl: pinned certificate written"
copy "$KC/host-cert.pem" "$HOSTDIR/host-cert.pem"

# host (no network for the install itself): signer, install, agent.
systemctl restart "vrtg-agent-signer-$H"
( cd /tmp && offline sudo -u virtrigaud-agent "$BIN" enroll install --csr "$HOSTDIR/host.csr" --certificate "$HOSTDIR/host-cert.pem" \
    --uris-file "$HOSTDIR/request-uris" --state-dir "$AG" --signer-socket "$SOCK" --signer-key-file "$SG/host.key" )
log "host: certificate installed offline"
systemctl restart "vrtg-agent-$H"
for _ in $(seq 1 200); do
  v=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.conditions[?(@.type=="AgentAnswerVerified")].status}')
  [ "$v" = True ] && break; sleep 1
done
log "AgentEnrolled=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.conditions[?(@.type=="AgentEnrolled")].reason}') AgentAnswerVerified=$v"
if [ "$v" = True ]; then log "PASS offline enrolment: a signed host answer verified"; else
  log "FAIL offline enrolment: AgentAnswerVerified=$v"; exit 1; fi
