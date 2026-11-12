#!/usr/bin/env bash
# Real write and fsync errors (EIO) under the journal.
#
#   eio.sh TARGET CYCLES WORKDIR
#
# TARGET:
#   store   - dm-error over the blocks of state.db (a bad region under the
#             bbolt file; the rest of the volume stays healthy)
#   anchor  - dm-error over the blocks of anchor.log
#   device  - dm-flakey error_writes over the whole volume (every write in the
#             down interval fails)
#
# Each cycle: the workload runs on a healthy table; the fault is switched in
# while it runs and it holds after its first failed commit. The verifier then
# (1) reopens the store with the fault still in place (page cache dropped), and
# (2) after the device is healthy again and the volume remounted, reopens it
# twice. Expected: the commit fails with lost{io_error}; no acknowledged op is
# missing; and, where any write can still land, lost{io_error} survives the
# restarts.
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/common.sh"
TARGET=${1:?target}; CYCLES=${2:-10}; W=${3:-/dev/shm/jt-eio-$1}
FSTYPE=${FSTYPE:-ext4}; SIZE=${SIZE:-256M}
DM=jt-eio

cleanup() {
  [ -n "${PID:-}" ] && kill -9 "$PID" 2>/dev/null || true
  umount "$W/mnt" 2>/dev/null || true; dmsetup remove "$DM" 2>/dev/null || true
  [ -n "${DATA:-}" ] && losetup -d "$DATA" 2>/dev/null || true
}
trap cleanup EXIT
rm -rf "$W"; mkdir -p "$W/mnt"
DATA=$(loopdev "$W/data.img" "$SIZE")
SECTORS=$(blockdev --getsz "$DATA")
linear() { echo "0 $SECTORS linear $DATA 0"; }
# error_over FILE: a linear table with dm-error over FILE's extents.
error_over() {
  local bs ranges
  bs=$(stat -f -c %S "$W/mnt")
  ranges=$(filefrag -e "$1" | awk -F'[:. ]+' -v bs="$bs" '/^ *[0-9]+:/ {
      for (i = 1; i <= NF; i++) if ($i != "") f[++n] = $i
      s = f[4] * bs / 512; e = (f[5] + 1) * bs / 512; print s, e; n = 0 }' | sort -n)
  local pos=0
  while read -r s e; do
    [ "$s" -gt "$pos" ] && echo "$pos $((s - pos)) linear $DATA $pos"
    echo "$s $((e - s)) error"
    pos=$e
  done <<< "$ranges"
  [ "$pos" -lt "$SECTORS" ] && echo "$pos $((SECTORS - pos)) linear $DATA $pos"
}
swap() { dmsetup suspend --nolockfs "$DM"; dmsetup load "$DM" --table "$1"; dmsetup resume "$DM"; }
verify() { local r; r=$("$JT" verify -dir "$W/mnt/state/s" -ack "$1" ${2:-} 2>"$W/verify.err" | tail -1)
  case "$r" in \{*) echo "$r" ;; *) echo "{\"verdict\":\"verifier_crash\",\"stderr\":\"$(head -c 300 "$W/verify.err" | tr '\n"' "  ")\"}" ;; esac; }

dmsetup create "$DM" --table "$(linear)"
for c in $(seq 1 "$CYCLES"); do
  mkfs_dev "$FSTYPE" "/dev/mapper/$DM"
  mount "/dev/mapper/$DM" "$W/mnt"; install -d -m 0700 "$W/mnt/state"
  ACKS="$W/acks-$c.txt"; : > "$ACKS"
  "$JT" run -dir "$W/mnt/state/s" -ack "$ACKS" -n $((30 + RANDOM % 100)) >/dev/null
  "$JT" run -dir "$W/mnt/state/s" -ack "$ACKS" -interval 20ms -hold-on-error >/dev/null 2>&1 &
  PID=$!
  sleep 0.$((RANDOM % 9 + 1))
  sync -f "$W/mnt/state/s/state.db" 2>/dev/null || true
  case "$TARGET" in
    store) FAULT=$(error_over "$W/mnt/state/s/state.db") ;;
    anchor) FAULT=$(error_over "$W/mnt/state/s/anchor.log") ;;
    device) FAULT="0 $SECTORS flakey $DATA 0 ${UP:-1} ${DOWN:-2} 1 error_writes" ;;
  esac
  printf '%s\n' "$FAULT" > "$W/fault-$c.table"
  swap "$FAULT"
  for _ in $(seq 1 100); do grep -q '^ERR' "$ACKS" && break; sleep 0.1; done
  ERRLINE=$(grep -m1 '^ERR' "$ACKS" | tr '"' "'" || echo none)
  kill -9 "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; PID=
  # (1) Restart with the fault still present.
  echo 3 > /proc/sys/vm/drop_caches
  R1=$(verify "$ACKS")
  # (2) Device healthy again, volume remounted.
  umount "$W/mnt" 2>/dev/null || umount -l "$W/mnt"
  swap "$(linear)"
  echo 3 > /proc/sys/vm/drop_caches
  if mount "/dev/mapper/$DM" "$W/mnt" 2>"$W/mount.err"; then
    R2=$(verify "$ACKS" -reopen)
    Q=$(cat "$W/mnt/state/s/quarantine" 2>/dev/null || echo none)
    umount "$W/mnt"
  else
    R2="{\"verdict\":\"fs_unmountable_after_ack\"}"; Q=unknown
  fi
  echo "{\"cycle\":$c,\"target\":\"$TARGET\",\"run_err\":\"$ERRLINE\",\"quarantine_file\":\"$Q\",\"faulty\":$R1,\"healed\":$R2}" >> "$W/results.jsonl"
done
echo "target=$TARGET cycles=$CYCLES fs=$FSTYPE" | tee "$W/summary.txt"
