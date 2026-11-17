#!/usr/bin/env bash
# shutdown-guard.sh run [N] | after-reboot: the shutdown unit's guard on a running
# host. Run as root inside the test VM after install.sh (fake executor).
#
# run: seeds N (default 8) powered-on fake-executor VMs, then performs every
# maintenance action an operator or a package manager takes on the shutdown
# unit while the host keeps running, and after each one compares the fake
# VMs (content digest and modification time) and the executor's effects log
# with the state before: any difference is a VM touched. Actions:
#   stop, start, restart and try-restart of vrtg-vm-shutdown.service;
#   a package-style upgrade (unit files reinstalled, daemon-reload, then
#   try-restart of all three units, as a package's post-install step does);
#   a maintenance stop of vrtg-agent.service followed by stop/start/
#   try-restart of the shutdown unit, a daemon-reload and the package-style
#   upgrade again: the agent must stay stopped (no unit may pull it back in).
# The VMs are the fake executor's: install with EXECUTOR=fake.
# It prints one PASS/FAIL line per action and a final verdict.
#
# after-reboot: the positive control, after "systemctl reboot" with the VMs
# of "run" still on: the previous boot's journal must show the guard acting
# (the host was shutting down) and every fake VM must now be off.
set -uo pipefail
. "$(dirname "$0")/common.sh"
set +e
MODE=${1:?usage: shutdown-guard.sh run [N] | after-reboot}
N=${2:-8}
fails=0

# guard_lines SINCE: the shutdown-vms result lines logged since SINCE.
guard_lines() {
  journalctl -o cat --no-pager --since "$1" -u vrtg-vm-shutdown.service |
    grep -oE '"msg":"[^"]*(VM|shutting down)[^"]*"' || true
}

# check NAME BEFORE: compare the fake VMs with BEFORE and report.
check() {
  local name=$1 before=$2 since=$3 now on
  now=$(fake_vm_digest)
  on=$(powered_on)
  if [ "$now" = "$before" ] && [ "$on" = "$N" ]; then
    log "PASS $name: $on/$N VMs on, no VM file or effect changed"
  else
    log "FAIL $name: $on/$N VMs on"
    diff <(echo "$before") <(echo "$now")
    fails=$((fails + 1))
  fi
  guard_lines "$since" | sed 's/^/    guard: /'
}

# act NAME CMD...: run CMD, then check.
act() {
  local name=$1 since before
  shift
  since=$(date '+%Y-%m-%d %H:%M:%S')
  before=$(fake_vm_digest)
  sleep 1
  "$@"
  sleep 1
  check "$name" "$before" "$since"
  log "    agent: $(unit_state vrtg-agent.service)"
  log "    shutdown unit: $(unit_state vrtg-vm-shutdown.service)"
}

package_upgrade() {
  local u
  # A read-only /usr (an immutable host) makes the reinstall a no-op.
  for u in "${UNITS[@]}"; do cp -p "$UNIT_DIR/$u" "/tmp/$u" && install -m 0644 "/tmp/$u" "$UNIT_DIR/$u" 2>/dev/null; done
  systemctl daemon-reload
  systemctl try-restart "${UNITS[@]}"
}

case "$MODE" in
run)
  systemctl is-system-running || true
  seed_fake_vms "$N"
  systemctl restart vrtg-agent.service
  wait_state vrtg-agent.service active || log "agent not active: $(unit_state vrtg-agent.service)"
  systemctl start vrtg-vm-shutdown.service
  log "seeded $N fake VMs; on=$(powered_on)"
  act "stop vrtg-vm-shutdown" systemctl stop vrtg-vm-shutdown.service
  act "start vrtg-vm-shutdown" systemctl start vrtg-vm-shutdown.service
  act "restart vrtg-vm-shutdown" systemctl restart vrtg-vm-shutdown.service
  act "try-restart vrtg-vm-shutdown" systemctl try-restart vrtg-vm-shutdown.service
  act "package-style upgrade (reinstall, daemon-reload, try-restart all)" package_upgrade
  act "maintenance stop of vrtg-agent" systemctl stop vrtg-agent.service
  act "stop vrtg-vm-shutdown (agent stopped)" systemctl stop vrtg-vm-shutdown.service
  act "start vrtg-vm-shutdown (agent stopped)" systemctl start vrtg-vm-shutdown.service
  act "try-restart vrtg-vm-shutdown (agent stopped)" systemctl try-restart vrtg-vm-shutdown.service
  act "daemon-reload (agent stopped)" systemctl daemon-reload
  act "package-style upgrade (agent stopped)" package_upgrade
  if [ "$(systemctl show -p ActiveState --value vrtg-agent.service)" = active ]; then
    log "FAIL the maintenance-stopped agent was started again"
    fails=$((fails + 1))
  else
    log "PASS the maintenance-stopped agent stayed stopped"
  fi
  systemctl start vrtg-agent.service
  wait_state vrtg-agent.service active
  log "agent restarted: $(unit_state vrtg-agent.service)"
  ;;
after-reboot)
  prev=$(journalctl -b -1 -o cat --no-pager -u vrtg-vm-shutdown.service)
  echo "$prev" | grep -E '"msg"' | sed 's/^/    previous boot: /'
  on=$(powered_on)
  total=$(find "$FAKE_DIR/vms" -name '*.json' | wc -l)
  if echo "$prev" | grep -q 'every VM stopped for the host shutdown' && [ "$on" = 0 ] && [ "$total" -gt 0 ]; then
    log "PASS positive control: the guard acted at the reboot; $on/$total VMs on"
  else
    log "FAIL positive control: $on/$total VMs on"
    fails=$((fails + 1))
  fi
  ;;
*) echo "usage: shutdown-guard.sh run [N] | after-reboot" >&2; exit 2 ;;
esac
if [ "$fails" = 0 ]; then log "VERDICT PASS"; else log "VERDICT FAIL ($fails)"; fi
[ "$fails" = 0 ]
