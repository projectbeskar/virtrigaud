#!/usr/bin/env bash
# Records, once a second, every change of a Host's Ready, AgentEnrolled,
# AgentAnswerVerified and AgentInventoryVerified conditions (status, reason,
# message, transition time) next to its pinned certificate expiry, so the
# moment a condition turns CertificateExpired can be compared with the
# expiry itself, and how fast it leaves it after a re-key.
#   expiry-watch.sh HOST [SECONDS]
# Run as root inside a disposable test VM only.
set -uo pipefail

H=${1:?host}; DURATION=${2:-3600}
PROVIDER_NS=${PROVIDER_NS:-infra}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
end=$(( $(date +%s) + DURATION )); last=""
while [ "$(date +%s)" -lt "$end" ]; do
  cur=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='expiry={.status.agent.certificateExpiry}{"\n"}{range .status.conditions[?(@.type=="Ready")]}Ready={.status}/{.reason} @{.lastTransitionTime}: {.message}{"\n"}{end}{range .status.conditions[?(@.type=="AgentEnrolled")]}AgentEnrolled={.status}/{.reason} @{.lastTransitionTime}{"\n"}{end}{range .status.conditions[?(@.type=="AgentAnswerVerified")]}AgentAnswerVerified={.status}/{.reason} @{.lastTransitionTime}: {.message}{"\n"}{end}{range .status.conditions[?(@.type=="AgentInventoryVerified")]}AgentInventoryVerified={.status}/{.reason} @{.lastTransitionTime}: {.message}{"\n"}{end}' 2>/dev/null)
  if [ "$cur" != "$last" ]; then
    printf '%s CHANGE\n' "$(date -u +%FT%T.%3NZ)"
    diff <(echo "$last") <(echo "$cur") | grep '^>' | sed 's/^> /  /' | cut -c1-320
    last=$cur
  fi
  sleep 1
done
