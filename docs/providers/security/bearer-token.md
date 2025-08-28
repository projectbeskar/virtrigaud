# Bearer Token Authentication

This guide covers how to configure bearer token authentication for VirtRigaud providers using JWT tokens and RBAC.

## Overview

Bearer token authentication provides a stateless, scalable authentication mechanism using JSON Web Tokens (JWT). This approach is suitable for:

- **Multi-tenant environments**: Different tokens for different tenants
- **API-based access**: External systems accessing provider services
- **Short-lived sessions**: Tokens with configurable expiration
- **Fine-grained permissions**: Token-based RBAC

## JWT Token Structure

### Token Claims

```json
{
  "iss": "virtrigaud-manager",
  "sub": "provider-client",
  "aud": "virtrigaud-provider",
  "exp": 1640995200,
  "iat": 1640908800,
  "nbf": 1640908800,
  "scope": "vm:create vm:read vm:update vm:delete",
  "tenant": "default",
  "provider": "vsphere",
  "jti": "unique-token-id"
}
```

### Scopes Definition

| Scope | Description |
|-------|-------------|
| `vm:create` | Create virtual machines |
| `vm:read` | Read virtual machine information |
| `vm:update` | Update virtual machine configuration |
| `vm:delete` | Delete virtual machines |
| `vm:power` | Control virtual machine power state |
| `vm:snapshot` | Create and manage snapshots |
| `vm:clone` | Clone virtual machines |
| `admin` | Full administrative access |

## Token Generation

### JWT Signing Key

```bash
# Generate RS256 private key
openssl genrsa -out jwt-private-key.pem 2048

# Extract public key
openssl rsa -in jwt-private-key.pem -pubout -out jwt-public-key.pem

# Store as Kubernetes secret
kubectl create secret generic jwt-keys \
  --from-file=private-key=jwt-private-key.pem \
  --from-file=public-key=jwt-public-key.pem \
  --namespace=virtrigaud-system
```

### Token Generation Service

```go
package auth

import (
    "crypto/rsa"
    "time"
    
    "github.com/golang-jwt/jwt/v4"
)

type TokenClaims struct {
    Issuer    string   `json:"iss"`
    Subject   string   `json:"sub"`
    Audience  string   `json:"aud"`
    ExpiresAt int64    `json:"exp"`
    IssuedAt  int64    `json:"iat"`
    NotBefore int64    `json:"nbf"`
    Scope     string   `json:"scope"`
    Tenant    string   `json:"tenant"`
    Provider  string   `json:"provider"`
    ID        string   `json:"jti"`
    jwt.RegisteredClaims
}

type TokenService struct {
    privateKey *rsa.PrivateKey
    publicKey  *rsa.PublicKey
    issuer     string
}

func NewTokenService(privateKey *rsa.PrivateKey, publicKey *rsa.PublicKey, issuer string) *TokenService {
    return &TokenService{
        privateKey: privateKey,
        publicKey:  publicKey,
        issuer:     issuer,
    }
}

func (ts *TokenService) GenerateToken(subject, tenant, provider string, scopes []string, duration time.Duration) (string, error) {
    now := time.Now()
    claims := &TokenClaims{
        Issuer:    ts.issuer,
        Subject:   subject,
        Audience:  "virtrigaud-provider",
        ExpiresAt: now.Add(duration).Unix(),
        IssuedAt:  now.Unix(),
        NotBefore: now.Unix(),
        Scope:     strings.Join(scopes, " "),
        Tenant:    tenant,
        Provider:  provider,
        ID:        generateJTI(),
    }
    
    token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
    return token.SignedString(ts.privateKey)
}

func (ts *TokenService) ValidateToken(tokenString string) (*TokenClaims, error) {
    token, err := jwt.ParseWithClaims(tokenString, &TokenClaims{}, func(token *jwt.Token) (interface{}, error) {
        if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
            return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
        }
        return ts.publicKey, nil
    })
    
    if err != nil {
        return nil, err
    }
    
    if claims, ok := token.Claims.(*TokenClaims); ok && token.Valid {
        return claims, nil
    }
    
    return nil, fmt.Errorf("invalid token")
}

func generateJTI() string {
    return uuid.New().String()
}
```

## Provider Authentication Interceptor

### gRPC Interceptor

```go
package middleware

import (
    "context"
    "strings"
    
    "google.golang.org/grpc"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/metadata"
    "google.golang.org/grpc/status"
)

type AuthInterceptor struct {
    tokenService *auth.TokenService
    rbac         *RBACManager
}

func NewAuthInterceptor(tokenService *auth.TokenService, rbac *RBACManager) *AuthInterceptor {
    return &AuthInterceptor{
        tokenService: tokenService,
        rbac:         rbac,
    }
}

func (ai *AuthInterceptor) Unary() grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
        // Skip authentication for health checks
        if strings.HasSuffix(info.FullMethod, "/Health/Check") {
            return handler(ctx, req)
        }
        
        token, err := ai.extractToken(ctx)
        if err != nil {
            return nil, status.Errorf(codes.Unauthenticated, "missing or invalid token: %v", err)
        }
        
        claims, err := ai.tokenService.ValidateToken(token)
        if err != nil {
            return nil, status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
        }
        
        // Check authorization
        if !ai.rbac.IsAuthorized(claims, info.FullMethod) {
            return nil, status.Errorf(codes.PermissionDenied, "insufficient permissions")
        }
        
        // Add claims to context
        ctx = context.WithValue(ctx, "claims", claims)
        
        return handler(ctx, req)
    }
}

func (ai *AuthInterceptor) extractToken(ctx context.Context) (string, error) {
    md, ok := metadata.FromIncomingContext(ctx)
    if !ok {
        return "", fmt.Errorf("missing metadata")
    }
    
    authHeaders := md.Get("authorization")
    if len(authHeaders) == 0 {
        return "", fmt.Errorf("missing authorization header")
    }
    
    authHeader := authHeaders[0]
    if !strings.HasPrefix(authHeader, "Bearer ") {
        return "", fmt.Errorf("invalid authorization header format")
    }
    
    return strings.TrimPrefix(authHeader, "Bearer "), nil
}
```

### RBAC Manager

```go
package middleware

import (
    "strings"
)

type Permission struct {
    Resource string
    Action   string
}

type RBACManager struct {
    permissions map[string][]Permission
}

func NewRBACManager() *RBACManager {
    return &RBACManager{
        permissions: map[string][]Permission{
            // RPC method to required permissions mapping
            "/provider.v1.ProviderService/CreateVM": {
                {Resource: "vm", Action: "create"},
            },
            "/provider.v1.ProviderService/GetVM": {
                {Resource: "vm", Action: "read"},
            },
            "/provider.v1.ProviderService/UpdateVM": {
                {Resource: "vm", Action: "update"},
            },
            "/provider.v1.ProviderService/DeleteVM": {
                {Resource: "vm", Action: "delete"},
            },
            "/provider.v1.ProviderService/PowerVM": {
                {Resource: "vm", Action: "power"},
            },
            "/provider.v1.ProviderService/CreateSnapshot": {
                {Resource: "vm", Action: "snapshot"},
            },
            "/provider.v1.ProviderService/CloneVM": {
                {Resource: "vm", Action: "clone"},
            },
        },
    }
}

func (rbac *RBACManager) IsAuthorized(claims *auth.TokenClaims, method string) bool {
    requiredPerms, exists := rbac.permissions[method]
    if !exists {
        // Allow if no specific permissions required
        return true
    }
    
    userScopes := strings.Split(claims.Scope, " ")
    
    // Check if user has admin scope
    for _, scope := range userScopes {
        if scope == "admin" {
            return true
        }
    }
    
    // Check specific permissions
    for _, requiredPerm := range requiredPerms {
        requiredScope := requiredPerm.Resource + ":" + requiredPerm.Action
        
        hasPermission := false
        for _, userScope := range userScopes {
            if userScope == requiredScope {
                hasPermission = true
                break
            }
        }
        
        if !hasPermission {
            return false
        }
    }
    
    return true
}
```

## Kubernetes RBAC Integration

### ServiceAccount and ClusterRole

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: virtrigaud-token-manager
  namespace: virtrigaud-system

---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: virtrigaud-token-manager
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list", "watch"]

---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: virtrigaud-token-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: virtrigaud-token-manager
subjects:
  - kind: ServiceAccount
    name: virtrigaud-token-manager
    namespace: virtrigaud-system
```

### Token Management ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: token-config
  namespace: virtrigaud-system
data:
  config.yaml: |
    tokenService:
      issuer: "virtrigaud-manager"
      defaultDuration: "1h"
      maxDuration: "24h"
      
    scopes:
      - name: "vm:create"
        description: "Create virtual machines"
      - name: "vm:read"
        description: "Read virtual machine information"
      - name: "vm:update"
        description: "Update virtual machine configuration"
      - name: "vm:delete"
        description: "Delete virtual machines"
      - name: "vm:power"
        description: "Control virtual machine power state"
      - name: "vm:snapshot"
        description: "Create and manage snapshots"
      - name: "vm:clone"
        description: "Clone virtual machines"
      - name: "admin"
        description: "Full administrative access"
        
    tenants:
      - name: "default"
        description: "Default tenant"
        allowedScopes: ["vm:create", "vm:read", "vm:update", "vm:delete", "vm:power"]
      - name: "development"
        description: "Development environment"
        allowedScopes: ["vm:create", "vm:read", "vm:update", "vm:delete", "vm:power", "vm:snapshot", "vm:clone"]
      - name: "production"
        description: "Production environment"
        allowedScopes: ["vm:read", "vm:power"]
```

## Client Configuration

### Manager Client Setup

```go
package client

import (
    "context"
    "time"
    
    "google.golang.org/grpc"
    "google.golang.org/grpc/metadata"
)

type AuthenticatedClient struct {
    client providerv1.ProviderServiceClient
    token  string
}

func NewAuthenticatedClient(endpoint, token string) (*AuthenticatedClient, error) {
    conn, err := grpc.Dial(endpoint, grpc.WithInsecure())
    if err != nil {
        return nil, err
    }
    
    return &AuthenticatedClient{
        client: providerv1.NewProviderServiceClient(conn),
        token:  token,
    }, nil
}

func (ac *AuthenticatedClient) CreateVM(ctx context.Context, req *providerv1.CreateVMRequest) (*providerv1.CreateVMResponse, error) {
    ctx = ac.addAuthHeader(ctx)
    return ac.client.CreateVM(ctx, req)
}

func (ac *AuthenticatedClient) addAuthHeader(ctx context.Context) context.Context {
    md := metadata.Pairs("authorization", "Bearer "+ac.token)
    return metadata.NewOutgoingContext(ctx, md)
}
```

### Token Refresh

```go
package auth

import (
    "sync"
    "time"
)

type TokenManager struct {
    tokenService *TokenService
    currentToken string
    expiresAt    time.Time
    mutex        sync.RWMutex
    
    subject  string
    tenant   string
    provider string
    scopes   []string
}

func NewTokenManager(tokenService *TokenService, subject, tenant, provider string, scopes []string) *TokenManager {
    return &TokenManager{
        tokenService: tokenService,
        subject:      subject,
        tenant:       tenant,
        provider:     provider,
        scopes:       scopes,
    }
}

func (tm *TokenManager) GetToken() (string, error) {
    tm.mutex.RLock()
    if tm.currentToken != "" && time.Now().Before(tm.expiresAt.Add(-5*time.Minute)) {
        token := tm.currentToken
        tm.mutex.RUnlock()
        return token, nil
    }
    tm.mutex.RUnlock()
    
    return tm.refreshToken()
}

func (tm *TokenManager) refreshToken() (string, error) {
    tm.mutex.Lock()
    defer tm.mutex.Unlock()
    
    // Double-check after acquiring write lock
    if tm.currentToken != "" && time.Now().Before(tm.expiresAt.Add(-5*time.Minute)) {
        return tm.currentToken, nil
    }
    
    token, err := tm.tokenService.GenerateToken(tm.subject, tm.tenant, tm.provider, tm.scopes, time.Hour)
    if err != nil {
        return "", err
    }
    
    tm.currentToken = token
    tm.expiresAt = time.Now().Add(time.Hour)
    
    return token, nil
}
```

## Helm Chart Integration

### Provider Runtime with Bearer Token Auth

```yaml
# values-bearer-auth.yaml
auth:
  type: "bearer"
  jwt:
    publicKeySecret: "jwt-keys"
    publicKeyKey: "public-key"
    issuer: "virtrigaud-manager"
    audience: "virtrigaud-provider"

# Environment variables for authentication
env:
  - name: AUTH_TYPE
    value: "bearer"
  - name: JWT_PUBLIC_KEY_PATH
    value: "/etc/jwt/public-key"
  - name: JWT_ISSUER
    value: "virtrigaud-manager"
  - name: JWT_AUDIENCE
    value: "virtrigaud-provider"

# Mount JWT public key
volumes:
  - name: jwt-public-key
    secret:
      secretName: jwt-keys

volumeMounts:
  - name: jwt-public-key
    mountPath: /etc/jwt
    readOnly: true
```

## Monitoring and Logging

### Authentication Metrics

```go
package metrics

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    authenticationAttempts = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "virtrigaud_authentication_attempts_total",
            Help: "Total number of authentication attempts",
        },
        []string{"method", "result", "tenant"},
    )
    
    authenticationDuration = promauto.NewHistogramVec(
        prometheus.HistogramOpts{
            Name: "virtrigaud_authentication_duration_seconds",
            Help: "Duration of authentication operations",
        },
        []string{"method", "result"},
    )
    
    activeTokens = promauto.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "virtrigaud_active_tokens",
            Help: "Number of active tokens by tenant",
        },
        []string{"tenant", "provider"},
    )
)

func RecordAuthAttempt(method, result, tenant string) {
    authenticationAttempts.WithLabelValues(method, result, tenant).Inc()
}

func RecordAuthDuration(method, result string, duration time.Duration) {
    authenticationDuration.WithLabelValues(method, result).Observe(duration.Seconds())
}
```

### Audit Logging

```go
package audit

import (
    "context"
    "encoding/json"
    "time"
    
    "go.uber.org/zap"
)

type AuditEvent struct {
    Timestamp time.Time `json:"timestamp"`
    EventType string    `json:"event_type"`
    Subject   string    `json:"subject"`
    Tenant    string    `json:"tenant"`
    Provider  string    `json:"provider"`
    Resource  string    `json:"resource"`
    Action    string    `json:"action"`
    Result    string    `json:"result"`
    Error     string    `json:"error,omitempty"`
    Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

type AuditLogger struct {
    logger *zap.Logger
}

func NewAuditLogger(logger *zap.Logger) *AuditLogger {
    return &AuditLogger{logger: logger}
}

func (al *AuditLogger) LogAuthEvent(ctx context.Context, eventType, subject, tenant, provider, result string, err error) {
    event := AuditEvent{
        Timestamp: time.Now(),
        EventType: eventType,
        Subject:   subject,
        Tenant:    tenant,
        Provider:  provider,
        Result:    result,
    }
    
    if err != nil {
        event.Error = err.Error()
    }
    
    eventJSON, _ := json.Marshal(event)
    al.logger.Info("audit_event", zap.String("event", string(eventJSON)))
}
```

## Security Best Practices

### 1. Token Validation

```go
// Always validate all token claims
func validateTokenClaims(claims *TokenClaims) error {
    now := time.Now()
    
    // Check expiration
    if claims.ExpiresAt < now.Unix() {
        return fmt.Errorf("token expired")
    }
    
    // Check not before
    if claims.NotBefore > now.Unix() {
        return fmt.Errorf("token not yet valid")
    }
    
    // Check issuer
    if claims.Issuer != expectedIssuer {
        return fmt.Errorf("invalid issuer")
    }
    
    // Check audience
    if claims.Audience != expectedAudience {
        return fmt.Errorf("invalid audience")
    }
    
    return nil
}
```

### 2. Rate Limiting

```go
// Implement rate limiting for token generation
type RateLimiter struct {
    requests map[string][]time.Time
    mutex    sync.RWMutex
    limit    int
    window   time.Duration
}

func (rl *RateLimiter) Allow(key string) bool {
    rl.mutex.Lock()
    defer rl.mutex.Unlock()
    
    now := time.Now()
    requests := rl.requests[key]
    
    // Remove old requests outside the window
    var validRequests []time.Time
    for _, req := range requests {
        if now.Sub(req) < rl.window {
            validRequests = append(validRequests, req)
        }
    }
    
    // Check if we've exceeded the limit
    if len(validRequests) >= rl.limit {
        return false
    }
    
    // Add the current request
    validRequests = append(validRequests, now)
    rl.requests[key] = validRequests
    
    return true
}
```

### 3. Token Blacklisting

```go
// Implement token blacklisting for revoked tokens
type TokenBlacklist struct {
    blacklistedTokens map[string]time.Time
    mutex             sync.RWMutex
}

func (tb *TokenBlacklist) IsBlacklisted(jti string) bool {
    tb.mutex.RLock()
    defer tb.mutex.RUnlock()
    
    expiresAt, exists := tb.blacklistedTokens[jti]
    if !exists {
        return false
    }
    
    // Remove expired entries
    if time.Now().After(expiresAt) {
        delete(tb.blacklistedTokens, jti)
        return false
    }
    
    return true
}

func (tb *TokenBlacklist) BlacklistToken(jti string, expiresAt time.Time) {
    tb.mutex.Lock()
    defer tb.mutex.Unlock()
    tb.blacklistedTokens[jti] = expiresAt
}
```

