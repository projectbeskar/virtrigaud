# Build the provider binary
FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.23 as builder
ARG BUILDPLATFORM
ARG TARGETPLATFORM
ARG VERSION="dev"
ARG GIT_SHA="unknown"

WORKDIR /workspace
# Copy the Go Modules manifests and local modules (needed for replace directives)
COPY go.mod go.mod
COPY go.sum go.sum
COPY sdk/ sdk/
COPY proto/ proto/
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/provider-proxmox/main.go cmd/provider-proxmox/main.go
COPY internal/ internal/
COPY api/ api/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
ARG CGO_ENABLED=0
ARG GOOS=linux 
ARG GOARCH
RUN CGO_ENABLED=${CGO_ENABLED} GOOS=${GOOS} GOARCH=${GOARCH} go build \
    -ldflags="-X 'github.com/projectbeskar/virtrigaud/internal/version.Version=${VERSION}' -X 'github.com/projectbeskar/virtrigaud/internal/version.GitSHA=${GIT_SHA}'" \
    -a -o provider-proxmox cmd/provider-proxmox/main.go

# Use distroless as minimal base image to package the provider binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/provider-proxmox .
USER 65532:65532

ENTRYPOINT ["/provider-proxmox"]