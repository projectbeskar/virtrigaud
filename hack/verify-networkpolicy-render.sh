#!/usr/bin/env bash
# verify-networkpolicy-render.sh — CI regression guard for the chart's
# NetworkPolicy templates.
#
# It asserts three things, all by rendering only (no cluster):
#
#   1. Default values render NO NetworkPolicy (the feature is opt-in).
#   2. networkPolicy.enabled=true renders the manager and provider policies, and
#      every ingress `from` / egress `to` peer is non-empty. An empty peer is
#      rejected by the API server ("must specify a peer") and fails the release.
#   3. `helm upgrade --reuse-values` from v0.3.11 renders NO NetworkPolicy.
#      --reuse-values replaces the new chart's defaults with the OLD chart's
#      values.yaml; v0.3.11 shipped `security.networkPolicies.enabled: true`,
#      which must not switch policies on. This is emulated by rendering the
#      current templates with v0.3.11's values.yaml as the chart defaults.
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

count_netpols() {
  python3 - "$1" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
print(sum(1 for d in docs if d.get("kind") == "NetworkPolicy"))
PY
}

echo "==> 1. default values render no NetworkPolicy"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" > "${WORKDIR}/default.yaml"
n="$(count_netpols "${WORKDIR}/default.yaml")"
if [[ "${n}" != "0" ]]; then
  echo "❌ default render produced ${n} NetworkPolicy object(s); the feature must be opt-in" >&2
  exit 1
fi

echo "==> 2. networkPolicy.enabled=true renders valid peers (with and without webhooks)"
for webhooks in false true; do
  out="${WORKDIR}/enabled-${webhooks}.yaml"
  helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" \
    --set networkPolicy.enabled=true --set webhooks.enabled="${webhooks}" > "${out}"
  python3 - "${out}" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
pols = [d for d in docs if d.get("kind") == "NetworkPolicy"]
if len(pols) < 2:
    sys.exit(f"❌ expected the manager and provider NetworkPolicies, got {len(pols)}")
for p in pols:
    name = p["metadata"]["name"]
    spec = p.get("spec", {})
    for direction, key in (("ingress", "from"), ("egress", "to")):
        for i, rule in enumerate(spec.get(direction) or []):
            for j, peer in enumerate(rule.get(key) or []):
                if not peer:
                    sys.exit(f"❌ {name}: {direction}[{i}].{key}[{j}] is an empty peer (API server rejects it)")
print(f"   ok: {len(pols)} policies, all peers non-empty")
PY
done

echo "==> 3. --reuse-values from ${LEGACY_REF} renders no NetworkPolicy"
if ! git -C "${REPO_ROOT}" cat-file -e "${LEGACY_REF}:charts/virtrigaud/values.yaml" 2>/dev/null; then
  echo "❌ cannot read ${LEGACY_REF}:charts/virtrigaud/values.yaml (fetch tags: git fetch --tags)" >&2
  exit 1
fi
cp -r "${CHART_DIR}" "${WORKDIR}/chart"
git -C "${REPO_ROOT}" show "${LEGACY_REF}:charts/virtrigaud/values.yaml" > "${WORKDIR}/chart/values.yaml"
helm template virtrigaud "${WORKDIR}/chart" --namespace "${NAMESPACE}" > "${WORKDIR}/legacy.yaml"
n="$(count_netpols "${WORKDIR}/legacy.yaml")"
if [[ "${n}" != "0" ]]; then
  echo "❌ --reuse-values from ${LEGACY_REF} would render ${n} NetworkPolicy object(s) (legacy security.networkPolicies.enabled leaked through)" >&2
  exit 1
fi

echo "==> 4. enabling policies on top of ${LEGACY_REF} defaults fails with a clear message"
if helm template virtrigaud "${WORKDIR}/chart" --namespace "${NAMESPACE}" \
  --set networkPolicy.enabled=true > /dev/null 2> "${WORKDIR}/legacy-enabled.err"; then
  echo "❌ networkPolicy.enabled=true over ${LEGACY_REF} defaults rendered without the missing-settings guard" >&2
  exit 1
fi
if ! grep -q 'reset-then-reuse-values' "${WORKDIR}/legacy-enabled.err"; then
  echo "❌ the missing-settings guard did not explain the --reset-then-reuse-values remedy:" >&2
  cat "${WORKDIR}/legacy-enabled.err" >&2
  exit 1
fi

echo "✅ NetworkPolicy render checks passed"
