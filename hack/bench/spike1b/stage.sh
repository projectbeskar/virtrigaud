#!/usr/bin/env bash
# Spike 1B: run one stage end to end.
#
#   stage.sh NAME PROVIDERS HOSTS SEED NAMESPACES "CONFIGS" "BURSTS" [WARMUP_S] [STEADY_S]
#   e.g. stage.sh A 1 100 1000 10 "A B C D" "100 1000"
#
# Recreates the kind cluster, seeds the stage once, then for each manager
# configuration: switch config (records cache initialisation), warm up, steady
# window, bursts (each followed by its deletes), audit. Results:
# docs/performance/spike-1b/results/stage-NAME/<config>/ (+ analysis.json).
set -uo pipefail
ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
# shellcheck source=run.sh
source "$ROOT/hack/bench/spike1b/run.sh"
NAME="$1" PROVIDERS="$2" HOSTS="$3" SEED="$4" NSS="$5" CONFIGS="$6" BURSTS="$7"
WARMUP="${8:-180}" STEADY="${9:-240}"
RES="${RES:-$ROOT/docs/performance/spike-1b/results}/stage-$NAME"
mkdir -p "$RES"

build_driver
if [[ "${REUSE_CLUSTER:-false}" != "true" ]]; then
	cluster_up
	t0=$SECONDS
	stage_setup "$PROVIDERS" "$HOSTS" "$SEED" "$NSS" | tee "$RES/setup.txt"
	echo "setup+populate: $((SECONDS - t0)) s" | tee -a "$RES/setup.txt"
fi

for c in $CONFIGS; do
	dir="$RES/$c"
	mkdir -p "$dir"
	echo "== stage $NAME config $c"
	use_config "$c" "$dir"
	collector_start "$dir"
	mark "$dir" warmup_start
	sleep "$WARMUP"
	mark "$dir" warmup_end
	if [[ "${PPROF:-false}" == "true" ]]; then
		pprof "$dir" steady 30
	fi
	steady "$dir" "$STEADY"
	for n in $BURSTS; do
		burst "$dir" "$n" --timeout "${BURST_TIMEOUT:-20m}"
		sleep 30
	done
	audit "$dir" final
	collector_stop
	python3 "$ROOT/hack/bench/spike1b/analyze.py" "$dir" >"$dir/analysis.txt" 2>&1
	echo "stage $NAME config $c done"
done
