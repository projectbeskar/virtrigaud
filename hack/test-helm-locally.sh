#!/bin/bash

# Local Helm Testing Script
# Tests Helm charts and runtime charts locally with Kind clusters

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
KIND_CLUSTER_NAME="virtrigaud-test"
HELM_TIMEOUT="10m"

cd "$PROJECT_ROOT"

# Check dependencies
check_dependencies() {
    log_info "🔍 Checking dependencies..."
    
    local missing_deps=()
    
    if ! command -v helm >/dev/null 2>&1; then
        missing_deps+=("helm")
    fi
    
    if ! command -v kind >/dev/null 2>&1; then
        missing_deps+=("kind")
    fi
    
    if ! command -v docker >/dev/null 2>&1; then
        missing_deps+=("docker")
    fi
    
    if ! command -v kubectl >/dev/null 2>&1; then
        missing_deps+=("kubectl")
    fi
    
    if [[ ${#missing_deps[@]} -gt 0 ]]; then
        log_error "Missing dependencies: ${missing_deps[*]}"
        log_info "Please install the missing dependencies:"
        echo "  - helm: https://helm.sh/docs/intro/install/"
        echo "  - kind: https://kind.sigs.k8s.io/docs/user/quick-start/"
        echo "  - docker: https://docs.docker.com/get-docker/"
        echo "  - kubectl: https://kubernetes.io/docs/tasks/tools/"
        exit 1
    fi
    
    log_success "All dependencies found"
}

# Setup Kind cluster
setup_kind_cluster() {
    log_info "🚀 Setting up Kind cluster: $KIND_CLUSTER_NAME"
    
    # Check if cluster already exists
    if kind get clusters | grep -q "^$KIND_CLUSTER_NAME$"; then
        log_warning "Kind cluster '$KIND_CLUSTER_NAME' already exists"
        read -p "Delete and recreate? (y/N): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            log_info "Deleting existing cluster..."
            kind delete cluster --name "$KIND_CLUSTER_NAME"
        else
            log_info "Using existing cluster"
            kind get kubeconfig --name "$KIND_CLUSTER_NAME" > /tmp/kubeconfig-$KIND_CLUSTER_NAME
            export KUBECONFIG="/tmp/kubeconfig-$KIND_CLUSTER_NAME"
            kubectl cluster-info
            return 0
        fi
    fi
    
    # Create new cluster
    log_info "Creating Kind cluster..."
    if kind create cluster --name "$KIND_CLUSTER_NAME" --wait 60s; then
        # Export kubeconfig
        kind get kubeconfig --name "$KIND_CLUSTER_NAME" > /tmp/kubeconfig-$KIND_CLUSTER_NAME
        export KUBECONFIG="/tmp/kubeconfig-$KIND_CLUSTER_NAME"
        
        log_success "Kind cluster created successfully"
        kubectl cluster-info
    else
        log_error "Failed to create Kind cluster"
        exit 1
    fi
}

# Test Helm chart linting
test_helm_lint() {
    log_info "🔍 Testing Helm chart linting..."
    
    local charts=("charts/virtrigaud")
    
    # Add runtime chart if it exists
    if [[ -d "charts/virtrigaud-provider-runtime" ]]; then
        charts+=("charts/virtrigaud-provider-runtime")
    fi
    
    for chart in "${charts[@]}"; do
        log_info "Linting $chart..."
        if helm lint "$chart"; then
            log_success "$chart passed lint"
        else
            log_error "$chart failed lint"
            return 1
        fi
    done
}

# Test Helm template rendering
test_helm_template() {
    log_info "🏗️  Testing Helm template rendering..."
    
    # Test main chart
    log_info "Testing virtrigaud chart template..."
    if helm template test-virtrigaud charts/virtrigaud --values charts/virtrigaud/values.yaml > /tmp/virtrigaud-rendered.yaml; then
        log_success "virtrigaud chart template rendered successfully"
        
        # Basic YAML validation
        python3 -c "
import yaml, sys
try:
    with open('/tmp/virtrigaud-rendered.yaml', 'r') as f:
        list(yaml.safe_load_all(f))
    print('✅ YAML syntax validation passed')
except yaml.YAMLError as e:
    print(f'❌ YAML syntax error: {e}')
    sys.exit(1)
"
    else
        log_error "Failed to render virtrigaud chart template"
        return 1
    fi
    
    # Test runtime chart if it exists
    if [[ -d "charts/virtrigaud-provider-runtime" ]]; then
        log_info "Testing runtime chart templates..."
        
        local value_files=(
            "charts/virtrigaud-provider-runtime/examples/values-mock.yaml"
            "charts/virtrigaud-provider-runtime/examples/values-vsphere.yaml" 
            "charts/virtrigaud-provider-runtime/examples/values-libvirt.yaml"
            "charts/virtrigaud-provider-runtime/examples/values-proxmox.yaml"
        )
        
        for values_file in "${value_files[@]}"; do
            if [[ -f "$values_file" ]]; then
                local provider=$(basename "$values_file" | sed 's/values-//; s/.yaml//')
                log_info "Testing runtime chart with $provider values..."
                
                if helm template "test-$provider" charts/virtrigaud-provider-runtime/ -f "$values_file" > "/tmp/runtime-$provider-rendered.yaml"; then
                    log_success "Runtime chart template with $provider values rendered successfully"
                else
                    log_error "Failed to render runtime chart template with $provider values"
                    return 1
                fi
            fi
        done
    fi
}

# Install and test main chart
test_main_chart_install() {
    log_info "📦 Testing main chart installation..."
    
    # Create test values that work without real infrastructure
    cat > /tmp/test-values.yaml << EOF
# Test configuration for virtrigaud chart
manager:
  replicaCount: 1
  image:
    repository: nginx  # Use nginx as a placeholder
    tag: latest
    pullPolicy: IfNotPresent

# Disable all providers for this test
providers:
  libvirt:
    enabled: false
  vsphere:
    enabled: false
  proxmox:
    enabled: false

# Disable webhooks for simplicity
webhooks:
  enabled: false

# Use minimal resources
resources:
  limits:
    cpu: 100m
    memory: 128Mi
  requests:
    cpu: 50m
    memory: 64Mi
EOF
    
    log_info "Installing virtrigaud chart..."
    if helm install virtrigaud-test charts/virtrigaud \
        -f /tmp/test-values.yaml \
        --wait \
        --timeout="$HELM_TIMEOUT"; then
        
        log_success "virtrigaud chart installed successfully"
        
        # Check deployment status
        log_info "Checking deployment status..."
        kubectl get pods,deployments,services
        
        # Wait for pods to be ready
        if kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=virtrigaud --timeout=60s; then
            log_success "All pods are ready"
        else
            log_warning "Some pods may not be ready, checking status..."
            kubectl describe pods
        fi
        
    else
        log_error "Failed to install virtrigaud chart"
        kubectl get pods,events
        return 1
    fi
}

# Test runtime chart with mock provider
test_runtime_chart_install() {
    log_info "🎭 Testing runtime chart with mock provider..."
    
    if [[ ! -d "charts/virtrigaud-provider-runtime" ]]; then
        log_warning "Runtime chart not found, skipping runtime chart test"
        return 0
    fi
    
    # Check if mock provider values exist
    local mock_values="charts/virtrigaud-provider-runtime/examples/values-mock.yaml"
    if [[ ! -f "$mock_values" ]]; then
        log_warning "Mock provider values not found, creating minimal configuration..."
        mock_values="/tmp/mock-values.yaml"
        cat > "$mock_values" << EOF
image:
  repository: nginx  # Use nginx as mock provider
  tag: latest
  pullPolicy: IfNotPresent

env:
  - name: LOG_LEVEL
    value: "debug"

resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 100m
    memory: 128Mi

service:
  type: ClusterIP
  port: 9443

livenessProbe:
  httpGet:
    path: /
    port: 80
  initialDelaySeconds: 30
  periodSeconds: 30

readinessProbe:
  httpGet:
    path: /
    port: 80
  initialDelaySeconds: 5
  periodSeconds: 10
EOF
    fi
    
    log_info "Installing runtime chart with mock provider..."
    if helm install runtime-test charts/virtrigaud-provider-runtime/ \
        -f "$mock_values" \
        --wait \
        --timeout="$HELM_TIMEOUT"; then
        
        log_success "Runtime chart installed successfully"
        
        # Check deployment status
        log_info "Checking runtime deployment status..."
        kubectl get pods,deployments,services
        
        # Wait for pods to be ready
        if kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=virtrigaud-provider-runtime --timeout=60s; then
            log_success "Runtime chart pods are ready"
        else
            log_warning "Some runtime pods may not be ready"
            kubectl describe pods -l app.kubernetes.io/name=virtrigaud-provider-runtime
        fi
        
    else
        log_error "Failed to install runtime chart"
        kubectl get pods,events
        return 1
    fi
}

# Test CRD installation
test_crd_installation() {
    log_info "📋 Testing CRD installation..."
    
    # Install CRDs via main chart (without manager)
    log_info "Installing CRDs via main chart..."
    if helm install virtrigaud-crds charts/virtrigaud \
        --set manager.replicaCount=0 \
        --set providers.libvirt.enabled=false \
        --set providers.vsphere.enabled=false \
        --set providers.proxmox.enabled=false \
        --set webhooks.enabled=false \
        --wait \
        --timeout="$HELM_TIMEOUT"; then
        
        log_success "CRDs installed successfully"
        
        # Verify CRDs
        log_info "Verifying CRDs..."
        if kubectl get crd | grep virtrigaud.io; then
            log_success "virtrigaud.io CRDs found"
            
            # Test creating sample resources
            log_info "Testing CRD functionality..."
            
            # Create a secret for credentials
            kubectl create secret generic test-credentials \
                --from-literal=username=test \
                --from-literal=password=test
            
            # Create a sample Provider resource
            kubectl apply -f - <<EOF
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: test-provider
spec:
  type: proxmox
  endpoint: http://localhost:9443
  credentialSecretRef:
    name: test-credentials
EOF
            
            # Verify resource was created
            if kubectl get provider test-provider; then
                log_success "Sample Provider resource created successfully"
                
                # Clean up
                kubectl delete provider test-provider
                kubectl delete secret test-credentials
            else
                log_error "Failed to create sample Provider resource"
                return 1
            fi
        else
            log_error "virtrigaud.io CRDs not found"
            return 1
        fi
        
        # Clean up CRDs
        helm uninstall virtrigaud-crds
        
    else
        log_error "Failed to install CRDs"
        return 1
    fi
}

# Cleanup function
cleanup_cluster() {
    log_info "🧹 Cleaning up Kind cluster..."
    
    # Uninstall Helm releases
    helm list --all-namespaces | grep -E "(virtrigaud-test|runtime-test|virtrigaud-crds)" | awk '{print $1, $2}' | while read -r name namespace; do
        log_info "Uninstalling $name from $namespace..."
        helm uninstall "$name" -n "$namespace" || true
    done
    
    # Delete Kind cluster
    if kind get clusters | grep -q "^$KIND_CLUSTER_NAME$"; then
        read -p "Delete Kind cluster '$KIND_CLUSTER_NAME'? (y/N): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            kind delete cluster --name "$KIND_CLUSTER_NAME"
            rm -f "/tmp/kubeconfig-$KIND_CLUSTER_NAME"
            log_success "Kind cluster deleted"
        else
            log_info "Kind cluster preserved for inspection"
            log_info "To access: export KUBECONFIG=/tmp/kubeconfig-$KIND_CLUSTER_NAME"
        fi
    fi
    
    # Clean up temp files
    rm -f /tmp/test-values.yaml /tmp/mock-values.yaml /tmp/*-rendered.yaml
}

# Show help
show_help() {
    cat << EOF
Local Helm Testing Script

Usage: $0 [COMMAND]

Commands:
    lint        Test Helm chart linting only
    template    Test Helm template rendering only
    crd         Test CRD installation only
    main        Test main chart installation only
    runtime     Test runtime chart installation only
    full        Run all tests (default)
    cleanup     Clean up test cluster and resources
    help        Show this help

Examples:
    $0                # Run full test suite
    $0 lint           # Just lint charts
    $0 template       # Just test templating
    $0 main           # Just test main chart install
    $0 cleanup        # Clean up after testing

What this script tests:
    - Helm chart linting (helm lint)
    - Template rendering with various values
    - CRD installation and functionality
    - Main chart installation (with test values)
    - Runtime chart installation (if present)
    - Pod readiness and basic functionality

Requirements:
    - helm, kind, docker, kubectl
    - Kind cluster will be created/used: $KIND_CLUSTER_NAME

Note: Uses placeholder images (nginx) for testing without building real containers.
EOF
}

# Main execution
main() {
    local command="${1:-full}"
    
    case "$command" in
        "lint")
            check_dependencies
            test_helm_lint
            ;;
        "template")
            check_dependencies
            test_helm_template
            ;;
        "crd")
            check_dependencies
            setup_kind_cluster
            test_crd_installation
            ;;
        "main")
            check_dependencies
            setup_kind_cluster
            test_main_chart_install
            ;;
        "runtime")
            check_dependencies
            setup_kind_cluster
            test_runtime_chart_install
            ;;
        "full")
            check_dependencies
            test_helm_lint
            test_helm_template
            setup_kind_cluster
            test_crd_installation
            test_main_chart_install
            test_runtime_chart_install
            log_success "🎉 All Helm tests completed successfully!"
            ;;
        "cleanup")
            cleanup_cluster
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
trap cleanup_cluster EXIT

# Run main
main "$@"
