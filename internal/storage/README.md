# Storage Package

The storage package provides an abstraction layer for storing and retrieving VM disk images during migration operations.

## Overview

The storage package implements a pluggable storage backend system that supports multiple storage types:

- **S3-compatible storage** (AWS S3, MinIO, etc.)
- **HTTP/HTTPS storage** (simple file servers)
- **NFS storage** (Network File System for on-premises)

## Interfaces

### Storage Interface

The main `Storage` interface provides methods for:

- `Upload`: Upload a file to storage with progress tracking and checksum calculation
- `Download`: Download a file from storage with resume support and checksum verification
- `Delete`: Remove a file from storage
- `GetMetadata`: Retrieve file metadata
- `ValidateURL`: Verify URL format
- `Close`: Clean up resources

## Supported Backends

### S3 Storage

S3-compatible storage backend using the AWS SDK. Supports:

- AWS S3
- MinIO
- Any S3-compatible object storage

**Features**:
- Multipart uploads for large files
- Concurrent chunk uploading/downloading
- Automatic retry on failures
- SHA256 checksum validation
- Custom metadata support
- SSL/TLS with optional certificate verification

**Configuration**:
```go
config := storage.StorageConfig{
    Type:       "s3",
    Endpoint:   "https://s3.amazonaws.com",
    Bucket:     "vm-migrations",
    Region:     "us-east-1",
    AccessKey:  "AKIAIOSFODNN7EXAMPLE",
    SecretKey:  "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
    UseSSL:     true,
    ChunkSize:  10 * 1024 * 1024, // 10MB chunks
}
```

**URL Formats**:
- `s3://bucket-name/path/to/file`
- `https://s3.amazonaws.com/bucket-name/path/to/file`
- `/path/to/file` (relative to configured bucket)

### HTTP Storage

HTTP/HTTPS storage backend for simple file servers.

**Features**:
- GET for downloads
- PUT for uploads (if server supports)
- DELETE for cleanup (if server supports)
- HEAD for metadata
- Resume support via Range headers
- Bearer token authentication
- SHA256 checksum validation

**Configuration**:
```go
config := storage.StorageConfig{
    Type:     "http",
    Endpoint: "https://fileserver.example.com",
    Token:    "bearer-token-here",
    Timeout:  300,
}
```

**URL Formats**:
- `https://fileserver.example.com/path/to/file`
- `http://fileserver.example.com/path/to/file`

### NFS Storage

NFS (Network File System) storage backend for on-premises deployments with shared NFS mounts.

**Features**:
- Direct filesystem operations (native performance)
- Atomic writes via temporary files and rename
- SHA256 checksum validation
- Progress tracking for large files
- Resume support via file seeking
- Metadata storage in sidecar files
- Automatic directory creation
- Mount verification on initialization
- Efficient buffered I/O with configurable buffer size

**Configuration**:
```go
config := storage.StorageConfig{
    Type:      "nfs",
    Endpoint:  "/mnt/nfs-share",  // Mount point
    ChunkSize: 32 * 1024 * 1024,  // 32MB buffer
}
```

**Prerequisites**:
- NFS share must be pre-mounted on the host/pod
- Mount point must have read/write permissions
- Sufficient disk space on NFS share

**URL Formats**:
- `nfs://relative/path/to/file`
- `/absolute/path/to/file`
- `relative/path/to/file`

**Mount Setup Example**:
```bash
# Mount NFS share
sudo mount -t nfs nfs-server:/export/vm-migrations /mnt/nfs-share

# Verify mount
df -h /mnt/nfs-share
```

**Kubernetes Mount Example**:
```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: nfs-migration-pv
spec:
  capacity:
    storage: 1Ti
  accessModes:
    - ReadWriteMany
  nfs:
    server: nfs-server.example.com
    path: /export/vm-migrations
  mountOptions:
    - hard
    - nfsvers=4.1
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: nfs-migration-pvc
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: 1Ti
```

**Performance Considerations**:
- NFS performance depends on network and NFS server
- Use NFSv4.1 or later for best performance
- Consider `async` mount option for write performance (trade-off: less durability)
- Mount options: `rsize=1048576,wsize=1048576` for large transfers
- Local SSD cache on NFS server improves performance significantly

**Advantages**:
- No additional infrastructure needed (uses existing NFS)
- Native filesystem performance
- Simple setup and management
- Works well for on-premises deployments
- No API rate limits or quotas

**Disadvantages**:
- Requires NFS mount to be pre-configured
- Single point of failure (NFS server)
- Network dependency
- No built-in replication (depends on NFS setup)

## Usage Examples

### Upload a File

```go
package main

import (
    "context"
    "fmt"
    "github.com/projectbeskar/virtrigaud/internal/storage"
)

func main() {
    // Create storage backend
    config := storage.StorageConfig{
        Type:      "s3",
        Endpoint:  "https://s3.amazonaws.com",
        Bucket:    "vm-migrations",
        Region:    "us-east-1",
        AccessKey: "your-access-key",
        SecretKey: "your-secret-key",
    }
    
    store, err := storage.NewStorage(config)
    if err != nil {
        panic(err)
    }
    defer store.Close()
    
    // Upload file
    req := storage.UploadRequest{
        SourcePath:     "/path/to/vm-disk.qcow2",
        DestinationURL: "s3://vm-migrations/disks/vm-disk.qcow2",
        ProgressCallback: func(transferred, total int64) {
            pct := float64(transferred) / float64(total) * 100
            fmt.Printf("Progress: %.2f%%\n", pct)
        },
    }
    
    resp, err := store.Upload(context.Background(), req)
    if err != nil {
        panic(err)
    }
    
    fmt.Printf("Upload complete! Checksum: %s\n", resp.Checksum)
}
```

### Download a File

```go
package main

import (
    "context"
    "fmt"
    "github.com/projectbeskar/virtrigaud/internal/storage"
)

func main() {
    config := storage.StorageConfig{
        Type:      "s3",
        Endpoint:  "https://s3.amazonaws.com",
        Bucket:    "vm-migrations",
        Region:    "us-east-1",
        AccessKey: "your-access-key",
        SecretKey: "your-secret-key",
    }
    
    store, err := storage.NewStorage(config)
    if err != nil {
        panic(err)
    }
    defer store.Close()
    
    // Download file
    req := storage.DownloadRequest{
        SourceURL:        "s3://vm-migrations/disks/vm-disk.qcow2",
        DestinationPath:  "/path/to/downloaded-disk.qcow2",
        VerifyChecksum:   true,
        ExpectedChecksum: "abc123...",
        ProgressCallback: func(transferred, total int64) {
            pct := float64(transferred) / float64(total) * 100
            fmt.Printf("Progress: %.2f%%\n", pct)
        },
    }
    
    resp, err := store.Download(context.Background(), req)
    if err != nil {
        panic(err)
    }
    
    fmt.Printf("Download complete! Checksum: %s\n", resp.Checksum)
}
```

### NFS-Specific Examples

#### Upload to NFS

```go
package main

import (
    "context"
    "fmt"
    "github.com/projectbeskar/virtrigaud/internal/storage"
)

func main() {
    // Create NFS storage backend
    config := storage.StorageConfig{
        Type:      "nfs",
        Endpoint:  "/mnt/nfs-migration-share", // NFS mount point
        ChunkSize: 32 * 1024 * 1024,           // 32MB buffer
    }
    
    store, err := storage.NewStorage(config)
    if err != nil {
        panic(err)
    }
    defer store.Close()
    
    // Upload file to NFS
    req := storage.UploadRequest{
        SourcePath:     "/var/lib/libvirt/images/vm-disk.qcow2",
        DestinationURL: "nfs://vm-exports/libvirt/vm-disk.qcow2",
        Metadata: map[string]string{
            "vm-name": "test-vm-01",
            "source":  "libvirt",
        },
        ProgressCallback: func(transferred, total int64) {
            pct := float64(transferred) / float64(total) * 100
            fmt.Printf("Upload Progress: %.2f%% (%d/%d bytes)\n", pct, transferred, total)
        },
    }
    
    resp, err := store.Upload(context.Background(), req)
    if err != nil {
        panic(err)
    }
    
    fmt.Printf("NFS Upload complete!\n")
    fmt.Printf("  URL: %s\n", resp.URL)
    fmt.Printf("  Checksum: %s\n", resp.Checksum)
    fmt.Printf("  Bytes: %d\n", resp.BytesTransferred)
}
```

#### Download from NFS

```go
package main

import (
    "context"
    "fmt"
    "github.com/projectbeskar/virtrigaud/internal/storage"
)

func main() {
    // Create NFS storage backend
    config := storage.StorageConfig{
        Type:     "nfs",
        Endpoint: "/mnt/nfs-migration-share",
    }
    
    store, err := storage.NewStorage(config)
    if err != nil {
        panic(err)
    }
    defer store.Close()
    
    // Download file from NFS
    req := storage.DownloadRequest{
        SourceURL:        "nfs://vm-exports/libvirt/vm-disk.qcow2",
        DestinationPath:  "/var/lib/proxmox/images/imported-disk.qcow2",
        VerifyChecksum:   true,
        ExpectedChecksum: "sha256-checksum-here",
        ProgressCallback: func(transferred, total int64) {
            pct := float64(transferred) / float64(total) * 100
            rate := float64(transferred) / 1024 / 1024 // MB
            fmt.Printf("Download: %.2f%% (%.2f MB)\n", pct, rate)
        },
    }
    
    resp, err := store.Download(context.Background(), req)
    if err != nil {
        panic(err)
    }
    
    fmt.Printf("NFS Download complete!\n")
    fmt.Printf("  Checksum: %s\n", resp.Checksum)
    fmt.Printf("  Size: %d bytes\n", resp.ContentLength)
}
```

#### Get NFS File Metadata

```go
package main

import (
    "context"
    "fmt"
    "github.com/projectbeskar/virtrigaud/internal/storage"
)

func main() {
    config := storage.StorageConfig{
        Type:     "nfs",
        Endpoint: "/mnt/nfs-migration-share",
    }
    
    store, err := storage.NewStorage(config)
    if err != nil {
        panic(err)
    }
    defer store.Close()
    
    // Get metadata
    metadata, err := store.GetMetadata(
        context.Background(),
        "nfs://vm-exports/libvirt/vm-disk.qcow2",
    )
    if err != nil {
        panic(err)
    }
    
    fmt.Printf("File Metadata:\n")
    fmt.Printf("  Size: %d bytes\n", metadata.Size)
    fmt.Printf("  Checksum: %s\n", metadata.Checksum)
    fmt.Printf("  Modified: %s\n", metadata.LastModified)
    fmt.Printf("  Custom Metadata:\n")
    for k, v := range metadata.CustomMetadata {
        fmt.Printf("    %s: %s\n", k, v)
    }
}
```

## Error Handling

The storage package defines structured error types:

- `ErrorTypeNotFound`: File not found
- `ErrorTypePermissionDenied`: Insufficient permissions
- `ErrorTypeNetworkError`: Network-related error
- `ErrorTypeChecksumMismatch`: Checksum verification failed
- `ErrorTypeInvalidConfig`: Invalid configuration
- `ErrorTypeOperationFailed`: Generic operation failure

Example error handling:

```go
resp, err := store.Upload(ctx, req)
if err != nil {
    if storageErr, ok := err.(*storage.StorageError); ok {
        switch storageErr.Type {
        case storage.ErrorTypeNetworkError:
            // Retry or log network issue
        case storage.ErrorTypeChecksumMismatch:
            // Data corruption detected
        default:
            // Handle other errors
        }
    }
}
```

## Integration with VM Migration

The storage package is used by VM migration controllers to:

1. **Export Phase**: Upload VM disk from source provider to storage
2. **Transfer Phase**: Manage disk transfer between providers
3. **Import Phase**: Download VM disk from storage to target provider
4. **Cleanup**: Remove temporary disk copies after successful migration

## Performance Considerations

### Large Files

- Uses multipart uploads/downloads with configurable chunk sizes
- Default chunk size: 10MB (adjustable based on network conditions)
- Concurrent chunk transfers for better throughput

### Progress Tracking

- Progress callbacks invoked after each chunk transfer
- Useful for updating migration status and ETAs

### Checksums

- SHA256 checksums calculated during upload/download
- Prevents data corruption
- Minimal performance overhead (streaming calculation)

### Retry Logic

- Automatic retry on transient failures (network issues, 5xx errors)
- Configurable max retries (default: 3)
- Exponential backoff between retries

## Security

### Credentials

- Access keys and tokens never logged
- Support for AWS credential chains (environment, IAM roles, etc.)
- Bearer token support for HTTP storage

### SSL/TLS

- SSL/TLS enabled by default
- Optional certificate verification skip (for self-signed certs)
- Configurable per storage backend

### Data Integrity

- SHA256 checksums for all transfers
- Prevents man-in-the-middle attacks
- Detects data corruption

## Future Enhancements

- **Resume Support**: Resume failed uploads/downloads
- **Compression**: Optional compression during transfer
- **Encryption**: Client-side encryption before upload
- **Additional Backends**: NFS, CIFS, local filesystem
- **Bandwidth Limiting**: Rate limiting for network transfers
- **Parallel Transfers**: Multiple concurrent file transfers

