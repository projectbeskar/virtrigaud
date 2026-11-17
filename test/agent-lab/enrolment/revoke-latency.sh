#!/usr/bin/env bash
# Revocation latency: for each sample, provision and enrol a fresh host
# (a revoked Host cannot be un-revoked), check that the Provider routes calls
# to it, then revoke it with `vrtg host revoke` while probing the Provider
# with GetHostInfo for that host every few milliseconds. One JSON line per
# sample (enrol-lab revoke-probe): time to the Host's AgentEnrolled=False/
# Revoked, to the inventory Secret without the host, and to the first call
# the Provider refuses. The sample's agent units are stopped afterwards.
#   revoke-latency.sh FIRST LAST [BASEPORT]
# Run as root inside a disposable test VM only.
set -euo pipefail

FIRST=${1:?first}; LAST=${2:?last}; BASEPORT=${3:-9500}
LAB=${LAB:-/root/lab}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
AK="$LAB/kube/admin.kubeconfig"
svc=virtrigaud-provider-$PROVIDER_NS-$PROVIDER_NAME
fqdn=$svc.$PROVIDER_NS.svc.cluster.local
OUT=${OUT:-$LAB/out/revoke-latency.jsonl}
mkdir -p "$(dirname "$OUT")"

for i in $(seq "$FIRST" "$LAST"); do
  H=$(printf 'r%02d' "$i"); P=$((BASEPORT + i))
  bash "$LAB/scripts/host-provision.sh" "$H" "$P" >/dev/null
  bash "$LAB/scripts/enrol-online.sh" "$H" "$P" > "$LAB/out/enrol-$H.log" 2>&1
  tail -1 "$LAB/out/enrol-$H.log"
  "$LAB/bin/enrol-lab" revoke-probe -kubeconfig "$AK" -namespace "$PROVIDER_NS" -host "$H" \
    -inventory-secret "$PROVIDER_NAME-hosts" -provider "$fqdn:9443" \
    -tls-cert "$LAB/pki/mtls.crt" -tls-key "$LAB/pki/mtls.key" -tls-ca "$LAB/pki/mtls-ca.crt" -server-name "$fqdn" \
    -revoke-cmd "$LAB/bin/vrtg --kubeconfig $AK -n $PROVIDER_NS host revoke --host $H" | tee -a "$OUT"
  systemctl stop "vrtg-agent-$H" "vrtg-agent-signer-$H"
done
