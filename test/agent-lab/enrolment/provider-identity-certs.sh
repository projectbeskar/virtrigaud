#!/usr/bin/env bash
# Checks the Provider identity approver-policy sample: with cert-manager's
# default approver disabled, creates cert-manager Certificates for the
# Provider identity issuer in the Provider's namespace and reports how each
# one's CertificateRequest was decided:
#   documented   the documented shape (Provider URI with its UID, 720h,
#                digital signature + client auth, ECDSA P-256)
#   other-uri    another Provider's URI
#   long         a duration of 721h
#   usages       server auth added
#   rsa          an RSA key
# Run as root inside a disposable test VM only.
set -uo pipefail

TRUST_DOMAIN=${TRUST_DOMAIN:-enrol.test}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
K="kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
uid=$($K -n "$PROVIDER_NS" get provider "$PROVIDER_NAME" -o jsonpath='{.metadata.uid}')
own="spiffe://$TRUST_DOMAIN/virtrigaud/provider/$PROVIDER_NS/$PROVIDER_NAME/$uid"

cert() { # name uri duration usages keyalg keysize
  $K apply -f - >/dev/null <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: probe-pid-$1, namespace: $PROVIDER_NS}
spec:
  secretName: probe-pid-$1
  duration: $3
  privateKey: {algorithm: $5, size: $6}
  usages: $4
  uris: ["$2"]
  issuerRef: {kind: ClusterIssuer, name: provider-identity-ca}
EOF
}
cert documented "$own" 720h '["digital signature", "client auth"]' ECDSA 256
cert other-uri "spiffe://$TRUST_DOMAIN/virtrigaud/provider/$PROVIDER_NS/other/$uid" 720h '["digital signature", "client auth"]' ECDSA 256
cert long "$own" 721h '["digital signature", "client auth"]' ECDSA 256
cert usages "$own" 720h '["digital signature", "client auth", "server auth"]' ECDSA 256
cert rsa "$own" 720h '["digital signature", "client auth"]' RSA 2048
sleep 15
for n in documented other-uri long usages rsa; do
  cr=$($K -n "$PROVIDER_NS" get certificaterequests -o jsonpath="{range .items[?(@.metadata.annotations.cert-manager\.io/certificate-name==\"probe-pid-$n\")]}{.metadata.name}{end}")
  echo "$n: $cr $($K -n "$PROVIDER_NS" get certificaterequest "$cr" -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason}: {.message}; {end}' 2>/dev/null | cut -c1-330)"
done
$K -n "$PROVIDER_NS" delete certificate probe-pid-documented probe-pid-other-uri probe-pid-long probe-pid-usages probe-pid-rsa >/dev/null
$K -n "$PROVIDER_NS" delete secret probe-pid-documented --ignore-not-found >/dev/null
