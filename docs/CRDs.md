# Custom Resource Definitions (CRDs)

This document describes all the Custom Resource Definitions (CRDs) provided by virtrigaud.

## VirtualMachine

The `VirtualMachine` CRD represents a virtual machine instance.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `providerRef` | `ObjectRef` | Yes | Reference to the Provider resource |
| `classRef` | `ObjectRef` | Yes | Reference to the VMClass resource |
| `imageRef` | `ObjectRef` | Yes | Reference to the VMImage resource |
| `networks` | `[]VMNetworkRef` | No | Network attachments |
| `disks` | `[]DiskSpec` | No | Additional disks |
| `userData` | `UserData` | No | Cloud-init configuration |
| `placement` | `Placement` | No | Placement hints |
| `powerState` | `string` | No | Desired power state (On/Off) |
| `tags` | `[]string` | No | Tags for organization |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `id` | `string` | Provider-specific VM identifier |
| `powerState` | `string` | Current power state |
| `ips` | `[]string` | Assigned IP addresses |
| `consoleURL` | `string` | Console access URL |
| `conditions` | `[]Condition` | Status conditions |
| `observedGeneration` | `int64` | Last observed generation |
| `lastTaskRef` | `string` | Reference to last async task |
| `provider` | `map[string]string` | Provider-specific details |

### Example

```yaml
apiVersion: infra.virtrigaud.io/v1alpha1
kind: VirtualMachine
metadata:
  name: demo-web-01
spec:
  providerRef:
    name: vsphere-prod
  classRef:
    name: small
  imageRef:
    name: ubuntu-22-template
  networks:
    - name: app-net
      ipPolicy: dhcp
  powerState: On
```

## VMClass

The `VMClass` CRD defines resource allocation for virtual machines.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `cpu` | `int32` | Yes | Number of virtual CPUs |
| `memoryMiB` | `int32` | Yes | Memory in MiB |
| `firmware` | `string` | No | Firmware type (BIOS/UEFI) |
| `diskDefaults` | `DiskDefaults` | No | Default disk settings |
| `guestToolsPolicy` | `string` | No | Guest tools policy |
| `extraConfig` | `map[string]string` | No | Provider-specific configuration |

### Example

```yaml
apiVersion: infra.virtrigaud.io/v1alpha1
kind: VMClass
metadata:
  name: small
spec:
  cpu: 2
  memoryMiB: 4096
  firmware: UEFI
  diskDefaults:
    type: thin
    sizeGiB: 40
```

## VMImage

The `VMImage` CRD defines base templates/images for virtual machines.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `vsphere` | `VSphereImageSpec` | No | vSphere-specific configuration |
| `libvirt` | `LibvirtImageSpec` | No | Libvirt-specific configuration |
| `prepare` | `ImagePrepare` | No | Image preparation options |

### Example

```yaml
apiVersion: infra.virtrigaud.io/v1alpha1
kind: VMImage
metadata:
  name: ubuntu-22-template
spec:
  vsphere:
    templateName: "tmpl-ubuntu-22.04-cloudimg"
  libvirt:
    url: "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
    format: qcow2
```

## VMNetworkAttachment

The `VMNetworkAttachment` CRD defines network configurations.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `vsphere` | `VSphereNetworkSpec` | No | vSphere-specific network config |
| `libvirt` | `LibvirtNetworkSpec` | No | Libvirt-specific network config |
| `ipPolicy` | `string` | No | IP assignment policy |
| `macAddress` | `string` | No | Static MAC address |

### Example

```yaml
apiVersion: infra.virtrigaud.io/v1alpha1
kind: VMNetworkAttachment
metadata:
  name: app-net
spec:
  vsphere:
    portgroup: "PG-App"
  ipPolicy: dhcp
```

## Provider

The `Provider` CRD configures hypervisor connection details.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | `string` | Yes | Provider type (vsphere/libvirt/etc) |
| `endpoint` | `string` | Yes | Provider endpoint URI |
| `credentialSecretRef` | `ObjectRef` | Yes | Secret containing credentials |
| `insecureSkipVerify` | `bool` | No | Skip TLS verification |
| `defaults` | `ProviderDefaults` | No | Default placement settings |
| `rateLimit` | `RateLimit` | No | API rate limiting |

### Example

```yaml
apiVersion: infra.virtrigaud.io/v1alpha1
kind: Provider
metadata:
  name: vsphere-prod
spec:
  type: vsphere
  endpoint: https://vcenter.example.com
  credentialSecretRef:
    name: vsphere-creds
  defaults:
    datastore: datastore1
    cluster: compute-cluster-a
```

## Common Types

### ObjectRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Object name |
| `namespace` | `string` | No | Object namespace |

### DiskSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sizeGiB` | `int32` | Yes | Disk size in GiB |
| `type` | `string` | No | Disk type |
| `name` | `string` | No | Disk name |

### UserData

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `cloudInit` | `CloudInitConfig` | No | Cloud-init configuration |

### CloudInitConfig

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `secretRef` | `ObjectRef` | No | Secret containing cloud-init data |
| `inline` | `string` | No | Inline cloud-init configuration |
