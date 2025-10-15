# Changelog

All notable changes to VirtRigaud will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.3] - 2025-10-13 

### Added

#### vSphere Provider
- **VM Reconfiguration Support**: Complete implementation of dynamic VM reconfiguration
  - Online CPU adjustment for running VMs (hot-add when supported by guest OS)
  - Online memory adjustment for running VMs (hot-add when supported)
  - Disk resizing with safety checks to prevent data loss (shrinking prevented)
  - Intelligent change detection to avoid unnecessary reconfigurations
  - Memory parsing support for multiple units (Mi, Gi, MiB, GiB)
  - Automatic fallback to offline changes when online modification is not supported
- **Asynchronous Task Tracking**: Full TaskStatus RPC implementation
  - Real-time monitoring of vSphere async operations via govmomi
  - Task state reporting (queued, running, success, error)
  - Error information extraction from failed tasks
  - Progress tracking with percentage completion
  - Integration with vSphere task manager for reliable operation status
- **VM Cloning Operations**: Complete clone functionality
  - Full clone support for independent VM copies
  - Linked clone support for space-efficient template-based deployments
  - Automatic snapshot creation for linked clones when no snapshot exists
  - Proper disk relocation and storage configuration
  - Clone naming and folder placement control
- **Console URL Generation**: Web-based VM console access
  - Automatic vSphere web client console URL generation
  - Direct browser-based VM console access via vCenter
  - URL includes VM instance UUID for reliable identification
  - Integration with vCenter endpoint for proper routing

#### Libvirt Provider
- **VM Reconfiguration Support**: Complete virsh-based reconfiguration
  - Online CPU adjustment via `virsh setvcpus --live` for running VMs
  - Online memory adjustment via `virsh setmem --live` for running VMs
  - Offline configuration updates for stopped VMs via `virsh setvcpus/setmem --config`
  - Disk volume resizing via storage provider integration
  - Automatic VM info parsing to extract current CPU and memory settings
  - Graceful handling of operations requiring VM restart
  - Memory unit conversion and validation (bytes, KiB, MiB, GiB)
- **VNC Console URL Generation**: Remote console access support
  - Automatic VNC port extraction from domain XML configuration
  - VNC console URL generation for direct viewer connections
  - Support for standard VNC clients and web-based VNC viewers
  - Integration with libvirt graphics configuration

#### Proxmox Provider
- **Guest Agent IP Detection**: Enhanced network information retrieval
  - QEMU guest agent integration for accurate IP address detection
  - Extraction of all network interfaces from running VMs
  - Automatic filtering of loopback and link-local addresses
  - Support for both IPv4 and IPv6 address reporting
  - Real-time IP information when guest agent is installed and running
- **Template Cloning Support**: Full VM cloning from Proxmox templates
  - Automatic template detection from VMImage CRD `templateID` or `templateName`
  - Full clone and linked clone support via `fullClone` parameter
  - Proper storage pool selection during clone operation
  - Clone task monitoring with async operation support
- **Intelligent Boot Order Configuration**: 
  - Auto-detection of primary boot disk from cloned template
  - Support for multiple disk types: scsi, virtio, sata, ide
  - Automatic boot order generation (e.g., `boot=order=scsi0;ide2`)
  - Exclusion of CD-ROM devices from boot order
- **Cloud-Init Integration**:
  - Complete cloud-init configuration via `ide2:cloudinit` device
  - SSH key injection with proper URL encoding
  - User creation and authentication setup
  - Network configuration (static IP and DHCP)
  - Package installation and custom commands via runcmd
- **Complete CRD Support**:
  - ProxmoxImageSource integration for template-based deployments
  - ProxmoxNetworkConfig for bridge and VLAN configuration
  - Controller parsing of Proxmox-specific fields
  - Provider RPC implementation for all VM operations
- **Production Features**:
  - VM creation, power management, deletion
  - Status reporting with IP address detection
  - Task-based async operations with progress tracking
  - Comprehensive error handling and logging

### Fixed

#### vSphere Provider

**Reconfigure Type Mismatch**:
- **Root Issue**: Memory comparison in Reconfigure function caused compilation error due to type mismatch between int64 and int32
- **Cause**: govmomi's `VirtualMachineConfigInfo.Hardware.MemoryMB` field is int32, but comparison was using int64
- **Fix**: Added explicit type casting `int64(vmMo.Config.Hardware.MemoryMB)` to ensure type compatibility
- **Impact**: Reconfigure operations now compile and execute correctly without type errors

#### Libvirt Provider

**Missing Standard Library Imports**:
- **Root Issue**: Reconfigure and Describe functions referenced undefined packages causing compilation failures
- **Cause**: Implementation added `strconv` usage for integer conversion and `net/url` for URL parsing without importing packages
- **Fix**: Added missing imports:
  - `import "strconv"` for string-to-integer conversions in CPU/memory parsing
  - `import "net/url"` for VNC console URL construction
- **Impact**: Provider now compiles successfully and all reconfiguration and console URL features work correctly

#### Proxmox Provider

**SSH Keys Encoding**:
- **Root Issue**: Proxmox API's `sshkeys` parameter requires **double URL encoding** due to its internal decoding behavior
**Template Cloning and Boot Order**:
- **Root Issue**: Provider was creating new VMs instead of cloning from templates, causing boot failures
- **Fix**: Implemented proper template detection and cloning workflow:
  1. Parse `TemplateName` from VMImage CRD (via controller)
  2. Call CloneVM API with template ID and storage configuration
  3. Wait for clone task completion
  4. Detect primary boot disk from cloned VM config
  5. Reconfigure VM with cloud-init and correct boot order
- **Boot Order Detection**: Added intelligent disk detection that:
  - Scans VM config for disk attachments (virtio, scsi, sata, ide)
  - Filters out CD-ROM devices
  - Prioritizes disk types (virtio > scsi > sata > ide)
  - Constructs proper boot parameter (e.g., `boot=order=scsi0;ide2`)


**Controller Image Source Parsing**:
- **Root Issue**: Controller was sending empty `TemplateName` to provider, causing fallback to VM creation
- **Cause**: JSON field name mismatch - controller uses `TemplateName` (capital T) but provider expected `template_name` (snake_case)
- **Fix**: Updated provider to parse `TemplateName` field correctly from contracts.VMImage JSON
- **Debug Process**: Added comprehensive debug logging in both controller and provider to trace data flow
- **Impact**: Template ID from VMImage CRD is now correctly transmitted to Proxmox provider

#### Release Workflow

**Helm Chart Image Tag Updates**:
- **Root Issue**: Manager and provider images were not being updated during Helm releases, staying pinned to v0.2.0
- **Cause**: sed patterns in release workflow used incorrect range expressions that stopped before reaching the `tag:` lines
  - Manager pattern: `/^manager:/,/^  image:/` stopped at line 16, but `tag:` is on line 18
  - Provider patterns: Nested ranges didn't reach `tag:` lines which appear after provider names
- **Fix**: Replaced fragile regex ranges with precise line-number-based patterns:
  - Manager: `MANAGER_START` to `MANAGER_START+10`
  - LibVirt: `LIBVIRT_START` to `LIBVIRT_START+15`
  - vSphere: `VSPHERE_START` to `VSPHERE_START+15`
  - Proxmox: `PROXMOX_START` to `PROXMOX_START+15`
- **Impact**: All component images (manager, providers, kubectl) now update correctly to match release version


## [0.2.2] - 2025-10-13

### Added (Continued)

#### Nested Virtualization Support
- **VMClass PerformanceProfile**: Added `nestedVirtualization` field to enable nested virtualization capabilities in VMs, allowing VMs to run their own hypervisors and nested virtual machines
- **vSphere Provider Implementation**: 
  - Automatically configures `vhv.enable=TRUE` for hardware-assisted virtualization
  - Enables `vhv.allowNestedPageTables=TRUE` for improved nested VM performance
  - Compatible with VM hardware version 9+ (version 14+ recommended)
- **LibVirt Provider Implementation**: 
  - Configures CPU mode with required virtualization extensions (vmx for Intel VT-x, svm for AMD-V)
  - Automatically passes through host CPU virtualization features to guest VMs
  - Compatible with QEMU/KVM hypervisors with nested virtualization enabled
- **VT-d/AMD-Vi Support**: Added `vtdEnabled` field in SecurityProfile for Intel VT-d or AMD IOMMU support, improving I/O performance for nested environments
- **CPU/Memory Hot-Add**: Added `cpuHotAddEnabled` and `memoryHotAddEnabled` in PerformanceProfile for dynamic resource scaling without VM restart
- **Virtualization Based Security**: Added `virtualizationBasedSecurity` field in PerformanceProfile for Windows VBS features

#### Security Features
- **TPM (Trusted Platform Module) Support**: 
  - Added `tpmEnabled` and `tpmVersion` fields in VMClass SecurityProfile
  - vSphere Provider: Full TPM 2.0 device support (requires vSphere 6.7+ and VM hardware version 14+)
  - LibVirt Provider: TPM emulator support with tpm-tis model and version 2.0
  - Automatically enforces UEFI firmware requirement when TPM is enabled
  - Enables Windows 11 support and BitLocker encryption capabilities
- **Secure Boot Support**: 
  - Added `secureBoot` field in SecurityProfile for UEFI Secure Boot functionality
  - vSphere Provider: Configures EFI Secure Boot through VM boot options
  - LibVirt Provider: Uses OVMF firmware with Secure Boot capabilities
  - Automatically forces UEFI firmware when enabled
  - Protects against rootkits and bootkits at firmware level
- **Comprehensive Documentation**: 
  - Added `docs/NESTED_VIRTUALIZATION.md` with detailed configuration guide
  - Added `docs/examples/nested-virtualization.yaml` with complete working examples
  - Includes verification steps, troubleshooting guidance, and performance recommendations

#### Use Cases Enabled
- Development and testing of virtualization platforms (e.g., Proxmox, OpenStack, vSphere)
- Running Kubernetes clusters with nested container runtimes
- Creating isolated lab environments for security testing
- Educational scenarios for learning virtualization technologies

#### VM Snapshot Management
- **Complete VMSnapshot CRD**: Full-featured API for VM snapshot lifecycle management
  - Snapshot creation with memory state and filesystem quiescing options
  - Snapshot deletion with proper cleanup
  - Snapshot revert for rollback scenarios
  - Retention policies (maxAge, deleteOnVMDelete, maxCount)
  - Automated scheduling support via cron expressions
  - Snapshot metadata and tagging
- **vSphere Provider Implementation**:
  - Full govmomi-based snapshot operations (Create, Delete, Revert)
  - Memory snapshot support for powered-on VMs
  - Filesystem quiescing with VMware Tools integration
  - Automatic power state handling during revert
  - Hierarchical snapshot tree navigation
  - Synchronous operations for immediate completion
- **LibVirt Provider Implementation**:
  - Full virsh-based snapshot operations (Create, Delete, Revert)
  - Memory snapshot support for running VMs with qcow2 storage
  - Disk-only snapshots for VMs with incompatible storage backends
  - Atomic snapshot creation with --atomic flag
  - Automatic power state preservation during revert
  - Snapshot existence validation before operations
  - Synchronous operations with immediate feedback
  - Snapshot name sanitization for virsh compatibility
  - Helper methods for snapshot listing and querying
- **Proxmox Provider Implementation**:
  - Complete snapshot lifecycle support
  - Memory state inclusion (vmstate)
  - Async task handling with status tracking
  - Full VM creation from templates with cloud-init
  - Intelligent boot order configuration
  - SSH key injection with proper encoding
  - Network configuration with bridge support
  - Storage pool management
- **Controller Integration**:
  - Real provider RPC calls (no more simulation)
  - Proper task status polling for async operations
  - Comprehensive error handling and reporting
  - Event recording for observability
  - Finalizer-based cleanup
- **Transport Layer**:
  - Added snapshot methods to gRPC client
  - TaskStatus RPC for async operation tracking
  - Proper request/response type mapping
- **Use Cases**:
  - Pre-maintenance backups with quick rollback
  - CI/CD testing with snapshot-based environments
  - Disaster recovery and point-in-time restore
  - Development environment versioning

### Fixed

#### vSphere Provider
- **Placement Override Bug**: Fixed critical bug where VirtualMachine `spec.placement.folder`, `spec.placement.datastore`, and `spec.placement.cluster` overrides were not being respected by the vSphere provider. The provider was always using the default values from the Provider CRD instead of honoring the per-VM placement overrides specified in the VirtualMachine manifest. VMs are now correctly created in the specified folder, datastore, and cluster when placement overrides are provided.

## [0.2.1] - 2025-01-29

### Patch Release: Critical Fixes and Documentation Updates

This patch release addresses several critical issues discovered in v0.2.0, including linter compliance fixes, documentation improvements, and enhanced provider capabilities. VirtRigaud v0.2.1 ensures improved stability and usability for production deployments.

### Fixed

#### Code Quality and Compliance
- **Error Handling**: Fixed unchecked error return values in vSphere provider fmt.Sscanf calls
- **Linting Compliance**: Resolved golangci-lint errcheck violations that were causing CI failures
- **CRD Validation**: Fixed CRD validation conflicts for OffGraceful powerState transitions

#### Documentation and Examples
- **Broken Links**: Corrected broken documentation links in README.md
- **Example Updates**: Consolidated and enhanced examples with v0.2.1 features
- **CLI Documentation**: Added comprehensive CLI documentation and reference guides

#### Provider Enhancements
- **VMClass Disk Settings**: Fixed VMClass disk size settings to be properly respected across all providers
- **CRD Schema Sync**: Synchronized Helm chart CRDs with latest schema fixes for consistency

### Added

#### Infrastructure Improvements
- **Build Artifacts**: Enhanced .gitignore to properly exclude dist/ and build artifacts
- **Automated CRD Sync**: Implemented automated CRD synchronization workflow for improved consistency
- **Field Test Exclusions**: Added fieldTest exclusions to .gitignore for cleaner repository

#### vSphere Provider Features
- **Hardware Version Management**: Added VM hardware version management support with version comparison logic
- **Graceful Shutdown**: Implemented graceful shutdown capabilities for virtual machines
- **Enhanced Power States**: Improved power state management with better error handling

### Enhanced

#### Documentation
- **README Updates**: Comprehensive updates to project README with corrected examples and links
- **CLI Reference**: Complete CLI documentation covering all available commands and options
- **Provider Guides**: Enhanced provider-specific documentation with updated examples

#### Development Workflow
- **Release Preparation**: Streamlined release preparation process with automated documentation sync
- **CI/CD Pipeline**: Improved continuous integration with better linting and validation checks

### Technical Details

#### API Stability
- Maintains full backward compatibility with v0.2.0
- No breaking changes to existing APIs or configurations
- CRD schemas remain stable with validation improvements

#### Provider Compatibility
- All existing provider configurations continue to work without modification
- Enhanced error handling improves provider reliability
- VMClass configurations now properly enforce disk size settings

### Deployment Notes

#### Container Images
Updated provider images are available from GitHub Container Registry:
- **Manager**: `ghcr.io/projectbeskar/virtrigaud/manager:v0.2.1`
- **vSphere Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.2.1`
- **LibVirt Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:v0.2.1`
- **Proxmox Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.2.1`
- **Mock Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-mock:v0.2.1`

#### Helm Charts
- **Main Chart**: `virtrigaud/virtrigaud:0.2.1`
- **Provider Runtime Chart**: `virtrigaud/virtrigaud-provider-runtime:0.2.1`

#### Upgrade Path
- Direct upgrade from v0.2.0 with no manual intervention required
- Existing deployments will automatically benefit from the fixes
- No configuration changes needed for standard deployments

### Acknowledgments

This release includes important fixes identified by the community and addresses issues reported in production environments. Thanks to all contributors who helped identify and resolve these issues.

---

## [0.2.0] - 2025-01-15

### Major Release: Production-Ready Provider Architecture

This release marks a significant milestone for VirtRigaud with production-ready vSphere and LibVirt providers, comprehensive documentation, and a complete CLI toolset. VirtRigaud v0.2.0 delivers enterprise-grade virtual machine management across multiple hypervisor platforms.

### Added

#### Core Features
- **Remote Provider Architecture**: Complete implementation of the remote provider model with gRPC communication
- **Production-Ready vSphere Provider**: Full VMware vSphere integration with enterprise features
- **Production-Ready LibVirt Provider**: Comprehensive KVM/QEMU support via virsh-based implementation
- **Advanced Storage Management**: Storage pools, volume operations, and cloud image handling
- **Enhanced Cloud-Init Support**: NoCloud datasource implementation with ISO generation
- **QEMU Guest Agent Integration**: Enhanced guest OS monitoring and communication

#### CLI Tools Suite
- **vrtg**: Complete virtual machine management CLI with resource operations
- **vcts**: Conformance testing suite for provider validation
- **vrtg-provider**: Provider development toolkit for scaffolding and code generation
- **virtrigaud-loadgen**: Load testing and performance benchmarking tool

#### Provider Capabilities

**vSphere Provider:**
- VM creation from templates, OVA/OVF files, and content libraries
- Power management with suspend/resume support
- Advanced networking with distributed switches and port groups
- Snapshot management with memory state preservation
- Template and content library integration
- High availability and DRS configuration support
- Storage policy management and vSAN integration
- Comprehensive error handling and async task monitoring

**LibVirt Provider:**
- VM creation from cloud images with automatic download
- Storage pool and volume management with multiple backends
- Network configuration with bridges and virtual networks
- Cloud-init integration via NoCloud ISO generation
- QEMU Guest Agent support for enhanced monitoring
- Snapshot operations with storage-dependent features
- Resource configuration and management
- Performance optimization with virtio drivers

#### Documentation
- **Comprehensive Provider Documentation**: Detailed guides for each supported provider
- **CLI Reference Manual**: Complete documentation for all command-line tools
- **Provider Capabilities Matrix**: Feature comparison and implementation status
- **Architecture Documentation**: Remote provider design and configuration flows
- **Examples and Tutorials**: Real-world configuration examples and best practices

### Enhanced

#### Core Improvements
- **Provider Registry**: Centralized provider discovery and capability reporting
- **Error Handling**: Improved error classification and retry logic
- **Resource Management**: Enhanced VM lifecycle management with proper cleanup
- **Network Configuration**: Advanced networking features across all providers
- **Monitoring Integration**: Comprehensive metrics and observability features

#### CI/CD Pipeline
- **Automated Testing**: Enhanced test coverage with conformance testing
- **Release Automation**: Streamlined build and release processes
- **Documentation Generation**: Automated API reference and capability documentation
- **Quality Assurance**: Comprehensive linting and static analysis

### Fixed

#### Stability and Reliability
- **Connection Management**: Robust connection handling with automatic retry
- **Resource Cleanup**: Proper cleanup of VM resources and associated storage
- **Memory Management**: Improved memory usage in provider implementations
- **Concurrent Operations**: Thread-safe operations and proper synchronization
- **Error Recovery**: Enhanced error recovery and graceful degradation

#### Provider-Specific Fixes
- **vSphere**: Resolved template deployment and network configuration issues
- **LibVirt**: Fixed storage pool management and cloud-init generation
- **Cross-Platform**: Improved compatibility across different hypervisor versions

### Technical Details

#### API Changes
- **Stable v1beta1 API**: Production-ready API with comprehensive resource definitions
- **Provider Contract**: Standardized provider interface with capability discovery
- **Resource Schemas**: Enhanced CRD schemas with validation and defaults
- **Backward Compatibility**: Seamless upgrade path from previous versions

#### Performance Improvements
- **Async Operations**: Non-blocking VM operations with progress tracking
- **Connection Pooling**: Efficient resource utilization in provider connections
- **Caching**: Intelligent caching of templates, images, and metadata
- **Batch Operations**: Support for bulk VM operations where applicable

#### Security Enhancements
- **Credential Management**: Secure handling of hypervisor credentials
- **Network Isolation**: Provider network isolation with configurable policies
- **RBAC Integration**: Fine-grained role-based access control
- **Audit Logging**: Comprehensive audit trail for all operations

### Provider Feature Matrix

| Feature | vSphere | LibVirt | Status |
|---------|---------|---------|---------|
| VM Lifecycle | Complete | Complete | Production |
| Power Management | Complete | Complete | Production |
| Storage Management | Complete | Complete | Production |
| Network Configuration | Complete | Complete | Production |
| Snapshot Operations | Complete | Storage-dependent | Production |
| Template Management | Complete | Cloud Images | Production |
| Guest Integration | VMware Tools | QEMU Guest Agent | Production |
| High Availability | Complete | Planned | vSphere Only |
| Live Migration | Complete | Planned | vSphere Only |
| Hot Reconfiguration | Complete | Restart Required | Mixed |

### Deployment and Operations

#### Container Images
All provider images are available from GitHub Container Registry:
- **Manager**: `ghcr.io/projectbeskar/virtrigaud/manager:v0.2.0`
- **vSphere Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.2.0`
- **LibVirt Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:v0.2.0`
- **Proxmox Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.2.0`
- **Mock Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-mock:v0.2.0`

#### Helm Charts
- **Main Chart**: `virtrigaud/virtrigaud:0.2.0`
- **Provider Runtime Chart**: `virtrigaud/virtrigaud-provider-runtime:0.2.0`

#### Installation Methods
- Helm charts with comprehensive configuration options
- Kustomize overlays for different deployment scenarios
- Direct YAML manifests for custom deployments
- CLI-based installation and management

### Upgrade Notes

#### Breaking Changes
- None. This release maintains full backward compatibility with v0.1.x deployments.

#### Migration Guide
- Existing v0.1.x deployments can be upgraded in-place
- Provider configurations are automatically migrated
- No manual intervention required for standard deployments

#### Deprecations
- None in this release. All APIs remain stable and supported.

### Known Issues

#### Current Limitations
- **LibVirt Hot Reconfiguration**: CPU and memory changes require VM restart
- **LibVirt Memory Snapshots**: Not supported on all storage backends
- **Cross-Provider Migration**: Not yet implemented between different provider types

#### Workarounds
- Detailed workarounds are documented in the provider-specific guides
- Community support available for deployment-specific issues

### Acknowledgments

This release includes contributions from the VirtRigaud development team and community feedback from early adopters. Special thanks to all contributors who helped shape this production-ready release.

### What's Next

#### Roadmap for v0.3.0
- **Enhanced LibVirt Features**: Live migration and hot reconfiguration support
- **Proxmox VE Provider**: Production-ready Proxmox integration
- **Multi-Cloud Providers**: AWS EC2, Azure, and GCP provider implementations
- **Advanced Networking**: Service mesh integration and network policies
- **Backup and Recovery**: Integrated backup solutions and disaster recovery

#### Community
- Join our community discussions on GitHub
- Contribute to provider development and documentation
- Report issues and feature requests through GitHub Issues

For detailed upgrade instructions and deployment guides, see the [Installation Documentation](docs/install-helm-only.md).

For provider-specific configuration and capabilities, see the [Provider Documentation](docs/providers/).

---

**Full Changelog**: https://github.com/projectbeskar/virtrigaud/compare/v0.2.0...v0.2.1
#### Proxmox Provider CRD Integration (Completed)

The Proxmox provider now has full CRD integration for template-based VM deployment:

**VMImage CRD - ProxmoxImageSource**:
- Template ID or template name references
- Storage pool selection for cloned VMs
- Node specification for template location
- Full clone vs linked clone selection
- Disk format configuration (qcow2, raw, vmdk)

**VMNetworkAttachment CRD - ProxmoxNetworkConfig**:
- Linux bridge selection (vmbr0, vmbr1, etc.)
- Network card model selection (virtio, e1000, rtl8139, vmxnet3)
- VLAN tagging support
- Proxmox firewall integration
- Bandwidth rate limiting
- MTU configuration

**Controller Integration**:
- Full parsing of Proxmox-specific fields from CRDs
- Conversion to provider contracts and gRPC messages
- Proper JSON field mapping (TemplateName, Bridge, Model, etc.)

**Documentation**:
- Complete provider documentation in `docs/providers/PROXMOX.md`
- Working examples in `examples/proxmox/`
- Troubleshooting guides for common issues

**Impact**: The Proxmox provider now has feature parity with vSphere and LibVirt providers, enabling production-ready VM management on Proxmox VE clusters via Kubernetes CRDs.

