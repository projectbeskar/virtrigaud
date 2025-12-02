#!/bin/bash
# Setup git hooks for virtrigaud development
# Run this script to install pre-commit hooks that automatically regenerate CRDs

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GIT_HOOKS_DIR="$(git rev-parse --git-dir)/hooks"

echo "🔧 Setting up git hooks for VirtRigaud..."

# Check if .git directory exists
if [ ! -d "$GIT_HOOKS_DIR" ]; then
    echo "❌ Error: Not a git repository or .git/hooks directory not found"
    exit 1
fi

# Install pre-commit hook
echo "📝 Installing pre-commit hook..."
if [ -f "$GIT_HOOKS_DIR/pre-commit" ] && [ ! -L "$GIT_HOOKS_DIR/pre-commit" ]; then
    echo "⚠️  Existing pre-commit hook found (not a symlink)"
    read -p "   Replace with VirtRigaud hook? (y/N) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "❌ Aborted - keeping existing hook"
        exit 1
    fi
    rm "$GIT_HOOKS_DIR/pre-commit"
fi

# Create symlink to pre-commit hook
ln -sf "$SCRIPT_DIR/pre-commit" "$GIT_HOOKS_DIR/pre-commit"
chmod +x "$SCRIPT_DIR/pre-commit"

echo "✅ Git hooks installed successfully!"
echo ""
echo "The pre-commit hook will automatically:"
echo "  • Regenerate CRD YAMLs when *_types.go files change"
echo "  • Generate DeepCopy methods"
echo "  • Sync CRDs to Helm chart"
echo "  • Format Go code"
echo "  • Run linter checks"
echo ""
echo "To skip the hook (not recommended), use: git commit --no-verify"
echo ""
echo "To uninstall, run: rm $GIT_HOOKS_DIR/pre-commit"
