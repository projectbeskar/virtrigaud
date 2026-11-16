#!/usr/bin/env bash
# verify-systemd-units.sh — lint the reference systemd units of the per-host
# agent (packaging/systemd/*.service) with `systemd-analyze verify`, then
# check properties the analyzer cannot see:
#
#  - a unit with a system call filter sets SystemCallErrorNumber=, so a
#    filtered call fails instead of killing the process (the agent's host
#    inventory probes a call the filter denies on some kernels);
#  - no binary is installed under /usr/libexec (not merged from a system
#    extension on some immutable hosts);
#  - the signer can write its key directory (a certificate renewal creates
#    the pending key there) and nothing else besides its socket directory;
#  - the multi-user.target drop-in upholds the shutdown unit and the
#    once-per-boot start unit, and never the agent itself;
#  - the agent, the signer and the shutdown unit audit filtered privileged
#    calls and require the setup unit (which refuses moved user ids).
#
# The units start a binary under /usr/lib/virtrigaud that does not exist on a
# build machine, so each unit is copied to a temporary directory with that
# path replaced by /usr/bin/true before it is verified. Any message from
# systemd-analyze fails the check. Needs systemd-analyze (any Linux runner
# with systemd); no root, nothing is installed.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UNIT_DIR="${REPO_ROOT}/packaging/systemd"
BINARY_PATH="/usr/lib/virtrigaud/"
UPHOLDS_DROPIN="${UNIT_DIR}/multi-user.target.d/virtrigaud-agent.conf"

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

fail=0
problem() {
	echo "verify-systemd-units: $*" >&2
	fail=1
}
# value FILE KEY prints the values of KEY= in FILE, one per line.
value() {
	sed -nE "s/^[[:space:]]*$2=(.*)$/\1/p" "$1"
}
for f in "${UNIT_DIR}"/*.service; do
	name="$(basename "${f}")"
	if grep -qE '^[[:space:]]*Exec[A-Za-z]*=.*/usr/libexec/' "${f}"; then
		problem "${name}: a binary under /usr/libexec (use /usr/lib/virtrigaud)"
	fi
	if [ -n "$(value "${f}" SystemCallFilter)" ]; then
		errno="$(value "${f}" SystemCallErrorNumber | tail -n 1)"
		case "${errno}" in
		"" | kill | KILL)
			problem "${name}: SystemCallFilter= without SystemCallErrorNumber=: a filtered call kills the process"
			;;
		esac
	fi
done
for name in vrtg-agent.service vrtg-agent-signer.service vrtg-vm-shutdown.service; do
	if [ "$(value "${UNIT_DIR}/${name}" SystemCallLog)" != "@privileged" ]; then
		problem "${name}: SystemCallLog=@privileged missing: filtered privileged calls would not be audited"
	fi
	if ! value "${UNIT_DIR}/${name}" Requires | tr ' ' '\n' | grep -qx vrtg-agent-setup.service; then
		problem "${name}: does not require vrtg-agent-setup.service: it would start when its users' ids moved"
	fi
done
signer="${UNIT_DIR}/vrtg-agent-signer.service"
rw="$(value "${signer}" ReadWritePaths | tr ' ' '\n' | sed '/^$/d' | sort | paste -sd' ' -)"
if [ "${rw}" != "/run/virtrigaud-signer /var/lib/virtrigaud-signer" ]; then
	problem "vrtg-agent-signer.service: ReadWritePaths must be exactly its key directory and socket directory (got: ${rw})"
fi
if value "${signer}" ReadOnlyPaths | grep -q virtrigaud-signer; then
	problem "vrtg-agent-signer.service: its key directory is read-only: a renewal cannot write the pending key"
fi
if [ ! -f "${UPHOLDS_DROPIN}" ]; then
	problem "missing ${UPHOLDS_DROPIN#"${REPO_ROOT}/"}"
else
	upheld=" $(value "${UPHOLDS_DROPIN}" Upholds | tr '\n' ' ') "
	for want in vrtg-vm-shutdown.service vrtg-agent-boot-start.service; do
		case "${upheld}" in
		*" ${want} "*) ;;
		*) problem "multi-user.target drop-in does not uphold ${want}" ;;
		esac
	done
	case "${upheld}" in
	*" vrtg-agent.service "* | *" vrtg-agent-signer.service "*)
		problem "multi-user.target drop-in upholds the agent or the signer: a maintenance stop would be undone"
		;;
	esac
fi
if [ "${fail}" -ne 0 ]; then
	exit 1
fi
echo "OK: ${#units[@]} systemd units verified."
