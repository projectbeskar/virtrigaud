# Build stage for vSphere provider (pure Go, no CGO)
FROM golang:1.23-bookworm AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.mod
COPY go.sum go.sum

# Download dependencies
RUN go mod download

# Copy source code
COPY cmd/provider-vsphere/ cmd/provider-vsphere/
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
