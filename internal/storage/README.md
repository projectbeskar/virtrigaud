# Storage Package

The storage package stages VM disk images while a `VMMigration` moves them between
providers. It is used by the provider pods (`internal/providers/*`), not by the
manager.

## Backends

[`NewStorage`](storage.go) returns one of two backends, chosen by
`StorageConfig.Type`:

| `Type` | Backend | Source |
|--------|---------|--------|
| `pvc` (or empty) | A file on a PersistentVolumeClaim mounted into the provider pod | [`pvc.go`](pvc.go) |
| `s3` | An S3-compatible object store, through [minio-go](https://github.com/minio/minio-go) | [`s3.go`](s3.go) |

Any other value is refused with an `InvalidConfig` error. The **NFS** staging
backend (`spec.storage.type: nfs` on a `VMMigration`) is not part of this
interface: it moves the disk with qemu-img's own NFS transport, and its address
policy, file layout and options live in [`migration/`](migration/).

How a migration chooses and uses a backend is described in
[ADR-0006](../../docs/adr/0006-storage-backend-agnostic-cross-hypervisor-migration.md)
and [`examples/migration/README.md`](../../examples/migration/README.md).

## The `Storage` interface

| Method | What it does |
|--------|--------------|
| `Upload`, `Download` | Copy a local file to or from the backend, with progress callback and SHA256 checksum |
| `UploadStream`, `DownloadStream` | Stream from an `io.Reader` or to an `io.Writer` without buffering the disk in memory; the SHA256 is computed in the stream and checked against `ExpectedChecksum` on download |
| `Delete` | Remove an object |
| `GetMetadata` | Size, checksum, ETag, last-modified |
| `ValidateURL` | Check that a URL belongs to the backend |
| `Close` | Release the backend's resources |

Request and response types are in [`storage.go`](storage.go).

## PVC backend

The PVC must already be mounted in the provider pod at
`<PVCMountRoot>/<pvc-name>` (the Provider controller does this for the PVC of an
in-flight migration; see `examples/migration/README.md`). URLs are
`pvc://<pvc-name>/<file-path>`.

Every file operation resolves the path **beneath the mount, following no symbolic
link**, and accepts only a regular file with a single hard link. A link, FIFO,
device node or hard-linked file at the staged name is refused with an
`UnsafePath` error whose message names no path. See [`stagedfile/`](stagedfile/).

```go
cfg := storage.StorageConfig{
    Type:         "pvc",
    PVCName:      "migration-storage",
    PVCNamespace: "default",
    MountPath:    storage.PVCMountRoot + "/migration-storage",
}
```

## S3 backend

The provider pod is the S3 client. Bytes flow host or vCenter, through the pod,
to S3 on export, and the other way on import; they never traverse a CSI PVC.
Large objects are uploaded in multiple parts automatically. A failed transfer is
retried whole; it is not resumed.

```go
cfg := storage.StorageConfig{
    Type: "s3",
    S3: &storage.S3Config{
        Endpoint:        "https://s3.example.com", // an http:// scheme selects plaintext; empty is AWS
        Region:          "us-east-1",
        Bucket:          "virtrigaud",
        UsePathStyle:    true, // needed by most non-AWS S3
        AccessKeyID:     accessKey,
        SecretAccessKey: secretKey,
        SessionToken:    "", // optional (STS)
    },
}
```

URLs are `s3://<bucket>/<key>`. The credentials come from the Secret named in
`VMMigration.spec.storage.s3.credentialsSecretRef`; they are secret material and
are never logged.

## Usage

```go
import "github.com/projectbeskar/virtrigaud/internal/storage"

backend, err := storage.NewStorage(cfg)
if err != nil {
    return fmt.Errorf("create storage backend: %w", err)
}
defer func() { _ = backend.Close() }()

resp, err := backend.UploadStream(ctx, storage.StreamUploadRequest{
    DestinationURL: "s3://virtrigaud/ns/migration/disk.qcow2",
    Reader:         diskReader,
    ContentLength:  -1, // unknown size
})
if err != nil {
    return fmt.Errorf("upload disk: %w", err)
}
// resp.Checksum is the SHA256 of what was uploaded.
```

## Errors

Operations return `*storage.StorageError` with a `Type`: `NotFound`,
`PermissionDenied`, `NetworkError`, `ChecksumMismatch`, `InvalidConfig`,
`OperationFailed` or `UnsafePath`. Use `errors.As` to read it. Messages that
reach a `VMMigration`'s status carry no host path and no credential.
