#!/bin/bash

###############################################################################
# Proxmox Manual VM Creation Test Script
# 
# This script creates a VM directly via Proxmox API using curl,
# mimicking what VirtRigaud does internally.
#
# Usage:
#   1. Edit the variables below to match your environment
#   2. chmod +x proxmox-manual-test.sh
#   3. ./proxmox-manual-test.sh
#
# This allows manual testing of different SSH key encoding approaches
###############################################################################

# Proxmox Connection Settings
PVE_HOST="${PVE_HOST:-172.16.56.190}"
PVE_PORT="${PVE_PORT:-8006}"
PVE_NODE="${PVE_NODE:-pve}"
PVE_TOKEN_ID="${PVE_TOKEN_ID:-root@pam!mytoken}"
PVE_TOKEN_SECRET="${PVE_TOKEN_SECRET:-af777916-e07a-41f0-b373-f83b722505f6}"

# VM Configuration
TEMPLATE_ID="${TEMPLATE_ID:-9000}"
NEW_VM_ID="${NEW_VM_ID:-9001}"
VM_NAME="${VM_NAME:-test-vm-manual}"
VM_CORES="${VM_CORES:-2}"
VM_MEMORY="${VM_MEMORY:-4096}"  # in MB
STORAGE="${STORAGE:-vms}"
NETWORK_BRIDGE="${NETWORK_BRIDGE:-vmbr0}"

# SSH Key (from complete-vm-fixed.yaml)
SSH_KEY="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN7lHIuo2QJBkdVDL79bl+tEmJh3pBz7rHImwvNMjenK"

# Cloud-Init User
CI_USER="wrkode"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

###############################################################################
# Helper Functions
###############################################################################

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

# Make API call to Proxmox
pve_api_call() {
    local method="$1"
    local path="$2"
    local data="$3"
    
    local url="https://${PVE_HOST}:${PVE_PORT}${path}"
    local auth_header="Authorization: PVEAPIToken=${PVE_TOKEN_ID}=${PVE_TOKEN_SECRET}"
    
    if [ "$method" = "GET" ]; then
        curl -k -s -X GET \
            -H "$auth_header" \
            "$url"
    else
        curl -k -s -X POST \
            -H "$auth_header" \
            -H "Content-Type: application/x-www-form-urlencoded" \
            --data-raw "$data" \
            "$url"
    fi
}

###############################################################################
# Encoding Functions - Test Different Approaches
###############################################################################

# Approach 1: No encoding (let curl handle it)
encode_sshkey_none() {
    echo "$SSH_KEY"
}

# Approach 2: Single URL encoding
encode_sshkey_single() {
    printf '%s' "$SSH_KEY" | jq -sRr @uri
}

# Approach 3: Double URL encoding (current rc9 approach)
encode_sshkey_double() {
    local first=$(printf '%s' "$SSH_KEY" | jq -sRr @uri)
    printf '%s' "$first" | jq -sRr @uri
}

# Approach 4: Python-style quote (space as %20, manual)
encode_sshkey_python() {
    # Use sed to manually encode spaces as %20
    echo "$SSH_KEY" | sed 's/ /%20/g'
}

# Approach 5: Base64 encoding (just to test)
encode_sshkey_base64() {
    echo -n "$SSH_KEY" | base64
}

###############################################################################
# Main Functions
###############################################################################

# Check if VM already exists
check_vm_exists() {
    log_info "Checking if VM $NEW_VM_ID already exists..."
    local response=$(pve_api_call "GET" "/api2/json/nodes/${PVE_NODE}/qemu/${NEW_VM_ID}/status/current" "")
    
    if echo "$response" | jq -e '.data' > /dev/null 2>&1; then
        log_warn "VM $NEW_VM_ID already exists!"
        return 0
    else
        log_info "VM $NEW_VM_ID does not exist."
        return 1
    fi
}

# Delete existing VM
delete_vm() {
    log_info "Deleting VM $NEW_VM_ID..."
    local response=$(pve_api_call "DELETE" "/api2/json/nodes/${PVE_NODE}/qemu/${NEW_VM_ID}" "")
    
    if echo "$response" | jq -e '.data' > /dev/null 2>&1; then
        log_success "VM $NEW_VM_ID deleted successfully"
        sleep 2
    else
        log_error "Failed to delete VM: $response"
        exit 1
    fi
}

# Clone VM from template
clone_vm() {
    log_info "Cloning VM from template $TEMPLATE_ID to $NEW_VM_ID..."
    
    local data="newid=${NEW_VM_ID}&name=${VM_NAME}&storage=${STORAGE}&full=1&target=${PVE_NODE}"
    
    log_info "Clone request data: $data"
    
    local response=$(pve_api_call "POST" "/api2/json/nodes/${PVE_NODE}/qemu/${TEMPLATE_ID}/clone" "$data")
    
    log_info "Clone response: $response"
    
    if echo "$response" | jq -e '.data' > /dev/null 2>&1; then
        log_success "VM cloned successfully"
        local upid=$(echo "$response" | jq -r '.data')
        log_info "Task UPID: $upid"
        
        # Wait for clone to complete
        log_info "Waiting for clone operation to complete..."
        sleep 5
    else
        log_error "Failed to clone VM: $response"
        exit 1
    fi
}

# Configure VM with cloud-init and SSH keys
# Usage: configure_vm <encoding_approach>
configure_vm() {
    local approach="$1"
    
    log_info "Configuring VM with approach: $approach"
    
    # Encode SSH key based on approach
    local encoded_key=""
    case "$approach" in
        "none")
            encoded_key=$(encode_sshkey_none)
            ;;
        "single")
            encoded_key=$(encode_sshkey_single)
            ;;
        "double")
            encoded_key=$(encode_sshkey_double)
            ;;
        "python")
            encoded_key=$(encode_sshkey_python)
            ;;
        "base64")
            encoded_key=$(encode_sshkey_base64)
            ;;
        *)
            log_error "Unknown encoding approach: $approach"
            exit 1
            ;;
    esac
    
    log_info "Original SSH key: $SSH_KEY"
    log_info "Encoded SSH key: $encoded_key"
    log_info "Encoded key length: ${#encoded_key}"
    
    # Build configuration data
    local data="cores=${VM_CORES}&memory=${VM_MEMORY}"
    data="${data}&ciuser=${CI_USER}"
    data="${data}&sshkeys=${encoded_key}"
    data="${data}&ipconfig0=ip=dhcp"  # Proxmox expects "ip=dhcp" format
    data="${data}&ide2=${STORAGE}:cloudinit"
    data="${data}&net0=virtio,bridge=${NETWORK_BRIDGE}"  # First value is model (no prefix needed)
    
    log_info "Configuration request data (first 200 chars): ${data:0:200}..."
    
    local response=$(pve_api_call "POST" "/api2/json/nodes/${PVE_NODE}/qemu/${NEW_VM_ID}/config" "$data")
    
    log_info "Configuration response: $response"
    
    if echo "$response" | jq -e '.errors.sshkeys' > /dev/null 2>&1; then
        local error=$(echo "$response" | jq -r '.errors.sshkeys')
        log_error "SSH key encoding FAILED with approach '$approach': $error"
        return 1
    elif echo "$response" | jq -e '.data' > /dev/null 2>&1; then
        log_success "VM configured successfully with approach '$approach'!"
        return 0
    else
        log_error "Configuration failed: $response"
        return 1
    fi
}

# Start VM
start_vm() {
    log_info "Starting VM $NEW_VM_ID..."
    
    local response=$(pve_api_call "POST" "/api2/json/nodes/${PVE_NODE}/qemu/${NEW_VM_ID}/status/start" "")
    
    if echo "$response" | jq -e '.data' > /dev/null 2>&1; then
        log_success "VM started successfully"
    else
        log_warn "Failed to start VM or VM already running: $response"
    fi
}

# Get VM info
get_vm_info() {
    log_info "Fetching VM $NEW_VM_ID info..."
    
    local response=$(pve_api_call "GET" "/api2/json/nodes/${PVE_NODE}/qemu/${NEW_VM_ID}/config" "")
    
    echo "$response" | jq '.'
}

###############################################################################
# Test Runner
###############################################################################

run_test() {
    local approach="$1"
    
    echo ""
    echo "======================================================================="
    log_info "Testing SSH key encoding approach: $approach"
    echo "======================================================================="
    
    # Clean up if VM exists
    if check_vm_exists; then
        delete_vm
    fi
    
    # Clone from template
    clone_vm
    
    # Configure with specified encoding approach
    if configure_vm "$approach"; then
        log_success "✅ Approach '$approach' SUCCEEDED!"
        
        # Optionally start the VM
        # start_vm
        
        # Show VM config
        get_vm_info
        
        return 0
    else
        log_error "❌ Approach '$approach' FAILED!"
        return 1
    fi
}

###############################################################################
# Main Script
###############################################################################

main() {
    echo "======================================================================="
    echo "Proxmox Manual VM Creation Test"
    echo "======================================================================="
    echo ""
    echo "Proxmox Endpoint: https://${PVE_HOST}:${PVE_PORT}"
    echo "Node: ${PVE_NODE}"
    echo "Template ID: ${TEMPLATE_ID}"
    echo "New VM ID: ${NEW_VM_ID}"
    echo "SSH Key: ${SSH_KEY:0:40}..."
    echo ""
    
    # Check if jq is installed
    if ! command -v jq &> /dev/null; then
        log_error "jq is required but not installed. Please install it: apt-get install jq"
        exit 1
    fi
    
    # If a specific approach is passed as argument, test only that
    if [ -n "$1" ]; then
        run_test "$1"
        exit $?
    fi
    
    # Otherwise, test all approaches sequentially
    local approaches=("none" "single" "double" "python")
    local successful_approach=""
    
    for approach in "${approaches[@]}"; do
        if run_test "$approach"; then
            successful_approach="$approach"
            break
        fi
        
        echo ""
        log_info "Trying next approach..."
        sleep 2
    done
    
    echo ""
    echo "======================================================================="
    if [ -n "$successful_approach" ]; then
        log_success "RESULT: Approach '$successful_approach' worked!"
    else
        log_error "RESULT: All approaches failed!"
    fi
    echo "======================================================================="
}

# Show usage if --help
if [ "$1" = "--help" ] || [ "$1" = "-h" ]; then
    echo "Usage: $0 [approach]"
    echo ""
    echo "Approaches:"
    echo "  none     - No encoding, let curl handle it"
    echo "  single   - Single URL encoding"
    echo "  double   - Double URL encoding (VirtRigaud rc9 approach)"
    echo "  python   - Python-style encoding (space as %20)"
    echo "  base64   - Base64 encoding (experimental)"
    echo ""
    echo "If no approach is specified, all approaches will be tested sequentially."
    echo ""
    echo "Environment Variables:"
    echo "  PVE_HOST          - Proxmox host (default: 172.16.56.190)"
    echo "  PVE_PORT          - Proxmox port (default: 8006)"
    echo "  PVE_NODE          - Proxmox node name (default: pve)"
    echo "  PVE_TOKEN_ID      - API token ID (default: root@pam!mytoken)"
    echo "  PVE_TOKEN_SECRET  - API token secret (default: af777916-e07a-41f0-b373-f83b722505f6)"
    echo "  TEMPLATE_ID       - Template VM ID (default: 9000)"
    echo "  NEW_VM_ID         - New VM ID to create (default: 9001)"
    exit 0
fi

# Run main
main "$@"

