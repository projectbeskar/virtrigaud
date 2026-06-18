# Build the provider binary
ARG BUILDER_IMAGE=golang:1.26.4
# The Proxmox provider shells out to `ssh`/`sshpass` for the disk data plane
# (qemu-img/qm/pvesm run ON the PVE node over SSH), so the runtime image needs an
# ssh client + sshpass — a static-distroless base cannot carry them.
ARG BASE_IMAGE=debian:bookworm-slim
FROM --platform=${BUILDPLATFORM:-linux/amd64} ${BUILDER_IMAGE} as builder
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

ARG GOPROXY=""
ENV GOPROXY=${GOPROXY}
ARG GOINSECURE=""
ENV GOINSECURE=${GOINSECURE}
ARG GOPRIVATE=""
ENV GOPRIVATE=${GOPRIVATE}
ARG GOSUMDB="sum.golang.org"
ENV GOSUMDB=${GOSUMDB}

COPY *.crt /etc/ssl/certs/
RUN update-ca-certificates

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

# Runtime image: needs an ssh client + sshpass for the SSH disk data plane.
FROM ${BASE_IMAGE}
RUN apt-get update && apt-get install -y \
    ca-certificates \
    openssh-client \
    sshpass \
    curl \
    && apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/* \
    && which ssh \
    && which sshpass \
    && ssh -V \
    && sshpass -V

# Non-root user with UID 65532 (matches the Kubernetes nonroot user).
RUN groupadd -r app -g 65532 && useradd -r -g app -u 65532 -m -d /home/app -s /bin/bash app
ENV HOME=/home/app
RUN mkdir -p /home/app/.ssh && chown -R app:app /home/app && chmod 700 /home/app/.ssh

# Directories for TLS and mounted credentials.
RUN mkdir -p /etc/virtrigaud/tls /etc/virtrigaud/credentials && \
    chown -R app:app /etc/virtrigaud

WORKDIR /
COPY --from=builder /workspace/provider-proxmox /usr/local/bin/provider-proxmox
USER app

EXPOSE 9443 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/provider-proxmox"]