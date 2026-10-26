#!/usr/bin/env bash
# Spike 1B orchestration helpers. Source this file, then call the functions,
# or run "run.sh <function> args..." (see docs/performance/spike-1b-control-plane-baseline.md).
#
#   cluster_up                        (re)create the kind cluster, load images, install the chart (config A)
#   stage_setup P H V NS              Providers, Hosts, seeded VMs (manager scaled to 0 while seeding)
#   use_config X                      helm upgrade to values-X.yaml; record cache initialisation
#   collector_start DIR / collector_stop
#   mark DIR LABEL                    append "LABEL <unix-seconds>" to DIR/markers.txt
#   steady DIR SECONDS                steady-state window (markers steady_start/steady_end)
#   burst DIR N [extra driver flags]  N concurrent creates, then deletes
#   churn DIR RATE SECONDS            churn at RATE creates/s
#   audit DIR LABEL                   invariant audit
#   pprof DIR LABEL SECONDS           manager CPU and heap profiles
set -uo pipefail
ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
CTX="${CTX:-kind-virtrigaud-spike1b}"
CLUSTER="${CLUSTER:-virtrigaud-spike1b}"
K="kubectl --context $CTX"
H="helm --kube-context $CTX"
DRIVER="${DRIVER:-$ROOT/bin/spike1b-driver}"
NS=virtrigaud-system
COLLECTOR_PID=""

build_driver() { (cd "$ROOT" && go build -o "$DRIVER" ./test/spike1b/driver); }

cluster_up() {
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	local prev
	prev="$(kubectl config current-context 2>/dev/null || true)"
	kind create cluster --config "$ROOT/hack/bench/spike1b/kind-config.yaml" >/dev/null
	# kind switches the current context; give it back.
	[[ -n "$prev" ]] && kubectl config use-context "$prev" >/dev/null
	$K wait --for=condition=Ready node --all --timeout=180s >/dev/null
	# shellcheck disable=SC2086
	kind load docker-image --name "$CLUSTER" ${MANAGER_IMAGES:-spike1b.local/manager:exp} spike1b.local/fakeprovider:dev >/dev/null
	(cd "$ROOT" && make -s gen-helm-crds >/dev/null)
	$K apply --server-side -f "$ROOT/charts/virtrigaud/crds/" >/dev/null
	$H install virtrigaud "$ROOT/charts/virtrigaud" -n "$NS" --create-namespace \
		-f "$ROOT/hack/bench/spike1b/values-A.yaml" --wait --timeout 5m >/dev/null
	echo "cluster up, manager on config A"
}

scale_manager() {
	$K -n "$NS" scale deploy/virtrigaud-manager --replicas="$1" >/dev/null
	if [[ "$1" == 0 ]]; then
		$K -n "$NS" wait --for=delete pod -l app.kubernetes.io/name=virtrigaud --timeout=180s >/dev/null 2>&1 || true
	else
		$K -n "$NS" rollout status deploy/virtrigaud-manager --timeout=600s >/dev/null
	fi
}

stage_setup() {
	local p="$1" h="$2" v="$3" ns="$4"
	"$DRIVER" setup --providers "$p" --hosts "$h" --seed "$v" --namespaces "$ns"
	# Wait for the fake providers to run and the Hosts to report Ready.
	sleep 20
	$K -n spike-infra rollout status deploy --timeout=300s >/dev/null 2>&1 || true
	scale_manager 0
	local t0=$SECONDS
	"$DRIVER" populate
	echo "populate: $((SECONDS - t0)) s"
	scale_manager 1
}

# use_config X DIR: switch the manager to configuration X and record how long
# the new pod took to start its VirtualMachine workers (cache sync included).
use_config() {
	local x="$1" dir="$2"
	mkdir -p "$dir"
	$H upgrade virtrigaud "$ROOT/charts/virtrigaud" -n "$NS" -f "$ROOT/hack/bench/spike1b/values-$x.yaml" >/dev/null
	$K -n "$NS" rollout restart deploy/virtrigaud-manager >/dev/null
	$K -n "$NS" rollout status deploy/virtrigaud-manager --timeout=900s >/dev/null
	local pod
	pod="$($K -n "$NS" get pod -l app.kubernetes.io/name=virtrigaud --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')"
	for _ in $(seq 1 120); do
		$K -n "$NS" logs "$pod" 2>/dev/null | grep -q '"controller": "virtualmachine".*Starting workers\|Starting workers.*"controller": "virtualmachine"' && break
		sleep 2
	done
	{
		echo "config $x pod $pod"
		$K -n "$NS" get pod "$pod" -o jsonpath='{"started "}{.status.startTime}{"\n"}'
		$K -n "$NS" logs "$pod" 2>/dev/null | grep -E 'Starting EventSource|Starting Controller|Starting workers' | grep -E '"controller": "(virtualmachine|host)"' | head -6
	} >"$dir/cache-init.txt"
	echo "config $x active"
}

collector_start() {
	OUT="$1/metrics" INTERVAL="${INTERVAL:-15}" "$ROOT/hack/bench/spike1b/collect.sh" >/dev/null 2>&1 &
	COLLECTOR_PID=$!
}

collector_stop() { [[ -n "$COLLECTOR_PID" ]] && kill "$COLLECTOR_PID" 2>/dev/null; COLLECTOR_PID=""; }

mark() { echo "$2 $(date +%s)" >>"$1/markers.txt"; }

steady() {
	mark "$1" steady_start
	"$DRIVER" hosts --interval 15s --duration "${2}s" --out "$1/hosts-steady.csv" >/dev/null
	mark "$1" steady_end
	"$DRIVER" status --json >"$1/status-after-steady.json"
}

burst() {
	local dir="$1" n="$2"
	shift 2
	local run="b${n}x$(date +%s)"
	mark "$dir" "burst_${n}_start"
	"$DRIVER" burst --run "$run" --count "$n" --out "$dir" "$@" >"$dir/burst-$n.log" 2>&1
	mark "$dir" "burst_${n}_end"
	tail -n 3 "$dir/burst-$n.log" | head -n 1 >/dev/null
	echo "burst $n done: $(grep -o '"ready": [0-9]*' "$dir/$run-summary.json" | head -1)"
}

churn() {
	local dir="$1" rate="$2" secs="$3"
	local run="c${rate//./p}x$(date +%s)"
	mark "$dir" "churn_${rate}_start"
	"$DRIVER" churn --run "$run" --rate "$rate" --duration "${secs}s" --recovery 10m --out "$dir" >"$dir/churn-$rate.log" 2>&1
	mark "$dir" "churn_${rate}_end"
	echo "churn $rate done"
}

audit() { "$DRIVER" audit --out "$1/audit-$2.json" >/dev/null && grep -A8 '"violations"' "$1/audit-$2.json" | head -9; }

pprof() {
	local dir="$1" label="$2" secs="${3:-30}"
	local pod
	pod="$($K -n "$NS" get pod -l app.kubernetes.io/name=virtrigaud --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')"
	$K -n "$NS" port-forward "$pod" 16060:6060 >/dev/null 2>&1 &
	local pf=$!
	sleep 2
	mkdir -p "$dir/profiles"
	curl -s "http://127.0.0.1:16060/debug/pprof/profile?seconds=$secs" -o "$dir/profiles/$label.cpu.pb.gz"
	curl -s "http://127.0.0.1:16060/debug/pprof/heap" -o "$dir/profiles/$label.heap.pb.gz"
	kill "$pf" 2>/dev/null
}

if [[ "${BASH_SOURCE[0]}" == "$0" && $# -gt 0 ]]; then
	"$@"
fi
