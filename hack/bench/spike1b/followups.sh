#!/usr/bin/env bash
# Spike 1B follow-ups: (1) delete while a Create is in flight, (2) API cost of
# a create on an otherwise empty cluster. Results: results/followups/.
set -uo pipefail
ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
# shellcheck source=run.sh
source "$ROOT/hack/bench/spike1b/run.sh"
RES="${RES:-$ROOT/docs/performance/spike-1b/results}/followups"
mkdir -p "$RES/inflight" "$RES/empty"
build_driver

# (1) On the extras cluster (1,000 seeded VMs, config D): Create takes 5 s,
# the VMs are deleted 4 s after their create, i.e. while Creates run.
"$DRIVER" fakeconfig --provider fake-p0 --json '{"latency":{"create":{"mean":"5s","jitter":"0s"}}}' >/dev/null
"$DRIVER" burst --run "inflight$(date +%s)" --count 200 --delete-after 4s --timeout 10m --out "$RES/inflight" >"$RES/inflight/burst.log" 2>&1
sleep 60
"$DRIVER" fakeconfig --provider fake-p0 --json '{"latency":{"create":{"mean":"200ms","jitter":"100ms"}}}' >/dev/null
audit "$RES/inflight" after-inflight-deletes
"$DRIVER" fakeconfig --provider fake-p0 >"$RES/inflight/fakeconfig.json"

# (2) Empty cluster: 10 hosts, no seeded VMs, config D; one 100-VM burst.
cluster_up
stage_setup 1 10 0 2 >"$RES/empty/setup.txt"
use_config D "$RES/empty"
collector_start "$RES/empty"
sleep 60
burst "$RES/empty" 100 --timeout 10m
sleep 30
collector_stop
python3 "$ROOT/hack/bench/spike1b/analyze.py" "$RES/empty" >"$RES/empty/analysis.txt" 2>&1
audit "$RES/empty" final
echo followups done
