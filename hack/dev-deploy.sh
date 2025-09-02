#!/bin/bash
# Local Development Deployment Script for virtrigaud
# Builds images locally and deploys to Kind/minikube cluster

set -euo pipefail

# Configuration
CLUSTER_NAME="${CLUSTER_NAME:-virtrigaud-dev}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
TAG="${TAG:-dev-$(git rev-parse --short HEAD)}"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
HELM_NAMESPACE="${HELM_NAMESPACE:-virtrigaud-system}"
SKIP_BUILD="${SKIP_BUILD:-false}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log() {
    echo -e "${BLUE}[$(date +'%Y-%m-%d %H:%M:%S')] $1${NC}"
}

success() {
    echo -e "${GREEN}✅ $1${NC}"
}

warn() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

error() {
    echo -e "${RED}❌ $1${NC}"
}

# Check prerequisites
check_prerequisites() {
    log "Checking prerequisites..."
    
    local missing_tools=()
    
    if ! command -v ${CONTAINER_TOOL} &> /dev/null; then
        missing_tools+=("${CONTAINER_TOOL}")
    fi
    
    if ! command -v kind &> /dev/null; then
        missing_tools+=("kind")
    fi
    
    if ! command -v helm &> /dev/null; then
        missing_tools+=("helm")
    fi
    
    if ! command -v kubectl &> /dev/null; then
        missing_tools+=("kubectl")
    fi
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        error "Missing required tools: ${missing_tools[*]}"
        echo "Install them with:"
        echo "  brew install kind helm kubectl # macOS"
        echo "  # or your system package manager"
        exit 1
    fi
    
    success "All prerequisites available"
}

# Setup local registry for Kind
setup_local_registry() {
    if [ "${CONTAINER_TOOL}" = "docker" ]; then
        log "Setting up local Docker registry..."
        
        # Check if registry is already running
        if ! ${CONTAINER_TOOL} ps --format '{{.Names}}' | grep -q "^kind-registry$"; then
            log "Starting local registry container..."
            ${CONTAINER_TOOL} run -d --restart=always -p "127.0.0.1:${REGISTRY_PORT}:5000" --name kind-registry registry:2
            success "Local registry started on port ${REGISTRY_PORT}"
        else
            success "Local registry already running"
        fi
    fi
}

# Create or ensure Kind cluster exists
setup_kind_cluster() {
    log "Setting up Kind cluster '${CLUSTER_NAME}'..."
    
    if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
        log "Creating new Kind cluster..."
        cat <<EOF | kind create cluster --name="${CLUSTER_NAME}" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry.mirrors."localhost:${REGISTRY_PORT}"]
    endpoint = ["http://kind-registry:5000"]
nodes:
- role: control-plane
  image: kindest/node:v1.31.2@sha256:18fbefc20a7113353c7b75b5c869d7145a6abd6269154825872dc59c1329912e
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        node-labels: "ingress-ready=true"
  extraPortMappings:
  - containerPort: 80
    hostPort: 80
    protocol: TCP
  - containerPort: 443
    hostPort: 443
    protocol: TCP
- role: worker
  image: kindest/node:v1.31.2@sha256:18fbefc20a7113353c7b75b5c869d7145a6abd6269154825872dc59c1329912e
- role: worker
  image: kindest/node:v1.31.2@sha256:18fbefc20a7113353c7b75b5c869d7145a6abd6269154825872dc59c1329912e
EOF
        
        # Connect registry to cluster network
        if [ "${CONTAINER_TOOL}" = "docker" ]; then
            ${CONTAINER_TOOL} network connect "kind" kind-registry 2>/dev/null || true
        fi
        
        success "Kind cluster '${CLUSTER_NAME}' created"
    else
        success "Kind cluster '${CLUSTER_NAME}' already exists"
    fi
    
    # Set kubectl context
    kubectl config use-context "kind-${CLUSTER_NAME}"
    success "kubectl context set to kind-${CLUSTER_NAME}"
}

# Build and push images locally
build_and_push_images() {
    if [ "${SKIP_BUILD}" = "true" ]; then
        warn "Skipping image builds (SKIP_BUILD=true)"
        return
    fi
    
    log "Building virtrigaud images locally..."
    
    # Build manager image
    log "Building manager image..."
    ${CONTAINER_TOOL} build -f build/Dockerfile.manager -t "localhost:${REGISTRY_PORT}/virtrigaud/manager:${TAG}" .
    ${CONTAINER_TOOL} push "localhost:${REGISTRY_PORT}/virtrigaud/manager:${TAG}"
    success "Manager image built and pushed"
    
    # Build provider images
    for provider in libvirt vsphere proxmox; do
        if [ -f "cmd/provider-${provider}/Dockerfile" ]; then
            log "Building provider-${provider} image..."
            ${CONTAINER_TOOL} build -f "cmd/provider-${provider}/Dockerfile" -t "localhost:${REGISTRY_PORT}/virtrigaud/provider-${provider}:${TAG}" .
            ${CONTAINER_TOOL} push "localhost:${REGISTRY_PORT}/virtrigaud/provider-${provider}:${TAG}"
            success "Provider ${provider} image built and pushed"
        fi
    done
}

# Generate and apply CRDs
apply_crds() {
    log "Generating and applying CRDs..."
    make generate
    kubectl apply -f config/crd/bases/
    success "CRDs applied"
}

# Deploy using Helm
deploy_with_helm() {
    log "Deploying virtrigaud with Helm..."
    
    # Create namespace
    kubectl create namespace "${HELM_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    
    # Build Helm values for local deployment
    cat > /tmp/virtrigaud-dev-values.yaml <<EOF
global:
  imageRegistry: localhost:${REGISTRY_PORT}
  imagePullPolicy: Always

manager:
  image:
    repository: virtrigaud/manager
    tag: ${TAG}
    pullPolicy: Always

providers:
  libvirt:
    enabled: true
    image:
      repository: virtrigaud/provider-libvirt
      tag: ${TAG}
      pullPolicy: Always
  vsphere:
    enabled: true
    image:
      repository: virtrigaud/provider-vsphere
      tag: ${TAG}
      pullPolicy: Always
  proxmox:
    enabled: true
    image:
      repository: virtrigaud/provider-proxmox
      tag: ${TAG}
      pullPolicy: Always
    env:
      - name: PVE_ENDPOINT
        value: "https://proxmox.example.com:8006"
      - name: PVE_USERNAME
        value: "root@pam"
      - name: PVE_PASSWORD
        value: "test-password"
      - name: PVE_INSECURE_SKIP_VERIFY
        value: "true"

# Development settings
replicaCount: 1
resources:
  limits:
    memory: 128Mi
  requests:
    memory: 64Mi

# RBAC configuration for cluster-scoped CRDs
rbac:
  create: true
  scope: cluster

# Enable debug logging
logLevel: debug
logEncoder: console

# Disable webhook for easier development
webhook:
  enabled: false
EOF
    
    # Deploy or upgrade
    helm upgrade --install virtrigaud ./charts/virtrigaud \
        --namespace "${HELM_NAMESPACE}" \
        --values /tmp/virtrigaud-dev-values.yaml \
        --wait --timeout=10m
    
    success "virtrigaud deployed to ${HELM_NAMESPACE} namespace"
}

# Wait for pods to be ready
wait_for_deployment() {
    log "Waiting for pods to be ready..."
    kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=virtrigaud -n "${HELM_NAMESPACE}" --timeout=300s
    success "All pods are ready"
}

# Show deployment status
show_status() {
    log "Deployment Status:"
    echo
    kubectl get pods -n "${HELM_NAMESPACE}" -o wide
    echo
    kubectl get services -n "${HELM_NAMESPACE}"
    echo
    log "To check logs:"
    echo "  kubectl logs -f deployment/virtrigaud-manager -n ${HELM_NAMESPACE}"
    echo
    log "To port-forward metrics:"
    echo "  kubectl port-forward svc/virtrigaud-manager-metrics-service 8443:8443 -n ${HELM_NAMESPACE}"
    echo
    log "To access the cluster:"
    echo "  kubectl config use-context kind-${CLUSTER_NAME}"
}

# Hot reload function
hot_reload() {
    log "Hot reloading virtrigaud..."
    build_and_push_images
    kubectl rollout restart deployment/virtrigaud-manager -n "${HELM_NAMESPACE}"
    kubectl rollout status deployment/virtrigaud-manager -n "${HELM_NAMESPACE}" --timeout=300s
    success "Hot reload complete"
}

# Cleanup function
cleanup() {
    log "Cleaning up development deployment..."
    helm uninstall virtrigaud -n "${HELM_NAMESPACE}" 2>/dev/null || true
    kubectl delete namespace "${HELM_NAMESPACE}" --ignore-not-found
    kind delete cluster --name="${CLUSTER_NAME}" 2>/dev/null || true
    ${CONTAINER_TOOL} stop kind-registry 2>/dev/null || true
    ${CONTAINER_TOOL} rm kind-registry 2>/dev/null || true
    success "Cleanup complete"
}

# Main function
main() {
    case "${1:-deploy}" in
        "deploy")
            check_prerequisites
            setup_local_registry
            setup_kind_cluster
            build_and_push_images
            apply_crds
            deploy_with_helm
            wait_for_deployment
            show_status
            ;;
        "reload"|"hot-reload")
            hot_reload
            ;;
        "status")
            show_status
            ;;
        "cleanup"|"clean")
            cleanup
            ;;
        "logs")
            kubectl logs -f deployment/virtrigaud-manager -n "${HELM_NAMESPACE}"
            ;;
        "shell")
            kubectl exec -it deployment/virtrigaud-manager -n "${HELM_NAMESPACE}" -- /bin/sh
            ;;
        *)
            echo "Usage: $0 {deploy|reload|status|cleanup|logs|shell}"
            echo "  deploy     - Full deployment (default)"
            echo "  reload     - Hot reload after code changes"
            echo "  status     - Show deployment status"
            echo "  cleanup    - Remove everything"
            echo "  logs       - Follow manager logs"
            echo "  shell      - Get shell in manager pod"
            echo
            echo "Environment variables:"
            echo "  CLUSTER_NAME    - Kind cluster name (default: virtrigaud-dev)"
            echo "  TAG             - Image tag (default: dev-<git-hash>)"
            echo "  HELM_NAMESPACE  - Kubernetes namespace (default: virtrigaud-system)"
            echo "  SKIP_BUILD      - Skip image building (default: false)"
            exit 1
            ;;
    esac
}

main "$@"
