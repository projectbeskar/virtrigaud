# Storage Package

The storage package provides an abstraction layer for storing and retrieving VM disk images during migration operations.

## Overview

The storage package implements a pluggable storage backend system that supports multiple storage types:

- **S3-compatible storage** (AWS S3, MinIO, etc.)
- **HTTP/HTTPS storage** (simple file servers)

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

