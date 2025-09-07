# Build stage with CGO support for libvirt
FROM golang:1.23-bookworm AS builder

# Install libvirt development libraries
RUN apt-get update && apt-get install -y \
    libvirt-dev \
    pkg-config \
    gcc \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

# Copy go mod files and local modules (needed for replace directives)
COPY go.mod go.mod
COPY go.sum go.sum
COPY sdk/ sdk/
COPY proto/ proto/

# Download dependencies (now that local modules are available)
RUN go mod download

# Copy source code
COPY cmd/provider-libvirt/ cmd/provider-libvirt/
COPY api/ api/
COPY internal/ internal/
COPY proto/ proto/

# Protobuf files are already generated in proto/rpc/
# No need to regenerate them during Docker build

# Build the provider binary
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_SHA=unknown
RUN CGO_ENABLED=1 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/projectbeskar/virtrigaud/internal/version.Version=${VERSION} -X github.com/projectbeskar/virtrigaud/internal/version.GitSHA=${GIT_SHA}" \
    -a -o provider-libvirt cmd/provider-libvirt/main.go

# Runtime stage
FROM debian:bookworm-slim

# Install runtime dependencies with latest security updates
RUN apt-get update && apt-get install -y \
    libvirt0 \
    libxml2 \
    ca-certificates \
    curl \
    && apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN groupadd -r virtrigaud && useradd -r -g virtrigaud virtrigaud

# Create directories for TLS and credentials
RUN mkdir -p /etc/virtrigaud/tls /etc/virtrigaud/credentials && \
    chown -R virtrigaud:virtrigaud /etc/virtrigaud

WORKDIR /

# Copy the binary from builder stage
COPY --from=builder /workspace/provider-libvirt /usr/local/bin/provider-libvirt

# Switch to non-root user
USER virtrigaud

# Expose gRPC and metrics ports
EXPOSE 9443 8080

# Health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/provider-libvirt"]
