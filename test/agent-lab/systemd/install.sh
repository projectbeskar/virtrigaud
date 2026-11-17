#!/usr/bin/env bash
# install.sh STAGE: install the per-host agent's reference systemd units,
# their multi-user.target drop-in, sysusers.d and tmpfiles.d fragments, a
# development build of the agent and a lab identity on a disposable test
# host, exactly as packaging/systemd ships them, the way a package would,
# then enable and start the units. Run as root inside the test VM.
#
# STAGE holds:
#   bin/vrtg-agent        development build of cmd/vrtg-agent
#   bin/agent-labpki      test/agent-lab/cmd/agent-labpki (for the probe)
#   units/                a copy of packaging/systemd (with multi-user.target.d/)
#   id/                   the output of "agent-labpki gen"
#
# The users and directories come from the shipped vrtg-agent-setup.service
# (started here before the identity is put in place, since the identity
# files belong to those users). Nothing is changed in the units.
# EXECUTOR=fake adds /etc drop-ins for the development fake executor, so
# that VMs exist to observe (default: none, the units as shipped).
# START=0 installs without starting the agent, signer and shutdown units.
set -euo pipefail
. "$(dirname "$0")/common.sh"
STAGE=${1:?usage: install.sh STAGE}
EXECUTOR=${EXECUTOR:-none}
START=${START:-1}

install -D -m 0755 "$STAGE/bin/vrtg-agent" "$AGENT_BIN"
install -D -m 0755 "$STAGE/bin/agent-labpki" /usr/local/bin/agent-labpki
for u in "${UNITS[@]}"; do install -D -m 0644 "$STAGE/units/$u" "$UNIT_DIR/$u"; done
install -D -m 0644 "$STAGE/units/$TARGET_DROPIN" "$UNIT_DIR/$TARGET_DROPIN"
install -D -m 0644 "$STAGE/units/virtrigaud-agent.sysusers.conf" /usr/lib/sysusers.d/virtrigaud-agent.conf
install -D -m 0644 "$STAGE/units/virtrigaud-agent.tmpfiles.conf" /usr/lib/tmpfiles.d/virtrigaud-agent.conf
systemctl daemon-reload
systemctl start vrtg-agent-setup.service
log "vrtg-agent-setup.service: $(unit_state vrtg-agent-setup.service)"
getent passwd "$AGENT_USER" "$SIGNER_USER"

# Trust configuration: root-owned, read-only to the agent (the signed-image
# location of a release build).
install -d -m 0755 -o root -g root "$TRUST_DIR"
install -m 0644 -o root -g root "$STAGE"/id/trust/* "$TRUST_DIR/"
# Host key for the signer; certificate and provisioning marker for the agent.
install -m 0600 -o "$SIGNER_USER" -g "$SIGNER_USER" "$STAGE/id/signer/host.key" "$SIGNER_DIR/host.key"
install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" "$STAGE/id/pki/host.crt" "$STATE_DIR/pki/host.crt"
install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" "$STAGE/id/host-id" "$STATE_DIR/host-id"
install -d -m 0700 /root/labid && cp -r "$STAGE/id/provider" /root/labid/

if [ "$EXECUTOR" = fake ]; then fake_dropins; fi
systemctl daemon-reload
systemctl enable "${ENABLE_UNITS[@]}"
if [ "$START" = 1 ]; then
  systemctl start vrtg-agent-signer.service vrtg-agent.service vrtg-vm-shutdown.service
  for u in "${UNITS[@]}"; do log "$u: $(unit_state "$u")"; done
fi
