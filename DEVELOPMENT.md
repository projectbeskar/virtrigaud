# virtrigaud Development Guide

This guide helps you set up a local development environment for fast iteration on virtrigaud.

## Quick Start

### Prerequisites

```bash
# Install required tools
brew install kind helm kubectl fswatch  # macOS
# or
apt-get install kind helm kubectl inotify-tools  # Ubuntu
```

### One-Command Development Setup

```bash
# Deploy to local Kind cluster
make dev-deploy
```

This will:
- Create a Kind cluster (Kubernetes v1.31.2) with local registry
- Build all images locally
- Deploy virtrigaud with Helm
- Set up port forwarding and show status

### Fast Development Workflow

1. **Initial deployment:**
   ```bash
   make dev-deploy
   ```

2. **Make code changes** in your editor

3. **Hot reload** (manual):
   ```bash
   make dev-reload
   ```

4. **Or use automatic file watching:**
   ```bash
   make dev-watch  # In separate terminal
   ```

## Commands Reference

| Command | Description |
|---------|-------------|
| `make dev-deploy` | Full deployment to local Kind cluster |
| `make dev-reload` | Rebuild images and restart pods |
| `make dev-status` | Show deployment status |
| `make dev-logs` | Follow controller manager logs |
| `make dev-shell` | Get shell in manager pod |
| `make dev-watch` | Auto-reload on file changes |
| `make dev-cleanup` | Clean up everything |

## Development Tips

### Fast Iteration Loop

```bash
# Terminal 1: Start auto-watcher
make dev-watch

# Terminal 2: Make code changes and see automatic reloads
# Edit internal/controller/virtualmachine_controller.go
# Save file → automatic rebuild and restart

# Terminal 3: Watch logs
make dev-logs
```

### Testing Specific Components

```bash
# Skip image builds if you only changed configs
SKIP_BUILD=true make dev-reload

# Use different cluster name
CLUSTER_NAME=my-test make dev-deploy

# Use different tag
TAG=my-feature make dev-deploy
```

### Debugging

```bash
# Get shell in manager pod
make dev-shell

# Port forward metrics endpoint
kubectl port-forward svc/virtrigaud-controller-manager-metrics-service 8443:8443 -n virtrigaud-system

# Check CRDs
kubectl get crds | grep virtrigaud

# Create test resources
kubectl apply -f examples/vm-ubuntu-small.yaml
```

### Working with Providers

```bash
# Check provider status
kubectl get providers -A

# Create a libvirt provider
kubectl apply -f examples/provider-libvirt.yaml

# Test VM creation
kubectl apply -f examples/libvirt-complete-example.yaml
```

## Architecture Overview

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────┐
│   Kind Cluster  │    │ Local Registry   │    │ Your Code       │
│                 │    │                  │    │                 │
│  ┌──────────────┐│    │ localhost:5001   │    │ internal/       │
│  │ virtrigaud   ││◄───┤                  │◄───┤ cmd/            │
│  │ manager      ││    │ Images:          │    │ api/            │
│  └──────────────┘│    │ - manager:dev-xx │    │                 │
│                 │    │ - provider-*:..  │    └─────────────────┘
│  ┌──────────────┐│    └──────────────────┘              │
│  │ Providers    ││                                      │
│  │ (libvirt,    ││                                      │
│  │  vsphere,    ││                               ┌──────▼──────┐
│  │  proxmox)    ││                               │ File Watcher│
│  └──────────────┘│                               │ (fswatch/   │
└─────────────────┘                               │ inotify)    │
                                                  └─────────────┘
```

## Troubleshooting

### Common Issues

1. **Port conflicts:**
   ```bash
   # Change registry port if 5001 is taken
   REGISTRY_PORT=5002 make dev-deploy
   ```

2. **Build failures:**
   ```bash
   # Clean and rebuild
   make dev-cleanup
   make dev-deploy
   ```

3. **Image pull issues:**
   ```bash
   # Check registry connection
   docker ps | grep registry
   
   # Restart registry
   docker restart kind-registry
   ```

4. **CRD conflicts:**
   ```bash
   # Clean CRDs
   kubectl delete crd -l app.kubernetes.io/name=virtrigaud
   make dev-deploy
   ```

### Performance Optimization

- Use `SKIP_BUILD=true` when only changing configs
- Run `make dev-watch` for automatic reloads
- Use multiple terminals for parallel operations
- Keep local registry running between deployments

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CLUSTER_NAME` | `virtrigaud-dev` | Kind cluster name |
| `REGISTRY_PORT` | `5001` | Local registry port |
| `TAG` | `dev-<git-hash>` | Image tag |
| `HELM_NAMESPACE` | `virtrigaud-system` | Kubernetes namespace |
| `SKIP_BUILD` | `false` | Skip image building |
| `CONTAINER_TOOL` | `docker` | Container tool (docker/podman) |

## Integration with IDE

### VS Code

Add to `.vscode/tasks.json`:

```json
{
    "version": "2.0.0",
    "tasks": [
        {
            "label": "virtrigaud: dev-deploy",
            "type": "shell",
            "command": "make dev-deploy",
            "group": "build",
            "presentation": {
                "echo": true,
                "reveal": "always",
                "focus": false,
                "panel": "shared"
            }
        },
        {
            "label": "virtrigaud: dev-reload",
            "type": "shell",
            "command": "make dev-reload",
            "group": "build"
        }
    ]
}
```

### IntelliJ/GoLand

Add run configurations for the make targets.

## Next Steps

- Explore the [examples/](examples/) directory for sample resources
- Read the [API reference](docs/api-reference/) for CRD details
- Check [provider documentation](docs/providers/) for provider-specific setup
- Contribute improvements to this development workflow!
