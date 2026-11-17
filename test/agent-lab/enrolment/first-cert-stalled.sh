#!/usr/bin/env bash
# A Host waiting for its first certificate: with the host-agent issuer
# stalled (pointed at a missing Secret), creates the Host and submits its
# enrolment request as documented, holds for HOLD seconds, restores the
# issuer, then fetches the certificate, installs it and starts the agent.
# Logs the manager's `GetHostInfo failed` lines for the Host per minute while
# it waited, when its key was pinned and when a signed answer verified.
#   first-cert-stalled.sh HOST PORT [HOLD]
# The host must be provisioned (host-provision.sh). Run as root inside a
# disposable test VM only.
set -uo pipefail

H=${1:?host}; P=${2:?port}; HOLD=${3:-240}
LAB=${LAB:-/root/lab}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
ENROL_NS=${ENROL_NS:-host-agent-pki}
ISSUER=${ISSUER:-host-agent-ca}
ADMIN="--kubeconfig $LAB/kube/admin.kubeconfig"
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
AG=/var/lib/vrtg-agent-$H; SG=/var/lib/vrtg-signer-$H; SOCK=/run/vrtg-signer-$H/signer.sock
HO=/run/vrtg-enrol-$H; BIN=/usr/lib/virtrigaud/vrtg-agent
log() { printf '%s [%s] %s\n' "$(date -u +%FT%T.%3NZ)" "$H" "$*"; }
nodeip=$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')

$K patch clusterissuer "$ISSUER" --type=merge -p '{"spec":{"ca":{"secretName":"lab-missing-ca"}}}' >/dev/null
log "issuer stalled"
t0=$(date -u '+%F %T')
$K apply -f - >/dev/null <<EOF
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
  [ "$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.requestURIs}' | tr -cd ',' | wc -c)" -ge 4 ] && break; sleep 0.5
done
install -d -m 0755 "$HO"
$K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{range .status.agent.requestURIs[*]}{@}{"\n"}{end}' > "$HO/request-uris"; chmod 0644 "$HO/request-uris"
( cd /tmp && sudo -u virtrigaud-signer "$BIN" enroll csr --uris-file "$HO/request-uris" --key-file "$SG/host.key" --out - ) > "$HO/host.csr" 2>/dev/null
chmod 0644 "$HO/host.csr"
out=$("$LAB/bin/vrtg" $ADMIN -n "$PROVIDER_NS" host enroll --host "$H" --csr "$HO/host.csr" --issuer-namespace "$ENROL_NS" --issuer "ClusterIssuer/$ISSUER" --duration 15m)
CR=$(echo "$out" | sed -n "s|^Created CertificateRequest $ENROL_NS/\([^ ]*\) .*|\1|p")
log "request $CR submitted; holding ${HOLD}s with the issuer stalled"
sleep "$HOLD"
log "GetHostInfo failed lines for $H per minute while waiting:"
journalctl -u virtrigaud-manager --since "$t0" --no-pager | grep 'GetHostInfo failed' | grep "\"name\":\"$H\"" | awk '{print substr($3,1,5)}' | uniq -c
$K patch clusterissuer "$ISSUER" --type=merge -p "{\"spec\":{\"ca\":{\"secretName\":\"$ISSUER\"}}}" >/dev/null
log "issuer restored"
for _ in $(seq 1 600); do [ -n "$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.keyPins}')" ] && break; sleep 0.2; done
log "key pinned"
"$LAB/bin/vrtg" $ADMIN -n "$PROVIDER_NS" host enroll --host "$H" --issuer-namespace "$ENROL_NS" --request "$CR" --certificate-out "$HO/host-cert.pem" >/dev/null
chmod 0644 "$HO/host-cert.pem"
systemctl restart "vrtg-agent-signer-$H"
( cd /tmp && sudo -u virtrigaud-agent "$BIN" enroll install --csr "$HO/host.csr" --certificate "$HO/host-cert.pem" --uris-file "$HO/request-uris" \
    --state-dir "$AG" --signer-socket "$SOCK" --signer-key-file "$SG/host.key" ) >/dev/null 2>&1
systemctl restart "vrtg-agent-$H"
log "agent started"
for _ in $(seq 1 600); do
  [ "$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.conditions[?(@.type=="AgentAnswerVerified")].status}')" = True ] && break; sleep 0.2
done
log "AgentAnswerVerified=True"
log "GetHostInfo failed lines for $H in total: $(journalctl -u virtrigaud-manager --since "$t0" --no-pager | grep 'GetHostInfo failed' | grep -c "\"name\":\"$H\"")"
