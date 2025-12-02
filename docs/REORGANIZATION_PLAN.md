# Documentation Reorganization Plan

## Overview

The VirtRigaud documentation has been reorganized following the structure used in Bindy, providing better navigation and clearer separation of concerns.

## New Structure

The documentation is now organized into these main chapters:

### 1. Getting Started
- **Installation** - All installation methods and prerequisites
- **Basic Concepts** - Core concepts, architecture, CRDs, providers

### 2. User Guide
- **Managing Virtual Machines** - Creating, configuring, and managing VMs
- **Provider Configuration** - Setting up vSphere, Libvirt, Proxmox providers
- **VM Migration** - Migration between providers and clusters

### 3. Operations
- **Configuration** - Controller and provider configuration
- **Monitoring** - Observability, metrics, logging, status conditions
- **Troubleshooting** - Common issues, debugging, FAQ
- **Maintenance** - Upgrades and resilience

### 4. Advanced Topics
- **High Availability** - HA setup, cluster configuration, failover
- **Security** - mTLS, bearer tokens, external secrets, network policies
- **Performance** - Nested virtualization, hardware versions, tuning
- **Integration** - Custom providers, GitOps

### 5. Developer Guide
- **Development Setup** - Building, testing, workflow
- **Architecture Deep Dive** - Controller design, reconciliation, provider integration
- **Contributing** - Code style, testing guidelines, PR process

### 6. Reference
- **API Reference** - VirtualMachine, VMClass, Provider specs, status conditions
- **CLI Reference** - CLI tools and kubectl plugin
- **Examples** - Simple and production setup examples

## Implementation Steps

### Step 1: SUMMARY.md Updated ✅

The [docs/src/SUMMARY.md](src/SUMMARY.md) has been reorganized with:
- Clear chapter divisions following Bindy's structure
- Logical grouping of related topics
- Progressive difficulty (Getting Started → User Guide → Operations → Advanced → Developer)
- Comprehensive reference section

### Step 2: Create Missing Directories

Create the new directory structure:

```bash
cd /Users/erick/dev/virtrigaud/docs/src

# Create main chapter directories
mkdir -p installation
mkdir -p guide
mkdir -p operations
mkdir -p advanced
mkdir -p reference

# Subdirectories already exist
# - concepts/
# - providers/
# - development/
# - getting-started/
# - migration/
# - api-reference/
```

### Step 3: Create Placeholder Files

A script has been prepared at `/tmp/create_doc_structure.sh` that will create all the placeholder files referenced in the new SUMMARY.md. Execute it with:

```bash
bash /tmp/create_doc_structure.sh
```

This creates comprehensive placeholder files for:

**Installation:**
- installation/installation.md
- installation/prerequisites.md
- installation/crds.md
- installation/controller.md

**User Guide:**
- guide/virtual-machines.md
- guide/creating-vms.md
- guide/vm-configuration.md
- guide/providers.md
- guide/migration.md

**Operations:**
- operations/configuration.md
- operations/resources.md
- operations/rbac.md
- operations/monitoring.md
- operations/status.md
- operations/logging.md
- operations/troubleshooting.md
- operations/common-issues.md
- operations/debugging.md
- operations/faq.md
- operations/maintenance.md

**Advanced Topics:**
- advanced/ha.md
- advanced/cluster-config.md
- advanced/failover.md
- advanced/security.md
- advanced/performance.md
- advanced/tuning.md
- advanced/integration.md
- advanced/custom-providers.md
- advanced/gitops.md

**Developer Guide:**
- development/setup.md
- development/building.md
- development/testing.md
- development/workflow.md
- development/architecture-deep-dive.md
- development/controller-design.md
- development/reconciliation.md
- development/provider-integration.md
- development/contributing.md
- development/code-style.md
- development/testing-guidelines.md
- development/pr-process.md

**Reference:**
- reference/api.md
- reference/virtualmachine-spec.md
- reference/vmclass-spec.md
- reference/provider-spec.md
- reference/status-conditions.md
- reference/cli.md
- reference/examples.md
- reference/examples-simple.md
- reference/examples-production.md

**Concepts:**
- concepts/concepts.md
- concepts/architecture.md

**Root Level:**
- changelog.md
- license.md

### Step 4: Move Existing Documentation (Manual)

Some existing files are already in place and referenced correctly:
- ✅ README.md (Introduction)
- ✅ getting-started/quickstart.md
- ✅ install-helm-only.md
- ✅ HELM_CRD_UPGRADES.md
- ✅ CRDs.md
- ✅ PROVIDERS.md
- ✅ PROVIDERS_CAPABILITIES.md
- ✅ REMOTE_PROVIDERS.md
- ✅ concepts/status-update-logic.md
- ✅ providers/* (all provider docs)
- ✅ migration/* (migration docs)
- ✅ api-reference/* (API reference docs)
- ✅ ADVANCED_LIFECYCLE.md
- ✅ GRACEFUL_SHUTDOWN.md
- ✅ VM_MIGRATION_GUIDE.md
- ✅ VSPHERE_HARDWARE_VERSION.md
- ✅ NESTED_VIRTUALIZATION.md
- ✅ OBSERVABILITY.md
- ✅ SECURITY.md
- ✅ RESILIENCE.md
- ✅ UPGRADE.md
- ✅ CLI.md
- ✅ catalog.md
- ✅ EXAMPLES.md
- ✅ TESTING_WORKFLOWS_LOCALLY.md
- ✅ development/crd-workflow.md

No files need to be moved - they're referenced in their current locations.

### Step 5: Fill in Placeholder Content (Progressive)

The placeholder files created by the script include:

1. **Overview sections** - Explaining what each chapter covers
2. **Navigation links** - Linking to related topics
3. **Code examples** - Real examples from VirtRigaud
4. **Quick starts** - Getting started quickly in each section
5. **Cross-references** - Links to existing documentation

These can be expanded progressively as content is developed.

### Step 6: Build and Verify

After creating the structure:

```bash
cd /Users/erick/dev/virtrigaud/docs
mdbook build
mdbook serve
```

Visit http://localhost:3000 to preview the reorganized documentation.

### Step 7: Update CHANGELOG

Document this reorganization in the project's CHANGELOG.md.

## Benefits of New Structure

1. **Better Navigation** - Clear progression from getting started to advanced topics
2. **Easier to Find Content** - Related content grouped together
3. **User-Centric** - Organized by user journey (install → use → operate → develop)
4. **Comprehensive** - All aspects covered (operations, security, development)
5. **Professional** - Follows industry best practices (like Bindy)
6. **Maintainable** - Clear place for new content

## Comparison with Bindy Structure

| Bindy Chapter | VirtRigaud Chapter | Status |
|---------------|-------------------|--------|
| Getting Started | Getting Started | ✅ Mapped |
| User Guide | User Guide | ✅ Mapped |
| Operations | Operations | ✅ Mapped |
| Advanced Topics | Advanced Topics | ✅ Mapped |
| Developer Guide | Developer Guide | ✅ Mapped |
| Reference | Reference | ✅ Mapped |

## Next Steps

1. Execute the script to create placeholder files:
   ```bash
   bash /tmp/create_doc_structure.sh
   ```

2. Build and verify the docs:
   ```bash
   cd docs && mdbook build && mdbook serve
   ```

3. Progressively fill in missing content in placeholder files

4. Update cross-references in existing docs to point to new structure

5. Add more detailed examples and diagrams

6. Update CHANGELOG.md

## Files Reference

- **New SUMMARY.md**: `/Users/erick/dev/virtrigaud/docs/src/SUMMARY.md`
- **Creation Script**: `/tmp/create_doc_structure.sh`
- **This Plan**: `/Users/erick/dev/virtrigaud/docs/REORGANIZATION_PLAN.md`
