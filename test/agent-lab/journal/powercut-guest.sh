#!/usr/bin/env bash
# Guest side of the power-cut run. The state volume is a dedicated virtual
# disk attached with cache=none; acknowledgements go to a serial line whose
# host side is a file, so they survive the cut.
#
#   powercut-guest.sh init               format the state volume
#   powercut-guest.sh cycle ACKED        mount, verify against ACKED, start load
#   powercut-guest.sh verify ACKED       mount and verify only
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/common.sh"
DEV=${DEV:?state volume device}; MNT=${MNT:-/mnt/jt-state}; ACKTTY=${ACKTTY:-/dev/ttyS1}
DIR=$MNT/state/s

case "${1:?}" in
  init)
    mkfs.ext4 -q -F "$DEV"
    mkdir -p "$MNT"; mount "$DEV" "$MNT"; install -d -m 0700 "$MNT/state"; umount "$MNT"
    echo "{\"init\":\"ok\"}" ;;
  cycle|verify)
    ACKED=${2:?acked}
    mkdir -p "$MNT"; mountpoint -q "$MNT" || mount "$DEV" "$MNT"
    R=$("$JT" verify -dir "$DIR" -acked "$ACKED" -probe -reopen 2>/tmp/jt-verify.err | tail -1 || true)
    case "$R" in \{*) ;; *) R="{\"verdict\":\"verifier_crash\"}" ;; esac
    echo "$R"
    if [ "$1" = cycle ]; then
      stty -F "$ACKTTY" raw -echo 115200
      systemd-run --quiet --unit=jt-load --collect "$JT" run -dir "$DIR" -ack "$ACKTTY" \
        -rotate "${ROTATE:-4096}" -checkpoint "${CHECKPOINT:-16}" >/dev/null
    fi ;;
esac
