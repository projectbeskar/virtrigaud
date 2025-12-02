# Documentation Structure

This document describes the VirtRigaud documentation organization, inspired by industry best practices from the Bindy project.

## Structure Overview

The documentation is organized into **six main chapters** that guide users through their journey from installation to advanced topics:

### 1. Getting Started
**Purpose**: Get new users up and running quickly

- **Installation**
  - Prerequisites - System requirements
  - Quick Start - 5-minute getting started
  - Installing CRDs - Custom Resource Definitions
  - Deploying the Controller - Controller deployment
  - Helm Installation - Production Helm setup
  - Helm CRD Upgrades - CRD upgrade management

- **Basic Concepts**
  - Architecture Overview - System design
  - Custom Resource Definitions - CRD reference
  - Provider Architecture - Provider abstraction
  - Provider Capabilities - Feature matrix
  - Remote Providers - Remote provider architecture
  - Status Update Logic - Status reconciliation

### 2. User Guide
**Purpose**: Teach users how to accomplish common tasks

- **Managing Virtual Machines**
  - Creating VMs - Step-by-step VM creation
  - VM Configuration - CPU, memory, storage, networking
  - VM Lifecycle - Power operations
  - Graceful Shutdown - Shutdown handling

- **Provider Configuration**
  - vSphere Provider - VMware vCenter/ESXi
  - Libvirt Provider - KVM/QEMU
  - Proxmox Provider - Proxmox VE
  - Provider Tutorial - Step-by-step setup
  - Libvirt Host Preparation - Host setup

- **VM Migration**
  - Migration User Guide - Migration walkthrough
  - VM Migration Guide - Advanced scenarios

### 3. Operations
**Purpose**: Help operators run VirtRigaud in production

- **Configuration**
  - Provider Versioning
  - Resource Management
  - RBAC

- **Monitoring**
  - Observability
  - Status Conditions
  - Logging
  - Metrics Catalog

- **Troubleshooting**
  - Common Issues
  - Debugging
  - FAQ

- **Maintenance**
  - Upgrade Guide
  - Resilience

### 4. Advanced Topics
**Purpose**: Cover advanced use cases and configurations

- **High Availability**
  - Cluster Configuration
  - Failover Strategies

- **Security**
  - Security Overview
  - Bearer Token Authentication
  - mTLS Configuration
  - External Secrets
  - Network Policies

- **Performance**
  - Nested Virtualization
  - vSphere Hardware Versions
  - Tuning

- **Integration**
  - Custom Providers
  - GitOps

### 5. Developer Guide
**Purpose**: Enable developers to contribute and extend VirtRigaud

- **Development Setup**
  - Building from Source
  - Running Tests
  - Testing Workflows Locally
  - Development Workflow

- **Architecture Deep Dive**
  - Controller Design
  - Reconciliation Logic
  - Provider Integration
  - CRD Development Workflow

- **Contributing**
  - Code Style
  - Testing Guidelines
  - Pull Request Process

### 6. Reference
**Purpose**: Provide comprehensive API and configuration reference

- **API Reference**
  - CRD API Reference
  - API Types
  - Provider Contract
  - SDK Reference
  - Utilities
  - VirtualMachine Spec
  - VMClass Spec
  - Provider Spec
  - Status Conditions

- **CLI Reference**
  - CLI Tools Reference
  - CLI API Reference

- **Examples**
  - Basic Examples
  - Simple Setup
  - Production Setup

- **Catalogs**
  - Provider Catalog
  - Migration API Reference

## Design Principles

### 1. User Journey Focus
Documentation follows natural user progression:
- Installation → Concepts → Usage → Operations → Advanced → Reference

### 2. Progressive Disclosure
Information is layered from simple to complex:
- Quick Start for beginners
- User Guide for common tasks
- Advanced Topics for power users
- Reference for complete details

### 3. Task-Oriented
Documentation focuses on accomplishing tasks:
- "Creating VMs" not "VirtualMachine Resource"
- "Troubleshooting" not "Error Messages"

### 4. Clear Navigation
Hierarchy makes finding information intuitive:
- Main chapters (6)
- Sections within chapters
- Topics within sections

## Comparison with Bindy Structure

VirtRigaud's structure closely mirrors Bindy's proven organization:

| Bindy Chapter | VirtRigaud Chapter | Notes |
|---------------|-------------------|-------|
| Getting Started | Getting Started | ✅ Same structure |
| User Guide | User Guide | ✅ Same structure |
| Operations | Operations | ✅ Same structure |
| Advanced Topics | Advanced Topics | ✅ Same structure |
| Developer Guide | Developer Guide | ✅ Same structure |
| Reference | Reference | ✅ Same structure |

## File Organization

```
docs/src/
├── SUMMARY.md              # Table of contents
├── README.md               # Introduction
├── installation/           # Getting Started - Installation
│   ├── installation.md
│   ├── prerequisites.md
│   ├── crds.md
│   └── controller.md
├── concepts/               # Getting Started - Concepts
│   ├── concepts.md
│   ├── architecture.md
│   └── status-update-logic.md
├── guide/                  # User Guide
│   ├── virtual-machines.md
│   ├── creating-vms.md
│   ├── vm-configuration.md
│   ├── providers.md
│   └── migration.md
├── operations/             # Operations
│   ├── configuration.md
│   ├── monitoring.md
│   ├── troubleshooting.md
│   └── maintenance.md
├── advanced/               # Advanced Topics
│   ├── ha.md
│   ├── security.md
│   ├── performance.md
│   └── integration.md
├── development/            # Developer Guide
│   ├── setup.md
│   ├── architecture-deep-dive.md
│   └── contributing.md
├── reference/              # Reference
│   ├── api.md
│   ├── cli.md
│   └── examples.md
├── providers/              # Provider-specific docs
├── api-reference/          # API reference files
├── migration/              # Migration guides
├── changelog.md
└── license.md
```

## Benefits

### For New Users
- Clear starting point (Getting Started)
- Progressive learning path
- Quick wins with Quick Start

### For Operators
- Dedicated Operations chapter
- Easy troubleshooting access
- Production-ready examples

### For Developers
- Complete Developer Guide
- Architecture documentation
- Clear contribution process

### For Everyone
- Intuitive navigation
- Easy to find information
- Professional presentation

## Implementation Status

✅ **Completed:**
- [SUMMARY.md](SUMMARY.md) reorganized with new structure
- Directory structure created
- Placeholder files generated
- CHANGELOG.md updated

🔄 **In Progress:**
- Filling placeholder content
- Moving existing content to new locations
- Creating missing documentation

📋 **TODO:**
- Complete all placeholder files
- Add diagrams and examples
- Generate API reference docs
- Test mdbook build

## Building the Documentation

```bash
# Install mdbook
cargo install mdbook

# Build documentation
cd docs
mdbook build

# Serve locally
mdbook serve --open
```

## Contributing to Documentation

When adding new documentation:

1. **Find the right chapter** - Place content in the appropriate section
2. **Update SUMMARY.md** - Add new pages to the table of contents
3. **Follow structure** - Match the style of existing pages
4. **Add examples** - Include code examples and YAML snippets
5. **Link related topics** - Cross-reference related documentation

## Maintenance

The documentation structure should remain stable. Changes to the organization require:
- Team discussion
- User feedback consideration
- Version coordination (if breaking)

## References

- [Bindy Documentation](https://github.com/tinyzimmer/bindy/tree/main/docs/src)
- [mdBook Documentation](https://rust-lang.github.io/mdBook/)
- [Kubernetes Documentation Style Guide](https://kubernetes.io/docs/contribute/style/style-guide/)
