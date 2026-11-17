#!/usr/bin/env bash
# Negative check of the Provider identity certificate: re-issues the
# Provider's certificate (its cert-manager Certificate) WITHOUT the trailing
# /<Provider UID> in its URI, restarts the Provider pod (it reads the
# certificate at start only), and logs every SECONDS how the Host reports the
# host (Ready reason and message) plus the agent's refusal log lines. Then
# restores the correct URI and restarts the Provider again.
# With ISSUE=secret (needed once the Provider identity approver-policy sample
# is applied, since it denies such a request), the Certificate is set aside
# and the bad certificate is issued by hand (openssl, the lab's Provider
# identity CA in $LAB/pki) into the Secret; the restore re-applies the
# Certificate, so cert-manager issues the correct one again.
#   [ISSUE=certificate|secret] provider-cert-nouid.sh HOST [SECONDS_TO_OBSERVE] [INTERVAL]
# Run as root inside a disposable test VM only.
set -euo pipefail

H=${1:?host}; OBSERVE=${2:-150}; INTERVAL=${3:-5}
TRUST_DOMAIN=${TRUST_DOMAIN:-enrol.test}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
LAB=${LAB:-/root/lab}
ISSUE=${ISSUE:-certificate}
CERT=$PROVIDER_NAME-provider-identity
DEPLOY=virtrigaud-provider-$PROVIDER_NS-$PROVIDER_NAME
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
uid=$($K -n "$PROVIDER_NS" get provider "$PROVIDER_NAME" -o jsonpath='{.metadata.uid}')
base="spiffe://$TRUST_DOMAIN/virtrigaud/provider/$PROVIDER_NS/$PROVIDER_NAME"

reissue() { # uri
  local before
  before=$($K -n "$PROVIDER_NS" get secret "$PROVIDER_NAME-credentials" -o jsonpath='{.data.tls\.crt}')
  if [ "$ISSUE" = secret ] && [ "$1" = "$base" ]; then
    $K -n "$PROVIDER_NS" get certificate "$CERT" -o yaml > "$LAB/run/$CERT.saved.yaml"
    $K -n "$PROVIDER_NS" delete certificate "$CERT" >/dev/null
    ( cd "$LAB/run" && umask 077 && openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out nouid.key 2>/dev/null &&
      openssl req -new -key nouid.key -subj "/CN=$PROVIDER_NAME" -out nouid.csr &&
      printf '%s\n' "basicConstraints=critical,CA:FALSE" "keyUsage=critical,digitalSignature" "extendedKeyUsage=clientAuth" \
        "subjectAltName=URI:$1" > nouid.ext &&
      openssl x509 -req -in nouid.csr -CA "$LAB/pki/provider-ca.crt" -CAkey "$LAB/pki/provider-ca.key" -CAcreateserial -days 1 \
        -sha256 -extfile nouid.ext -out nouid.crt 2>/dev/null )
    $K -n "$PROVIDER_NS" create secret tls "$PROVIDER_NAME-credentials" --cert="$LAB/run/nouid.crt" --key="$LAB/run/nouid.key" \
      --dry-run=client -o yaml | $K apply -f - >/dev/null
  elif [ "$ISSUE" = secret ]; then
    grep -v '^  \(resourceVersion\|uid\|creationTimestamp\|generation\):' "$LAB/run/$CERT.saved.yaml" | sed '/^status:/,$d' | $K apply -f - >/dev/null
  else
    $K -n "$PROVIDER_NS" patch certificate "$CERT" --type=merge -p "{\"spec\":{\"uris\":[\"$1\"]}}" >/dev/null
  fi
  for _ in $(seq 1 120); do
    [ "$($K -n "$PROVIDER_NS" get secret "$PROVIDER_NAME-credentials" -o jsonpath='{.data.tls\.crt}')" != "$before" ] && break; sleep 1
  done
  log "certificate re-issued: $($K -n "$PROVIDER_NS" get secret "$PROVIDER_NAME-credentials" -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -ext subjectAltName | tail -1 | tr -d ' ')"
  $K -n "$PROVIDER_NS" rollout restart deploy/"$DEPLOY" >/dev/null
  $K -n "$PROVIDER_NS" rollout status deploy/"$DEPLOY" --timeout=180s >/dev/null
  log "Provider restarted"
}

reissue "$base"
t0=$(date +%s); last=""
while [ $(( $(date +%s) - t0 )) -lt "$OBSERVE" ]; do
  cur=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}: {.status.conditions[?(@.type=="Ready")].message}' | cut -c1-220)
  [ "$cur" != "$last" ] && { log "+$(( $(date +%s) - t0 ))s Ready=$cur"; last=$cur; }
  sleep "$INTERVAL"
done
log "agent log lines mentioning the Provider certificate:"
journalctl -u "vrtg-agent-$H" --since "@$t0" --no-pager -o cat | grep -i 'provider' | cut -c1-400 | head -5
log "Provider log lines:"
$K -n "$PROVIDER_NS" logs deploy/"$DEPLOY" | grep -i 'refus' | cut -c1-300 | head -5 || true

reissue "$base/$uid"
for _ in $(seq 1 30); do
  v=$($K -n "$PROVIDER_NS" get host "$H" -o jsonpath='{.status.conditions[?(@.type=="AgentAnswerVerified")].lastTransitionTime} {.status.conditions[?(@.type=="Ready")].reason}')
  sleep 5
done
log "restored: Ready reason / AgentAnswerVerified transition: $v"
