#!/usr/bin/env bash
# verify-systemd-units.sh — lint the reference systemd units of the per-host
# agent (packaging/systemd/*.service) with `systemd-analyze verify`.
#
# The units start a binary under /usr/libexec that does not exist on a build
# machine, so each unit is copied to a temporary directory with that path
# replaced by /usr/bin/true before it is verified. Any message from
# systemd-analyze fails the check. Needs systemd-analyze (any Linux runner
# with systemd); no root, nothing is installed.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UNIT_DIR="${REPO_ROOT}/packaging/systemd"
BINARY_PATH="/usr/libexec/virtrigaud/"

if ! command -v systemd-analyze >/dev/null 2>&1; then
	echo "verify-systemd-units: systemd-analyze not found" >&2
	exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

units=()
for f in "${UNIT_DIR}"/*.service; do
	name="$(basename "${f}")"
	sed -E "s#${BINARY_PATH}[A-Za-z0-9_.-]+#/usr/bin/true#g" "${f}" >"${tmp}/${name}"
	units+=("${tmp}/${name}")
done

# --recursive-errors=no (systemd 250 and later) keeps the exit status to the
# units given here; older systemd-analyze lacks the option. It still loads the units they reference (targets, dbus) from
# the build machine and prints that machine's own warnings about them, so
# only messages that name one of our units count.
names_re="$(printf '%s\n' "${units[@]##*/}" | sed 's/[.]/[.]/g' | paste -sd'|' -)"
extra=()
if systemd-analyze --help 2>/dev/null | grep -q -- '--recursive-errors'; then
	extra+=(--recursive-errors=no)
fi
raw="$(systemd-analyze verify --man=no "${extra[@]}" "${units[@]}" 2>&1)" || {
	echo "${raw}" >&2
	echo "verify-systemd-units: systemd-analyze verify failed" >&2
	exit 1
}
out="$(printf '%s\n' "${raw}" | grep -E "${names_re}" || true)"
if [ -n "${out}" ]; then
	echo "${out}" >&2
	echo "verify-systemd-units: systemd-analyze reported the messages above" >&2
	exit 1
fi
echo "OK: ${#units[@]} systemd units verified."
