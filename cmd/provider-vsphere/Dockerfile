# Build stage for vSphere provider (pure Go, no CGO)
FROM golang:1.25-bookworm AS builder

WORKDIR /workspace

# Copy go mod files and local modules (needed for replace directives)
COPY go.mod go.mod
COPY go.sum go.sum
COPY proto/ proto/
COPY sdk/ sdk/

# Download dependencies (now that local modules are available)
RUN go mod download

# Copy source code
COPY cmd/provider-vsphere/ cmd/provider-vsphere/
COPY api/ api/
COPY internal/ internal/

# Protobuf files are already generated in proto/rpc/
# No need to regenerate them during Docker build

# Build the provider binary (no CGO needed for vSphere)
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_SHA=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/projectbeskar/virtrigaud/internal/version.Version=${VERSION} -X github.com/projectbeskar/virtrigaud/internal/version.GitSHA=${GIT_SHA}" \
    -a -o provider-vsphere cmd/provider-vsphere/main.go

# Runtime stage - use Debian slim for qemu-img support
FROM debian:bookworm-slim

# Install qemu-img and CA certificates
RUN apt-get update && apt-get install -y \
    qemu-utils \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && echo "DEBIAN BUILD SUCCESS - QEMU-IMG AVAILABLE" \
    && which qemu-img \
    && qemu-img --version

# Create non-root user
RUN groupadd -r app -g 65532 && \
    useradd -r -g app -u 65532 -m -d /home/app -s /bin/bash app

# Copy the binary from builder stage
COPY --from=builder /workspace/provider-vsphere /usr/local/bin/provider-vsphere

# Switch to non-root user
USER 65532:65532

# Expose gRPC and metrics ports
EXPOSE 9443 8080

ENTRYPOINT ["/usr/local/bin/provider-vsphere"]
