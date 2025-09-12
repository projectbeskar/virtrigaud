# Build stage (musl) - FORCE ALPINE BUILD
FROM golang:1.23-alpine AS builder

# Install libvirt development libraries  
RUN apk add --no-cache \
    build-base pkgconfig libvirt-dev libxml2-dev openssh-client git

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

# Runtime (musl) - FORCE ALPINE RUNTIME  
FROM alpine:3.18
RUN apk add --no-cache libvirt-libs libxml2 ca-certificates openssh-client curl \
 && update-ca-certificates \
 && echo "ALPINE BUILD SUCCESS - SSH CLIENT AVAILABLE" \
 && which ssh \
 && ssh -V \
 && cat /etc/os-release

# Create non-root user
RUN addgroup -g 1000 app \
 && adduser -D -u 1000 -G app -h /home/app -s /bin/sh app
ENV HOME=/home/app
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
RUN mkdir -p /home/app/.ssh && chown -R app:app /home/app && chmod 700 /home/app/.ssh

# Create directories for TLS and credentials
RUN mkdir -p /etc/virtrigaud/tls /etc/virtrigaud/credentials && \
    chown -R app:app /etc/virtrigaud

WORKDIR /

# Copy the binary from builder stage
COPY --from=builder /workspace/provider-libvirt /usr/local/bin/provider-libvirt
RUN ln -s /usr/bin/ssh /usr/local/bin/ssh || true

# Switch to non-root user
USER app

# Expose gRPC and metrics ports
EXPOSE 9443 8080

# Health check (using curl which we installed)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/provider-libvirt"]
