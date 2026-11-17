#!/usr/bin/env bash
# rollback-reader-version.sh OLD_BINARY: roll the agent back to an older build whose
# operation store reader is below the store's min_reader_version, the way a
# package downgrade would (replace the binary, restart the unit), and check
# that the older agent refuses to start with a clear reason while the store
# and the VMs stay untouched; then roll forward and check that the current
# agent starts on the same store. Run as root inside the test VM after
# install.sh (fake executor).
#
# OLD_BINARY is a development build of cmd/vrtg-agent from a commit whose
# journal reads only schema 1 (see the README for how it is built). That
# build predates the separate host key signer and reads the host key from
# the agent's own pki/ directory, so for the rollback the key is also put
# there (as such a host would have had it), and removed again for the roll
# forward.
#
# Steps:
#   control   the older binary starts on a fresh state directory of its own
#             (proves the binary works and that a refusal comes from the
#             store, not from the build);
#   rollback  stop, swap the binary, start: the unit must fail, the journal
#             must name the store's reader version, the store files and the
#             fake VMs must be byte-identical (and unmodified) afterwards;
#   forward   restore the current binary, start: READY, same VMs.
set -uo pipefail
. "$(dirname "$0")/common.sh"
set +e
OLD=${1:?usage: rollback-reader-version.sh OLD_BINARY}
N=${N:-4}
CTL_DIR=/var/lib/vrtg-old-control
fails=0
pass() { log "PASS $*"; }
fail() { log "FAIL $*"; fails=$((fails + 1)); }

log "current: $("$AGENT_BIN" version) sha256 $(sha256sum "$AGENT_BIN" | cut -c1-16)"
log "older:   $("$OLD" version) sha256 $(sha256sum "$OLD" | cut -c1-16)"
cp -p "$AGENT_BIN" /root/vrtg-agent.current

# Control: the older binary on a fresh state directory of its own.
rm -rf "$CTL_DIR"
install -d -m 0700 -o "$AGENT_USER" -g "$AGENT_USER" "$CTL_DIR" "$CTL_DIR/pki"
install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" "$STATE_DIR/host-id" "$CTL_DIR/host-id"
install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" "$STATE_DIR/pki/host.crt" "$CTL_DIR/pki/host.crt"
install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" "$SIGNER_DIR/host.key" "$CTL_DIR/pki/host.key"
systemctl reset-failed vrtg-old-control.service 2>/dev/null
systemd-run --unit=vrtg-old-control --uid="$AGENT_USER" --gid="$AGENT_USER" -p Type=notify -p NotifyAccess=main \
  -p TimeoutStartSec=30 "$OLD" run --state-dir="$CTL_DIR" --listen-address=127.0.0.1:9461
if [ "$(systemctl show -p ActiveState --value vrtg-old-control.service)" = active ]; then
  pass "control: the older agent starts on a fresh store: $(unit_state vrtg-old-control.service)"
else
  fail "control: the older agent did not start: $(unit_state vrtg-old-control.service)"
fi
journalctl -o cat --no-pager -u vrtg-old-control.service | grep -E '"msg"' | tail -4 | sed 's/^/    control: /'
systemctl stop vrtg-old-control.service
rm -rf "$CTL_DIR"

# The store as the current agent left it.
seed_fake_vms "$N"
systemctl restart vrtg-agent.service
wait_state vrtg-agent.service active
log "current agent: $(unit_state vrtg-agent.service)"
/usr/local/bin/agent-labpki hello -dir /root/labid | grep -o '"journalSchemaVersion":[0-9]*' | sed 's/^/    hello: /'
systemctl stop vrtg-agent.service

# Rollback.
install -m 0600 -o "$AGENT_USER" -g "$AGENT_USER" "$SIGNER_DIR/host.key" "$STATE_DIR/pki/host.key"
install -m 0755 "$OLD" "$AGENT_BIN"
store_before=$(store_digest)
vms_before=$(fake_vm_digest)
echo "$store_before" | sed 's/^/    store: /'
cursor=$(journalctl -n 1 --show-cursor -o cat --no-pager | sed -n 's/^-- cursor: //p')
systemctl reset-failed vrtg-agent.service
systemctl start vrtg-agent.service
for i in $(seq 120); do
  [ "$(systemctl show -p ActiveState --value vrtg-agent.service)" = failed ] && break
  sleep 1
done
log "older agent: $(unit_state vrtg-agent.service)"
msgs=$(journalctl -o cat --no-pager --after-cursor "$cursor" -u vrtg-agent.service)
echo "$msgs" | grep -E '"level":"ERROR"' | sed 's/"time":"[^"]*",//' | sort | uniq -c | sed 's/^/    journal: /'
starts=$(echo "$msgs" | grep -c '"msg":"starting"')
ready=$(echo "$msgs" | grep -c '"msg":"started"')
log "older agent start attempts: $starts, reached ready: $ready"
[ "$(systemctl show -p ActiveState --value vrtg-agent.service)" = failed ] && [ "$ready" = 0 ] &&
  pass "the older agent refused to start (unit failed, never ready)" || fail "the older agent was not refused"
echo "$msgs" | grep -qiE 'reader|schema|newer' && pass "the refusal names the store's version requirement" ||
  fail "no version reason in the journal"
[ "$(store_digest)" = "$store_before" ] && pass "store files byte-identical and unmodified" ||
  { fail "store changed"; diff <(echo "$store_before") <(store_digest); }
[ "$(fake_vm_digest)" = "$vms_before" ] && [ "$(powered_on)" = "$N" ] && pass "VMs untouched ($N/$N on)" ||
  fail "VMs changed"

# Roll forward.
rm -f "$STATE_DIR/pki/host.key"
install -m 0755 /root/vrtg-agent.current "$AGENT_BIN"
systemctl reset-failed vrtg-agent.service
systemctl start vrtg-agent.service
wait_state vrtg-agent.service active
log "current agent again: $(unit_state vrtg-agent.service)"
[ "$(systemctl show -p StatusText --value vrtg-agent.service)" = AGENT_STATE_READY ] &&
  pass "the current agent starts READY on the same store" || fail "the current agent is not READY"
[ "$(fake_vm_digest)" = "$vms_before" ] && pass "VMs still untouched" || fail "VMs changed after roll forward"
if [ "$fails" = 0 ]; then log "VERDICT PASS"; else log "VERDICT FAIL ($fails)"; fi
[ "$fails" = 0 ]
