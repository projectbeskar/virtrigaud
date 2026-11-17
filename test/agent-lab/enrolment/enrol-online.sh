#!/usr/bin/env bash
# Online enrolment of one host, step by step as docs/host-agent-enrolment.md
# and docs/per-host-agent-development.md#enrolment describe it:
#   enrol-online.sh HOST PORT [DURATION]
# The host must be provisioned (host-provision.sh). The administrator steps
# use the enrolment administrator's kubeconfig; the host steps run as the
# signer's and the agent's users. Prints a timestamped line per step.
# Run as root inside a disposable test VM only.
set -euo pipefail

H=${1:?host}; P=${2:?port}; DURATION=${3:-720h}
LAB=${LAB:-/root/lab}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
ENROL_NS=${ENROL_NS:-host-agent-pki}
ISSUER=${ISSUER:-host-agent-ca}
ADMIN="--kubeconfig $LAB/kube/admin.kubeconfig"
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
AG=/var/lib/vrtg-agent-$H
SG=/var/lib/vrtg-signer-$H
SOCK=/run/vrtg-signer-$H/signer.sock
HANDOFF=/run/vrtg-enrol-$H
BIN=/usr/lib/virtrigaud/vrtg-agent
log() { printf '%s [%s] %s\n' "$(date -u +%FT%T.%3NZ)" "$H" "$*"; }
nodeip=$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')

# 1. The Host is prepared on the manager (an administrator-assigned UUID as
#    its identity anchor).
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
log "Host created"
for _ in $(seq 1 120); do
  n=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.requestURIs}' | tr -cd ',' | wc -c)
  [ "$n" -ge 4 ] && break; sleep 0.5
done
install -d -m 0755 -o root -g root "$HANDOFF"
$K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{range .status.agent.requestURIs[*]}{@}{"\n"}{end}' > "$HANDOFF/request-uris"
chmod 0644 "$HANDOFF/request-uris"
log "requestURIs published: $(wc -l < "$HANDOFF/request-uris") URIs"

# 2. enroll csr as the signer's user (key created; request to stdout).
( cd /tmp && sudo -u virtrigaud-signer "$BIN" enroll csr --uris-file "$HANDOFF/request-uris" --key-file "$SG/host.key" --out - ) > "$HANDOFF/host.csr"
chmod 0644 "$HANDOFF/host.csr"
log "CSR written"

# 3. The administrator submits it.
out=$("$LAB/bin/vrtg" $ADMIN -n "$PROVIDER_NS" host enroll --host "$H" --csr "$HANDOFF/host.csr" \
  --issuer-namespace "$ENROL_NS" --issuer "ClusterIssuer/$ISSUER" --duration "$DURATION")
echo "$out"
CR=$(echo "$out" | sed -n "s|^Created CertificateRequest $ENROL_NS/\([^ ]*\) .*|\1|p")
echo "$CR" > "$HANDOFF/request-name"
log "CertificateRequest $CR created"
for _ in $(seq 1 240); do
  if "$LAB/bin/vrtg" $ADMIN -n "$PROVIDER_NS" host enroll --host "$H" --issuer-namespace "$ENROL_NS" \
      --request "$CR" --certificate-out "$HANDOFF/host-cert.pem" >/dev/null 2>"$HANDOFF/cert-out.err"; then break; fi
  sleep 0.5
done
[ -s "$HANDOFF/host-cert.pem" ] || { cat "$HANDOFF/cert-out.err"; exit 1; }
chmod 0644 "$HANDOFF/host-cert.pem"
log "pinned certificate written ($(openssl x509 -in "$HANDOFF/host-cert.pem" -noout -enddate))"

# 4. The signer is (re)started so it loads the key.
systemctl restart "vrtg-agent-signer-$H"
log "signer started"

# 5. enroll install as the agent's user.
( cd /tmp && sudo -u virtrigaud-agent "$BIN" enroll install --csr "$HANDOFF/host.csr" --certificate "$HANDOFF/host-cert.pem" \
    --uris-file "$HANDOFF/request-uris" --state-dir "$AG" --signer-socket "$SOCK" --signer-key-file "$SG/host.key" )
log "certificate installed"

# 6. The agent starts.
systemctl restart "vrtg-agent-$H"
log "agent started: $(systemctl show -p StatusText --value "vrtg-agent-$H")"
for _ in $(seq 1 200); do
  v=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.conditions[?(@.type=="AgentAnswerVerified")].status}')
  [ "$v" = True ] && break; sleep 1
done
log "AgentEnrolled=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.conditions[?(@.type=="AgentEnrolled")].reason}') AgentAnswerVerified=$v"
if [ "$v" = True ]; then log "PASS online enrolment: a signed host answer verified"; else
  log "FAIL online enrolment: AgentAnswerVerified=$v"; exit 1; fi
