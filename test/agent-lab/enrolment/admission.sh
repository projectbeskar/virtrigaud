#!/usr/bin/env bash
# Selects the optional control on the manager's own CertificateRequests, from
# the repository's samples in config/rbac/host-enrolment/admission/ with
# their placeholders replaced by this run's names:
#   admission.sh approver-policy   the approver-policy sample (and no admission policy)
#   admission.sh admission         the deny-only ValidatingAdmissionPolicy sample (and no approver-policy sample)
#   admission.sh none              neither
# Run as root inside a disposable test VM only.
set -euo pipefail

MODE=${1:?approver-policy|admission|none}
LAB=${LAB:-/root/lab}
TRUST_DOMAIN=${TRUST_DOMAIN:-enrol.test}
SAMPLES=$LAB/repo/config/rbac/host-enrolment/admission
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
mkdir -p "$LAB/run"

# The samples' placeholders: example.test (trust domain); host-agent-pki,
# host-agent-ca and virtrigaud-system/virtrigaud-controller-manager already
# name this run's objects (checked).
for f in approver-policy.yaml manager-certificaterequests.yaml; do
  sed "s|example.test|$TRUST_DOMAIN|g" "$SAMPLES/$f" > "$LAB/run/$f"
done
grep -q 'name: host-agent-ca' "$LAB/run/approver-policy.yaml"
grep -q "system:serviceaccount:virtrigaud-system:virtrigaud-controller-manager" "$LAB/run/manager-certificaterequests.yaml"

case "$MODE" in
approver-policy)
  $K delete --ignore-not-found -f "$LAB/run/manager-certificaterequests.yaml"
  $K apply -f "$LAB/run/approver-policy.yaml" ;;
admission)
  $K delete --ignore-not-found -f "$LAB/run/approver-policy.yaml"
  $K apply -f "$LAB/run/manager-certificaterequests.yaml" ;;
none)
  $K delete --ignore-not-found -f "$LAB/run/approver-policy.yaml" -f "$LAB/run/manager-certificaterequests.yaml" ;;
*) echo "unknown mode $MODE" >&2; exit 2 ;;
esac
$K get certificaterequestpolicies,validatingadmissionpolicies,validatingadmissionpolicybindings 2>&1 | grep -v '^No resources' || true
