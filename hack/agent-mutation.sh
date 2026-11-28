#!/usr/bin/env bash
# agent-mutation.sh — mutation test of the per-host agent's operation store.
#
# Every safety guard of internal/agent (listed in
# internal/agent/internal/guard/guard.go) can be switched off in a test binary
# built with the virtrigaud_mutation build tag, by naming it in the
# VIRTRIGAUD_MUTATION environment variable. This script first checks that the
# tests pass with no guard off, then runs them once per guard and expects at
# least one test to fail. A guard whose removal no test notices is a
# surviving mutant: the script prints it and exits 1. Only a behavioural test
# counts: TestMutationGuardIsKnown, which refuses a guard missing from
# guard.All(), is skipped in the rounds and must pass for every guard in a
# preflight, so a guard missing from All() stops the script instead of being
# counted as killed.
#
# Each round runs the tests twice: as a release build, and as a development
# build (virtrigaud_dev), whose operation path (runners, the fake executor)
# only a development build serves. A guard is killed when either fails.
#
# The reference-model test runs with VIRTRIGAUD_MODEL_RUNS (default here 3000)
# so that one round stays short.
#
# The manager has guards of its own (internal/controller/mutation_guard.go,
# the checks of a retire-if-unknown); each is switched off the same way and
# must make a manager test fail. They run on ./internal/controller as a
# development build (the host ledger exists only there), filtered to the
# retirement tests that cover them.
#
# MUTATION_GUARDS, when set, limits a run to the guards it names
# (space-separated, agent or manager); the default is every guard.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

PKGS="./internal/agent/..."
TAGS="virtrigaud_mutation"
DEV_TAGS="virtrigaud_mutation,virtrigaud_dev"

# KNOWN_TEST refuses a VIRTRIGAUD_MUTATION value that names no guard. It is
# not a behavioural test: its failure says the guard is missing from
# guard.All(), not that a test noticed the guard's removal. The rounds skip it,
# and the preflight below requires it to pass for every guard, so that a guard
# missing from All() is an error rather than a mutant counted as killed.
KNOWN_TEST='^TestMutationGuardIsKnown$'

# run_tests runs the agent tests in both builds, without KNOWN_TEST; it fails
# when either fails.
run_tests() {
	go test -count=1 -tags "${TAGS}" -skip "${KNOWN_TEST}" ${PKGS} &&
		go test -count=1 -tags "${DEV_TAGS}" -skip "${KNOWN_TEST}" ${PKGS}
}
export VIRTRIGAUD_MODEL_RUNS="${VIRTRIGAUD_MODEL_RUNS:-3000}"

# The manager's guards and the tests that cover them.
MANAGER_PKG="./internal/controller"
MANAGER_TAGS="virtrigaud_dev,virtrigaud_mutation"
MANAGER_RUN='^(TestAgentIdentity_Retire.*|TestMutationGuardsListed)$'

# run_manager_tests runs the manager's retirement tests.
run_manager_tests() {
	go test -count=1 -tags "${MANAGER_TAGS}" -run "${MANAGER_RUN}" "${MANAGER_PKG}"
}

# selected reports whether guard $1 is in MUTATION_GUARDS (or that is unset).
selected() {
	[ -z "${MUTATION_GUARDS:-}" ] && return 0
	case " ${MUTATION_GUARDS} " in *" $1 "*) return 0 ;; esac
	return 1
}

guards="$(sed -n 's/^[[:space:]]*[A-Za-z]* Name = "\([a-z-]*\)"$/\1/p' internal/agent/internal/guard/guard.go)"
if [ -z "${guards}" ]; then
	echo "agent-mutation: no guards found" >&2
	exit 1
fi
manager_guards="$(sed -n 's/^[[:space:]]*[A-Za-z]* mutationGuard = "\([a-z-]*\)"$/\1/p' internal/controller/mutation_guard.go)"
if [ -z "${manager_guards}" ]; then
	echo "agent-mutation: no manager guards found" >&2
	exit 1
fi

echo "agent-mutation: preflight (every guard is in guard.All())"
for g in ${guards}; do
	if ! VIRTRIGAUD_MUTATION="${g}" go test -count=1 -tags "${TAGS}" -run "${KNOWN_TEST}" ./internal/agent/opstore/ >/dev/null 2>&1; then
		echo "agent-mutation: guard ${g} is missing from guard.All()" >&2
		exit 1
	fi
done

echo "agent-mutation: baseline (no guard off)"
if ! VIRTRIGAUD_MUTATION="" run_tests >/dev/null; then
	echo "agent-mutation: the tests fail with no guard off" >&2
	exit 1
fi
if ! VIRTRIGAUD_MUTATION="" run_manager_tests >/dev/null; then
	echo "agent-mutation: the manager tests fail with no guard off" >&2
	exit 1
fi

survivors=()
total=0
for g in ${manager_guards}; do
	selected "${g}" || continue
	total=$((total + 1))
	if VIRTRIGAUD_MUTATION="${g}" run_manager_tests >/dev/null 2>&1; then
		echo "SURVIVED  ${g} (manager)"
		survivors+=("${g}")
	else
		echo "killed    ${g} (manager)"
	fi
done
for g in ${guards}; do
	selected "${g}" || continue
	total=$((total + 1))
	if VIRTRIGAUD_MUTATION="${g}" run_tests >/dev/null 2>&1; then
		echo "SURVIVED  ${g}"
		survivors+=("${g}")
	else
		echo "killed    ${g}"
	fi
done

echo "agent-mutation: ${total} guards, ${#survivors[@]} surviving"
if [ "${#survivors[@]}" -gt 0 ]; then
	exit 1
fi
