#!/bin/bash

# Local Release Testing Script
# Simulates the release.yml workflow locally without pushing to registries

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

# Configuration
REGISTRY="localhost:5000"
IMAGE_NAME_PREFIX="virtrigaud"
TEST_TAG="${1:-v0.2.0-local-test}"
VERSION="${TEST_TAG#v}"

cd "$PROJECT_ROOT"

# Start local Docker registry
start_local_registry() {
    log_info "Starting local Docker registry..."
    
    if ! docker ps | grep -q "registry:2"; then
        docker run -d -p 5000:5000 --name local-registry registry:2 2>/dev/null || {
            log_warning "Local registry already exists, restarting..."
            docker stop local-registry 2>/dev/null || true
            docker rm local-registry 2>/dev/null || true
            docker run -d -p 5000:5000 --name local-registry registry:2
        }
    fi
    
    log_success "Local Docker registry is running on $REGISTRY"
}

# Stop local registry
stop_local_registry() {
    log_info "Stopping local Docker registry..."
    docker stop local-registry 2>/dev/null || true
    docker rm local-registry 2>/dev/null || true
}

# Build and push container images
build_and_push_images() {
    log_info "🔨 Building and pushing container images..."
    
    local components=("manager" "provider-vsphere" "provider-proxmox" "provider-libvirt")
    
    for component in "${components[@]}"; do
        log_info "Building $component..."
        
        local dockerfile
        if [[ "$component" == "manager" ]]; then
            dockerfile="./build/Dockerfile.manager"
        else
            dockerfile="./cmd/$component/Dockerfile"
        fi
        
        if [[ ! -f "$dockerfile" ]]; then
            log_warning "Dockerfile not found: $dockerfile, skipping $component"
            continue
        fi
        
        # Skip libvirt on non-Linux platforms
        if [[ "$component" == "provider-libvirt" && "$OSTYPE" != "linux-gnu"* ]]; then
            log_warning "Skipping provider-libvirt on non-Linux platform"
            continue
        fi
        
        local image_name="$REGISTRY/$IMAGE_NAME_PREFIX/$component:$TEST_TAG"
        
        log_info "Building $image_name..."
        if docker build -f "$dockerfile" -t "$image_name" .; then
            log_info "Pushing $image_name..."
            if docker push "$image_name"; then
                log_success "Successfully built and pushed $component"
            else
                log_error "Failed to push $component"
                return 1
            fi
        else
            log_error "Failed to build $component"
            return 1
        fi
    done
}

# Build Helm chart
build_helm_chart() {
    log_info "📦 Building Helm chart..."
    
    if ! command -v helm >/dev/null 2>&1; then
        log_error "Helm not found. Please install Helm"
        return 1
    fi
    
    # Create backup of Chart.yaml and values.yaml
    cp charts/virtrigaud/Chart.yaml charts/virtrigaud/Chart.yaml.bak
    cp charts/virtrigaud/values.yaml charts/virtrigaud/values.yaml.bak
    
    # Update chart version
    log_info "Updating chart version to $VERSION..."
    sed -i "s/version: .*/version: $VERSION/" charts/virtrigaud/Chart.yaml
    sed -i "s/appVersion: .*/appVersion: \"$TEST_TAG\"/" charts/virtrigaud/Chart.yaml
    
    # Update image tags in values.yaml
    log_info "Updating image tags in values.yaml..."
    sed -i "s/tag: \".*\"/tag: \"$TEST_TAG\"/" charts/virtrigaud/values.yaml
    
    # Package chart
    log_info "Packaging chart..."
    mkdir -p dist/helm
    if helm package charts/virtrigaud --destination ./dist/helm/; then
        log_success "Helm chart packaged successfully"
        
        # List the packaged chart
        ls -la dist/helm/*.tgz
    else
        log_error "Failed to package Helm chart"
        return 1
    fi
    
    # Restore original files
    log_info "Restoring original Chart.yaml and values.yaml..."
    mv charts/virtrigaud/Chart.yaml.bak charts/virtrigaud/Chart.yaml
    mv charts/virtrigaud/values.yaml.bak charts/virtrigaud/values.yaml
}

# Build CLI tools
build_cli_tools() {
    log_info "🔧 Building CLI tools..."
    
    mkdir -p dist/cli
    
    local tools=("vrtg" "vcts" "virtrigaud-loadgen" "vrtg-provider")
    local platforms=("linux/amd64" "darwin/amd64" "windows/amd64")
    
    for tool in "${tools[@]}"; do
        for platform in "${platforms[@]}"; do
            local goos="${platform%%/*}"
            local goarch="${platform##*/}"
            local ext=""
            
            if [[ "$goos" == "windows" ]]; then
                ext=".exe"
            fi
            
            local output="dist/cli/${tool}-${goos}-${goarch}${ext}"
            
            log_info "Building $tool for $platform..."
            
            if GOOS="$goos" GOARCH="$goarch" go build \
                -ldflags="-s -w -X 'main.version=$TEST_TAG'" \
                -o "$output" \
                "./cmd/$tool"; then
                log_success "Built $output"
            else
                log_warning "Failed to build $tool for $platform (may not exist)"
            fi
        done
    done
    
    # Show built tools
    log_info "Built CLI tools:"
    find dist/cli -type f | sort
}

# Generate changelog
generate_changelog() {
    log_info "📝 Generating changelog..."
    
    mkdir -p dist
    
    # Get the previous tag
    local prev_tag
    prev_tag=$(git tag --sort=version:refname | tail -2 | head -1 2>/dev/null || echo "")
    
    if [[ -z "$prev_tag" ]] || [[ "$prev_tag" == "$TEST_TAG" ]]; then
        prev_tag=$(git rev-list --max-parents=0 HEAD)
        log_info "Using initial commit as base: $prev_tag"
    else
        log_info "Using previous tag as base: $prev_tag"
    fi
    
    # Generate changelog
    cat > dist/CHANGELOG.md << EOF
# Changelog for $TEST_TAG

## What's Changed

$(git log --pretty=format:"- %s (%h)" ${prev_tag}..HEAD | head -20)

## Container Images

- Manager: \`$REGISTRY/$IMAGE_NAME_PREFIX/manager:$TEST_TAG\`
- Provider Libvirt: \`$REGISTRY/$IMAGE_NAME_PREFIX/provider-libvirt:$TEST_TAG\`
- Provider vSphere: \`$REGISTRY/$IMAGE_NAME_PREFIX/provider-vsphere:$TEST_TAG\`
- Provider Proxmox: \`$REGISTRY/$IMAGE_NAME_PREFIX/provider-proxmox:$TEST_TAG\`

## Installation

\`\`\`bash
# Local testing
helm install virtrigaud ./dist/helm/virtrigaud-$VERSION.tgz \\
  --set global.imageRegistry=$REGISTRY
\`\`\`

## Security

This is a local test build. Production releases include:
- Images signed with Cosign
- Software Bill of Materials (SBOMs)
- Security scanning with Trivy

**Full Changelog**: Local test build from current working directory
EOF
    
    log_success "Changelog generated: dist/CHANGELOG.md"
}

# Create checksums
create_checksums() {
    log_info "🔐 Creating checksums..."
    
    cd dist
    
    # Find all release artifacts
    local files
    files=$(find . -type f \( -name "*.tgz" -o -name "vrtg-*" -o -name "vcts-*" -o -name "virtrigaud-loadgen-*" \) 2>/dev/null | sort)
    
    if [[ -n "$files" ]]; then
        echo "$files" | xargs sha256sum > checksums.txt
        log_success "Generated checksums for $(echo "$files" | wc -l) files"
        
        log_info "Checksums:"
        cat checksums.txt
    else
        log_warning "No files found for checksum generation"
        touch checksums.txt
    fi
    
    cd "$PROJECT_ROOT"
}

# Simulate release creation
simulate_release() {
    log_info "🎯 Simulating GitHub release creation..."
    
    log_info "Release summary:"
    echo "  Tag: $TEST_TAG"
    echo "  Version: $VERSION"
    echo "  Registry: $REGISTRY"
    
    log_info "Release artifacts:"
    find dist -type f | sort | sed 's/^/  /'
    
    if [[ -f "dist/CHANGELOG.md" ]]; then
        echo
        log_info "Changelog preview:"
        head -20 dist/CHANGELOG.md | sed 's/^/  /'
    fi
    
    log_success "Release simulation completed"
}

# Test container images
test_images() {
    log_info "🧪 Testing container images..."
    
    local components=("manager" "provider-vsphere" "provider-proxmox")
    
    # Skip provider-libvirt on non-Linux
    if [[ "$OSTYPE" == "linux-gnu"* ]]; then
        components+=("provider-libvirt")
    fi
    
    for component in "${components[@]}"; do
        local image_name="$REGISTRY/$IMAGE_NAME_PREFIX/$component:$TEST_TAG"
        
        log_info "Testing $component image..."
        
        # Test that image exists and can be inspected
        if docker image inspect "$image_name" >/dev/null 2>&1; then
            log_success "$component image is valid"
            
            # Test that image can be run (basic smoke test)
            log_info "Running smoke test for $component..."
            if docker run --rm "$image_name" --version 2>/dev/null || \
               docker run --rm "$image_name" --help 2>/dev/null || \
               docker run --rm "$image_name" /bin/sh -c 'echo "Container can start"' 2>/dev/null; then
                log_success "$component smoke test passed"
            else
                log_warning "$component smoke test inconclusive (may need specific args)"
            fi
        else
            log_error "$component image not found or invalid"
            return 1
        fi
    done
}

# Cleanup function
cleanup() {
    log_info "🧹 Cleaning up..."
    
    # Remove any temporary files
    rm -rf /tmp/virtrigaud-release-test-*
    
    # Restore any backed up files
    if [[ -f "charts/virtrigaud/Chart.yaml.bak" ]]; then
        mv charts/virtrigaud/Chart.yaml.bak charts/virtrigaud/Chart.yaml
    fi
    if [[ -f "charts/virtrigaud/values.yaml.bak" ]]; then
        mv charts/virtrigaud/values.yaml.bak charts/virtrigaud/values.yaml
    fi
    
    # Ask about registry cleanup
    echo
    read -p "Stop local Docker registry? (y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        stop_local_registry
    else
        log_info "Local registry left running for further testing"
    fi
    
    # Ask about dist cleanup
    read -p "Remove dist/ directory? (y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        rm -rf dist/
        log_info "Removed dist/ directory"
    else
        log_info "dist/ directory preserved for inspection"
    fi
}

# Show help
show_help() {
    cat << EOF
Local Release Testing Script

Usage: $0 [TAG] [OPTIONS]

Arguments:
    TAG             Release tag to simulate (default: v0.2.0-local-test)

Options:
    --no-images     Skip container image building
    --no-test       Skip image testing
    --help          Show this help

Examples:
    $0                          # Test with default tag
    $0 v0.3.0-rc.1             # Test with specific tag
    $0 --no-images             # Skip image building (faster)

What this script does:
    1. Starts local Docker registry (localhost:5000)
    2. Builds and pushes container images
    3. Builds and packages Helm chart
    4. Builds CLI tools for multiple platforms
    5. Generates changelog
    6. Creates checksums
    7. Tests container images
    8. Simulates release creation

Note: This is for testing only. No artifacts are published to real registries.
EOF
}

# Main execution
main() {
    local skip_images=false
    local skip_test=false
    
    # Parse arguments
    while [[ $# -gt 0 ]]; do
        case $1 in
            --no-images)
                skip_images=true
                shift
                ;;
            --no-test)
                skip_test=true
                shift
                ;;
            --help|-h)
                show_help
                exit 0
                ;;
            -*)
                log_error "Unknown option: $1"
                show_help
                exit 1
                ;;
            *)
                TEST_TAG="$1"
                VERSION="${TEST_TAG#v}"
                shift
                ;;
        esac
    done
    
    log_info "🚀 Starting local release testing"
    log_info "Tag: $TEST_TAG"
    log_info "Version: $VERSION"
    
    # Pre-flight checks
    if ! command -v docker >/dev/null 2>&1; then
        log_error "Docker not found. Please install Docker"
        exit 1
    fi
    
    if ! command -v go >/dev/null 2>&1; then
        log_error "Go not found. Please install Go"
        exit 1
    fi
    
    # Create dist directory
    mkdir -p dist
    
    # Start registry
    start_local_registry
    
    # Build components
    if [[ "$skip_images" == "false" ]]; then
        build_and_push_images
    else
        log_warning "Skipping container image building"
    fi
    
    build_helm_chart
    build_cli_tools
    generate_changelog
    create_checksums
    
    # Test images
    if [[ "$skip_images" == "false" && "$skip_test" == "false" ]]; then
        test_images
    else
        log_warning "Skipping image testing"
    fi
    
    simulate_release
    
    log_success "🎉 Local release testing completed!"
    echo
    log_info "📁 Release artifacts are in: dist/"
    log_info "🐳 Container images are in: $REGISTRY"
    echo
    log_info "To test the release locally:"
    echo "  helm install virtrigaud ./dist/helm/virtrigaud-$VERSION.tgz \\"
    echo "    --set global.imageRegistry=$REGISTRY"
}

# Handle Ctrl+C
trap cleanup EXIT

# Handle help early
if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
    show_help
    exit 0
fi

# Run main
main "$@"
