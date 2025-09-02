#!/bin/bash
# Development file watcher for virtrigaud
# Automatically rebuilds and reloads when Go files change

set -euo pipefail

# Configuration
WATCH_PATHS="${WATCH_PATHS:-cmd/ internal/ api/}"
DEBOUNCE_DELAY="${DEBOUNCE_DELAY:-2}"
EXCLUDE_PATTERNS="${EXCLUDE_PATTERNS:-*_test.go *.pb.go vendor/ .git/}"

# Colors
BLUE='\033[0;34m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log() {
    echo -e "${BLUE}[$(date +'%H:%M:%S')] $1${NC}"
}

success() {
    echo -e "${GREEN}✅ $1${NC}"
}

warn() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

# Check if fswatch is available
check_fswatch() {
    if ! command -v fswatch &> /dev/null; then
        echo "fswatch not found. Install it with:"
        echo "  macOS: brew install fswatch"
        echo "  Ubuntu: apt-get install fswatch"
        echo "  Or use alternative with inotify-tools"
        exit 1
    fi
}

# Alternative using inotifywait (Linux)
watch_with_inotify() {
    if ! command -v inotifywait &> /dev/null; then
        echo "inotifywait not found. Install with:"
        echo "  Ubuntu: apt-get install inotify-tools"
        echo "  RHEL/CentOS: yum install inotify-tools"
        exit 1
    fi
    
    log "Starting file watcher with inotifywait..."
    
    while true; do
        # Watch for modify events on Go files
        inotifywait -r -e modify --include='\.go$' $WATCH_PATHS 2>/dev/null || true
        
        log "Go files changed, triggering reload..."
        trigger_reload
        
        # Debounce: sleep to avoid multiple rapid rebuilds
        sleep $DEBOUNCE_DELAY
    done
}

# Watch using fswatch (macOS/cross-platform)
watch_with_fswatch() {
    log "Starting file watcher with fswatch..."
    
    # Build exclude arguments
    local exclude_args=""
    for pattern in $EXCLUDE_PATTERNS; do
        exclude_args="$exclude_args --exclude=$pattern"
    done
    
    fswatch -r $exclude_args --include='\.go$' $WATCH_PATHS | while read file; do
        log "File changed: $file"
        trigger_reload
        
        # Debounce: wait for more changes and drain the pipe
        sleep $DEBOUNCE_DELAY
        while read -t 0.1 file 2>/dev/null; do
            log "Draining: $file"
        done
    done
}

# Trigger the rebuild and reload
trigger_reload() {
    local start_time=$(date +%s)
    
    log "🔄 Triggering hot reload..."
    
    # Run lint check first
    if ! make lint-check >/dev/null 2>&1; then
        warn "Lint check failed, skipping reload"
        return 1
    fi
    
    # Trigger the reload
    if make dev-reload; then
        local end_time=$(date +%s)
        local duration=$((end_time - start_time))
        success "Hot reload completed in ${duration}s"
    else
        warn "Hot reload failed"
        return 1
    fi
}

# Cleanup on exit
cleanup() {
    log "Stopping file watcher..."
    exit 0
}

trap cleanup INT TERM

# Main function
main() {
    log "🔍 virtrigaud Development File Watcher"
    log "Watching paths: $WATCH_PATHS"
    log "Exclude patterns: $EXCLUDE_PATTERNS"
    log "Debounce delay: ${DEBOUNCE_DELAY}s"
    echo
    log "Press Ctrl+C to stop"
    echo
    
    # Check if deployment exists
    if ! kubectl get deployment virtrigaud-controller-manager -n virtrigaud-system &>/dev/null; then
        warn "virtrigaud not deployed. Run 'make dev-deploy' first."
        exit 1
    fi
    
    success "virtrigaud deployment found, starting file watcher..."
    
    # Choose watcher based on available tools
    if command -v fswatch &> /dev/null; then
        watch_with_fswatch
    elif command -v inotifywait &> /dev/null; then
        watch_with_inotify
    else
        echo "No file watcher available. Install fswatch or inotify-tools."
        exit 1
    fi
}

# Handle arguments
case "${1:-watch}" in
    "watch")
        main
        ;;
    "test")
        log "Testing reload trigger..."
        trigger_reload
        ;;
    *)
        echo "Usage: $0 {watch|test}"
        echo "  watch  - Start file watcher (default)"
        echo "  test   - Test reload trigger once"
        echo
        echo "Environment variables:"
        echo "  WATCH_PATHS       - Paths to watch (default: cmd/ internal/ api/)"
        echo "  DEBOUNCE_DELAY    - Delay between changes (default: 2s)"
        echo "  EXCLUDE_PATTERNS  - Files to exclude (default: *_test.go *.pb.go vendor/ .git/)"
        exit 1
        ;;
esac
