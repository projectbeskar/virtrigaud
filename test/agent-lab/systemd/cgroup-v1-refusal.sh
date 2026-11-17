#!/usr/bin/env bash
# cgroup-v1-refusal.sh set MODE | check EXPECT: boot the test host with another
# cgroup hierarchy and check the agent's start-up refusal. Run as root inside
# the test VM after install.sh.
#
#   set unified|hybrid|legacy  write the kernel command line for the next boot
#                              (grubby on EL, /etc/default/grub.d on Debian
#                              and Ubuntu); the caller then reboots.
#       unified  systemd.unified_cgroup_hierarchy=1 (the default)
#       hybrid   systemd.unified_cgroup_hierarchy=0 (cgroup v1 controllers,
#                systemd's own tree on a cgroup2 mount under /sys/fs/cgroup/unified)
#       legacy   hybrid plus systemd.legacy_systemd_cgroup_controller=1 (no
#                cgroup2 mount at all)
#   check refuse|start         record the hierarchy and the units' state after
#                              boot; "refuse" expects the agent refused with
#                              UnsupportedHost (and never ready) while the
#                              signer and the shutdown unit run and the fake
#                              VMs are untouched; "start" expects it READY.
set -uo pipefail
. "$(dirname "$0")/common.sh"
set +e
CMD=${1:?usage: cgroup-v1-refusal.sh set unified|hybrid|legacy | check refuse|start}
ARG=${2:?missing argument}
ARGS_ALL="systemd.unified_cgroup_hierarchy=0 systemd.unified_cgroup_hierarchy=1 systemd.legacy_systemd_cgroup_controller=1"

set_mode() {
  local add
  case "$1" in
  unified) add="" ;;
  hybrid) add="systemd.unified_cgroup_hierarchy=0" ;;
  legacy) add="systemd.unified_cgroup_hierarchy=0 systemd.legacy_systemd_cgroup_controller=1" ;;
  *) echo "unknown mode $1" >&2; exit 2 ;;
  esac
  if command -v grubby >/dev/null; then
    grubby --update-kernel=ALL --remove-args="$ARGS_ALL"
    [ -n "$add" ] && grubby --update-kernel=ALL --args="$add"
    grubby --info=DEFAULT | grep '^args'
  else
    printf 'GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT %s"\n' "$add" >/etc/default/grub.d/99-lab-cgroup.cfg
    update-grub 2>&1 | tail -1
  fi
  log "next boot: $1"
}

check() {
  local fails=0 msgs
  log "cmdline: $(cat /proc/cmdline)"
  log "/sys/fs/cgroup: $(stat -fc %T /sys/fs/cgroup); mounts:"
  findmnt -rno TARGET,FSTYPE -R /sys/fs/cgroup | sed 's/^/    /'
  for i in $(seq 90); do
    case "$(systemctl show -p ActiveState --value vrtg-agent.service)" in active | failed) break ;; esac
    sleep 1
  done
  for u in "${UNITS[@]}"; do log "$u: $(unit_state "$u")"; done
  msgs=$(journalctl -b -o cat --no-pager -u vrtg-agent.service)
  echo "$msgs" | grep -E '"level":"ERROR"' | sed 's/"time":"[^"]*",//' | sort | uniq -c | sed 's/^/    journal: /'
  log "agent starts this boot: $(echo "$msgs" | grep -c '"msg":"starting"'), ready: $(echo "$msgs" | grep -c '"msg":"started"')"
  log "fake VMs on: $(powered_on)"
  case "$1" in
  refuse)
    echo "$msgs" | grep -q 'UnsupportedHost' && log "PASS refusal reason UnsupportedHost" ||
      { log "FAIL no UnsupportedHost in the journal"; fails=$((fails + 1)); }
    [ "$(echo "$msgs" | grep -c '"msg":"started"')" = 0 ] && log "PASS the agent never became ready" ||
      { log "FAIL the agent became ready"; fails=$((fails + 1)); }
    ;;
  start)
    [ "$(systemctl show -p StatusText --value vrtg-agent.service)" = AGENT_STATE_READY ] && log "PASS agent READY" ||
      { log "FAIL agent not READY"; fails=$((fails + 1)); }
    ;;
  esac
  if [ "$fails" = 0 ]; then log "VERDICT PASS"; else log "VERDICT FAIL ($fails)"; fi
  [ "$fails" = 0 ]
}

case "$CMD" in
set) set_mode "$ARG" ;;
check) check "$ARG" ;;
*) echo "usage: cgroup-v1-refusal.sh set MODE | check EXPECT" >&2; exit 2 ;;
esac
