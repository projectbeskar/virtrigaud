#!/usr/bin/env bash
# Generates the test PKI of an enrolment run, inside a disposable test VM:
#   - a test root CA and, under it, the host-agent subordinate CA that the
#     cert-manager ClusterIssuer signs agent certificates with;
#   - a separate Provider identity CA and the Provider's client certificate
#     (URI spiffe://<trust domain>/virtrigaud/provider/<ns>/<name>);
#   - the development operation signer key file (anchor key + install id);
#   - an enrolment administrator client certificate, signed by the cluster's
#     client CA, whose organisation is the enrolment administrator group.
# Nothing here is a production procedure. Keys stay in $PKI (mode 0700).
set -euo pipefail

PKI=${PKI:-/root/lab/pki}
TRUST_DOMAIN=${TRUST_DOMAIN:-enrol.test}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
ADMIN_USER=${ADMIN_USER:-enrol-admin}
ADMIN_GROUP=${ADMIN_GROUP:-virtrigaud-enrolment-admins}
CLIENT_CA_DIR=${CLIENT_CA_DIR:-/var/lib/rancher/k3s/server/tls}

umask 077
mkdir -p "$PKI"
cd "$PKI"

ec_key() { openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$1"; }

# Test root (path length 1) and host-agent sub-CA (path length 0, URI name
# constraint for the trust domain).
if [ ! -f root.crt ]; then
  ec_key root.key
  openssl req -x509 -new -key root.key -sha256 -days 30 -subj "/CN=Lab test root CA" -out root.crt \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:1" -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -addext "subjectKeyIdentifier=hash"
  ec_key subca.key
  openssl req -new -key subca.key -subj "/CN=Lab host-agent sub-CA" -out subca.csr
  # The sub-CA's URI name constraint permits only the trust domain: every
  # URI in an agent certificate has it as its host
  # (docs/host-agent-enrolment.md, "Constrain the sub-CA to the trust domain").
  printf '%s\n' "basicConstraints=critical,CA:TRUE,pathlen:0" "keyUsage=critical,keyCertSign,cRLSign" \
    "nameConstraints=critical,permitted;URI:$TRUST_DOMAIN" \
    "subjectKeyIdentifier=hash" "authorityKeyIdentifier=keyid" > subca.ext
  openssl x509 -req -in subca.csr -CA root.crt -CAkey root.key -CAcreateserial -days 29 -sha256 \
    -extfile subca.ext -out subca.crt
  openssl verify -CAfile root.crt subca.crt
fi

# Provider identity CA (its own hierarchy) and, once the Provider exists
# (PROVIDER_UID set), the Provider client certificate. Its one URI is
# spiffe://<trust domain>/virtrigaud/provider/<ns>/<name>/<Provider UID>
# (docs/per-host-agent-development.md#peer-authentication).
if [ ! -f provider-ca.crt ]; then
  ec_key provider-ca.key
  openssl req -x509 -new -key provider-ca.key -sha256 -days 30 -subj "/CN=Lab Provider identity CA" -out provider-ca.crt \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -addext "subjectKeyIdentifier=hash"
fi
if [ -n "${PROVIDER_UID:-}" ]; then
  ec_key provider.key
  openssl req -new -key provider.key -subj "/CN=$PROVIDER_NAME" -out provider.csr
  printf '%s\n' "basicConstraints=critical,CA:FALSE" "keyUsage=critical,digitalSignature" "extendedKeyUsage=clientAuth" \
    "subjectAltName=URI:spiffe://$TRUST_DOMAIN/virtrigaud/provider/$PROVIDER_NS/$PROVIDER_NAME/$PROVIDER_UID" \
    "authorityKeyIdentifier=keyid" > provider.ext
  openssl x509 -req -in provider.csr -CA provider-ca.crt -CAkey provider-ca.key -CAcreateserial -days 29 -sha256 \
    -extfile provider.ext -out provider.crt
  openssl verify -CAfile provider-ca.crt provider.crt
fi

# Manager <-> Provider gRPC mTLS (one Secret: the manager uses tls.crt as its
# client certificate, the Provider pod serves it), its own CA.
if [ ! -f mtls.crt ]; then
  ec_key mtls-ca.key
  openssl req -x509 -new -key mtls-ca.key -sha256 -days 30 -subj "/CN=Lab manager-provider mTLS CA" -out mtls-ca.crt \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" -addext "keyUsage=critical,keyCertSign,cRLSign"
  ec_key mtls.key
  svc="virtrigaud-provider-$PROVIDER_NS-$PROVIDER_NAME.$PROVIDER_NS.svc.cluster.local"
  openssl req -new -key mtls.key -subj "/CN=$svc" -out mtls.csr
  printf '%s\n' "basicConstraints=critical,CA:FALSE" "keyUsage=critical,digitalSignature" \
    "extendedKeyUsage=serverAuth,clientAuth" "subjectAltName=DNS:$svc" > mtls.ext
  openssl x509 -req -in mtls.csr -CA mtls-ca.crt -CAkey mtls-ca.key -CAcreateserial -days 29 -sha256 \
    -extfile mtls.ext -out mtls.crt
fi

# Development operation signer key file (docs/operation-grants-manager.md).
if [ ! -f signer.pem ]; then
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 > signer.pem
  { echo '-----BEGIN VIRTRIGAUD DEV INSTALL ID-----'
    head -c 16 /dev/urandom | base64
    echo '-----END VIRTRIGAUD DEV INSTALL ID-----'; } >> signer.pem
fi

# Enrolment administrator: a client certificate of the cluster's client CA,
# organisation = the enrolment administrator group.
if [ ! -f admin.crt ]; then
  ec_key admin.key
  openssl req -new -key admin.key -subj "/O=$ADMIN_GROUP/CN=$ADMIN_USER" -out admin.csr
  printf '%s\n' "basicConstraints=critical,CA:FALSE" "keyUsage=critical,digitalSignature" "extendedKeyUsage=clientAuth" > admin.ext
  openssl x509 -req -in admin.csr -CA "$CLIENT_CA_DIR/client-ca.crt" -CAkey "$CLIENT_CA_DIR/client-ca.key" \
    -CAcreateserial -CAserial admin.srl -days 7 -sha256 -extfile admin.ext -out admin.crt
fi
ls -l "$PKI"
