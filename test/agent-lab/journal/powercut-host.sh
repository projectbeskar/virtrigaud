#!/usr/bin/env bash
# Host side of the power-cut run: boot the test VM, verify the store against
# the acknowledgements received so far, let the workload run for a random
# time, then hard-kill the VM's QEMU process (SIGKILL: no guest shutdown, no
# flush). Repeat CUTS times, then boot once more and verify.
#
# Unprivileged: it only starts and kills the user's own QEMU process.
#
#   VM_START='...' VM_SSH='...' VM_PIDFILE=... ACK_LOG=... powercut-host.sh CUTS OUT
#
# VM_START boots the VM and returns once ssh works; VM_SSH runs a command in
# it; VM_PIDFILE is the QEMU pid file; ACK_LOG is the host file behind the
# guest's acknowledgement serial line.
set -euo pipefail
CUTS=${1:-20}; OUT=${2:?output jsonl}
: "${VM_START:?}" "${VM_SSH:?}" "${VM_PIDFILE:?}" "${ACK_LOG:?}"
GUEST=${GUEST_SCRIPT:-/opt/jt/scripts/powercut-guest.sh}
GUEST_ENV=${GUEST_ENV:-}
MIN=${MIN_RUN:-3}; MAX=${MAX_RUN:-15}

acked() { grep -ao 'ACK [0-9]* [0-9]*' "$ACK_LOG" 2>/dev/null | awk '$2 > m {m = $2} END {print m + 0}'; }

for c in $(seq 0 "$CUTS"); do
  t0=$(date +%s)
  $VM_START >/dev/null
  A=$(acked)
  if [ "$c" -eq "$CUTS" ]; then
    R=$($VM_SSH "$GUEST_ENV $GUEST verify $A" | tail -1)
    echo "{\"cut\":$c,\"final\":true,\"acked\":$A,\"boot_s\":$(( $(date +%s) - t0 )),\"r\":$R}" | tee -a "$OUT"
    break
  fi
  R=$($VM_SSH "$GUEST_ENV $GUEST cycle $A" | tail -1)
  RUN=$(( MIN + RANDOM % (MAX - MIN + 1) ))
  sleep "$RUN"
  PID=$(cat "$VM_PIDFILE")
  kill -9 "$PID"
  while kill -0 "$PID" 2>/dev/null; do sleep 0.1; done
  echo "{\"cut\":$c,\"acked_before\":$A,\"run_s\":$RUN,\"acked_at_cut\":$(acked),\"r\":$R}" | tee -a "$OUT"
done
