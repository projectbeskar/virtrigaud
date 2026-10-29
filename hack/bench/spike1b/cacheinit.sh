#!/usr/bin/env bash
# Spike 1B: manager cache initialisation at a given seeded size. Recreates the
# cluster, seeds V VMs on H hosts, starts the manager (config X, optional
# memory override) and records the seconds from container start to the
# VirtualMachine controller's "Starting workers" log line (informer caches
# synced), plus the restart count and peak working set over the next minutes.
#   cacheinit.sh H V X [MEMORY]   e.g. cacheinit.sh 1000 100000 D 6Gi
set -uo pipefail
ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
# shellcheck source=run.sh
source "$ROOT/hack/bench/spike1b/run.sh"
HOSTS="$1" SEED="$2" CFG="$3" MEM="${4:-}"
RES="${RES:-$ROOT/docs/performance/spike-1b/results}/cache-init"
mkdir -p "$RES"
out="$RES/h${HOSTS}-v${SEED}-${CFG}${MEM:+-$MEM}.txt"
build_driver
cluster_up
stage_setup 1 "$HOSTS" "$SEED" 10 >/dev/null
scale_manager 0
extra=()
[[ -n "$MEM" ]] && extra=(--set "manager.resources.limits.memory=$MEM")
$H upgrade virtrigaud "$ROOT/charts/virtrigaud" -n "$NS" -f "$ROOT/hack/bench/spike1b/values-$CFG.yaml" "${extra[@]}" >/dev/null
$K -n "$NS" scale deploy/virtrigaud-manager --replicas=1 >/dev/null
sleep 5
pod="$($K -n "$NS" get pod -l app.kubernetes.io/name=virtrigaud -o jsonpath='{.items[0].metadata.name}')"
for _ in $(seq 1 300); do
	$K -n "$NS" logs "$pod" 2>/dev/null | grep 'Starting workers' | grep -q virtualmachine && break
	sleep 1
done
started="$($K -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.running.startedAt}')"
workers="$($K -n "$NS" logs "$pod" 2>/dev/null | grep 'Starting workers' | grep virtualmachine | head -1 | awk '{print $1}')"
{
	echo "hosts=$HOSTS vms=$SEED config=$CFG memory=${MEM:-chart}"
	echo "container_started=$started"
	echo "vm_workers_started=$workers"
	python3 - "$started" "$workers" <<'PY'
import sys, datetime
p = lambda s: datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))
try:
    print(f"cache_init_seconds={(p(sys.argv[2]) - p(sys.argv[1])).total_seconds():.1f}")
except Exception as e:
    print(f"cache_init_seconds=unknown ({e})")
PY
} >"$out"
peak=0
for _ in $(seq 1 24); do
	ws=$($K get --raw "/api/v1/nodes/virtrigaud-spike1b-worker/proxy/metrics/cadvisor" 2>/dev/null | grep '^container_memory_working_set_bytes{.*container="manager"' | awk '{print $2}' | sort -g | tail -1)
	[[ -n "$ws" ]] && peak=$(python3 -c "print(max($peak, int(float('$ws'))))")
	sleep 5
done
restarts="$($K -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].lastState.terminated.reason}')"
echo "peak_working_set_mib_first_2min=$((peak / 1048576))" >>"$out"
echo "restarts_after_2min=$restarts" >>"$out"
cat "$out"
