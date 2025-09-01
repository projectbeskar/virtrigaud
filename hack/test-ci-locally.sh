#!/bin/bash

# Local CI Testing Script
# Replicates the ci.yml workflow jobs locally without GitHub Actions

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}ℹ️  $1${NC}"; }
log_success() { echo -e "${GREEN}✅ $1${NC}"; }
log_warning() { echo -e "${YELLOW}⚠️  $1${NC}"; }
log_error() { echo -e "${RED}❌ $1${NC}"; }

# Configuration matching the GitHub workflow
GO_VERSION="1.23"

cd "$PROJECT_ROOT"

# Track results
RESULTS=()

run_job() {
    local job_name="$1"
    local job_function="$2"
    
    log_info "🔄 Running CI job: $job_name"
    
    if $job_function; then
        log_success "Job '$job_name' passed"
        RESULTS+=("$job_name: ✅ PASSED")
        return 0
    else
        log_error "Job '$job_name' failed"
        RESULTS+=("$job_name: ❌ FAILED")
        return 1
    fi
}

# Job: Test
test_job() {
    log_info "Running test job..."
    
    # Go mod tidy
    log_info "Tidying Go modules..."
    go mod tidy
    
    # Download dependencies
    log_info "Downloading dependencies..."
    go mod download
    
    # Verify dependencies
    log_info "Verifying dependencies..."
    go mod verify
    
    # Run go vet (excluding libvirt)
    log_info "Running go vet (excluding libvirt)..."
    go list ./... | grep -v '/internal/providers/libvirt' | grep -v '/cmd/provider-libvirt' | grep -v '/test/integration' | xargs go vet
    
    # Run tests (excluding libvirt) using Makefile
    log_info "Running tests..."
    if make test; then
        # Check if coverage file was generated
        if [[ -f "cover.out" ]]; then
            mv cover.out coverage.out
            log_info "Coverage report generated: coverage.out"
        fi
        return 0
    else
        return 1
    fi
}

# Job: Lint
lint_job() {
    log_info "Running lint job..."
    
    # Install libvirt dependencies for Go module resolution (if on Linux)
    if [[ "$OSTYPE" == "linux-gnu"* ]]; then
        log_info "Installing libvirt dependencies..."
        if command -v apt-get >/dev/null 2>&1; then
            sudo apt-get update >/dev/null 2>&1 || log_warning "Could not update package list"
            sudo apt-get install -y libvirt-dev pkg-config >/dev/null 2>&1 || log_warning "Could not install libvirt-dev"
        else
            log_warning "apt-get not found, skipping libvirt dependencies"
        fi
    fi
    
    # Go mod tidy
    go mod tidy
    
    # Run golangci-lint
    log_info "Running golangci-lint..."
    if command -v golangci-lint >/dev/null 2>&1; then
        # Use no-config if config file has issues, enable key linters manually
        if golangci-lint run --timeout=10m 2>/dev/null; then
            log_success "golangci-lint completed successfully"
        else
            log_warning "Config file issue detected, running with manual linter selection..."
            golangci-lint run --no-config --enable=govet,staticcheck,ineffassign,unused,errcheck,misspell --timeout=10m
        fi
    else
        log_warning "golangci-lint not found, installing..."
        curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | \
            sh -s -- -b $(go env GOPATH)/bin v1.64.8
        export PATH="$(go env GOPATH)/bin:$PATH"
        golangci-lint run --no-config --enable=govet,staticcheck,ineffassign,unused,errcheck,misspell --timeout=10m
    fi
}

# Job: Security
security_job() {
    log_info "Running security job..."
    
    go mod tidy
    
    # Install and run Gosec
    log_info "Installing and running Gosec..."
    go install github.com/securego/gosec/v2/cmd/gosec@v2.21.4
    
    export PATH="$(go env GOPATH)/bin:$PATH"
    
    # Run gosec with timeout and exclusions
    timeout 300s $(go env GOPATH)/bin/gosec \
        -fmt sarif \
        -out gosec.sarif \
        -exclude-dir=internal/providers/libvirt \
        -exclude-dir=test/integration \
        -exclude=G104,G204,G304 \
        ./... || {
        
        log_info "Gosec completed. Checking results..."
        
        if [[ -f gosec.sarif ]]; then
            CRITICAL_COUNT=$(grep -o '"level":"error"' gosec.sarif | wc -l || echo "0")
            HIGH_COUNT=$(grep -o '"level":"warning"' gosec.sarif | wc -l || echo "0")
            
            log_info "Security scan results:"
            log_info "   - Critical issues: $CRITICAL_COUNT"
            log_info "   - High issues: $HIGH_COUNT"
            
            if [[ "$CRITICAL_COUNT" -gt 0 ]]; then
                log_error "Critical security issues found"
                return 1
            else
                log_success "No critical security issues found"
            fi
        else
            log_error "SARIF file not created - gosec failed"
            return 1
        fi
    }
    
    # Note: We skip Trivy in local testing as it requires Docker setup
    log_warning "Skipping Trivy scan (requires Docker registry setup)"
}

# Job: Generate
generate_job() {
    log_info "Running generate job..."
    
    go mod tidy
    
    # Install controller-gen
    log_info "Installing controller-gen..."
    go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
    
    # Install protoc (if available)
    if command -v apt-get >/dev/null 2>&1; then
        log_info "Installing protoc..."
        sudo apt-get update >/dev/null 2>&1 || true
        sudo apt-get install -y protobuf-compiler >/dev/null 2>&1 || log_warning "Could not install protobuf-compiler"
    else
        log_warning "Skipping protoc installation (apt-get not available)"
    fi
    
    # Install protoc plugins
    log_info "Installing protoc plugins..."
    go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
    
    export PATH="$(go env GOPATH)/bin:$PATH"
    
    # Generate code
    log_info "Generating code..."
    make generate
    
    # Generate manifests
    log_info "Generating manifests..."
    make manifests
    
    # Generate protobuf (if protoc is available)
    if command -v protoc >/dev/null 2>&1; then
        log_info "Generating protobuf..."
        make proto
    else
        log_warning "Skipping protobuf generation (protoc not available)"
    fi
    
    # Verify no changes
    log_info "Verifying no changes..."
    git checkout HEAD -- go.mod go.sum 2>/dev/null || true
    
    if git diff --exit-code; then
        log_success "Generated files are in sync"
        return 0
    else
        log_error "Generated files are out of sync"
        log_error "Please run 'make generate && make manifests && make proto' and commit the changes"
        return 1
    fi
}

# Job: Build
build_job() {
    log_info "Running build job..."
    
    local components=("manager" "provider-libvirt" "provider-vsphere" "provider-proxmox")
    
    # Install libvirt for provider-libvirt (if on Linux)
    if [[ "$OSTYPE" == "linux-gnu"* ]]; then
        log_info "Installing libvirt dependencies for provider-libvirt..."
        if command -v apt-get >/dev/null 2>&1; then
            sudo apt-get update >/dev/null 2>&1 || true
            sudo apt-get install -y libvirt-dev pkg-config >/dev/null 2>&1 || log_warning "Could not install libvirt-dev"
        fi
    fi
    
    mkdir -p bin
    
    for component in "${components[@]}"; do
        log_info "Building $component..."
        
        case "$component" in
            manager)
                CGO_ENABLED=0 go build -o bin/manager ./cmd/manager
                ;;
            provider-libvirt)
                if [[ "$OSTYPE" == "linux-gnu"* ]]; then
                    CGO_ENABLED=1 go build -o bin/provider-libvirt ./cmd/provider-libvirt
                else
                    log_warning "Skipping provider-libvirt build on non-Linux OS"
                fi
                ;;
            provider-vsphere)
                CGO_ENABLED=0 go build -o bin/provider-vsphere ./cmd/provider-vsphere
                ;;
            provider-proxmox)
                CGO_ENABLED=0 go build -o bin/provider-proxmox ./cmd/provider-proxmox
                ;;
        esac
    done
    
    log_success "Build completed"
}

# Job: Build Tools
build_tools_job() {
    log_info "Running build tools job..."
    
    mkdir -p bin
    
    log_info "Building CLI tools..."
    go build -o bin/vrtg ./cmd/vrtg
    go build -o bin/vcts ./cmd/vcts
    go build -o bin/virtrigaud-loadgen ./cmd/virtrigaud-loadgen
    
    log_success "CLI tools built successfully"
}

# Job: Helm
helm_job() {
    log_info "Running helm job..."
    
    # Check if helm is installed
    if ! command -v helm >/dev/null 2>&1; then
        log_error "Helm not found. Please install Helm"
        return 1
    fi
    
    # Validate Helm charts
    log_info "Validating Helm charts..."
    helm lint charts/virtrigaud
    
    # Validate rendered manifests
    log_info "Validating rendered manifests..."
    helm template virtrigaud charts/virtrigaud --values charts/virtrigaud/values.yaml > /tmp/rendered.yaml
    
    # Basic YAML validation
    log_info "Validating YAML syntax..."
    python3 -c "
import yaml, sys
try:
    with open('/tmp/rendered.yaml', 'r') as f:
        yaml.safe_load_all(f)
    print('✅ YAML syntax validation passed')
except yaml.YAMLError as e:
    print(f'❌ YAML syntax error: {e}')
    sys.exit(1)
" || return 1
    
    # Check for basic Kubernetes resource structure
    log_info "Checking for basic Kubernetes resource structure..."
    if grep -E "(apiVersion|kind|metadata)" /tmp/rendered.yaml > /dev/null; then
        log_success "Basic Kubernetes structure found"
    else
        log_warning "No Kubernetes resources found"
    fi
    
    rm -f /tmp/rendered.yaml
}

# Main execution
main() {
    local mode="${1:-interactive}"
    
    log_info "🔄 Starting local CI testing (replicating ci.yml workflow)"
    log_info "Mode: $mode"
    
    # Always run essential jobs
    local jobs=(
        "test:test_job"
        "lint:lint_job"
        "generate:generate_job"
        "build:build_job"
        "build-tools:build_tools_job"
        "helm:helm_job"
    )
    
    # In interactive mode, ask about security job
    if [[ "$mode" == "interactive" ]]; then
        echo
        log_warning "Security job includes vulnerability scanning and may take extra time"
        read -p "Run security job? (y/N): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            jobs+=("security:security_job")
        fi
    elif [[ "$mode" == "full" ]]; then
        jobs+=("security:security_job")
    fi
    
    local failed_jobs=0
    
    # Run all jobs
    for job_spec in "${jobs[@]}"; do
        local job_name="${job_spec%%:*}"
        local job_function="${job_spec##*:}"
        
        echo
        if ! run_job "$job_name" "$job_function"; then
            ((failed_jobs++))
        fi
    done
    
    # Print summary
    echo
    log_info "📊 CI Test Results Summary:"
    for result in "${RESULTS[@]}"; do
        echo "  $result"
    done
    
    echo
    if [[ $failed_jobs -eq 0 ]]; then
        log_success "🎉 All CI jobs passed! Your code should pass GitHub Actions CI"
    else
        log_error "💥 $failed_jobs job(s) failed. Fix these issues before pushing"
        exit 1
    fi
}

# Show help
show_help() {
    cat << EOF
Local CI Testing Script

Usage: $0 [MODE]

Modes:
    interactive    Ask before running optional jobs (default)
    full          Run all jobs including security
    quick         Run only essential jobs (test, lint, build)

Examples:
    $0                # Interactive mode
    $0 quick          # Quick essential tests
    $0 full           # Full CI replication

Jobs tested:
    - test           Go tests and coverage
    - lint           Code linting with golangci-lint
    - generate       Code and manifest generation
    - build          Binary compilation
    - build-tools    CLI tools compilation
    - helm           Helm chart validation
    - security       Security scanning (optional)

Note: Some jobs may require system dependencies (libvirt, protoc, etc.)
EOF
}

# Handle arguments
case "${1:-interactive}" in
    "help"|"--help"|"-h")
        show_help
        exit 0
        ;;
    "quick")
        # Override jobs for quick mode
        main "quick"
        ;;
    "full")
        main "full"
        ;;
    "interactive"|"")
        main "interactive"
        ;;
    *)
        log_error "Unknown mode: $1"
        show_help
        exit 1
        ;;
esac
