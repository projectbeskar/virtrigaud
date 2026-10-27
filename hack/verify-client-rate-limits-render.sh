#!/usr/bin/env bash
# verify-client-rate-limits-render.sh — CI regression guard for the manager's
# client-side Kubernetes API rate limits (manager.clientQPS /
# manager.clientBurst, the manager's --client-qps / --client-burst).
#
# It asserts, by rendering only (no cluster):
#
#   1. Default values: the manager gets --client-qps=20 and --client-burst=40
#      (the limits it has always used) and its resources are the chart's
#      defaults (500m / 512Mi limits).
#   2. Custom values (--set and a values file) reach the manager's args;
#      nothing else in the manager Deployment changes.
#   3. Zero, negative, non-numeric and fractional-burst values fail the
#      render, naming the key.
#   4. Empty values omit the flags (the manager's defaults apply).
#   5. `helm upgrade --reuse-values` from v0.3.11 (whose values.yaml has no
#      such keys) still renders, without the flags.
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

render() {
  local out="$1"
  shift
  helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" "$@" > "${out}"
}

# check FILE WANT_QPS WANT_BURST [BASELINE] — WANT_* is the expected flag value
# ("-" = absent). With BASELINE, the manager Deployment must equal BASELINE's
# apart from these two flags.
check() {
  python3 - "$@" <<'PY'
import sys, yaml
path, want_qps, want_burst = sys.argv[1:4]
baseline = sys.argv[4] if len(sys.argv) > 4 else None

def manager(p):
    docs = [d for d in yaml.safe_load_all(open(p)) if d]
    mgr = [d for d in docs if d.get("kind") == "Deployment" and d["metadata"]["name"].endswith("-manager")]
    if len(mgr) != 1:
        sys.exit(f"❌ want one manager Deployment in {p}, got {len(mgr)}")
    return mgr[0]

def strip(dep):
    c = dep["spec"]["template"]["spec"]["containers"][0]
    c["args"] = [a for a in c.get("args", []) if not a.startswith(("--client-qps", "--client-burst"))]
    return dep

dep = manager(path)
args = dep["spec"]["template"]["spec"]["containers"][0].get("args", [])
for flag, want in (("--client-qps", want_qps), ("--client-burst", want_burst)):
    got = [a for a in args if a.startswith(flag + "=") or a == flag]
    if want == "-" and got:
        sys.exit(f"❌ unexpected {got}")
    if want != "-" and got != [f"{flag}={want}"]:
        sys.exit(f"❌ manager args {got}, want {flag}={want}")
if baseline is not None and strip(dep) != strip(manager(baseline)):
    sys.exit("❌ the manager Deployment differs from the baseline beyond the client rate-limit flags")
print(f"   ok: --client-qps={want_qps} --client-burst={want_burst}" + (" (rest of the Deployment unchanged)" if baseline else ""))
PY
}

echo "==> 1. defaults: --client-qps=20 --client-burst=40, default resources"
render "${WORKDIR}/default.yaml"
check "${WORKDIR}/default.yaml" 20 40
python3 - "${WORKDIR}/default.yaml" <<'PY'
import sys, yaml
dep = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d.get("kind") == "Deployment"
       and d["metadata"]["name"].endswith("-manager")][0]
lim = dep["spec"]["template"]["spec"]["containers"][0]["resources"]["limits"]
if lim != {"cpu": "500m", "memory": "512Mi"}:
    sys.exit(f"❌ manager limits {lim}, want 500m / 512Mi")
print("   ok: manager limits 500m / 512Mi")
PY

echo "==> 2. custom values reach the manager; nothing else changes"
render "${WORKDIR}/q100.yaml" --set manager.clientQPS=100
check "${WORKDIR}/q100.yaml" 100 40 "${WORKDIR}/default.yaml"
cat > "${WORKDIR}/values-q100.yaml" <<'EOF'
manager:
  clientQPS: 100
  clientBurst: 40
EOF
render "${WORKDIR}/q100-file.yaml" -f "${WORKDIR}/values-q100.yaml"
check "${WORKDIR}/q100-file.yaml" 100 40 "${WORKDIR}/default.yaml"
render "${WORKDIR}/custom.yaml" --set-string manager.clientQPS=2.5 --set manager.clientBurst=200
check "${WORKDIR}/custom.yaml" 2.5 200 "${WORKDIR}/default.yaml"
render "${WORKDIR}/big.yaml" --set manager.clientQPS=1000000 --set manager.clientBurst=1000000
check "${WORKDIR}/big.yaml" 1000000 1000000 "${WORKDIR}/default.yaml"

echo "==> 3. invalid values fail the render, naming the key"
for bad in "manager.clientQPS=0" "manager.clientQPS=-5" "manager.clientQPS=lots" "manager.clientQPS=true" \
            "manager.clientBurst=0" "manager.clientBurst=-1" "manager.clientBurst=1.5" "manager.clientBurst=lots"; do
  if helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --set "${bad}" \
       > /dev/null 2> "${WORKDIR}/bad.err"; then
    echo "❌ ${bad} rendered; an invalid client rate limit must fail the render" >&2
    exit 1
  fi
  if ! grep -q "${bad%%=*}" "${WORKDIR}/bad.err"; then
    echo "❌ ${bad} failed without naming the key: $(cat "${WORKDIR}/bad.err")" >&2
    exit 1
  fi
  echo "   ok: ${bad} refused"
done

echo "==> 4. empty values omit the flags (the manager's defaults apply)"
render "${WORKDIR}/empty.yaml" --set-string manager.clientQPS= --set-string manager.clientBurst=
check "${WORKDIR}/empty.yaml" - - "${WORKDIR}/default.yaml"

echo "==> 5. --reuse-values from ${LEGACY_REF} still renders, without the flags"
if ! git -C "${REPO_ROOT}" cat-file -e "${LEGACY_REF}:charts/virtrigaud/values.yaml" 2>/dev/null; then
  echo "❌ cannot read ${LEGACY_REF}:charts/virtrigaud/values.yaml (fetch tags: git fetch --tags)" >&2
  exit 1
fi
cp -r "${CHART_DIR}" "${WORKDIR}/chart"
git -C "${REPO_ROOT}" show "${LEGACY_REF}:charts/virtrigaud/values.yaml" > "${WORKDIR}/chart/values.yaml"
helm template virtrigaud "${WORKDIR}/chart" --namespace "${NAMESPACE}" > "${WORKDIR}/legacy.yaml"
check "${WORKDIR}/legacy.yaml" - -

echo "✅ manager client rate-limit render checks passed"
