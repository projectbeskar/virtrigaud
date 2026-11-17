#!/usr/bin/env bash
# sysext-host.sh COMMAND ...: the host side of the system-extension runs on
# an immutable test host (Kairos, GRUB boot) whose /usr comes from images and
# whose /etc and /var are overlays unless a persistence rule says otherwise.
# Run as root inside the disposable test VM.
#
#   persist on|off          install or remove a Kairos "rootfs" stage in /oem
#                           that adds the agent's and the signer's state
#                           directories to the persistent bind mounts
#                           (takes effect at the next boot)
#   activate IMAGE          install IMAGE as the only VirtRigaud extension,
#                           enabled for every boot entry (--common), with a
#                           relative enable link; takes effect at the next boot
#   provision STAGE         make sure vrtg-agent-setup.service ran (users and
#                           directories), then put the lab identity from
#                           STAGE/id in place
#   report                  one block of boot evidence: boot id, merge time,
#                           extension list, users, state-directory mounts,
#                           units (state, start time, restarts), agent
#                           journal reasons, fake VMs, Hello
#   kicker-stop             with the agent stopped for maintenance, stop
#                           vrtg-agent-boot-start by hand: Upholds= runs it
#                           again, and the agent must stay stopped (the
#                           once-per-boot marker is still there)
#   maint                   maintenance checks on the running host: the agent
#                           stopped by an operator stays stopped through a
#                           stop/start/try-restart of the shutdown unit, a
#                           daemon-reload and an extension refresh
set -uo pipefail
. "$(dirname "$0")/common.sh"
set +e
EXT_DIR=/var/lib/kairos/extensions
PERSIST_FILE=/oem/91_virtrigaud_agent_persistence.yaml
CMD=${1:?usage: sysext-host.sh persist|activate|provision|report|maint|kicker-stop ...}
shift

persist() {
  case "$1" in
  on)
    cat >"$PERSIST_FILE" <<'EOF'
name: "virtrigaud agent persistent state"
stages:
  rootfs:
    - name: "virtrigaud agent bind mounts"
      environment_file: /run/cos/custom-layout.env
      environment:
        CUSTOM_BIND_MOUNTS: "/var/lib/virtrigaud-agent /var/lib/virtrigaud-signer"
EOF
    ;;
  off) rm -f "$PERSIST_FILE" ;;
  esac
  ls -la /oem
}

activate() {
  local img=$1 name
  name=$(basename "$img")
  for e in $(kairos-agent sysext list --common 2>/dev/null | grep -o 'virtrigaud-agent[^ ]*\.raw' | sort -u); do
    kairos-agent sysext disable --common "${e%.raw}" >/dev/null 2>&1
    kairos-agent sysext remove "${e%.raw}" >/dev/null 2>&1
  done
  rm -f "$EXT_DIR"/common/virtrigaud-agent-* "$EXT_DIR"/virtrigaud-agent-*
  kairos-agent sysext install "file://$img" || return 1
  kairos-agent sysext enable --common "${name%.raw}" || return 1
  # kairos-agent writes an absolute link, which the initramfs cannot
  # resolve; a relative one is what its own install hook writes.
  ln -sfn "../$name" "$EXT_DIR/common/$name"
  ls -la "$EXT_DIR" "$EXT_DIR/common"
}

provision() {
  local stage=$1
  systemctl start vrtg-agent-setup.service
  install -m 0600 -o "$SIGNER_USER" -g "$SIGNER_USER" "$stage/id/signer/host.key" "$SIGNER_DIR/host.key"
  install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" "$stage/id/pki/host.crt" "$STATE_DIR/pki/host.crt"
  install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" "$stage/id/host-id" "$STATE_DIR/host-id"
  install -d -m 0700 /root/labid && cp -r "$stage/id/provider" /root/labid/
  ls -laR "$STATE_DIR" "$SIGNER_DIR" /run/virtrigaud-signer
}

report() {
  local u
  echo "boot: $(cat /proc/sys/kernel/random/boot_id) uptime=$(cut -d' ' -f1 /proc/uptime)s"
  echo "boots recorded: $(journalctl --list-boots --no-pager 2>/dev/null | wc -l)"
  echo "extensions: $(systemd-sysext list --no-pager 2>/dev/null | tail -n +2 | awk '{print $1}' | paste -sd' ' -)"
  journalctl -b -o short-monotonic --no-pager -u systemd-sysext.service | grep -E 'Merged|Started|Finished|Using extensions' | head -5
  echo "users: $(getent passwd "$AGENT_USER" "$SIGNER_USER" | cut -d: -f1,3 | paste -sd' ' -)"
  echo "state dir: $(findmnt -no SOURCE,FSTYPE -T "$STATE_DIR" | head -1) owner=$(stat -c %U:%a "$STATE_DIR" 2>/dev/null)"
  echo "signer dir: $(findmnt -no SOURCE,FSTYPE -T "$SIGNER_DIR" | head -1) socket dir: $(stat -c '%U:%G:%a' /run/virtrigaud-signer 2>/dev/null)"
  for u in "${UNITS[@]}"; do
    printf '%-32s %s Load=%s ActiveEnter=%s\n' "$u" "$(unit_state "$u")" \
      "$(systemctl show -p LoadState --value "$u")" "$(systemctl show -p ActiveEnterTimestampMonotonic --value "$u")"
  done
  local setup_t u_t order=ok
  setup_t=$(systemctl show -p ActiveEnterTimestampMonotonic --value vrtg-agent-setup.service)
  for u in vrtg-agent-signer.service vrtg-agent.service vrtg-vm-shutdown.service; do
    u_t=$(systemctl show -p InactiveExitTimestampMonotonic --value "$u")
    { [ "${setup_t:-0}" -gt 0 ] && [ "${u_t:-0}" -ge "$setup_t" ]; } || order="NOT-OK($u setup=$setup_t unit=$u_t)"
  done
  echo "setup before units: $order"
  echo "uids: agent=$(id -u "$AGENT_USER" 2>/dev/null) (want $AGENT_UID) signer=$(id -u "$SIGNER_USER" 2>/dev/null) (want $SIGNER_UID)"
  echo "setup unit: $(journalctl -b -o cat --no-pager -u vrtg-agent-setup.service | grep -cE 'Failed|status=') failure lines"
  echo "multi-user.target upholds: $(systemctl show -p Upholds --value multi-user.target)"
  journalctl -b -o cat --no-pager -u vrtg-agent.service -u vrtg-agent-signer.service |
    grep -E '"level":"ERROR"|"msg":"started"|status=|Failed' | sed 's/"time":"[^"]*",//' | sort | uniq -c | head -12
  echo "fake VMs on: $(powered_on) of $(find "$FAKE_DIR/vms" -name '*.json' 2>/dev/null | wc -l)"
  /usr/local/bin/agent-labpki hello -dir /root/labid -addr 127.0.0.1:9460 2>&1 | grep -oE '"signed":[a-z]+|"agentState":"[A-Z_]+"|"error":"[^"]{0,120}' | paste -sd' ' -
}

maint() {
  local fails=0 a
  systemctl stop vrtg-agent.service
  for step in "systemctl stop vrtg-vm-shutdown.service" "systemctl start vrtg-vm-shutdown.service" \
    "systemctl try-restart vrtg-vm-shutdown.service" "systemctl daemon-reload" "systemd-sysext refresh" \
    "systemctl stop vrtg-vm-shutdown.service"; do
    $step >/dev/null 2>&1
    sleep 2
    a=$(systemctl show -p ActiveState --value vrtg-agent.service)
    printf '%-48s agent=%s shutdown-unit=%s\n' "$step" "$a" "$(systemctl show -p ActiveState --value vrtg-vm-shutdown.service)"
    [ "$a" = active ] && fails=$((fails + 1))
  done
  systemctl start vrtg-agent.service
  wait_state vrtg-agent.service active
  echo "agent started again: $(unit_state vrtg-agent.service)"
  if [ "$fails" = 0 ]; then log "PASS a maintenance-stopped agent stayed stopped"; else log "FAIL the agent was started again ($fails)"; fi
  [ "$fails" = 0 ]
}

kicker_stop() {
  local fails=0
  systemctl stop vrtg-agent.service
  sleep 2
  echo "agent stopped: $(unit_state vrtg-agent.service)"
  systemctl stop vrtg-agent-boot-start.service
  sleep 3
  echo "after stopping the boot-start unit: boot-start $(systemctl show -p ActiveState --value vrtg-agent-boot-start.service), agent $(unit_state vrtg-agent.service)"
  if [ "$(systemctl show -p ActiveState --value vrtg-agent.service)" = active ]; then
    log "FAIL stopping the boot-start unit by hand started the maintenance-stopped agent"
    fails=1
  else
    log "PASS the maintenance-stopped agent stayed stopped"
  fi
  systemctl start vrtg-agent.service
  wait_state vrtg-agent.service active
  echo "agent started again: $(unit_state vrtg-agent.service)"
  [ "$fails" = 0 ]
}

case "$CMD" in
persist) persist "$@" ;;
activate) activate "$@" ;;
provision) provision "$@" ;;
report) report ;;
maint) maint ;;
kicker-stop) kicker_stop ;;
*) echo "unknown command $CMD" >&2; exit 2 ;;
esac
