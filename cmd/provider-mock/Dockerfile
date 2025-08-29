# Build stage for mock provider
FROM golang:1.23-bookworm AS builder

WORKDIR /workspace

# Copy go mod files and local modules (needed for replace directives)
COPY go.mod go.mod
COPY go.sum go.sum
COPY sdk/ sdk/
COPY proto/ proto/

# Download dependencies (now that local modules are available)
RUN go mod download

# Copy source code
COPY cmd/provider-mock/ cmd/provider-mock/
COPY internal/ internal/

# Build the provider binary
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_SHA=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/projectbeskar/virtrigaud/internal/version.Version=${VERSION} -X github.com/projectbeskar/virtrigaud/internal/version.GitSHA=${GIT_SHA}" \
    -a -o provider-mock cmd/provider-mock/main.go

# Runtime stage - use distroless for minimal attack surface
FROM gcr.io/distroless/static:nonroot

# Copy the binary from builder stage
COPY --from=builder /workspace/provider-mock /usr/local/bin/provider-mock

USER 65532:65532

# Expose gRPC and health ports
EXPOSE 9443 8080

ENTRYPOINT ["/usr/local/bin/provider-mock"]
