#!/usr/bin/env bash
# Shared helpers for the per-host agent systemd scripts. Run inside a
# disposable test VM as root; never on a shared host.
set -euo pipefail

# Installed layout (as packaging/systemd expects it).
AGENT_BIN=${AGENT_BIN:-/usr/lib/virtrigaud/vrtg-agent}
UNIT_DIR=${UNIT_DIR:-/usr/lib/systemd/system}
TRUST_DIR=${TRUST_DIR:-/usr/share/virtrigaud/trust}
STATE_DIR=/var/lib/virtrigaud-agent
SIGNER_DIR=/var/lib/virtrigaud-signer
AGENT_USER=virtrigaud-agent
SIGNER_USER=virtrigaud-signer
# Every shipped unit, in start order.
UNITS=(vrtg-agent-setup.service vrtg-agent-signer.service vrtg-agent.service vrtg-vm-shutdown.service
  vrtg-agent-boot-start.service)
# The units with an [Install] section (what a package enables).
ENABLE_UNITS=(vrtg-agent-signer.service vrtg-agent.service vrtg-vm-shutdown.service vrtg-agent-boot-start.service)
# The drop-in a package ships for multi-user.target.
TARGET_DROPIN=multi-user.target.d/virtrigaud-agent.conf
# Fixed ids from the sysusers.d fragment.
AGENT_UID=564
SIGNER_UID=565
FAKE_DIR=$STATE_DIR/fake-executor

log() { printf '%s %s\n' "$(date -u +%H:%M:%S.%3N)" "$*"; }

# unit_state UNIT: ActiveState/SubState/Result/NRestarts/MainPID on one line.
unit_state() {
  systemctl show -p ActiveState -p SubState -p Result -p NRestarts -p MainPID -p StatusText "$1" | paste -sd' ' -
}

# wait_state UNIT STATE [SECONDS]: wait until ActiveState is STATE.
wait_state() {
  local u=$1 want=$2 n=${3:-30} i
  for ((i = 0; i < n * 5; i++)); do
    [ "$(systemctl show -p ActiveState --value "$u")" = "$want" ] && return 0
    sleep 0.2
  done
  return 1
}

# fake_dropins: drop-ins in /etc that make the agent and the shutdown unit
# use the development fake executor (its VMs are files under the state
# directory). The shipped units use the no-op executor, under which no VM
# exists to observe; scripts that need observable VMs add these and say so.
fake_dropins() {
  install -d /etc/systemd/system/vrtg-agent.service.d /etc/systemd/system/vrtg-vm-shutdown.service.d
  printf '[Service]\nExecStart=\nExecStart=%s run --executor=fake\n' "$AGENT_BIN" \
    >/etc/systemd/system/vrtg-agent.service.d/50-lab-fake-executor.conf
  printf '[Service]\nExecStop=\nExecStop=%s shutdown-vms --executor=fake\n' "$AGENT_BIN" \
    >/etc/systemd/system/vrtg-vm-shutdown.service.d/50-lab-fake-executor.conf
}

# seed_fake_vms N: N fake-executor VMs, powered on, owned by the agent's user.
seed_fake_vms() {
  local n=$1 i
  install -d -m 0700 -o "$AGENT_USER" -g "$AGENT_USER" "$FAKE_DIR" "$FAKE_DIR/vms" "$FAKE_DIR/effects"
  for ((i = 1; i <= n; i++)); do
    printf '{"binding":"%032x","power":"on","generation":0}\n' "$i" >"$FAKE_DIR/vms/vm-$i.json"
  done
  chown -R "$AGENT_USER:$AGENT_USER" "$FAKE_DIR"
  chmod 0600 "$FAKE_DIR"/vms/*.json
}

# fake_vm_digest: one line per fake VM file and the effects log: content
# digest and modification time, so that any write shows.
fake_vm_digest() {
  find "$FAKE_DIR" -type f -printf '%p %T@ ' -exec sha256sum {} \; 2>/dev/null | awk '{print $1, $2, $3}' | sort
}

# powered_on: how many fake VMs are on.
powered_on() {
  { grep -l '"power":"on"' "$FAKE_DIR"/vms/*.json 2>/dev/null || true; } | wc -l
}

# store_digest: digests of every file in the agent's state directory except
# the fake executor's (the store, its anchor log, marker and clock floor).
store_digest() {
  find "$STATE_DIR" -path "$FAKE_DIR" -prune -o -type f -printf '%p %T@ ' -exec sha256sum {} \; | awk '{print $1, $2, $3}' | sort
}
