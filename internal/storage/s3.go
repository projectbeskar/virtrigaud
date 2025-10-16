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
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

// S3Storage implements the Storage interface for S3-compatible storage
type S3Storage struct {
	client     *s3.S3
	uploader   *s3manager.Uploader
	downloader *s3manager.Downloader
	config     StorageConfig
	session    *session.Session
}

// NewS3Storage creates a new S3 storage backend
func NewS3Storage(config StorageConfig) (*S3Storage, error) {
	log.Printf("INFO Initializing S3 storage: endpoint=%s, bucket=%s, region=%s",
		config.Endpoint, config.Bucket, config.Region)

	// Set defaults
	if config.Region == "" {
		config.Region = "us-east-1"
	}
	if config.Timeout == 0 {
		config.Timeout = 300 // 5 minutes default
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.ChunkSize == 0 {
		config.ChunkSize = 10 * 1024 * 1024 // 10MB default chunk size
	}

	// Create AWS config
	awsConfig := &aws.Config{
		Region:           aws.String(config.Region),
		S3ForcePathStyle: aws.Bool(true), // Required for MinIO and custom S3 endpoints
		MaxRetries:       aws.Int(config.MaxRetries),
		HTTPClient: &http.Client{
			Timeout: time.Duration(config.Timeout) * time.Second,
		},
	}

	// Add endpoint if specified (for MinIO or custom S3)
	if config.Endpoint != "" {
		awsConfig.Endpoint = aws.String(config.Endpoint)
	}

	// Configure SSL
	if !config.UseSSL {
		awsConfig.DisableSSL = aws.Bool(true)
	}

	if config.InsecureSkipVerify {
		awsConfig.HTTPClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
	}

	// Add credentials
	if config.AccessKey != "" && config.SecretKey != "" {
		awsConfig.Credentials = credentials.NewStaticCredentials(
			config.AccessKey,
			config.SecretKey,
			config.Token,
		)
	}

	// Create session
	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "failed to create AWS session",
			Cause:   err,
		}
	}

	// Create S3 client
	client := s3.New(sess)

	// Create uploader with custom part size
	uploader := s3manager.NewUploader(sess, func(u *s3manager.Uploader) {
		u.PartSize = config.ChunkSize
		u.Concurrency = 3 // Upload 3 parts concurrently
	})

	// Create downloader
	downloader := s3manager.NewDownloader(sess, func(d *s3manager.Downloader) {
		d.PartSize = config.ChunkSize
		d.Concurrency = 5 // Download 5 parts concurrently
	})

	storage := &S3Storage{
		client:     client,
		uploader:   uploader,
		downloader: downloader,
		config:     config,
		session:    sess,
	}

	// Validate connection by checking if bucket exists
	_, err = client.HeadBucket(&s3.HeadBucketInput{
		Bucket: aws.String(config.Bucket),
	})
	if err != nil {
		log.Printf("WARN S3 bucket validation failed: %v (bucket may not exist or permissions issue)", err)
	}

	log.Printf("INFO S3 storage initialized successfully")
	return storage, nil
}

// Upload uploads a file to S3
func (s *S3Storage) Upload(ctx context.Context, req UploadRequest) (UploadResponse, error) {
	log.Printf("INFO Uploading to S3: %s", req.DestinationURL)

	// Parse S3 URL to get key
	key, err := s.parseS3Key(req.DestinationURL)
	if err != nil {
		return UploadResponse{}, err
	}

	var reader io.Reader
	var fileSize int64

	// Use provided reader or open file
	if req.Reader != nil {
		reader = req.Reader
		fileSize = req.ContentLength
	} else if req.SourcePath != "" {
		file, err := os.Open(req.SourcePath)
		if err != nil {
			return UploadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to open source file",
				Cause:   err,
			}
		}
		defer file.Close()

		fileInfo, err := file.Stat()
		if err != nil {
			return UploadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to get file info",
				Cause:   err,
			}
		}
		fileSize = fileInfo.Size()
		reader = file
	} else {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "either SourcePath or Reader must be provided",
		}
	}

	// Wrap reader with progress tracking and checksum calculation
	var checksum string
	progressReader := &progressReader{
		reader:   reader,
		total:    fileSize,
		callback: req.ProgressCallback,
		hasher:   sha256.New(),
	}

	// Prepare metadata
	metadata := make(map[string]*string)
	for k, v := range req.Metadata {
		metadata[k] = aws.String(v)
	}
	if req.Checksum != "" {
		metadata["sha256"] = aws.String(req.Checksum)
	}

	// Upload to S3
	result, err := s.uploader.UploadWithContext(ctx, &s3manager.UploadInput{
		Bucket:   aws.String(s.config.Bucket),
		Key:      aws.String(key),
		Body:     progressReader,
		Metadata: metadata,
	})
	if err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to upload to S3",
			Cause:   err,
		}
	}

	// Get checksum from hasher
	checksum = hex.EncodeToString(progressReader.hasher.Sum(nil))

	// Verify checksum if provided
	if req.Checksum != "" && req.Checksum != checksum {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeChecksumMismatch,
			Message: fmt.Sprintf("checksum mismatch: expected %s, got %s", req.Checksum, checksum),
		}
	}

	log.Printf("INFO Upload complete: %s (checksum=%s, bytes=%d)", key, checksum, progressReader.transferred)

	return UploadResponse{
		URL:              req.DestinationURL,
		Checksum:         checksum,
		BytesTransferred: progressReader.transferred,
		ETag:             aws.StringValue(result.ETag),
	}, nil
}

// Download downloads a file from S3
func (s *S3Storage) Download(ctx context.Context, req DownloadRequest) (DownloadResponse, error) {
	log.Printf("INFO Downloading from S3: %s", req.SourceURL)

	// Parse S3 URL to get key
	key, err := s.parseS3Key(req.SourceURL)
	if err != nil {
		return DownloadResponse{}, err
	}

	// Get object metadata first
	headResult, err := s.client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeNotFound,
			Message: "failed to get object metadata",
			Cause:   err,
		}
	}

	contentLength := aws.Int64Value(headResult.ContentLength)

	var writer io.WriterAt
	var file *os.File

	// Use provided writer or create file
	if req.Writer != nil {
		// For io.Writer, we need to wrap it
		writer = &writerAtWrapper{writer: req.Writer}
	} else if req.DestinationPath != "" {
		// Create directory if needed
		dir := filepath.Dir(req.DestinationPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to create destination directory",
				Cause:   err,
			}
		}

		file, err = os.Create(req.DestinationPath)
		if err != nil {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to create destination file",
				Cause:   err,
			}
		}
		defer file.Close()
		writer = file
	} else {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "either DestinationPath or Writer must be provided",
		}
	}

	// Wrap writer with progress tracking and checksum calculation
	progressWriter := &progressWriter{
		writer:   writer,
		total:    contentLength,
		callback: req.ProgressCallback,
		hasher:   sha256.New(),
	}

	// Download from S3
	bytesDownloaded, err := s.downloader.DownloadWithContext(ctx, progressWriter, &s3.GetObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to download from S3",
			Cause:   err,
		}
	}

	// Get checksum from hasher
	checksum := hex.EncodeToString(progressWriter.hasher.Sum(nil))

	// Verify checksum if requested
	if req.VerifyChecksum && req.ExpectedChecksum != "" {
		if req.ExpectedChecksum != checksum {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeChecksumMismatch,
				Message: fmt.Sprintf("checksum mismatch: expected %s, got %s", req.ExpectedChecksum, checksum),
			}
		}
	}

	log.Printf("INFO Download complete: %s (checksum=%s, bytes=%d)", key, checksum, bytesDownloaded)

	return DownloadResponse{
		BytesTransferred: bytesDownloaded,
		Checksum:         checksum,
		ContentLength:    contentLength,
	}, nil
}

// Delete deletes a file from S3
func (s *S3Storage) Delete(ctx context.Context, url string) error {
	log.Printf("INFO Deleting from S3: %s", url)

	key, err := s.parseS3Key(url)
	if err != nil {
		return err
	}

	_, err = s.client.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to delete object from S3",
			Cause:   err,
		}
	}

	log.Printf("INFO Deleted from S3: %s", key)
	return nil
}

// GetMetadata retrieves metadata about a stored file
func (s *S3Storage) GetMetadata(ctx context.Context, url string) (FileMetadata, error) {
	key, err := s.parseS3Key(url)
	if err != nil {
		return FileMetadata{}, err
	}

	result, err := s.client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return FileMetadata{}, &StorageError{
			Type:    ErrorTypeNotFound,
			Message: "failed to get object metadata",
			Cause:   err,
		}
	}

	metadata := FileMetadata{
		URL:            url,
		Size:           aws.Int64Value(result.ContentLength),
		ETag:           aws.StringValue(result.ETag),
		ContentType:    aws.StringValue(result.ContentType),
		LastModified:   result.LastModified.Format(time.RFC3339),
		CustomMetadata: make(map[string]string),
	}

	// Extract custom metadata
	for k, v := range result.Metadata {
		metadata.CustomMetadata[k] = aws.StringValue(v)
		if k == "sha256" {
			metadata.Checksum = aws.StringValue(v)
		}
	}

	return metadata, nil
}

// ValidateURL checks if a URL is valid for S3 storage
func (s *S3Storage) ValidateURL(url string) error {
	_, err := s.parseS3Key(url)
	return err
}

// Close closes the S3 storage connection
func (s *S3Storage) Close() error {
	// AWS SDK doesn't require explicit cleanup
	return nil
}

// parseS3Key extracts the S3 key from a URL
// Supports formats: s3://bucket/key, https://endpoint/bucket/key, /key
func (s *S3Storage) parseS3Key(url string) (string, error) {
	url = strings.TrimSpace(url)

	// Handle s3:// URL format
	if strings.HasPrefix(url, "s3://") {
		parts := strings.SplitN(strings.TrimPrefix(url, "s3://"), "/", 2)
		if len(parts) < 2 {
			return "", &StorageError{
				Type:    ErrorTypeInvalidConfig,
				Message: "invalid s3:// URL format, expected s3://bucket/key",
			}
		}
		// Verify bucket matches configured bucket
		if parts[0] != s.config.Bucket {
			return "", &StorageError{
				Type:    ErrorTypeInvalidConfig,
				Message: fmt.Sprintf("bucket mismatch: URL has %s, configured %s", parts[0], s.config.Bucket),
			}
		}
		return parts[1], nil
	}

	// Handle HTTPS URL format
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		// Extract path after domain
		parts := strings.SplitN(url, "/", 4)
		if len(parts) < 4 {
			return "", &StorageError{
				Type:    ErrorTypeInvalidConfig,
				Message: "invalid HTTP(S) URL format",
			}
		}
		// parts[3] should be bucket/key
		keyParts := strings.SplitN(parts[3], "/", 2)
		if len(keyParts) < 2 {
			return "", &StorageError{
				Type:    ErrorTypeInvalidConfig,
				Message: "invalid HTTP(S) URL format, expected https://endpoint/bucket/key",
			}
		}
		return keyParts[1], nil
	}

	// Handle plain key format (relative path)
	if strings.HasPrefix(url, "/") {
		return strings.TrimPrefix(url, "/"), nil
	}

	// Assume it's a plain key
	return url, nil
}

// progressReader wraps an io.Reader to track progress and calculate checksum
type progressReader struct {
	reader      io.Reader
	total       int64
	transferred int64
	callback    func(int64, int64)
	hasher      hash.Hash
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		pr.transferred += int64(n)
		if pr.hasher != nil {
			pr.hasher.Write(p[:n])
		}
		if pr.callback != nil {
			pr.callback(pr.transferred, pr.total)
		}
	}
	return n, err
}

// progressWriter wraps an io.WriterAt to track progress and calculate checksum
type progressWriter struct {
	writer      io.WriterAt
	total       int64
	transferred int64
	callback    func(int64, int64)
	hasher      hash.Hash
}

func (pw *progressWriter) WriteAt(p []byte, off int64) (int, error) {
	n, err := pw.writer.WriteAt(p, off)
	if n > 0 {
		pw.transferred += int64(n)
		if pw.hasher != nil {
			pw.hasher.Write(p[:n])
		}
		if pw.callback != nil {
			pw.callback(pw.transferred, pw.total)
		}
	}
	return n, err
}

// writerAtWrapper wraps an io.Writer to implement io.WriterAt
type writerAtWrapper struct {
	writer io.Writer
}

func (w *writerAtWrapper) WriteAt(p []byte, off int64) (n int, err error) {
	// This is a simple implementation that assumes sequential writes
	// For non-sequential writes, we'd need a more complex buffer
	return w.writer.Write(p)
}
