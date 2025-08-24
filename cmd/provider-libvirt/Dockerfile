# Build stage with CGO support for libvirt
FROM golang:1.23-bookworm AS builder

# Install libvirt development libraries
RUN apt-get update && apt-get install -y \
    libvirt-dev \
    pkg-config \
    gcc \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.mod
COPY go.sum go.sum

# Download dependencies
RUN go mod download

# Copy source code
COPY cmd/provider-libvirt/ cmd/provider-libvirt/
COPY api/ api/
COPY internal/ internal/
COPY proto/ proto/

# Ensure protobuf stubs are generated
RUN mkdir -p /go/bin && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@latest && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest && \
    apt-get update && apt-get install -y protobuf-compiler && \
    export PATH=$PATH:/go/bin && \
    protoc -I proto \
        --go_out=internal/rpc --go_opt=paths=source_relative \
        --go-grpc_out=internal/rpc --go-grpc_opt=paths=source_relative \
        proto/provider/v1/provider.proto

# Build the provider binary
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=1 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -a -o provider-libvirt cmd/provider-libvirt/main.go

# Runtime stage
FROM debian:bookworm-slim

# Install runtime dependencies
RUN apt-get update && apt-get install -y \
    libvirt0 \
    libxml2 \
    ca-certificates \
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
