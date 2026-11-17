#!/usr/bin/env bash
# maint-contract.sh COMMAND: the agent's maintenance contract with the shipped
# units on a running host. Run as root inside the test VM after install.sh
# (or, on an immutable host, after the extension is active and provisioned).
#
#   kicker-stopped   with the agent stopped for maintenance: try-restart and
#                    restart of vrtg-agent-boot-start.service, then a
#                    package-style reinstall + daemon-reload + try-restart of
#                    every shipped unit; the agent must stay stopped after
#                    each. Leaves the agent stopped (a reboot must start it).
#   boot-state       after a boot: the agent's enablement, the units' states,
#                    the kicker's restarts and its log for this boot
#   uid-move USER    re-create USER (virtrigaud-agent or virtrigaud-signer)
#                    with uid 600 by editing /etc/passwd (no file is chowned),
#                    stop everything, then start the units: the setup unit
#                    must fail and neither agent nor signer may start (the
#                    shutdown unit only wants the setup unit; its state is shown)
#   uid-restore USER put the shipped uid back and start the units: all start
#   maint-burst      routine maintenance in quick succession: stop, start,
#                    restart, try-restart of the shutdown unit, a stop and a
#                    start of the agent, then restart and try-restart of the
#                    shutdown unit again; reports whether the shutdown unit is
#                    active and never reached start-limit-hit
#   restart-burst N  N restarts each of the shutdown and setup units back to back (no pause);
#                    reports the time taken and any start-limit record
#   foreign-entries DIR [pki]
#                    plant a subdirectory and a symbolic link owned by uid
#                    999 under DIR (or one file under DIR/pki with "pki"),
#                    restart the setup unit: it must fail and name them;
#                    remove them and restart it: it must succeed
#   seccomp-log      a transient unit with the agent's filter lines
#                    (@system-service, ~@privileged, EPERM, SystemCallLog=
#                    @privileged) running chroot(2) as an unprivileged user;
#                    reports the result and any kernel/audit SECCOMP record,
#                    plus records from the agent itself this boot
set -uo pipefail
. "$(dirname "$0")/common.sh"
set +e
CMD=${1:?usage: maint-contract.sh kicker-stopped|boot-state|uid-move USER|uid-restore USER|maint-burst|restart-burst [N]|foreign-entries DIR [pki]|seccomp-log}
fails=0
agent_state() { systemctl show -p ActiveState --value vrtg-agent.service; }
check_stopped() {
  sleep 3
  if [ "$(agent_state)" = inactive ]; then log "PASS $1: agent stayed stopped"; else
    log "FAIL $1: agent is $(agent_state)"; fails=$((fails + 1)); fi
  log "    kicker: $(unit_state vrtg-agent-boot-start.service)"
}

case "$CMD" in
kicker-stopped)
  systemctl stop vrtg-agent.service
  check_stopped "maintenance stop"
  systemctl try-restart vrtg-agent-boot-start.service; check_stopped "try-restart kicker"
  systemctl restart vrtg-agent-boot-start.service; check_stopped "restart kicker"
  for u in "${UNITS[@]}"; do cp -p "$UNIT_DIR/$u" "/tmp/$u" && install -m 0644 "/tmp/$u" "$UNIT_DIR/$u" 2>/dev/null; done
  systemctl daemon-reload
  systemctl try-restart "${UNITS[@]}"; check_stopped "package-style reinstall + daemon-reload + try-restart all"
  systemctl restart vrtg-agent-setup.service vrtg-agent-boot-start.service; check_stopped "restart setup + kicker"
  journalctl -b -o cat --no-pager -u vrtg-agent-boot-start.service | grep '"msg"' | sed 's/"time":"[^"]*",//' | sort | uniq -c | sed 's/^/    kicker: /'
  ;;
boot-state)
  echo "is-enabled vrtg-agent: $(systemctl is-enabled vrtg-agent.service 2>&1)"
  for u in "${UNITS[@]}"; do log "$u: $(unit_state "$u")"; done
  echo "kicker log this boot:"
  journalctl -b -o cat --no-pager -u vrtg-agent-boot-start.service | sed 's/"time":"[^"]*",//' | sort | uniq -c | sed 's/^/    /'
  echo "start-limit hits this boot: $(journalctl -b -o cat --no-pager | grep -c 'vrtg-.*Start request repeated too quickly\|vrtg-.*start-limit-hit')"
  ;;
uid-move | uid-restore)
  user=${2:?missing USER}
  case "$user" in virtrigaud-agent) want=$AGENT_UID ;; virtrigaud-signer) want=$SIGNER_UID ;; *) exit 2 ;; esac
  systemctl stop vrtg-agent.service vrtg-agent-signer.service vrtg-vm-shutdown.service vrtg-agent-setup.service
  systemctl reset-failed 'vrtg-*'
  if [ "$CMD" = uid-move ]; then new=600; else new=$want; fi
  sed -i -E "s/^($user:x:)[0-9]+:/\1$new:/" /etc/passwd
  log "$user is now uid $(id -u "$user"); state dir owner $(stat -c %u "$STATE_DIR"), signer dir owner $(stat -c %u "$SIGNER_DIR")"
  cursor=$(journalctl -n 1 --show-cursor -o cat --no-pager | sed -n 's/^-- cursor: //p')
  systemctl start vrtg-agent-signer.service vrtg-agent.service vrtg-vm-shutdown.service 2>&1 | sed 's/^/    start: /'
  sleep 3
  for u in "${UNITS[@]}"; do log "$u: $(unit_state "$u")"; done
  journalctl -o cat --no-pager --after-cursor "$cursor" -u vrtg-agent-setup.service | grep -E 'owned by|Failed|status=' | sed 's/^/    setup: /'
  if [ "$CMD" = uid-move ]; then
    [ "$(systemctl show -p Result --value vrtg-agent-setup.service)" != success ] && log "PASS setup unit failed" ||
      { log "FAIL setup unit did not fail"; fails=$((fails + 1)); }
    log "    vrtg-vm-shutdown.service (wants the setup unit; may be active, its start runs nothing): $(unit_state vrtg-vm-shutdown.service)"
    for u in vrtg-agent.service vrtg-agent-signer.service; do
      n=$(journalctl -o cat --no-pager --after-cursor "$cursor" | grep -c "^Started .*$(systemctl show -p Description --value "$u" | sed 's/[().]/./g')")
      [ "$(systemctl show -p ActiveState --value "$u")" != active ] && [ "$n" = 0 ] && log "PASS $u did not start" ||
        { log "FAIL $u started ($(unit_state "$u"))"; fails=$((fails + 1)); }
    done
  else
    for u in vrtg-agent-setup.service vrtg-agent.service vrtg-agent-signer.service vrtg-vm-shutdown.service; do
      [ "$(systemctl show -p ActiveState --value "$u")" = active ] && log "PASS $u active" ||
        { log "FAIL $u not active ($(unit_state "$u"))"; fails=$((fails + 1)); }
    done
  fi
  ;;
maint-burst)
  cursor=$(journalctl -n 1 --show-cursor -o cat --no-pager | sed -n 's/^-- cursor: //p')
  t0=$(date +%s)
  for a in "stop vrtg-vm-shutdown" "start vrtg-vm-shutdown" "restart vrtg-vm-shutdown" "try-restart vrtg-vm-shutdown" \
    "stop vrtg-agent" "start vrtg-agent" "restart vrtg-vm-shutdown" "try-restart vrtg-vm-shutdown"; do
    systemctl $a.service; sleep 1
    log "    $a: shutdown unit $(systemctl show -p ActiveState --value vrtg-vm-shutdown.service)/$(systemctl show -p Result --value vrtg-vm-shutdown.service)"
  done
  log "8 actions in $(( $(date +%s) - t0 )) s"
  hits=$(journalctl -o cat --no-pager --after-cursor "$cursor" -u vrtg-vm-shutdown.service | grep -cE 'start-limit-hit|repeated too quickly')
  if [ "$(systemctl show -p ActiveState --value vrtg-vm-shutdown.service)" = active ] && [ "$hits" = 0 ]; then
    log "PASS shutdown unit active, 0 start-limit records"; else log "FAIL shutdown unit $(unit_state vrtg-vm-shutdown.service), $hits start-limit records"; fails=$((fails + 1)); fi
  ;;
restart-burst)
  cursor=$(journalctl -n 1 --show-cursor -o cat --no-pager | sed -n 's/^-- cursor: //p')
  t0=$(date +%s.%N)
  for i in $(seq "${2:-8}"); do
    systemctl restart vrtg-vm-shutdown.service || log "    restart $i of the shutdown unit failed"
    systemctl restart vrtg-agent-setup.service || log "    restart $i of the setup unit failed"
  done
  log "${2:-8} restarts of the shutdown and setup units in $(awk "BEGIN{printf \"%.2f\", $(date +%s.%N) - $t0}") s"
  for u in vrtg-vm-shutdown.service vrtg-agent-setup.service; do
    hits=$(journalctl -o cat --no-pager --after-cursor "$cursor" -u "$u" | grep -cE 'start-limit-hit|repeated too quickly')
    if [ "$(systemctl show -p ActiveState --value "$u")" = active ] && [ "$hits" = 0 ]; then
      log "PASS $u active, 0 start-limit records"; else log "FAIL $u $(unit_state "$u"), $hits start-limit records"; fails=$((fails + 1)); fi
  done
  ;;
foreign-entries)
  d=${2:?missing DIR}
  if [ "${3:-}" = pki ] && [ ! -d "$d/pki" ]; then
    log "SKIP $d has no pki/ directory"; exit 0
  elif [ "${3:-}" = pki ]; then
    planted=("$d/pki/foreign-file"); install -o 999 -g 999 -m 0600 /dev/null "$d/pki/foreign-file"
  else
    planted=("$d/foreign-dir" "$d/foreign-link"); install -d -o 999 -g 999 -m 0700 "$d/foreign-dir"
    ln -s /etc/hostname "$d/foreign-link"; chown -h 999:999 "$d/foreign-link"
  fi
  ls -lan "$d" ${3:+"$d/pki"} | sed 's/^/    /'
  systemctl reset-failed vrtg-agent-setup.service; systemctl restart vrtg-agent-setup.service 2>/dev/null
  out=$(journalctl -o cat --no-pager -n 20 -u vrtg-agent-setup.service)
  echo "$out" | grep -E 'entries under|foreign' | sed 's/^/    setup: /'
  [ "$(systemctl show -p Result --value vrtg-agent-setup.service)" != success ] && log "PASS setup refused" || { log "FAIL setup succeeded"; fails=$((fails + 1)); }
  for f in "${planted[@]}"; do echo "$out" | grep -qF "$f" && log "PASS named: $f" || { log "FAIL not named: $f"; fails=$((fails + 1)); }; done
  rm -rf "${planted[@]}"
  systemctl reset-failed vrtg-agent-setup.service; systemctl restart vrtg-agent-setup.service
  [ "$(systemctl show -p Result --value vrtg-agent-setup.service)" = success ] && log "PASS setup succeeds after removal" || { log "FAIL setup still fails"; fails=$((fails + 1)); }
  systemctl start vrtg-agent-signer.service vrtg-agent.service
  ;;
seccomp-log)
  echo "kernel.seccomp.actions_logged: $(cat /proc/sys/kernel/seccomp/actions_logged 2>/dev/null)"
  echo "agent SystemCallLog: $(systemctl show -p SystemCallLog --value vrtg-agent.service | cut -c1-80)"
  cursor=$(journalctl -n 1 --show-cursor -o cat --no-pager | sed -n 's/^-- cursor: //p')
  out=$(systemd-run --wait --pipe --quiet -p User=nobody -p NoNewPrivileges=yes -p SystemCallFilter=@system-service \
    -p SystemCallFilter=~@privileged -p SystemCallErrorNumber=EPERM -p SystemCallLog=@privileged \
    /usr/sbin/chroot / /bin/true 2>&1)
  echo "transient chroot result: rc=$? $out"
  sleep 1
  echo "kernel/audit SECCOMP records since: $(journalctl -o cat --no-pager --after-cursor "$cursor" | grep -cE 'type=1326|SECCOMP')"
  journalctl -o cat --no-pager --after-cursor "$cursor" | grep -E 'type=1326|SECCOMP' | head -3 | sed 's/^/    /'
  # Control: SystemCallLog= alone (no filter) on the same call, to show that
  # the log channel works on this host.
  c2=$(journalctl -n 1 --show-cursor -o cat --no-pager | sed -n 's/^-- cursor: //p')
  systemd-run --wait --pipe --quiet -p User=nobody -p SystemCallLog=@privileged /usr/sbin/chroot / /bin/true >/dev/null 2>&1
  sleep 1
  echo "control (SystemCallLog only) records: $(journalctl -o cat --no-pager --after-cursor "$c2" | grep -cE 'type=1326|SECCOMP')"
  [ -f /var/log/audit/audit.log ] && echo "audit.log SECCOMP total: $(grep -c type=SECCOMP /var/log/audit/audit.log); vrtg-agent: $(grep type=SECCOMP /var/log/audit/audit.log | grep -c vrtg-agent)" &&
    grep type=SECCOMP /var/log/audit/audit.log | tail -2 | sed 's/^/    /'
  echo "agent records this boot (journal): $(journalctl -b -o cat --no-pager | grep -E 'type=1326|SECCOMP' | grep -c vrtg-agent)"
  ;;
*) echo "unknown command" >&2; exit 2 ;;
esac
if [ "$fails" = 0 ]; then log "VERDICT PASS"; else log "VERDICT FAIL ($fails)"; fi
[ "$fails" = 0 ]
