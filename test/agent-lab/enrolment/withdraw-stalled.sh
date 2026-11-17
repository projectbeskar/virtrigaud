#!/usr/bin/env bash
# Withdraws a host's renewal while its certificate request cannot be issued
# yet, then lets it be issued, with no manager restart, and logs how the
# manager marks the request:
#   1. the host-agent ClusterIssuer is pointed at a Secret that does not
#      exist, so an approved request stays unissued;
#   2. once the Host's renewal shows its CertificateRequest, the events and
#      the request's conditions are logged (approval), the Host is revoked;
#   3. ten seconds later the issuer is restored; the request's conditions
#      are logged every second for two minutes.
#   withdraw-stalled.sh HOST
# Run as root inside a disposable test VM only.
set -uo pipefail

H=${1:?host}
LAB=${LAB:-/root/lab}
PROVIDER_NS=${PROVIDER_NS:-infra}
ENROL_NS=${ENROL_NS:-host-agent-pki}
ISSUER=${ISSUER:-host-agent-ca}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
log() { printf '%s [%s] %s\n' "$(date -u +%FT%T.%3NZ)" "$H" "$*"; }
conds() { $K -n "$ENROL_NS" get certificaterequest "$1" -o jsonpath='{range .status.conditions[*]}{.lastTransitionTime} {.type}={.status}/{.reason}; {end}'; }

$K patch clusterissuer "$ISSUER" --type=merge -p '{"spec":{"ca":{"secretName":"lab-missing-ca"}}}' >/dev/null
log "issuer stalled"
cr=""
for _ in $(seq 1 900); do
  cr=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.renewal.certificateRequest}' | cut -d/ -f2)
  [ -n "$cr" ] && break; sleep 1
done
[ -n "$cr" ] || { log "no renewal request"; $K patch clusterissuer "$ISSUER" --type=merge -p "{\"spec\":{\"ca\":{\"secretName\":\"$ISSUER\"}}}" >/dev/null; exit 1; }
sleep 3
log "renewal request $cr: $(conds "$cr") phase=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.renewal.phase}')"
log "events: $($K get events -n "$PROVIDER_NS" --field-selector involvedObject.name="$H" -o jsonpath='{range .items[*]}{.reason} {end}')"
"$LAB/bin/vrtg" --kubeconfig "$LAB/kube/admin.kubeconfig" -n "$PROVIDER_NS" host revoke --host "$H"
log "revoked"
sleep 10
log "before issuance: $(conds "$cr")"
$K patch clusterissuer "$ISSUER" --type=merge -p "{\"spec\":{\"ca\":{\"secretName\":\"$ISSUER\"}}}" >/dev/null
log "issuer restored"
last=""
for i in $(seq 1 120); do
  c=$(conds "$cr"); [ "$c" != "$last" ] && { log "+${i}s $c"; last=$c; }
  sleep 1
done
log "manager restarts during the run: $(journalctl -u virtrigaud-manager --since '-5 min' --no-pager | grep -c 'Starting workers.*host-enrolment-request')"
log "events: $($K get events -n "$PROVIDER_NS" --field-selector involvedObject.name="$H" -o jsonpath='{range .items[*]}{.reason} {end}')"
