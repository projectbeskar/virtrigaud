#!/usr/bin/env bash
# build-sysext.sh STAGE OUT: build a systemd system extension image (erofs +
# dm-verity hash partition, no signature partition: the format an immutable
# host booted through GRUB activates) that carries exactly the shipped files:
# the agent binary at /usr/lib/virtrigaud/vrtg-agent, every unit of
# packaging/systemd and its multi-user.target.d drop-in, the sysusers.d and
# tmpfiles.d fragments, and a trust configuration. Nothing is added or
# changed. Run as root inside a disposable build VM with systemd-repart and
# erofs-utils.
#
# STAGE holds bin/vrtg-agent, units/ (a copy of packaging/systemd) and
# id/trust/ (agent-labpki gen). OUT receives virtrigaud-agent.raw, its
# sha256 and CONTENTS.
set -euo pipefail
STAGE=${1:?usage: build-sysext.sh STAGE OUT}
OUT=${2:?missing OUT}
NAME=virtrigaud-agent
T=$(mktemp -d)/tree
U=$T/usr/lib/systemd/system
umask 022

install -D -m 0755 "$STAGE/bin/vrtg-agent" "$T/usr/lib/virtrigaud/vrtg-agent"
for f in "$STAGE"/units/*.service; do install -D -m 0644 "$f" "$U/$(basename "$f")"; done
install -D -m 0644 "$STAGE/units/multi-user.target.d/virtrigaud-agent.conf" "$U/multi-user.target.d/virtrigaud-agent.conf"
install -D -m 0644 "$STAGE/units/virtrigaud-agent.sysusers.conf" "$T/usr/lib/sysusers.d/virtrigaud-agent.conf"
install -D -m 0644 "$STAGE/units/virtrigaud-agent.tmpfiles.conf" "$T/usr/lib/tmpfiles.d/virtrigaud-agent.conf"
install -d -m 0755 "$T/usr/share/virtrigaud/trust"
install -m 0644 "$STAGE"/id/trust/* "$T/usr/share/virtrigaud/trust/"
install -d -m 0755 "$T/usr/lib/extension-release.d"
cat >"$T/usr/lib/extension-release.d/extension-release.$NAME" <<EOF
ID=_any
ARCHITECTURE=x86-64
EXTENSION_RELOAD_MANAGER=1
EOF

defs=$(mktemp -d)
cat >"$defs/10-root.conf" <<'EOF'
[Partition]
Type=root
Format=erofs
CopyFiles=/usr/
Verity=data
VerityMatchKey=root
Minimize=best
EOF
cat >"$defs/20-root-verity.conf" <<'EOF'
[Partition]
Type=root-verity
Verity=hash
VerityMatchKey=root
Minimize=best
EOF
mkdir -p "$OUT"
rm -f "$OUT/$NAME.raw"
systemd-repart --empty=create --size=auto --definitions="$defs" --copy-source="$T" \
  --architecture=x86-64 --json=off "$OUT/$NAME.raw" >"$OUT/$NAME.repart.log" 2>&1 ||
  { cat "$OUT/$NAME.repart.log" >&2; exit 1; }
(cd "$OUT" && sha256sum "$NAME.raw" >"$NAME.raw.sha256")
(cd "$T" && find . \( -type f -o -type l \) -exec sha256sum {} + | sort -k2) >"$OUT/$NAME.CONTENTS"
echo "built $OUT/$NAME.raw"
