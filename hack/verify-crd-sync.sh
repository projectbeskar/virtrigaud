#!/bin/bash
# Verify that CRD YAML files are in sync with Go type definitions
# This script is used in CI to ensure developers ran `make generate manifests`

set -e

echo "🔍 Verifying CRD synchronization..."

# Store original state
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

echo "  1. Saving current state..."
cp -r config/crd/bases "$TMPDIR/crd-before"
cp -r api/infra.virtrigaud.io/v1beta1/**/zz_generated.deepcopy.go "$TMPDIR/deepcopy-before" 2>/dev/null || true
cp -r charts/virtrigaud/crds "$TMPDIR/helm-crds-before"

echo "  2. Regenerating CRDs and manifests..."
make generate manifests sync-helm-crds >/dev/null 2>&1

echo "  3. Comparing with current state..."

# Check if CRD YAMLs changed
if ! diff -r "$TMPDIR/crd-before" config/crd/bases/ > /dev/null 2>&1; then
    echo "❌ CRD YAML files are out of sync with Go types!"
    echo ""
    echo "The following files have differences:"
    diff -r "$TMPDIR/crd-before" config/crd/bases/ || true
    echo ""
    echo "To fix this, run:"
    echo "  make generate manifests sync-helm-crds"
    echo ""
    exit 1
fi

# Check if DeepCopy files changed
if [ -d "$TMPDIR/deepcopy-before" ]; then
    if ! diff -r "$TMPDIR/deepcopy-before" api/infra.virtrigaud.io/v1beta1/ > /dev/null 2>&1; then
        echo "❌ DeepCopy methods are out of sync!"
        echo ""
        echo "To fix this, run:"
        echo "  make generate"
        echo ""
        exit 1
    fi
fi

# Check if Helm CRDs changed
if ! diff -r "$TMPDIR/helm-crds-before" charts/virtrigaud/crds/ > /dev/null 2>&1; then
    echo "❌ Helm chart CRDs are out of sync!"
    echo ""
    echo "To fix this, run:"
    echo "  make sync-helm-crds"
    echo ""
    exit 1
fi

echo "✅ CRDs are in sync with Go type definitions!"
