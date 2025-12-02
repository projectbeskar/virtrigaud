# Deploying the Controller

After installing CRDs, deploy the VirtRigaud controller.

## Using Helm (Recommended)

```bash
helm repo add virtrigaud https://projectbeskar.github.io/virtrigaud
helm repo update
helm install virtrigaud virtrigaud/virtrigaud \
  --namespace virtrigaud-system \
  --create-namespace
```

## Using kubectl

```bash
kubectl apply -f https://github.com/projectbeskar/virtrigaud/releases/latest/download/install.yaml
```

## Verify Deployment

Check controller pod status:

```bash
kubectl get pods -n virtrigaud-system
```

Expected output:
```
NAME                                    READY   STATUS    RESTARTS   AGE
virtrigaud-controller-manager-xxx       2/2     Running   0          1m
```

Check logs:

```bash
kubectl logs -n virtrigaud-system deployment/virtrigaud-controller-manager
```

## Configuration Options

Common Helm values:

```yaml
# values.yaml
replicaCount: 2

resources:
  limits:
    cpu: 500m
    memory: 512Mi
  requests:
    cpu: 100m
    memory: 128Mi

logLevel: info
```

Apply with:

```bash
helm install virtrigaud virtrigaud/virtrigaud -f values.yaml
```

## What's Next?

- [Provider Configuration](../guide/providers.md)
- [Creating Your First VM](../guide/creating-vms.md)
