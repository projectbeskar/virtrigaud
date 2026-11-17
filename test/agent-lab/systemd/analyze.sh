#!/usr/bin/env bash
# analyze.sh [REPO]: record what this systemd makes of the per-host agent's
# installed units: systemd-analyze verify, systemd-analyze security (overall
# exposure and the per-property table), the effective unit properties, the
# running processes' credentials and sandbox, and a Hello probe of the agent.
# With REPO (a copy of the repository's hack/ and packaging/ directories), it
# also runs hack/verify-systemd-units.sh there. Run as root inside the test VM
# after install.sh.
set -uo pipefail
. "$(dirname "$0")/common.sh"
set +e
REPO=${1:-}

section() { printf '\n===== %s\n' "$*"; }

section "host"
. /etc/os-release && echo "$PRETTY_NAME"
uname -r
systemctl --version | head -1
stat -fc 'cgroup fs: %T' /sys/fs/cgroup

section "systemd-analyze verify (installed units)"
paths=()
for u in "${UNITS[@]}"; do paths+=("$UNIT_DIR/$u"); done
systemd-analyze verify --man=no "${paths[@]}"
echo "exit=$?"

if [ -n "$REPO" ]; then
  section "hack/verify-systemd-units.sh"
  bash "$REPO/hack/verify-systemd-units.sh"
  echo "exit=$?"
fi

section "unknown or ignored settings logged by the manager this boot"
journalctl -b -o cat --no-pager -u 'vrtg-*' -t systemd |
  grep -Ei 'unknown|ignoring|unsupported|not supported|failed to parse' || echo "(none)"
journalctl -b -o cat --no-pager | grep -E 'vrtg-[a-z-]+\.service:[0-9]+:' || echo "(no unit-file line warnings)"

for u in "${UNITS[@]}"; do
  section "systemd-analyze security $u"
  systemd-analyze security --no-pager "$u"
done

section "overall exposure"
for u in "${UNITS[@]}"; do
  printf '%-28s %s\n' "$u" "$(systemd-analyze security --no-pager "$u" 2>&1 | grep -E 'Overall exposure level')"
done

PROPS=(Type User Group NoNewPrivileges CapabilityBoundingSet AmbientCapabilities ProtectSystem ProtectHome
  PrivateTmp PrivateDevices PrivateNetwork PrivateIPC ProtectProc ProtectKernelTunables ProtectKernelModules
  ProtectKernelLogs ProtectControlGroups ProtectClock ProtectHostname RestrictNamespaces RestrictRealtime
  RestrictSUIDSGID LockPersonality MemoryDenyWriteExecute RemoveIPC SystemCallArchitectures SystemCallFilter
  SystemCallErrorNumber RestrictAddressFamilies Restart RestartUSec RestartPreventExitStatus StartLimitIntervalUSec
  StartLimitBurst KillMode KillSignal TimeoutStopUSec Delegate MemoryMax MemorySwapMax TasksMax LimitNOFILE
  LimitCORE UMask StateDirectory StateDirectoryMode ReadOnlyPaths ReadWritePaths Wants Requires BindsTo
  PartOf Upholds After Before WantedBy)
for u in "${UNITS[@]}"; do
  section "properties $u"
  args=()
  for p in "${PROPS[@]}"; do args+=(-p "$p"); done
  systemctl show "${args[@]}" "$u" | sed 's/^SystemCallFilter=\(.\{120\}\).*/SystemCallFilter=\1... (truncated)/'
done

section "running processes"
for u in "${UNITS[@]}"; do
  pid=$(systemctl show -p MainPID --value "$u")
  echo "$u: $(unit_state "$u")"
  if [ "${pid:-0}" != 0 ]; then
    grep -E '^(Uid|Gid|CapEff|CapBnd|NoNewPrivs|Seccomp):' "/proc/$pid/status" | paste -sd' ' -
    for ns in mnt net ipc uts pid cgroup user; do printf '%s=%s ' "$ns" "$(readlink "/proc/$pid/ns/$ns")"; done; echo
  fi
done

section "hello"
/usr/local/bin/agent-labpki hello -dir /root/labid -addr 127.0.0.1:9460

section "inventory and seccomp"
# The first inventory runs right after READY; wait past one more collection.
pid0=$(systemctl show -p MainPID --value vrtg-agent.service)
for i in $(seq "${SETTLE:-70}"); do sleep 1; done
echo "agent: $(unit_state vrtg-agent.service) (MainPID at start of wait: $pid0)"
echo "inventory collections logged: $(journalctl -b -o cat --no-pager -u vrtg-agent.service | grep -c 'host inventory collected')"
echo "SIGSYS exits this boot: $(journalctl -b -o cat --no-pager -u 'vrtg-*' | grep -c 'status=31/SYS')"
if [ -f /var/log/audit/audit.log ]; then
  echo "audit SECCOMP records for vrtg-agent: $(grep 'type=SECCOMP' /var/log/audit/audit.log | grep -c vrtg-agent)"
fi
echo "kernel seccomp log lines: $(journalctl -b -k -o cat --no-pager | grep -c 'type=1326')"
/usr/local/bin/agent-labpki hostinfo -dir /root/labid -addr 127.0.0.1:9460 |
  grep -oE '\{"name":"[a-z_]+","result":"[A-Z_]+"(,"reason":"[a-z_]+")?\}' | sed 's/^/    feature: /'
