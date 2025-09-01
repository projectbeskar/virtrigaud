#!/bin/bash

# Script to verify that all CRDs have only a single version and no conversion stanza
# Ensures v1beta1 is the only served and storage version

set -e

echo "Verifying single-version CRDs..."

# Check that all CRDs only have v1beta1 version
CRD_DIR="config/crd/bases"
if [ ! -d "$CRD_DIR" ]; then
    echo "ERROR: CRD directory $CRD_DIR not found"
    exit 1
fi

exit_code=0

for crd_file in "$CRD_DIR"/*.yaml; do
    echo "Checking $crd_file..."
    
    # Count number of versions in each CRD (look for patterns like "name: v1alpha1" or "name: v1beta1")
    version_count=$(grep -c "name: v[0-9]" "$crd_file" || true)
    
    if [ "$version_count" -ne 1 ]; then
        echo "ERROR: $crd_file has $version_count versions, expected 1"
        grep -A 3 -B 3 "name: v" "$crd_file" || true
        exit_code=1
        continue
    fi
    
    # Check that the single version is v1beta1
    if ! grep -q "name: v1beta1" "$crd_file"; then
        echo "ERROR: $crd_file does not have v1beta1 as its version"
        grep -A 3 -B 3 "name: v" "$crd_file" || true
        exit_code=1
        continue
    fi
    
    # Check that there's no conversion section
    if grep -q "conversion:" "$crd_file"; then
        echo "ERROR: $crd_file still has a conversion section"
        grep -A 10 -B 2 "conversion:" "$crd_file" || true
        exit_code=1
        continue
    fi
    
    # Check that the version is served and storage
    # Extract the section after "name: v1beta1" until the end of the version block
    served_check=$(sed -n '/name: v1beta1/,/^  - name:/p' "$crd_file" | grep "served: true" || echo "")
    storage_check=$(sed -n '/name: v1beta1/,/^  - name:/p' "$crd_file" | grep "storage: true" || echo "")
    
    # If there's no "^  - name:" (i.e., it's the last version), check until end
    if [ -z "$served_check" ]; then
        served_check=$(sed -n '/name: v1beta1/,$p' "$crd_file" | grep "served: true" || echo "")
    fi
    if [ -z "$storage_check" ]; then
        storage_check=$(sed -n '/name: v1beta1/,$p' "$crd_file" | grep "storage: true" || echo "")
    fi
    
    if [ -z "$served_check" ]; then
        echo "ERROR: v1beta1 is not served in $crd_file"
        exit_code=1
        continue
    fi
    
    if [ -z "$storage_check" ]; then
        echo "ERROR: v1beta1 is not storage version in $crd_file"
        exit_code=1
        continue
    fi
    
    echo "✅ $crd_file has single v1beta1 version (served: true, storage: true)"
done

if [ $exit_code -eq 0 ]; then
    echo ""
    echo "✅ All CRDs verified: single v1beta1 version with no conversion webhooks"
else
    echo ""
    echo "❌ CRD verification failed - some CRDs have multiple versions or conversion sections"
fi

exit $exit_code
