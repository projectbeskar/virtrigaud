#!/usr/bin/env bash
# Spike 1B: churn, provider-latency comparison and failure injection on a
# small pool (100 hosts, 1,000 seeded VMs, manager configuration D), each
# followed by an invariant audit. Results: docs/performance/spike-1b/results/extras/.
set -uo pipefail
ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
# shellcheck source=run.sh
source "$ROOT/hack/bench/spike1b/run.sh"
RES="${RES:-$ROOT/docs/performance/spike-1b/results}/extras"
mkdir -p "$RES"
fakecfg() { "$DRIVER" fakeconfig --provider fake-p0 --json "$1" >>"$RES/fakeconfig.log"; }

build_driver
cluster_up
stage_setup 1 100 1000 10 | tee "$RES/setup.txt"
use_config D "$RES"
collector_start "$RES"
sleep 120

# 1. Sustained churn at increasing requested rates (small VMs, delete on Ready).
for rate in 2 5 10 20; do
	churn "$RES" "$rate" 180
	audit "$RES" "churn-$rate"
done

# 2. Provider latency: the same 500-VM burst with Create at 200 ms (default)
#    and at 2 s mean, everything else fixed.
burst "$RES" 500 --timeout 15m
mv "$RES/burst-500.log" "$RES/burst-500-create200ms.log"
fakecfg '{"latency":{"create":{"mean":"2s","jitter":"500ms"}}}'
mark "$RES" latency2s_start
burst "$RES" 500 --timeout 15m
mark "$RES" latency2s_end
mv "$RES/burst-500.log" "$RES/burst-500-create2s.log"
fakecfg '{"latency":{"create":{"mean":"200ms","jitter":"100ms"}}}'
audit "$RES" latency

# 3. Transient failures: 20% of Creates fail before doing anything
#    (HOST_UNAVAILABLE), 20% execute and lose their reply (OUTCOME_UNKNOWN_RETRY).
fakecfg '{"createFailBefore":0.2,"createLostReply":0.2}'
mark "$RES" faults_start
burst "$RES" 300 --timeout 15m
mark "$RES" faults_end
mv "$RES/burst-300.log" "$RES/burst-300-faults.log"
fakecfg '{"createFailBefore":0,"createLostReply":0}'
audit "$RES" faults

# 4. Delete while creating: 300 VMs deleted 3 s after their create, Ready or not.
mark "$RES" delete_while_creating_start
"$DRIVER" burst --run "dwc$(date +%s)" --count 300 --delete-after 3s --timeout 10m --out "$RES" >"$RES/burst-300-delete-while-creating.log" 2>&1
mark "$RES" delete_while_creating_end
sleep 30
audit "$RES" delete-while-creating

# 5. An unreachable host during a burst, then restored.
fakecfg '{"unreachableHosts":["p0-host-0003","p0-host-0004"]}'
mark "$RES" unreachable_start
burst "$RES" 200 --timeout 8m --delete=false
mark "$RES" unreachable_end
audit "$RES" unreachable-during
fakecfg '{"unreachableHosts":[]}'
sleep 180
"$DRIVER" status >"$RES/status-after-restore.txt"
audit "$RES" unreachable-restored
for run in $($K get vm -A -o jsonpath='{range .items[*]}{.metadata.labels.spike1b\/run}{"\n"}{end}' 2>/dev/null | sort -u | grep -v '^seed$'); do
	"$DRIVER" cleanup --run "$run" --wait 10m
done
audit "$RES" after-cleanup
collector_stop
python3 "$ROOT/hack/bench/spike1b/analyze.py" "$RES" >"$RES/analysis.txt" 2>&1
echo "extras done"
