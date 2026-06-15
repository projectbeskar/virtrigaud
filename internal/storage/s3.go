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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Storage implements the Storage interface against an S3-compatible object
// store using minio-go (ADR-0006). The provider pod is the S3 client: bytes flow
// host/vCenter → pod → S3 on export and S3 → pod → host on import; nothing ever
// traverses a CSI PVC. Streaming methods compute the SHA256 in-stream so the
// disk is never buffered in memory.
//
// Multipart: minio's PutObject auto-multiparts large objects, satisfying the
// ADR-0006 "multipart from day one" requirement. Full crash-resume (persisting
// UploadId/parts in Status) is OUT of scope for Slice 1 — a failed transfer is
// retried whole — and is tracked as a follow-up.
type S3Storage struct {
	client *minio.Client
	bucket string
	secure bool
}

// NewS3Storage creates an S3 storage backend from config. Credentials come from
// config.S3 (which the controller populated from the credentials Secret) and are
// never logged. An "http://" endpoint selects plaintext HTTP; path-style
// addressing is selected by config.S3.UsePathStyle (required by most non-AWS S3
// such as rustfs/MinIO/Ceph RGW).
func NewS3Storage(config StorageConfig) (*S3Storage, error) {
	if config.S3 == nil {
		return nil, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "s3 storage configuration is required for s3 backend",
		}
	}
	s3cfg := config.S3
	if s3cfg.Bucket == "" {
		return nil, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "s3 bucket is required",
		}
	}
	if s3cfg.AccessKeyID == "" || s3cfg.SecretAccessKey == "" {
		return nil, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "s3 credentials are required (accessKeyID/secretAccessKey)",
		}
	}

	endpoint, secure, err := parseS3Endpoint(s3cfg.Endpoint)
	if err != nil {
		return nil, &StorageError{Type: ErrorTypeInvalidConfig, Message: err.Error()}
	}

	region := s3cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	lookup := minio.BucketLookupDNS
	if s3cfg.UsePathStyle {
		lookup = minio.BucketLookupPath
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(s3cfg.AccessKeyID, s3cfg.SecretAccessKey, s3cfg.SessionToken),
		Secure:       secure,
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "failed to create S3 client",
			Cause:   err,
		}
	}

	return &S3Storage{client: client, bucket: s3cfg.Bucket, secure: secure}, nil
}

// parseS3Endpoint splits an endpoint URL into the host:port minio expects and a
// secure flag. An empty endpoint means AWS's default ("s3.amazonaws.com",
// secure). An explicit "http://" scheme selects plaintext; "https://" or a bare
// host:port selects TLS.
func parseS3Endpoint(endpoint string) (host string, secure bool, err error) {
	if endpoint == "" {
		return "s3.amazonaws.com", true, nil
	}
	if !strings.Contains(endpoint, "://") {
		// Bare host[:port] — default to TLS.
		return endpoint, true, nil
	}
	u, perr := url.Parse(endpoint)
	if perr != nil {
		return "", false, fmt.Errorf("invalid s3 endpoint %q: %w", endpoint, perr)
	}
	switch u.Scheme {
	case "http":
		return u.Host, false, nil
	case "https":
		return u.Host, true, nil
	default:
		return "", false, fmt.Errorf("unsupported s3 endpoint scheme %q (use http or https)", u.Scheme)
	}
}

// parseS3Key extracts the object key from an "s3://bucket/key" URL or a bare
// "bucket/key"/"key" form, returning the key relative to s.bucket. The bucket in
// the URL must match the configured bucket when present.
func (s *S3Storage) parseS3Key(rawURL string) (string, error) {
	raw := strings.TrimSpace(rawURL)
	raw = strings.TrimPrefix(raw, "s3://")
	if raw == "" {
		return "", &StorageError{Type: ErrorTypeInvalidConfig, Message: "empty s3 URL"}
	}
	// If the first path segment names the bucket, strip it.
	parts := strings.SplitN(raw, "/", 2)
	if parts[0] == s.bucket {
		if len(parts) < 2 || parts[1] == "" {
			return "", &StorageError{Type: ErrorTypeInvalidConfig, Message: "s3 URL has no object key: " + rawURL}
		}
		return parts[1], nil
	}
	// Otherwise treat the whole remainder as the key under the configured bucket.
	return raw, nil
}

// UploadStream streams the reader into S3 via auto-multipart PutObject, computing
// SHA256 in-stream with an io.TeeReader. A negative or zero ContentLength selects
// streaming mode (size unknown).
func (s *S3Storage) UploadStream(ctx context.Context, req StreamUploadRequest) (UploadResponse, error) {
	key, err := s.parseS3Key(req.DestinationURL)
	if err != nil {
		return UploadResponse{}, err
	}
	if req.Reader == nil {
		return UploadResponse{}, &StorageError{Type: ErrorTypeInvalidConfig, Message: "Reader is required for UploadStream"}
	}

	hasher := sha256.New()
	tee := io.TeeReader(req.Reader, hasher)

	size := req.ContentLength
	if size <= 0 {
		size = -1 // minio streaming auto-multipart
	}

	info, err := s.client.PutObject(ctx, s.bucket, key, tee, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("S3 upload failed: s3://%s/%s", s.bucket, key),
			Cause:   err,
		}
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	return UploadResponse{
		URL:              fmt.Sprintf("s3://%s/%s", s.bucket, key),
		Checksum:         checksum,
		BytesTransferred: info.Size,
		ETag:             info.ETag,
	}, nil
}

// DownloadStream streams an S3 object into the writer, computing SHA256 in-stream
// and verifying it against ExpectedChecksum when set.
func (s *S3Storage) DownloadStream(ctx context.Context, req StreamDownloadRequest) (DownloadResponse, error) {
	key, err := s.parseS3Key(req.SourceURL)
	if err != nil {
		return DownloadResponse{}, err
	}
	if req.Writer == nil {
		return DownloadResponse{}, &StorageError{Type: ErrorTypeInvalidConfig, Message: "Writer is required for DownloadStream"}
	}

	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("S3 GetObject failed: s3://%s/%s", s.bucket, key),
			Cause:   err,
		}
	}
	defer func() { _ = obj.Close() }()

	hasher := sha256.New()
	mw := io.MultiWriter(req.Writer, hasher)

	n, err := io.Copy(mw, obj)
	if err != nil {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeNetworkError,
			Message: fmt.Sprintf("S3 download failed: s3://%s/%s", s.bucket, key),
			Cause:   err,
		}
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	if req.ExpectedChecksum != "" && req.ExpectedChecksum != checksum {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeChecksumMismatch,
			Message: fmt.Sprintf("S3 object checksum mismatch: expected=%s actual=%s", req.ExpectedChecksum, checksum),
		}
	}

	return DownloadResponse{
		BytesTransferred: n,
		Checksum:         checksum,
		ContentLength:    n,
	}, nil
}

// Upload adapts a file/reader UploadRequest onto the streaming S3 path.
func (s *S3Storage) Upload(ctx context.Context, req UploadRequest) (UploadResponse, error) {
	if req.Reader == nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "S3 Upload requires a Reader (use UploadStream); file-path upload is not supported on the s3 backend",
		}
	}
	resp, err := s.UploadStream(ctx, StreamUploadRequest{
		DestinationURL: req.DestinationURL,
		Reader:         req.Reader,
		ContentLength:  req.ContentLength,
	})
	if err != nil {
		return UploadResponse{}, err
	}
	if req.Checksum != "" && req.Checksum != resp.Checksum {
		_ = s.Delete(ctx, resp.URL)
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeChecksumMismatch,
			Message: fmt.Sprintf("checksum mismatch: expected=%s actual=%s", req.Checksum, resp.Checksum),
		}
	}
	return resp, nil
}

// Download adapts a writer DownloadRequest onto the streaming S3 path.
func (s *S3Storage) Download(ctx context.Context, req DownloadRequest) (DownloadResponse, error) {
	if req.Writer == nil {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "S3 Download requires a Writer (use DownloadStream); file-path download is not supported on the s3 backend",
		}
	}
	expected := ""
	if req.VerifyChecksum {
		expected = req.ExpectedChecksum
	}
	return s.DownloadStream(ctx, StreamDownloadRequest{
		SourceURL:        req.SourceURL,
		Writer:           req.Writer,
		ExpectedChecksum: expected,
	})
}

// Delete removes an object from the bucket.
func (s *S3Storage) Delete(ctx context.Context, rawURL string) error {
	key, err := s.parseS3Key(rawURL)
	if err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("S3 delete failed: s3://%s/%s", s.bucket, key),
			Cause:   err,
		}
	}
	return nil
}

// GetMetadata returns object metadata via StatObject (no body transfer).
func (s *S3Storage) GetMetadata(ctx context.Context, rawURL string) (FileMetadata, error) {
	key, err := s.parseS3Key(rawURL)
	if err != nil {
		return FileMetadata{}, err
	}
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return FileMetadata{}, &StorageError{
			Type:    ErrorTypeNotFound,
			Message: fmt.Sprintf("S3 stat failed: s3://%s/%s", s.bucket, key),
			Cause:   err,
		}
	}
	return FileMetadata{
		URL:          fmt.Sprintf("s3://%s/%s", s.bucket, key),
		Size:         info.Size,
		ETag:         info.ETag,
		LastModified: info.LastModified.String(),
		ContentType:  info.ContentType,
	}, nil
}

// ValidateURL checks that an S3 URL resolves to a non-empty key.
func (s *S3Storage) ValidateURL(rawURL string) error {
	_, err := s.parseS3Key(rawURL)
	return err
}

// Close releases the backend. The minio client holds no long-lived connections
// that require explicit teardown.
func (s *S3Storage) Close() error { return nil }
