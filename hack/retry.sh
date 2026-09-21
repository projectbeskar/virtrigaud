#!/usr/bin/env bash
#
# hack/retry.sh — run a command, retrying with exponential backoff.
#
# Wraps a single command invocation and re-runs it when it exits non-zero, so
# a transient failure is absorbed while a genuine one still fails the job. In
# CI this shields the network-fetching Go steps (go mod tidy / go mod download
# / go install ...@...) from intermittent proxy.golang.org hiccups such as
#
#   read "https://proxy.golang.org/.../@v/....zip": stream error ...
#   INTERNAL_ERROR; received from peer
#
# which otherwise fail the `test` / `lint` jobs and — because the image
# `build` job `needs: [test, lint]` — silently skip the provider-image push.
#
# It does NOT mask real failures: the command is retried only on a non-zero
# exit, and after the final attempt its own exit code is propagated, so a
# genuine go.mod drift or compile break still fails the job (just later).
#
# Environment overrides:
#   RETRY_ATTEMPTS   total attempts before giving up   (default: 4)
#   RETRY_DELAY      initial backoff, seconds          (default: 8)
#   RETRY_MAX_DELAY  cap on the backoff, seconds       (default: 60)
#
# The backoff doubles after each failed attempt, capped at RETRY_MAX_DELAY.
# Retries are announced on stderr; the wrapped command's own stdout/stderr
# pass through untouched.
#
# Usage:
#   ./hack/retry.sh go mod tidy
#   ./hack/retry.sh go install golang.org/x/vuln/cmd/govulncheck@latest
#   RETRY_ATTEMPTS=6 RETRY_DELAY=5 ./hack/retry.sh go mod download
#
set -u

attempts="${RETRY_ATTEMPTS:-4}"
delay="${RETRY_DELAY:-8}"
max_delay="${RETRY_MAX_DELAY:-60}"

if [ "$#" -eq 0 ]; then
  echo "retry.sh: no command given" >&2
  echo "usage: retry.sh <command> [args...]" >&2
  exit 2
fi

attempt=1
while true; do
  "$@"
  status=$?
  if [ "$status" -eq 0 ]; then
    exit 0
  fi

  if [ "$attempt" -ge "$attempts" ]; then
    echo "retry.sh: '$*' failed after ${attempt} attempt(s) (exit ${status}); giving up" >&2
    exit "$status"
  fi

  echo "retry.sh: '$*' failed (exit ${status}); attempt ${attempt}/${attempts}, retrying in ${delay}s..." >&2
  sleep "$delay"

  attempt=$((attempt + 1))
  delay=$((delay * 2))
  if [ "$delay" -gt "$max_delay" ]; then
    delay="$max_delay"
  fi
done
