#!/usr/bin/env bash
# Shared helpers for the journal durability scripts. Run inside a disposable
# test VM as root; never on a shared host.
set -euo pipefail

JT=${JT:-/opt/jt/journal-torture}
REPLAY_LOG=${REPLAY_LOG:-/opt/lw/replay-log}

# mkfs_dev FSTYPE DEV: make a file system without discards or lazy init, so
# that the whole format is in the write log and no background writer runs.
mkfs_dev() {
  case "$1" in
    ext4) mkfs.ext4 -q -F -E nodiscard,lazy_itable_init=0,lazy_journal_init=0 "$2" ;;
    xfs) mkfs.xfs -q -f -K "$2" ;;
    *) echo "unsupported fs $1" >&2; return 1 ;;
  esac
}

# loopdev FILE SIZE: a loop device over a new sparse file.
loopdev() {
  rm -f "$1"; truncate -s "$2" "$1"
  losetup -f --show "$1"
}

# verdict_counts FILE: count verify verdicts in a JSON-lines file.
verdict_counts() {
  grep -o '"verdict":"[a-z_]*"' "$1" | sort | uniq -c
}
