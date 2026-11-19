#!/usr/bin/env bash
# Runs go vet and the tests of a development build (the virtrigaud_dev build
# tag) with the race detector. The per-host agent, its enrolment and the
# clustered Cloud Hypervisor paths exist only in that build, so `make test`
# never runs their tests.
#
# This script only vets and tests; it builds no artefact. The build tag is named
# here, not in the Makefile or a workflow, because
# internal/devgate.TestNoReleaseBuildUsesTheDevTag refuses it there, so that no
# release build can compile the development gate in.
#
# Needs KUBEBUILDER_ASSETS (exported by `make test-dev`) and helm: the chart
# render tests skip without helm.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

TAG=virtrigaud_dev

if [[ -z "${KUBEBUILDER_ASSETS:-}" ]]; then
  echo "test-dev: KUBEBUILDER_ASSETS is not set; run make test-dev" >&2
  exit 1
fi
if ! command -v helm >/dev/null 2>&1; then
  echo "test-dev: helm is not installed; the chart render tests would skip" >&2
  exit 1
fi

echo "test-dev: go vet (development build)"
go list -tags "${TAG}" ./... | grep -v '/test/integration' | xargs go vet -tags "${TAG}"

echo "test-dev: go test (development build, race detector)"
go list -tags "${TAG}" ./... | grep -v '/test/e2e' | grep -v '/test/integration' | \
  xargs go test -tags "${TAG}" -race -count=1 -timeout 30m
