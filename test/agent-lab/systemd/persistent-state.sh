#!/usr/bin/env bash
# persistent-state.sh CASE...: the agent's positive persistent-state check on
# a real host. Run as root inside the test VM after install.sh. For each
# CASE the agent is stopped, the state directory is put on the named
# storage with its current content (so that every other start-up check still
# passes), the unit is started, and the outcome is compared with the
# expectation; the original state directory is then restored and the agent
# started again.
#
#   as-is          the state directory where it is (expect: start, READY)
#   tmpfs          a tmpfs mounted over it (expect: StateNotPersistent)
#   overlay-tmpfs  an overlay whose upper and work directories are on tmpfs,
#                  with the real directory as the lower (expect:
#                  StateNotPersistent)
#   loop-tmpfs     an ext4 file system on a loop device whose backing file
#                  is on tmpfs (expect: StateNotPersistent)
#   no-marker      as-is but without the host-id provisioning marker, the
#                  stand-in for an empty directory mounted in its place
#                  (expect: StateNotPersistent)
#
# EXPECT_AS_IS=refuse inverts the as-is expectation (a host whose default
# state directory is itself not persistent, such as an immutable OS without
# a persistence rule).
set -uo pipefail
. "$(dirname "$0")/common.sh"
set +e
[ $# -gt 0 ] || { echo "usage: persistent-state.sh CASE..." >&2; exit 2; }
SAVE=/root/pstate-copy
fails=0

copy_in() { cp -a "$SAVE/." "$STATE_DIR/"; chown "$AGENT_USER:$AGENT_USER" "$STATE_DIR"; chmod 0700 "$STATE_DIR"; }

prepare() {
  case "$1" in
  as-is) ;;
  tmpfs)
    mount -t tmpfs -o "mode=0700,uid=$(id -u "$AGENT_USER"),gid=$(id -g "$AGENT_USER")" lab-pstate "$STATE_DIR" && copy_in ;;
  overlay-tmpfs)
    mkdir -p /run/lab-pstate && mount -t tmpfs lab-pstate /run/lab-pstate && mkdir -p /run/lab-pstate/upper /run/lab-pstate/work &&
      mkdir -p /run/lab-pstate-lower && mount --bind "$STATE_DIR" /run/lab-pstate-lower &&
      mount -t overlay -o lowerdir=/run/lab-pstate-lower,upperdir=/run/lab-pstate/upper,workdir=/run/lab-pstate/work lab-pstate "$STATE_DIR" ;;
  loop-tmpfs)
    mkdir -p /run/lab-pstate && mount -t tmpfs lab-pstate /run/lab-pstate && truncate -s 64M /run/lab-pstate/img &&
      mkfs.ext4 -q /run/lab-pstate/img && mount -o loop /run/lab-pstate/img "$STATE_DIR" && copy_in ;;
  no-marker) mv "$STATE_DIR/host-id" /root/pstate-host-id ;;
  *) echo "unknown case $1" >&2; return 2 ;;
  esac
}

restore() {
  case "$1" in
  tmpfs | loop-tmpfs) umount "$STATE_DIR"; umount /run/lab-pstate 2>/dev/null ;;
  overlay-tmpfs) umount "$STATE_DIR"; umount /run/lab-pstate-lower; umount /run/lab-pstate ;;
  no-marker) mv /root/pstate-host-id "$STATE_DIR/host-id" ;;
  esac
}

systemctl stop vrtg-agent.service
rm -rf "$SAVE" && cp -a "$STATE_DIR" "$SAVE"
for c in "$@"; do
  systemctl stop vrtg-agent.service
  prepare "$c" || { log "FAIL $c: could not prepare"; fails=$((fails + 1)); continue; }
  log "case $c: $(findmnt -no SOURCE,FSTYPE -T "$STATE_DIR" | head -1)"
  cursor=$(journalctl -n 1 --show-cursor -o cat --no-pager | sed -n 's/^-- cursor: //p')
  systemctl reset-failed vrtg-agent.service
  systemctl start --no-block vrtg-agent.service
  for i in $(seq 60); do
    case "$(systemctl show -p ActiveState --value vrtg-agent.service)" in active | failed) break ;; esac
    sleep 1
  done
  sleep 2
  state=$(systemctl show -p StatusText --value vrtg-agent.service)
  msg=$(journalctl -o cat --no-pager --after-cursor "$cursor" -u vrtg-agent.service | grep -m1 -oE '"reason":"[A-Za-z]+"|"err":"[^"]*"' | head -2 | paste -sd' ' -)
  first=$(journalctl -o cat --no-pager --after-cursor "$cursor" -u vrtg-agent.service | grep -m1 '"start refused"' | sed 's/"time":"[^"]*",//')
  want=refuse
  [ "$c" = as-is ] && want=${EXPECT_AS_IS:-start}
  got=refuse
  [ "$state" = AGENT_STATE_READY ] && got=start
  log "    unit: $(unit_state vrtg-agent.service)"
  [ -n "$first" ] && log "    journal: $first"
  if [ "$got" = "$want" ] && { [ "$want" = start ] || echo "$first" | grep -q StateNotPersistent; }; then
    log "PASS $c: expected $want, got $got"
  else
    log "FAIL $c: expected $want, got $got ($msg)"
    fails=$((fails + 1))
  fi
  systemctl stop vrtg-agent.service
  restore "$c"
done
systemctl reset-failed vrtg-agent.service
systemctl start vrtg-agent.service
log "restored: $(findmnt -no SOURCE,FSTYPE -T "$STATE_DIR" | head -1); $(unit_state vrtg-agent.service)"
if [ "$fails" = 0 ]; then log "VERDICT PASS"; else log "VERDICT FAIL ($fails)"; fi
[ "$fails" = 0 ]
