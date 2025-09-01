#!/bin/bash

# Local Workflow Testing Script for VirtRigaud
# This script allows you to test GitHub Actions workflows locally using 'act'
# Run this before pushing to save GitHub Actions costs and catch issues early

set -euo pipefail

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ACT_VERSION="0.2.70"
ACT_RUNNER_IMAGE="catthehacker/ubuntu:act-22.04"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${BLUE}ℹ️  $1${NC}"
}

log_success() {
    echo -e "${GREEN}✅ $1${NC}"
}

log_warning() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

log_error() {
    echo -e "${RED}❌ $1${NC}"
}

# Check if act is installed and install if needed
check_and_install_act() {
    if command -v act >/dev/null 2>&1; then
        local current_version
        current_version=$(act --version | grep -o 'act version [0-9.]*' | cut -d' ' -f3 || echo "unknown")
        log_info "Found act version: $current_version"
        
        if [[ "$current_version" != "$ACT_VERSION" ]]; then
            log_warning "Different act version detected. Recommended: $ACT_VERSION"
            read -p "Do you want to upgrade? (y/N): " -n 1 -r
            echo
            if [[ $REPLY =~ ^[Yy]$ ]]; then
                install_act
            fi
        fi
    else
        log_warning "act not found. Installing act..."
        install_act
    fi
}

install_act() {
    log_info "Installing act version $ACT_VERSION..."
    
    if [[ "$OSTYPE" == "linux-gnu"* ]]; then
        curl -L https://github.com/nektos/act/releases/download/v${ACT_VERSION}/act_Linux_x86_64.tar.gz | tar xz
        sudo mv act /usr/local/bin/act
    elif [[ "$OSTYPE" == "darwin"* ]]; then
        if command -v brew >/dev/null 2>&1; then
            brew install act
        else
            curl -L https://github.com/nektos/act/releases/download/v${ACT_VERSION}/act_Darwin_x86_64.tar.gz | tar xz
            sudo mv act /usr/local/bin/act
        fi
    else
        log_error "Unsupported OS. Please install act manually: https://github.com/nektos/act"
        exit 1
    fi
    
    chmod +x /usr/local/bin/act 2>/dev/null || true
    log_success "act installed successfully"
}

# Create .actrc configuration file
setup_act_config() {
    local actrc="$PROJECT_ROOT/.actrc"
    
    if [[ ! -f "$actrc" ]]; then
        log_info "Creating .actrc configuration..."
        cat > "$actrc" << EOF
# Act configuration for VirtRigaud
-P ubuntu-latest=$ACT_RUNNER_IMAGE
-P ubuntu-22.04=$ACT_RUNNER_IMAGE
-P ubuntu-24.04=$ACT_RUNNER_IMAGE
--container-daemon-socket /var/run/docker.sock
--reuse
--rm
EOF
        log_success "Created .actrc configuration"
    fi
}

# Create secrets file for act
setup_act_secrets() {
    local secrets_file="$PROJECT_ROOT/.secrets"
    
    if [[ ! -f "$secrets_file" ]]; then
        log_info "Creating .secrets file for act..."
        cat > "$secrets_file" << EOF
# GitHub token (optional - only needed for release workflows)
GITHUB_TOKEN=your_github_token_here

# Container registry (for local testing)
REGISTRY=localhost:5000
EOF
        log_warning "Created .secrets file. Update with real values if needed for release testing."
        log_warning "Add .secrets to .gitignore if it's not already there."
    fi
}

# Test individual workflow
test_workflow() {
    local workflow_file="$1"
    local job_name="${2:-}"
    local event="${3:-push}"
    
    log_info "Testing workflow: $workflow_file"
    
    cd "$PROJECT_ROOT"
    
    # Base act command
    local act_cmd=(
        "act"
        "$event"
        "-W" ".github/workflows/$workflow_file"
        "--artifact-server-path" "/tmp/act-artifacts"
        "--env-file" ".env.local"
    )
    
    # Add job filter if specified
    if [[ -n "$job_name" ]]; then
        act_cmd+=("-j" "$job_name")
    fi
    
    # Add secrets if file exists
    if [[ -f ".secrets" ]]; then
        act_cmd+=("--secret-file" ".secrets")
    fi
    
    log_info "Running: ${act_cmd[*]}"
    
    if "${act_cmd[@]}"; then
        log_success "Workflow $workflow_file completed successfully"
    else
        log_error "Workflow $workflow_file failed"
        return 1
    fi
}

# Create local environment file
create_env_file() {
    local env_file="$PROJECT_ROOT/.env.local"
    
    if [[ ! -f "$env_file" ]]; then
        log_info "Creating .env.local file..."
        cat > "$env_file" << EOF
# Local environment variables for act
GO_VERSION=1.23
GOLANGCI_LINT_VERSION=v1.64.8
REGISTRY=localhost:5000
IMAGE_NAME_PREFIX=virtrigaud

# Mock values for testing
GITHUB_ACTOR=local-user
GITHUB_REPOSITORY=projectbeskar/virtrigaud
GITHUB_REF=refs/heads/main
GITHUB_SHA=local-test-sha
GITHUB_RUN_ID=12345
GITHUB_RUN_NUMBER=1
EOF
        log_success "Created .env.local file"
    fi
}

# Start local Docker registry for testing
start_local_registry() {
    log_info "Starting local Docker registry..."
    
    if ! docker ps | grep -q "registry:2"; then
        docker run -d -p 5000:5000 --name local-registry registry:2 2>/dev/null || {
            log_warning "Local registry already exists or failed to start"
            docker start local-registry 2>/dev/null || true
        }
    fi
    
    log_success "Local Docker registry is running on localhost:5000"
}

# Test specific workflow functions
test_lint() {
    log_info "🔍 Testing Lint Workflow"
    test_workflow "lint.yml"
}

test_ci() {
    log_info "🔄 Testing CI Workflow"
    
    log_warning "CI workflow is complex and may take 20+ minutes locally"
    read -p "Do you want to test the full CI workflow? (y/N): " -n 1 -r
    echo
    
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        # Start local registry for container builds
        start_local_registry
        test_workflow "ci.yml"
    else
        log_info "Testing individual CI jobs instead..."
        
        log_info "Testing 'test' job..."
        test_workflow "ci.yml" "test"
        
        log_info "Testing 'lint' job..."
        test_workflow "ci.yml" "lint"
        
        log_info "Testing 'generate' job..."
        test_workflow "ci.yml" "generate"
        
        log_success "Individual CI jobs tested"
    fi
}

test_release() {
    log_info "🚀 Testing Release Workflow"
    
    log_warning "Release workflow requires secrets and may push to registries"
    log_warning "This will create a mock release locally"
    
    read -p "Do you want to test the release workflow? (y/N): " -n 1 -r
    echo
    
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        start_local_registry
        # Test with workflow_dispatch event and mock tag
        export INPUT_TAG="v0.2.0-local-test"
        test_workflow "release.yml" "" "workflow_dispatch"
    else
        log_info "Skipping release workflow test"
    fi
}

test_runtime_chart() {
    log_info "📦 Testing Runtime Chart Workflow"
    test_workflow "runtime-chart.yml"
}

# Quick smoke test - just check workflow syntax
smoke_test() {
    log_info "🔥 Running smoke tests (syntax validation only)"
    
    local workflows=("lint.yml" "ci.yml" "release.yml" "runtime-chart.yml")
    
    for workflow in "${workflows[@]}"; do
        log_info "Validating syntax for $workflow..."
        if act -W ".github/workflows/$workflow" --list >/dev/null 2>&1; then
            log_success "$workflow syntax is valid"
        else
            log_error "$workflow has syntax errors"
            return 1
        fi
    done
    
    log_success "All workflow syntax validation passed"
}

# Cleanup function
cleanup() {
    log_info "🧹 Cleaning up local test environment..."
    
    # Stop local registry if it was started
    docker stop local-registry 2>/dev/null || true
    docker rm local-registry 2>/dev/null || true
    
    # Clean up act containers
    docker ps -a | grep "act-" | awk '{print $1}' | xargs -r docker rm -f 2>/dev/null || true
    
    # Clean up act artifacts
    rm -rf /tmp/act-artifacts 2>/dev/null || true
    
    log_success "Cleanup completed"
}

# Print help
show_help() {
    cat << EOF
VirtRigaud Local Workflow Testing

Usage: $0 [COMMAND]

Commands:
    setup       Set up act and configuration files
    smoke       Quick syntax validation of all workflows
    lint        Test lint workflow
    ci          Test CI workflow (with options for full or partial)
    release     Test release workflow (requires secrets)
    runtime     Test runtime chart workflow
    all         Test all workflows sequentially
    cleanup     Clean up local test environment
    help        Show this help message

Examples:
    $0 setup          # First-time setup
    $0 smoke          # Quick validation
    $0 lint           # Test just the lint workflow
    $0 ci             # Test CI workflow with options
    $0 all            # Test all workflows (interactive)

Requirements:
    - Docker (for act runner images)
    - act (will be installed automatically)
    - Git (for repository context)

Environment:
    - .env.local      # Local environment variables
    - .secrets        # GitHub secrets (optional)
    - .actrc          # Act configuration

Note: Testing locally can save GitHub Actions costs and catch issues early.
Some workflows may require secrets or external dependencies.
EOF
}

# Main function
main() {
    local command="${1:-help}"
    
    cd "$PROJECT_ROOT"
    
    case "$command" in
        "setup")
            log_info "🔧 Setting up local workflow testing environment..."
            check_and_install_act
            setup_act_config
            setup_act_secrets
            create_env_file
            log_success "Setup completed! Run '$0 smoke' to test syntax validation."
            ;;
        "smoke")
            smoke_test
            ;;
        "lint")
            test_lint
            ;;
        "ci")
            test_ci
            ;;
        "release")
            test_release
            ;;
        "runtime")
            test_runtime_chart
            ;;
        "all")
            log_info "🧪 Testing all workflows..."
            smoke_test
            test_lint
            test_ci
            test_runtime_chart
            # Skip release by default in 'all' mode
            log_warning "Skipping release workflow (run manually with 'release' command)"
            log_success "All workflow tests completed!"
            ;;
        "cleanup")
            cleanup
            ;;
        "help"|"--help"|"-h")
            show_help
            ;;
        *)
            log_error "Unknown command: $command"
            show_help
            exit 1
            ;;
    esac
}

# Handle Ctrl+C
trap cleanup EXIT

# Run main function
main "$@"
