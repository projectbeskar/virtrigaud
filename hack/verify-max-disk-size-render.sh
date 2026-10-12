#!/usr/bin/env bash
# verify-max-disk-size-render.sh — CI regression guard for the administrator's
# disk-size ceiling (manager.maxDiskSize, the manager's --max-disk-size).
#
# It asserts, by rendering only (no cluster):
#
#   1. Default values: the manager gets no --max-disk-size flag, and with
#      webhooks.enabled=true the ValidatingWebhookConfiguration holds only
#      vprovider.kb.io (no admission dependency on VM / VMClass writes).
#   2. manager.maxDiskSize=500Gi becomes --max-disk-size=500Gi; with webhooks
#      off no webhook configuration is rendered.
#   3. With webhooks on as well, the VirtualMachine and VMClass disk-size
#      webhooks are rendered, each with the same caBundle as the serving
#      Secret's ca.crt, and their name, path, rules and failurePolicy match
#      config/webhook/manifests.yaml (generated from the Go markers).
#   4. An invalid value fails the render, naming manager.maxDiskSize.
#   5. `helm upgrade --reuse-values` from v0.3.11 (whose values.yaml has no
#      such key) still renders, with no flag.
#
# Requires: helm, python3 (with PyYAML), git. Renders only.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/virtrigaud"
NAMESPACE="virtrigaud-system"
LEGACY_REF="${LEGACY_REF:-v0.3.11}"

for bin in helm python3 git; do
  if ! command -v "${bin}" >/dev/null 2>&1; then
    echo "❌ required tool not found: ${bin}" >&2
    exit 1
  fi
done

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# check FILE WANT_FLAG WANT_HOOKS — WANT_FLAG is the expected --max-disk-size
# value ("-" = absent); WANT_HOOKS the comma-separated webhook names expected
# in the ValidatingWebhookConfiguration ("-" = none rendered).
check() {
  python3 - "$@" "${REPO_ROOT}/config/webhook/manifests.yaml" <<'PY'
import sys, yaml
path, want_flag, want_hooks, generated = sys.argv[1:]
docs = [d for d in yaml.safe_load_all(open(path)) if d]
mgr = [d for d in docs if d.get("kind") == "Deployment" and d["metadata"]["name"].endswith("-manager")]
if len(mgr) != 1:
    sys.exit(f"❌ want one manager Deployment, got {len(mgr)}")
args = mgr[0]["spec"]["template"]["spec"]["containers"][0].get("args", [])
flags = [a for a in args if a.startswith("--max-disk-size")]
if want_flag == "-" and flags:
    sys.exit(f"❌ unexpected {flags}")
if want_flag != "-" and flags != [f"--max-disk-size={want_flag}"]:
    sys.exit(f"❌ manager args {flags}, want --max-disk-size={want_flag}")
vwcs = [d for d in docs if d.get("kind") == "ValidatingWebhookConfiguration"]
if want_hooks == "-":
    if vwcs:
        sys.exit("❌ a ValidatingWebhookConfiguration is rendered with webhooks off")
    print(f"   ok: flag={want_flag} hooks=none")
    sys.exit(0)
if len(vwcs) != 1:
    sys.exit(f"❌ want one ValidatingWebhookConfiguration, got {len(vwcs)}")
hooks = vwcs[0]["webhooks"]
names = [h["name"] for h in hooks]
if names != want_hooks.split(","):
    sys.exit(f"❌ webhooks {names}, want {want_hooks}")
secret = [d for d in docs if d.get("kind") == "Secret" and d.get("type") == "kubernetes.io/tls"
          and d["metadata"]["name"].endswith("-webhook-certs")]
if len(secret) != 1:
    sys.exit("❌ the self-signed serving Secret is not rendered")
ca = secret[0]["data"]["ca.crt"]
gen = {}
for doc in yaml.safe_load_all(open(generated)):
    for g in (doc or {}).get("webhooks", []):
        gen[g["name"]] = g
for h in hooks:
    if h["clientConfig"].get("caBundle") != ca:
        sys.exit(f"❌ {h['name']}: caBundle is not the serving Secret's ca.crt")
    g = gen.get(h["name"])
    if g is None:
        sys.exit(f"❌ {h['name']} is not in config/webhook/manifests.yaml (no Go marker backs it)")
    if h["clientConfig"]["service"]["path"] != g["clientConfig"]["service"]["path"]:
        sys.exit(f"❌ {h['name']}: path {h['clientConfig']['service']['path']} != generated {g['clientConfig']['service']['path']}")
    if h["rules"] != g["rules"] or h["sideEffects"] != g["sideEffects"] or h["failurePolicy"] != g["failurePolicy"]:
        sys.exit(f"❌ {h['name']}: rules/sideEffects/failurePolicy differ from config/webhook/manifests.yaml")
print(f"   ok: flag={want_flag} hooks={names}, caBundle coherent, in sync with config/webhook/manifests.yaml")
PY
}

echo "==> 1. defaults: no flag, only the Provider webhook"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" > "${WORKDIR}/default.yaml"
check "${WORKDIR}/default.yaml" - -
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --set webhooks.enabled=true > "${WORKDIR}/default-hooks.yaml"
check "${WORKDIR}/default-hooks.yaml" - vprovider.kb.io

echo "==> 2. manager.maxDiskSize becomes the manager flag"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --set manager.maxDiskSize=500Gi > "${WORKDIR}/max.yaml"
check "${WORKDIR}/max.yaml" 500Gi -

echo "==> 3. with webhooks on, the disk-size webhooks are rendered and coherent"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --set manager.maxDiskSize=2Ti \
  --set webhooks.enabled=true > "${WORKDIR}/max-hooks.yaml"
check "${WORKDIR}/max-hooks.yaml" 2Ti vprovider.kb.io,vvirtualmachine-disksize.kb.io,vvmclass-disksize.kb.io

echo "==> 4. invalid values (and anything below 1Gi, which the manager refuses) fail the render"
for ok in "1Gi" "1.5Gi" "1.1G" "2T"; do
  helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --set-string "manager.maxDiskSize=${ok}" > "${WORKDIR}/ok.yaml"
  check "${WORKDIR}/ok.yaml" "${ok}" -
done
for bad in "500" "lots" "0Gi" "100Mi" "none" "-1Gi" "1.5" "1G" "0.5Gi" "1.07G"; do
  if helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --set-string "manager.maxDiskSize=${bad}" \
       > /dev/null 2> "${WORKDIR}/bad.err"; then
    echo "❌ manager.maxDiskSize=${bad} rendered; an invalid disk-size limit must fail the render" >&2
    exit 1
  fi
  if ! grep -q "manager.maxDiskSize" "${WORKDIR}/bad.err"; then
    echo "❌ manager.maxDiskSize=${bad} failed without naming the key: $(cat "${WORKDIR}/bad.err")" >&2
    exit 1
  fi
  echo "   ok: manager.maxDiskSize=${bad} refused"
done

echo "==> 5. --reuse-values from ${LEGACY_REF} still renders"
if ! git -C "${REPO_ROOT}" cat-file -e "${LEGACY_REF}:charts/virtrigaud/values.yaml" 2>/dev/null; then
  echo "❌ cannot read ${LEGACY_REF}:charts/virtrigaud/values.yaml (fetch tags: git fetch --tags)" >&2
  exit 1
fi
cp -r "${CHART_DIR}" "${WORKDIR}/chart"
git -C "${REPO_ROOT}" show "${LEGACY_REF}:charts/virtrigaud/values.yaml" > "${WORKDIR}/chart/values.yaml"
helm template virtrigaud "${WORKDIR}/chart" --namespace "${NAMESPACE}" > "${WORKDIR}/legacy.yaml"
check "${WORKDIR}/legacy.yaml" - -

echo "✅ manager.maxDiskSize render checks passed"
