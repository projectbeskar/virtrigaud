#!/usr/bin/env bash
# crash-temps.sh: after an agent or signer crash, the next start removes the
# temporary files an interrupted atomic write leaves behind. Run as root
# inside the test VM with the agent and signer running.
#
# The script plants what a write killed between creating and renaming its
# temporary leaves: in the agent's pki/ directory ".start-epoch.<16 hex>" and
# ".host.crt.<16 hex>", in the signer's key directory ".host.key.<16 hex>" and
# ".host.key.pending.<16 hex>", each owned like the real file. It also plants
# one look-alike per directory that does not match the pattern (it must stay).
# Then it kills both processes with SIGKILL (a crash; Restart=always starts
# them again) and checks the directories after the restart.
set -uo pipefail
. "$(dirname "$0")/common.sh"
set +e
fails=0
hex() { od -An -N8 -tx1 /dev/urandom | tr -d ' \n'; }
plant() { printf 'interrupted\n' >"$1" && chown "$2:$2" "$1" && chmod 0600 "$1"; }

PKI=$STATE_DIR/pki
a1=$PKI/.start-epoch.$(hex); a2=$PKI/.host.crt.$(hex); akeep=$PKI/.start-epoch.notatemp
s1=$SIGNER_DIR/.host.key.$(hex); s2=$SIGNER_DIR/.host.key.pending.$(hex); skeep=$SIGNER_DIR/.host.key.notatemp
for f in "$a1" "$a2" "$akeep"; do plant "$f" "$AGENT_USER"; done
for f in "$s1" "$s2" "$skeep"; do plant "$f" "$SIGNER_USER"; done
ls -la "$PKI" "$SIGNER_DIR" | sed 's/^/    before: /'
cursor=$(journalctl -n 1 --show-cursor -o cat --no-pager | sed -n 's/^-- cursor: //p')
apid=$(systemctl show -p MainPID --value vrtg-agent.service)
spid=$(systemctl show -p MainPID --value vrtg-agent-signer.service)
systemctl kill -s SIGKILL vrtg-agent-signer.service
systemctl kill -s SIGKILL vrtg-agent.service
for i in $(seq 60); do
  sleep 1
  na=$(systemctl show -p MainPID --value vrtg-agent.service)
  ns=$(systemctl show -p MainPID --value vrtg-agent-signer.service)
  [ "$na" != 0 ] && [ "$na" != "$apid" ] && [ "$ns" != 0 ] && [ "$ns" != "$spid" ] &&
    [ "$(systemctl show -p StatusText --value vrtg-agent.service)" = AGENT_STATE_READY ] && break
done
log "agent: $(unit_state vrtg-agent.service) (was $apid)"
log "signer: $(unit_state vrtg-agent-signer.service) (was $spid)"
ls -la "$PKI" "$SIGNER_DIR" | sed 's/^/    after: /'
journalctl -o cat --no-pager --after-cursor "$cursor" -u vrtg-agent.service -u vrtg-agent-signer.service |
  grep -E 'removed temporary|status=9|signal=KILL|"msg":"started"' | sed 's/"time":"[^"]*",//; s/^/    journal: /'
for f in "$a1" "$a2" "$s1" "$s2"; do
  [ -e "$f" ] && { log "FAIL still present: $f"; fails=$((fails + 1)); } || log "PASS removed: $f"
done
for f in "$akeep" "$skeep"; do
  [ -e "$f" ] && log "PASS look-alike kept: $f" || { log "FAIL look-alike removed: $f"; fails=$((fails + 1)); }
  rm -f "$f"
done
leftover=$(find "$PKI" "$SIGNER_DIR" -maxdepth 1 -name '.*.*' -newer /proc/1 2>/dev/null | grep -E '\.[0-9a-f]{16}$')
[ -z "$leftover" ] || { log "FAIL temporaries left: $leftover"; fails=$((fails + 1)); }
if [ "$fails" = 0 ]; then log "VERDICT PASS"; else log "VERDICT FAIL ($fails)"; fi
[ "$fails" = 0 ]
