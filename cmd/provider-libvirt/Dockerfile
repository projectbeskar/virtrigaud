# Build stage for libvirt provider (pure Go, no CGO)
ARG BUILDER_IMAGE=golang:1.26.6-bookworm
ARG BASE_IMAGE=debian:bookworm-slim
FROM ${BUILDER_IMAGE} AS builder

# Install git (module resolution). ADR-0008 PR 3: SSH to the hypervisor host is
# now an in-process golang.org/x/crypto/ssh client, not a subprocess, so no
# openssh-client is needed here either — this builder stage never execs ssh.
# No libvirt-dev / pkg-config / gcc: the provider does not cgo-link libvirt.
RUN apt-get update && apt-get install -y \
    git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

# Copy go mod files and local modules (needed for replace directives)
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
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/projectbeskar/virtrigaud/internal/version.Version=${VERSION} -X github.com/projectbeskar/virtrigaud/internal/version.GitSHA=${GIT_SHA}" \
    -a -o provider-libvirt cmd/provider-libvirt/main.go

# Runtime stage.
#
# ADR-0008 PR 3: SSH to the hypervisor host is now an in-process
# golang.org/x/crypto/ssh client (sshclient.go) — openssh-client and sshpass
# are gone from this image. That removes the sshpass -e / SSHPASS
# environment-variable exposure of a hypervisor-root-equivalent password
# (readable from /proc/<pid>/environ) and the on-disk SSH private key +
# generated ~/.ssh/config the old ssh/sshpass subprocess transport needed —
# key material now lives in memory only, which is what unblocks a read-only
# root filesystem for this pod.
#
# libvirt-clients (the virsh binary) STAYS for now: ADR-0008 D4 keeps `virsh
# migrate` on the CLI in v1 (this PR does not touch migration), and the
# genuinely-local (non-ssh://, e.g. qemu:///system) connection fallback in
# virsh.go still execs the local virsh binary directly — a path this PR does
# not exercise in any real deployment (every field Provider targets
# qemu+ssh://) but has not been proven unreachable either. Dropping
# libvirt-clients/libvirt0/libxml2 is flagged as an ADR-0008 follow-up once
# migration moves off the virsh CLI (P5) and the local-connection fallback is
# audited/removed — see the PR description. Until then this is the honest,
# non-guessing call per the ADR.
#
# curl and its HEALTHCHECK are dropped: inert under Kubernetes, which uses
# liveness/readiness probes against the SDK server's own /healthz, not Docker
# HEALTHCHECK.
#
# ADR-0008 PR 3 (#306) dropped the runtime apt-get's openssh-client/sshpass/curl
# but accidentally removed this `FROM ${BASE_IMAGE}` too, so the runtime content
# below (culminating in `COPY --from=builder`) collapsed back into the builder
# stage and buildx failed with "circular dependency detected on stage: builder".
# This line starts the runtime stage — mirroring cmd/provider-proxmox/Dockerfile.
FROM ${BASE_IMAGE}

RUN apt-get update && apt-get install -y \
    libvirt0 \
    libvirt-clients \
    libxml2 \
    ca-certificates \
    && apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/* \
    && echo "DEBIAN BUILD SUCCESS - VIRSH AVAILABLE (in-process SSH, ADR-0008 PR 3)" \
    && which virsh \
    && virsh --version

# Create non-root user with UID 65532 (matches Kubernetes nonroot user)
RUN groupadd -r app -g 65532 && useradd -r -g app -u 65532 -m -d /home/app -s /bin/bash app
ENV HOME=/home/app

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

# No Docker HEALTHCHECK: inert under Kubernetes, which probes the SDK
# server's own /healthz via liveness/readiness probes, not `docker inspect`.

ENTRYPOINT ["/usr/local/bin/provider-libvirt"]
