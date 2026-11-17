#!/usr/bin/env bash
# Installs a development-built VirtRigaud manager with per-host agent
# enrolment into the disposable lab cluster (setup-cluster.sh), following
# docs/host-agent-enrolment.md and docs/clustered-agent-provider-development.md.
# Run as root INSIDE A DISPOSABLE TEST VM only, after pki.sh.
#
# Layout expected under $LAB:
#   repo/config/...   the repository's config/ directory (CRDs and RBAC)
#   bin/              manager, provider-cloudhypervisor, vrtg, vrtg-agent,
#                     enrol-lab (all built with -tags virtrigaud_dev)
#   pki/              from pki.sh
#
# The manager runs as a systemd service on the node, out of cluster, with a
# kubeconfig holding a token of its own ServiceAccount (so the cluster
# authorises it exactly as the RBAC below says). The CH Provider runs as the
# Deployment the manager renders, from a local image built from the
# development binary.
set -euo pipefail

LAB=${LAB:-/root/lab}
TRUST_DOMAIN=${TRUST_DOMAIN:-enrol.test}
ENROL_NS=${ENROL_NS:-host-agent-pki}
ISSUER=${ISSUER:-host-agent-ca}
PROVIDER_NS=${PROVIDER_NS:-infra}
PROVIDER_NAME=${PROVIDER_NAME:-ch}
ADMIN_USER=${ADMIN_USER:-enrol-admin}
ADMIN_GROUP=${ADMIN_GROUP:-virtrigaud-enrolment-admins}
MANAGER_NS=virtrigaud-system
MANAGER_SA=virtrigaud-controller-manager
PROVIDER_IMAGE=${PROVIDER_IMAGE:-example.invalid/virtrigaud/provider-cloudhypervisor:lab}
# ADMISSION: the optional control on the manager's own requests, from the
# repository's samples (admission.sh): approver-policy, admission or none.
ADMISSION=${ADMISSION:-approver-policy}

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
K="kubectl"
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
mkdir -p "$LAB/kube" "$LAB/run"

for ns in "$MANAGER_NS" "$PROVIDER_NS" "$ENROL_NS"; do $K create ns "$ns" --dry-run=client -o yaml | $K apply -f -; done

# CRDs (generated; never edited).
$K apply --server-side --force-conflicts -f "$LAB/repo/config/crd/bases/"
$K wait --for=condition=Established crd --all --timeout=120s >/dev/null

# Manager RBAC: config/rbac with the prefix and namespace config/default uses.
mkdir -p "$LAB/overlay/rbac"
cat > "$LAB/overlay/rbac/kustomization.yaml" <<EOF
namespace: $MANAGER_NS
namePrefix: virtrigaud-
resources:
- ../../repo/config/rbac
EOF
$K apply -k "$LAB/overlay/rbac"
# Enrolment RBAC: config/rbac/host-enrolment; its placeholders already name
# this run's namespace, ClusterIssuer and ServiceAccount (checked here).
grep -q "namespace: $ENROL_NS" "$LAB/repo/config/rbac/host-enrolment/role.yaml"
grep -q "clusterissuers.cert-manager.io/$ISSUER" "$LAB/repo/config/rbac/host-enrolment/role.yaml"
$K apply -k "$LAB/repo/config/rbac/host-enrolment"

# The CH Provider's inventory Secret watch (what the chart renders for
# experimental.agentProviderInventoryWatch; written out here because the
# manager is not installed with the chart).
$K apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: virtrigaud-provider-inventory-$PROVIDER_NAME, namespace: $PROVIDER_NS}
rules:
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "watch"]
  resourceNames: ["$PROVIDER_NAME-hosts"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: virtrigaud-provider-inventory-$PROVIDER_NAME, namespace: $PROVIDER_NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: virtrigaud-provider-inventory-$PROVIDER_NAME}
subjects:
- kind: ServiceAccount
  name: virtrigaud-provider-$PROVIDER_NS-$PROVIDER_NAME
  namespace: $PROVIDER_NS
EOF

# Host-agent sub-CA as a ClusterIssuer; its key Secret lives in cert-manager's
# cluster resource namespace (cert-manager), not in the enrolment namespace.
$K -n cert-manager create secret tls "$ISSUER" --cert="$LAB/pki/subca.crt" --key="$LAB/pki/subca.key" \
  --dry-run=client -o yaml | $K apply -f -
$K apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: {name: $ISSUER}
spec:
  ca: {secretName: $ISSUER}
EOF
$K wait --for=condition=Ready clusterissuer/"$ISSUER" --timeout=60s

# The optional control on the manager's own CertificateRequests: one of the
# repository's samples (config/rbac/host-enrolment/admission/).
bash "$(dirname "$0")/admission.sh" "$ADMISSION"

# The Provider identity issuer (a second ClusterIssuer, its own CA, whose
# certificate the agents pin in provider-ca.pem), used by the cert-manager
# Certificate of docs/clustered-agent-provider-development.md. cert-manager's
# default approver is disabled, so its requests are approved by the
# repository's sample config/rbac/host-enrolment/admission/
# provider-identity-approver-policy.yaml (placeholders replaced).
$K -n cert-manager create secret tls provider-identity-ca --cert="$LAB/pki/provider-ca.crt" --key="$LAB/pki/provider-ca.key" \
  --dry-run=client -o yaml | $K apply -f -
$K apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: {name: provider-identity-ca}
spec:
  ca: {secretName: provider-identity-ca}
EOF
sed -e "s|example.test|$TRUST_DOMAIN|g" -e "s|/provider/infra/ch/|/provider/$PROVIDER_NS/$PROVIDER_NAME/|" \
  -e "s|provider-identity-infra-ch|provider-identity-$PROVIDER_NS-$PROVIDER_NAME|" \
  "$LAB/repo/config/rbac/host-enrolment/admission/provider-identity-approver-policy.yaml" > "$LAB/run/provider-identity-approver-policy.yaml"
grep -q "matchNames:" "$LAB/run/provider-identity-approver-policy.yaml"
$K apply -f "$LAB/run/provider-identity-approver-policy.yaml"
$K wait --for=condition=Ready clusterissuer/provider-identity-ca --timeout=60s

# Enrolment administrator: create requests in the enrolment namespace, the
# enrol and rekey verbs on Hosts, patch Hosts (revoke, rekey window).
$K apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: virtrigaud-enrolment-admin, namespace: $ENROL_NS}
rules:
- apiGroups: ["cert-manager.io"]
  resources: ["certificaterequests"]
  verbs: ["create", "get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: virtrigaud-enrolment-admin, namespace: $ENROL_NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: virtrigaud-enrolment-admin}
subjects:
- {kind: User, name: $ADMIN_USER, apiGroup: rbac.authorization.k8s.io}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: virtrigaud-host-admin, namespace: $PROVIDER_NS}
rules:
- apiGroups: ["infra.virtrigaud.io"]
  resources: ["hosts"]
  verbs: ["get", "list", "watch", "patch", "enrol", "rekey"]
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["$PROVIDER_NAME-hosts"]
  verbs: ["get", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: virtrigaud-host-admin, namespace: $PROVIDER_NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: virtrigaud-host-admin}
subjects:
- {kind: User, name: $ADMIN_USER, apiGroup: rbac.authorization.k8s.io}
EOF

server=$($K config view --raw -o jsonpath='{.clusters[0].cluster.server}')
$K config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "$LAB/kube/cluster-ca.crt"
mkkubeconfig() { # file user [token|cert key]
  local f=$1 u=$2; shift 2
  KUBECONFIG=$f $K config set-cluster lab --server="$server" --certificate-authority="$LAB/kube/cluster-ca.crt" --embed-certs >/dev/null
  if [ "$#" = 1 ]; then KUBECONFIG=$f $K config set-credentials "$u" --token="$1" >/dev/null
  else KUBECONFIG=$f $K config set-credentials "$u" --client-certificate="$1" --client-key="$2" --embed-certs >/dev/null; fi
  KUBECONFIG=$f $K config set-context lab --cluster=lab --user="$u" >/dev/null
  KUBECONFIG=$f $K config use-context lab >/dev/null
  chmod 0600 "$f"
}
mkkubeconfig "$LAB/kube/admin.kubeconfig" "$ADMIN_USER" "$LAB/pki/admin.crt" "$LAB/pki/admin.key"
mkkubeconfig "$LAB/kube/manager.kubeconfig" manager "$($K -n "$MANAGER_NS" create token "$MANAGER_SA" --duration=48h)"
KUBECONFIG=$LAB/kube/admin.kubeconfig $K auth whoami

# Provider image: the development binary as its entrypoint, nothing else (the
# Deployment the manager renders passes every flag).
"$LAB/bin/enrol-lab" image -binary "$LAB/bin/provider-cloudhypervisor" -name "$PROVIDER_IMAGE" -out "$LAB/run/provider-image.tar"
k3s ctr -n k8s.io images import "$LAB/run/provider-image.tar"
sha256sum "$LAB/run/provider-image.tar" >> "$LAB/versions.txt"

$K -n "$PROVIDER_NS" create secret generic "$PROVIDER_NAME-tls" --from-file=tls.crt="$LAB/pki/mtls.crt" \
  --from-file=tls.key="$LAB/pki/mtls.key" --from-file=ca.crt="$LAB/pki/mtls-ca.crt" --dry-run=client -o yaml | $K apply -f -

# Manager.
cat > /etc/systemd/system/virtrigaud-manager.service <<EOF
[Unit]
Description=VirtRigaud manager (development build, lab)
After=k3s.service
[Service]
Environment=KUBECONFIG=$LAB/kube/manager.kubeconfig
ExecStart=$LAB/bin/manager --leader-elect=false \\
  --metrics-bind-address=127.0.0.1:18080 --health-probe-bind-address=127.0.0.1:18081 \\
  --enable-dev-provider-types=cloudhypervisor \\
  --dev-operation-signer-key-file=$LAB/pki/signer.pem \\
  --dev-host-enrolment-namespace=$ENROL_NS \\
  --dev-host-enrolment-issuer=ClusterIssuer/$ISSUER \\
  --dev-host-enrolment-admin-group=$ADMIN_GROUP \\
  --dev-host-enrolment-trust-domain=$TRUST_DOMAIN \\
  --dev-host-enrolment-ca-bundle-file=$LAB/pki/subca.crt \\
  --dev-agent-provider-inventory-watch=$PROVIDER_NS/$PROVIDER_NAME
Restart=on-failure
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl restart virtrigaud-manager
sleep 5
systemctl is-active virtrigaud-manager

# The clustered CH Provider and its pool.
$K apply -f - <<EOF
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata: {name: $PROVIDER_NAME, namespace: $PROVIDER_NS}
spec:
  type: cloudhypervisor
  topology: cluster
  endpoint: grpc://agents.invalid:9460
  credentialSecretRef: {name: $PROVIDER_NAME-credentials}
  runtime:
    mode: Remote
    image: $PROVIDER_IMAGE
    imagePullPolicy: Never
    service: {tls: {enabled: true, secretRef: {name: $PROVIDER_NAME-tls}}}
---
apiVersion: infra.virtrigaud.io/v1beta1
kind: HostPool
metadata: {name: pool, namespace: $PROVIDER_NS}
spec:
  providerRef: {name: $PROVIDER_NAME}
EOF
# The Provider identity certificate names the Provider's UID: once the
# Provider exists, a cert-manager Certificate issues it into the Secret its
# credentialSecretRef names (docs/clustered-agent-provider-development.md,
# "Provider identity certificate"); the rendered pod waits for the Secret.
uid=$($K -n "$PROVIDER_NS" get provider "$PROVIDER_NAME" -o jsonpath='{.metadata.uid}')
PROVIDER_URI_SUFFIX=${PROVIDER_URI_SUFFIX-/$uid}
$K apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: $PROVIDER_NAME-provider-identity, namespace: $PROVIDER_NS}
spec:
  secretName: $PROVIDER_NAME-credentials
  duration: 720h
  privateKey: {algorithm: ECDSA, size: 256}
  usages: ["digital signature", "client auth"]
  uris: ["spiffe://$TRUST_DOMAIN/virtrigaud/provider/$PROVIDER_NS/$PROVIDER_NAME$PROVIDER_URI_SUFFIX"]
  issuerRef: {kind: ClusterIssuer, name: provider-identity-ca}
EOF
$K -n "$PROVIDER_NS" wait --for=condition=Ready certificate/"$PROVIDER_NAME-provider-identity" --timeout=120s
log "virtrigaud installed"

# The manager runs out of cluster: let it resolve the Provider Service name
# it records in Provider.status.runtime.endpoint (<svc>.<ns>.svc.cluster.local).
svc=virtrigaud-provider-$PROVIDER_NS-$PROVIDER_NAME
for _ in $(seq 1 60); do ip=$($K -n "$PROVIDER_NS" get svc "$svc" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true); [ -n "$ip" ] && break; sleep 2; done
sed -i "/ $svc.$PROVIDER_NS.svc.cluster.local\$/d" /etc/hosts
echo "$ip $svc.$PROVIDER_NS.svc.cluster.local" >> /etc/hosts
$K -n "$PROVIDER_NS" rollout status deploy/"$svc" --timeout=180s
log "Provider $svc at $ip"
