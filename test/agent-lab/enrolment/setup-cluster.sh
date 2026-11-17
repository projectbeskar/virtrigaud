#!/usr/bin/env bash
# Installs a disposable single-node Kubernetes (k3s) with cert-manager (its
# default CertificateRequest approver disabled) and cert-manager
# approver-policy. Run as root INSIDE A DISPOSABLE TEST VM only.
#
# Every download is pinned by version and checked against the publisher's
# checksum where one is published; the sha256 of everything fetched is
# appended to $LAB/versions.txt.
set -euo pipefail

LAB=${LAB:-/root/lab}
K3S_VERSION=${K3S_VERSION:-v1.32.5+k3s1}
CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-v1.18.2}
HELM_VERSION=${HELM_VERSION:-v3.18.4}
APPROVER_POLICY_VERSION=${APPROVER_POLICY_VERSION:-v0.21.0}

mkdir -p "$LAB/dl"
cd "$LAB/dl"
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
record() { printf '%s  %s  %s\n' "$(sha256sum "$2" | cut -d' ' -f1)" "$1" "$2" >> "$LAB/versions.txt"; }

# k3s: binary checked against the release's sha256sum-amd64.txt.
k3s_url="https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION//+/%2B}"
if [ ! -x /usr/local/bin/k3s ]; then
  curl -fsSLo k3s "$k3s_url/k3s"
  curl -fsSLo k3s-sha256sum-amd64.txt "$k3s_url/sha256sum-amd64.txt"
  grep ' k3s$' k3s-sha256sum-amd64.txt | sha256sum -c -
  install -m 0755 k3s /usr/local/bin/k3s
  record "k3s $K3S_VERSION" k3s
  ln -sf k3s /usr/local/bin/kubectl
  cat > /etc/systemd/system/k3s.service <<'UNIT'
[Unit]
Description=k3s (disposable lab cluster)
After=network-online.target
Wants=network-online.target
[Service]
Type=notify
ExecStart=/usr/local/bin/k3s server --disable=traefik --disable=metrics-server --write-kubeconfig-mode=0600
KillMode=process
Delegate=yes
LimitNOFILE=1048576
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now k3s
fi
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
for _ in $(seq 1 120); do kubectl get nodes 2>/dev/null | grep -q ' Ready' && break; sleep 2; done
kubectl get nodes -o wide
log "k3s ready"

# cert-manager, with the default approver disabled as the enrolment page
# requires (--controllers=*,-certificaterequests-approver).
if ! kubectl get ns cert-manager >/dev/null 2>&1; then
  curl -fsSLo cert-manager.yaml "https://github.com/cert-manager/cert-manager/releases/download/$CERT_MANAGER_VERSION/cert-manager.yaml"
  record "cert-manager $CERT_MANAGER_VERSION manifest" cert-manager.yaml
  kubectl apply -f cert-manager.yaml
fi
if ! kubectl -n cert-manager get deploy cert-manager -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -q certificaterequests-approver; then
  kubectl -n cert-manager patch deploy cert-manager --type=json \
    -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--controllers=*,-certificaterequests-approver"}]'
fi
kubectl -n cert-manager rollout status deploy/cert-manager --timeout=300s
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=300s
kubectl -n cert-manager rollout status deploy/cert-manager-cainjector --timeout=300s
log "cert-manager ready (default approver disabled)"

# Helm, checked against its published sha256.
if [ ! -x /usr/local/bin/helm ]; then
  curl -fsSLo helm.tgz "https://get.helm.sh/helm-$HELM_VERSION-linux-amd64.tar.gz"
  curl -fsSLo helm.tgz.sha256sum "https://get.helm.sh/helm-$HELM_VERSION-linux-amd64.tar.gz.sha256sum"
  echo "$(cut -d' ' -f1 helm.tgz.sha256sum)  helm.tgz" | sha256sum -c -
  tar -xzf helm.tgz linux-amd64/helm
  install -m 0755 linux-amd64/helm /usr/local/bin/helm
  record "helm $HELM_VERSION" helm.tgz
fi

# approver-policy (chart pulled once, recorded, installed from the file).
if ! helm -n cert-manager status cert-manager-approver-policy >/dev/null 2>&1; then
  helm pull oci://quay.io/jetstack/charts/cert-manager-approver-policy --version "$APPROVER_POLICY_VERSION" -d .
  chart=$(ls cert-manager-approver-policy-*.tgz)
  record "approver-policy $APPROVER_POLICY_VERSION chart" "$chart"
  helm install cert-manager-approver-policy "./$chart" -n cert-manager --wait --timeout 5m
fi
kubectl -n cert-manager get deploy -o wide
for d in cert-manager cert-manager-webhook cert-manager-cainjector cert-manager-approver-policy; do
  kubectl -n cert-manager get deploy "$d" -o jsonpath='{.metadata.name}{" "}{.spec.template.spec.containers[0].image}{"\n"}'
done | tee -a "$LAB/versions.txt"
kubectl get pods -A -o jsonpath='{range .items[*]}{.status.containerStatuses[0].image}{" "}{.status.containerStatuses[0].imageID}{"\n"}{end}' \
  | sort -u >> "$LAB/versions.txt"
log "approver-policy ready"
