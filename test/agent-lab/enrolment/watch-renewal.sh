#!/usr/bin/env bash
# Records a host's certificate renewal as it happens: every INTERVAL seconds,
# one line when anything changed among the Host's status.agent (renewal
# phase, key pins, certificate expiry, enrolment id), the renewal
# CertificateRequests (approved/denied/ready, approver) and the agent's
# installed certificate; plus every new Event on the Host.
#   watch-renewal.sh HOST [SECONDS] [INTERVAL]
# Run as root inside a disposable test VM only.
set -uo pipefail

H=${1:?host}; DURATION=${2:-3600}; INTERVAL=${3:-2}
PROVIDER_NS=${PROVIDER_NS:-infra}
ENROL_NS=${ENROL_NS:-host-agent-pki}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
AG=/var/lib/vrtg-agent-$H
end=$(( $(date +%s) + DURATION ))
last=""; seen=""
while [ "$(date +%s)" -lt "$end" ]; do
  agent=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='enrol={.status.agent.enrolmentID} pins={.status.agent.keyPins} expiry={.status.agent.certificateExpiry} renewal={.status.agent.renewal}' 2>/dev/null)
  crs=$($K -n "$ENROL_NS" get certificaterequests -l enrolment.virtrigaud.io/renewal=true \
    -o jsonpath='{range .items[*]}{.metadata.name}[{.spec.username}|{range .status.conditions[*]}{.type}={.status}/{.reason};{end}]{" "}{end}' 2>/dev/null)
  crt=$(openssl x509 -in "$AG/pki/host.crt" -noout -serial -enddate 2>/dev/null | tr '\n' ' ')
  staged=$(ls "$AG/pki" 2>/dev/null | tr '\n' ',')
  cur="$agent | CR: $crs | host.crt: $crt | pki: $staged"
  if [ "$cur" != "$last" ]; then printf '%s STATE %s\n' "$(date -u +%FT%TZ)" "$cur"; last=$cur; fi
  while IFS= read -r ev; do
    [ -z "$ev" ] && continue
    case "$seen" in *"$ev"*) ;; *) printf '%s EVENT %s\n' "$(date -u +%FT%TZ)" "$ev"; seen="$seen
$ev";; esac
  done < <($K -n "$PROVIDER_NS" get events --field-selector involvedObject.name="$H" \
      -o jsonpath='{range .items[*]}{.lastTimestamp} {.reason}: {.message}{"\n"}{end}' 2>/dev/null | grep -i 'renew\|pinned\|overlap')
  sleep "$INTERVAL"
done
