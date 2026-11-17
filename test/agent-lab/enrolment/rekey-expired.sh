#!/usr/bin/env bash
# Re-keys a Host, as docs/host-agent-enrolment.md ("Re-keying an enrolled
# Host") describes, for example after its certificate expired: opens a
# re-key window (vrtg host rekey), makes the host draw a new key (the old key
# file removed, enroll csr), submits and fetches the certificate as the
# administrator, restarts the signer, installs with --replace and restarts
# the agent; then waits for AgentEnrolled=True and a verified signed answer.
# Removes a fault-injection drop-in of the signer first if one exists.
#   rekey-expired.sh HOST
# Run as root inside a disposable test VM only.
set -euo pipefail

H=${1:?host}
LAB=${LAB:-/root/lab}
PROVIDER_NS=${PROVIDER_NS:-infra}
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
cond() { $K -n "$PROVIDER_NS" get host "$H" -o jsonpath="{.status.conditions[?(@.type==\"$1\")].status}/{.status.conditions[?(@.type==\"$1\")].reason}"; }

log "before: AgentEnrolled=$(cond AgentEnrolled) Ready=$(cond Ready) enrolment=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.enrolmentID}')"
if [ -d "/etc/systemd/system/vrtg-agent-signer-$H.service.d" ]; then
  rm -rf "/etc/systemd/system/vrtg-agent-signer-$H.service.d"; systemctl daemon-reload; log "signer fault drop-in removed"
fi
"$LAB/bin/vrtg" $ADMIN -n "$PROVIDER_NS" host rekey --host "$H"
for _ in $(seq 1 120); do
  [ -n "$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.pendingEnrolmentID}')" ] && break; sleep 0.5
done
log "re-key window open: pendingEnrolmentID=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.pendingEnrolmentID}')"
$K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{range .status.agent.requestURIs[*]}{@}{"\n"}{end}' > "$HANDOFF/request-uris.new"
mv "$HANDOFF/request-uris.new" "$HANDOFF/request-uris"; chmod 0644 "$HANDOFF/request-uris"
rm -f "$SG/host.key" "$HANDOFF/host.csr" "$HANDOFF/host-cert.pem"
( cd /tmp && sudo -u virtrigaud-signer "$BIN" enroll csr --uris-file "$HANDOFF/request-uris" --key-file "$SG/host.key" --out - ) > "$HANDOFF/host.csr"
chmod 0644 "$HANDOFF/host.csr"
out=$("$LAB/bin/vrtg" $ADMIN -n "$PROVIDER_NS" host enroll --host "$H" --csr "$HANDOFF/host.csr" \
  --issuer-namespace "$ENROL_NS" --issuer "ClusterIssuer/$ISSUER")
CR=$(echo "$out" | sed -n "s|^Created CertificateRequest $ENROL_NS/\([^ ]*\) .*|\1|p")
log "re-key request $CR created"
for _ in $(seq 1 240); do
  "$LAB/bin/vrtg" $ADMIN -n "$PROVIDER_NS" host enroll --host "$H" --issuer-namespace "$ENROL_NS" \
    --request "$CR" --certificate-out "$HANDOFF/host-cert.pem" >/dev/null 2>"$HANDOFF/cert-out.err" && break
  sleep 0.5
done
[ -s "$HANDOFF/host-cert.pem" ] || { cat "$HANDOFF/cert-out.err"; exit 1; }
chmod 0644 "$HANDOFF/host-cert.pem"
log "pinned certificate written"
systemctl restart "vrtg-agent-signer-$H"
( cd /tmp && sudo -u virtrigaud-agent "$BIN" enroll install --replace --csr "$HANDOFF/host.csr" --certificate "$HANDOFF/host-cert.pem" \
    --uris-file "$HANDOFF/request-uris" --state-dir "$AG" --signer-socket "$SOCK" --signer-key-file "$SG/host.key" )
systemctl restart "vrtg-agent-$H"
log "installed and agent restarted"
for _ in $(seq 1 200); do
  [ "$(cond AgentAnswerVerified)" = True/Verified ] && [ "$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.conditions[?(@.type=="AgentAnswerVerified")].lastTransitionTime}')" \> "$(date -u -d '-5 min' +%FT%TZ)" ] && break
  sleep 1
done
log "after: AgentEnrolled=$(cond AgentEnrolled) AgentAnswerVerified=$(cond AgentAnswerVerified) Ready=$(cond Ready) expiry=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.certificateExpiry}')"
if [ "$(cond AgentAnswerVerified)" = True/Verified ]; then log "PASS re-key: a signed host answer verified with the new key"; else
  log "FAIL re-key: AgentAnswerVerified=$(cond AgentAnswerVerified)"; exit 1; fi
