#!/usr/bin/env bash
# Handshake faults that are not a refusal of the Provider's certificate, on
# one host, each followed by a restart of the Provider pod so that the next
# call needs a new TLS handshake; logs the Host's Ready reason and the
# Provider's per-call causes for the host while the fault lasts:
#   signer   the host key signer is stopped: the agent cannot sign the
#            handshake (it ends it with an alert that is not a certificate
#            alert)
#   reset    every TCP segment after the SYN to the agent's port is answered
#            with a reset (iptables REJECT --reject-with tcp-reset), so the
#            transport is reset during, or right after, the handshake
#   handshake-faults.sh HOST PORT [SECONDS]
# Run as root inside a disposable test VM only.
set -uo pipefail

H=${1:?host}; P=${2:?port}; OBSERVE=${3:-75}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
DEPLOY=virtrigaud-provider-$PROVIDER_NS-$PROVIDER_NAME
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }

observe() { # label
  local t0 last="" cur
  $K -n "$PROVIDER_NS" rollout restart deploy/"$DEPLOY" >/dev/null
  $K -n "$PROVIDER_NS" rollout status deploy/"$DEPLOY" --timeout=180s >/dev/null
  t0=$(date +%s); log "$1: Provider restarted"
  while [ $(( $(date +%s) - t0 )) -lt "$OBSERVE" ]; do
    cur=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}: {.status.conditions[?(@.type=="Ready")].message}' | grep -o '([A-Za-z]*)' | head -1)
    [ "$cur" != "$last" ] && { log "$1: +$(( $(date +%s) - t0 ))s Host Ready cause $cur"; last=$cur; }
    sleep 3
  done
  log "$1: Provider causes for $H: $($K -n "$PROVIDER_NS" logs deploy/"$DEPLOY" | grep "\"$H\\\\\" is unavailable" | grep -o '([A-Za-z]*)' | sort | uniq -c | tr '\n' ' ')"
  log "$1: ProviderCertificateRefused lines for $H: $($K -n "$PROVIDER_NS" logs deploy/"$DEPLOY" | grep "\"$H\\\\\"" | grep -c ProviderCertificateRefused)"
}

systemctl stop "vrtg-agent-signer-$H"
observe signer
log "signer: agent log: $(journalctl -u "vrtg-agent-$H" --since "-$((OBSERVE + 30))s" --no-pager -o cat | grep -iE 'signer|refus|handshake' | tail -2 | cut -c1-250 | tr '\n' ' ')"
systemctl start "vrtg-agent-signer-$H"

iptables -I INPUT -p tcp --dport "$P" -m conntrack --ctstate ESTABLISHED -j REJECT --reject-with tcp-reset
observe reset
log "reset: iptables hits: $(iptables -L INPUT -v -n | grep "dpt:$P" | awk '{print $1}')"
iptables -D INPUT -p tcp --dport "$P" -m conntrack --ctstate ESTABLISHED -j REJECT --reject-with tcp-reset

$K -n "$PROVIDER_NS" rollout restart deploy/"$DEPLOY" >/dev/null
$K -n "$PROVIDER_NS" rollout status deploy/"$DEPLOY" --timeout=180s >/dev/null
log "faults removed; Provider restarted"
