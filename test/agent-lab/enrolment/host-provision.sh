#!/usr/bin/env bash
# Provisions one per-host agent instance on this (disposable) machine, from
# the repository's packaged units (packaging/systemd), unchanged except for
# the per-instance places, without enrolling it:
#   host-provision.sh HOST PORT
# Several instances can share one test VM: each gets its own state, signer
# key directory, signer socket directory, listen port and pair of units
# (vrtg-agent-signer-HOST.service, vrtg-agent-HOST.service), generated from
# the packaged units with the documented flags naming those places. The
# development fake executor is used. The signer is NOT started: per the
# enrolment steps it is (re)started once `enroll csr` made the host key.
# Run as root inside a disposable test VM only.
set -euo pipefail

H=${1:?host}; P=${2:?port}
LAB=${LAB:-/root/lab}
TRUST_DOMAIN=${TRUST_DOMAIN:-enrol.test}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
PKG=${PKG:-$LAB/repo/packaging/systemd}
TRUST=/usr/share/virtrigaud/trust
BIN=/usr/lib/virtrigaud/vrtg-agent

AG=/var/lib/vrtg-agent-$H
SG=/var/lib/vrtg-signer-$H
SOCKDIR=/run/vrtg-signer-$H

# Packaged sysusers/tmpfiles fragments and the setup unit that applies them
# (the agent and signer units want it), binary, shared trust configuration.
install -D -m 0644 "$PKG/virtrigaud-agent.sysusers.conf" /usr/lib/sysusers.d/virtrigaud-agent.conf
install -D -m 0644 "$PKG/virtrigaud-agent.tmpfiles.conf" /usr/lib/tmpfiles.d/virtrigaud-agent.conf
install -m 0644 "$PKG/vrtg-agent-setup.service" /etc/systemd/system/vrtg-agent-setup.service
systemctl daemon-reload
systemctl start vrtg-agent-setup.service
id virtrigaud-agent >/dev/null && id virtrigaud-signer >/dev/null
install -D -m 0755 -o root -g root "$LAB/bin/vrtg-agent" "$BIN"
if [ ! -f "$TRUST/provider-sans" ]; then
  install -d -m 0755 -o root -g root "$TRUST"
  install -m 0644 -o root -g root "$LAB/pki/subca.crt" "$TRUST/host-ca.pem"
  install -m 0644 -o root -g root "$LAB/pki/provider-ca.crt" "$TRUST/provider-ca.pem"
  "$LAB/bin/enrol-lab" anchor -signer-key "$LAB/pki/signer.pem" > "$LAB/run/grant-anchors.pem"
  install -m 0644 -o root -g root "$LAB/run/grant-anchors.pem" "$TRUST/grant-anchors.pem"
  echo "spiffe://$TRUST_DOMAIN/virtrigaud/provider/$PROVIDER_NS/$PROVIDER_NAME" > "$LAB/run/provider-sans"
  install -m 0644 -o root -g root "$LAB/run/provider-sans" "$TRUST/provider-sans"
fi

# Directories (the packaged tmpfiles layout, per instance).
install -d -m 0700 -o virtrigaud-agent -g virtrigaud-agent "$AG" "$AG/pki"
install -d -m 0700 -o virtrigaud-signer -g virtrigaud-signer "$SG"
install -d -m 2750 -o virtrigaud-signer -g virtrigaud-agent "$SOCKDIR"
# Provisioning marker: the host's URI identity.
echo "spiffe://$TRUST_DOMAIN/virtrigaud/host/$PROVIDER_NS/$H" > "$AG/host-id.tmp"
install -m 0600 -o virtrigaud-agent -g virtrigaud-agent "$AG/host-id.tmp" "$AG/host-id"
rm -f "$AG/host-id.tmp"

# Units: the packaged ones, with the instance's places as flags.
sed -e "s|^ExecStart=$BIN signer$|ExecStart=$BIN signer --socket $SOCKDIR/signer.sock --key-file $SG/host.key|" \
    -e "s|^ReadWritePaths=/var/lib/virtrigaud-signer /run/virtrigaud-signer$|ReadWritePaths=$SG $SOCKDIR|" \
    "$PKG/vrtg-agent-signer.service" > "/etc/systemd/system/vrtg-agent-signer-$H.service"
sed -e "s|^ExecStart=$BIN run$|ExecStart=$BIN run --state-dir $AG --listen-address :$P --signer-socket $SOCKDIR/signer.sock --signer-key-file $SG/host.key --executor fake|" \
    -e "s|vrtg-agent-signer.service|vrtg-agent-signer-$H.service|g" \
    -e "s|^StateDirectory=virtrigaud-agent$|StateDirectory=vrtg-agent-$H|" \
    "$PKG/vrtg-agent.service" > "/etc/systemd/system/vrtg-agent-$H.service"
grep -q -- "--key-file $SG/host.key" "/etc/systemd/system/vrtg-agent-signer-$H.service"
grep -q "^ReadWritePaths=$SG $SOCKDIR$" "/etc/systemd/system/vrtg-agent-signer-$H.service"
grep -q -- "--state-dir $AG" "/etc/systemd/system/vrtg-agent-$H.service"
systemctl daemon-reload
echo "provisioned $H on :$P"
