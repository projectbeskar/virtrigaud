#!/usr/bin/env bash
# dm-flakey fault modes on the state volume. Each cycle: a healthy phase
# (linear table), then a faulty phase (dm-flakey with MODE, alternating up and
# down intervals) while the workload runs, then a crash: the workload is
# killed, every outstanding write is dropped (a drop-all table during the
# unmount, so that nothing in the page cache reaches the disk), and the volume
# is remounted healthy and verified against the out-of-band ack log.
#
#   flakey-modes.sh MODE CYCLES WORKDIR
#
# MODE: drop_writes | error_writes | corrupt_byte | random_corrupt
# SCOPE=device (default) applies the mode to the whole volume; SCOPE=files
# applies it only to the blocks the store file and the anchor log occupy when
# the faulty phase starts (file-system metadata stays intact, so the result
# shows the journal's own detection).
# Output: WORKDIR/results.jsonl, one verify report per cycle.
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/common.sh"
MODE=${1:?mode}; CYCLES=${2:-20}; W=${3:-/dev/shm/jt-fl-$1}
FSTYPE=${FSTYPE:-ext4}; SIZE=${SIZE:-256M}
UP=${UP:-1}; DOWN=${DOWN:-1}; SCOPE=${SCOPE:-device}
DM=jt-fl

case "$MODE" in
  drop_writes) FEAT="1 drop_writes" ;;
  error_writes) FEAT="1 error_writes" ;;
  corrupt_byte) FEAT="5 corrupt_bio_byte ${BYTE:-200} w 0 0" ;;
  random_corrupt) FEAT="2 random_write_corrupt ${PROB:-50000000}" ;;
  *) echo "unknown mode $MODE" >&2; exit 64 ;;
esac

cleanup() {
  [ -n "${PID:-}" ] && kill -9 "$PID" 2>/dev/null || true
  umount "$W/mnt" 2>/dev/null || true; dmsetup remove "$DM" 2>/dev/null || true
  [ -n "${DATA:-}" ] && losetup -d "$DATA" 2>/dev/null || true
}
trap cleanup EXIT
rm -rf "$W"; mkdir -p "$W/mnt"
DATA=$(loopdev "$W/data.img" "$SIZE")
SECTORS=$(blockdev --getsz "$DATA")
table() { # table linear|faulty|dropall
  case "$1" in
    linear) echo "0 $SECTORS linear $DATA 0" ;;
    faulty)
      if [ "$SCOPE" = files ]; then
        sync -f "$W/mnt/state/s/state.db"
        table_over "$SECTORS" "$DATA" \
          "$(file_sectors "$W/mnt" "$W/mnt/state/s/state.db" "$W/mnt/state/s/anchor.log")" \
          flakey "$DATA" OFFSET "$UP" "$DOWN" $FEAT
      else
        echo "0 $SECTORS flakey $DATA 0 $UP $DOWN $FEAT"
      fi ;;
    dropall) echo "0 $SECTORS flakey $DATA 0 0 1 1 drop_writes" ;;
  esac
}
swap() { # the table is computed before the suspend (it may sync the volume)
  local t; t=$(table "$1")
  dmsetup suspend --nolockfs "$DM"; dmsetup load "$DM" --table "$t"; dmsetup resume "$DM"; }

dmsetup create "$DM" --table "$(table linear)"
mkfs_dev "$FSTYPE" "/dev/mapper/$DM"
for c in $(seq 1 "$CYCLES"); do
  ACKS="$W/acks-$c.txt"; : > "$ACKS"
  mount "/dev/mapper/$DM" "$W/mnt"; install -d -m 0700 "$W/mnt/state"
  # Healthy phase (continues the same store from cycle to cycle while it is
  # usable; a quarantined store is replaced by a fresh one).
  if ! "$JT" verify -dir "$W/mnt/state/s" -ack "$W/acks-all.txt" >/dev/null 2>&1; then
    [ -f "$W/acks-all.txt" ] && rm -rf "$W/mnt/state/s" && : > "$W/acks-all.txt"
  fi
  "$JT" run -dir "$W/mnt/state/s" -ack "$ACKS" -n $((20 + RANDOM % 60)) >/dev/null
  # Faulty phase, then a crash at a random point.
  swap faulty
  "$JT" run -dir "$W/mnt/state/s" -ack "$ACKS" -interval 5ms > /dev/null 2>&1 &
  PID=$!
  sleep "$(awk -v r=$RANDOM 'BEGIN{printf "%.2f", 1 + (r % 400) / 100}')"
  kill -9 "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; PID=
  swap dropall
  umount "$W/mnt" || umount -l "$W/mnt"
  swap linear
  echo 3 > /proc/sys/vm/drop_caches
  cat "$ACKS" >> "$W/acks-all.txt"
  mount "/dev/mapper/$DM" "$W/mnt" 2>"$W/mount.err" || { echo "{\"cycle\":$c,\"verdict\":\"fs_unmountable_after_ack\"}" >> "$W/results.jsonl"; mkfs_dev "$FSTYPE" "/dev/mapper/$DM"; : > "$W/acks-all.txt"; continue; }
  R=$("$JT" verify -dir "$W/mnt/state/s" -ack "$W/acks-all.txt" -reopen 2>"$W/verify.err" | tail -1 || true)
  case "$R" in \{*) ;; *) R="{\"verdict\":\"verifier_crash\"}"; cp "$W/verify.err" "$W/verify-crash-$c.err" ;; esac
  ERRLINE=$(grep -m1 '^ERR' "$ACKS" | tr '"' "'" || true)
  echo "{\"cycle\":$c,\"mode\":\"$MODE\",\"run_err\":\"$ERRLINE\",\"r\":$R}" >> "$W/results.jsonl"
  umount "$W/mnt"
done
echo "mode=$MODE scope=$SCOPE cycles=$CYCLES fs=$FSTYPE up=$UP down=$DOWN" | tee "$W/summary.txt"
verdict_counts "$W/results.jsonl" | tee -a "$W/summary.txt"
