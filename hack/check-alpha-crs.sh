#!/bin/bash

# Script to check for existing v1alpha1 custom resources
# Exits non-zero if any v1alpha1 objects are found

set -e

# Function to check for v1alpha1 objects of a specific kind
check_alpha_objects() {
    local kind=$1
    echo "Checking for v1alpha1 $kind objects..."
    
    # Get all objects of this kind across all namespaces and check for v1alpha1 apiVersion
    if kubectl get "$kind".infra.virtrigaud.io -A -o yaml 2>/dev/null | grep -q "apiVersion: .*v1alpha1"; then
        echo "ERROR: Found v1alpha1 $kind objects:"
        kubectl get "$kind".infra.virtrigaud.io -A -o yaml | grep -A 5 -B 5 "apiVersion: .*v1alpha1"
        return 1
    fi
    return 0
}

# Check if kubectl is available
if ! command -v kubectl &> /dev/null; then
    echo "ERROR: kubectl is not available"
    exit 1
fi

# Check if we can connect to the cluster
if ! kubectl cluster-info &> /dev/null; then
    echo "ERROR: Cannot connect to Kubernetes cluster"
    exit 1
fi

echo "Checking for virtrigaud v1alpha1 custom resources..."

# Get all CRDs for virtrigaud
crds=$(kubectl get crd | grep infra.virtrigaud.io | awk '{print $1}' || true)

if [ -z "$crds" ]; then
    echo "No virtrigaud CRDs found in cluster"
    exit 0
fi

echo "Found virtrigaud CRDs: $crds"

exit_code=0

# Check each CRD kind for v1alpha1 objects
for crd in $crds; do
    # Extract the kind from CRD name (e.g., virtualmachines.infra.virtrigaud.io -> virtualmachines)
    kind=$(echo "$crd" | cut -d'.' -f1)
    
    if ! check_alpha_objects "$kind"; then
        exit_code=1
    fi
done

if [ $exit_code -eq 0 ]; then
    echo "✅ No v1alpha1 objects found - safe to proceed with v1alpha1 removal"
else
    echo "❌ Found v1alpha1 objects - migration required before removing v1alpha1 support"
    echo ""
    echo "To migrate v1alpha1 objects to v1beta1:"
    echo "1. Use the alpha-to-beta-dryrun tool to preview conversions"
    echo "2. Apply the converted manifests"
    echo "3. Re-run this check"
fi

exit $exit_code
