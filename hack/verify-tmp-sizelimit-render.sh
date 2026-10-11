#!/usr/bin/env bash
# verify-tmp-sizelimit-render.sh — CI regression guard for the /tmp emptyDir
# sizeLimits the chart renders.
#
# It asserts, by rendering only (no cluster):
#
#   1. Default values: the manager's /tmp has sizeLimit 128Mi, the templated
#      providers' /tmp have libvirt 1Gi, Proxmox 1Gi, vSphere 512Gi, and the
#      manager gets no --provider-tmp-size-limit flag (per-type defaults).
#   2. Overrides: a set manager.providerTmpSizeLimit becomes the manager flag,
#      and a set providers.<name>.tmpSizeLimit becomes that pod's sizeLimit.
#   3. An empty value, or "none", renders an unbounded emptyDir ({}), never an
#      invalid one; an invalid value fails the render.
#   4. `helm upgrade --reuse-values` from v0.3.11 (whose values.yaml has none
#      of these keys) still renders: every /tmp is a valid, unbounded emptyDir.
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

# All three templated providers on (they are opt-in), plaintext so they render.
PROVIDERS=(--set providers.libvirt.enabled=true --set providers.vsphere.enabled=true
  --set providers.proxmox.enabled=true --set providerTLS.insecure=true)

# check FILE EXPECT_FLAG KEY=LIMIT... — asserts each Deployment's tmp volume
# sizeLimit ("none" = unbounded {}), keyed by the Deployment name suffix, and the
# manager's --provider-tmp-size-limit flag ("-" = absent).
check() {
  python3 - "$@" <<'PY'
import sys, yaml
path, want_flag, *pairs = sys.argv[1:]
want = dict(p.split("=", 1) for p in pairs)
docs = [d for d in yaml.safe_load_all(open(path)) if d]
deps = {d["metadata"]["name"]: d for d in docs if d.get("kind") == "Deployment"}
seen = set()
for name, dep in deps.items():
    spec = dep["spec"]["template"]["spec"]
    for suffix, limit in want.items():
        if not name.endswith(suffix):
            continue
        seen.add(suffix)
        tmp = [v for v in spec.get("volumes", []) if v["name"] == "tmp"]
        if len(tmp) != 1 or "emptyDir" not in tmp[0] or not isinstance(tmp[0]["emptyDir"], dict):
            sys.exit(f"❌ {name}: /tmp is not a valid emptyDir volume: {tmp}")
        got = tmp[0]["emptyDir"].get("sizeLimit", "none")
        if str(got) != limit:
            sys.exit(f"❌ {name}: /tmp sizeLimit {got!r}, want {limit!r}")
        if suffix == "manager":
            args = spec["containers"][0].get("args", [])
            flags = [a for a in args if a.startswith("--provider-tmp-size-limit")]
            if want_flag == "-" and flags:
                sys.exit(f"❌ {name}: unexpected {flags}")
            if want_flag != "-" and flags != [f"--provider-tmp-size-limit={want_flag}"]:
                sys.exit(f"❌ {name}: got {flags}, want --provider-tmp-size-limit={want_flag}")
missing = set(want) - seen
if missing:
    sys.exit(f"❌ no Deployment rendered for {sorted(missing)}")
print(f"   ok: {sorted(seen)}")
PY
}

echo "==> 1. default limits"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" "${PROVIDERS[@]}" > "${WORKDIR}/default.yaml"
check "${WORKDIR}/default.yaml" - manager=128Mi provider-libvirt=1Gi provider-proxmox=1Gi provider-vsphere=512Gi

echo "==> 2. overrides"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" "${PROVIDERS[@]}" \
  --set manager.providerTmpSizeLimit=20Gi --set providers.vsphere.tmpSizeLimit=1Ti \
  --set manager.tmpSizeLimit=64Mi > "${WORKDIR}/override.yaml"
check "${WORKDIR}/override.yaml" 20Gi manager=64Mi provider-vsphere=1Ti provider-libvirt=1Gi

echo "==> 2b. the ceiling becomes the manager flag"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" \
  --set manager.providerTmpSizeLimitMax=200Gi > "${WORKDIR}/max.yaml"
python3 - "${WORKDIR}/max.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
mgr = [d for d in docs if d.get("kind") == "Deployment" and d["metadata"]["name"].endswith("manager")][0]
args = mgr["spec"]["template"]["spec"]["containers"][0]["args"]
if "--provider-tmp-size-limit-max=200Gi" not in args:
    sys.exit(f"❌ manager args lack --provider-tmp-size-limit-max=200Gi: {args}")
print("   ok: --provider-tmp-size-limit-max=200Gi")
PY

echo "==> 3. empty values and \"none\" render an unbounded emptyDir; invalid values fail"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" "${PROVIDERS[@]}" \
  --set manager.tmpSizeLimit= --set providers.libvirt.tmpSizeLimit= > "${WORKDIR}/empty.yaml"
check "${WORKDIR}/empty.yaml" - manager=none provider-libvirt=none provider-vsphere=512Gi
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" "${PROVIDERS[@]}" \
  --set manager.tmpSizeLimit=none --set providers.vsphere.tmpSizeLimit=none \
  --set providers.proxmox.tmpSizeLimit=none --set manager.providerTmpSizeLimit=none > "${WORKDIR}/none.yaml"
check "${WORKDIR}/none.yaml" none manager=none provider-vsphere=none provider-proxmox=none
for bad in "providers.libvirt.tmpSizeLimit=lots" "providers.vsphere.tmpSizeLimit=0" "manager.tmpSizeLimit=-1Gi" \
           "manager.providerTmpSizeLimit=big" "manager.providerTmpSizeLimitMax=none" "manager.providerTmpSizeLimitMax=x1"; do
  if helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" "${PROVIDERS[@]}" --set "${bad}" \
       > /dev/null 2> "${WORKDIR}/bad.err"; then
    echo "❌ ${bad} rendered; an invalid /tmp sizeLimit must fail the render" >&2
    exit 1
  fi
  key="${bad%%=*}"
  if ! grep -q "${key}" "${WORKDIR}/bad.err"; then
    echo "❌ ${bad} failed without naming ${key}: $(cat "${WORKDIR}/bad.err")" >&2
    exit 1
  fi
  echo "   ok: ${bad} refused"
done

echo "==> 4. --reuse-values from ${LEGACY_REF} still renders"
if ! git -C "${REPO_ROOT}" cat-file -e "${LEGACY_REF}:charts/virtrigaud/values.yaml" 2>/dev/null; then
  echo "❌ cannot read ${LEGACY_REF}:charts/virtrigaud/values.yaml (fetch tags: git fetch --tags)" >&2
  exit 1
fi
cp -r "${CHART_DIR}" "${WORKDIR}/chart"
git -C "${REPO_ROOT}" show "${LEGACY_REF}:charts/virtrigaud/values.yaml" > "${WORKDIR}/chart/values.yaml"
helm template virtrigaud "${WORKDIR}/chart" --namespace "${NAMESPACE}" > "${WORKDIR}/legacy.yaml"
check "${WORKDIR}/legacy.yaml" - manager=none

echo "✅ /tmp sizeLimit render checks passed"
