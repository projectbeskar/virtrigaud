#!/bin/bash
# test-conversion-no-skips.sh - Run conversion tests and fail if any are skipped

set -e

echo "Running conversion tests and checking for skips..."

# Run tests with verbose output and capture both stdout and stderr
TEST_OUTPUT=$(go test ./api/... -run ".*Conversion.*" -v 2>&1)
EXIT_CODE=$?

echo "$TEST_OUTPUT"

# Check if any round-trip conversion tests were skipped
SKIPPED_TESTS=$(echo "$TEST_OUTPUT" | grep -- "--- SKIP" | grep -i "roundtrip\|AlphaBetaAlpha\|BetaAlphaBeta" || true)

if [ -n "$SKIPPED_TESTS" ]; then
    echo ""
    echo "❌ ERROR: Conversion tests were skipped:"
    echo "$SKIPPED_TESTS"
    echo ""
    echo "All conversion tests must be implemented and passing."
    echo "Skipped conversion tests indicate missing or incomplete conversion implementations."
    exit 1
fi

# Check overall test exit code
if [ $EXIT_CODE -ne 0 ]; then
    echo ""
    echo "❌ ERROR: Conversion tests failed (exit code: $EXIT_CODE)"
    exit $EXIT_CODE
fi

echo ""
echo "✅ All conversion tests passed with no skips!"
