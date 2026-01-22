# Cloud-Init Configuration Guide

This guide explains how to configure virtual machines using cloud-init with VirtRigaud, including both `userData` and `metaData` fields.

## Overview

VirtRigaud supports cloud-init configuration through two separate fields:

- **`userData`**: Contains the cloud-init user-data configuration (what to do)
- **`metaData`**: Contains the cloud-init metadata configuration (instance information)

Both fields support inline YAML or references to Kubernetes Secrets.

## Understanding UserData vs MetaData

### UserData (Cloud-Init User-Data)

The `userData` field contains the cloud-init configuration that defines **what should be done** to the VM during initialization:

- Install packages
- Configure users and SSH keys
- Run commands
- Write files
- Configure services

### MetaData (Cloud-Init Metadata)

The `metaData` field contains information **about the instance** itself:

- Instance ID
- Hostname
- Network configuration
- Public keys
- Custom metadata fields (region, environment, etc.)

## Basic Usage

### Example: UserData Only (Default Metadata)

If you only specify `userData`, VirtRigaud automatically provides default metadata:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: simple-vm
spec:
  providerRef:
    name: vsphere-datacenter
  classRef:
    name: small
  imageRef:
    name: ubuntu-22-04-template
  
  userData:
    cloudInit:
      inline: |
        #cloud-config
        users:
          - name: admin
            sudo: ALL=(ALL) NOPASSWD:ALL
            ssh_authorized_keys:
              - ssh-rsa AAAAB3... key@example.com
        
        packages:
          - nginx
          - docker.io
        
        runcmd:
          - systemctl enable nginx
          - systemctl start nginx
  
  networks:
    - name: app-net
      ipPolicy: dhcp
  
  powerState: On
```

**Default Metadata Generated:**
```json
{
  "instance-id": "simple-vm"
}
```

### Example: Custom MetaData

Specify custom metadata for more control over instance configuration:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: custom-vm
spec:
  providerRef:
    name: vsphere-datacenter
  classRef:
    name: medium
  imageRef:
    name: ubuntu-22-04-template
  
  userData:
    cloudInit:
      inline: |
        #cloud-config
        users:
          - name: developer
            groups: docker, sudo
        
        packages:
          - python3
          - nodejs
  
  metaData:
    cloudInit:
      inline: |
        instance-id: custom-vm-prod-001
        local-hostname: app-server-01.example.com
        
        network:
          version: 2
          ethernets:
            eth0:
              dhcp4: true
        
        # Custom metadata
        region: us-west-2
        availability-zone: us-west-2a
        environment: production
        project: web-app
  
  networks:
    - name: production-net
      ipPolicy: dhcp
  
  powerState: On
```

## Advanced Configuration

### Network Configuration in MetaData

You can configure network settings via metadata:

```yaml
metaData:
  cloudInit:
    inline: |
      instance-id: static-ip-vm
      local-hostname: database-server
      
      network:
        version: 2
        ethernets:
          eth0:
            addresses:
              - 192.168.1.100/24
            gateway4: 192.168.1.1
            nameservers:
              addresses:
                - 8.8.8.8
                - 8.8.4.4
```

### Public Keys in MetaData

Add SSH public keys via metadata (alternative to user-data):

```yaml
metaData:
  cloudInit:
    inline: |
      instance-id: secure-vm-001
      local-hostname: secure-host
      
      public-keys:
        - ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ... admin@example.com
        - ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ... backup@example.com
```

### Using Kubernetes Secrets

For sensitive data or centralized management, use Secrets:

```yaml
---
apiVersion: v1
kind: Secret
metadata:
  name: vm-cloud-init-config
  namespace: default
type: Opaque
stringData:
  userdata: |
    #cloud-config
    users:
      - name: admin
        ssh_authorized_keys:
          - ssh-rsa AAAAB3... admin@example.com
  
  metadata: |
    instance-id: prod-vm-001
    local-hostname: production-server
    environment: production
    region: us-east-1

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: vm-from-secrets
spec:
  providerRef:
    name: vsphere-datacenter
  classRef:
    name: medium
  imageRef:
    name: ubuntu-22-04-template
  
  userData:
    cloudInit:
      secretRef:
        name: vm-cloud-init-config
        key: userdata
  
  metaData:
    secretRef:
      name: vm-cloud-init-config
      key: metadata
  
  networks:
    - name: production
      ipPolicy: dhcp
  
  powerState: On
```

## VMware vSphere Implementation

When you configure cloud-init in VirtRigaud for vSphere, the following guestinfo properties are set:

### With Custom MetaData

```
guestinfo.userdata = <your-cloud-init-config>
guestinfo.userdata.encoding = yaml

guestinfo.metadata = <your-custom-metadata>
guestinfo.metadata.encoding = yaml
```

### With Default MetaData

```
guestinfo.userdata = <your-cloud-init-config>
guestinfo.userdata.encoding = yaml

guestinfo.metadata = {"instance-id": "<vm-name>"}
guestinfo.metadata.encoding = json
```

## Complete Example

Here's a complete example showing all features:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: full-featured-vm
  namespace: production
spec:
  providerRef:
    name: vsphere-datacenter
  classRef:
    name: large
  imageRef:
    name: ubuntu-22-04-template
  
  # User-data: What to do
  userData:
    cloudInit:
      inline: |
        #cloud-config
        
        # Set timezone
        timezone: America/New_York
        
        # Configure users
        users:
          - name: admin
            sudo: ALL=(ALL) NOPASSWD:ALL
            groups: docker, sudo
            shell: /bin/bash
            ssh_authorized_keys:
              - ssh-rsa AAAAB3... admin@example.com
          
          - name: developer
            groups: docker, users
            shell: /bin/bash
            ssh_authorized_keys:
              - ssh-rsa AAAAB3... dev@example.com
        
        # Install packages
        packages:
          - docker.io
          - docker-compose
          - nginx
          - certbot
          - python3-certbot-nginx
          - git
          - curl
          - wget
          - htop
        
        # Write configuration files
        write_files:
          - path: /etc/nginx/sites-available/app
            content: |
              server {
                listen 80;
                server_name app.example.com;
                location / {
                  proxy_pass http://localhost:3000;
                }
              }
            permissions: '0644'
        
        # Run commands
        runcmd:
          - systemctl enable docker
          - systemctl start docker
          - systemctl enable nginx
          - systemctl start nginx
          - ln -s /etc/nginx/sites-available/app /etc/nginx/sites-enabled/
          - nginx -t && systemctl reload nginx
          - echo "VM provisioning complete at $(date)" >> /var/log/cloud-init-done.log
  
  # Metadata: Information about the instance
  metaData:
    inline: |
      instance-id: full-featured-vm-prod-001
      local-hostname: app-server-prod-01.example.com
      
      # Network configuration
      network:
        version: 2
        ethernets:
          eth0:
            dhcp4: true
            dhcp6: false
      
      # Public SSH keys (alternative location)
      public-keys:
        - ssh-rsa AAAAB3... backup-key@example.com
      
      # Custom metadata for organization
      region: us-west-2
      availability-zone: us-west-2a
      environment: production
      project: web-application
      team: platform-engineering
      cost-center: engineering
      managed-by: virtrigaud
  
  # Network configuration
  networks:
    - name: production-app-tier
      ipPolicy: dhcp
  
  # Additional disks
  disks:
    - name: data
      sizeGiB: 100
      type: thin
  
  # Power state
  powerState: On
  
  # Tags for organization
  tags:
    - production
    - web-application
    - us-west-2
```

## Best Practices

### 1. Use Secrets for Sensitive Data

Never put passwords or tokens in inline configuration:

```yaml
# ❌ BAD - Don't do this
userData:
  cloudInit:
    inline: |
      #cloud-config
      users:
        - name: admin
          passwd: $6$rounds=4096$saltysalt... # Visible in git!

# ✅ GOOD - Use Secrets
userData:
  cloudInit:
    secretRef:
      name: admin-user-config
```

### 2. Provide Instance ID in MetaData

Always provide a unique instance-id if using custom metadata:

```yaml
metaData:
  inline: |
    instance-id: unique-vm-id-12345  # Required for cloud-init
    local-hostname: my-server
```

### 3. Test Cloud-Init Configuration

Validate your cloud-init config before deploying:

```bash
# Install cloud-init tools
sudo apt-get install cloud-init

# Validate your config
cloud-init schema --config-file my-config.yaml
```

### 4. Use Network Configuration Carefully

Network configuration in metadata can override DHCP. Ensure your settings are correct:

```yaml
metaData:
  inline: |
    network:
      version: 2
      ethernets:
        eth0:
          dhcp4: true  # Or specify static configuration
```

### 5. Monitor Cloud-Init Execution

After VM creation, check cloud-init status:

```bash
# Check cloud-init status
sudo cloud-init status

# View cloud-init logs
sudo cat /var/log/cloud-init.log
sudo cat /var/log/cloud-init-output.log

# Check for errors
sudo cloud-init analyze show
```

## Troubleshooting

### Cloud-Init Not Running

Check if cloud-init datasource is configured correctly:

```bash
# Check datasource
sudo cloud-init query ds

# Should show VMware datasource
```

### Metadata Not Applied

Verify guestinfo properties are set in vSphere:

```bash
# From ESXi or vCenter, check extraConfig
# Should see:
# guestinfo.userdata
# guestinfo.userdata.encoding
# guestinfo.metadata
# guestinfo.metadata.encoding
```

### Configuration Syntax Errors

Validate YAML syntax before applying:

```bash
# Check YAML syntax
yamllint my-vm.yaml

# Validate cloud-init config
cloud-init schema --config-file cloud-config.yaml
```

## See Also

- [VirtualMachine CRD Reference](CRDs.md#virtualmachine)
- [Examples: Cloud-Init with Metadata](examples/cloud-init-with-metadata.yaml)
- [Cloud-Init Documentation](https://cloudinit.readthedocs.io/)
- [VMware Cloud-Init DataSource](https://cloudinit.readthedocs.io/en/latest/topics/datasources/vmware.html)
