<!-- a0e316f7-4bbb-458d-ba10-4d2844c9038b 0844c86b-3597-44a1-91db-9d478891ee55 -->
# Documentation Alignment for v0.2.3

## Scope

Comprehensive documentation update to reflect v0.2.3 provider feature parity, remove obsolete files, update version references, fix documentation structure issues, and ensure all examples are current.

## Phase 1: Cleanup Obsolete Files

### Root-Level Development Notes (DELETE)

- `PROXMOX_STATUS.md` - Outdated status, info now in CHANGELOG
- `PROXMOX_IMPLEMENTATION_COMPLETE.md` - Historical, no longer relevant
- `PROXMOX_CRD_INTEGRATION_PLAN.md` - Completed work, documented in CHANGELOG
- `PROXMOX_SSH_KEYS_SOLUTION.md` - Info now in provider docs
- `PROXMOX_SSH_TROUBLESHOOTING.md` - Info now in provider docs
- `provider-feature-assessment.plan.md` - Planning artifact
- `.release-checklist-v0.2.2-rc1.md` - Old release artifact

### Empty Directories (DELETE)

- `examples/proxmox/` - Empty directory, examples in docs/examples/

## Phase 2: Update Main Documentation Structure

### docs/README.md (MAJOR REWRITE)

**Issue**: References non-existent directories (concepts/, user-guides/, admin-guides/, security/, developer/)

**Fix**: Rewrite to reflect actual documentation structure:

- Remove broken links to non-existent sections
- Update navigation to actual documentation files
- Fix version from v0.2.0 to v0.2.3
- Add links to actual provider docs, examples, and guides

### Root README.md (UPDATE)

**Current Issues**:

- Helm install example shows `--version v0.2.2`
- Provider Feature Matrix outdated (doesn't show new v0.2.3 features)
- Missing Reconfigure, TaskStatus, Clone, ConsoleURL capabilities

**Updates Needed**:

- Update version references to v0.2.3
- Update feature matrix with new capabilities:
  - vSphere: Reconfigure (full support), TaskStatus, Clone, ConsoleURL
  - Libvirt: Reconfigure (requires restart), VNC ConsoleURL
  - Proxmox: Guest agent IP detection
- Update provider maturity assessment
- Update supported operations table

## Phase 3: Update Provider Capabilities Documentation

### docs/PROVIDERS_CAPABILITIES.md (COMPREHENSIVE UPDATE)

**Current State**: Last updated for v0.2.0, missing all v0.2.3 enhancements

**Updates Required**:

#### Version and Header

- Update version from v0.2.0 to v0.2.3 throughout
- Update "as of v0.2.0" references

#### Core Operations Table (lines 32-40)

- Update Reconfigure row: vSphere ✅ Complete, Libvirt ⚠️ Restart Required, Proxmox ✅ Complete
- Add TaskStatus row for async operation tracking
- Add ConsoleURL row for remote console access

#### Resource Management Table (lines 43-51)

- Update "Hot CPU Add" and "Hot Memory Add" with accurate vSphere support details
- Note vSphere Reconfigure now fully implemented

#### VM Lifecycle Table (lines 78-85)

- Update Clone Operations to show vSphere ✅ Complete
- Update VM Reconfiguration details for all providers

#### Monitoring & Observability (lines 129-137)

- Add Console URL Generation row
- Update IP detection for Proxmox (guest agent support)

#### Provider-Specific Features

- vSphere: Add Reconfigure, TaskStatus, Clone, ConsoleURL
- Libvirt: Add Reconfigure, VNC Console URLs
- Proxmox: Add Guest Agent IP detection

#### Provider Images (lines 197-204)

- Update all image tags from v0.2.0 to v0.2.3

#### Version History (lines 283-286)

- Add entry: "v0.2.3: Provider feature parity - Reconfigure, Clone, TaskStatus, ConsoleURL"

## Phase 4: Update Provider-Specific Documentation

### docs/providers/vsphere.md

**Add New Features Section** (after line 17):

```markdown
- **TaskStatus**: Async operation tracking with progress monitoring
```

**Add Feature Details**:

- Reconfigure section: Explain CPU/memory/disk hot-add capabilities
- Clone section: Full and linked clone support with snapshot handling
- TaskStatus section: Async operation tracking with govmomi task manager
- Console Access section: Web client console URL generation

**Update Examples**: Ensure all YAML examples reference v0.2.3 images

### docs/providers/libvirt.md

**Add New Features Section** (after line 21):

```markdown
- **Reconfigure**: VM resource modification (requires restart for most changes)
- **VNC Console Access**: VNC console URL generation for remote access
```

**Add Feature Details**:

- Reconfigure section: Explain virsh setvcpus/setmem/vol-resize, restart requirements
- Console section: VNC URL generation and viewer compatibility

**Update Examples**: Ensure all YAML examples reference v0.2.3 images

### docs/providers/proxmox.md or docs/providers/PROXMOX.md

**Note**: Two files exist - proxmox.md and PROXMOX.md (merge or deduplicate if needed)

**Add New Features Section** (after line 16):

```markdown
- **Guest Agent Integration**: Enhanced IP address detection via QEMU guest agent
```

**Add Feature Details**:

- Guest Agent section: Explain IP detection, network interface extraction
- Requirements: Document need for qemu-guest-agent in VM

**Update Examples**: Ensure all YAML examples reference v0.2.3 images

## Phase 5: Update Examples

### docs/examples/README.md (UPDATE)

**Current Issues**:

- References v0.2.1 features prominently
- Version compatibility section outdated

**Updates**:

- Update feature summary to v0.2.3
- Add v0.2.3 feature highlights (Reconfigure, Clone, TaskStatus, ConsoleURL)
- Update version compatibility section
- Update all provider image references to v0.2.3

### Individual Example Files (AUDIT & UPDATE)

Review and update these files:

- `complete-example.yaml` - Update image versions, add reconfigure examples
- `vsphere-advanced-example.yaml` - Add clone, reconfigure examples
- `libvirt-advanced-example.yaml` - Add reconfigure examples
- `libvirt-complete-example.yaml` - Update image versions
- `proxmox-complete-example.yaml` - Add guest agent examples
- `multi-provider-example.yaml` - Update all provider images
- `provider-vsphere.yaml` - Update to v0.2.3
- `provider-libvirt.yaml` - Update to v0.2.3
- `advanced/vm-reconfigure-and-snapshot.yaml` - Validate works with v0.2.3
- `advanced/vm-reconfigure-patch.yaml` - Validate reconfigure examples

### Add New Examples (CREATE)

- `examples/advanced/vsphere-clone-example.yaml` - Demonstrate vSphere cloning
- `examples/advanced/vsphere-task-tracking.yaml` - Demonstrate TaskStatus
- `examples/advanced/console-access-example.yaml` - Demonstrate console URLs

## Phase 6: Update Supporting Documentation

### docs/EXAMPLES.md (UPDATE)

- Update version references from v0.2.0 to v0.2.3
- Add new example categories for v0.2.3 features

### docs/CLI.md (AUDIT)

- Verify CLI documentation matches current implementation
- Update version references

### docs/getting-started/quickstart.md (UPDATE)

- Update Helm install commands to use v0.2.3
- Update provider image references
- Update feature descriptions

### docs/install-helm-only.md (UPDATE)

- Update version references to v0.2.3
- Update Helm chart version references

## Phase 7: Verify and Validate

### Cross-Reference Check

- Ensure all internal links work
- Verify example files parse correctly (YAML validation)
- Check that provider image tags are consistent

### Feature Consistency Check

- Provider docs match PROVIDERS_CAPABILITIES.md
- CHANGELOG.md matches provider documentation
- README.md feature matrix matches detailed docs

### Version Consistency Check

- All version references updated to v0.2.3
- All container image tags reference v0.2.3
- Helm chart versions consistent

## Summary of Changes

### Files to Delete (9)

- 7 root-level Proxmox development notes
- 1 planning artifact
- 1 empty directory

### Files to Update (20+)

- Main README.md
- docs/README.md
- docs/PROVIDERS_CAPABILITIES.md
- docs/providers/vsphere.md
- docs/providers/libvirt.md
- docs/providers/proxmox.md
- docs/examples/README.md
- docs/EXAMPLES.md
- docs/getting-started/quickstart.md
- docs/install-helm-only.md
- 10+ example YAML files

### Files to Create (3)

- New advanced examples showcasing v0.2.3 features

### Key Themes

1. Version alignment: v0.2.0/v0.2.1/v0.2.2 → v0.2.3
2. Feature updates: Document Reconfigure, Clone, TaskStatus, ConsoleURL
3. Structure fixes: Fix broken docs/README.md navigation
4. Cleanup: Remove obsolete development notes
5. Consistency: Ensure all docs tell the same story

### To-dos

- [ ] Delete obsolete Proxmox development notes and empty directories
- [ ] Update root README.md with v0.2.3 versions and new feature matrix
- [ ] Rewrite docs/README.md to fix broken structure and navigation
- [ ] Comprehensive update to docs/PROVIDERS_CAPABILITIES.md for v0.2.3
- [ ] Update docs/providers/vsphere.md with Reconfigure, Clone, TaskStatus, ConsoleURL
- [ ] Update docs/providers/libvirt.md with Reconfigure and VNC ConsoleURL
- [ ] Update docs/providers/proxmox.md with guest agent IP detection
- [ ] Update docs/examples/README.md for v0.2.3 features
- [ ] Audit and update all example YAML files with v0.2.3 versions
- [ ] Create new examples for vSphere clone, TaskStatus, and console access
- [ ] Update EXAMPLES.md, quickstart.md, install-helm-only.md
- [ ] Validate all links, YAML syntax, and cross-references