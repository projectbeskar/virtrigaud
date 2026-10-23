#!/usr/bin/env bash
# verify-no-lab-names.sh — CI guard: tracked files must not name internal lab
# hosts or addresses. The project is used in regulated environments and must
# not publish internal lab topology.
#
# Use documentation-reserved values instead: RFC 5737 addresses (192.0.2.0/24,
# 198.51.100.0/24, 203.0.113.0/24), RFC 1918 generic addresses where a test
# needs a private one (10.0.0.x, 172.20.x.x, 192.168.x.x), and RFC 2606 / 6761
# names (example.com, example.internal, example.test).
#
# Scans `git ls-files`, excluding fieldTesting/ (a local scratch workspace) and
# this script (which holds the patterns). Exits 1 and prints each hit.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# Lab IP range, lab DNS suffixes, and lab host names.
PATTERN='172\.16\.56\.|\.lab\.k8|labk8s\.io|\bdome[23]?\b|\bvr1\b|\bpve\.lab|esxi\.lab|rustfs\.lab|\bomv\.lab'

if git grep -nIE "${PATTERN}" -- . ':!fieldTesting' ':!hack/verify-no-lab-names.sh'; then
  echo >&2
  echo "ERROR: internal lab hostnames/addresses found in tracked files (above)." >&2
  echo "Replace them with documentation-reserved values (see the header of hack/verify-no-lab-names.sh)." >&2
  exit 1
fi
echo "OK: no internal lab names found in tracked files."
