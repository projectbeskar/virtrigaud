# Build stage for vSphere provider (pure Go, no CGO)
FROM golang:1.23-bookworm AS builder

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

# Runtime stage - use distroless for minimal attack surface
FROM gcr.io/distroless/static:nonroot

# Copy the binary from builder stage
COPY --from=builder /workspace/provider-vsphere /usr/local/bin/provider-vsphere

# Create directories for TLS and credentials (distroless doesn't have mkdir)
USER 65532:65532

# Expose gRPC and metrics ports
EXPOSE 9443 8080

ENTRYPOINT ["/usr/local/bin/provider-vsphere"]
