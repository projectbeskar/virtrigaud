#!/usr/bin/env bash
# A certificate renewal with a slow issuer: as soon as the host's agent has
# made its renewal request (pki/renewal.csr appears), the host-agent issuer is
# stalled (pointed at a missing Secret) for HOLD seconds, then restored. Logs
# the renewal events with their times until AgentRenewed or
# AgentRenewalPinnedAtExpiry, and whether the agent drew a second key.
#   renewal-slow-issuer.sh HOST [HOLD]
# Run as root inside a disposable test VM only.
set -uo pipefail

H=${1:?host}; HOLD=${2:-60}
PROVIDER_NS=${PROVIDER_NS:-infra}
ISSUER=${ISSUER:-host-agent-ca}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
AG=/var/lib/vrtg-agent-$H
log() { printf '%s [%s] %s\n' "$(date -u +%FT%T.%3NZ)" "$H" "$*"; }

until [ -f "$AG/pki/renewal.csr" ]; do sleep 0.2; done
$K patch clusterissuer "$ISSUER" --type=merge -p '{"spec":{"ca":{"secretName":"lab-missing-ca"}}}' >/dev/null
log "agent made its renewal request; issuer stalled for ${HOLD}s"
sleep "$HOLD"
$K patch clusterissuer "$ISSUER" --type=merge -p "{\"spec\":{\"ca\":{\"secretName\":\"$ISSUER\"}}}" >/dev/null
log "issuer restored"
for _ in $(seq 1 900); do
  $K get events -n "$PROVIDER_NS" --field-selector involvedObject.name="$H" -o jsonpath='{.items[*].reason}' | grep -qE 'AgentRenewed|AgentRenewalPinnedAtExpiry|AgentRenewalGivenUp' && break
  sleep 1
done
$K get events -n "$PROVIDER_NS" --field-selector involvedObject.name="$H" --sort-by=.lastTimestamp \
  -o custom-columns=T:.lastTimestamp,N:.count,R:.reason --no-headers | grep -i renew
log "agent renewal log:"
journalctl -u "vrtg-agent-$H" --no-pager -o cat | grep -iE 'renewal (requested|discarded)|renewed certificate' | cut -c1-200
