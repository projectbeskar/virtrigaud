# Libvirt Host Preparation Guide

This guide provides detailed instructions on preparing a Libvirt/KVM host to work with Virtrigaud.

## Table of Contents
- [Prerequisites](#prerequisites)
- [User and Group Configuration](#user-and-group-configuration)
- [SSH Configuration](#ssh-configuration)
- [Storage Configuration](#storage-configuration)
- [Network Configuration](#network-configuration)
- [SELinux/AppArmor Configuration](#selinuxapparmor-configuration)
- [Verification](#verification)
- [Troubleshooting](#troubleshooting)

## Prerequisites

### Required Packages

Install the following packages on your Libvirt host:

```bash
# Ubuntu/Debian
sudo apt-get update
sudo apt-get install -y \
  qemu-kvm \
  libvirt-daemon-system \
  libvirt-clients \
  bridge-utils \
  cloud-utils \
  genisoimage \
  sshpass \
  qemu-utils

# RHEL/CentOS/Fedora
sudo dnf install -y \
  qemu-kvm \
  libvirt \
  libvirt-client \
  bridge-utils \
  cloud-utils \
  genisoimage \
  sshpass \
  qemu-img
```

### Virtualization Support

Ensure your system supports hardware virtualization:

```bash
# Check for Intel VT-x or AMD-V support
egrep -c '(vmx|svm)' /proc/cpuinfo
# Should return a number > 0

# Verify KVM modules are loaded
lsmod | grep kvm
# Should show kvm_intel or kvm_amd
```

## User and Group Configuration

### Critical: libvirt-qemu Group Membership

**This is the most critical step!** The `libvirt-qemu` user (which QEMU/KVM runs as) must be a member of the `libvirt` group to access VM disks in `/var/lib/libvirt/images/`.

```bash
# Add libvirt-qemu user to the libvirt group
sudo usermod -aG libvirt libvirt-qemu

# Verify the group membership
id libvirt-qemu
# Should show: groups=994(kvm),111(libvirt),64055(libvirt-qemu)

# Restart libvirtd for changes to take effect
sudo systemctl restart libvirtd
```

### SSH User Configuration

Create or configure a user for Virtrigaud to connect via SSH:

```bash
# If the user doesn't exist, create it
sudo useradd -m -s /bin/bash virt-admin

# Add the user to the libvirt and kvm groups
sudo usermod -aG libvirt,kvm virt-admin

# Set up passwordless sudo for libvirt commands (optional but recommended)
echo "virt-admin ALL=(ALL) NOPASSWD: /usr/bin/virsh, /usr/bin/qemu-img, /usr/bin/chown, /usr/bin/chmod, /bin/systemctl restart libvirtd" | sudo tee /etc/sudoers.d/virt-admin
sudo chmod 0440 /etc/sudoers.d/virt-admin
```

### SSH Key Setup

Set up SSH key authentication for the Virtrigaud provider:

```bash
# On your Kubernetes/Virtrigaud host, generate an SSH key if needed
ssh-keygen -t ed25519 -f ~/.ssh/virtrigaud_libvirt -N ""

# Copy the public key to the Libvirt host
ssh-copy-id -i ~/.ssh/virtrigaud_libvirt.pub virt-admin@<libvirt-host>

# Test the connection
ssh -i ~/.ssh/virtrigaud_libvirt virt-admin@<libvirt-host> "virsh version"
```

## Storage Configuration

### Default Storage Pool

Virtrigaud uses `/var/lib/libvirt/images` as the default storage location. Ensure proper permissions:

```bash
# Verify directory permissions
ls -ld /var/lib/libvirt/images
# Should show: drwxrwsrwx 2 root root 4096 ...

# If permissions are incorrect, fix them:
sudo mkdir -p /var/lib/libvirt/images
sudo chmod 777 /var/lib/libvirt/images
sudo chmod g+s /var/lib/libvirt/images  # Set GID bit for inheritance

# Verify parent directory permissions
ls -ld /var/lib/libvirt
# Should show: drwxr-x--- 8 root libvirt ... (with libvirt group)
```

### Storage Pool Definition

Create or verify the default storage pool:

```bash
# Check if a pool exists
virsh pool-list --all

# If no pool exists, create one:
virsh pool-define-as default dir --target /var/lib/libvirt/images
virsh pool-build default
virsh pool-start default
virsh pool-autostart default

# Verify the pool
virsh pool-info default
```

### Alternative Storage Locations

If you need to use a different storage location (e.g., NFS mount, dedicated partition):

```bash
# Example: Using /vm-pool01
sudo mkdir -p /vm-pool01
sudo chown root:libvirt /vm-pool01
sudo chmod 770 /vm-pool01

# Create a storage pool
virsh pool-define-as vm-pool01 dir --target /vm-pool01
virsh pool-build vm-pool01
virsh pool-start vm-pool01
virsh pool-autostart vm-pool01
```

**Note:** Update your `VMImage` resource to reference images in the custom pool path.

## Network Configuration

### Bridge Network Setup

For VMs to have direct network access, configure a bridge:

```bash
# Install bridge utilities
sudo apt-get install bridge-utils  # Ubuntu/Debian
sudo dnf install bridge-utils      # RHEL/Fedora

# Example: Create br0 bridge on eno1 interface
# WARNING: This will temporarily disrupt network connectivity
# It's recommended to do this via console access

# Using netplan (Ubuntu 18.04+):
cat <<EOF | sudo tee /etc/netplan/01-netcfg.yaml
network:
  version: 2
  renderer: networkd
  ethernets:
    eno1:
      dhcp4: no
      dhcp6: no
  bridges:
    br0:
      interfaces: [eno1]
      dhcp4: yes
      dhcp6: no
      parameters:
        stp: false
        forward-delay: 0
EOF

sudo netplan apply

# Verify bridge
brctl show
ip addr show br0
```

### Libvirt Network Configuration

Create a libvirt network that uses the bridge:

```bash
# Create network XML
cat <<EOF > /tmp/host-bridge.xml
<network>
  <name>host-bridge</name>
  <forward mode='bridge'/>
  <bridge name='br0'/>
</network>
EOF

# Define and start the network
virsh net-define /tmp/host-bridge.xml
virsh net-start host-bridge
virsh net-autostart host-bridge

# Verify
virsh net-list --all
virsh net-dumpxml host-bridge
```

### NAT Network (Alternative)

If you prefer NAT networking instead of bridged:

```bash
# The default network usually provides NAT
virsh net-list --all
# Should show 'default' network

# If not present, create it:
virsh net-define /usr/share/libvirt/networks/default.xml
virsh net-start default
virsh net-autostart default
```

## SELinux/AppArmor Configuration

### SELinux (RHEL/CentOS/Fedora)

If SELinux is enabled, ensure proper contexts:

```bash
# Check SELinux status
getenforce

# If SELinux is enabled, set proper contexts
sudo semanage fcontext -a -t virt_image_t "/var/lib/libvirt/images(/.*)?"
sudo restorecon -Rv /var/lib/libvirt/images

# For custom storage locations
sudo semanage fcontext -a -t virt_image_t "/vm-pool01(/.*)?"
sudo restorecon -Rv /vm-pool01
```

**Note:** Virtrigaud automatically runs `restorecon` on disk images after creation, but this will fail silently if SELinux is not installed.

### AppArmor (Ubuntu/Debian)

AppArmor profiles for libvirt are usually pre-configured, but verify:

```bash
# Check AppArmor status
sudo aa-status | grep libvirt

# If issues arise, you may need to adjust the profile
sudo aa-complain /usr/sbin/libvirtd  # Set to complain mode for debugging
# or
sudo aa-disable /usr/sbin/libvirtd   # Disable AppArmor for libvirt
```

### Disable Security (Not Recommended for Production)

For testing environments only:

```bash
# Disable SELinux temporarily
sudo setenforce 0

# Or disable AppArmor for libvirt
sudo systemctl stop apparmor
```

## Verification

### Pre-Flight Checks

Run these commands to verify your setup:

```bash
# 1. Check libvirt daemon
sudo systemctl status libvirtd
virsh version

# 2. Verify user groups
id libvirt-qemu | grep libvirt
# Should show 'libvirt' in the groups

# 3. Check storage permissions
ls -ld /var/lib/libvirt
ls -ld /var/lib/libvirt/images
# Parent should be drwxr-x--- root libvirt
# Images dir should be drwxrwsrwx

# 4. Test storage pool
virsh pool-list --all
virsh pool-info default

# 5. Check networks
virsh net-list --all
brctl show

# 6. Test SSH connectivity
ssh virt-admin@localhost "virsh list --all"

# 7. Test file access as libvirt-qemu
sudo -u libvirt-qemu test -r /var/lib/libvirt/images && echo "✓ READ OK" || echo "✗ READ FAILED"
sudo -u libvirt-qemu test -w /var/lib/libvirt/images && echo "✓ WRITE OK" || echo "✗ WRITE FAILED"
```

### Test VM Creation

Create a minimal test VM to verify everything works:

```bash
# Download a cloud image
cd /var/lib/libvirt/images
sudo wget https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img

# Set correct permissions
sudo chown libvirt-qemu:kvm noble-server-cloudimg-amd64.img
sudo chmod 777 noble-server-cloudimg-amd64.img

# Create a test VM via Virtrigaud or manually with virt-install
```

## Troubleshooting

### Permission Denied Errors

**Symptom:** `Cannot access storage file ... Permission denied`

**Common Causes:**
1. `libvirt-qemu` user not in `libvirt` group
2. Parent directory `/var/lib/libvirt` has restrictive permissions
3. Disk file missing execute bit (should be 777, not 666)

**Solution:**
```bash
# Fix group membership
sudo usermod -aG libvirt libvirt-qemu
sudo systemctl restart libvirtd

# Fix file permissions
sudo chmod 777 /var/lib/libvirt/images/*.qcow2

# Verify access
sudo -u libvirt-qemu test -r /var/lib/libvirt/images/disk.qcow2 && echo "OK" || echo "FAILED"
```

### Network Issues

**Symptom:** VM has no network connectivity or wrong interface type

**Solution:**
```bash
# Check network is active
virsh net-list --all

# Verify bridge exists
brctl show
ip link show br0

# Check VM network configuration
virsh domiflist <vm-name>
# Should show bridge interface, not 'user' type

# Restart network
virsh net-destroy host-bridge
virsh net-start host-bridge
```

### SSH Connection Issues

**Symptom:** Provider cannot connect to libvirt host

**Solution:**
```bash
# Test SSH connection
ssh -v virt-admin@<host> "virsh version"

# Check SSH key authentication
ssh -i /path/to/key virt-admin@<host> "echo OK"

# Verify sshpass is installed (needed for password auth)
which sshpass
```

### Storage Pool Issues

**Symptom:** `storage pool 'default' is not active`

**Solution:**
```bash
# Check pool status
virsh pool-list --all

# Start pool
virsh pool-start default
virsh pool-autostart default

# Delete and recreate if path is wrong
virsh pool-destroy old-pool
virsh pool-undefine old-pool
virsh pool-define-as default dir --target /var/lib/libvirt/images
virsh pool-build default
virsh pool-start default
virsh pool-autostart default
```

### Cloud Image Issues

**Symptom:** VM fails to boot or cloud-init doesn't work

**Solution:**
```bash
# Verify cloud-init ISO was created
ls -l /var/lib/libvirt/images/*-cidata.iso

# Check ISO permissions
sudo chmod 644 /var/lib/libvirt/images/*-cidata.iso

# Verify cloud-init disk is attached
virsh dumpxml <vm-name> | grep -A 5 "device='cdrom'"

# Check cloud-init status inside VM
ssh user@vm-ip "cloud-init status"
# Should show: status: done

# If cloud-init is stuck, check logs
ssh user@vm-ip "sudo cat /var/log/cloud-init.log | grep ERROR"
```

### Cloud-init Network Configuration

**Symptom:** VM network doesn't work, requires reboot, or cloud-init doesn't complete

**Explanation:** Virtrigaud uses cloud-init **version 2 network configuration** format in meta-data, which is supported across all major Linux distributions. Cloud-init automatically translates this to the appropriate network manager for each distro.

**Current Behavior (v0.3.7-dev+):**
- Network config uses version 2 format (netplan YAML syntax)
- Cloud-init translates to distribution-specific network configuration:
  - **Ubuntu/Debian**: Netplan → systemd-networkd
  - **RHEL/CentOS/Rocky/Alma**: NetworkManager or network-scripts (ifcfg files)
  - **Fedora**: NetworkManager
  - **openSUSE**: Wicked or NetworkManager
- Wildcard interface matching (`name: "e*"`) works across different naming schemes
- **Note**: On Ubuntu, netplan regeneration may require a reboot for network to fully function

**Meta-data Network Config:**
```yaml
instance-id: vm-name
local-hostname: vm-name
network:
  version: 2
  ethernets:
    eth0:
      match:
        name: "e*"
      dhcp4: true
      dhcp6: false
```

**Distribution-Specific Behavior:**

| Distribution | Network Manager | Cloud-init Translation | Reboot Required? |
|--------------|----------------|------------------------|------------------|
| Ubuntu 18.04+ | netplan + systemd-networkd | Writes to `/etc/netplan/50-cloud-init.yaml` | Sometimes* |
| Debian 11+ | netplan or ifupdown | Depends on installed tools | Maybe |
| RHEL/CentOS 7 | network-scripts | Writes to `/etc/sysconfig/network-scripts/ifcfg-*` | No |
| RHEL/CentOS 8+ | NetworkManager | Writes to `/etc/NetworkManager/system-connections/` | No |
| Fedora | NetworkManager | Writes to `/etc/NetworkManager/system-connections/` | No |
| openSUSE | Wicked/NetworkManager | Writes distribution-specific configs | No |

*On Ubuntu, if cloud-init regenerates netplan configuration, `netplan apply` may not fully activate the network until after a reboot.

**Workaround for Ubuntu Reboot Issue:**
If you need immediate network without reboot on Ubuntu, include this in your user-data:
```yaml
runcmd:
  - netplan apply
  - systemctl restart systemd-networkd
```

**For Custom Networking:**
- Provide custom network config in user-data using `write_files`
- Use distribution-specific network configuration formats as needed
- For complex scenarios, consider using `runcmd` to configure networking directly

### Windows Support

**Note:** Windows cloud images typically use **cloudbase-init** instead of cloud-init, which has different configuration requirements:

- **Network Configuration**: Windows images usually auto-configure networking via DHCP without explicit cloud-init config
- **Meta-data Format**: Cloudbase-init accepts the same meta-data format but ignores network configuration
- **User-data**: Use PowerShell scripts or unattend.xml format in user-data
- **Guest Agent**: Windows requires **QEMU Guest Agent for Windows** to be installed for IP detection

**Example Windows User-data:**
```yaml
#ps1_sysnative
# PowerShell script for Windows initialization
Set-NetFirewallProfile -Profile Domain,Public,Private -Enabled False
Install-WindowsFeature -Name Web-Server -IncludeManagementTools
```

For detailed Windows support, refer to [Cloudbase-init documentation](https://cloudbase-init.readthedocs.io/).

### "Pending Changes" in Cockpit

**Symptom:** Cockpit shows "pending changes" for CPU/Network after VM creation

**Explanation:** This is normal behavior. When a VM is created with `cpu mode='host-model'`, libvirt dynamically expands this to specific CPU features when the VM starts. Virtrigaud automatically syncs the persistent definition with the running configuration to eliminate this warning.

**If the warning persists:**
```bash
# Manually sync the persistent definition
virsh dumpxml <vm-name> > /tmp/vm.xml
virsh define /tmp/vm.xml

# Verify no differences remain
diff <(virsh dumpxml <vm-name> --inactive) <(virsh dumpxml <vm-name>)
```

## Security Considerations

### Production Environments

For production deployments:

1. **Use SSH keys** instead of passwords
2. **Restrict sudo access** to only required commands
3. **Enable SELinux/AppArmor** with proper contexts
4. **Use firewall rules** to restrict libvirt host access
5. **Use dedicated storage** with proper quotas
6. **Regular backups** of VM configurations and storage pools
7. **Audit logging** for all VM operations

### Minimal Privilege Setup

```bash
# Create a dedicated virtrigaud user with minimal permissions
sudo useradd -m -s /bin/bash virtrigaud
sudo usermod -aG libvirt,kvm virtrigaud

# Restrict sudo to specific commands only
cat <<EOF | sudo tee /etc/sudoers.d/virtrigaud
virtrigaud ALL=(ALL) NOPASSWD: /usr/bin/virsh, /usr/bin/qemu-img, /usr/bin/chown, /usr/bin/chmod
EOF
sudo chmod 0440 /etc/sudoers.d/virtrigaud
```

## Additional Resources

- [Libvirt Documentation](https://libvirt.org/docs.html)
- [KVM Documentation](https://www.linux-kvm.org/page/Documents)
- [Cloud-init Documentation](https://cloudinit.readthedocs.io/)
- [Virtrigaud Provider Documentation](./PROVIDERS.md)
- [Virtrigaud Remote Providers Guide](./REMOTE_PROVIDERS.md)

## Support

If you encounter issues not covered in this guide:

1. Check the Virtrigaud provider logs: `kubectl logs -n <namespace> deploy/virtrigaud-provider-<name>`
2. Check libvirt logs: `sudo journalctl -u libvirtd -f`
3. Enable debug logging: Set `LIBVIRT_DEBUG=1` in provider environment
4. Open an issue on the Virtrigaud GitHub repository with detailed logs and configuration

