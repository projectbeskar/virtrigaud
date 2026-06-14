#!/bin/bash

# Verify that every generated CRD base in config/crd/bases/ is referenced in
# config/crd/kustomization.yaml (and vice-versa).
#
# Why this exists:
#   `make gen-crds` (controller-gen) writes one file per CRD into
#   config/crd/bases/, but it does NOT touch the kustomize `resources:` list.
#   That list is only appended to by `kubebuilder create api` at the
#   `# +kubebuilder:scaffold:crdkustomizeresource` marker. If a new CRD is added
#   without updating kustomization.yaml, `kustomize build config/default`
#   silently ships fewer CRDs than exist (so `make install`, `make deploy` and
#   `make build-installer`/dist/install.yaml are all incomplete), and any
#   controller for the missing kind crash-loops the manager at startup with
#   `no matches for kind "..."`. The Helm path is unaffected because
#   `make gen-helm-crds` runs controller-gen directly over ./api, bypassing
#   this list — which is exactly why such drift can go unnoticed.

set -e

CRD_DIR="config/crd/bases"
KUSTOMIZATION="config/crd/kustomization.yaml"

if [ ! -d "$CRD_DIR" ]; then
    echo "ERROR: CRD directory $CRD_DIR not found"
    exit 1
fi
if [ ! -f "$KUSTOMIZATION" ]; then
    echo "ERROR: $KUSTOMIZATION not found"
    exit 1
fi

echo "Verifying CRD bases are in sync with $KUSTOMIZATION..."

exit_code=0

# 1) Every base file on disk must be referenced in the resources list.
for crd_file in "$CRD_DIR"/*.yaml; do
    base_name=$(basename "$crd_file")
    if grep -qF -- "bases/${base_name}" "$KUSTOMIZATION"; then
        echo "✅ ${base_name} is referenced"
    else
        echo "❌ ${base_name} exists but is NOT referenced in $KUSTOMIZATION"
        exit_code=1
    fi
done

# 2) Every referenced base must exist on disk (catch stale/renamed/typo entries).
while IFS= read -r ref; do
    if [ ! -f "$CRD_DIR/$ref" ]; then
        echo "❌ $KUSTOMIZATION references bases/$ref but $CRD_DIR/$ref does not exist"
        exit_code=1
    fi
done < <(grep -oE 'bases/[A-Za-z0-9._-]+\.yaml' "$KUSTOMIZATION" | sed 's#bases/##' | sort -u)

if [ $exit_code -eq 0 ]; then
    echo ""
    echo "✅ All $(ls -1 "$CRD_DIR"/*.yaml | wc -l | tr -d ' ') CRD bases are referenced in kustomization.yaml"
else
    echo ""
    echo "❌ CRD kustomization is out of sync."
    echo "   For each missing CRD, add a line under 'resources:' in $KUSTOMIZATION"
    echo "   (before the '# +kubebuilder:scaffold:crdkustomizeresource' marker):"
    echo "     - bases/<group>_<plural>.yaml"
fi

exit $exit_code
