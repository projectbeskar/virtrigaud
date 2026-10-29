#!/usr/bin/env bash
# Spike 1A: scheduling performance and contention baseline.
# Report: docs/performance/spike-1a-scheduler-baseline.md
#
# Runs the scheduler-only and controller-level placement benchmarks, the
# contention harness, CPU/memory/block profiles and a max-RSS probe, and writes
# everything to $OUT. Read-only with respect to production code.
#
# Usage:
#   hack/bench/spike1a.sh                 # full run (~1 h on a 10-core box)
#   QUICK=true hack/bench/spike1a.sh      # skip the 100,000-VM scenarios
#   OUT=/tmp/s1a COUNT=3 hack/bench/spike1a.sh
#
# Knobs (environment):
#   OUT                    output directory (default docs/performance/spike-1a/results)
#   COUNT                  -count for the micro-benchmarks (default 5)
#   QUICK=true             sets SCHEDBENCH_SKIP_LARGE=true
#   SCHEDBENCH_SCENARIOS   comma-separated scenario name substrings
#   SCHEDBENCH_DECISIONS   decisions per contention run
#   ENVTEST=true           also compare the seam with a real informer cache
#                          (needs envtest binaries; see make setup-envtest)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

OUT="${OUT:-docs/performance/spike-1a/results}"
COUNT="${COUNT:-5}"
BENCHSTAT_VERSION="v0.0.0-20250813145418-2f7363a06fe1"
if [[ "${QUICK:-false}" == "true" ]]; then
	export SCHEDBENCH_SKIP_LARGE=true
fi
mkdir -p "$OUT/profiles"

benchstat() {
	if type -P benchstat >/dev/null 2>&1; then
		command benchstat "$@"
	else
		go run "golang.org/x/perf/cmd/benchstat@${BENCHSTAT_VERSION}" "$@"
	fi
}

echo "== environment"
{
	echo "date:       $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "commit:     $(git rev-parse HEAD)"
	echo "worktree:   (git status --porcelain)"
	git status --porcelain | sed 's/^/            /'
	echo "go:         $(go version)"
	echo "kernel:     $(uname -srm)"
	echo "os:         $(. /etc/os-release && echo "$PRETTY_NAME")"
	echo "cpu:        $(lscpu | sed -n 's/^Model name:[[:space:]]*//p')"
	echo "cpus:       $(nproc) logical"
	echo "memory:     $(free -g | awk '/^Mem:/ {print $2 " GiB"}')"
	echo "GOMAXPROCS: ${GOMAXPROCS:-default ($(nproc))}"
	echo "GOGC:       ${GOGC:-default (100)}"
	echo "skip-large: ${SCHEDBENCH_SKIP_LARGE:-false}"
} | tee "$OUT/env.txt"

echo "== scheduler-only benchmarks"
go test ./internal/scheduler/ -run '^$' -bench 'BenchmarkSchedule(Phases)?$' \
	-benchmem -count="$COUNT" -timeout 0 | tee "$OUT/scheduler.txt"

echo "== controller-level benchmarks (scan, decision, breakdown)"
go test ./internal/controller/ -run '^$' -bench 'BenchmarkPlacement(Scan|Decision|Breakdown)$' \
	-benchmem -count="$COUNT" -timeout 0 | tee "$OUT/controller.txt"

echo "== decision with one CPU (GOMAXPROCS=1; the chart's manager limit is 500m)"
SCHEDBENCH_SCENARIOS=/base go test ./internal/controller/ -run '^$' -bench 'BenchmarkPlacementDecision$' \
	-benchmem -count="$COUNT" -cpu 1 -timeout 0 | tee "$OUT/controller-cpu1.txt"

echo "== contention harness"
go test ./internal/controller/ -run '^$' -bench 'BenchmarkPlacementContention$' \
	-benchtime=1x -count=1 -timeout 0 | tee "$OUT/contention.txt"

echo "== correctness tests"
go test ./internal/controller/ -run 'TestSchedBench' -count=1 -race -v 2>&1 \
	| grep -E '^(=== RUN|--- |PASS|FAIL|ok)' | tee "$OUT/correctness.txt"

echo "== profiles"
LARGE="h1000_v100k"
if [[ "${SCHEDBENCH_SKIP_LARGE:-false}" == "true" ]]; then
	LARGE="h100_v10k"
fi
P="$OUT/profiles"
SCHEDBENCH_SCENARIOS="$LARGE/base" go test ./internal/controller/ -run '^$' -bench 'BenchmarkPlacementDecision$' \
	-benchtime=20x -cpuprofile "$P/decision.cpu.pb.gz" -memprofile "$P/decision.mem.pb.gz" -o "$P/controller.test" >/dev/null
SCHEDBENCH_SCENARIOS="$LARGE/affinity" go test ./internal/scheduler/ -run '^$' -bench 'BenchmarkSchedule$' \
	-benchtime=20x -cpuprofile "$P/affinity.cpu.pb.gz" -memprofile "$P/affinity.mem.pb.gz" -o "$P/scheduler.test" >/dev/null
SCHEDBENCH_SCENARIOS="$LARGE" go test ./internal/controller/ -run '^$' \
	-bench "BenchmarkPlacementContention$/$LARGE/providers1/delay0s/w16$" -benchtime=1x \
	-blockprofile "$P/contention.block.pb.gz" -cpuprofile "$P/contention.cpu.pb.gz" -o "$P/controller-c.test" >/dev/null
for prof in decision.cpu decision.mem; do
	go tool pprof -top -nodecount=40 "$P/controller.test" "$P/$prof.pb.gz" >"$P/$prof.top.txt" 2>/dev/null
done
go tool pprof -top -sample_index=alloc_space -nodecount=40 "$P/controller.test" "$P/decision.mem.pb.gz" >"$P/decision.alloc_space.top.txt" 2>/dev/null
go tool pprof -top -cum -nodecount=40 "$P/controller.test" "$P/decision.cpu.pb.gz" >"$P/decision.cpu.cum.txt" 2>/dev/null
go tool pprof -top -nodecount=40 "$P/scheduler.test" "$P/affinity.cpu.pb.gz" >"$P/affinity.cpu.top.txt" 2>/dev/null
go tool pprof -top -nodecount=30 "$P/controller-c.test" "$P/contention.block.pb.gz" >"$P/contention.block.top.txt" 2>/dev/null
go tool pprof -top -nodecount=30 "$P/controller-c.test" "$P/contention.cpu.pb.gz" >"$P/contention.cpu.top.txt" 2>/dev/null
rm -f "$P"/*.test

echo "== max RSS of one scan benchmark process ($LARGE)"
go test -c -o "$OUT/controller.test" ./internal/controller/
SCHEDBENCH_SCENARIOS="$LARGE/base" /usr/bin/time -v "$OUT/controller.test" -test.run '^$' \
	-test.bench 'BenchmarkPlacementScan$' -test.benchtime=5x 2>&1 \
	| grep -E 'Benchmark|Maximum resident' | tee "$OUT/maxrss.txt"
rm -f "$OUT/controller.test"

if [[ "${ENVTEST:-false}" == "true" ]]; then
	echo "== seam fidelity against a real informer cache (envtest)"
	for n in 10000 100000; do
		SCHEDBENCH_ENVTEST_VMS=$n go test -tags schedbench_envtest ./internal/controller/ -run '^$' \
			-bench BenchmarkEnvtestScan -benchtime 10x -benchmem -count 3 -timeout 0 2>&1 \
			| grep -E 'Benchmark|loaded|FAIL|envtest_test'
	done | tee "$OUT/envtest-fidelity.txt"
fi

echo "== summaries"
benchstat "$OUT/scheduler.txt" >"$OUT/scheduler.benchstat.txt"
benchstat "$OUT/controller.txt" >"$OUT/controller.benchstat.txt"
benchstat "$OUT/controller-cpu1.txt" >"$OUT/controller-cpu1.benchstat.txt"
echo "done: $OUT"
