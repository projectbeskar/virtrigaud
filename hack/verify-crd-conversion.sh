#!/bin/bash

# verify-crd-conversion.sh
# This script verifies that all CRDs in config/crd/bases have conversion webhook configuration

set -euo pipefail

echo "Verifying CRD conversion webhook configuration..."

# Generate the rendered CRDs with conversion patches
echo "Generating rendered CRDs with conversion webhooks..."
TEMP_CRD_FILE=$(mktemp)
cd config/crd && kustomize build . > "$TEMP_CRD_FILE" 2>/dev/null
cd - > /dev/null

if [[ ! -s "$TEMP_CRD_FILE" ]]; then
    echo "Error: Failed to generate CRDs with kustomize"
    rm -f "$TEMP_CRD_FILE"
    exit 1
fi

echo "Checking rendered CRDs for conversion webhook configuration..."
FAILED=0

# Split the multi-document YAML into individual CRDs for checking
csplit -s -z "$TEMP_CRD_FILE" '/^---$/' '{*}' 2>/dev/null || true

for crd_file in xx*; do
    if [[ ! -f "$crd_file" ]]; then
        continue
    fi
    
    echo "Checking $(basename "$crd_file")..."
    
    # Check if the CRD has conversion webhook configuration
    if ! grep -q "spec:" "$crd_file"; then
        echo "  Warning: No spec section found in $crd_file"
        continue
    fi
    
    if ! grep -q "conversion:" "$crd_file"; then
        echo "  ❌ Error: No conversion section found in $crd_file"
        FAILED=1
        continue
    fi
    
    if ! grep -q "strategy: Webhook" "$crd_file"; then
        echo "  ❌ Error: No webhook strategy found in $crd_file"
        FAILED=1
        continue
    fi
    
    if ! grep -q "path: /convert" "$crd_file"; then
        echo "  ❌ Error: No /convert path found in $crd_file"
        FAILED=1
        continue
    fi
    
    if ! grep -q "conversionReviewVersions:" "$crd_file"; then
        echo "  ❌ Error: No conversionReviewVersions found in $crd_file"
        FAILED=1
        continue
    fi
    
    # Check that both v1alpha1 and v1beta1 versions are present
    if ! grep -q "name: v1alpha1" "$crd_file"; then
        echo "  ❌ Error: No v1alpha1 version found in $crd_file"
        FAILED=1
        continue
    fi
    
    if ! grep -q "name: v1beta1" "$crd_file"; then
        echo "  ❌ Error: No v1beta1 version found in $crd_file"
        FAILED=1
        continue
    fi
    
    # Check storage version (look for storage: true after v1beta1 version section)
    if ! awk '/name: v1beta1/,/^  - name:/ { if (/storage: true/) found=1 } END { exit !found }' "$crd_file"; then
        echo "  ❌ Error: v1beta1 is not set as storage version in $crd_file"
        FAILED=1
        continue
    fi
    
    # Get the CRD name for better reporting
    crd_name=$(grep -o 'name: [a-z.-]*\.infra\.virtrigaud\.io' "$crd_file" 2>/dev/null | cut -d' ' -f2 || echo "unknown")
    echo "  ✅ $crd_name has valid conversion configuration"
done

# Clean up temporary files
rm -f "$TEMP_CRD_FILE" xx*

echo ""
if [[ $FAILED -eq 0 ]]; then
    echo "✅ All CRDs have valid conversion webhook configuration!"
    exit 0
else
    echo "❌ Some CRDs are missing valid conversion webhook configuration"
    echo ""
    echo "To fix this, ensure all CRDs include:"
    echo "  spec:"
    echo "    conversion:"
    echo "      strategy: Webhook"
    echo "      webhook:"
    echo "        clientConfig:"
    echo "          service:"
    echo "            name: virtrigaud-webhook"
    echo "            namespace: virtrigaud"
    echo "            path: /convert"
    echo "        conversionReviewVersions: [\"v1\"]"
    echo ""
    echo "Also ensure both v1alpha1 and v1beta1 versions are defined with v1beta1 as storage version."
    exit 1
fi
