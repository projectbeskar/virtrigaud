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

# file_sectors MOUNT FILE...: the 512-byte sector ranges "start end" (end
# exclusive) that FILEs occupy on the device under MOUNT, sorted and merged.
file_sectors() {
  local mnt=$1 bs; shift
  bs=$(stat -f -c %S "$mnt")
  for f in "$@"; do
    filefrag -e "$f" | awk -F'[:. ]+' -v bs="$bs" '/^ *[0-9]+:/ {
        n = 0; for (i = 1; i <= NF; i++) if ($i != "") f[++n] = $i
        print f[4] * bs / 512, (f[5] + 1) * bs / 512 }'
  done | sort -n | awk 'NR == 1 {s = $1; e = $2; next}
      $1 <= e {if ($2 > e) e = $2; next} {print s, e; s = $1; e = $2} END {if (NR) print s, e}'
}

# table_over SECTORS DEV RANGES TARGET...: a table that maps DEV linearly and
# replaces each "start end" range of RANGES with TARGET (where the literal
# word OFFSET is replaced by the range start).
table_over() {
  local sectors=$1 dev=$2 ranges=$3; shift 3
  local pos=0 s e
  while read -r s e; do
    [ -z "$s" ] && continue
    [ "$s" -gt "$pos" ] && echo "$pos $((s - pos)) linear $dev $pos"
    echo "$s $((e - s)) ${*//OFFSET/$s}"
    pos=$e
  done <<< "$ranges"
  [ "$pos" -lt "$sectors" ] && echo "$pos $((sectors - pos)) linear $dev $pos"
}
