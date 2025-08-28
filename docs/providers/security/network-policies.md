# Network Policies for Provider Security

This guide covers Kubernetes NetworkPolicy configurations to secure communication between VirtRigaud components and provider services.

## Overview

NetworkPolicies provide network-level security by controlling traffic flow between pods, namespaces, and external endpoints. For VirtRigaud providers, this includes:

- **Ingress Control**: Restricting which services can communicate with providers
- **Egress Control**: Limiting provider access to external hypervisor endpoints
- **Namespace Isolation**: Preventing cross-tenant communication
- **External Access**: Controlling access to hypervisor management interfaces

## Basic NetworkPolicy Template

### Provider Ingress Policy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: provider-ingress
  namespace: provider-namespace
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: virtrigaud-provider
  policyTypes:
    - Ingress
  ingress:
    # Allow from VirtRigaud manager
    - from:
        - namespaceSelector:
            matchLabels:
              name: virtrigaud-system
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: virtrigaud-manager
      ports:
        - protocol: TCP
          port: 9443  # gRPC provider port
    
    # Allow health checks from monitoring
    - from:
        - namespaceSelector:
            matchLabels:
              name: monitoring
        - podSelector:
            matchLabels:
              app: prometheus
      ports:
        - protocol: TCP
          port: 8080  # Health/metrics port
    
    # Allow from same namespace (for debugging)
    - from:
        - podSelector: {}
      ports:
        - protocol: TCP
          port: 8080
```

### Provider Egress Policy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: provider-egress
  namespace: provider-namespace
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: virtrigaud-provider
  policyTypes:
    - Egress
  egress:
    # Allow DNS resolution
    - to: []
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    
    # Allow HTTPS to Kubernetes API
    - to:
        - namespaceSelector:
            matchLabels:
              name: kube-system
      ports:
        - protocol: TCP
          port: 443
    
    # Allow access to hypervisor management interfaces
    - to: []
      ports:
        - protocol: TCP
          port: 443  # vCenter HTTPS
        - protocol: TCP
          port: 80   # vCenter HTTP (if needed)
    
    # For libvirt providers - allow access to hypervisor nodes
    - to:
        - podSelector:
            matchLabels:
              node-role.kubernetes.io/worker: "true"
      ports:
        - protocol: TCP
          port: 16509  # libvirt daemon
```

## Environment-Specific Policies

### vSphere Provider

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: vsphere-provider-policy
  namespace: vsphere-providers
  labels:
    provider: vsphere
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: virtrigaud-provider-runtime
      provider: vsphere
  policyTypes:
    - Ingress
    - Egress
  
  ingress:
    # Manager access
    - from:
        - namespaceSelector:
            matchLabels:
              name: virtrigaud-system
      ports:
        - protocol: TCP
          port: 9443
    
    # Monitoring access
    - from:
        - namespaceSelector:
            matchLabels:
              name: monitoring
      ports:
        - protocol: TCP
          port: 8080

  egress:
    # DNS
    - to: []
      ports:
        - protocol: UDP
          port: 53
    
    # vCenter access (specific IP ranges)
    - to:
        - ipBlock:
            cidr: 10.0.0.0/8
            except:
              - 10.244.0.0/16  # Exclude pod network
      ports:
        - protocol: TCP
          port: 443
    
    - to:
        - ipBlock:
            cidr: 192.168.0.0/16
      ports:
        - protocol: TCP
          port: 443
    
    # ESXi host access for direct operations
    - to:
        - ipBlock:
            cidr: 10.1.0.0/24  # ESXi management network
      ports:
        - protocol: TCP
          port: 443
        - protocol: TCP
          port: 902   # vCenter agent
```

### Libvirt Provider

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: libvirt-provider-policy
  namespace: libvirt-providers
  labels:
    provider: libvirt
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: virtrigaud-provider-runtime
      provider: libvirt
  policyTypes:
    - Ingress
    - Egress
  
  ingress:
    # Manager access
    - from:
        - namespaceSelector:
            matchLabels:
              name: virtrigaud-system
      ports:
        - protocol: TCP
          port: 9443
    
    # Monitoring access
    - from:
        - namespaceSelector:
            matchLabels:
              name: monitoring
      ports:
        - protocol: TCP
          port: 8080

  egress:
    # DNS
    - to: []
      ports:
        - protocol: UDP
          port: 53
    
    # Access to hypervisor nodes
    - to: []
      ports:
        - protocol: TCP
          port: 16509  # libvirt daemon
        - protocol: TCP
          port: 22     # SSH for remote libvirt
    
    # Access to shared storage (NFS, iSCSI, etc.)
    - to:
        - ipBlock:
            cidr: 10.2.0.0/24  # Storage network
      ports:
        - protocol: TCP
          port: 2049  # NFS
        - protocol: TCP
          port: 3260  # iSCSI
        - protocol: UDP
          port: 111   # RPC portmapper
```

### Mock Provider (Development)

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: mock-provider-policy
  namespace: development
  labels:
    provider: mock
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: virtrigaud-provider-runtime
      provider: mock
  policyTypes:
    - Ingress
    - Egress
  
  ingress:
    # Allow from manager and other development pods
    - from:
        - namespaceSelector:
            matchLabels:
              environment: development
      ports:
        - protocol: TCP
          port: 9443
        - protocol: TCP
          port: 8080

  egress:
    # Allow all egress for development environment
    - to: []
```

## Multi-Tenant Isolation

### Tenant Namespace Policies

```yaml
# Template for tenant-specific policies
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: tenant-isolation
  namespace: tenant-{{TENANT_NAME}}
  labels:
    tenant: "{{TENANT_NAME}}"
spec:
  podSelector: {}  # Apply to all pods in namespace
  policyTypes:
    - Ingress
    - Egress
  
  ingress:
    # Allow from same tenant namespace
    - from:
        - namespaceSelector:
            matchLabels:
              tenant: "{{TENANT_NAME}}"
    
    # Allow from VirtRigaud system namespace
    - from:
        - namespaceSelector:
            matchLabels:
              name: virtrigaud-system
    
    # Allow from monitoring namespace
    - from:
        - namespaceSelector:
            matchLabels:
              name: monitoring

  egress:
    # Allow to same tenant namespace
    - to:
        - namespaceSelector:
            matchLabels:
              tenant: "{{TENANT_NAME}}"
    
    # Allow to VirtRigaud system namespace
    - to:
        - namespaceSelector:
            matchLabels:
              name: virtrigaud-system
    
    # DNS resolution
    - to: []
      ports:
        - protocol: UDP
          port: 53
    
    # External hypervisor access (tenant-specific IP ranges)
    - to:
        - ipBlock:
            cidr: "{{TENANT_HYPERVISOR_CIDR}}"
      ports:
        - protocol: TCP
          port: 443
```

### Cross-Tenant Communication Prevention

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-cross-tenant
  namespace: tenant-production
spec:
  podSelector: {}
  policyTypes:
    - Ingress
    - Egress
  
  ingress:
    # Explicitly deny from other tenant namespaces
    - from: []
      # Empty from selector with explicit namespace exclusions
  
  egress:
    # Explicitly deny to other tenant namespaces
    - to:
        - namespaceSelector:
            matchLabels:
              name: virtrigaud-system
    - to:
        - namespaceSelector:
            matchLabels:
              name: monitoring
    - to:
        - namespaceSelector:
            matchLabels:
              tenant: production
    # Deny all other namespace access
```

## Advanced Policies

### Time-Based Access Control

```yaml
# Use external controllers like OPA Gatekeeper for time-based policies
apiVersion: templates.gatekeeper.sh/v1beta1
kind: ConstraintTemplate
metadata:
  name: timerestriction
spec:
  crd:
    spec:
      names:
        kind: TimeRestriction
      validation:
        type: object
        properties:
          allowedHours:
            type: array
            items:
              type: integer
            description: "Allowed hours (0-23) for network access"
  targets:
    - target: admission.k8s.gatekeeper.sh
      rego: |
        package timerestriction
        
        violation[{"msg": msg}] {
          current_hour := time.now_ns() / 1000000000 / 3600 % 24
          not current_hour in input.parameters.allowedHours
          msg := sprintf("Network access not allowed at hour %v", [current_hour])
        }

---
apiVersion: constraints.gatekeeper.sh/v1beta1
kind: TimeRestriction
metadata:
  name: business-hours-only
spec:
  match:
    kinds:
      - apiGroups: ["networking.k8s.io"]
        kinds: ["NetworkPolicy"]
    namespaces: ["production"]
  parameters:
    allowedHours: [8, 9, 10, 11, 12, 13, 14, 15, 16, 17]  # 8 AM - 5 PM
```

### Dynamic IP Allow-listing

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: dynamic-hypervisor-access
  namespace: provider-namespace
  annotations:
    # Use external controllers to update IP blocks dynamically
    network-policy-controller/update-interval: "300s"
    network-policy-controller/ip-source: "configmap:hypervisor-ips"
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: virtrigaud-provider
  policyTypes:
    - Egress
  egress:
    # Will be dynamically updated by controller
    - to:
        - ipBlock:
            cidr: 10.0.0.0/8
    # Static rules remain
    - to: []
      ports:
        - protocol: UDP
          port: 53
```

## Monitoring and Troubleshooting

### NetworkPolicy Monitoring

```yaml
# ServiceMonitor for network policy violations
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: networkpolicy-monitoring
  namespace: monitoring
spec:
  selector:
    matchLabels:
      app: networkpolicy-exporter
  endpoints:
    - port: metrics
      interval: 30s
      path: /metrics

---
# Example alerts for network policy violations
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: networkpolicy-alerts
  namespace: monitoring
spec:
  groups:
    - name: networkpolicy.rules
      rules:
        - alert: NetworkPolicyDeniedConnections
          expr: increase(networkpolicy_denied_connections_total[5m]) > 10
          for: 2m
          labels:
            severity: warning
          annotations:
            summary: "High number of denied network connections"
            description: "{{ $labels.source_namespace }}/{{ $labels.source_pod }} had {{ $value }} denied connections to {{ $labels.dest_namespace }}/{{ $labels.dest_pod }}"
```

### Debug NetworkPolicies

```bash
#!/bin/bash
# debug-networkpolicy.sh

NAMESPACE=${1:-default}
POD_NAME=${2}

echo "=== NetworkPolicy Debug for $NAMESPACE/$POD_NAME ==="

# List all NetworkPolicies in namespace
echo "NetworkPolicies in namespace $NAMESPACE:"
kubectl get networkpolicy -n $NAMESPACE

# Show specific NetworkPolicy details
echo -e "\nNetworkPolicy details:"
kubectl get networkpolicy -n $NAMESPACE -o yaml

# Test connectivity
if [ ! -z "$POD_NAME" ]; then
    echo -e "\nTesting connectivity from $POD_NAME:"
    
    # Test DNS resolution
    kubectl exec -n $NAMESPACE $POD_NAME -- nslookup kubernetes.default.svc.cluster.local
    
    # Test internal connectivity
    kubectl exec -n $NAMESPACE $POD_NAME -- wget -qO- --timeout=5 http://kubernetes.default.svc.cluster.local/api
    
    # Test external connectivity (adjust as needed)
    kubectl exec -n $NAMESPACE $POD_NAME -- wget -qO- --timeout=5 https://google.com
fi

# Check iptables rules (if accessible)
echo -e "\nIPTables rules (if accessible):"
kubectl get nodes -o wide
echo "Run the following on a node to see iptables:"
echo "sudo iptables -L -n | grep -E '(KUBE|Chain)'"
```

### CNI-Specific Troubleshooting

#### Calico

```bash
# Check Calico network policies
kubectl get networkpolicy --all-namespaces
kubectl get globalnetworkpolicy

# Check Calico endpoints
kubectl get endpoints --all-namespaces

# Debug Calico connectivity
kubectl exec -it -n kube-system <calico-node-pod> -- /bin/sh
calicoctl get wep --all-namespaces
calicoctl get netpol --all-namespaces
```

#### Cilium

```bash
# Check Cilium network policies
kubectl get cnp --all-namespaces  # Cilium Network Policies
kubectl get ccnp --all-namespaces # Cilium Cluster Network Policies

# Debug Cilium connectivity
kubectl exec -it -n kube-system <cilium-pod> -- cilium endpoint list
kubectl exec -it -n kube-system <cilium-pod> -- cilium policy get
```

## Security Best Practices

### 1. Principle of Least Privilege

```yaml
# Example: Minimal egress for a provider
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: minimal-egress-example
spec:
  podSelector:
    matchLabels:
      app: provider
  policyTypes:
    - Egress
  egress:
    # Only allow what's absolutely necessary
    - to: []
      ports:
        - protocol: UDP
          port: 53  # DNS only
    - to:
        - ipBlock:
            cidr: 10.1.1.100/32  # Specific vCenter IP only
      ports:
        - protocol: TCP
          port: 443  # HTTPS only
```

### 2. Default Deny Policies

```yaml
# Apply default deny to all namespaces
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-all
  namespace: production
spec:
  podSelector: {}
  policyTypes:
    - Ingress
    - Egress
  # Empty ingress/egress rules = deny all
```

### 3. Regular Policy Auditing

```bash
#!/bin/bash
# audit-networkpolicies.sh

echo "=== NetworkPolicy Audit Report ==="
echo "Generated: $(date)"
echo

# Check for namespaces without NetworkPolicies
echo "Namespaces without NetworkPolicies:"
for ns in $(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}'); do
    if [ $(kubectl get networkpolicy -n $ns --no-headers 2>/dev/null | wc -l) -eq 0 ]; then
        echo "  - $ns (WARNING: No network policies)"
    fi
done

echo

# Check for overly permissive policies
echo "Potentially overly permissive policies:"
kubectl get networkpolicy --all-namespaces -o json | jq -r '
  .items[] | 
  select(
    (.spec.egress[]?.to // []) | length == 0 or
    (.spec.ingress[]?.from // []) | length == 0
  ) | 
  "\(.metadata.namespace)/\(.metadata.name) - Check for overly broad rules"
'

echo

# Check for unused NetworkPolicies
echo "NetworkPolicies with no matching pods:"
kubectl get networkpolicy --all-namespaces -o json | jq -r '
  .items[] as $np |
  $np.metadata.namespace as $ns |
  $np.spec.podSelector as $selector |
  if ($selector | keys | length) == 0 then
    "\($ns)/\($np.metadata.name) - Applies to all pods in namespace"
  else
    "\($ns)/\($np.metadata.name) - Check if pods match selector"
  end
'
```

### 4. Integration with Service Mesh

```yaml
# Example: Istio integration with NetworkPolicies
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: istio-compatible-policy
spec:
  podSelector:
    matchLabels:
      app: provider
  policyTypes:
    - Ingress
    - Egress
  ingress:
    # Allow Istio sidecar communication
    - from:
        - podSelector:
            matchLabels:
              app: istio-proxy
      ports:
        - protocol: TCP
          port: 15090  # Istio pilot
    # Your application ports
    - from:
        - namespaceSelector:
            matchLabels:
              name: virtrigaud-system
      ports:
        - protocol: TCP
          port: 9443
  egress:
    # Allow Istio control plane
    - to:
        - namespaceSelector:
            matchLabels:
              name: istio-system
      ports:
        - protocol: TCP
          port: 15010  # Pilot
        - protocol: TCP
          port: 15011  # Pilot secure
```

