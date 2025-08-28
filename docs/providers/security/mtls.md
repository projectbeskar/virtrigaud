# mTLS Security Configuration

This guide covers how to configure mutual TLS (mTLS) authentication between VirtRigaud managers and providers.

## Overview

mTLS provides strong authentication and encryption for gRPC communication between the VirtRigaud manager and provider services. It ensures:

- **Authentication**: Both client and server verify each other's certificates
- **Encryption**: All traffic is encrypted in transit
- **Certificate Pinning**: Specific certificate authorities are trusted
- **Certificate Rotation**: Automated certificate renewal

## Certificate Management

### 1. Generate CA Certificate

```bash
# Create CA private key
openssl genrsa -out ca-key.pem 4096

# Create CA certificate
openssl req -new -x509 -key ca-key.pem -out ca-cert.pem -days 365 \
  -subj "/C=US/ST=CA/L=San Francisco/O=VirtRigaud/CN=VirtRigaud CA"
```

### 2. Generate Server Certificate (Provider)

```bash
# Create server private key
openssl genrsa -out server-key.pem 4096

# Create server certificate signing request
openssl req -new -key server-key.pem -out server-csr.pem \
  -subj "/C=US/ST=CA/L=San Francisco/O=VirtRigaud/CN=provider-service"

# Sign server certificate
openssl x509 -req -in server-csr.pem -CA ca-cert.pem -CAkey ca-key.pem \
  -CAcreateserial -out server-cert.pem -days 365 \
  -extensions v3_req -extfile <(cat <<EOF
[v3_req]
keyUsage = keyEncipherment, dataEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names
[alt_names]
DNS.1 = provider-service
DNS.2 = provider-service.default.svc.cluster.local
DNS.3 = localhost
IP.1 = 127.0.0.1
EOF
)
```

### 3. Generate Client Certificate (Manager)

```bash
# Create client private key
openssl genrsa -out client-key.pem 4096

# Create client certificate signing request
openssl req -new -key client-key.pem -out client-csr.pem \
  -subj "/C=US/ST=CA/L=San Francisco/O=VirtRigaud/CN=manager-client"

# Sign client certificate
openssl x509 -req -in client-csr.pem -CA ca-cert.pem -CAkey ca-key.pem \
  -CAcreateserial -out client-cert.pem -days 365 \
  -extensions v3_req -extfile <(cat <<EOF
[v3_req]
keyUsage = keyEncipherment, dataEncipherment
extendedKeyUsage = clientAuth
EOF
)
```

## Kubernetes Secret Configuration

### Provider TLS Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: provider-tls
  namespace: default
type: kubernetes.io/tls
data:
  tls.crt: # base64 encoded server-cert.pem
  tls.key: # base64 encoded server-key.pem
  ca.crt: # base64 encoded ca-cert.pem
```

### Manager TLS Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: manager-tls
  namespace: virtrigaud-system
type: kubernetes.io/tls
data:
  tls.crt: # base64 encoded client-cert.pem
  tls.key: # base64 encoded client-key.pem
  ca.crt: # base64 encoded ca-cert.pem
```

## Provider Configuration

### SDK Server Configuration

```go
package main

import (
    "crypto/tls"
    "crypto/x509"
    "fmt"
    "io/ioutil"
    
    "github.com/projectbeskar/virtrigaud/sdk/provider/server"
)

func main() {
    // Load certificates
    cert, err := tls.LoadX509KeyPair("/etc/tls/tls.crt", "/etc/tls/tls.key")
    if err != nil {
        panic(fmt.Sprintf("Failed to load server certificates: %v", err))
    }
    
    // Load CA certificate for client verification
    caCert, err := ioutil.ReadFile("/etc/tls/ca.crt")
    if err != nil {
        panic(fmt.Sprintf("Failed to load CA certificate: %v", err))
    }
    
    caCertPool := x509.NewCertPool()
    if !caCertPool.AppendCertsFromPEM(caCert) {
        panic("Failed to parse CA certificate")
    }
    
    // Configure TLS
    tlsConfig := &tls.Config{
        Certificates: []tls.Certificate{cert},
        ClientAuth:   tls.RequireAndVerifyClientCert,
        ClientCAs:    caCertPool,
        MinVersion:   tls.VersionTLS12,
        CipherSuites: []uint16{
            tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
            tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
            tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
            tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
        },
    }
    
    // Create server with mTLS
    srv, err := server.New(&server.Config{
        Port:      9443,
        TLS:       tlsConfig,
        EnableTLS: true,
    })
    if err != nil {
        panic(fmt.Sprintf("Failed to create server: %v", err))
    }
    
    // Register your provider implementation here
    // providerv1.RegisterProviderServiceServer(srv.GRPCServer(), &YourProvider{})
    
    if err := srv.Serve(); err != nil {
        panic(fmt.Sprintf("Server failed: %v", err))
    }
}
```

### Helm Chart Values (Provider Runtime)

```yaml
# values-mtls.yaml
tls:
  enabled: true
  secretName: provider-tls

# Mount TLS certificates
volumes:
  - name: tls-certs
    secret:
      secretName: provider-tls

volumeMounts:
  - name: tls-certs
    mountPath: /etc/tls
    readOnly: true

# Environment variables for TLS
env:
  - name: TLS_ENABLED
    value: "true"
  - name: TLS_CERT_PATH
    value: "/etc/tls/tls.crt"
  - name: TLS_KEY_PATH
    value: "/etc/tls/tls.key"
  - name: TLS_CA_PATH
    value: "/etc/tls/ca.crt"
```

## Manager Configuration

### Client TLS Configuration

```go
// In manager code
func createProviderClient(endpoint string) (providerv1.ProviderServiceClient, error) {
    // Load client certificates
    cert, err := tls.LoadX509KeyPair("/etc/manager-tls/tls.crt", "/etc/manager-tls/tls.key")
    if err != nil {
        return nil, fmt.Errorf("failed to load client certificates: %w", err)
    }
    
    // Load CA certificate for server verification
    caCert, err := ioutil.ReadFile("/etc/manager-tls/ca.crt")
    if err != nil {
        return nil, fmt.Errorf("failed to load CA certificate: %w", err)
    }
    
    caCertPool := x509.NewCertPool()
    if !caCertPool.AppendCertsFromPEM(caCert) {
        return nil, fmt.Errorf("failed to parse CA certificate")
    }
    
    // Configure TLS
    tlsConfig := &tls.Config{
        Certificates: []tls.Certificate{cert},
        RootCAs:      caCertPool,
        ServerName:   "provider-service", // Must match server certificate CN/SAN
        MinVersion:   tls.VersionTLS12,
    }
    
    // Create gRPC connection with mTLS
    conn, err := grpc.Dial(endpoint,
        grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
    )
    if err != nil {
        return nil, fmt.Errorf("failed to connect: %w", err)
    }
    
    return providerv1.NewProviderServiceClient(conn), nil
}
```

## Certificate Rotation

### Using cert-manager

```yaml
# Install cert-manager first
# kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v1.12.0/cert-manager.yaml

apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: virtrigaud-ca-issuer
spec:
  ca:
    secretName: virtrigaud-ca-secret

---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: provider-tls
  namespace: default
spec:
  secretName: provider-tls
  issuerRef:
    name: virtrigaud-ca-issuer
    kind: ClusterIssuer
  commonName: provider-service
  dnsNames:
    - provider-service
    - provider-service.default.svc.cluster.local
  duration: 8760h # 1 year
  renewBefore: 720h # 30 days before expiry
```

### Manual Rotation Script

```bash
#!/bin/bash
# rotate-certs.sh

NAMESPACE=${1:-default}
SECRET_NAME=${2:-provider-tls}

echo "Rotating certificates for $SECRET_NAME in namespace $NAMESPACE"

# Generate new certificates (using the same process as above)
# ...

# Update Kubernetes secret
kubectl create secret tls $SECRET_NAME \
  --cert=server-cert.pem \
  --key=server-key.pem \
  --namespace=$NAMESPACE \
  --dry-run=client -o yaml | kubectl apply -f -

# Add CA certificate to the secret
kubectl patch secret $SECRET_NAME -n $NAMESPACE \
  --patch="$(cat <<EOF
data:
  ca.crt: $(base64 -w 0 ca-cert.pem)
EOF
)"

# Restart provider deployment to pick up new certificates
kubectl rollout restart deployment/provider-deployment -n $NAMESPACE

echo "Certificate rotation completed"
```

## Security Best Practices

### 1. Certificate Validation

```go
// Always validate certificate chains
func validateCertificate(cert *x509.Certificate, caCert *x509.Certificate) error {
    roots := x509.NewCertPool()
    roots.AddCert(caCert)
    
    opts := x509.VerifyOptions{
        Roots: roots,
        KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
    }
    
    _, err := cert.Verify(opts)
    return err
}
```

### 2. Certificate Pinning

```go
// Pin specific certificate or CA
func createTLSConfigWithPinning(expectedCertFingerprint string) *tls.Config {
    return &tls.Config{
        VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
            if len(rawCerts) == 0 {
                return fmt.Errorf("no certificates provided")
            }
            
            cert, err := x509.ParseCertificate(rawCerts[0])
            if err != nil {
                return err
            }
            
            fingerprint := sha256.Sum256(cert.Raw)
            if hex.EncodeToString(fingerprint[:]) != expectedCertFingerprint {
                return fmt.Errorf("certificate fingerprint mismatch")
            }
            
            return nil
        },
    }
}
```

### 3. Monitoring and Alerting

```yaml
# Prometheus AlertManager rules
groups:
  - name: virtrigaud.certificates
    rules:
      - alert: CertificateExpiringSoon
        expr: (cert_manager_certificate_expiration_timestamp_seconds - time()) / 86400 < 30
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "Certificate expiring soon"
          description: "Certificate {{ $labels.name }} expires in less than 30 days"
      
      - alert: CertificateExpired
        expr: cert_manager_certificate_expiration_timestamp_seconds < time()
        for: 0m
        labels:
          severity: critical
        annotations:
          summary: "Certificate expired"
          description: "Certificate {{ $labels.name }} has expired"
```

## Troubleshooting

### Common Issues

1. **Certificate chain issues**
   ```bash
   # Verify certificate chain
   openssl verify -CAfile ca-cert.pem server-cert.pem
   ```

2. **SAN mismatch**
   ```bash
   # Check certificate SAN entries
   openssl x509 -in server-cert.pem -text -noout | grep -A1 "Subject Alternative Name"
   ```

3. **TLS handshake failures**
   ```bash
   # Test TLS connection
   openssl s_client -connect provider-service:9443 -cert client-cert.pem -key client-key.pem -CAfile ca-cert.pem
   ```

4. **Clock skew issues**
   ```bash
   # Ensure time synchronization
   ntpdate -s time.nist.gov
   ```

### Debug Commands

```bash
# Check certificate validity
kubectl get secret provider-tls -o yaml | grep tls.crt | base64 -d | openssl x509 -text -noout

# Monitor certificate expiration
kubectl get certificates

# Check provider logs for TLS errors
kubectl logs deployment/provider-deployment | grep -i tls
```

