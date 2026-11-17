#!/usr/bin/env bash
# Provokes optimistic-lock conflicts on a Host's pins Secret during its
# certificate renewal: from the moment the renewal shows in the Host status
# (or AgentRenewalRequested appears) until AgentRenewed, it rewrites a
# harmless annotation on the pins Secret every INTERVAL seconds, so each
# manager write that carries an older resourceVersion conflicts. Afterwards
# it prints the renewal events and the manager's ERROR lines naming the Host
# or its pins Secret since the loop started.
#   pins-conflict.sh HOST [INTERVAL] [TIMEOUT]
# Run as root inside a disposable test VM only.
set -uo pipefail

H=${1:?host}; INTERVAL=${2:-0.2}; TIMEOUT=${3:-900}
PROVIDER_NS=${PROVIDER_NS:-infra}
ENROL_NS=${ENROL_NS:-host-agent-pki}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
log() { printf '%s [%s] %s\n' "$(date -u +%FT%TZ)" "$H" "$*"; }
secret=$($K -n "$ENROL_NS" get secrets -l app.kubernetes.io/component=host-agent-pins \
  -o jsonpath="{range .items[?(@.metadata.annotations.enrolment\.virtrigaud\.io/host==\"$PROVIDER_NS/$H\")]}{.metadata.name}{end}")
[ -n "$secret" ] || { log "no pins Secret"; exit 1; }
events() { $K get events -n "$PROVIDER_NS" --field-selector involvedObject.name="$H" -o jsonpath='{.items[*].reason}'; }
end=$(( $(date +%s) + TIMEOUT ))
until events | grep -q AgentRenewalRequested || [ -n "$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.agent.renewal.phase}')" ]; do
  [ "$(date +%s)" -gt "$end" ] && { log "no renewal started"; exit 1; }; sleep 0.5
done
since=$(date -u +%FT%TZ); n=0
log "renewal started; bumping $ENROL_NS/$secret every ${INTERVAL}s"
until events | grep -q AgentRenewed || [ "$(date +%s)" -gt "$end" ]; do
  $K -n "$ENROL_NS" annotate secret "$secret" --overwrite "lab.example/bump=$n" >/dev/null 2>&1 && n=$((n + 1))
  sleep "$INTERVAL"
done
log "stopped after $n bumps"
$K get events -n "$PROVIDER_NS" --field-selector involvedObject.name="$H" --sort-by=.lastTimestamp \
  -o custom-columns=T:.lastTimestamp,R:.reason --no-headers | grep -i renew
log "manager ERROR lines since $since naming $H or its pins Secret:"
journalctl -u virtrigaud-manager --since "$(date -d "$since" '+%F %T')" --no-pager | grep -E '\sERROR\s' | grep -E "\"$H\"|infra/$H|$secret" | cut -c1-300
log "manager lines mentioning a conflict since $since: $(journalctl -u virtrigaud-manager --since "$(date -d "$since" '+%F %T')" --no-pager | grep -c 'Operation cannot be fulfilled')"
