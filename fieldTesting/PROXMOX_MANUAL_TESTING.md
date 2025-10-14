# Proxmox Manual Testing Script

This directory contains a bash script for manually testing Proxmox VM creation via direct API calls, allowing you to experiment with different SSH key encoding approaches.

## Prerequisites

- `jq` installed (`apt-get install jq` or `brew install jq`)
- Access to a Proxmox VE server
- A Proxmox API token (or can modify script to use username/password)
- A VM template with cloud-init support

## Files

- `proxmox-manual-test.sh` - Main testing script
- `complete-vm-fixed.yaml` - VirtRigaud manifest for comparison

## Usage

### Quick Start

```bash
# Test all encoding approaches automatically
./proxmox-manual-test.sh

# Test a specific approach
./proxmox-manual-test.sh double

# Show help
./proxmox-manual-test.sh --help
```

### Configuration

Edit the script or set environment variables:

```bash
export PVE_HOST="172.16.56.190"
export PVE_PORT="8006"
export PVE_NODE="pve"
export PVE_TOKEN_ID="root@pam!mytoken"
export PVE_TOKEN_SECRET="your-secret-here"
export TEMPLATE_ID="9000"
export NEW_VM_ID="9001"

./proxmox-manual-test.sh
```

## Encoding Approaches

The script tests different SSH key encoding methods:

### 1. **none** - No Encoding
- Lets curl handle the encoding naturally
- Uses `--data-raw` with raw SSH key

### 2. **single** - Single URL Encoding
- Encodes SSH key once with `jq @uri`
- Space → `%20`, + → `%2B`

### 3. **double** - Double URL Encoding
- VirtRigaud rc9 approach
- First encode: space → `%20`
- Second encode: `%20` → `%2520`

### 4. **python** - Python-style Encoding
- Mimics Python's `quote(sshkey, safe='')`
- Manual encoding with space → `%20`

### 5. **base64** - Base64 Encoding
- Experimental approach
- Encodes entire key in base64

## What It Does

1. **Checks** if test VM already exists
2. **Deletes** existing VM (if present)
3. **Clones** VM from template
4. **Configures** VM with:
   - CPU cores
   - Memory
   - Cloud-init user
   - SSH keys (with selected encoding)
   - Network (DHCP)
   - Cloud-init storage
5. **Reports** success or failure with error details

## Output Example

```
=======================================================================
Proxmox Manual VM Creation Test
=======================================================================

Proxmox Endpoint: https://172.16.56.190:8006
Node: pve
Template ID: 9000
New VM ID: 9001
SSH Key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN7l...

=======================================================================
[INFO] Testing SSH key encoding approach: double
=======================================================================
[INFO] Checking if VM 9001 already exists...
[INFO] VM 9001 does not exist.
[INFO] Cloning VM from template 9000 to 9001...
[SUCCESS] VM cloned successfully
[INFO] Configuring VM with approach: double
[INFO] Original SSH key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5...
[INFO] Encoded SSH key: ssh-ed25519%2520AAAAC3NzaC1lZDI1...
[SUCCESS] VM configured successfully with approach 'double'!
[SUCCESS] ✅ Approach 'double' SUCCEEDED!

=======================================================================
[SUCCESS] RESULT: Approach 'double' worked!
=======================================================================
```

## Debugging Tips

### Check Proxmox Logs
```bash
# On Proxmox server
tail -f /var/log/pveproxy/access.log
tail -f /var/log/daemon.log | grep pveproxy
```

### Inspect Request Details
Modify the script to add `-v` flag to curl:
```bash
curl -k -v -X POST \
    -H "$auth_header" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-raw "$data" \
    "$url"
```

### Manual Testing with curl
```bash
# Example: Test double encoding manually
SSH_KEY="ssh-ed25519 AAAAC3Nza..."
ENCODED=$(printf '%s' "$SSH_KEY" | jq -sRr @uri | jq -sRr @uri)

curl -k -X POST \
  -H "Authorization: PVEAPIToken=root@pam!mytoken=your-secret" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-raw "cores=2&memory=4096&ciuser=wrkode&sshkeys=${ENCODED}" \
  https://172.16.56.190:8006/api2/json/nodes/pve/qemu/9001/config
```

## Comparing with VirtRigaud

To see what VirtRigaud does:

1. Deploy rc11 with debug logging:
   ```bash
   helm upgrade virtrigaud virtrigaud/virtrigaud \
     --version v0.2.3-rc11 \
     --reuse-values
   ```

2. Apply the manifest:
   ```bash
   kubectl apply -f complete-vm-fixed.yaml
   ```

3. Check logs for debug output:
   ```bash
   kubectl logs -n default deployment/virtrigaud-provider-default-proxmox-prod | grep "DEBUG SSH"
   ```

4. Compare the encoded values with your manual test

## Troubleshooting

### "Invalid urlencoded string" Error
This indicates Proxmox cannot decode the SSH key. Try:
- Different encoding approach
- Checking for trailing newlines
- Verifying the SSH key format

### VM Clone Fails
- Ensure template ID exists: `pvesh get /nodes/pve/qemu`
- Check storage permissions
- Verify network bridge exists

### VM Starts but SSH Fails
- Encoding worked, but cloud-init might not have applied
- Check cloud-init logs in the VM: `cloud-init status --long`

## Related Files

- VirtRigaud implementation: `internal/providers/proxmox/pveapi/client.go`
- Debug logging: Added in v0.2.3-rc11
- Issue tracking: `PROXMOX_SSH_TROUBLESHOOTING.md`
