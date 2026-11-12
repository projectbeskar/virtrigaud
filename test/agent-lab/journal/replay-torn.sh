#!/usr/bin/env bash
# Torn-write replay: run the journal workload through dm-log-writes, then
# replay the recorded write stream entry by entry onto a scratch device and,
# after every entry, verify a throw-away snapshot of it. Each check point is a
# state the device could be in after a power loss at that point.
#
#   replay-torn.sh FSTYPE OPS WORKDIR
#
# Output: WORKDIR/results.jsonl (one verify report per replay point).
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/common.sh"
FSTYPE=${1:-ext4}; OPS=${2:-300}; W=${3:-/dev/shm/jt-lw}
SIZE=${SIZE:-256M}; LOGSIZE=${LOGSIZE:-3G}
LW=jt-lw

cleanup() {
  umount "$W/mnt" 2>/dev/null || true; umount "$W/snapmnt" 2>/dev/null || true
  dmsetup remove jt-snap 2>/dev/null || true; dmsetup remove "$LW" 2>/dev/null || true
  for d in DATA LOG REPLAY COW; do [ -n "${!d:-}" ] && losetup -d "${!d}" 2>/dev/null || true; done
}
trap cleanup EXIT
rm -rf "$W"; mkdir -p "$W/mnt" "$W/snapmnt"
DATA=$(loopdev "$W/data.img" "$SIZE")
LOG=$(loopdev "$W/log.img" "$LOGSIZE")
REPLAY=$(loopdev "$W/replay.img" "$SIZE")
COW=$(loopdev "$W/cow.img" "$SIZE")
SECTORS=$(blockdev --getsz "$DATA")

# Record: format and workload through dm-log-writes.
dmsetup create "$LW" --table "0 $SECTORS log-writes $DATA $LOG"
mkfs_dev "$FSTYPE" "/dev/mapper/$LW"
dmsetup message "$LW" 0 mark mkfs
mount "/dev/mapper/$LW" "$W/mnt"
install -d -m 0700 "$W/mnt/state"
"$JT" run -dir "$W/mnt/state/s" -ack "$W/acks.txt" -n "$OPS" -dm-mark "$LW" \
  -rotate "${ROTATE:-4096}" -checkpoint "${CHECKPOINT:-16}"
umount "$W/mnt"
dmsetup message "$LW" 0 mark end
dmsetup remove "$LW"

"$JT" lwmarks -log "$LOG" > "$W/marks.txt"
NR=$("$REPLAY_LOG" --log "$LOG" --num-entries)
START=$("$REPLAY_LOG" --log "$LOG" --find --end-mark mkfs | cut -d@ -f1)
echo "entries=$NR mkfs_mark=$START acked=$(tail -1 "$W/acks.txt")" | tee "$W/summary.txt"

# Replay the format, then every later entry with a check after each.
"$REPLAY_LOG" --log "$LOG" --replay "$REPLAY" --end-mark mkfs
# --start-mark seeks past the mark: the first replayed entry is START + 1.
echo "$START" > "$W/counter"
cat > "$W/env" <<ENV
W=$W
REPLAY=$REPLAY
COW=$COW
SECTORS=$SECTORS
JT=$JT
ENV
"$REPLAY_LOG" --log "$LOG" --replay "$REPLAY" --start-mark mkfs --check 1 \
  --fsck "$HERE/replay-check.sh $W"
echo "replay points: $(wc -l < "$W/results.jsonl")" | tee -a "$W/summary.txt"
verdict_counts "$W/results.jsonl" | tee -a "$W/summary.txt"
