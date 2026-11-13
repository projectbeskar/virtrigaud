#!/usr/bin/env bash
# verify-no-binaries.sh — fail if any tracked file is a native executable, a
# static archive or a WebAssembly module.
#
# A binary built in place (go build inside a cmd directory) must never be
# committed. Each tracked regular file is checked by its magic bytes: ELF,
# Mach-O (32/64-bit, either byte order, and fat), PE ("MZ"), "!<arch>" static
# archives and WebAssembly ("\0asm"). A legitimate fixture is listed, one
# explicit repository-relative path per line, in hack/no-binaries-allowlist.
# Needs git, head and od; nothing is installed.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

ALLOWLIST="hack/no-binaries-allowlist"
declare -A allowed=()
if [ -f "${ALLOWLIST}" ]; then
	while IFS= read -r line; do
		case "${line}" in '' | '#'*) continue ;; esac
		allowed["${line}"]=1
	done <"${ALLOWLIST}"
fi

found=0
while IFS= read -r -d '' f; do
	[ -f "${f}" ] && [ ! -L "${f}" ] || continue
	magic="$(head -c 8 -- "${f}" | od -An -tx1 | tr -d ' \n')"
	kind=""
	case "${magic}" in
	7f454c46*) kind="ELF" ;;
	feedface* | feedfacf* | cefaedfe* | cffaedfe* | cafebabe*) kind="Mach-O or Java class" ;;
	4d5a*) kind="PE" ;;
	213c617263683e0a) kind="static archive" ;;
	0061736d*) kind="WebAssembly" ;;
	esac
	if [ -n "${kind}" ] && [ -z "${allowed[${f}]:-}" ]; then
		echo "verify-no-binaries: tracked ${kind} file: ${f}" >&2
		found=1
	fi
done < <(git ls-files -z)

if [ "${found}" -ne 0 ]; then
	exit 1
fi
echo "OK: no tracked executables, archives or modules."
