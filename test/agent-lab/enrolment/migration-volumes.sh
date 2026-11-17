#!/usr/bin/env bash
# Checks that the clustered Provider for per-host agents gets no VMMigration
# storage volume while a VMMigration names it, against a control Provider
# (type libvirt, never started successfully: only its rendered Deployment
# is looked at) that should get one. Creates a Bound PVC (static hostPath
# PV) labelled as migration storage, one VMMigration per Provider naming it
# as target with spec.storage.pvc.name set to that PVC, and writes their
# status (phase Pending, storagePVCName) as an administrator, as the
# migration controller would. Then logs, every few seconds for SECONDS, the
# PVC volumes of both Deployments and the CH pod's credentials volume, and
# deletes what it created.
#   migration-volumes.sh [SECONDS]
# Run as root inside a disposable test VM only.
set -uo pipefail

OBSERVE=${1:-60}
NS=${PROVIDER_NS:-infra}
CH=${PROVIDER_NAME:-ch}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }

$K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata: {name: lvctl-credentials, namespace: $NS}
stringData: {username: lab, password: lab}
---
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata: {name: lvctl, namespace: $NS}
spec:
  type: libvirt
  endpoint: qemu+ssh://lab@libvirt.invalid/system
  credentialSecretRef: {name: lvctl-credentials}
  runtime:
    mode: Remote
    image: example.invalid/never-pulled:lab
    service: {tls: {enabled: false}}
---
apiVersion: v1
kind: PersistentVolume
metadata: {name: lab-migpv}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  hostPath: {path: /var/tmp/lab-migpv}
  claimRef: {namespace: $NS, name: lab-migpvc}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: lab-migpvc
  namespace: $NS
  labels: {virtrigaud.io/component: migration-storage}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: lab-migpv
  resources: {requests: {storage: 1Gi}}
EOF
for p in "$CH" lvctl; do
  $K apply -f - >/dev/null <<EOF
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
metadata: {name: lab-mig-$p, namespace: $NS}
spec:
  source: {vmRef: {name: lab-absent-vm}}
  target: {name: lab-target-$p, providerRef: {name: $p}}
  storage: {type: pvc, pvc: {name: lab-migpvc}}
  options: {retryPolicy: {maxRetries: 10}}
EOF
done
for _ in $(seq 1 30); do [ "$($K -n "$NS" get pvc lab-migpvc -o jsonpath='{.status.phase}')" = Bound ] && break; sleep 1; done
log "PVC lab-migpvc: $($K -n "$NS" get pvc lab-migpvc -o jsonpath='{.status.phase}')"
t0=$(date +%s); last=""
while [ $(( $(date +%s) - t0 )) -lt "$OBSERVE" ]; do
  for p in "$CH" lvctl; do
    $K -n "$NS" patch vmmigration "lab-mig-$p" --subresource=status --type=merge \
      -p '{"status":{"phase":"Pending","storagePVCName":"lab-migpvc"}}' >/dev/null 2>&1
  done
  sleep 3
  cur="ch-pvc-volumes=[$($K -n "$NS" get deploy "virtrigaud-provider-$NS-$CH" -o jsonpath='{range .spec.template.spec.volumes[?(@.persistentVolumeClaim)]}{.persistentVolumeClaim.claimName} {end}')]"
  cur="$cur lvctl-pvc-volumes=[$($K -n "$NS" get deploy "virtrigaud-provider-$NS-lvctl" -o jsonpath='{range .spec.template.spec.volumes[?(@.persistentVolumeClaim)]}{.persistentVolumeClaim.claimName} {end}' 2>/dev/null)]"
  cur="$cur migrations=[$($K -n "$NS" get vmmigration -o jsonpath='{range .items[*]}{.metadata.name}:{.status.phase}/{.status.storagePVCName} {end}')]"
  [ "$cur" != "$last" ] && { log "+$(( $(date +%s) - t0 ))s $cur"; last=$cur; }
done
for p in "$CH" lvctl; do
  log "ReplicaSets of $p (PVC volumes per pod template): $($K -n "$NS" get rs -l "app.kubernetes.io/instance=virtrigaud-provider-$NS-$p" -o jsonpath='{range .items[*]}{.metadata.name}=[{range .spec.template.spec.volumes[?(@.persistentVolumeClaim)]}{.persistentVolumeClaim.claimName}{end}] {end}' 2>/dev/null)$($K -n "$NS" get rs -o jsonpath='{range .items[*]}{.metadata.name}=[{range .spec.template.spec.volumes[?(@.persistentVolumeClaim)]}{.persistentVolumeClaim.claimName}{end}] {end}' | tr ' ' '\n' | grep "provider-$NS-$p-" | tr '\n' ' ')"
done
log "CH Deployment volumes: $($K -n "$NS" get deploy "virtrigaud-provider-$NS-$CH" -o jsonpath='{range .spec.template.spec.volumes[*]}{.name} {end}')"
log "CH pod: $($K -n "$NS" get pods -l "app.kubernetes.io/instance=virtrigaud-provider-$NS-$CH" -o jsonpath='{range .items[*]}{.metadata.name} {.status.phase} restarts={.status.containerStatuses[0].restartCount} {end}' 2>/dev/null) $($K -n "$NS" get pods -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase},{.status.containerStatuses[0].restartCount} {end}')"
$K -n "$NS" delete vmmigration "lab-mig-$CH" lab-mig-lvctl --wait=false >/dev/null
$K -n "$NS" delete provider lvctl --wait=false >/dev/null
$K -n "$NS" delete pvc lab-migpvc --wait=false >/dev/null
$K delete pv lab-migpv --wait=false >/dev/null
log "cleaned up"
