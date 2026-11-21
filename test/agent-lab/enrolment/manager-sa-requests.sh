#!/usr/bin/env bash
# What a holder of the manager's ServiceAccount token can request from the
# host-agent issuer, under whichever control admission.sh selected. Creates,
# with the manager's kubeconfig, CertificateRequests for a made-up host
# ("ghost") and reports for each whether admission refused it and, if
# admitted, its conditions after a few seconds (who approved or denied it):
#   plain       no labels, no Host annotation (not shaped like a renewal)
#   ca          renewal-shaped but isCA: true
#   shaped      renewal-shaped (labels, Host annotation infra/ghost, the
#               host-agent issuer, the two usages, 720h); the names inside
#               the PKCS #10 request are the ghost's
#   shaped-h1   the same, annotated with an existing Host (infra/h1)
#   self-approve  shaped, then the same token sets Approved=True on it at
#               once (the token holds approve on the signer)
# Run as root inside a disposable test VM only.
set -uo pipefail

LAB=${LAB:-/root/lab}
TRUST_DOMAIN=${TRUST_DOMAIN:-enrol.test}
ENROL_NS=${ENROL_NS:-host-agent-pki}
ISSUER=${ISSUER:-host-agent-ca}
MK="kubectl --kubeconfig $LAB/kube/manager.kubeconfig"
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
d=$LAB/out/manager-sa-requests; mkdir -p "$d"; cd "$d" || exit 1
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out k.pem 2>/dev/null
openssl req -new -key k.pem -subj /CN=ghost -out r.csr -addext "subjectAltName=URI:spiffe://$TRUST_DOMAIN/virtrigaud/host/infra/ghost,URI:spiffe://$TRUST_DOMAIN/virtrigaud/record/enrolment/00112233445566778899aabbccddeeff,URI:spiffe://$TRUST_DOMAIN/virtrigaud/record/provider/x,URI:spiffe://$TRUST_DOMAIN/virtrigaud/record/install/00112233445566778899aabbccddeeff,URI:spiffe://$TRUST_DOMAIN/virtrigaud/record/mode/identity"
req=$(base64 -w0 r.csr)

manifest() { # kind host isCA
  local labels="" ann=""
  if [ "$1" != plain ]; then
    labels='labels: {enrolment.virtrigaud.io/agent: "true", enrolment.virtrigaud.io/renewal: "true"}'
    ann="annotations: {enrolment.virtrigaud.io/host: infra/$2}"
  fi
  cat <<EOF
apiVersion: cert-manager.io/v1
kind: CertificateRequest
metadata:
  generateName: probe-$1-
  namespace: $ENROL_NS
  $labels
  $ann
spec:
  issuerRef: {group: cert-manager.io, kind: ClusterIssuer, name: $ISSUER}
  duration: 720h
  isCA: $3
  usages: ["digital signature", "server auth"]
  request: $req
EOF
}

try() { # label kind host isCA [approve]
  local out name
  out=$(manifest "$2" "$3" "$4" | $MK create -f - -o name 2>&1)
  if ! echo "$out" | grep -q '^certificaterequest'; then
    echo "$1: REFUSED at admission: $(echo "$out" | tr '\n' ' ' | cut -c1-300)"; return
  fi
  name=${out#*/}
  if [ "${5:-}" = approve ]; then
    $MK -n "$ENROL_NS" patch certificaterequest "$name" --subresource=status --type=json -p \
      '[{"op":"add","path":"/status/conditions","value":[{"type":"Approved","status":"True","reason":"ManualProbe","message":"set with the manager token"}]}]' \
      >/dev/null 2>"$d/approve.err" && echo "$1: approve patch accepted" || echo "$1: approve patch failed: $(tr '\n' ' ' < "$d/approve.err" | cut -c1-250)"
  fi
  sleep 6
  echo "$1: admitted as $name; $($K -n "$ENROL_NS" get certificaterequest "$name" -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}: {.message}; {end}' | cut -c1-400)"
  if [ -n "$($K -n "$ENROL_NS" get certificaterequest "$name" -o jsonpath='{.status.certificate}')" ]; then
    echo "$1: ISSUED: $($K -n "$ENROL_NS" get certificaterequest "$name" -o jsonpath='{.status.certificate}' | base64 -d | openssl x509 -noout -subject -ext subjectAltName | tr '\n' ' ' | cut -c1-200)"
  fi
}

try plain plain ghost false
try ca shaped ghost true
try shaped shaped ghost false
try shaped-h1 shaped h1 false
try self-approve shaped ghost false approve
