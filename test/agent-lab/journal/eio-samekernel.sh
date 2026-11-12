#!/usr/bin/env bash
# Restart after a write error without a reboot: the page cache that saw the
# failed writeback is kept. Each cycle: the workload runs; the whole volume
# starts failing every write (dm-flakey error_writes, always down); after the
# first failed commit the workload is killed and the device is healed, but
# the volume is NOT remounted and caches are NOT dropped; the workload is
# restarted on the same mount (phase B); finally the volume is remounted
# with cold caches and verified against every acknowledgement of both phases.
#
#   eio-samekernel.sh CYCLES WORKDIR
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/common.sh"
CYCLES=${1:-10}; W=${2:-/dev/shm/jt-eio-sk}
FSTYPE=${FSTYPE:-ext4}; SIZE=${SIZE:-256M}
DM=jt-eiosk

cleanup() {
  [ -n "${PID:-}" ] && kill -9 "$PID" 2>/dev/null || true
  umount "$W/mnt" 2>/dev/null || true; dmsetup remove "$DM" 2>/dev/null || true
  [ -n "${DATA:-}" ] && losetup -d "$DATA" 2>/dev/null || true
}
trap cleanup EXIT
rm -rf "$W"; mkdir -p "$W/mnt"
DATA=$(loopdev "$W/data.img" "$SIZE")
SECTORS=$(blockdev --getsz "$DATA")
LINEAR="0 $SECTORS linear $DATA 0"
FAULT="0 $SECTORS flakey $DATA 0 0 1 1 error_writes"
swap() { dmsetup suspend --nolockfs "$DM"; dmsetup load "$DM" --table "$1"; dmsetup resume "$DM"; }
dmsetup create "$DM" --table "$LINEAR"
for c in $(seq 1 "$CYCLES"); do
  mkfs_dev "$FSTYPE" "/dev/mapper/$DM"
  mount "/dev/mapper/$DM" "$W/mnt"; install -d -m 0700 "$W/mnt/state"
  ACKS="$W/acks-$c.txt"; : > "$ACKS"
  "$JT" run -dir "$W/mnt/state/s" -ack "$ACKS" -n $((30 + RANDOM % 100)) >/dev/null
  "$JT" run -dir "$W/mnt/state/s" -ack "$ACKS" -interval 20ms -hold-on-error >/dev/null 2>&1 &
  PID=$!
  sleep 0.$((RANDOM % 9 + 1))
  swap "$FAULT"
  for _ in $(seq 1 100); do grep -q '^ERR' "$ACKS" && break; sleep 0.1; done
  kill -9 "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; PID=
  swap "$LINEAR"
  # Phase B: same mount, same page cache.
  B=$("$JT" run -dir "$W/mnt/state/s" -ack "$ACKS" -n 100 2>&1 >/dev/null; echo "exit=$?")
  PHASEB=$(grep -E '^(START|LOST-AT-OPEN|OPENERR|CONTINUITY-ERR|ERR)' "$ACKS" | tail -n +1 | tr '\n"' '; ' | cut -c1-600)
  FSERR=$(dmesg | tail -n 30 | grep -E -o 'EXT4-fs error[^]]*|XFS[^]]*shut[^]]*|Aborting journal[^]]*|remounted read-only' | tail -1 | tr '"' "'" || true)
  umount "$W/mnt" 2>/dev/null || umount -l "$W/mnt"
  echo 3 > /proc/sys/vm/drop_caches
  if mount "/dev/mapper/$DM" "$W/mnt" 2>"$W/mount.err"; then
    R=$("$JT" verify -dir "$W/mnt/state/s" -ack "$ACKS" -reopen 2>/dev/null | tail -1 || true)
    R2=$("$JT" verify -dir "$W/mnt/state/s" -ack "$ACKS" 2>/dev/null | tail -1 || true)
    cp -a "$W/mnt/state/s" "$W/store-$c"
    umount "$W/mnt"
  else
    R="{\"verdict\":\"fs_unmountable_after_ack\"}"; R2=null
  fi
  case "$R" in \{*) ;; *) R="{\"verdict\":\"verifier_crash\"}" ;; esac
  case "$R2" in \{*|null) ;; *) R2="{\"verdict\":\"verifier_crash\"}" ;; esac
  echo "{\"cycle\":$c,\"fs\":\"$FSTYPE\",\"phase_b\":\"$PHASEB $B\",\"fs_error\":\"$FSERR\",\"cold\":$R,\"restart\":$R2}" >> "$W/results.jsonl"
done
echo "eio-samekernel cycles=$CYCLES fs=$FSTYPE" | tee "$W/summary.txt"
verdict_counts "$W/results.jsonl" | tee -a "$W/summary.txt"
