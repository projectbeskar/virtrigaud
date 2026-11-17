#!/usr/bin/env bash
# Prepare a disposable host (a test VM, as root) for the agent soak and store
# growth runs: two unprivileged users, a dedicated block device formatted and
# mounted for the agent's state, and the directories the tool uses.
#
#   setup-host.sh [DEVICE] [MOUNTPOINT]      defaults /dev/vdb /mnt/agent-state
#
# Never run this on a shared host: it formats DEVICE.
set -euo pipefail

DEV=${1:-/dev/vdb}
MNT=${2:-/mnt/agent-state}

[ "$(id -u)" = 0 ] || { echo "run as root inside a disposable host" >&2; exit 1; }
[ -b "$DEV" ] || { echo "$DEV is not a block device" >&2; exit 1; }

id soakagent >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin soakagent
id soaksigner >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin soaksigner

if ! mountpoint -q "$MNT"; then
  mkfs.ext4 -F -q "$DEV"
  mkdir -p "$MNT"
  mount "$DEV" "$MNT"
fi
mkdir -p "$MNT/state" /etc/agent-soak /opt/agent-soak /var/lib/agent-soak
chmod 755 /etc/agent-soak /opt/agent-soak

echo "state disk:"; findmnt -no SOURCE,FSTYPE,TARGET "$MNT"
echo "kernel: $(uname -r); systemd: $(systemctl --version | head -1)"
