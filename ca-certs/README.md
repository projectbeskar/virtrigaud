# Corporate CA certificates for the manager build

This directory is consumed by `build/Dockerfile.manager` during the **builder stage** to install corporate / private CA certificates into the Go module fetch path. Upstream releases ship this directory empty (only `.gitkeep` + this `README.md`); the `COPY ca-certs/ /usr/local/share/ca-certificates/` step in the Dockerfile is a no-op in that case because `update-ca-certificates` skips non-`.crt` files.

## When you need this

You need to populate this directory if **either** of these is true for your build environment:

1. Your `go mod download` traffic transits a corporate **TLS-intercepting proxy** (Zscaler, Netskope, Symantec, BlueCoat, …) that re-signs HTTPS with an internal CA, and you have not pre-baked that CA into your builder image.
2. Your `GOPROXY` setting points at an **internal module mirror** (Artifactory, JFrog, Sonatype Nexus, GitLab Module Registry, …) that serves over HTTPS with a private CA.

If neither is true, leave this directory as-is — the upstream public CA bundle is sufficient.

## How to use

1. Drop one or more `.crt` files (PEM-encoded X.509, one cert per file is best) into this directory:
   ```
   ca-certs/corporate-root-ca.crt
   ca-certs/internal-tls-intercept.crt
   ```
2. Rebuild the manager image:
   ```bash
   make docker-build VERSION=v0.3.6 GIT_SHA=$(git rev-parse HEAD)
   # or directly:
   docker build -f build/Dockerfile.manager \
       --build-arg VERSION=v0.3.6 \
       --build-arg GIT_SHA=$(git rev-parse HEAD) \
       -t my-registry/virtrigaud-manager:v0.3.6 .
   ```
3. `update-ca-certificates` runs as part of the build stage and rebuilds `/etc/ssl/certs/ca-certificates.crt` so all subsequent `go mod download` calls inside the same build trust the certs you provided.

## Scope: builder only, NOT runtime

These certs are installed into the **builder stage** only. The final image (`gcr.io/distroless/static:nonroot` by default) is a minimal distroless static image and is NOT rebuilt to include these custom CAs.

If your **runtime** environment also needs custom CAs (for example, the manager talks to remote provider pods over HTTPS through a TLS-intercepting proxy), do **not** add them here — instead mount them into the deployed pod via a Kubernetes Secret or ConfigMap, or use a custom `BASE_IMAGE` build-arg that points at an image with your CAs already trusted.

## Security notes

- **One `.crt` per file, PEM-encoded.** Bundles (`.pem` files containing multiple certs) work too, but the file extension must be `.crt` for `update-ca-certificates` to pick it up.
- **Do not commit organisation-internal CAs to a public fork.** Use `.gitignore` in your private fork to keep them local, or inject them via your CI's secrets / artifact storage at build time.
- **No private keys, ever.** Only certificates (public, `BEGIN CERTIFICATE`). Anything starting `BEGIN PRIVATE KEY` or `BEGIN RSA PRIVATE KEY` does not belong in this directory.

## Related

- ADR-0002 (build-path consolidation) — `fieldTesting/ADR-0002-build-path-consolidation.md`
- H1 PR-2 — issue #116, the change that introduced this directory
- H1 umbrella — issue #92
