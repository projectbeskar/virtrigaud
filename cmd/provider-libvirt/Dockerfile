# Build stage with CGO support for libvirt
FROM golang:1.23-bookworm AS builder

# Install libvirt development libraries and SSH client
RUN apt-get update && apt-get install -y \
    libvirt-dev \
    pkg-config \
    gcc \
    openssh-client \
    git \
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

# Runtime stage with compatible libvirt and SSH client + SSH agent
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y \
    libvirt0 \
    libvirt-clients \
    libxml2 \
    ca-certificates \
    openssh-client \
    sshpass \
    curl \
    && apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/* \
    && echo "DEBIAN BUILD SUCCESS - SSH CLIENT + VIRSH + SSHPASS AVAILABLE" \
    && which ssh \
    && which virsh \
    && which sshpass \
    && ssh -V \
    && virsh --version \
    && sshpass -V

# Create non-root user with UID 65532 (matches Kubernetes nonroot user)
RUN groupadd -r app -g 65532 && useradd -r -g app -u 65532 -m -d /home/app -s /bin/bash app
ENV HOME=/home/app
RUN mkdir -p /home/app/.ssh && chown -R app:app /home/app && chmod 700 /home/app/.ssh

# Create directories for TLS and credentials
RUN mkdir -p /etc/virtrigaud/tls /etc/virtrigaud/credentials && \
    chown -R app:app /etc/virtrigaud

WORKDIR /

# Copy the binary from builder stage
COPY --from=builder /workspace/provider-libvirt /usr/local/bin/provider-libvirt

# Run as the app user (UID 65532)
USER app

# Expose gRPC and metrics ports
EXPOSE 9443 8080

# Health check (using curl which we installed)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/provider-libvirt"]
