#!/usr/bin/env bash
# Manager restarts and withdrawals around a host's certificate renewal, and
# what the manager then writes on the renewal's CertificateRequest:
#   renewal-restart.sh HOST midflight   restart the manager as soon as the
#       renewal is Issued (certificate published, new key not pinned yet),
#       wait for AgentRenewed, restart the manager again, and print the
#       request's conditions (expected: no VirtRigaudCertificateRefused)
#   renewal-restart.sh HOST completed   (renewal already finished) restart the
#       manager and print the conditions of the Host's renewal requests
#   renewal-restart.sh HOST withdraw    revoke the Host as soon as the renewal
#       is Issued (before its key is pinned) and print the request's
#       conditions (expected: VirtRigaudCertificateRefused=True)
#   renewal-restart.sh HOST withdraw-wait   the same without any manager
#       restart: logs the request's conditions every 10 s for 5 minutes
# Run as root inside a disposable test VM only.
set -uo pipefail

H=${1:?host}; MODE=${2:?midflight|completed|withdraw}
LAB=${LAB:-/root/lab}
PROVIDER_NS=${PROVIDER_NS:-infra}
ENROL_NS=${ENROL_NS:-host-agent-pki}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
log() { printf '%s [%s] %s\n' "$(date -u +%FT%TZ)" "$H" "$*"; }
phase() { $K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.renewal.phase}'; }
request() { $K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.renewal.certificateRequest}' | cut -d/ -f2; }
conds() { $K -n "$ENROL_NS" get certificaterequest "$1" -o jsonpath='{range .status.conditions[*]}{.lastTransitionTime} {.type}={.status}/{.reason}: {.message}; {end}'; }
restart_manager() { systemctl restart virtrigaud-manager; log "manager restarted"; sleep 25; }
host_requests() {
  $K -n "$ENROL_NS" get certificaterequests -o jsonpath="{range .items[?(@.metadata.annotations.enrolment\.virtrigaud\.io/host==\"$PROVIDER_NS/$H\")]}{.metadata.name}{\"\n\"}{end}" | grep '^virtrigaud-renewal-'
}

wait_issued() {
  for _ in $(seq 1 600); do [ "$(phase)" = Issued ] && return 0; sleep 1; done
  log "renewal never reached Issued"; return 1
}

case "$MODE" in
midflight)
  wait_issued || exit 1
  cr=$(request); log "renewal Issued, request $cr"
  restart_manager
  for _ in $(seq 1 300); do
    $K get events -n "$PROVIDER_NS" --field-selector involvedObject.name="$H" -o jsonpath='{.items[*].reason}' | grep -q AgentRenewed && break; sleep 2
  done
  log "after AgentRenewed: pins $($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.keyPins}')"
  restart_manager
  log "request $cr: $(conds "$cr")" ;;
completed)
  restart_manager
  for cr in $(host_requests); do log "request $cr: $(conds "$cr")"; done ;;
withdraw)
  wait_issued || exit 1
  cr=$(request); log "renewal Issued, request $cr; revoking the Host before the key is pinned"
  "$LAB/bin/vrtg" --kubeconfig "$LAB/kube/admin.kubeconfig" -n "$PROVIDER_NS" host revoke --host "$H"
  sleep 15
  log "request $cr: $(conds "$cr")"
  restart_manager
  log "after a manager restart, request $cr: $(conds "$cr")" ;;
withdraw-wait)
  wait_issued || exit 1
  cr=$(request); log "renewal Issued, request $cr; revoking the Host before the key is pinned (no manager restart)"
  "$LAB/bin/vrtg" --kubeconfig "$LAB/kube/admin.kubeconfig" -n "$PROVIDER_NS" host revoke --host "$H"
  last=""
  for i in $(seq 1 30); do
    c=$(conds "$cr"); [ "$c" != "$last" ] && { log "+$((i * 10 - 10))s request $cr: $c"; last=$c; }; sleep 10
  done
  log "manager restarts during the wait: $(journalctl -u virtrigaud-manager --since '-6 min' --no-pager | grep -c 'Starting workers.*host-enrolment-request')" ;;
*) echo "unknown mode" >&2; exit 2 ;;
esac
