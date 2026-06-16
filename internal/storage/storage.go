/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storage

import (
	"context"
	"io"
)

// Storage defines the interface for storing and retrieving VM disk images during migration
type Storage interface {
	// Upload uploads a file to storage
	// Returns the URL/path where the file was stored
	Upload(ctx context.Context, req UploadRequest) (UploadResponse, error)

	// Download downloads a file from storage
	Download(ctx context.Context, req DownloadRequest) (DownloadResponse, error)

	// UploadStream streams data from an io.Reader into the backend, computing the
	// SHA256 in-stream (ADR-0006 relay export). For S3 this is an auto-multipart
	// PutObject; the PVC backend streams to a file. The reader is consumed in
	// full; the backend does not buffer the whole object in memory.
	UploadStream(ctx context.Context, req StreamUploadRequest) (UploadResponse, error)

	// DownloadStream streams an object from the backend into an io.Writer,
	// computing the SHA256 in-stream and verifying it against ExpectedChecksum
	// when set (ADR-0006 relay import). For S3 this is a streaming GetObject; the
	// PVC backend streams from a file. The writer receives the whole object; the
	// backend does not buffer it in memory.
	DownloadStream(ctx context.Context, req StreamDownloadRequest) (DownloadResponse, error)

	// Delete removes a file from storage
	Delete(ctx context.Context, url string) error

	// GetMetadata retrieves metadata about a stored file
	GetMetadata(ctx context.Context, url string) (FileMetadata, error)

	// ValidateURL checks if a URL is valid for this storage backend
	ValidateURL(url string) error

	// Close closes any open connections
	Close() error
}

// StreamUploadRequest contains parameters for a streaming upload (ADR-0006).
type StreamUploadRequest struct {
	// DestinationURL is the backend URL to upload to (e.g. "s3://bucket/key").
	DestinationURL string
	// Reader provides the data to upload. It is read to EOF.
	Reader io.Reader
	// ContentLength is the expected size in bytes, or -1 when unknown. For S3 a
	// known size lets minio pick an efficient part size; -1 triggers streaming
	// auto-multipart.
	ContentLength int64
}

// StreamDownloadRequest contains parameters for a streaming download (ADR-0006).
type StreamDownloadRequest struct {
	// SourceURL is the backend URL to download from (e.g. "s3://bucket/key").
	SourceURL string
	// Writer receives the downloaded bytes. It is written in full before return.
	Writer io.Writer
	// ExpectedChecksum, when non-empty, is the SHA256 the downloaded object must
	// match; a mismatch returns an ErrorTypeChecksumMismatch error.
	ExpectedChecksum string
}

// UploadRequest contains parameters for uploading a file
type UploadRequest struct {
	// SourcePath is the local file path to upload
	SourcePath string
	// DestinationURL is where to upload the file
	DestinationURL string
	// Reader provides the data to upload (alternative to SourcePath)
	Reader io.Reader
	// ContentLength is the expected size in bytes
	ContentLength int64
	// Checksum is the expected checksum (SHA256)
	Checksum string
	// ProgressCallback is called periodically with upload progress
	ProgressCallback func(bytesTransferred int64, totalBytes int64)
	// ChunkSize for multipart uploads (0 = use default)
	ChunkSize int64
	// Metadata contains custom metadata
	Metadata map[string]string
}

// UploadResponse contains the result of an upload operation
type UploadResponse struct {
	// URL is the final storage URL
	URL string
	// Checksum is the calculated checksum (SHA256)
	Checksum string
	// BytesTransferred is the total bytes uploaded
	BytesTransferred int64
	// ETag from S3 or other storage (if available)
	ETag string
}

// DownloadRequest contains parameters for downloading a file
type DownloadRequest struct {
	// SourceURL is the URL to download from
	SourceURL string
	// DestinationPath is where to save the file locally
	DestinationPath string
	// Writer to write downloaded data (alternative to DestinationPath)
	Writer io.Writer
	// VerifyChecksum enables checksum verification
	VerifyChecksum bool
	// ExpectedChecksum is the expected checksum (SHA256)
	ExpectedChecksum string
	// ProgressCallback is called periodically with download progress
	ProgressCallback func(bytesTransferred int64, totalBytes int64)
	// ResumeOffset allows resuming from a specific byte offset
	ResumeOffset int64
}

// DownloadResponse contains the result of a download operation
type DownloadResponse struct {
	// BytesTransferred is the total bytes downloaded
	BytesTransferred int64
	// Checksum is the calculated checksum (SHA256)
	Checksum string
	// ContentLength is the total file size
	ContentLength int64
}

// FileMetadata contains metadata about a stored file
type FileMetadata struct {
	// URL is the storage URL
	URL string
	// Size is the file size in bytes
	Size int64
	// Checksum is the SHA256 checksum
	Checksum string
	// ETag from S3 or other storage (if available)
	ETag string
	// LastModified timestamp
	LastModified string
	// ContentType MIME type
	ContentType string
	// CustomMetadata contains custom key-value pairs
	CustomMetadata map[string]string
}

// StorageConfig contains storage backend configuration
type StorageConfig struct {
	// Type specifies the storage backend type ("pvc" or "s3"; "" == pvc).
	Type string
	// PVCName is the name of the PVC to use (pvc backend).
	PVCName string
	// PVCNamespace is the namespace of the PVC (pvc backend).
	PVCNamespace string
	// MountPath is where the PVC is mounted in the pod (pvc backend).
	MountPath string

	// S3 holds the S3-compatible object-storage configuration (s3 backend,
	// ADR-0006). Endpoint/bucket/region/path-style come from the migration's
	// MigrationStorage.S3; credentials are delivered separately and never live in
	// config or logs.
	S3 *S3Config
}

// S3Config configures an S3-compatible object-storage backend (ADR-0006). The
// provider pod is the S3 client; bytes flow host/vCenter → pod → S3 (export) and
// S3 → pod → host (import), never via a CSI PVC.
type S3Config struct {
	// Endpoint is the S3 endpoint URL. An "http://" scheme selects plaintext HTTP
	// (e.g. a lab rustfs/MinIO); empty means the AWS default endpoint.
	Endpoint string
	// Region is the S3 region (empty defaults to "us-east-1").
	Region string
	// Bucket is the S3 bucket holding the transfer object.
	Bucket string
	// UsePathStyle selects path-style addressing (needed by most non-AWS S3).
	UsePathStyle bool

	// AccessKeyID is the S3 access key ID. Secret material — never logged.
	AccessKeyID string
	// SecretAccessKey is the S3 secret access key. Secret material — never logged.
	SecretAccessKey string
	// SessionToken is the optional S3 session token (STS). Secret material.
	SessionToken string
}

// NewStorage creates a new storage backend based on the configuration.
func NewStorage(config StorageConfig) (Storage, error) {
	switch config.Type {
	case "pvc", "":
		return NewPVCStorage(config)
	case "s3":
		return NewS3Storage(config)
	default:
		return nil, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "unsupported storage type (supported: 'pvc', 's3'): " + config.Type,
		}
	}
}

// StorageError represents a storage operation error
type StorageError struct {
	Type    ErrorType
	Message string
	Cause   error
}

func (e *StorageError) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

func (e *StorageError) Unwrap() error {
	return e.Cause
}

// ErrorType categorizes storage errors
type ErrorType string

const (
	// ErrorTypeNotFound indicates the file was not found
	ErrorTypeNotFound ErrorType = "NotFound"
	// ErrorTypePermissionDenied indicates insufficient permissions
	ErrorTypePermissionDenied ErrorType = "PermissionDenied"
	// ErrorTypeNetworkError indicates a network-related error
	ErrorTypeNetworkError ErrorType = "NetworkError"
	// ErrorTypeChecksumMismatch indicates checksum verification failed
	ErrorTypeChecksumMismatch ErrorType = "ChecksumMismatch"
	// ErrorTypeInvalidConfig indicates invalid configuration
	ErrorTypeInvalidConfig ErrorType = "InvalidConfig"
	// ErrorTypeOperationFailed indicates a generic operation failure
	ErrorTypeOperationFailed ErrorType = "OperationFailed"
)
