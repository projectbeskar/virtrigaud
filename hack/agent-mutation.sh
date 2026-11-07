#!/usr/bin/env bash
# agent-mutation.sh — mutation test of the per-host agent's operation store.
#
# Every safety guard of internal/agent (listed in
# internal/agent/internal/guard/guard.go) can be switched off in a test binary
# built with the virtrigaud_mutation build tag, by naming it in the
# VIRTRIGAUD_MUTATION environment variable. This script first checks that the
# tests pass with no guard off, then runs them once per guard and expects at
# least one test to fail. A guard whose removal no test notices is a
# surviving mutant: the script prints it and exits 1.
#
# The reference-model test runs with VIRTRIGAUD_MODEL_RUNS (default here 3000)
# so that one round stays short.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

PKGS="./internal/agent/..."
TAGS="virtrigaud_mutation"
export VIRTRIGAUD_MODEL_RUNS="${VIRTRIGAUD_MODEL_RUNS:-3000}"

guards="$(sed -n 's/^[[:space:]]*[A-Za-z]* Name = "\([a-z-]*\)"$/\1/p' internal/agent/internal/guard/guard.go)"
if [ -z "${guards}" ]; then
	echo "agent-mutation: no guards found" >&2
	exit 1
fi

echo "agent-mutation: baseline (no guard off)"
if ! VIRTRIGAUD_MUTATION="" go test -count=1 -tags "${TAGS}" ${PKGS} >/dev/null; then
	echo "agent-mutation: the tests fail with no guard off" >&2
	exit 1
fi

survivors=()
total=0
for g in ${guards}; do
	total=$((total + 1))
	if VIRTRIGAUD_MUTATION="${g}" go test -count=1 -tags "${TAGS}" ${PKGS} >/dev/null 2>&1; then
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
