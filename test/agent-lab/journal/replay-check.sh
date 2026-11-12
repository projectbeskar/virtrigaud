#!/usr/bin/env bash
# Check hook for replay-log --check 1: verifies a snapshot of the replay
# device after the entry just replayed. Always exits 0 so that the replay goes
# on; the verdict is recorded in results.jsonl.
set -uo pipefail
W=$1
. "$W/env"
ENTRY=$(( $(cat "$W/counter") + 1 )); echo "$ENTRY" > "$W/counter"
# Acknowledged ops: the highest "ack:<i>" mark at or before this entry.
ACKED=$(awk -v e="$ENTRY" '$1 !~ /^#/ && $1 <= e && $2 ~ /^ack:/ {sub("ack:","",$2); if ($2+0 > m) m = $2+0} END {print m+0}' "$W/marks.txt")
dmsetup create jt-snap --table "0 $SECTORS snapshot $REPLAY $COW N 8" || { echo "{\"entry\":$ENTRY,\"acked\":$ACKED,\"verdict\":\"harness_error\"}" >> "$W/results.jsonl"; exit 0; }
if ! mount /dev/mapper/jt-snap "$W/snapmnt" 2>"$W/mount.err"; then
  # No mountable file system yet: only acceptable while nothing was acked.
  v=fs_unmountable; [ "$ACKED" -gt 0 ] && v=fs_unmountable_after_ack
  echo "{\"entry\":$ENTRY,\"acked\":$ACKED,\"verdict\":\"$v\"}" >> "$W/results.jsonl"
  dmsetup remove jt-snap; exit 0
fi
if [ ! -d "$W/snapmnt/state" ]; then
  v=no_state_dir; [ "$ACKED" -gt 0 ] && v=undetected_loss
  echo "{\"entry\":$ENTRY,\"acked\":$ACKED,\"verdict\":\"$v\"}" >> "$W/results.jsonl"
else
  R=$("$JT" verify -dir "$W/snapmnt/state/s" -acked "$ACKED" -probe -reopen 2>"$W/verify.err" | tail -1)
  case "$R" in
    \{*) echo "{\"entry\":$ENTRY,\"r\":$R}" >> "$W/results.jsonl" ;;
    *) echo "{\"entry\":$ENTRY,\"acked\":$ACKED,\"verdict\":\"verifier_crash\"}" >> "$W/results.jsonl"
       cp "$W/verify.err" "$W/verify-crash-$ENTRY.err" ;;
  esac
fi
umount "$W/snapmnt"
dmsetup remove jt-snap
exit 0
