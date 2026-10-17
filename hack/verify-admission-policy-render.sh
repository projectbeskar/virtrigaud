#!/usr/bin/env bash
# verify-admission-policy-render.sh — CI regression guard for the chart's
# reserved-keys ValidatingAdmissionPolicy (docs/admission-policy.md).
#
# It asserts, by rendering only (no cluster):
#
#   1. Default values render the policy and its binding (Kubernetes 1.30+):
#      failurePolicy Fail, the binding names the policy and Denies, and the
#      manager exemption is the chart's own ServiceAccount.
#   2. admissionPolicy.reservedKeys.enabled=false renders nothing.
#   3. A Kubernetes < 1.30 target renders nothing, unless it reports the
#      admissionregistration.k8s.io/v1 ValidatingAdmissionPolicy API.
#   4. manager.serviceAccount.name, exemptUsernames and exemptGroups reach the
#      match conditions (the group condition in the exact form the envtest
#      exercises).
#   5. (On every render that produces the policy.) The chart's CEL (match
#      constraints, variables, validations) is identical to
#      config/admission-policy/reserved_keys_policy.yaml, whose manager
#      identity is the kustomize ServiceAccount (config/default namespace and
#      namePrefix + config/rbac/service_account.yaml).
#   6. `helm upgrade --reuse-values` from ${LEGACY_REF} (values without an
#      admissionPolicy block) still renders, with the policy on.
#
# Requires: helm, python3 (with PyYAML), git. Renders only.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/virtrigaud"
NAMESPACE="virtrigaud-system"
LEGACY_REF="${LEGACY_REF:-v0.3.11}"
VAP_API="admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy"

for bin in helm python3 git; do
  if ! command -v "${bin}" >/dev/null 2>&1; then
    echo "❌ required tool not found: ${bin}" >&2
    exit 1
  fi
done

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# check FILE WANT_COUNT [EXPECTED_USERS_JSON] [EXPECTED_GROUPS_JSON]
check() {
  python3 - "$@" "${REPO_ROOT}" <<'PY'
import json, os, re, sys, yaml
path, want = sys.argv[1], int(sys.argv[2])
users = json.loads(sys.argv[3]) if len(sys.argv) > 4 else None
groups = json.loads(sys.argv[4]) if len(sys.argv) > 5 else None
root = sys.argv[-1]
docs = [d for d in yaml.safe_load_all(open(path)) if d]
pols = [d for d in docs if d.get("kind") == "ValidatingAdmissionPolicy"]
binds = [d for d in docs if d.get("kind") == "ValidatingAdmissionPolicyBinding"]
if len(pols) != want or len(binds) != want:
    sys.exit(f"❌ expected {want} policy/binding, got {len(pols)}/{len(binds)}")
if want == 0:
    print("   ok: nothing rendered")
    sys.exit(0)
p, b = pols[0], binds[0]
if p["spec"].get("failurePolicy") != "Fail":
    sys.exit("❌ failurePolicy must be Fail")
if b["spec"].get("policyName") != p["metadata"]["name"] or b["spec"].get("validationActions") != ["Deny"]:
    sys.exit("❌ the binding must name the policy and Deny")
conds = {c["name"]: c["expression"] for c in p["spec"].get("matchConditions", [])}
got_users = re.findall(r'"([^"]+)"', conds.get("not-the-virtrigaud-manager", ""))
if got_users != users:
    sys.exit(f"❌ exempt usernames {got_users}, want {users}")
if groups:
    want_expr = ("!(has(request.userInfo.groups) && request.userInfo.groups.exists(g, g in [" +
                 ", ".join(json.dumps(g) for g in groups) + "]))")
    if conds.get("not-an-exempt-group") != want_expr:
        sys.exit(f"❌ exempt-group condition {conds.get('not-an-exempt-group')!r}, want {want_expr!r}")
elif "not-an-exempt-group" in conds:
    sys.exit("❌ an exempt-group condition rendered without exemptGroups")

# The chart's CEL must equal the kustomize copy's.
kdocs = [d for d in yaml.safe_load_all(open(os.path.join(root, "config/admission-policy/reserved_keys_policy.yaml"))) if d]
kp = [d for d in kdocs if d["kind"] == "ValidatingAdmissionPolicy"][0]
kb = [d for d in kdocs if d["kind"] == "ValidatingAdmissionPolicyBinding"][0]
for field in ("failurePolicy", "matchConstraints", "variables", "validations"):
    if p["spec"].get(field) != kp["spec"].get(field):
        sys.exit(f"❌ spec.{field} differs between the chart and config/admission-policy/reserved_keys_policy.yaml")
if b["spec"]["validationActions"] != kb["spec"]["validationActions"]:
    sys.exit("❌ the binding's validationActions differ from the kustomize copy")
kconds = kp["spec"]["matchConditions"]
if len(kconds) != 1 or kconds[0]["name"] != "not-the-virtrigaud-manager":
    sys.exit("❌ the kustomize copy must exempt exactly the manager")
kdef = yaml.safe_load(open(os.path.join(root, "config/default/kustomization.yaml")))
sa = yaml.safe_load(open(os.path.join(root, "config/rbac/service_account.yaml")))
kuser = f"system:serviceaccount:{kdef['namespace']}:{kdef.get('namePrefix', '')}{sa['metadata']['name']}"
pieces = kconds[0]["expression"].split("'")
if "".join(re.findall(r"'([^']*)'", kconds[0]["expression"])) != kuser or len(pieces) != 9:
    sys.exit(f"❌ the kustomize copy must exempt {kuser}, written as 'system:serviceaccount:' + '<ns>' + ':' + '<name>'")
if "../admission-policy" not in kdef.get("resources", []):
    sys.exit("❌ config/default does not include ../admission-policy")
# config/default fills <ns> (index 3) and <name> (index 7) from the manager's
# ServiceAccount after its namespace and namePrefix are applied.
want = {("metadata.namespace", 3), ("metadata.name", 7)}
got = set()
for rep in kdef.get("replacements") or []:
    src = rep.get("source", {})
    if src.get("kind") != "ServiceAccount" or src.get("name") != sa["metadata"]["name"]:
        continue
    for tgt in rep.get("targets", []):
        sel = tgt.get("select", {})
        opts = tgt.get("options", {})
        if (sel.get("kind") == "ValidatingAdmissionPolicy" and sel.get("name") == kp["metadata"]["name"]
                and opts.get("delimiter") == "'"
                and "spec.matchConditions.[name=not-the-virtrigaud-manager].expression" in tgt.get("fieldPaths", [])):
            got.add((src.get("fieldPath"), opts.get("index")))
if got != want:
    sys.exit(f"❌ config/default must fill the policy's manager identity from the ServiceAccount (replacements {sorted(want)}), got {sorted(got)}")
print(f"   ok: policy {p['metadata']['name']}, exempt users {got_users}, groups {groups or []}, CEL in sync with config/")
PY
}

echo "==> 1. default values render the policy (Kubernetes 1.30+)"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --kube-version 1.30.0 > "${WORKDIR}/default.yaml"
check "${WORKDIR}/default.yaml" 1 "[\"system:serviceaccount:${NAMESPACE}:virtrigaud\"]"

echo "==> 2. enabled=false renders nothing"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --kube-version 1.30.0 \
  --set admissionPolicy.reservedKeys.enabled=false > "${WORKDIR}/disabled.yaml"
check "${WORKDIR}/disabled.yaml" 0

echo "==> 3. Kubernetes < 1.30 renders nothing unless the API is reported"
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --kube-version 1.29.4 > "${WORKDIR}/old.yaml"
check "${WORKDIR}/old.yaml" 0
helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --kube-version 1.29.4 \
  --api-versions "${VAP_API}" > "${WORKDIR}/old-with-api.yaml"
check "${WORKDIR}/old-with-api.yaml" 1 "[\"system:serviceaccount:${NAMESPACE}:virtrigaud\"]"

echo "==> 4. ServiceAccount name and exemptions reach the match conditions"
helm template virtrigaud "${CHART_DIR}" --namespace tenant-ops --kube-version 1.30.0 \
  --set manager.serviceAccount.name=vr-manager \
  --set 'admissionPolicy.reservedKeys.exemptUsernames[0]=system:serviceaccount:velero:velero' \
  --set 'admissionPolicy.reservedKeys.exemptGroups[0]=vr-break-glass' > "${WORKDIR}/exempt.yaml"
check "${WORKDIR}/exempt.yaml" 1 \
  '["system:serviceaccount:tenant-ops:vr-manager", "system:serviceaccount:velero:velero"]' '["vr-break-glass"]'

echo "==> 4b. an exemption that would break the CEL fails the render"
for bad in "exemptUsernames[0]=o'brien" "exemptGroups[0]=a\"b" "exemptUsernames[0]=x]) || true || (["; do
  if helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" --kube-version 1.30.0 \
    --set-string "admissionPolicy.reservedKeys.${bad}" > /dev/null 2> "${WORKDIR}/bad.err"; then
    echo "❌ admissionPolicy.reservedKeys.${bad} rendered; it must fail" >&2
    exit 1
  fi
  if ! grep -q "would make the reserved-keys policy deny every VirtualMachine write" "${WORKDIR}/bad.err"; then
    echo "❌ admissionPolicy.reservedKeys.${bad} failed without the guard's message:" >&2
    cat "${WORKDIR}/bad.err" >&2
    exit 1
  fi
done
echo "   ok: malformed exemptions are refused at render"

echo "==> 5b. kustomize: the manager identity follows config/default's namespace and namePrefix"
KUSTOMIZE_BIN="${KUSTOMIZE:-$(command -v kustomize || true)}"
if [[ -z "${KUSTOMIZE_BIN}" && -x "${REPO_ROOT}/bin/kustomize" ]]; then
  KUSTOMIZE_BIN="${REPO_ROOT}/bin/kustomize"
fi
if [[ -z "${KUSTOMIZE_BIN}" ]]; then
  echo "   skipped: kustomize not found (set KUSTOMIZE or run make kustomize)"
else
  mkdir -p "${REPO_ROOT}/config/.verify-admission-policy"
  trap 'rm -rf "${WORKDIR}" "${REPO_ROOT}/config/.verify-admission-policy"' EXIT
  cp "${REPO_ROOT}"/config/default/*.yaml "${REPO_ROOT}/config/.verify-admission-policy/"
  sed -i 's/^namespace: .*/namespace: verify-ns/; s/^namePrefix: .*/namePrefix: verify-/' \
    "${REPO_ROOT}/config/.verify-admission-policy/kustomization.yaml"
  "${KUSTOMIZE_BIN}" build "${REPO_ROOT}/config/.verify-admission-policy" > "${WORKDIR}/kustomize.yaml"
  python3 - "${WORKDIR}/kustomize.yaml" <<'PY'
import re, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
p = [d for d in docs if d.get("kind") == "ValidatingAdmissionPolicy"][0]
sa = [d for d in docs if d.get("kind") == "ServiceAccount" and d["metadata"]["name"].endswith("controller-manager")][0]
want = f"system:serviceaccount:{sa['metadata']['namespace']}:{sa['metadata']['name']}"
expr = [c for c in p["spec"]["matchConditions"] if c["name"] == "not-the-virtrigaud-manager"][0]["expression"]
got = "".join(re.findall(r"'([^']*)'", expr))
if got != want or "verify-ns" not in want:
    sys.exit(f"❌ kustomize build exempts {got!r}, want {want!r}")
print(f"   ok: kustomize exempts {got}")
PY
fi

echo "==> 6. --reuse-values from ${LEGACY_REF} still renders, with the policy on"
if ! git -C "${REPO_ROOT}" cat-file -e "${LEGACY_REF}:charts/virtrigaud/values.yaml" 2>/dev/null; then
  echo "❌ cannot read ${LEGACY_REF}:charts/virtrigaud/values.yaml (fetch tags: git fetch --tags)" >&2
  exit 1
fi
cp -r "${CHART_DIR}" "${WORKDIR}/chart"
git -C "${REPO_ROOT}" show "${LEGACY_REF}:charts/virtrigaud/values.yaml" > "${WORKDIR}/chart/values.yaml"
helm template virtrigaud "${WORKDIR}/chart" --namespace "${NAMESPACE}" --kube-version 1.30.0 > "${WORKDIR}/legacy.yaml"
check "${WORKDIR}/legacy.yaml" 1 "[\"system:serviceaccount:${NAMESPACE}:virtrigaud\"]"

echo "✅ reserved-keys admission policy render checks passed"
