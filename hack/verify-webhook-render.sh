#!/usr/bin/env bash
# verify-webhook-render.sh — CI regression guard for `webhooks.enabled=true`.
#
# Renders the Helm chart with the admission webhook turned on and asserts the
# rendered manifests are valid AND internally coherent. This is the check that
# would have caught the class of bugs fixed in fix/webhook-chart-enablement:
#
#   1. CA/caBundle incoherence — the ValidatingWebhookConfiguration caBundle MUST
#      be the exact CA that signed the serving cert in the mounted Secret, or
#      failurePolicy:Fail bricks all Provider admission.
#   2. Manager <-> chart flag mismatch — the Deployment must pass the real
#      --webhook-cert-path flag and no flags the manager binary does not define.
#   3. Unbacked mutating/conversion blocks — only ValidatingWebhookConfiguration
#      is backed by Go; no *AdmissionWebhookConfiguration (invalid kind) and no
#      MutatingWebhookConfiguration may be rendered.
#   4. Serving cert SANs must include the webhook Service DNS names.
#
# Requires: helm, python3 (with PyYAML), openssl. Renders only (no cluster).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/virtrigaud"
NAMESPACE="virtrigaud-system"

for bin in helm python3 openssl; do
  if ! command -v "${bin}" >/dev/null 2>&1; then
    echo "❌ required tool not found: ${bin}" >&2
    exit 1
  fi
done

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT
RENDERED="${WORKDIR}/rendered.yaml"

echo "==> helm template ${CHART_DIR} --set webhooks.enabled=true"
helm template virtrigaud "${CHART_DIR}" \
  --namespace "${NAMESPACE}" \
  --set webhooks.enabled=true \
  >"${RENDERED}"

python3 - "${RENDERED}" "${NAMESPACE}" <<'PY'
import base64, os, subprocess, sys, tempfile
import yaml

rendered_path, namespace = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(open(rendered_path)) if d]
failures = []

def check(cond, msg):
    print(("  ✅ " if cond else "  ❌ ") + msg)
    if not cond:
        failures.append(msg)

kinds = [d.get("kind") for d in docs]

print("== 1. Only valid webhook kinds are rendered ==")
bad_kinds = [k for k in kinds if k and k.endswith("AdmissionWebhookConfiguration")]
check(not bad_kinds, "no invalid *AdmissionWebhookConfiguration kinds (found: %s)" % bad_kinds)
check("MutatingWebhookConfiguration" not in kinds,
      "no MutatingWebhookConfiguration (no Go-backed mutating webhook exists)")
check(kinds.count("ValidatingWebhookConfiguration") == 1,
      "exactly one ValidatingWebhookConfiguration")
# The conversion block used to render a second tls Secret named *-conversion-certs.
conv = [d for d in docs if d.get("kind") == "Secret" and d["metadata"]["name"].endswith("-conversion-certs")]
check(not conv, "no -conversion-certs Secret (conversion block removed)")

print("== 2. Manager wired with the real --webhook-cert-path and no undefined flags ==")
dep = next(d for d in docs if d.get("kind") == "Deployment" and d["metadata"]["name"].endswith("-manager"))
args = dep["spec"]["template"]["spec"]["containers"][0].get("args", [])
check(any(a.startswith("--webhook-cert-path=") for a in args),
      "manager args contain --webhook-cert-path")
undefined = [a for a in args if a.startswith("--webhook-port") or a.startswith("--webhook-cert-dir")]
check(not undefined, "manager args contain no undefined webhook flags (found: %s)" % undefined)

print("== 3. CA / caBundle coherence (the load-bearing invariant) ==")
vwc = next(d for d in docs if d.get("kind") == "ValidatingWebhookConfiguration")
hook = vwc["webhooks"][0]
cab = hook["clientConfig"].get("caBundle", "")
check(bool(cab), "ValidatingWebhookConfiguration caBundle is non-empty")
secret = next((d for d in docs if d.get("kind") == "Secret"
               and d.get("type") == "kubernetes.io/tls"
               and d["metadata"]["name"].endswith("-webhook-certs")), None)
check(secret is not None, "self-signed serving Secret is rendered")

svc = next(d for d in docs if d.get("kind") == "Service"
           and d["metadata"]["name"].endswith("-webhook"))
svc_name = svc["metadata"]["name"]

if secret is not None:
    ca_crt = secret["data"]["ca.crt"]
    tls_crt = secret["data"]["tls.crt"]
    check(cab == ca_crt,
          "caBundle EQUALS the serving Secret's ca.crt (caBundle signs the serving cert)")

    def write_pem(b64):
        f = tempfile.NamedTemporaryFile(delete=False, suffix=".pem", dir=os.path.dirname(rendered_path))
        f.write(base64.b64decode(b64)); f.close(); return f.name

    ca_pem = write_pem(ca_crt)
    crt_pem = write_pem(tls_crt)

    # The serving cert must be signed by the CA whose cert is in caBundle.
    v = subprocess.run(["openssl", "verify", "-CAfile", ca_pem, crt_pem],
                       capture_output=True, text=True)
    check(v.returncode == 0,
          "openssl verifies the serving cert against the caBundle CA (%s)" % (v.stdout + v.stderr).strip())

    print("== 4. Serving cert SANs include the webhook Service DNS ==")
    san = subprocess.run(["openssl", "x509", "-in", crt_pem, "-noout", "-ext", "subjectAltName"],
                         capture_output=True, text=True).stdout
    for dns in ("%s.%s.svc" % (svc_name, namespace),
                "%s.%s.svc.cluster.local" % (svc_name, namespace)):
        check(("DNS:" + dns) in san, "SAN includes DNS:%s" % dns)

print()
if failures:
    print("❌ webhook render check FAILED (%d assertion(s)):" % len(failures))
    for f in failures:
        print("   - " + f)
    sys.exit(1)
print("✅ positive render (self-signed, webhooks.enabled=true) is valid and coherent")
PY

# --- Negative cases: misconfigurations MUST fail the render (fail-fast guards) ---
echo
echo "== 5. Misconfiguration guards fail the render (fail-fast) =="
neg_fail=0

if helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" \
     --set webhooks.enabled=true --set webhooks.certificates.source=bogus >/dev/null 2>&1; then
  echo "  ❌ an invalid certificates.source rendered successfully (should fail)"; neg_fail=1
else
  echo "  ✅ invalid certificates.source is rejected at render"
fi

if helm template virtrigaud "${CHART_DIR}" --namespace "${NAMESPACE}" \
     --set webhooks.enabled=true --set webhooks.certificates.source=manual >/dev/null 2>&1; then
  echo "  ❌ source=manual with no caBundle rendered successfully (should fail)"; neg_fail=1
else
  echo "  ✅ source=manual with no caBundle is rejected at render"
fi

if [ "${neg_fail}" -ne 0 ]; then
  echo "❌ webhook render check FAILED (a misconfiguration guard did not fire)"; exit 1
fi

echo
echo "✅ webhook render check PASSED — valid + coherent when enabled, misconfigurations fail fast"

