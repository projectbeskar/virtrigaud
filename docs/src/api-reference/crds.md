# API Reference

## Packages
- [infra.virtrigaud.io/v1beta1](#infravirtrigaudiov1beta1)


## infra.virtrigaud.io/v1beta1

Package v1beta1 contains API Schema definitions for the infra.virtrigaud.io v1beta1 API group

### Resource Types
- [Provider](#provider)
- [ProviderList](#providerlist)
- [VMClass](#vmclass)
- [VMClassList](#vmclasslist)
- [VMClone](#vmclone)
- [VMCloneList](#vmclonelist)
- [VMImage](#vmimage)
- [VMImageList](#vmimagelist)
- [VMMigration](#vmmigration)
- [VMMigrationList](#vmmigrationlist)
- [VMNetworkAttachment](#vmnetworkattachment)
- [VMNetworkAttachmentList](#vmnetworkattachmentlist)
- [VMPlacementPolicy](#vmplacementpolicy)
- [VMPlacementPolicyList](#vmplacementpolicylist)
- [VMSet](#vmset)
- [VMSetList](#vmsetlist)
- [VMSnapshot](#vmsnapshot)
- [VMSnapshotList](#vmsnapshotlist)
- [VirtualMachine](#virtualmachine)
- [VirtualMachineList](#virtualmachinelist)



#### AffinityRules



AffinityRules defines affinity placement rules



_Appears in:_
- [VMPlacementPolicySpec](#vmplacementpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vmAffinity` _[VMAffinity](#vmaffinity)_ | VMAffinity defines affinity rules between VMs |  |  |
| `hostAffinity` _[HostAffinityRule](#hostaffinityrule)_ | HostAffinity encourages VMs to be placed on the same host |  |  |
| `clusterAffinity` _[ClusterAffinityRule](#clusteraffinityrule)_ | ClusterAffinity encourages VMs to be placed in the same cluster |  |  |
| `datastoreAffinity` _[DatastoreAffinityRule](#datastoreaffinityrule)_ | DatastoreAffinity encourages VMs to be placed on the same datastore |  |  |
| `zoneAffinity` _[ZoneAffinityRule](#zoneaffinityrule)_ | ZoneAffinity encourages VMs to be placed in the same zone |  |  |
| `applicationAffinity` _[ApplicationAffinityRule](#applicationaffinityrule)_ | ApplicationAffinity encourages VMs from the same application to be co-located |  |  |


#### AntiAffinityRules



AntiAffinityRules defines anti-affinity placement rules



_Appears in:_
- [VMPlacementPolicySpec](#vmplacementpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vmAntiAffinity` _[VMAntiAffinity](#vmantiaffinity)_ | VMAntiAffinity defines anti-affinity rules between VMs |  |  |
| `hostAntiAffinity` _[HostAntiAffinityRule](#hostantiaffinityrule)_ | HostAntiAffinity prevents VMs from being placed on the same host |  |  |
| `clusterAntiAffinity` _[ClusterAntiAffinityRule](#clusterantiaffinityrule)_ | ClusterAntiAffinity prevents VMs from being placed in the same cluster |  |  |
| `datastoreAntiAffinity` _[DatastoreAntiAffinityRule](#datastoreantiaffinityrule)_ | DatastoreAntiAffinity prevents VMs from being placed on the same datastore |  |  |
| `zoneAntiAffinity` _[ZoneAntiAffinityRule](#zoneantiaffinityrule)_ | ZoneAntiAffinity prevents VMs from being placed in the same zone |  |  |
| `applicationAntiAffinity` _[ApplicationAntiAffinityRule](#applicationantiaffinityrule)_ | ApplicationAntiAffinity prevents VMs from different applications being co-located |  |  |


#### ApplicationAffinityRule



ApplicationAffinityRule defines application affinity rules



_Appears in:_
- [AffinityRules](#affinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if application affinity is enabled |  |  |
| `applicationLabel` _string_ | ApplicationLabel specifies the label key used to identify applications | app |  |
| `scope` _string_ | Scope defines the scope of the affinity rule |  | Enum: [strict preferred] <br /> |


#### ApplicationAntiAffinityRule



ApplicationAntiAffinityRule defines application anti-affinity rules



_Appears in:_
- [AntiAffinityRules](#antiaffinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if application anti-affinity is enabled |  |  |
| `applicationLabel` _string_ | ApplicationLabel specifies the label key used to identify applications | app |  |
| `scope` _string_ | Scope defines the scope of the anti-affinity rule |  | Enum: [strict preferred] <br /> |


#### BasicAuthConfig



BasicAuthConfig defines basic authentication configuration



_Appears in:_
- [HTTPAuthentication](#httpauthentication)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretRef` _[LocalObjectReference](#localobjectreference)_ | SecretRef references a secret containing username and password |  |  |
| `usernameKey` _string_ | UsernameKey is the key in the secret containing the username (default: username) | username |  |
| `passwordKey` _string_ | PasswordKey is the key in the secret containing the password (default: password) | password |  |


#### BearerTokenConfig



BearerTokenConfig defines bearer token authentication configuration



_Appears in:_
- [HTTPAuthentication](#httpauthentication)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretRef` _[LocalObjectReference](#localobjectreference)_ | SecretRef references a secret containing the bearer token |  |  |
| `tokenKey` _string_ | TokenKey is the key in the secret containing the token (default: token) | token |  |


#### BridgeConfig



BridgeConfig defines bridge configuration



_Appears in:_
- [LibvirtNetworkConfig](#libvirtnetworkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the bridge name |  | MaxLength: 15 <br /> |
| `stp` _boolean_ | STP enables Spanning Tree Protocol | false |  |
| `delay` _integer_ | Delay is the STP forwarding delay |  | Maximum: 30 <br />Minimum: 0 <br /> |


#### CertificateSpec



CertificateSpec defines a certificate to install



_Appears in:_
- [VMCustomization](#vmcustomization)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the certificate name |  | MaxLength: 255 <br /> |
| `data` _string_ | Data contains the certificate data (PEM format) |  |  |
| `secretRef` _[LocalObjectReference](#localobjectreference)_ | SecretRef references a secret containing the certificate |  |  |
| `secretKey` _string_ | SecretKey is the key in the secret containing the certificate | tls.crt |  |
| `store` _string_ | Store specifies the certificate store | root | Enum: [root ca my trust] <br /> |


#### ChecksumType

_Underlying type:_ _string_

ChecksumType represents the checksum algorithm

_Validation:_
- Enum: [md5 sha1 sha256 sha512]

_Appears in:_
- [HTTPImageSource](#httpimagesource)
- [LibvirtImageSource](#libvirtimagesource)
- [VSphereImageSource](#vsphereimagesource)

| Field | Description |
| --- | --- |
| `md5` | ChecksumTypeMD5 indicates MD5 checksum<br /> |
| `sha1` | ChecksumTypeSHA1 indicates SHA1 checksum<br /> |
| `sha256` | ChecksumTypeSHA256 indicates SHA256 checksum<br /> |
| `sha512` | ChecksumTypeSHA512 indicates SHA512 checksum<br /> |


#### ClientCertConfig



ClientCertConfig defines client certificate authentication configuration



_Appears in:_
- [HTTPAuthentication](#httpauthentication)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretRef` _[LocalObjectReference](#localobjectreference)_ | SecretRef references a secret containing the client certificate and key |  |  |
| `certKey` _string_ | CertKey is the key in the secret containing the certificate (default: tls.crt) | tls.crt |  |
| `keyKey` _string_ | KeyKey is the key in the secret containing the private key (default: tls.key) | tls.key |  |


#### CloneMetadata



CloneMetadata contains clone operation metadata



_Appears in:_
- [VMCloneSpec](#vmclonespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `purpose` _string_ | Purpose describes the purpose of the clone |  | Enum: [backup testing migration development production staging] <br /> |
| `createdBy` _string_ | CreatedBy identifies who or what created the clone |  | MaxLength: 255 <br /> |
| `project` _string_ | Project identifies the project this clone belongs to |  | MaxLength: 255 <br /> |
| `environment` _string_ | Environment specifies the environment |  | Enum: [dev staging prod test] <br /> |
| `tags` _object (keys:string, values:string)_ | Tags are key-value pairs for categorizing the clone |  | MaxProperties: 50 <br /> |


#### CloneOptions



CloneOptions defines cloning options



_Appears in:_
- [VMCloneSpec](#vmclonespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[CloneType](#clonetype)_ | Type specifies the clone type | FullClone | Enum: [FullClone LinkedClone InstantClone] <br /> |
| `powerOn` _boolean_ | PowerOn indicates whether to power on the cloned VM | false |  |
| `includeSnapshots` _boolean_ | IncludeSnapshots indicates whether to include snapshots in the clone | false |  |
| `parallel` _boolean_ | Parallel enables parallel disk cloning (if supported) | false |  |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | Timeout defines the maximum time to wait for clone completion | 30m |  |
| `retryPolicy` _[CloneRetryPolicy](#cloneretrypolicy)_ | RetryPolicy defines retry behavior for failed clones |  |  |
| `storage` _[CloneStorageOptions](#clonestorageoptions)_ | Storage defines storage-specific clone options |  |  |
| `performance` _[ClonePerformanceOptions](#cloneperformanceoptions)_ | Performance defines performance-related clone options |  |  |


#### ClonePerformanceOptions



ClonePerformanceOptions defines performance-related clone options



_Appears in:_
- [CloneOptions](#cloneoptions)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `priority` _string_ | Priority specifies the clone operation priority | Normal | Enum: [Low Normal High] <br /> |
| `ioThrottling` _boolean_ | IOThrottling enables I/O throttling during clone operations | false |  |
| `maxIOPS` _integer_ | MaxIOPS limits the maximum IOPS during clone operations |  | Maximum: 100000 <br />Minimum: 100 <br /> |
| `concurrentDisks` _integer_ | ConcurrentDisks limits the number of disks cloned concurrently | 2 | Maximum: 10 <br />Minimum: 1 <br /> |


#### ClonePhase

_Underlying type:_ _string_

ClonePhase represents the phase of a clone operation

_Validation:_
- Enum: [Pending Preparing Cloning Customizing Powering-On Ready Failed]

_Appears in:_
- [VMCloneStatus](#vmclonestatus)

| Field | Description |
| --- | --- |
| `Pending` | ClonePhasePending indicates the clone is waiting to be processed<br /> |
| `Preparing` | ClonePhasePreparing indicates the clone is being prepared<br /> |
| `Cloning` | ClonePhaseCloning indicates the clone operation is in progress<br /> |
| `Customizing` | ClonePhaseCustomizing indicates the clone is being customized<br /> |
| `Powering-On` | ClonePhasePoweringOn indicates the clone is being powered on<br /> |
| `Ready` | ClonePhaseReady indicates the clone is ready for use<br /> |
| `Failed` | ClonePhaseFailed indicates the clone operation failed<br /> |


#### CloneProgress



CloneProgress shows the clone operation progress



_Appears in:_
- [VMCloneStatus](#vmclonestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `totalDisks` _integer_ | TotalDisks is the total number of disks to clone |  |  |
| `completedDisks` _integer_ | CompletedDisks is the number of disks completed |  |  |
| `currentDisk` _[DiskCloneProgress](#diskcloneprogress)_ | CurrentDisk shows progress of the current disk being cloned |  |  |
| `overallPercentage` _integer_ | OverallPercentage is the overall completion percentage (0-100) |  | Maximum: 100 <br />Minimum: 0 <br /> |
| `eta` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | ETA is the estimated time to completion |  |  |


#### CloneRetryPolicy



CloneRetryPolicy defines retry behavior for failed clones



_Appears in:_
- [CloneOptions](#cloneoptions)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `maxRetries` _integer_ | MaxRetries is the maximum number of retry attempts | 3 | Maximum: 10 <br />Minimum: 0 <br /> |
| `retryDelay` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | RetryDelay is the delay between retry attempts | 5m |  |
| `backoffMultiplier` _integer_ | BackoffMultiplier is the multiplier for exponential backoff | 2 |  |


#### CloneSource



CloneSource defines the source for cloning



_Appears in:_
- [VMCloneSpec](#vmclonespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vmRef` _[LocalObjectReference](#localobjectreference)_ | VMRef references the source virtual machine to clone |  |  |
| `snapshotRef` _[LocalObjectReference](#localobjectreference)_ | SnapshotRef references a specific snapshot to clone from |  |  |
| `templateRef` _[ObjectRef](#objectref)_ | TemplateRef references a VM template to clone from |  |  |
| `imageRef` _[ObjectRef](#objectref)_ | ImageRef references a VM image to clone from |  |  |


#### CloneStorageOptions



CloneStorageOptions defines storage-specific clone options



_Appears in:_
- [CloneOptions](#cloneoptions)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `preferThinProvisioning` _boolean_ | PreferThinProvisioning prefers thin provisioning for cloned disks | true |  |
| `diskFormat` _[DiskType](#disktype)_ | DiskFormat specifies the preferred disk format for cloned disks |  | Enum: [thin thick eagerzeroedthick ssd hdd nvme] <br /> |
| `storageClass` _string_ | StorageClass specifies the storage class for cloned disks |  | MaxLength: 253 <br /> |
| `datastore` _string_ | Datastore specifies the target datastore for cloned disks |  | MaxLength: 255 <br /> |
| `folder` _string_ | Folder specifies the target folder for the cloned VM |  | MaxLength: 255 <br /> |
| `enableCompression` _boolean_ | EnableCompression enables compression during clone operations | false |  |


#### CloneType

_Underlying type:_ _string_

CloneType represents the type of clone operation

_Validation:_
- Enum: [FullClone LinkedClone InstantClone]

_Appears in:_
- [CloneOptions](#cloneoptions)
- [VMCloneStatus](#vmclonestatus)

| Field | Description |
| --- | --- |
| `FullClone` | CloneTypeFullClone creates a full independent clone<br /> |
| `LinkedClone` | CloneTypeLinkedClone creates a linked clone sharing storage with parent<br /> |
| `InstantClone` | CloneTypeInstantClone creates an instant clone (if supported)<br /> |


#### CloudInit



CloudInit defines cloud-init configuration



_Appears in:_
- [UserData](#userdata)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `inline` _string_ | Inline contains inline cloud-init data |  |  |
| `secretRef` _[LocalObjectReference](#localobjectreference)_ | SecretRef references a Secret containing cloud-init data |  |  |


#### ClusterAffinityRule



ClusterAffinityRule defines cluster affinity rules



_Appears in:_
- [AffinityRules](#affinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if cluster affinity is enabled |  |  |
| `preferredClusters` _string array_ | PreferredClusters lists preferred clusters |  | MaxItems: 20 <br /> |
| `scope` _string_ | Scope defines the scope of the affinity rule |  | Enum: [strict preferred] <br /> |


#### ClusterAntiAffinityRule



ClusterAntiAffinityRule defines cluster anti-affinity rules



_Appears in:_
- [AntiAffinityRules](#antiaffinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if cluster anti-affinity is enabled |  |  |
| `maxVMsPerCluster` _integer_ | MaxVMsPerCluster limits the number of VMs per cluster |  | Maximum: 10000 <br />Minimum: 1 <br /> |
| `scope` _string_ | Scope defines the scope of the anti-affinity rule |  | Enum: [strict preferred] <br /> |


#### ConnectionPooling



ConnectionPooling defines connection pooling settings



_Appears in:_
- [ProviderSpec](#providerspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `maxConnections` _integer_ | MaxConnections is the maximum number of connections to maintain | 10 | Maximum: 100 <br />Minimum: 1 <br /> |
| `maxIdleConnections` _integer_ | MaxIdleConnections is the maximum number of idle connections | 5 | Minimum: 1 <br /> |
| `connectionTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | ConnectionTimeout is the timeout for establishing connections | 30s |  |
| `idleTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | IdleTimeout is the timeout for idle connections | 5m |  |


#### ContentLibraryRef



ContentLibraryRef references a vSphere content library item



_Appears in:_
- [VSphereImageSource](#vsphereimagesource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `library` _string_ | Library is the name of the content library |  | MaxLength: 255 <br /> |
| `item` _string_ | Item is the name of the library item |  | MaxLength: 255 <br /> |
| `version` _string_ | Version specifies the item version (optional) |  |  |


#### CustomizationStatus



CustomizationStatus contains customization operation status



_Appears in:_
- [VMCloneStatus](#vmclonestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `started` _boolean_ | Started indicates if customization has started |  |  |
| `completed` _boolean_ | Completed indicates if customization has completed |  |  |
| `completedSteps` _string array_ | CompletedSteps lists completed customization steps |  |  |
| `failedSteps` _string array_ | FailedSteps lists failed customization steps |  |  |
| `currentStep` _string_ | CurrentStep is the current customization step |  |  |
| `message` _string_ | Message provides customization status details |  |  |


#### DHCPConfig



DHCPConfig defines DHCP configuration



_Appears in:_
- [IPAllocationConfig](#ipallocationconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clientID` _string_ | ClientID specifies the DHCP client ID |  | MaxLength: 255 <br /> |
| `hostname` _string_ | Hostname specifies the hostname to request |  | MaxLength: 255 <br /> |
| `options` _object (keys:string, values:string)_ | Options contains DHCP options |  | MaxProperties: 20 <br /> |


#### DNSConfig



DNSConfig defines DNS configuration



_Appears in:_
- [IPAllocationConfig](#ipallocationconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `servers` _string array_ | Servers contains DNS server addresses |  | MaxItems: 10 <br /> |
| `searchDomains` _string array_ | SearchDomains contains DNS search domains |  | MaxItems: 10 <br /> |
| `options` _object (keys:string, values:string)_ | Options contains DNS resolver options |  | MaxProperties: 10 <br /> |


#### DataVolumeImageSource



DataVolumeImageSource defines DataVolume-based image configuration



_Appears in:_
- [ImageSource](#imagesource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the DataVolume |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `namespace` _string_ | Namespace is the namespace of the DataVolume (defaults to image namespace) |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |


#### DatastoreAffinityRule



DatastoreAffinityRule defines datastore affinity rules



_Appears in:_
- [AffinityRules](#affinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if datastore affinity is enabled |  |  |
| `preferredDatastores` _string array_ | PreferredDatastores lists preferred datastores |  | MaxItems: 50 <br /> |
| `scope` _string_ | Scope defines the scope of the affinity rule |  | Enum: [strict preferred] <br /> |


#### DatastoreAntiAffinityRule



DatastoreAntiAffinityRule defines datastore anti-affinity rules



_Appears in:_
- [AntiAffinityRules](#antiaffinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if datastore anti-affinity is enabled |  |  |
| `maxVMsPerDatastore` _integer_ | MaxVMsPerDatastore limits the number of VMs per datastore |  | Maximum: 10000 <br />Minimum: 1 <br /> |
| `scope` _string_ | Scope defines the scope of the anti-affinity rule |  | Enum: [strict preferred] <br /> |


#### DiskCloneProgress



DiskCloneProgress shows the progress of cloning a single disk



_Appears in:_
- [CloneProgress](#cloneprogress)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the disk name |  |  |
| `totalBytes` _integer_ | TotalBytes is the total size of the disk |  |  |
| `completedBytes` _integer_ | CompletedBytes is the number of bytes completed |  |  |
| `percentage` _integer_ | Percentage is the completion percentage (0-100) |  | Maximum: 100 <br />Minimum: 0 <br /> |
| `transferRate` _integer_ | TransferRate is the current transfer rate in bytes per second |  |  |


#### DiskDefaults



DiskDefaults provides default disk settings



_Appears in:_
- [VMClassSpec](#vmclassspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[DiskType](#disktype)_ | Type specifies the default disk type | thin | Enum: [thin thick eagerzeroedthick ssd hdd nvme] <br /> |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | Size specifies the default root disk size | 40Gi |  |
| `iops` _integer_ | IOPS specifies the default IOPS limit |  | Maximum: 100000 <br />Minimum: 100 <br /> |
| `storageClass` _string_ | StorageClass specifies the default storage class |  | MaxLength: 253 <br /> |


#### DiskSpec



DiskSpec defines a disk configuration



_Appears in:_
- [MigrationTarget](#migrationtarget)
- [VMCloneTarget](#vmclonetarget)
- [VirtualMachineSpec](#virtualmachinespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the disk identifier |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `sizeGiB` _integer_ | SizeGiB is the size of the disk in GiB |  | Maximum: 65536 <br />Minimum: 1 <br /> |
| `type` _string_ | Type specifies the disk type (provider-specific) | thin | Enum: [thin thick eagerzeroedthick ssd hdd] <br /> |
| `expandPolicy` _string_ | ExpandPolicy defines how the disk can be expanded | Offline | Enum: [Online Offline] <br /> |
| `storageClass` _string_ | StorageClass specifies the storage class (optional) |  |  |


#### DiskType

_Underlying type:_ _string_

DiskType represents the type of disk provisioning

_Validation:_
- Enum: [thin thick eagerzeroedthick ssd hdd nvme]

_Appears in:_
- [CloneStorageOptions](#clonestorageoptions)
- [DiskDefaults](#diskdefaults)

| Field | Description |
| --- | --- |
| `thin` | DiskTypeThin indicates thin provisioned disks<br /> |
| `thick` | DiskTypeThick indicates thick provisioned disks<br /> |
| `eagerzeroedthick` | DiskTypeEagerZeroedThick indicates eager zeroed thick provisioned disks<br /> |
| `ssd` | DiskTypeSSD indicates SSD storage<br /> |
| `hdd` | DiskTypeHDD indicates HDD storage<br /> |
| `nvme` | DiskTypeNVMe indicates NVMe storage<br /> |


#### DistributedSwitchConfig



DistributedSwitchConfig defines distributed virtual switch configuration



_Appears in:_
- [VSphereNetworkConfig](#vspherenetworkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the distributed switch |  | MaxLength: 255 <br /> |
| `uuid` _string_ | UUID is the UUID of the distributed switch (optional) |  |  |
| `portgroupType` _string_ | PortgroupType specifies the type of portgroup |  | Enum: [ephemeral distributed] <br /> |


#### DomainJoinSpec



DomainJoinSpec defines domain join configuration



_Appears in:_
- [SysprepCustomization](#sysprepcustomization)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `domain` _string_ | Domain is the domain name to join |  | MaxLength: 255 <br /> |
| `username` _string_ | Username is the domain join username |  | MaxLength: 255 <br /> |
| `password` _[PasswordSpec](#passwordspec)_ | Password is the domain join password |  |  |
| `organizationalUnit` _string_ | OrganizationalUnit specifies the OU for the computer account |  | MaxLength: 500 <br /> |


#### EncryptionPolicy



EncryptionPolicy defines VM encryption settings



_Appears in:_
- [SecurityProfile](#securityprofile)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if encryption should be used | false |  |
| `keyProvider` _string_ | KeyProvider specifies the encryption key provider |  | Enum: [standard hardware external] <br /> |
| `requireEncryption` _boolean_ | RequireEncryption mandates encryption (fails if not available) | false |  |


#### ExecAction



ExecAction describes a command to be executed



_Appears in:_
- [LifecycleHandler](#lifecyclehandler)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `command` _string array_ | Command is the command line to execute |  | MinItems: 1 <br /> |


#### FirewallConfig



FirewallConfig defines firewall configuration



_Appears in:_
- [NetworkSecurityConfig](#networksecurityconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if firewall is enabled | false |  |
| `defaultPolicy` _string_ | DefaultPolicy specifies the default firewall policy | ACCEPT | Enum: [ACCEPT DROP REJECT] <br /> |
| `rules` _[FirewallRule](#firewallrule) array_ | Rules contains firewall rules |  | MaxItems: 100 <br /> |


#### FirewallRule



FirewallRule defines a firewall rule



_Appears in:_
- [FirewallConfig](#firewallconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the rule name |  | MaxLength: 255 <br /> |
| `action` _string_ | Action specifies the rule action |  | Enum: [ACCEPT DROP REJECT] <br /> |
| `direction` _string_ | Direction specifies the traffic direction |  | Enum: [in out inout] <br /> |
| `protocol` _string_ | Protocol specifies the protocol |  | Enum: [tcp udp icmp all] <br /> |
| `sourceCIDR` _string_ | SourceCIDR specifies the source CIDR |  |  |
| `destinationCIDR` _string_ | DestinationCIDR specifies the destination CIDR |  |  |
| `ports` _[PortRange](#portrange)_ | Ports specifies the port range |  |  |
| `priority` _integer_ | Priority specifies the rule priority |  | Maximum: 1000 <br />Minimum: 1 <br /> |


#### FirmwareType

_Underlying type:_ _string_

FirmwareType represents the firmware type for VMs

_Validation:_
- Enum: [BIOS UEFI EFI]

_Appears in:_
- [VMClassSpec](#vmclassspec)

| Field | Description |
| --- | --- |
| `BIOS` | FirmwareTypeBIOS indicates BIOS firmware<br /> |
| `UEFI` | FirmwareTypeUEFI indicates UEFI firmware<br /> |
| `EFI` | FirmwareTypeEFI indicates EFI firmware (alias for UEFI)<br /> |


#### GPUConfig



GPUConfig defines GPU configuration for a VM



_Appears in:_
- [VirtualMachineResources](#virtualmachineresources)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `count` _integer_ | Count specifies the number of GPUs to assign |  | Maximum: 8 <br />Minimum: 1 <br /> |
| `type` _string_ | Type specifies the GPU type (provider-specific) |  | Pattern: `^[a-zA-Z0-9-_]+$` <br /> |
| `memory` _integer_ | Memory specifies GPU memory in MiB |  | Minimum: 512 <br /> |


#### GuestCommand



GuestCommand defines a command to run in the guest OS



_Appears in:_
- [VMCustomization](#vmcustomization)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `command` _string_ | Command is the command to execute |  | MaxLength: 1000 <br /> |
| `arguments` _string array_ | Arguments contains command arguments |  | MaxItems: 20 <br /> |
| `workingDirectory` _string_ | WorkingDirectory specifies the working directory |  | MaxLength: 500 <br /> |
| `runAsUser` _string_ | RunAsUser specifies the user to run the command as |  | MaxLength: 255 <br /> |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | Timeout specifies the command timeout | 5m |  |
| `stage` _string_ | Stage specifies when to run the command | post-customization | Enum: [pre-customization post-customization first-boot] <br /> |


#### GuestToolsPolicy

_Underlying type:_ _string_

GuestToolsPolicy represents the guest tools installation policy

_Validation:_
- Enum: [install skip upgrade uninstall]

_Appears in:_
- [VMClassSpec](#vmclassspec)

| Field | Description |
| --- | --- |
| `install` | GuestToolsPolicyInstall installs guest tools if not present<br /> |
| `skip` | GuestToolsPolicySkip skips guest tools installation<br /> |
| `upgrade` | GuestToolsPolicyUpgrade upgrades guest tools if present<br /> |
| `uninstall` | GuestToolsPolicyUninstall removes guest tools if present<br /> |


#### HTTPAuthentication



HTTPAuthentication defines HTTP authentication options



_Appears in:_
- [HTTPImageSource](#httpimagesource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `basicAuth` _[BasicAuthConfig](#basicauthconfig)_ | BasicAuth contains basic authentication configuration |  |  |
| `bearer` _[BearerTokenConfig](#bearertokenconfig)_ | Bearer contains bearer token authentication |  |  |
| `clientCert` _[ClientCertConfig](#clientcertconfig)_ | ClientCert contains client certificate authentication |  |  |


#### HTTPGetAction



HTTPGetAction describes an HTTP GET request



_Appears in:_
- [LifecycleHandler](#lifecyclehandler)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `path` _string_ | Path is the HTTP path to access |  |  |
| `port` _integer_ | Port is the port to access |  | Maximum: 65535 <br />Minimum: 1 <br /> |
| `host` _string_ | Host name to connect to (defaults to VM IP) |  |  |
| `scheme` _string_ | Scheme to use for connecting (HTTP or HTTPS) | HTTP | Enum: [HTTP HTTPS] <br /> |


#### HTTPImageSource



HTTPImageSource defines HTTP/HTTPS download configuration



_Appears in:_
- [ImageSource](#imagesource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `url` _string_ | URL is the HTTP/HTTPS URL to download the image |  | Pattern: `^https?://.*` <br /> |
| `headers` _object (keys:string, values:string)_ | Headers contains HTTP headers to include in the request |  | MaxProperties: 20 <br /> |
| `checksum` _string_ | Checksum provides expected checksum for verification |  |  |
| `checksumType` _[ChecksumType](#checksumtype)_ | ChecksumType specifies the checksum algorithm | sha256 | Enum: [md5 sha1 sha256 sha512] <br /> |
| `authentication` _[HTTPAuthentication](#httpauthentication)_ | Authentication contains authentication configuration |  |  |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | Timeout specifies the download timeout | 30m |  |


#### HostAffinityRule



HostAffinityRule defines host affinity rules



_Appears in:_
- [AffinityRules](#affinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if host affinity is enabled |  |  |
| `preferredHosts` _string array_ | PreferredHosts lists preferred hosts |  | MaxItems: 50 <br /> |
| `scope` _string_ | Scope defines the scope of the affinity rule |  | Enum: [strict preferred] <br /> |


#### HostAntiAffinityRule



HostAntiAffinityRule defines host anti-affinity rules



_Appears in:_
- [AntiAffinityRules](#antiaffinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if host anti-affinity is enabled |  |  |
| `maxVMsPerHost` _integer_ | MaxVMsPerHost limits the number of VMs per host |  | Maximum: 1000 <br />Minimum: 1 <br /> |
| `scope` _string_ | Scope defines the scope of the anti-affinity rule |  | Enum: [strict preferred] <br /> |


#### IPAllocation



IPAllocation represents an IP allocation



_Appears in:_
- [VMNetworkAttachmentStatus](#vmnetworkattachmentstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vm` _string_ | VM is the VM name that has the IP allocated |  |  |
| `ip` _string_ | IP is the allocated IP address |  |  |
| `mac` _string_ | MAC is the allocated MAC address |  |  |
| `allocatedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | AllocatedAt is when the IP was allocated |  |  |
| `leaseExpiry` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LeaseExpiry is when the IP lease expires (for DHCP) |  |  |


#### IPAllocationConfig



IPAllocationConfig defines IP address allocation settings



_Appears in:_
- [VMNetworkAttachmentSpec](#vmnetworkattachmentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[IPAllocationType](#ipallocationtype)_ | Type specifies the IP allocation type | DHCP | Enum: [DHCP Static Pool None] <br /> |
| `staticConfig` _[StaticIPConfig](#staticipconfig)_ | StaticConfig contains static IP configuration |  |  |
| `poolConfig` _[IPPoolConfig](#ippoolconfig)_ | PoolConfig contains IP pool configuration |  |  |
| `dhcpConfig` _[DHCPConfig](#dhcpconfig)_ | DHCPConfig contains DHCP configuration |  |  |
| `dnsConfig` _[DNSConfig](#dnsconfig)_ | DNSConfig contains DNS configuration |  |  |


#### IPAllocationType

_Underlying type:_ _string_

IPAllocationType represents the type of IP allocation

_Validation:_
- Enum: [DHCP Static Pool None]

_Appears in:_
- [IPAllocationConfig](#ipallocationconfig)

| Field | Description |
| --- | --- |
| `DHCP` | IPAllocationTypeDHCP uses DHCP for IP allocation<br /> |
| `Static` | IPAllocationTypeStatic uses static IP allocation<br /> |
| `Pool` | IPAllocationTypePool uses an IP pool for allocation<br /> |
| `None` | IPAllocationTypeNone disables IP allocation<br /> |


#### IPPoolConfig



IPPoolConfig defines IP pool configuration



_Appears in:_
- [IPAllocationConfig](#ipallocationconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `poolRef` _[LocalObjectReference](#localobjectreference)_ | PoolRef references an IP pool resource |  |  |
| `preferredIP` _string_ | PreferredIP requests a preferred IP from the pool |  | Pattern: `^((25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)\.)\{3\}(25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)$` <br /> |


#### Ignition



Ignition defines Ignition configuration



_Appears in:_
- [UserData](#userdata)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `inline` _string_ | Inline contains inline Ignition data |  |  |
| `secretRef` _[LocalObjectReference](#localobjectreference)_ | SecretRef references a Secret containing Ignition data |  |  |


#### ImageFormat

_Underlying type:_ _string_

ImageFormat represents the format of a VM image

_Validation:_
- Enum: [qcow2 raw vmdk vhd vhdx iso ova ovf]

_Appears in:_
- [ImageOptimization](#imageoptimization)
- [LibvirtImageSource](#libvirtimagesource)
- [RegistryImageSource](#registryimagesource)
- [StoragePrepareOptions](#storageprepareoptions)
- [VMImageStatus](#vmimagestatus)

| Field | Description |
| --- | --- |
| `qcow2` | ImageFormatQCOW2 indicates QEMU QCOW2 format<br /> |
| `raw` | ImageFormatRaw indicates raw disk format<br /> |
| `vmdk` | ImageFormatVMDK indicates VMware VMDK format<br /> |
| `vhd` | ImageFormatVHD indicates Microsoft VHD format<br /> |
| `vhdx` | ImageFormatVHDX indicates Microsoft VHDX format<br /> |
| `iso` | ImageFormatISO indicates ISO format<br /> |
| `ova` | ImageFormatOVA indicates OVA format<br /> |
| `ovf` | ImageFormatOVF indicates OVF format<br /> |


#### ImageImportProgress



ImageImportProgress tracks the progress of image import operations



_Appears in:_
- [VMImageStatus](#vmimagestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `totalBytes` _integer_ | TotalBytes is the total size of the image being imported |  |  |
| `transferredBytes` _integer_ | TransferredBytes is the number of bytes transferred so far |  |  |
| `percentage` _integer_ | Percentage is the completion percentage (0-100) |  | Maximum: 100 <br />Minimum: 0 <br /> |
| `transferRate` _integer_ | TransferRate is the current transfer rate in bytes per second |  |  |
| `eta` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | ETA is the estimated time to completion |  |  |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | StartTime is when the import started |  |  |


#### ImageMetadata



ImageMetadata contains image metadata and annotations



_Appears in:_
- [VMImageSpec](#vmimagespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `displayName` _string_ | DisplayName is a human-readable name for the image |  | MaxLength: 255 <br /> |
| `description` _string_ | Description provides a description of the image |  | MaxLength: 1024 <br /> |
| `version` _string_ | Version specifies the image version |  | MaxLength: 100 <br /> |
| `architecture` _string_ | Architecture specifies the CPU architecture | amd64 | Enum: [amd64 arm64 x86_64 aarch64] <br /> |
| `tags` _object (keys:string, values:string)_ | Tags are key-value pairs for categorizing the image |  | MaxProperties: 50 <br /> |
| `annotations` _object (keys:string, values:string)_ | Annotations are additional metadata annotations |  | MaxProperties: 50 <br /> |


#### ImageMissingAction

_Underlying type:_ _string_

ImageMissingAction defines actions to take when an image is missing

_Validation:_
- Enum: [Import Fail Wait]

_Appears in:_
- [ImagePrepare](#imageprepare)

| Field | Description |
| --- | --- |
| `Import` | ImageMissingActionImport imports the missing image<br /> |
| `Fail` | ImageMissingActionFail fails when the image is missing<br /> |
| `Wait` | ImageMissingActionWait waits for the image to become available<br /> |


#### ImageOptimization



ImageOptimization defines image optimization options



_Appears in:_
- [ImagePrepare](#imageprepare)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enableCompression` _boolean_ | EnableCompression enables image compression | false |  |
| `removeUnusedSpace` _boolean_ | RemoveUnusedSpace removes unused space from the image | false |  |
| `convertFormat` _[ImageFormat](#imageformat)_ | ConvertFormat converts the image to a more optimal format |  | Enum: [qcow2 raw vmdk vhd vhdx iso ova ovf] <br /> |
| `enableDeltaSync` _boolean_ | EnableDeltaSync enables delta synchronization for updates | false |  |


#### ImagePhase

_Underlying type:_ _string_

ImagePhase represents the phase of image preparation

_Validation:_
- Enum: [Pending Downloading Importing Converting Optimizing Ready Failed]

_Appears in:_
- [VMImageStatus](#vmimagestatus)

| Field | Description |
| --- | --- |
| `Pending` | ImagePhasePending indicates the image is waiting to be processed<br /> |
| `Downloading` | ImagePhaseDownloading indicates the image is being downloaded<br /> |
| `Importing` | ImagePhaseImporting indicates the image is being imported<br /> |
| `Converting` | ImagePhaseConverting indicates the image is being converted<br /> |
| `Optimizing` | ImagePhaseOptimizing indicates the image is being optimized<br /> |
| `Ready` | ImagePhaseReady indicates the image is ready for use<br /> |
| `Failed` | ImagePhaseFailed indicates the image preparation failed<br /> |


#### ImagePrepare



ImagePrepare defines optional image preparation steps



_Appears in:_
- [VMImageSpec](#vmimagespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `onMissing` _[ImageMissingAction](#imagemissingaction)_ | OnMissing defines the action to take when image is missing | Import | Enum: [Import Fail Wait] <br /> |
| `validateChecksum` _boolean_ | ValidateChecksum validates the image checksum | true |  |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | Timeout defines the maximum time to wait for preparation | 30m |  |
| `retries` _integer_ | Retries defines the number of retry attempts for failed operations | 3 | Maximum: 10 <br />Minimum: 0 <br /> |
| `force` _boolean_ | Force forces re-import even if image exists |  |  |
| `storage` _[StoragePrepareOptions](#storageprepareoptions)_ | Storage defines storage-specific preparation options |  |  |
| `optimization` _[ImageOptimization](#imageoptimization)_ | Optimization defines image optimization options |  |  |


#### ImageSource



ImageSource defines the source of the VM image



_Appears in:_
- [VMImageSpec](#vmimagespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vsphere` _[VSphereImageSource](#vsphereimagesource)_ | VSphere contains vSphere-specific image configuration |  |  |
| `libvirt` _[LibvirtImageSource](#libvirtimagesource)_ | Libvirt contains Libvirt-specific image configuration |  |  |
| `http` _[HTTPImageSource](#httpimagesource)_ | HTTP contains HTTP/HTTPS download configuration |  |  |
| `registry` _[RegistryImageSource](#registryimagesource)_ | Registry contains container registry image configuration |  |  |
| `dataVolume` _[DataVolumeImageSource](#datavolumeimagesource)_ | DataVolume contains DataVolume-based image configuration |  |  |
| `proxmox` _[ProxmoxImageSource](#proxmoximagesource)_ | Proxmox contains Proxmox VE-specific image configuration |  |  |


#### ImportedDiskRef



ImportedDiskRef references a disk that was imported via migration or other means.
This allows VMs to be created from pre-existing disk images rather than templates.



_Appears in:_
- [VirtualMachineSpec](#virtualmachinespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `diskID` _string_ | DiskID is the provider-specific disk identifier.<br />For libvirt, this is typically the volume name. |  | MaxLength: 253 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `path` _string_ | Path is the optional disk path on the provider (e.g., /var/lib/libvirt/images/disk.qcow2).<br />If not specified, the provider will determine the path based on DiskID. |  |  |
| `format` _string_ | Format specifies the disk format (qcow2, vmdk, raw, etc.). | qcow2 | Enum: [qcow2 vmdk raw vdi vhdx] <br /> |
| `source` _string_ | Source indicates where the disk came from. |  | Enum: [migration clone import snapshot manual] <br /> |
| `migrationRef` _[LocalObjectReference](#localobjectreference)_ | MigrationRef references the VMMigration that imported this disk.<br />This provides traceability and audit trail for migrated disks. |  |  |
| `sizeGiB` _integer_ | SizeGiB specifies the expected disk size in GiB.<br />Used for validation and capacity planning. |  | Minimum: 1 <br /> |


#### KernelInfo



KernelInfo contains kernel information



_Appears in:_
- [OSDistribution](#osdistribution)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `version` _string_ | Version is the kernel version |  |  |
| `type` _string_ | Type is the kernel type |  | Enum: [linux windows freebsd other] <br /> |


#### LibvirtImageSource



LibvirtImageSource defines Libvirt-specific image configuration



_Appears in:_
- [ImageSource](#imagesource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `path` _string_ | Path specifies the path to the image file on the host |  |  |
| `url` _string_ | URL provides a URL to download the image |  | Pattern: `^(https?\|ftp)://.*` <br /> |
| `format` _[ImageFormat](#imageformat)_ | Format specifies the image format | qcow2 | Enum: [qcow2 raw vmdk vhd vhdx iso ova ovf] <br /> |
| `checksum` _string_ | Checksum provides expected checksum for verification |  |  |
| `checksumType` _[ChecksumType](#checksumtype)_ | ChecksumType specifies the checksum algorithm | sha256 | Enum: [md5 sha1 sha256 sha512] <br /> |
| `storagePool` _string_ | StoragePool specifies the libvirt storage pool |  | MaxLength: 255 <br /> |


#### LibvirtNetworkConfig



LibvirtNetworkConfig defines Libvirt-specific network configuration



_Appears in:_
- [NetworkConfig](#networkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `networkName` _string_ | NetworkName specifies the Libvirt network name |  | MaxLength: 255 <br /> |
| `bridge` _[BridgeConfig](#bridgeconfig)_ | Bridge specifies the bridge configuration |  |  |
| `model` _string_ | Model specifies the network device model | virtio | Enum: [virtio e1000 e1000e rtl8139 pcnet ne2k_pci] <br /> |
| `driver` _[NetworkDriverConfig](#networkdriverconfig)_ | Driver specifies the network driver configuration |  |  |
| `filterRef` _[NetworkFilterRef](#networkfilterref)_ | FilterRef specifies network filter configuration |  |  |


#### LibvirtStorageOptions



LibvirtStorageOptions defines Libvirt storage preparation options



_Appears in:_
- [StoragePrepareOptions](#storageprepareoptions)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `storagePool` _string_ | StoragePool specifies the target storage pool for import |  | MaxLength: 255 <br /> |
| `allocationPolicy` _string_ | AllocationPolicy defines how storage is allocated |  | Enum: [eager lazy] <br /> |
| `preallocation` _string_ | Preallocation specifies preallocation mode |  | Enum: [off metadata falloc full] <br /> |


#### LifecycleHandler



LifecycleHandler defines a specific action that should be taken



_Appears in:_
- [VirtualMachineLifecycle](#virtualmachinelifecycle)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `exec` _[ExecAction](#execaction)_ | Exec specifies a command to execute |  |  |
| `httpGet` _[HTTPGetAction](#httpgetaction)_ | HTTPGet specifies an HTTP GET request |  |  |
| `snapshot` _[SnapshotAction](#snapshotaction)_ | Snapshot specifies a snapshot to create |  |  |


#### LocalObjectReference



LocalObjectReference represents a reference to an object in the same namespace



_Appears in:_
- [BasicAuthConfig](#basicauthconfig)
- [BearerTokenConfig](#bearertokenconfig)
- [CertificateSpec](#certificatespec)
- [ClientCertConfig](#clientcertconfig)
- [CloneSource](#clonesource)
- [CloudInit](#cloudinit)
- [IPPoolConfig](#ippoolconfig)
- [Ignition](#ignition)
- [ImportedDiskRef](#importeddiskref)
- [MigrationSource](#migrationsource)
- [MigrationTarget](#migrationtarget)
- [NetworkEncryptionConfig](#networkencryptionconfig)
- [PasswordSpec](#passwordspec)
- [RegistryImageSource](#registryimagesource)
- [SnapshotEncryption](#snapshotencryption)
- [VMCloneStatus](#vmclonestatus)
- [VMCloneTarget](#vmclonetarget)
- [VMMigrationStatus](#vmmigrationstatus)
- [VMPlacementPolicyStatus](#vmplacementpolicystatus)
- [VMSnapshotOperation](#vmsnapshotoperation)
- [VMSnapshotSpec](#vmsnapshotspec)
- [VSphereImageSource](#vsphereimagesource)
- [VirtualMachineSpec](#virtualmachinespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the referenced object |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |


#### MigrationDiskInfo



MigrationDiskInfo contains information about the migrated disk



_Appears in:_
- [VMMigrationStatus](#vmmigrationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `sourceDiskID` _string_ | SourceDiskID is the source disk identifier |  |  |
| `sourceFormat` _string_ | SourceFormat is the source disk format |  |  |
| `sourceSize` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | SourceSize is the source disk size in bytes |  |  |
| `targetDiskID` _string_ | TargetDiskID is the target disk identifier |  |  |
| `targetFormat` _string_ | TargetFormat is the target disk format |  |  |
| `targetSize` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | TargetSize is the target disk size in bytes |  |  |
| `checksum` _string_ | Checksum is the SHA256 checksum of the disk |  |  |
| `sourceChecksum` _string_ | SourceChecksum is the SHA256 checksum of the source disk |  |  |
| `targetChecksum` _string_ | TargetChecksum is the SHA256 checksum of the target disk |  |  |


#### MigrationMetadata



MigrationMetadata contains migration metadata



_Appears in:_
- [VMMigrationSpec](#vmmigrationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `purpose` _string_ | Purpose describes the purpose of the migration |  | Enum: [disaster-recovery cloud-migration provider-change testing maintenance] <br /> |
| `createdBy` _string_ | CreatedBy identifies who or what created the migration |  | MaxLength: 255 <br /> |
| `project` _string_ | Project identifies the project this migration belongs to |  | MaxLength: 255 <br /> |
| `environment` _string_ | Environment specifies the environment |  | Enum: [dev staging prod test] <br /> |
| `tags` _object (keys:string, values:string)_ | Tags are key-value pairs for categorizing the migration |  | MaxProperties: 50 <br /> |


#### MigrationOptions



MigrationOptions defines migration options



_Appears in:_
- [VMMigrationSpec](#vmmigrationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `diskFormat` _string_ | DiskFormat specifies the desired disk format for the target |  | Enum: [qcow2 vmdk raw] <br /> |
| `compress` _boolean_ | Compress enables compression during transfer | false |  |
| `verifyChecksums` _boolean_ | VerifyChecksums enables checksum verification | true |  |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | Timeout defines the maximum time for the entire migration | 4h |  |
| `retryPolicy` _[MigrationRetryPolicy](#migrationretrypolicy)_ | RetryPolicy defines retry behavior for failed operations |  |  |
| `cleanupPolicy` _string_ | CleanupPolicy defines cleanup behavior | OnSuccess | Enum: [Always OnSuccess Never] <br /> |
| `validationChecks` _[ValidationChecks](#validationchecks)_ | ValidationChecks defines validation checks to perform |  |  |


#### MigrationPhase

_Underlying type:_ _string_

MigrationPhase represents the phase of a migration operation

_Validation:_
- Enum: [Pending Validating Snapshotting Exporting Transferring Converting Importing Creating Validating-Target Ready Failed]

_Appears in:_
- [MigrationProgress](#migrationprogress)
- [VMMigrationStatus](#vmmigrationstatus)

| Field | Description |
| --- | --- |
| `Pending` | MigrationPhasePending indicates the migration is waiting to be processed<br /> |
| `Validating` | MigrationPhaseValidating indicates the migration is being validated<br /> |
| `Snapshotting` | MigrationPhaseSnapshotting indicates a snapshot is being created<br /> |
| `Exporting` | MigrationPhaseExporting indicates the disk is being exported<br /> |
| `Transferring` | MigrationPhaseTransferring indicates the disk is being transferred<br /> |
| `Converting` | MigrationPhaseConverting indicates the disk format is being converted<br /> |
| `Importing` | MigrationPhaseImporting indicates the disk is being imported<br /> |
| `Creating` | MigrationPhaseCreating indicates the target VM is being created<br /> |
| `Validating-Target` | MigrationPhaseValidatingTarget indicates the target VM is being validated<br /> |
| `Ready` | MigrationPhaseReady indicates the migration is complete<br /> |
| `Failed` | MigrationPhaseFailed indicates the migration failed<br /> |


#### MigrationProgress



MigrationProgress shows the migration operation progress



_Appears in:_
- [VMMigrationStatus](#vmmigrationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `currentPhase` _[MigrationPhase](#migrationphase)_ | CurrentPhase is the current phase being executed |  | Enum: [Pending Validating Snapshotting Exporting Transferring Converting Importing Creating Validating-Target Ready Failed] <br /> |
| `totalBytes` _integer_ | TotalBytes is the total bytes to transfer |  |  |
| `transferredBytes` _integer_ | TransferredBytes is the bytes transferred so far |  |  |
| `percentage` _integer_ | Percentage is the overall completion percentage (0-100) |  | Maximum: 100 <br />Minimum: 0 <br /> |
| `eta` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | ETA is the estimated time to completion |  |  |
| `transferRate` _integer_ | TransferRate is the current transfer rate in bytes per second |  |  |
| `phaseStartTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | PhaseStartTime is when the current phase started |  |  |


#### MigrationRetryPolicy



MigrationRetryPolicy defines retry behavior for failed operations



_Appears in:_
- [MigrationOptions](#migrationoptions)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `maxRetries` _integer_ | MaxRetries is the maximum number of retry attempts | 3 | Maximum: 10 <br />Minimum: 0 <br /> |
| `retryDelay` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | RetryDelay is the delay between retry attempts | 5m |  |
| `backoffMultiplier` _integer_ | BackoffMultiplier is the multiplier for exponential backoff | 2 | Maximum: 10 <br />Minimum: 1 <br /> |


#### MigrationSource



MigrationSource defines the source VM for migration



_Appears in:_
- [VMMigrationSpec](#vmmigrationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vmRef` _[LocalObjectReference](#localobjectreference)_ | VMRef references the source virtual machine |  |  |
| `providerRef` _[ObjectRef](#objectref)_ | ProviderRef explicitly specifies the source provider (optional, auto-detected from VM) |  |  |
| `snapshotRef` _[LocalObjectReference](#localobjectreference)_ | SnapshotRef references a specific snapshot to migrate from |  |  |
| `createSnapshot` _boolean_ | CreateSnapshot indicates whether to create a snapshot before migration | true |  |
| `powerOffBeforeMigration` _boolean_ | PowerOffBeforeMigration ensures VM is powered off before migration | false |  |
| `deleteAfterMigration` _boolean_ | DeleteAfterMigration deletes source VM after successful migration | false |  |


#### MigrationStorage



MigrationStorage defines storage backend configuration



_Appears in:_
- [VMMigrationSpec](#vmmigrationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type specifies the storage backend type | pvc | Enum: [pvc] <br /> |
| `pvc` _[PVCStorageConfig](#pvcstorageconfig)_ | PVC specifies PVC-based storage configuration |  |  |


#### MigrationStorageInfo



MigrationStorageInfo contains information about intermediate storage



_Appears in:_
- [VMMigrationStatus](#vmmigrationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `url` _string_ | URL is the intermediate storage URL |  |  |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | Size is the size of data in intermediate storage |  |  |
| `uploadedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | UploadedAt is when the data was uploaded |  |  |
| `cleanedUp` _boolean_ | CleanedUp indicates if intermediate storage was cleaned up |  |  |


#### MigrationTarget



MigrationTarget defines the target provider and VM configuration



_Appears in:_
- [VMMigrationSpec](#vmmigrationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name for the target VM |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `namespace` _string_ | Namespace is the namespace for the target VM (defaults to source namespace) |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `providerRef` _[ObjectRef](#objectref)_ | ProviderRef references the target provider |  |  |
| `classRef` _[LocalObjectReference](#localobjectreference)_ | ClassRef references the VM class for resource allocation |  |  |
| `imageRef` _[LocalObjectReference](#localobjectreference)_ | ImageRef references the VM image (usually not needed for migration) |  |  |
| `networks` _[VMNetworkRef](#vmnetworkref) array_ | Networks defines network configuration for target VM |  | MaxItems: 10 <br /> |
| `disks` _[DiskSpec](#diskspec) array_ | Disks defines disk configuration overrides |  | MaxItems: 20 <br /> |
| `placementRef` _[LocalObjectReference](#localobjectreference)_ | PlacementRef references placement policy for the target VM |  |  |
| `powerOn` _boolean_ | PowerOn indicates whether to power on the target VM after migration | false |  |
| `labels` _object (keys:string, values:string)_ | Labels defines labels to apply to the target VM |  | MaxProperties: 50 <br /> |
| `annotations` _object (keys:string, values:string)_ | Annotations defines annotations to apply to the target VM |  | MaxProperties: 50 <br /> |


#### NetworkAttachmentPhase

_Underlying type:_ _string_

NetworkAttachmentPhase represents the phase of network attachment

_Validation:_
- Enum: [Pending Configuring Ready Failed]

_Appears in:_
- [VMNetworkAttachmentStatus](#vmnetworkattachmentstatus)

| Field | Description |
| --- | --- |
| `Pending` | NetworkAttachmentPhasePending indicates the network is being prepared<br /> |
| `Configuring` | NetworkAttachmentPhaseConfiguring indicates the network is being configured<br /> |
| `Ready` | NetworkAttachmentPhaseReady indicates the network is ready<br /> |
| `Failed` | NetworkAttachmentPhaseFailed indicates the network configuration failed<br /> |


#### NetworkConfig



NetworkConfig defines the underlying network configuration



_Appears in:_
- [VMNetworkAttachmentSpec](#vmnetworkattachmentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vsphere` _[VSphereNetworkConfig](#vspherenetworkconfig)_ | VSphere contains vSphere-specific network configuration |  |  |
| `libvirt` _[LibvirtNetworkConfig](#libvirtnetworkconfig)_ | Libvirt contains Libvirt-specific network configuration |  |  |
| `proxmox` _[ProxmoxNetworkConfig](#proxmoxnetworkconfig)_ | Proxmox contains Proxmox VE-specific network configuration |  |  |
| `type` _[NetworkType](#networktype)_ | Type specifies the network type | bridged | Enum: [bridged nat isolated host-only external] <br /> |
| `mtu` _integer_ | MTU specifies the Maximum Transmission Unit | 1500 | Maximum: 9000 <br />Minimum: 68 <br /> |


#### NetworkCustomization



NetworkCustomization defines network-specific customization



_Appears in:_
- [VMCustomization](#vmcustomization)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name identifies the network to customize |  | MaxLength: 255 <br /> |
| `ipAddress` _string_ | IPAddress sets a static IP address |  | Pattern: `^((25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)\.)\{3\}(25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)$` <br /> |
| `subnetMask` _string_ | SubnetMask sets the subnet mask |  | Pattern: `^((25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)\.)\{3\}(25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)$` <br /> |
| `gateway` _string_ | Gateway sets the network gateway |  | Pattern: `^((25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)\.)\{3\}(25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)$` <br /> |
| `dns` _string array_ | DNS sets DNS servers |  | MaxItems: 5 <br /> |
| `macAddress` _string_ | MACAddress sets a custom MAC address |  | Pattern: `^([0-9A-Fa-f]\{2\}[:-])\{5\}([0-9A-Fa-f]\{2\})$` <br /> |
| `dhcp` _boolean_ | DHCP enables DHCP for this network |  |  |


#### NetworkDriverConfig



NetworkDriverConfig defines network driver configuration



_Appears in:_
- [LibvirtNetworkConfig](#libvirtnetworkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the driver name |  | Enum: [kvm vfio uio] <br /> |
| `queues` _integer_ | Queues specifies the number of queues |  | Maximum: 16 <br />Minimum: 1 <br /> |
| `txMode` _string_ | TxMode specifies the TX mode |  | Enum: [iothread timer] <br /> |
| `ioEventFD` _boolean_ | IOEventFD enables IO event file descriptor |  |  |
| `eventIDX` _boolean_ | EventIDX enables event index |  |  |


#### NetworkEncryptionConfig



NetworkEncryptionConfig defines network encryption settings



_Appears in:_
- [NetworkSecurityConfig](#networksecurityconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if encryption is enabled | false |  |
| `protocol` _string_ | Protocol specifies the encryption protocol |  | Enum: [ipsec wireguard openvpn] <br /> |
| `keyRef` _[LocalObjectReference](#localobjectreference)_ | KeyRef references encryption keys |  |  |


#### NetworkFilterRef



NetworkFilterRef references a network filter



_Appears in:_
- [LibvirtNetworkConfig](#libvirtnetworkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `filter` _string_ | Filter is the filter name |  | MaxLength: 255 <br /> |
| `parameters` _object (keys:string, values:string)_ | Parameters contains filter parameters |  | MaxProperties: 20 <br /> |


#### NetworkIsolationConfig



NetworkIsolationConfig defines network isolation settings



_Appears in:_
- [NetworkSecurityConfig](#networksecurityconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _string_ | Mode specifies the isolation mode |  | Enum: [none strict custom] <br /> |
| `allowedNetworks` _string array_ | AllowedNetworks contains allowed network CIDRs |  | MaxItems: 50 <br /> |
| `deniedNetworks` _string array_ | DeniedNetworks contains denied network CIDRs |  | MaxItems: 50 <br /> |


#### NetworkMetadata



NetworkMetadata contains network metadata and labels



_Appears in:_
- [VMNetworkAttachmentSpec](#vmnetworkattachmentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `displayName` _string_ | DisplayName is a human-readable name |  | MaxLength: 255 <br /> |
| `description` _string_ | Description provides a description |  | MaxLength: 1024 <br /> |
| `environment` _string_ | Environment specifies the environment (dev, staging, prod) |  | Enum: [dev staging prod test] <br /> |
| `tags` _object (keys:string, values:string)_ | Tags are key-value pairs for categorizing |  | MaxProperties: 50 <br /> |


#### NetworkQoSConfig



NetworkQoSConfig defines Quality of Service settings



_Appears in:_
- [VMNetworkAttachmentSpec](#vmnetworkattachmentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ingressLimit` _integer_ | IngressLimit limits inbound traffic in bits per second |  | Minimum: 1000 <br /> |
| `egressLimit` _integer_ | EgressLimit limits outbound traffic in bits per second |  | Minimum: 1000 <br /> |
| `priority` _integer_ | Priority specifies traffic priority |  | Maximum: 7 <br />Minimum: 0 <br /> |
| `dscp` _integer_ | DSCP specifies DSCP marking |  | Maximum: 63 <br />Minimum: 0 <br /> |


#### NetworkSecurityConfig



NetworkSecurityConfig defines network security settings



_Appears in:_
- [VMNetworkAttachmentSpec](#vmnetworkattachmentspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `firewall` _[FirewallConfig](#firewallconfig)_ | Firewall contains firewall configuration |  |  |
| `isolation` _[NetworkIsolationConfig](#networkisolationconfig)_ | Isolation contains network isolation settings |  |  |
| `encryption` _[NetworkEncryptionConfig](#networkencryptionconfig)_ | Encryption contains network encryption settings |  |  |


#### NetworkType

_Underlying type:_ _string_

NetworkType represents the type of network

_Validation:_
- Enum: [bridged nat isolated host-only external]

_Appears in:_
- [NetworkConfig](#networkconfig)

| Field | Description |
| --- | --- |
| `bridged` | NetworkTypeBridged indicates a bridged network<br /> |
| `nat` | NetworkTypeNAT indicates a NAT network<br /> |
| `isolated` | NetworkTypeIsolated indicates an isolated network<br /> |
| `host-only` | NetworkTypeHostOnly indicates a host-only network<br /> |
| `external` | NetworkTypeExternal indicates an external network<br /> |


#### NetworkUsageStats



NetworkUsageStats represents network usage statistics



_Appears in:_
- [ProviderResourceUsage](#providerresourceusage)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `bytesReceived` _integer_ | BytesReceived is the total bytes received |  |  |
| `bytesSent` _integer_ | BytesSent is the total bytes sent |  |  |
| `packetsReceived` _integer_ | PacketsReceived is the total packets received |  |  |
| `packetsSent` _integer_ | PacketsSent is the total packets sent |  |  |


#### OSDistribution



OSDistribution contains OS distribution information



_Appears in:_
- [VMImageSpec](#vmimagespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the OS distribution name |  | Enum: [ubuntu centos rhel fedora debian suse windows freebsd coreos other] <br /> |
| `version` _string_ | Version is the distribution version |  | MaxLength: 100 <br /> |
| `variant` _string_ | Variant is the distribution variant (e.g., server, desktop) |  | MaxLength: 100 <br /> |
| `family` _string_ | Family is the OS family |  | Enum: [linux windows bsd other] <br /> |
| `kernel` _[KernelInfo](#kernelinfo)_ | Kernel specifies kernel information |  |  |


#### ObjectRef



ObjectRef represents a reference to another object



_Appears in:_
- [CloneSource](#clonesource)
- [MigrationSource](#migrationsource)
- [MigrationTarget](#migrationtarget)
- [ProviderSpec](#providerspec)
- [VMCloneTarget](#vmclonetarget)
- [VMNetworkRef](#vmnetworkref)
- [VirtualMachineSpec](#virtualmachinespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the referenced object |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `namespace` _string_ | Namespace of the referenced object (defaults to current namespace) |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |


#### PVCStorageConfig



PVCStorageConfig defines PVC storage configuration



_Appears in:_
- [MigrationStorage](#migrationstorage)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of an existing PVC to use for migration storage<br />If not specified, a temporary PVC will be created |  |  |
| `storageClassName` _string_ | StorageClassName for auto-created PVC<br />Required if Name is not specified |  |  |
| `size` _string_ | Size for auto-created PVC (e.g., "100Gi")<br />Required if Name is not specified |  | Pattern: `^[0-9]+(\.[0-9]+)?(Ei?\|Pi?\|Ti?\|Gi?\|Mi?\|Ki?)$` <br /> |
| `accessMode` _string_ | AccessMode for auto-created PVC | ReadWriteMany | Enum: [ReadWriteOnce ReadWriteMany ReadOnlyMany] <br /> |
| `mountPath` _string_ | MountPath within pods where PVC is mounted | /mnt/migration-storage |  |


#### PasswordSpec



PasswordSpec defines password configuration



_Appears in:_
- [DomainJoinSpec](#domainjoinspec)
- [SysprepCustomization](#sysprepcustomization)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `value` _string_ | Value is the plaintext password (not recommended for production) |  |  |
| `secretRef` _[LocalObjectReference](#localobjectreference)_ | SecretRef references a secret containing the password |  |  |
| `secretKey` _string_ | SecretKey is the key in the secret containing the password | password |  |


#### PerformanceProfile



PerformanceProfile defines performance-related settings



_Appears in:_
- [VMClassSpec](#vmclassspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `latencySensitivity` _string_ | LatencySensitivity configures latency sensitivity | normal | Enum: [low normal high] <br /> |
| `cpuHotAddEnabled` _boolean_ | CPUHotAddEnabled allows adding CPUs while VM is running | false |  |
| `memoryHotAddEnabled` _boolean_ | MemoryHotAddEnabled allows adding memory while VM is running | false |  |
| `virtualizationBasedSecurity` _boolean_ | VirtualizationBasedSecurity enables VBS features | false |  |
| `nestedVirtualization` _boolean_ | NestedVirtualization enables nested virtualization | false |  |
| `hyperThreadingPolicy` _string_ | HyperThreadingPolicy controls hyperthreading usage | auto | Enum: [auto prefer avoid require] <br /> |


#### PersistentVolumeClaimRetentionPolicyType

_Underlying type:_ _string_

PersistentVolumeClaimRetentionPolicyType defines the retention policy type

_Validation:_
- Enum: [Retain Delete]

_Appears in:_
- [VMSetPersistentVolumeClaimRetentionPolicy](#vmsetpersistentvolumeclaimretentionpolicy)

| Field | Description |
| --- | --- |
| `Retain` | RetainPersistentVolumeClaimRetentionPolicyType retains PVCs<br /> |
| `Delete` | DeletePersistentVolumeClaimRetentionPolicyType deletes PVCs<br /> |




#### PersistentVolumeClaimTemplate



PersistentVolumeClaimTemplate describes a PVC template for VMSet VMs



_Appears in:_
- [VMSetSpec](#vmsetspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[PersistentVolumeClaimSpec](#persistentvolumeclaimspec)_ | Spec is the desired characteristics of the volume |  |  |


#### Placement



Placement provides hints for VM placement



_Appears in:_
- [VirtualMachineSpec](#virtualmachinespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cluster` _string_ | Cluster specifies the target cluster |  |  |
| `host` _string_ | Host specifies the target host |  |  |
| `datastore` _string_ | Datastore specifies the target datastore |  |  |
| `folder` _string_ | Folder specifies the target folder |  |  |
| `resourcePool` _string_ | ResourcePool specifies the target resource pool |  |  |


#### PlacementConstraints



PlacementConstraints defines resource placement constraints



_Appears in:_
- [VMPlacementPolicySpec](#vmplacementpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusters` _string array_ | Clusters specifies allowed clusters for VM placement |  | MaxItems: 50 <br /> |
| `datastores` _string array_ | Datastores specifies allowed datastores for VM placement |  | MaxItems: 100 <br /> |
| `hosts` _string array_ | Hosts specifies allowed hosts for VM placement |  | MaxItems: 200 <br /> |
| `folders` _string array_ | Folders specifies allowed folders for VM placement |  | MaxItems: 50 <br /> |
| `resourcePools` _string array_ | ResourcePools specifies allowed resource pools for VM placement |  | MaxItems: 100 <br /> |
| `networks` _string array_ | Networks specifies allowed networks for VM placement |  | MaxItems: 50 <br /> |
| `zones` _string array_ | Zones specifies allowed availability zones |  | MaxItems: 20 <br /> |
| `regions` _string array_ | Regions specifies allowed regions |  | MaxItems: 20 <br /> |
| `nodeSelector` _object (keys:string, values:string)_ | NodeSelector specifies node selection criteria for libvirt provider |  | MaxProperties: 20 <br /> |
| `tolerations` _[VMToleration](#vmtoleration) array_ | Tolerations specifies tolerations for node placement |  | MaxItems: 20 <br /> |
| `excludedClusters` _string array_ | ExcludedClusters specifies clusters to exclude from placement |  | MaxItems: 50 <br /> |
| `excludedHosts` _string array_ | ExcludedHosts specifies hosts to exclude from placement |  | MaxItems: 200 <br /> |
| `excludedDatastores` _string array_ | ExcludedDatastores specifies datastores to exclude from placement |  | MaxItems: 100 <br /> |


#### PlacementStatistics



PlacementStatistics provides statistics about VM placements



_Appears in:_
- [VMPlacementPolicyStatus](#vmplacementpolicystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `totalPlacements` _integer_ | TotalPlacements is the total number of VM placements using this policy |  |  |
| `successfulPlacements` _integer_ | SuccessfulPlacements is the number of successful placements |  |  |
| `failedPlacements` _integer_ | FailedPlacements is the number of failed placements |  |  |
| `averagePlacementTime` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | AveragePlacementTime is the average time for VM placement |  |  |
| `constraintViolations` _integer_ | ConstraintViolations is the number of constraint violations |  |  |
| `lastPlacementTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastPlacementTime is when the last VM was placed using this policy |  |  |
| `placementDistribution` _object (keys:string, values:integer)_ | PlacementDistribution shows how VMs are distributed across hosts/clusters |  |  |


#### PolicyConflict



PolicyConflict represents a conflict between policies



_Appears in:_
- [VMPlacementPolicyStatus](#vmplacementpolicystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `policyName` _string_ | PolicyName is the name of the conflicting policy |  |  |
| `conflictType` _string_ | ConflictType describes the type of conflict |  | Enum: [hard soft resource security affinity] <br /> |
| `description` _string_ | Description provides details about the conflict |  |  |
| `severity` _string_ | Severity indicates the severity of the conflict |  | Enum: [low medium high critical] <br /> |
| `resolutionSuggestion` _string_ | ResolutionSuggestion provides suggestions for resolving the conflict |  |  |


#### PolicyValidationResult



PolicyValidationResult represents a validation result for a provider



_Appears in:_
- [VMPlacementPolicyStatus](#vmplacementpolicystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `valid` _boolean_ | Valid indicates if the policy is valid for the provider |  |  |
| `message` _string_ | Message provides details about the validation result |  |  |
| `warnings` _string array_ | Warnings lists any validation warnings |  | MaxItems: 20 <br /> |
| `errors` _string array_ | Errors lists any validation errors |  | MaxItems: 20 <br /> |
| `supportedFeatures` _string array_ | SupportedFeatures lists features supported by the provider |  | MaxItems: 50 <br /> |
| `unsupportedFeatures` _string array_ | UnsupportedFeatures lists features not supported by the provider |  | MaxItems: 50 <br /> |
| `lastValidated` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastValidated is when this validation was last performed |  |  |


#### PortRange



PortRange defines a port range



_Appears in:_
- [FirewallRule](#firewallrule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `start` _integer_ | Start is the starting port |  | Maximum: 65535 <br />Minimum: 1 <br /> |
| `end` _integer_ | End is the ending port (optional, defaults to start) |  | Maximum: 65535 <br />Minimum: 1 <br /> |


#### PortgroupSecurityConfig



PortgroupSecurityConfig defines portgroup security settings



_Appears in:_
- [VSphereNetworkConfig](#vspherenetworkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `allowPromiscuous` _boolean_ | AllowPromiscuous allows promiscuous mode | false |  |
| `allowMACChanges` _boolean_ | AllowMACChanges allows MAC address changes | true |  |
| `allowForgedTransmits` _boolean_ | AllowForgedTransmits allows forged transmits | true |  |


#### PowerState

_Underlying type:_ _string_

PowerState represents the desired power state of a VM

_Validation:_
- Enum: [On Off OffGraceful]

_Appears in:_
- [VirtualMachineSpec](#virtualmachinespec)
- [VirtualMachineStatus](#virtualmachinestatus)

| Field | Description |
| --- | --- |
| `On` | PowerStateOn indicates the VM should be powered on<br /> |
| `Off` | PowerStateOff indicates the VM should be powered off<br /> |
| `OffGraceful` | PowerStateOffGraceful indicates the VM should be gracefully shut down<br /> |


#### Provider



Provider is the Schema for the providers API



_Appears in:_
- [ProviderList](#providerlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `Provider` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ProviderSpec](#providerspec)_ |  |  |  |
| `status` _[ProviderStatus](#providerstatus)_ |  |  |  |


#### ProviderCapability

_Underlying type:_ _string_

ProviderCapability represents a provider capability

_Validation:_
- Enum: [VirtualMachines Snapshots Cloning LiveMigration ConsoleAccess DiskManagement NetworkManagement GPUPassthrough HighAvailability Backup Templates]

_Appears in:_
- [ProviderStatus](#providerstatus)

| Field | Description |
| --- | --- |
| `VirtualMachines` | ProviderCapabilityVirtualMachines indicates basic VM management<br /> |
| `Snapshots` | ProviderCapabilitySnapshots indicates snapshot support<br /> |
| `Cloning` | ProviderCapabilityCloning indicates VM cloning support<br /> |
| `LiveMigration` | ProviderCapabilityLiveMigration indicates live migration support<br /> |
| `ConsoleAccess` | ProviderCapabilityConsoleAccess indicates console access support<br /> |
| `DiskManagement` | ProviderCapabilityDiskManagement indicates disk management support<br /> |
| `NetworkManagement` | ProviderCapabilityNetworkManagement indicates network management support<br /> |
| `GPUPassthrough` | ProviderCapabilityGPUPassthrough indicates GPU passthrough support<br /> |
| `HighAvailability` | ProviderCapabilityHighAvailability indicates HA support<br /> |
| `Backup` | ProviderCapabilityBackup indicates backup support<br /> |
| `Templates` | ProviderCapabilityTemplates indicates template management support<br /> |


#### ProviderDefaults



ProviderDefaults provides default settings for VMs



_Appears in:_
- [ProviderSpec](#providerspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `datastore` _string_ | Datastore specifies the default datastore |  | MaxLength: 255 <br /> |
| `cluster` _string_ | Cluster specifies the default cluster |  | MaxLength: 255 <br /> |
| `folder` _string_ | Folder specifies the default folder |  | MaxLength: 255 <br /> |
| `resourcePool` _string_ | ResourcePool specifies the default resource pool |  | MaxLength: 255 <br /> |
| `network` _string_ | Network specifies the default network |  | MaxLength: 255 <br /> |


#### ProviderHealthCheck



ProviderHealthCheck defines health checking configuration



_Appears in:_
- [ProviderSpec](#providerspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates whether health checking is enabled | true |  |
| `interval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | Interval defines how often to check provider health | 30s |  |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | Timeout defines the timeout for health checks | 10s |  |
| `failureThreshold` _integer_ | FailureThreshold is the number of consecutive failures before marking unhealthy | 3 | Maximum: 10 <br />Minimum: 1 <br /> |
| `successThreshold` _integer_ | SuccessThreshold is the number of consecutive successes before marking healthy | 1 | Maximum: 10 <br />Minimum: 1 <br /> |


#### ProviderImageStatus



ProviderImageStatus contains provider-specific image status



_Appears in:_
- [VMImageStatus](#vmimagestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `available` _boolean_ | Available indicates if the image is available on this provider |  |  |
| `id` _string_ | ID is the provider-specific image identifier |  |  |
| `path` _string_ | Path is the provider-specific image path |  |  |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | Size is the image size on this provider |  |  |
| `lastUpdated` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastUpdated is when the status was last updated |  |  |
| `message` _string_ | Message provides provider-specific status information |  |  |


#### ProviderList



ProviderList contains a list of Provider





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `ProviderList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[Provider](#provider) array_ |  |  |  |


#### ProviderNetworkStatus



ProviderNetworkStatus contains provider-specific network status



_Appears in:_
- [VMNetworkAttachmentStatus](#vmnetworkattachmentstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `available` _boolean_ | Available indicates if the network is available on this provider |  |  |
| `id` _string_ | ID is the provider-specific network identifier |  |  |
| `state` _string_ | State is the provider-specific network state |  |  |
| `lastUpdated` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastUpdated is when the status was last updated |  |  |
| `message` _string_ | Message provides provider-specific status information |  |  |


#### ProviderResourceUsage



ProviderResourceUsage provides resource usage statistics



_Appears in:_
- [ProviderStatus](#providerstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cpu` _[ResourceUsageStats](#resourceusagestats)_ | CPU usage statistics |  |  |
| `memory` _[ResourceUsageStats](#resourceusagestats)_ | Memory usage statistics |  |  |
| `storage` _[ResourceUsageStats](#resourceusagestats)_ | Storage usage statistics |  |  |
| `network` _[NetworkUsageStats](#networkusagestats)_ | Network usage statistics |  |  |


#### ProviderRuntimeMode

_Underlying type:_ _string_

ProviderRuntimeMode specifies how the provider is executed

_Validation:_
- Enum: [Remote]

_Appears in:_
- [ProviderRuntimeSpec](#providerruntimespec)
- [ProviderRuntimeStatus](#providerruntimestatus)

| Field | Description |
| --- | --- |
| `Remote` | RuntimeModeRemote runs the provider as a separate deployment<br /> |


#### ProviderRuntimePhase

_Underlying type:_ _string_

ProviderRuntimePhase represents the phase of provider runtime

_Validation:_
- Enum: [Pending Starting Running Stopping Failed]

_Appears in:_
- [ProviderRuntimeStatus](#providerruntimestatus)

| Field | Description |
| --- | --- |
| `Pending` | ProviderRuntimePhasePending indicates the runtime is being prepared<br /> |
| `Starting` | ProviderRuntimePhaseStarting indicates the runtime is starting<br /> |
| `Running` | ProviderRuntimePhaseRunning indicates the runtime is operational<br /> |
| `Stopping` | ProviderRuntimePhaseStopping indicates the runtime is stopping<br /> |
| `Failed` | ProviderRuntimePhaseFailed indicates the runtime has failed<br /> |


#### ProviderRuntimeSpec



ProviderRuntimeSpec defines the runtime configuration for providers



_Appears in:_
- [ProviderSpec](#providerspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[ProviderRuntimeMode](#providerruntimemode)_ | Mode specifies the runtime mode (always Remote) | Remote | Enum: [Remote] <br /> |
| `image` _string_ | Image is the container image for remote providers (required) |  | Pattern: `^[a-zA-Z0-9._/-]+:[a-zA-Z0-9._-]+$` <br /> |
| `imagePullPolicy` _[PullPolicy](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#pullpolicy-v1-core)_ | ImagePullPolicy defines the image pull policy | IfNotPresent | Enum: [Always Never IfNotPresent] <br /> |
| `imagePullSecrets` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#localobjectreference-v1-core) array_ | ImagePullSecrets are references to secrets for pulling images |  | MaxItems: 10 <br /> |
| `replicas` _integer_ | Replicas is the number of provider instances (default 1) | 1 | Maximum: 10 <br />Minimum: 1 <br /> |
| `service` _[ProviderServiceSpec](#providerservicespec)_ | Service defines the service configuration |  |  |
| `resources` _[ResourceRequirements](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#resourcerequirements-v1-core)_ | Resources defines resource requirements for provider pods |  |  |
| `nodeSelector` _object (keys:string, values:string)_ | NodeSelector is a selector which must be true for the pod to fit on a node |  |  |
| `tolerations` _[Toleration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#toleration-v1-core) array_ | Tolerations allow pods to schedule onto nodes with matching taints |  | MaxItems: 20 <br /> |
| `affinity` _[Affinity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#affinity-v1-core)_ | Affinity defines scheduling constraints |  |  |
| `securityContext` _[SecurityContext](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#securitycontext-v1-core)_ | SecurityContext defines security context for provider pods |  |  |
| `env` _[EnvVar](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#envvar-v1-core) array_ | Env defines additional environment variables for provider pods |  | MaxItems: 50 <br /> |
| `livenessProbe` _[Probe](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#probe-v1-core)_ | LivenessProbe defines the liveness probe for provider pods |  |  |
| `readinessProbe` _[Probe](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#probe-v1-core)_ | ReadinessProbe defines the readiness probe for provider pods |  |  |


#### ProviderRuntimeStatus



ProviderRuntimeStatus defines the runtime status for providers



_Appears in:_
- [ProviderStatus](#providerstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[ProviderRuntimeMode](#providerruntimemode)_ | Mode indicates the current runtime mode |  | Enum: [Remote] <br /> |
| `endpoint` _string_ | Endpoint is the gRPC endpoint (host:port) for remote providers |  |  |
| `serviceRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#localobjectreference-v1-core)_ | ServiceRef references the Kubernetes service for remote providers |  |  |
| `phase` _[ProviderRuntimePhase](#providerruntimephase)_ | Phase indicates the runtime phase |  | Enum: [Pending Starting Running Stopping Failed] <br /> |
| `message` _string_ | Message provides additional details about the runtime status |  |  |
| `readyReplicas` _integer_ | ReadyReplicas is the number of ready provider replicas |  |  |
| `availableReplicas` _integer_ | AvailableReplicas is the number of available provider replicas |  |  |


#### ProviderServiceSpec



ProviderServiceSpec defines the service configuration for remote providers



_Appears in:_
- [ProviderRuntimeSpec](#providerruntimespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `port` _integer_ | Port is the gRPC service port | 9443 | Maximum: 65535 <br />Minimum: 1024 <br /> |
| `tls` _[ProviderTLSSpec](#providertlsspec)_ | TLS defines TLS configuration for the service |  |  |


#### ProviderSnapshotStatus



ProviderSnapshotStatus contains provider-specific snapshot status



_Appears in:_
- [VMSnapshotStatus](#vmsnapshotstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `available` _boolean_ | Available indicates if the snapshot is available on this provider |  |  |
| `id` _string_ | ID is the provider-specific snapshot identifier |  |  |
| `path` _string_ | Path is the provider-specific snapshot path |  |  |
| `state` _string_ | State is the provider-specific snapshot state |  |  |
| `lastUpdated` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastUpdated is when the status was last updated |  |  |
| `message` _string_ | Message provides provider-specific status information |  |  |


#### ProviderSpec



ProviderSpec defines the desired state of Provider



_Appears in:_
- [Provider](#provider)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[ProviderType](#providertype)_ | Type specifies the provider type |  | Enum: [vsphere libvirt firecracker qemu proxmox] <br /> |
| `endpoint` _string_ | Endpoint is the provider endpoint URI<br />Supports multiple protocols: HTTP(S), TCP, gRPC for general providers<br />and LibVirt-specific schemes: qemu://, qemu+ssh://, qemu+tcp://, qemu+tls:// |  | Pattern: `^((https?://[a-zA-Z0-9.-]+(:[0-9]+)?(/.*)?\|(tcp\|grpc)://[a-zA-Z0-9.-]+:[0-9]+(/.*)?)\|qemu(\+ssh\|\+tcp\|\+tls)?://([a-zA-Z0-9@.-]+(:[0-9]+)?)?(/.*))$` <br /> |
| `credentialSecretRef` _[ObjectRef](#objectref)_ | CredentialSecretRef references the Secret containing credentials |  |  |
| `insecureSkipVerify` _boolean_ | InsecureSkipVerify disables TLS verification (deprecated, use runtime.service.tls.insecureSkipVerify) | false |  |
| `defaults` _[ProviderDefaults](#providerdefaults)_ | Defaults provides default placement settings |  |  |
| `rateLimit` _[RateLimit](#ratelimit)_ | RateLimit configures API rate limiting |  |  |
| `runtime` _[ProviderRuntimeSpec](#providerruntimespec)_ | Runtime defines how the provider is executed (required) |  |  |
| `healthCheck` _[ProviderHealthCheck](#providerhealthcheck)_ | HealthCheck defines health checking configuration |  |  |
| `connectionPooling` _[ConnectionPooling](#connectionpooling)_ | ConnectionPooling defines connection pooling settings |  |  |


#### ProviderStatus



ProviderStatus defines the observed state of Provider



_Appears in:_
- [Provider](#provider)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `healthy` _boolean_ | Healthy indicates if the provider is healthy |  |  |
| `lastHealthCheck` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastHealthCheck records the last health check time |  |  |
| `runtime` _[ProviderRuntimeStatus](#providerruntimestatus)_ | Runtime provides runtime status information |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the latest available observations |  |  |
| `observedGeneration` _integer_ | ObservedGeneration reflects the generation observed by the controller |  |  |
| `capabilities` _[ProviderCapability](#providercapability) array_ | Capabilities lists the provider's supported capabilities |  | Enum: [VirtualMachines Snapshots Cloning LiveMigration ConsoleAccess DiskManagement NetworkManagement GPUPassthrough HighAvailability Backup Templates] <br /> |
| `version` _string_ | Version reports the provider version |  |  |
| `connectedVMs` _integer_ | ConnectedVMs is the number of VMs currently managed by this provider |  |  |
| `resourceUsage` _[ProviderResourceUsage](#providerresourceusage)_ | ResourceUsage provides resource usage statistics |  |  |


#### ProviderTLSSpec



ProviderTLSSpec defines TLS configuration for provider communication



_Appears in:_
- [ProviderServiceSpec](#providerservicespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled determines if TLS is enabled for provider communication | true |  |
| `secretRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#localobjectreference-v1-core)_ | SecretRef references a secret containing tls.crt, tls.key, and ca.crt |  |  |
| `insecureSkipVerify` _boolean_ | InsecureSkipVerify disables TLS certificate verification | false |  |


#### ProviderType

_Underlying type:_ _string_

ProviderType represents the type of virtualization provider

_Validation:_
- Enum: [vsphere libvirt firecracker qemu proxmox]

_Appears in:_
- [ProviderSpec](#providerspec)

| Field | Description |
| --- | --- |
| `vsphere` | ProviderTypeVSphere indicates a VMware vSphere provider<br /> |
| `libvirt` | ProviderTypeLibvirt indicates a libvirt provider<br /> |
| `firecracker` | ProviderTypeFirecracker indicates a Firecracker provider<br /> |
| `qemu` | ProviderTypeQEMU indicates a QEMU provider<br /> |
| `proxmox` | ProviderTypeProxmox indicates a Proxmox VE provider<br /> |


#### ProxmoxImageSource



ProxmoxImageSource defines Proxmox VE-specific image configuration



_Appears in:_
- [ImageSource](#imagesource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `templateID` _integer_ | TemplateID specifies an existing Proxmox template VMID |  | Maximum: 9.99999999e+08 <br />Minimum: 100 <br /> |
| `templateName` _string_ | TemplateName specifies an existing Proxmox template name |  | MaxLength: 255 <br /> |
| `storage` _string_ | Storage specifies the Proxmox storage for cloning<br />Examples: "local-lvm", "vms", "nfs-storage" |  | MaxLength: 255 <br /> |
| `node` _string_ | Node specifies the Proxmox node where the template exists |  | MaxLength: 255 <br /> |
| `format` _string_ | Format specifies the disk format | qcow2 | Enum: [raw qcow2 vmdk] <br /> |
| `fullClone` _boolean_ | FullClone determines if this should be a full clone (default) or linked clone | true |  |


#### ProxmoxNetworkConfig



ProxmoxNetworkConfig defines Proxmox VE-specific network configuration



_Appears in:_
- [NetworkConfig](#networkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `bridge` _string_ | Bridge specifies the Linux bridge<br />Examples: "vmbr0", "vmbr1", "vmbr2" |  | MaxLength: 15 <br />Pattern: `^vmbr[0-9]+$` <br /> |
| `model` _string_ | Model specifies the network card model | virtio | Enum: [virtio e1000 rtl8139 vmxnet3] <br /> |
| `vlanTag` _integer_ | VLANTag specifies the VLAN tag |  | Maximum: 4094 <br />Minimum: 1 <br /> |
| `firewall` _boolean_ | Firewall enables the Proxmox firewall for this interface | false |  |
| `rateLimit` _integer_ | RateLimit specifies the bandwidth limit in MB/s |  | Minimum: 1 <br /> |
| `mtu` _integer_ | MTU specifies the Maximum Transmission Unit |  | Maximum: 65520 <br />Minimum: 68 <br /> |


#### RateLimit



RateLimit configures API rate limiting



_Appears in:_
- [ProviderSpec](#providerspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `qps` _integer_ | QPS specifies queries per second | 10 | Maximum: 1000 <br />Minimum: 1 <br /> |
| `burst` _integer_ | Burst specifies the burst capacity | 20 | Maximum: 2000 <br />Minimum: 1 <br /> |


#### RegistryImageSource



RegistryImageSource defines container registry image configuration



_Appears in:_
- [ImageSource](#imagesource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _string_ | Image is the container image reference |  | Pattern: `^[a-zA-Z0-9._/-]+:[a-zA-Z0-9._-]+$` <br /> |
| `pullSecretRef` _[LocalObjectReference](#localobjectreference)_ | PullSecretRef references a secret for pulling the image |  |  |
| `format` _[ImageFormat](#imageformat)_ | Format specifies the expected image format | qcow2 | Enum: [qcow2 raw vmdk vhd vhdx iso ova ovf] <br /> |


#### ResourceConstraints



ResourceConstraints defines resource-based placement constraints



_Appears in:_
- [VMPlacementPolicySpec](#vmplacementpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `minCPUPerHost` _integer_ | MinCPUPerHost specifies minimum CPU available per host |  | Minimum: 1 <br /> |
| `minMemoryPerHost` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | MinMemoryPerHost specifies minimum memory available per host |  |  |
| `minDiskSpacePerHost` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | MinDiskSpacePerHost specifies minimum disk space available per host |  |  |
| `maxCPUUtilization` _integer_ | MaxCPUUtilization specifies maximum allowed CPU utilization |  | Maximum: 100 <br />Minimum: 0 <br /> |
| `maxMemoryUtilization` _integer_ | MaxMemoryUtilization specifies maximum allowed memory utilization |  | Maximum: 100 <br />Minimum: 0 <br /> |
| `maxDiskUtilization` _integer_ | MaxDiskUtilization specifies maximum allowed disk utilization |  | Maximum: 100 <br />Minimum: 0 <br /> |
| `requiredFeatures` _string array_ | RequiredFeatures lists required hardware features |  | MaxItems: 20 <br /> |
| `preferredFeatures` _string array_ | PreferredFeatures lists preferred hardware features |  | MaxItems: 20 <br /> |


#### ResourceUsageStats



ResourceUsageStats represents usage statistics for a resource



_Appears in:_
- [ProviderResourceUsage](#providerresourceusage)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `total` _integer_ | Total available capacity |  |  |
| `used` _integer_ | Used capacity |  |  |
| `available` _integer_ | Available capacity |  |  |
| `usagePercent` _integer_ | Usage percentage (0-100) |  |  |


#### RollingUpdateVMSetStrategy



RollingUpdateVMSetStrategy defines parameters for rolling updates



_Appears in:_
- [VMSetUpdateStrategy](#vmsetupdatestrategy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `maxUnavailable` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#intorstring-intstr-util)_ | MaxUnavailable is the maximum number of VMs that can be unavailable during update | 25% |  |
| `maxSurge` _[IntOrString](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#intorstring-intstr-util)_ | MaxSurge is the maximum number of VMs that can be created above desired replica count | 25% |  |
| `partition` _integer_ | Partition indicates the ordinal at which the VMSet should be partitioned for updates |  | Minimum: 0 <br /> |
| `podManagementPolicy` _[VMSetPodManagementPolicyType](#vmsetpodmanagementpolicytype)_ | PodManagementPolicy controls how VMs are created during initial scale up,<br />when replacing VMs on nodes, or when scaling down | OrderedReady | Enum: [OrderedReady Parallel] <br /> |


#### SecurityConstraints



SecurityConstraints defines security-based placement constraints



_Appears in:_
- [VMPlacementPolicySpec](#vmplacementpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `requireSecureBoot` _boolean_ | RequireSecureBoot requires hosts that support secure boot |  |  |
| `requireTPM` _boolean_ | RequireTPM requires hosts that support TPM |  |  |
| `requireEncryptedStorage` _boolean_ | RequireEncryptedStorage requires hosts that support encrypted storage |  |  |
| `requireNUMATopology` _boolean_ | RequireNUMATopology requires hosts that support NUMA topology |  |  |
| `allowedSecurityGroups` _string array_ | AllowedSecurityGroups lists allowed security groups |  | MaxItems: 20 <br /> |
| `deniedSecurityGroups` _string array_ | DeniedSecurityGroups lists denied security groups |  | MaxItems: 20 <br /> |
| `isolationLevel` _string_ | IsolationLevel specifies the required isolation level |  | Enum: [none basic strict maximum] <br /> |
| `trustLevel` _string_ | TrustLevel specifies the required trust level |  | Enum: [untrusted basic trusted highly-trusted] <br /> |


#### SecurityProfile



SecurityProfile defines security-related settings



_Appears in:_
- [VMClassSpec](#vmclassspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secureBoot` _boolean_ | SecureBoot enables secure boot functionality | false |  |
| `tpmEnabled` _boolean_ | TPMEnabled enables TPM (Trusted Platform Module) | false |  |
| `tpmVersion` _string_ | TPMVersion specifies the TPM version |  | Enum: [1.2 2] <br /> |
| `vtdEnabled` _boolean_ | VTDEnabled enables Intel VT-d or AMD-Vi | false |  |
| `encryptionPolicy` _[EncryptionPolicy](#encryptionpolicy)_ | EncryptionPolicy defines VM encryption settings |  |  |


#### SnapshotAction



SnapshotAction describes a snapshot operation



_Appears in:_
- [LifecycleHandler](#lifecyclehandler)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name hint for the snapshot |  |  |
| `includeMemory` _boolean_ | IncludeMemory indicates whether to include memory state |  |  |
| `description` _string_ | Description provides context for the snapshot |  |  |


#### SnapshotConfig



SnapshotConfig defines snapshot configuration options



_Appears in:_
- [VMSnapshotSpec](#vmsnapshotspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name provides a name hint for the snapshot (provider may modify) |  | MaxLength: 255 <br />Pattern: `^[a-zA-Z0-9]([a-zA-Z0-9\-_]*[a-zA-Z0-9])?$` <br /> |
| `description` _string_ | Description provides additional context for the snapshot |  | MaxLength: 1024 <br /> |
| `includeMemory` _boolean_ | IncludeMemory indicates whether to include memory state in the snapshot | false |  |
| `quiesce` _boolean_ | Quiesce indicates whether to quiesce the file system before snapshotting | true |  |
| `type` _[SnapshotType](#snapshottype)_ | Type specifies the snapshot type | Standard | Enum: [Standard Crash Application] <br /> |
| `compression` _boolean_ | Compression enables snapshot compression | false |  |
| `encryption` _[SnapshotEncryption](#snapshotencryption)_ | Encryption enables snapshot encryption |  |  |
| `consistencyLevel` _string_ | ConsistencyLevel defines the consistency level required | FilesystemConsistent | Enum: [CrashConsistent FilesystemConsistent ApplicationConsistent] <br /> |


#### SnapshotEncryption



SnapshotEncryption defines snapshot encryption settings



_Appears in:_
- [SnapshotConfig](#snapshotconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if encryption should be used | false |  |
| `keyProvider` _string_ | KeyProvider specifies the encryption key provider |  | Enum: [standard hardware external] <br /> |
| `keyRef` _[LocalObjectReference](#localobjectreference)_ | KeyRef references encryption keys |  |  |


#### SnapshotMetadata



SnapshotMetadata contains snapshot metadata



_Appears in:_
- [VMSnapshotSpec](#vmsnapshotspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tags` _object (keys:string, values:string)_ | Tags are key-value pairs for categorizing the snapshot |  | MaxProperties: 50 <br /> |
| `pinned` _boolean_ | Pinned indicates whether the snapshot is pinned (protected from automatic deletion) | false |  |
| `application` _string_ | Application specifies the application that created the snapshot |  | MaxLength: 255 <br /> |
| `purpose` _string_ | Purpose describes the purpose of the snapshot |  | Enum: [backup testing migration restore-point update other] <br /> |
| `environment` _string_ | Environment specifies the environment |  | Enum: [dev staging prod test] <br /> |


#### SnapshotPhase

_Underlying type:_ _string_

SnapshotPhase represents the phase of a snapshot

_Validation:_
- Enum: [Pending Creating Ready Deleting Failed Expired]

_Appears in:_
- [VMSnapshotStatus](#vmsnapshotstatus)

| Field | Description |
| --- | --- |
| `Pending` | SnapshotPhasePending indicates the snapshot is waiting to be processed<br /> |
| `Creating` | SnapshotPhaseCreating indicates the snapshot is being created<br /> |
| `Ready` | SnapshotPhaseReady indicates the snapshot is ready for use<br /> |
| `Deleting` | SnapshotPhaseDeleting indicates the snapshot is being deleted<br /> |
| `Failed` | SnapshotPhaseFailed indicates the snapshot operation failed<br /> |
| `Expired` | SnapshotPhaseExpired indicates the snapshot has expired<br /> |


#### SnapshotProgress



SnapshotProgress shows the snapshot creation progress



_Appears in:_
- [VMSnapshotStatus](#vmsnapshotstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `totalBytes` _integer_ | TotalBytes is the total number of bytes to snapshot |  |  |
| `completedBytes` _integer_ | CompletedBytes is the number of bytes completed |  |  |
| `percentage` _integer_ | Percentage is the completion percentage (0-100) |  | Maximum: 100 <br />Minimum: 0 <br /> |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | StartTime is when the snapshot creation started |  |  |
| `eta` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | ETA is the estimated time to completion |  |  |


#### SnapshotRef



SnapshotRef references a snapshot



_Appears in:_
- [VMSnapshotStatus](#vmsnapshotstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the snapshot name |  |  |
| `namespace` _string_ | Namespace is the snapshot namespace |  |  |
| `uid` _string_ | UID is the snapshot UID |  |  |


#### SnapshotRetentionPolicy



SnapshotRetentionPolicy defines snapshot retention rules



_Appears in:_
- [VMSnapshotSpec](#vmsnapshotspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `maxAge` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | MaxAge is the maximum age before snapshot should be deleted |  |  |
| `maxCount` _integer_ | MaxCount is the maximum number of snapshots to retain |  | Maximum: 100 <br />Minimum: 1 <br /> |
| `deleteOnVMDelete` _boolean_ | DeleteOnVMDelete indicates whether to delete snapshot when VM is deleted | true |  |
| `preservePinned` _boolean_ | PreservePinned indicates whether to preserve pinned snapshots | true |  |
| `gracePeriod` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | GracePeriod is the grace period before deletion | 24h |  |


#### SnapshotSchedule



SnapshotSchedule defines automated snapshot scheduling



_Appears in:_
- [VMSnapshotSpec](#vmsnapshotspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if scheduled snapshots are enabled | false |  |
| `cronSpec` _string_ | CronSpec defines the schedule in cron format |  | Pattern: `^(@(annually\|yearly\|monthly\|weekly\|daily\|hourly\|reboot))\|(@every (\d+(ns\|us\|µs\|ms\|s\|m\|h))+)\|((((\d+,)+\d+\|(\d+(\/\|-)\d+)\|\d+\|\*) ?)\{5,7\})$` <br /> |
| `timezone` _string_ | Timezone specifies the timezone for the schedule | UTC |  |
| `suspend` _boolean_ | Suspend indicates whether to suspend scheduled snapshots | false |  |
| `concurrencyPolicy` _string_ | ConcurrencyPolicy specifies how to handle concurrent snapshot jobs | Forbid | Enum: [Allow Forbid Replace] <br /> |
| `successfulJobsHistoryLimit` _integer_ | SuccessfulJobsHistoryLimit limits retained successful jobs | 3 | Maximum: 100 <br />Minimum: 0 <br /> |
| `failedJobsHistoryLimit` _integer_ | FailedJobsHistoryLimit limits retained failed jobs | 1 | Maximum: 100 <br />Minimum: 0 <br /> |


#### SnapshotType

_Underlying type:_ _string_

SnapshotType represents the type of snapshot

_Validation:_
- Enum: [Standard Crash Application]

_Appears in:_
- [SnapshotConfig](#snapshotconfig)

| Field | Description |
| --- | --- |
| `Standard` | SnapshotTypeStandard indicates a standard snapshot<br /> |
| `Crash` | SnapshotTypeCrash indicates a crash-consistent snapshot<br /> |
| `Application` | SnapshotTypeApplication indicates an application-consistent snapshot<br /> |


#### StaticIPConfig



StaticIPConfig defines static IP configuration



_Appears in:_
- [IPAllocationConfig](#ipallocationconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `address` _string_ | Address is the static IP address (CIDR notation) |  | Pattern: `^((25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)\.)\{3\}(25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)/([0-9]\|[1-2][0-9]\|3[0-2])$` <br /> |
| `gateway` _string_ | Gateway is the default gateway |  | Pattern: `^((25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)\.)\{3\}(25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)$` <br /> |
| `routes` _[StaticRoute](#staticroute) array_ | Routes contains static routes |  | MaxItems: 20 <br /> |


#### StaticRoute



StaticRoute defines a static route



_Appears in:_
- [StaticIPConfig](#staticipconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `destination` _string_ | Destination is the destination network (CIDR notation) |  | Pattern: `^((25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)\.)\{3\}(25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)/([0-9]\|[1-2][0-9]\|3[0-2])$` <br /> |
| `gateway` _string_ | Gateway is the route gateway |  | Pattern: `^((25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)\.)\{3\}(25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)$` <br /> |
| `metric` _integer_ | Metric is the route metric |  | Maximum: 65535 <br />Minimum: 0 <br /> |


#### StoragePrepareOptions



StoragePrepareOptions defines storage-specific preparation options



_Appears in:_
- [ImagePrepare](#imageprepare)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vsphere` _[VSphereStorageOptions](#vspherestorageoptions)_ | VSphere storage options |  |  |
| `libvirt` _[LibvirtStorageOptions](#libvirtstorageoptions)_ | Libvirt storage options |  |  |
| `preferredFormat` _[ImageFormat](#imageformat)_ | PreferredFormat specifies the preferred target format |  | Enum: [qcow2 raw vmdk vhd vhdx iso ova ovf] <br /> |
| `compression` _boolean_ | Compression enables compression during import | false |  |


#### SysprepCustomization



SysprepCustomization defines Windows sysprep customization



_Appears in:_
- [VMCustomization](#vmcustomization)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if sysprep should be run | false |  |
| `productKey` _string_ | ProductKey specifies the Windows product key |  |  |
| `organization` _string_ | Organization specifies the organization name |  | MaxLength: 255 <br /> |
| `owner` _string_ | Owner specifies the owner name |  | MaxLength: 255 <br /> |
| `adminPassword` _[PasswordSpec](#passwordspec)_ | AdminPassword specifies the administrator password |  |  |
| `joinDomain` _[DomainJoinSpec](#domainjoinspec)_ | JoinDomain specifies domain join configuration |  |  |
| `customCommands` _string array_ | CustomCommands specifies custom commands to run during sysprep |  | MaxItems: 20 <br /> |


#### TrafficShapingConfig



TrafficShapingConfig defines traffic shaping settings



_Appears in:_
- [VSphereNetworkConfig](#vspherenetworkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if traffic shaping is enabled | false |  |
| `averageBandwidth` _integer_ | AverageBandwidth is the average bandwidth in bits per second |  | Minimum: 1000 <br /> |
| `peakBandwidth` _integer_ | PeakBandwidth is the peak bandwidth in bits per second |  | Minimum: 1000 <br /> |
| `burstSize` _integer_ | BurstSize is the burst size in bytes |  | Minimum: 1024 <br /> |






#### UserData



UserData defines cloud-init configuration



_Appears in:_
- [VMCustomization](#vmcustomization)
- [VirtualMachineSpec](#virtualmachinespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cloudInit` _[CloudInit](#cloudinit)_ | CloudInit contains cloud-init configuration |  |  |
| `ignition` _[Ignition](#ignition)_ | Ignition contains Ignition configuration for CoreOS/RHEL |  |  |


#### VLANConfig



VLANConfig defines VLAN configuration



_Appears in:_
- [VSphereNetworkConfig](#vspherenetworkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _string_ | Type specifies the VLAN type | none | Enum: [none vlan pvlan trunk] <br /> |
| `vlanID` _integer_ | VlanID specifies the VLAN ID for VLAN type |  | Maximum: 4094 <br />Minimum: 1 <br /> |
| `trunkVlanIDs` _integer array_ | TrunkVlanIDs specifies VLAN IDs for trunk type |  | MaxItems: 100 <br /> |
| `primaryVlanID` _integer_ | PrimaryVlanID specifies the primary VLAN ID for PVLAN |  | Maximum: 4094 <br />Minimum: 1 <br /> |
| `secondaryVlanID` _integer_ | SecondaryVlanID specifies the secondary VLAN ID for PVLAN |  | Maximum: 4094 <br />Minimum: 1 <br /> |


#### VMAffinity



VMAffinity defines affinity rules between VMs



_Appears in:_
- [AffinityRules](#affinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `requiredDuringScheduling` _[VMAffinityTerm](#vmaffinityterm) array_ | RequiredDuringScheduling specifies hard affinity rules |  | MaxItems: 20 <br /> |
| `preferredDuringScheduling` _[WeightedVMAffinityTerm](#weightedvmaffinityterm) array_ | PreferredDuringScheduling specifies soft affinity rules |  | MaxItems: 20 <br /> |


#### VMAffinityTerm



VMAffinityTerm defines a VM affinity term



_Appears in:_
- [VMAffinity](#vmaffinity)
- [VMAntiAffinity](#vmantiaffinity)
- [WeightedVMAffinityTerm](#weightedvmaffinityterm)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `labelSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#labelselector-v1-meta)_ | LabelSelector selects VMs for affinity rules |  |  |
| `namespaces` _string array_ | Namespaces specifies which namespaces to consider |  | MaxItems: 20 <br /> |
| `namespaceSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#labelselector-v1-meta)_ | NamespaceSelector selects namespaces using label selectors |  |  |
| `topologyKey` _string_ | TopologyKey specifies the topology domain for the rule |  | MaxLength: 253 <br /> |
| `matchExpressions` _[VMSelectorRequirement](#vmselectorrequirement) array_ | MatchExpressions is a list of VM selector requirements |  | MaxItems: 20 <br /> |


#### VMAntiAffinity



VMAntiAffinity defines anti-affinity rules between VMs



_Appears in:_
- [AntiAffinityRules](#antiaffinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `requiredDuringScheduling` _[VMAffinityTerm](#vmaffinityterm) array_ | RequiredDuringScheduling specifies hard anti-affinity rules |  | MaxItems: 20 <br /> |
| `preferredDuringScheduling` _[WeightedVMAffinityTerm](#weightedvmaffinityterm) array_ | PreferredDuringScheduling specifies soft anti-affinity rules |  | MaxItems: 20 <br /> |


#### VMClass



VMClass is the Schema for the vmclasses API



_Appears in:_
- [VMClassList](#vmclasslist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMClass` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VMClassSpec](#vmclassspec)_ |  |  |  |
| `status` _[VMClassStatus](#vmclassstatus)_ |  |  |  |


#### VMClassList



VMClassList contains a list of VMClass





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMClassList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VMClass](#vmclass) array_ |  |  |  |


#### VMClassSpec



VMClassSpec defines the desired state of VMClass



_Appears in:_
- [VMClass](#vmclass)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cpu` _integer_ | CPU specifies the number of virtual CPUs |  | Maximum: 128 <br />Minimum: 1 <br /> |
| `memory` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | Memory specifies memory allocation using Kubernetes resource quantities |  |  |
| `firmware` _[FirmwareType](#firmwaretype)_ | Firmware specifies the firmware type | BIOS | Enum: [BIOS UEFI EFI] <br /> |
| `diskDefaults` _[DiskDefaults](#diskdefaults)_ | DiskDefaults provides default disk settings |  |  |
| `guestToolsPolicy` _[GuestToolsPolicy](#guesttoolspolicy)_ | GuestToolsPolicy specifies guest tools installation policy | install | Enum: [install skip upgrade uninstall] <br /> |
| `extraConfig` _object (keys:string, values:string)_ | ExtraConfig contains provider-specific extra configuration |  | MaxProperties: 50 <br /> |
| `resourceLimits` _[VMResourceLimits](#vmresourcelimits)_ | ResourceLimits defines resource limits and reservations |  |  |
| `performanceProfile` _[PerformanceProfile](#performanceprofile)_ | PerformanceProfile defines performance-related settings |  |  |
| `securityProfile` _[SecurityProfile](#securityprofile)_ | SecurityProfile defines security-related settings |  |  |


#### VMClassStatus



VMClassStatus defines the observed state of VMClass



_Appears in:_
- [VMClass](#vmclass)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the latest available observations |  |  |
| `observedGeneration` _integer_ | ObservedGeneration reflects the generation observed by the controller |  |  |
| `usedByVMs` _integer_ | UsedByVMs is the number of VMs currently using this class |  |  |
| `supportedProviders` _string array_ | SupportedProviders lists the providers that support this class |  |  |
| `validationResults` _object (keys:string, values:[ValidationResult](#validationresult))_ | ValidationResults contains validation results for different providers |  |  |


#### VMClone



VMClone is the Schema for the vmclones API



_Appears in:_
- [VMCloneList](#vmclonelist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMClone` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VMCloneSpec](#vmclonespec)_ |  |  |  |
| `status` _[VMCloneStatus](#vmclonestatus)_ |  |  |  |


#### VMCloneList



VMCloneList contains a list of VMClone





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMCloneList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VMClone](#vmclone) array_ |  |  |  |


#### VMCloneSpec



VMCloneSpec defines the desired state of VMClone



_Appears in:_
- [VMClone](#vmclone)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `source` _[CloneSource](#clonesource)_ | Source defines the source for cloning |  |  |
| `target` _[VMCloneTarget](#vmclonetarget)_ | Target defines the target VM configuration |  |  |
| `options` _[CloneOptions](#cloneoptions)_ | Options defines cloning options |  |  |
| `customization` _[VMCustomization](#vmcustomization)_ | Customization defines VM customization options |  |  |
| `metadata` _[CloneMetadata](#clonemetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |


#### VMCloneStatus



VMCloneStatus defines the observed state of VMClone



_Appears in:_
- [VMClone](#vmclone)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetRef` _[LocalObjectReference](#localobjectreference)_ | TargetRef references the created target VM |  |  |
| `phase` _[ClonePhase](#clonephase)_ | Phase represents the current phase of the clone operation |  | Enum: [Pending Preparing Cloning Customizing Powering-On Ready Failed] <br /> |
| `message` _string_ | Message provides additional details about the current state |  |  |
| `taskRef` _string_ | TaskRef tracks any ongoing async operations |  |  |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | StartTime is when the clone operation started |  |  |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | CompletionTime is when the clone operation completed |  |  |
| `actualCloneType` _[CloneType](#clonetype)_ | ActualCloneType indicates the actual clone type that was used |  | Enum: [FullClone LinkedClone InstantClone] <br /> |
| `progress` _[CloneProgress](#cloneprogress)_ | Progress shows the clone operation progress |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the current service state |  |  |
| `observedGeneration` _integer_ | ObservedGeneration reflects the generation observed by the controller |  |  |
| `retryCount` _integer_ | RetryCount is the number of times the clone has been retried |  |  |
| `lastRetryTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastRetryTime is when the clone was last retried |  |  |
| `customizationStatus` _[CustomizationStatus](#customizationstatus)_ | CustomizationStatus contains customization operation status |  |  |


#### VMCloneTarget



VMCloneTarget defines the target VM configuration



_Appears in:_
- [VMCloneSpec](#vmclonespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the target VM |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `namespace` _string_ | Namespace is the namespace for the target VM (defaults to source namespace) |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `providerRef` _[ObjectRef](#objectref)_ | ProviderRef references the target provider (defaults to source provider) |  |  |
| `classRef` _[LocalObjectReference](#localobjectreference)_ | ClassRef references the VM class for resource allocation |  |  |
| `placementRef` _[LocalObjectReference](#localobjectreference)_ | PlacementRef references placement policy for the target VM |  |  |
| `networks` _[VMNetworkRef](#vmnetworkref) array_ | Networks defines network configuration overrides |  | MaxItems: 10 <br /> |
| `disks` _[DiskSpec](#diskspec) array_ | Disks defines disk configuration overrides |  | MaxItems: 20 <br /> |
| `labels` _object (keys:string, values:string)_ | Labels defines labels to apply to the target VM |  | MaxProperties: 50 <br /> |
| `annotations` _object (keys:string, values:string)_ | Annotations defines annotations to apply to the target VM |  | MaxProperties: 50 <br /> |


#### VMCustomization



VMCustomization defines VM customization options



_Appears in:_
- [VMCloneSpec](#vmclonespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hostname` _string_ | Hostname sets the target VM hostname |  | MaxLength: 255 <br />Pattern: `^[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?$` <br /> |
| `domain` _string_ | Domain sets the domain name |  | MaxLength: 255 <br /> |
| `timeZone` _string_ | TimeZone sets the timezone |  | MaxLength: 100 <br /> |
| `networks` _[NetworkCustomization](#networkcustomization) array_ | Networks defines network customization |  | MaxItems: 10 <br /> |
| `userData` _[UserData](#userdata)_ | UserData provides cloud-init or similar customization data |  |  |
| `sysprep` _[SysprepCustomization](#sysprepcustomization)_ | Sysprep provides Windows sysprep customization |  |  |
| `tags` _string array_ | Tags defines additional tags for the cloned VM |  | MaxItems: 50 <br /> |
| `guestCommands` _[GuestCommand](#guestcommand) array_ | GuestCommands defines commands to run in the guest OS |  | MaxItems: 20 <br /> |
| `certificates` _[CertificateSpec](#certificatespec) array_ | Certificates defines certificates to install |  | MaxItems: 10 <br /> |


#### VMImage



VMImage is the Schema for the vmimages API



_Appears in:_
- [VMImageList](#vmimagelist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMImage` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VMImageSpec](#vmimagespec)_ |  |  |  |
| `status` _[VMImageStatus](#vmimagestatus)_ |  |  |  |


#### VMImageList



VMImageList contains a list of VMImage





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMImageList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VMImage](#vmimage) array_ |  |  |  |


#### VMImageSpec



VMImageSpec defines the desired state of VMImage



_Appears in:_
- [VMImage](#vmimage)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `source` _[ImageSource](#imagesource)_ | Source defines the image source configuration |  |  |
| `prepare` _[ImagePrepare](#imageprepare)_ | Prepare contains optional image preparation steps |  |  |
| `metadata` _[ImageMetadata](#imagemetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `distribution` _[OSDistribution](#osdistribution)_ | Distribution contains OS distribution information |  |  |


#### VMImageStatus



VMImageStatus defines the observed state of VMImage



_Appears in:_
- [VMImage](#vmimage)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ready` _boolean_ | Ready indicates if the image is ready for use |  |  |
| `phase` _[ImagePhase](#imagephase)_ | Phase represents the current phase of image preparation |  | Enum: [Pending Downloading Importing Converting Optimizing Ready Failed] <br /> |
| `message` _string_ | Message provides additional details about the current state |  |  |
| `availableOn` _string array_ | AvailableOn lists the providers where the image is available |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the latest available observations |  |  |
| `observedGeneration` _integer_ | ObservedGeneration reflects the generation observed by the controller |  |  |
| `lastPrepareTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastPrepareTime records when the image was last prepared |  |  |
| `prepareTaskRef` _string_ | PrepareTaskRef tracks any ongoing image preparation operations |  |  |
| `importProgress` _[ImageImportProgress](#imageimportprogress)_ | ImportProgress shows the progress of image import operations |  |  |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | Size is the size of the prepared image |  |  |
| `checksum` _string_ | Checksum is the actual checksum of the prepared image |  |  |
| `format` _[ImageFormat](#imageformat)_ | Format is the actual format of the prepared image |  | Enum: [qcow2 raw vmdk vhd vhdx iso ova ovf] <br /> |
| `providerStatus` _object (keys:string, values:[ProviderImageStatus](#providerimagestatus))_ | ProviderStatus contains provider-specific status information |  |  |


#### VMMigration



VMMigration is the Schema for the vmmigrations API



_Appears in:_
- [VMMigrationList](#vmmigrationlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMMigration` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VMMigrationSpec](#vmmigrationspec)_ |  |  |  |
| `status` _[VMMigrationStatus](#vmmigrationstatus)_ |  |  |  |


#### VMMigrationList



VMMigrationList contains a list of VMMigration





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMMigrationList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VMMigration](#vmmigration) array_ |  |  |  |


#### VMMigrationSpec



VMMigrationSpec defines the desired state of VMMigration



_Appears in:_
- [VMMigration](#vmmigration)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `source` _[MigrationSource](#migrationsource)_ | Source defines the source VM to migrate from |  |  |
| `target` _[MigrationTarget](#migrationtarget)_ | Target defines the target provider and configuration |  |  |
| `options` _[MigrationOptions](#migrationoptions)_ | Options defines migration options |  |  |
| `storage` _[MigrationStorage](#migrationstorage)_ | Storage defines storage backend configuration for transfer |  |  |
| `metadata` _[MigrationMetadata](#migrationmetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |


#### VMMigrationStatus



VMMigrationStatus defines the observed state of VMMigration



_Appears in:_
- [VMMigration](#vmmigration)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[MigrationPhase](#migrationphase)_ | Phase represents the current phase of the migration |  | Enum: [Pending Validating Snapshotting Exporting Transferring Converting Importing Creating Validating-Target Ready Failed] <br /> |
| `message` _string_ | Message provides additional details about the current state |  |  |
| `targetVMRef` _[LocalObjectReference](#localobjectreference)_ | TargetVMRef references the created target VM |  |  |
| `snapshotRef` _string_ | SnapshotRef references the source snapshot used for migration |  |  |
| `snapshotID` _string_ | SnapshotID is the provider-specific snapshot identifier |  |  |
| `exportID` _string_ | ExportID is the export operation identifier |  |  |
| `importID` _string_ | ImportID is the import operation identifier |  |  |
| `taskRef` _string_ | TaskRef is the current task reference for async operations |  |  |
| `targetVMID` _string_ | TargetVMID is the provider-specific target VM identifier |  |  |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | StartTime is when the migration started |  |  |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | CompletionTime is when the migration completed |  |  |
| `progress` _[MigrationProgress](#migrationprogress)_ | Progress shows the migration operation progress |  |  |
| `diskInfo` _[MigrationDiskInfo](#migrationdiskinfo)_ | DiskInfo contains information about the migrated disk |  |  |
| `storageInfo` _[MigrationStorageInfo](#migrationstorageinfo)_ | StorageInfo contains information about intermediate storage |  |  |
| `storagePVCName` _string_ | StoragePVCName is the name of the PVC used for migration storage |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the current service state |  |  |
| `observedGeneration` _integer_ | ObservedGeneration reflects the generation observed by the controller |  |  |
| `retryCount` _integer_ | RetryCount is the number of times the migration has been retried |  |  |
| `lastRetryTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastRetryTime is when the migration was last retried |  |  |
| `validationResults` _[ValidationResults](#validationresults)_ | ValidationResults contains results of validation checks |  |  |


#### VMNetworkAttachment



VMNetworkAttachment is the Schema for the vmnetworkattachments API



_Appears in:_
- [VMNetworkAttachmentList](#vmnetworkattachmentlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMNetworkAttachment` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VMNetworkAttachmentSpec](#vmnetworkattachmentspec)_ |  |  |  |
| `status` _[VMNetworkAttachmentStatus](#vmnetworkattachmentstatus)_ |  |  |  |


#### VMNetworkAttachmentList



VMNetworkAttachmentList contains a list of VMNetworkAttachment





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMNetworkAttachmentList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VMNetworkAttachment](#vmnetworkattachment) array_ |  |  |  |


#### VMNetworkAttachmentSpec



VMNetworkAttachmentSpec defines the desired state of VMNetworkAttachment



_Appears in:_
- [VMNetworkAttachment](#vmnetworkattachment)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `network` _[NetworkConfig](#networkconfig)_ | Network defines the underlying network configuration |  |  |
| `ipAllocation` _[IPAllocationConfig](#ipallocationconfig)_ | IPAllocation defines IP address allocation settings |  |  |
| `security` _[NetworkSecurityConfig](#networksecurityconfig)_ | Security defines network security settings |  |  |
| `qos` _[NetworkQoSConfig](#networkqosconfig)_ | QoS defines Quality of Service settings |  |  |
| `metadata` _[NetworkMetadata](#networkmetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |


#### VMNetworkAttachmentStatus



VMNetworkAttachmentStatus defines the observed state of VMNetworkAttachment



_Appears in:_
- [VMNetworkAttachment](#vmnetworkattachment)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ready` _boolean_ | Ready indicates if the network is ready for use |  |  |
| `phase` _[NetworkAttachmentPhase](#networkattachmentphase)_ | Phase represents the current phase |  | Enum: [Pending Configuring Ready Failed] <br /> |
| `message` _string_ | Message provides additional details about the current state |  |  |
| `availableOn` _string array_ | AvailableOn lists the providers where the network is available |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the latest available observations |  |  |
| `observedGeneration` _integer_ | ObservedGeneration reflects the generation observed by the controller |  |  |
| `connectedVMs` _integer_ | ConnectedVMs is the number of VMs using this network |  |  |
| `ipAllocations` _[IPAllocation](#ipallocation) array_ | IPAllocations contains current IP allocations |  |  |
| `providerStatus` _object (keys:string, values:[ProviderNetworkStatus](#providernetworkstatus))_ | ProviderStatus contains provider-specific status information |  |  |


#### VMNetworkRef



VMNetworkRef represents a reference to a network attachment



_Appears in:_
- [MigrationTarget](#migrationtarget)
- [VMCloneTarget](#vmclonetarget)
- [VirtualMachineSpec](#virtualmachinespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of this network attachment |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `networkRef` _[ObjectRef](#objectref)_ | NetworkRef references the VMNetworkAttachment |  |  |
| `ipAddress` _string_ | IPAddress specifies a static IP address (optional) |  | Pattern: `^((25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)\.)\{3\}(25[0-5]\|2[0-4][0-9]\|[01]?[0-9][0-9]?)$` <br /> |
| `macAddress` _string_ | MACAddress specifies a static MAC address (optional) |  | Pattern: `^([0-9A-Fa-f]\{2\}[:-])\{5\}([0-9A-Fa-f]\{2\})$` <br /> |


#### VMPlacementPolicy



VMPlacementPolicy is the Schema for the vmplacementpolicies API



_Appears in:_
- [VMPlacementPolicyList](#vmplacementpolicylist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMPlacementPolicy` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VMPlacementPolicySpec](#vmplacementpolicyspec)_ |  |  |  |
| `status` _[VMPlacementPolicyStatus](#vmplacementpolicystatus)_ |  |  |  |


#### VMPlacementPolicyList



VMPlacementPolicyList contains a list of VMPlacementPolicy





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMPlacementPolicyList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VMPlacementPolicy](#vmplacementpolicy) array_ |  |  |  |


#### VMPlacementPolicySpec



VMPlacementPolicySpec defines the desired state of VMPlacementPolicy



_Appears in:_
- [VMPlacementPolicy](#vmplacementpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hard` _[PlacementConstraints](#placementconstraints)_ | Hard constraints that must be satisfied for VM placement |  |  |
| `soft` _[PlacementConstraints](#placementconstraints)_ | Soft constraints that should be satisfied if possible |  |  |
| `antiAffinity` _[AntiAffinityRules](#antiaffinityrules)_ | AntiAffinity defines anti-affinity rules for VMs |  |  |
| `affinity` _[AffinityRules](#affinityrules)_ | Affinity defines affinity rules for VMs |  |  |
| `resourceConstraints` _[ResourceConstraints](#resourceconstraints)_ | ResourceConstraints defines resource-based placement constraints |  |  |
| `securityConstraints` _[SecurityConstraints](#securityconstraints)_ | SecurityConstraints defines security-based placement constraints |  |  |
| `priority` _integer_ | Priority defines the priority of this placement policy |  | Maximum: 1000 <br />Minimum: 0 <br /> |
| `weight` _integer_ | Weight defines the weight of this placement policy |  | Maximum: 100 <br />Minimum: 1 <br /> |


#### VMPlacementPolicyStatus



VMPlacementPolicyStatus defines the observed state of VMPlacementPolicy



_Appears in:_
- [VMPlacementPolicy](#vmplacementpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the most recent generation observed by the controller |  |  |
| `usedByVMs` _[LocalObjectReference](#localobjectreference) array_ | UsedByVMs lists VMs currently using this policy |  | MaxItems: 1000 <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the current service state |  |  |
| `validationResults` _object (keys:string, values:[PolicyValidationResult](#policyvalidationresult))_ | ValidationResults contains validation results for different providers |  |  |
| `placementStats` _[PlacementStatistics](#placementstatistics)_ | PlacementStats provides statistics about VM placements using this policy |  |  |
| `conflictingPolicies` _[PolicyConflict](#policyconflict) array_ | ConflictingPolicies lists policies that conflict with this policy |  | MaxItems: 50 <br /> |


#### VMResourceLimits



VMResourceLimits defines resource limits and reservations



_Appears in:_
- [VMClassSpec](#vmclassspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cpuLimit` _integer_ | CPULimit is the maximum CPU usage limit (in MHz or percentage) |  | Maximum: 100000 <br />Minimum: 100 <br /> |
| `cpuReservation` _integer_ | CPUReservation is the guaranteed CPU allocation (in MHz) |  | Maximum: 100000 <br />Minimum: 0 <br /> |
| `memoryLimit` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | MemoryLimit is the maximum memory usage limit |  |  |
| `memoryReservation` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | MemoryReservation is the guaranteed memory allocation |  |  |
| `cpuShares` _integer_ | CPUShares defines the relative CPU priority (higher = more priority) |  | Maximum: 1e+06 <br />Minimum: 1 <br /> |


#### VMSelectorOperator

_Underlying type:_ _string_

VMSelectorOperator represents a selector operator

_Validation:_
- Enum: [In NotIn Exists DoesNotExist]

_Appears in:_
- [VMSelectorRequirement](#vmselectorrequirement)

| Field | Description |
| --- | --- |
| `In` | VMSelectorOpIn means the key must be in the set of values<br /> |
| `NotIn` | VMSelectorOpNotIn means the key must not be in the set of values<br /> |
| `Exists` | VMSelectorOpExists means the key must exist<br /> |
| `DoesNotExist` | VMSelectorOpDoesNotExist means the key must not exist<br /> |


#### VMSelectorRequirement



VMSelectorRequirement defines a VM selector requirement



_Appears in:_
- [VMAffinityTerm](#vmaffinityterm)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `key` _string_ | Key is the label key that the selector applies to |  | MaxLength: 253 <br /> |
| `operator` _[VMSelectorOperator](#vmselectoroperator)_ | Operator represents a key's relationship to a set of values |  | Enum: [In NotIn Exists DoesNotExist] <br /> |
| `values` _string array_ | Values is an array of string values |  | MaxItems: 50 <br /> |


#### VMSet



VMSet is the Schema for the vmsets API



_Appears in:_
- [VMSetList](#vmsetlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMSet` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VMSetSpec](#vmsetspec)_ |  |  |  |
| `status` _[VMSetStatus](#vmsetstatus)_ |  |  |  |


#### VMSetFailedVM



VMSetFailedVM represents a VM that failed to update



_Appears in:_
- [VMSetUpdateStatus](#vmsetupdatestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the failed VM |  |  |
| `reason` _string_ | Reason provides the reason for failure |  |  |
| `message` _string_ | Message provides additional details about the failure |  |  |
| `lastAttempt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastAttempt is when the last update attempt was made |  |  |
| `retryCount` _integer_ | RetryCount is the number of retry attempts |  |  |


#### VMSetList



VMSetList contains a list of VMSet





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMSetList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VMSet](#vmset) array_ |  |  |  |


#### VMSetOrdinals



VMSetOrdinals configures the sequential ordering of VM indices



_Appears in:_
- [VMSetSpec](#vmsetspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `start` _integer_ | Start is the number representing the first replica's index | 0 | Maximum: 999999 <br />Minimum: 0 <br /> |


#### VMSetPersistentVolumeClaimRetentionPolicy



VMSetPersistentVolumeClaimRetentionPolicy defines the retention policy for PVCs



_Appears in:_
- [VMSetSpec](#vmsetspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `whenDeleted` _[PersistentVolumeClaimRetentionPolicyType](#persistentvolumeclaimretentionpolicytype)_ | WhenDeleted specifies what happens to PVCs created from VMSet VolumeClaimTemplates when the VMSet is deleted | Retain | Enum: [Retain Delete] <br /> |
| `whenScaled` _[PersistentVolumeClaimRetentionPolicyType](#persistentvolumeclaimretentionpolicytype)_ | WhenScaled specifies what happens to PVCs created from VMSet VolumeClaimTemplates when the VMSet is scaled down | Retain | Enum: [Retain Delete] <br /> |


#### VMSetPodManagementPolicyType

_Underlying type:_ _string_

VMSetPodManagementPolicyType defines the policy for creating VMs

_Validation:_
- Enum: [OrderedReady Parallel]

_Appears in:_
- [RollingUpdateVMSetStrategy](#rollingupdatevmsetstrategy)

| Field | Description |
| --- | --- |
| `OrderedReady` | OrderedReadyVMSetPodManagementPolicy creates VMs in order and waits for each to be ready<br /> |
| `Parallel` | ParallelVMSetPodManagementPolicy creates VMs in parallel<br /> |


#### VMSetSpec



VMSetSpec defines the desired state of VMSet



_Appears in:_
- [VMSet](#vmset)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | Replicas is the desired number of VMs in the set | 1 | Maximum: 1000 <br />Minimum: 0 <br /> |
| `selector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#labelselector-v1-meta)_ | Selector is a label query over VMs that should match the replica count |  |  |
| `template` _[VMSetTemplate](#vmsettemplate)_ | Template is the object that describes the VM that will be created |  |  |
| `updateStrategy` _[VMSetUpdateStrategy](#vmsetupdatestrategy)_ | UpdateStrategy defines how to replace existing VMs with new ones |  |  |
| `minReadySeconds` _integer_ | MinReadySeconds is the minimum number of seconds for which a newly created VM<br />should be ready without any of its containers crashing |  | Maximum: 3600 <br />Minimum: 0 <br /> |
| `revisionHistoryLimit` _integer_ | RevisionHistoryLimit is the number of old VMSets to retain | 10 | Maximum: 100 <br />Minimum: 0 <br /> |
| `persistentVolumeClaimRetentionPolicy` _[VMSetPersistentVolumeClaimRetentionPolicy](#vmsetpersistentvolumeclaimretentionpolicy)_ | PersistentVolumeClaimRetentionPolicy defines the retention policy for PVCs |  |  |
| `ordinals` _[VMSetOrdinals](#vmsetordinals)_ | Ordinals configures the sequential ordering of VM indices |  |  |
| `serviceName` _string_ | ServiceName is the name of the service that governs this VMSet |  | MaxLength: 253 <br /> |
| `volumeClaimTemplates` _[PersistentVolumeClaimTemplate](#persistentvolumeclaimtemplate) array_ | VolumeClaimTemplates defines a list of claims that VMs are allowed to reference |  | MaxItems: 20 <br /> |


#### VMSetStatus



VMSetStatus defines the observed state of VMSet



_Appears in:_
- [VMSet](#vmset)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the most recent generation observed by the controller |  |  |
| `replicas` _integer_ | Replicas is the number of VMs created by the VMSet controller |  |  |
| `readyReplicas` _integer_ | ReadyReplicas is the number of VMs that are ready |  |  |
| `availableReplicas` _integer_ | AvailableReplicas is the number of VMs that are available |  |  |
| `updatedReplicas` _integer_ | UpdatedReplicas is the number of VMs that have been updated |  |  |
| `currentRevision` _string_ | CurrentRevision is the revision of the current VMSet |  |  |
| `updateRevision` _string_ | UpdateRevision is the revision of the updated VMSet |  |  |
| `collisionCount` _integer_ | CollisionCount is the count of hash collisions for the VMSet |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the current service state |  |  |
| `currentReplicas` _integer_ | CurrentReplicas is the number of VMs currently running |  |  |
| `updateStatus` _[VMSetUpdateStatus](#vmsetupdatestatus)_ | UpdateStatus provides detailed update operation status |  |  |
| `vmStatus` _[VMSetVMStatus](#vmsetvmstatus) array_ | VMStatus provides per-VM status information |  | MaxItems: 1000 <br /> |


#### VMSetTemplate



VMSetTemplate defines the template for VMs in a VMSet



_Appears in:_
- [VMSetSpec](#vmsetspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VirtualMachineSpec](#virtualmachinespec)_ | Spec is the VM specification |  |  |


#### VMSetUpdatePhase

_Underlying type:_ _string_

VMSetUpdatePhase represents the phase of a VMSet update

_Validation:_
- Enum: [Pending InProgress Paused Completed Failed]

_Appears in:_
- [VMSetUpdateStatus](#vmsetupdatestatus)

| Field | Description |
| --- | --- |
| `Pending` | VMSetUpdatePhasePending indicates the update is pending<br /> |
| `InProgress` | VMSetUpdatePhaseInProgress indicates the update is in progress<br /> |
| `Paused` | VMSetUpdatePhasePaused indicates the update is paused<br /> |
| `Completed` | VMSetUpdatePhaseCompleted indicates the update is completed<br /> |
| `Failed` | VMSetUpdatePhaseFailed indicates the update failed<br /> |


#### VMSetUpdateStatus



VMSetUpdateStatus provides detailed update operation status



_Appears in:_
- [VMSetStatus](#vmsetstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[VMSetUpdatePhase](#vmsetupdatephase)_ | Phase represents the current phase of the update |  | Enum: [Pending InProgress Paused Completed Failed] <br /> |
| `message` _string_ | Message provides additional details about the update |  |  |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | StartTime is when the update started |  |  |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | CompletionTime is when the update completed |  |  |
| `updatedVMs` _string array_ | UpdatedVMs lists VMs that have been updated |  | MaxItems: 1000 <br /> |
| `pendingVMs` _string array_ | PendingVMs lists VMs that are pending update |  | MaxItems: 1000 <br /> |
| `failedVMs` _[VMSetFailedVM](#vmsetfailedvm) array_ | FailedVMs lists VMs that failed to update |  | MaxItems: 1000 <br /> |


#### VMSetUpdateStrategy



VMSetUpdateStrategy defines the update strategy for a VMSet



_Appears in:_
- [VMSetSpec](#vmsetspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[VMSetUpdateStrategyType](#vmsetupdatestrategytype)_ | Type can be "RollingUpdate" or "OnDelete" | RollingUpdate | Enum: [RollingUpdate OnDelete Recreate] <br /> |
| `rollingUpdate` _[RollingUpdateVMSetStrategy](#rollingupdatevmsetstrategy)_ | RollingUpdate is used when Type is RollingUpdate |  |  |


#### VMSetUpdateStrategyType

_Underlying type:_ _string_

VMSetUpdateStrategyType defines the type of update strategy

_Validation:_
- Enum: [RollingUpdate OnDelete Recreate]

_Appears in:_
- [VMSetUpdateStrategy](#vmsetupdatestrategy)

| Field | Description |
| --- | --- |
| `RollingUpdate` | RollingUpdateVMSetStrategyType replaces VMs one by one<br /> |
| `OnDelete` | OnDeleteVMSetStrategyType replaces VMs only when manually deleted<br /> |
| `Recreate` | RecreateVMSetStrategyType deletes all VMs before creating new ones<br /> |


#### VMSetVMStatus



VMSetVMStatus provides per-VM status information



_Appears in:_
- [VMSetStatus](#vmsetstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the VM name |  |  |
| `phase` _[VirtualMachinePhase](#virtualmachinephase)_ | Phase is the VM phase |  | Enum: [Pending Provisioning Running Stopped Reconfiguring Deleting Failed] <br /> |
| `ready` _boolean_ | Ready indicates if the VM is ready |  |  |
| `revision` _string_ | Revision is the VM revision |  |  |
| `creationTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | CreationTime is when the VM was created |  |  |
| `lastUpdateTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastUpdateTime is when the VM was last updated |  |  |
| `message` _string_ | Message provides additional VM status information |  |  |


#### VMSnapshot



VMSnapshot is the Schema for the vmsnapshots API



_Appears in:_
- [VMSnapshotList](#vmsnapshotlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMSnapshot` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VMSnapshotSpec](#vmsnapshotspec)_ |  |  |  |
| `status` _[VMSnapshotStatus](#vmsnapshotstatus)_ |  |  |  |


#### VMSnapshotInfo



VMSnapshotInfo provides information about a VM snapshot



_Appears in:_
- [VirtualMachineStatus](#virtualmachinestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `id` _string_ | ID is the provider-specific snapshot identifier |  |  |
| `name` _string_ | Name is the snapshot name |  |  |
| `creationTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | CreationTime is when the snapshot was created |  |  |
| `description` _string_ | Description provides additional context |  |  |
| `sizeBytes` _integer_ | SizeBytes is the size of the snapshot |  |  |
| `hasMemory` _boolean_ | HasMemory indicates if memory state is included |  |  |


#### VMSnapshotList



VMSnapshotList contains a list of VMSnapshot





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VMSnapshotList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VMSnapshot](#vmsnapshot) array_ |  |  |  |


#### VMSnapshotOperation



VMSnapshotOperation defines snapshot operations in VM spec



_Appears in:_
- [VirtualMachineSpec](#virtualmachinespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `revertToRef` _[LocalObjectReference](#localobjectreference)_ | RevertToRef specifies a snapshot to revert to |  |  |


#### VMSnapshotSpec



VMSnapshotSpec defines the desired state of VMSnapshot



_Appears in:_
- [VMSnapshot](#vmsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vmRef` _[LocalObjectReference](#localobjectreference)_ | VMRef references the virtual machine to snapshot |  |  |
| `snapshotConfig` _[SnapshotConfig](#snapshotconfig)_ | SnapshotConfig defines snapshot configuration options |  |  |
| `retentionPolicy` _[SnapshotRetentionPolicy](#snapshotretentionpolicy)_ | RetentionPolicy defines how long to keep this snapshot |  |  |
| `schedule` _[SnapshotSchedule](#snapshotschedule)_ | Schedule defines automated snapshot scheduling |  |  |
| `metadata` _[SnapshotMetadata](#snapshotmetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |


#### VMSnapshotStatus



VMSnapshotStatus defines the observed state of VMSnapshot



_Appears in:_
- [VMSnapshot](#vmsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `snapshotID` _string_ | SnapshotID is the provider-specific identifier for the snapshot |  |  |
| `phase` _[SnapshotPhase](#snapshotphase)_ | Phase represents the current phase of the snapshot |  | Enum: [Pending Creating Ready Deleting Failed Expired] <br /> |
| `message` _string_ | Message provides additional details about the current state |  |  |
| `creationTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | CreationTime is when the snapshot was created |  |  |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | CompletionTime is when the snapshot creation completed |  |  |
| `size` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | Size is the size of the snapshot |  |  |
| `virtualSize` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#quantity-resource-api)_ | VirtualSize is the virtual size of the snapshot |  |  |
| `taskRef` _string_ | TaskRef tracks any ongoing async operations |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the current service state |  |  |
| `observedGeneration` _integer_ | ObservedGeneration reflects the generation observed by the controller |  |  |
| `progress` _[SnapshotProgress](#snapshotprogress)_ | Progress shows the snapshot creation progress |  |  |
| `providerStatus` _object (keys:string, values:[ProviderSnapshotStatus](#providersnapshotstatus))_ | ProviderStatus contains provider-specific status information |  |  |
| `children` _[SnapshotRef](#snapshotref) array_ | Children lists child snapshots (for snapshot trees) |  |  |
| `parent` _[SnapshotRef](#snapshotref)_ | Parent references the parent snapshot (for snapshot trees) |  |  |
| `expiryTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | ExpiryTime is when the snapshot will expire (based on retention policy) |  |  |


#### VMTaintEffect

_Underlying type:_ _string_

VMTaintEffect represents the effect of a taint

_Validation:_
- Enum: [NoSchedule PreferNoSchedule NoExecute]

_Appears in:_
- [VMToleration](#vmtoleration)

| Field | Description |
| --- | --- |
| `NoSchedule` | VMTaintEffectNoSchedule means no new VMs will be scheduled<br /> |
| `PreferNoSchedule` | VMTaintEffectPreferNoSchedule means avoid scheduling if possible<br /> |
| `NoExecute` | VMTaintEffectNoExecute means existing VMs will be evicted<br /> |


#### VMToleration



VMToleration represents a toleration for VM placement



_Appears in:_
- [PlacementConstraints](#placementconstraints)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `key` _string_ | Key is the taint key that the toleration applies to |  | MaxLength: 253 <br /> |
| `operator` _[VMTolerationOperator](#vmtolerationoperator)_ | Operator represents the relationship between the key and value | Equal | Enum: [Exists Equal] <br /> |
| `value` _string_ | Value is the taint value the toleration matches to |  | MaxLength: 253 <br /> |
| `effect` _[VMTaintEffect](#vmtainteffect)_ | Effect indicates the taint effect to match |  | Enum: [NoSchedule PreferNoSchedule NoExecute] <br /> |
| `tolerationSeconds` _integer_ | TolerationSeconds represents the period of time the toleration tolerates the taint |  | Minimum: 0 <br /> |


#### VMTolerationOperator

_Underlying type:_ _string_

VMTolerationOperator represents the operator for toleration

_Validation:_
- Enum: [Exists Equal]

_Appears in:_
- [VMToleration](#vmtoleration)

| Field | Description |
| --- | --- |
| `Exists` | VMTolerationOpExists means the toleration exists<br /> |
| `Equal` | VMTolerationOpEqual means the toleration equals the value<br /> |


#### VSphereImageSource



VSphereImageSource defines vSphere-specific image configuration



_Appears in:_
- [ImageSource](#imagesource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `templateName` _string_ | TemplateName references an existing vSphere template |  | MaxLength: 255 <br /> |
| `contentLibrary` _[ContentLibraryRef](#contentlibraryref)_ | ContentLibrary references a vSphere content library item |  |  |
| `ovaURL` _string_ | OVAURL provides a URL to an OVA file to import |  | Pattern: `^https?://.*\.(ova\|ovf)$` <br /> |
| `checksum` _string_ | Checksum provides expected checksum for verification |  |  |
| `checksumType` _[ChecksumType](#checksumtype)_ | ChecksumType specifies the checksum algorithm | sha256 | Enum: [md5 sha1 sha256 sha512] <br /> |
| `providerRef` _[LocalObjectReference](#localobjectreference)_ | ProviderRef references the vSphere provider for importing |  |  |


#### VSphereNetworkConfig



VSphereNetworkConfig defines vSphere-specific network configuration



_Appears in:_
- [NetworkConfig](#networkconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `portgroup` _string_ | Portgroup specifies the vSphere portgroup name |  | MaxLength: 255 <br /> |
| `distributedSwitch` _[DistributedSwitchConfig](#distributedswitchconfig)_ | DistributedSwitch specifies the distributed virtual switch |  |  |
| `vlan` _[VLANConfig](#vlanconfig)_ | VLAN specifies the VLAN configuration |  |  |
| `security` _[PortgroupSecurityConfig](#portgroupsecurityconfig)_ | Security defines portgroup security settings |  |  |
| `trafficShaping` _[TrafficShapingConfig](#trafficshapingconfig)_ | TrafficShaping defines traffic shaping settings |  |  |


#### VSphereStorageOptions



VSphereStorageOptions defines vSphere storage preparation options



_Appears in:_
- [StoragePrepareOptions](#storageprepareoptions)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `datastore` _string_ | Datastore specifies the target datastore for import |  | MaxLength: 255 <br /> |
| `folder` _string_ | Folder specifies the target folder for import |  | MaxLength: 255 <br /> |
| `thinProvisioned` _boolean_ | ThinProvisioned indicates whether to use thin provisioning |  |  |
| `diskType` _string_ | DiskType specifies the disk provisioning type |  | Enum: [thin thick eagerzeroedthick] <br /> |


#### ValidationChecks



ValidationChecks defines validation checks to perform



_Appears in:_
- [MigrationOptions](#migrationoptions)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `checkDiskSize` _boolean_ | CheckDiskSize verifies disk size matches | true |  |
| `checkChecksum` _boolean_ | CheckChecksum verifies checksums match | true |  |
| `checkBoot` _boolean_ | CheckBoot verifies VM boots successfully | false |  |
| `checkConnectivity` _boolean_ | CheckConnectivity tests network connectivity | false |  |


#### ValidationResult



ValidationResult represents a validation result for a provider



_Appears in:_
- [VMClassStatus](#vmclassstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `valid` _boolean_ | Valid indicates if the class is valid for the provider |  |  |
| `message` _string_ | Message provides details about the validation result |  |  |
| `warnings` _string array_ | Warnings lists any validation warnings |  |  |
| `lastValidated` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastValidated is when this validation was last performed |  |  |


#### ValidationResults



ValidationResults contains results of validation checks



_Appears in:_
- [VMMigrationStatus](#vmmigrationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `diskSizeMatch` _boolean_ | DiskSizeMatch indicates if disk sizes match |  |  |
| `checksumMatch` _boolean_ | ChecksumMatch indicates if checksums match |  |  |
| `bootSuccess` _boolean_ | BootSuccess indicates if the target VM booted successfully |  |  |
| `connectivitySuccess` _boolean_ | ConnectivitySuccess indicates if network connectivity works |  |  |
| `validationErrors` _string array_ | ValidationErrors lists any validation errors |  |  |


#### VirtualMachine



VirtualMachine is the Schema for the virtualmachines API



_Appears in:_
- [VirtualMachineList](#virtualmachinelist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VirtualMachine` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VirtualMachineSpec](#virtualmachinespec)_ |  |  |  |
| `status` _[VirtualMachineStatus](#virtualmachinestatus)_ |  |  |  |


#### VirtualMachineLifecycle



VirtualMachineLifecycle defines lifecycle configuration for a VM



_Appears in:_
- [VirtualMachineSpec](#virtualmachinespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `preStop` _[LifecycleHandler](#lifecyclehandler)_ | PreStop defines actions to take before stopping the VM |  |  |
| `postStart` _[LifecycleHandler](#lifecyclehandler)_ | PostStart defines actions to take after starting the VM |  |  |
| `gracefulShutdownTimeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#duration-v1-meta)_ | GracefulShutdownTimeout defines how long to wait for graceful shutdown | 60s |  |


#### VirtualMachineList



VirtualMachineList contains a list of VirtualMachine





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `infra.virtrigaud.io/v1beta1` | | |
| `kind` _string_ | `VirtualMachineList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VirtualMachine](#virtualmachine) array_ |  |  |  |


#### VirtualMachinePhase

_Underlying type:_ _string_

VirtualMachinePhase represents the phase of a VM

_Validation:_
- Enum: [Pending Provisioning Running Stopped Reconfiguring Deleting Failed]

_Appears in:_
- [VMSetVMStatus](#vmsetvmstatus)
- [VirtualMachineStatus](#virtualmachinestatus)

| Field | Description |
| --- | --- |
| `Pending` | VirtualMachinePhasePending indicates the VM is waiting to be processed<br /> |
| `Provisioning` | VirtualMachinePhaseProvisioning indicates the VM is being created<br /> |
| `Running` | VirtualMachinePhaseRunning indicates the VM is running<br /> |
| `Stopped` | VirtualMachinePhaseStopped indicates the VM is stopped<br /> |
| `Reconfiguring` | VirtualMachinePhaseReconfiguring indicates the VM is being reconfigured<br /> |
| `Deleting` | VirtualMachinePhaseDeleting indicates the VM is being deleted<br /> |
| `Failed` | VirtualMachinePhaseFailed indicates the VM is in a failed state<br /> |


#### VirtualMachineResources



VirtualMachineResources defines resource overrides for a VM



_Appears in:_
- [VirtualMachineSpec](#virtualmachinespec)
- [VirtualMachineStatus](#virtualmachinestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cpu` _integer_ | CPU specifies the number of virtual CPUs |  | Maximum: 128 <br />Minimum: 1 <br /> |
| `memoryMiB` _integer_ | MemoryMiB specifies the amount of memory in MiB |  | Maximum: 1.048576e+06 <br />Minimum: 128 <br /> |
| `gpu` _[GPUConfig](#gpuconfig)_ | GPU specifies GPU configuration |  |  |


#### VirtualMachineSpec



VirtualMachineSpec defines the desired state of VirtualMachine.



_Appears in:_
- [VMSetTemplate](#vmsettemplate)
- [VirtualMachine](#virtualmachine)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `providerRef` _[ObjectRef](#objectref)_ | ProviderRef references the Provider that manages this VM |  |  |
| `classRef` _[ObjectRef](#objectref)_ | ClassRef references the VMClass that defines resource allocation |  |  |
| `imageRef` _[ObjectRef](#objectref)_ | ImageRef references the VMImage to use as base template.<br />Either ImageRef or ImportedDisk must be specified, but not both. |  |  |
| `importedDisk` _[ImportedDiskRef](#importeddiskref)_ | ImportedDisk references a pre-imported disk (e.g., from migration).<br />Either ImageRef or ImportedDisk must be specified, but not both. |  |  |
| `networks` _[VMNetworkRef](#vmnetworkref) array_ | Networks specifies network attachments for the VM |  | MaxItems: 10 <br /> |
| `disks` _[DiskSpec](#diskspec) array_ | Disks specifies additional disks beyond the root disk |  | MaxItems: 20 <br /> |
| `userData` _[UserData](#userdata)_ | UserData contains cloud-init configuration |  |  |
| `placement` _[Placement](#placement)_ | Placement provides hints for VM placement |  |  |
| `powerState` _[PowerState](#powerstate)_ | PowerState specifies the desired power state |  | Enum: [On Off OffGraceful] <br /> |
| `tags` _string array_ | Tags are applied to the VM for organization |  | MaxItems: 50 <br /> |
| `resources` _[VirtualMachineResources](#virtualmachineresources)_ | Resources allows overriding resource allocation from the VMClass |  |  |
| `placementRef` _[LocalObjectReference](#localobjectreference)_ | PlacementRef references a VMPlacementPolicy for advanced placement rules |  |  |
| `snapshot` _[VMSnapshotOperation](#vmsnapshotoperation)_ | Snapshot defines snapshot-related operations |  |  |
| `lifecycle` _[VirtualMachineLifecycle](#virtualmachinelifecycle)_ | Lifecycle defines VM lifecycle configuration |  |  |


#### VirtualMachineStatus



VirtualMachineStatus defines the observed state of VirtualMachine.



_Appears in:_
- [VirtualMachine](#virtualmachine)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `id` _string_ | ID is the provider-specific identifier for this VM |  |  |
| `powerState` _[PowerState](#powerstate)_ | PowerState reflects the current power state |  | Enum: [On Off OffGraceful] <br /> |
| `ips` _string array_ | IPs contains the IP addresses assigned to the VM |  |  |
| `consoleURL` _string_ | ConsoleURL provides access to the VM console |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | Conditions represent the latest available observations |  |  |
| `observedGeneration` _integer_ | ObservedGeneration reflects the generation observed by the controller |  |  |
| `lastTaskRef` _string_ | LastTaskRef references the last async operation |  |  |
| `provider` _object (keys:string, values:string)_ | Provider contains provider-specific details |  |  |
| `reconfigureTaskRef` _string_ | ReconfigureTaskRef tracks reconfiguration operations |  |  |
| `lastReconfigureTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | LastReconfigureTime records when the last reconfiguration occurred |  |  |
| `currentResources` _[VirtualMachineResources](#virtualmachineresources)_ | CurrentResources shows the current resource allocation |  |  |
| `snapshots` _[VMSnapshotInfo](#vmsnapshotinfo) array_ | Snapshots lists available snapshots for this VM |  |  |
| `phase` _[VirtualMachinePhase](#virtualmachinephase)_ | Phase represents the current phase of the VM |  | Enum: [Pending Provisioning Running Stopped Reconfiguring Deleting Failed] <br /> |
| `message` _string_ | Message provides additional details about the current state |  |  |


#### WeightedVMAffinityTerm



WeightedVMAffinityTerm defines a weighted VM affinity term



_Appears in:_
- [VMAffinity](#vmaffinity)
- [VMAntiAffinity](#vmantiaffinity)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `weight` _integer_ | Weight associated with matching the corresponding VMAffinityTerm |  | Maximum: 100 <br />Minimum: 1 <br /> |
| `vmAffinityTerm` _[VMAffinityTerm](#vmaffinityterm)_ | VMAffinityTerm defines the VM affinity term |  |  |


#### ZoneAffinityRule



ZoneAffinityRule defines zone affinity rules



_Appears in:_
- [AffinityRules](#affinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if zone affinity is enabled |  |  |
| `preferredZones` _string array_ | PreferredZones lists preferred zones |  | MaxItems: 10 <br /> |
| `scope` _string_ | Scope defines the scope of the affinity rule |  | Enum: [strict preferred] <br /> |


#### ZoneAntiAffinityRule



ZoneAntiAffinityRule defines zone anti-affinity rules



_Appears in:_
- [AntiAffinityRules](#antiaffinityrules)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled indicates if zone anti-affinity is enabled |  |  |
| `maxVMsPerZone` _integer_ | MaxVMsPerZone limits the number of VMs per zone |  | Maximum: 10000 <br />Minimum: 1 <br /> |
| `scope` _string_ | Scope defines the scope of the anti-affinity rule |  | Enum: [strict preferred] <br /> |


