#!/usr/bin/env bash
# verify-golangci-lint-version.sh — CI guard: every place that pins golangci-lint
# must pin the same version, and .golangci.yml must be a v2 configuration.
#
# The Makefile (GOLANGCI_LINT_VERSION) is the reference; the CI workflow, the
# local helper scripts and .pre-commit-config.yaml must agree with it. The
# pre-commit hook used to pin v1.59 while the config is v2, which v1 cannot
# parse.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

want="$(sed -n 's/^GOLANGCI_LINT_VERSION ?= \(v[0-9][0-9.]*\)$/\1/p' Makefile)"
if [ -z "${want}" ]; then
  echo "ERROR: GOLANGCI_LINT_VERSION not found in Makefile." >&2
  exit 1
fi

status=0
# check <file> <sed -n expression printing the pinned version>
check() {
  local file="$1" expr="$2" got
  got="$(sed -n "${expr}" "${file}" | head -1)"
  if [ "${got}" != "${want}" ]; then
    echo "ERROR: ${file} pins golangci-lint '${got:-<none>}', Makefile pins '${want}'." >&2
    status=1
  fi
}

check "${REPO_ROOT}/.github/workflows/ci.yml" 's/^ *version: \(v2[0-9.]*\)$/\1/p'
check hack/test-lint-locally.sh 's/^GOLANGCI_LINT_VERSION="\(v[0-9.]*\)"$/\1/p'
check hack/test-workflows-locally.sh 's/^GOLANGCI_LINT_VERSION=\(v[0-9.]*\)$/\1/p'

# .pre-commit-config.yaml: the rev of the golangci-lint repository.
got="$(awk '/repo: https:\/\/github.com\/golangci\/golangci-lint$/ {getline; sub(/^ *rev: */, ""); print; exit}' .pre-commit-config.yaml)"
if [ "${got}" != "${want}" ]; then
  echo "ERROR: .pre-commit-config.yaml pins golangci-lint '${got:-<none>}', Makefile pins '${want}'." >&2
  status=1
fi

if ! grep -q '^version: "2"$' .golangci.yml; then
  echo "ERROR: .golangci.yml is not a golangci-lint v2 configuration (version: \"2\")." >&2
  status=1
fi

if [ "${status}" -eq 0 ]; then
  echo "golangci-lint is pinned to ${want} everywhere."
fi
exit "${status}"
