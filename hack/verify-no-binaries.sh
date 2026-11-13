#!/usr/bin/env bash
# verify-no-binaries.sh — fail if any tracked file is a native executable.
#
# A binary built in place (go build inside a cmd directory) must never be
# committed. Each tracked regular file is checked by its magic bytes: ELF,
# Mach-O (32/64-bit, either byte order, and fat) and PE ("MZ"). Needs git and
# od; nothing is installed.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

found=0
while IFS= read -r -d '' f; do
	[ -f "${f}" ] && [ ! -L "${f}" ] || continue
	magic="$(head -c 4 -- "${f}" | od -An -tx1 | tr -d ' \n')"
	case "${magic}" in
	7f454c46 | feedface | feedfacf | cefaedfe | cffaedfe | cafebabe | 4d5a*)
		# cafebabe is also a Java class file: those are not tracked here, and a
		# fat Mach-O is the case that matters.
		echo "verify-no-binaries: tracked executable: ${f}" >&2
		found=1
		;;
	esac
done < <(git ls-files -z)

if [ "${found}" -ne 0 ]; then
	exit 1
fi
echo "OK: no tracked native executables."
