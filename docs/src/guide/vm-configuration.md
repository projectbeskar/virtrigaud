# VM Configuration

Detailed guide to configuring virtual machines.

## CPU and Memory

Use VMClass for standardized sizing:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: large
spec:
  cpus: 8
  memory: 16Gi
  cpuReservation: 4000  # MHz
  memoryReservation: 8Gi
```

## Storage

Configure disks:

```yaml
spec:
  disks:
  - name: os-disk
    size: 100Gi
    storageClass: fast-ssd
  - name: data-disk
    size: 500Gi
    storageClass: standard
```

## Networking

Configure network interfaces:

```yaml
spec:
  networkInterfaces:
  - name: eth0
    networkName: vlan-100
  - name: eth1
    networkName: vlan-200
```

## Guest Customization

Cloud-init configuration:

```yaml
spec:
  guestInfo:
    hostname: web01.example.com
    cloudInit:
      userData: |
        #cloud-config
        users:
        - name: admin
          ssh_authorized_keys:
          - ssh-rsa AAAA...
```

## Advanced Options

See [Advanced Lifecycle](../advanced-lifecycle.md) for more configuration options.
