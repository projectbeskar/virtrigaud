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
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NFSStorage implements the Storage interface for NFS-mounted storage
// Suitable for on-premises deployments with shared NFS storage
type NFSStorage struct {
	config    StorageConfig
	mountPath string
	verified  bool
}

// NewNFSStorage creates a new NFS storage backend
func NewNFSStorage(config StorageConfig) (*NFSStorage, error) {
	log.Printf("INFO Initializing NFS storage: mount=%s", config.Endpoint)

	// Set defaults
	if config.Timeout == 0 {
		config.Timeout = 300 // 5 minutes default
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.ChunkSize == 0 {
		config.ChunkSize = 32 * 1024 * 1024 // 32MB default buffer size
	}

	// Validate mount path
	mountPath := config.Endpoint
	if mountPath == "" {
		return nil, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "NFS mount path (endpoint) is required",
		}
	}

	// Clean up path
	mountPath = filepath.Clean(mountPath)

	storage := &NFSStorage{
		config:    config,
		mountPath: mountPath,
		verified:  false,
	}

	// Verify mount is accessible
	if err := storage.verifyMount(); err != nil {
		return nil, err
	}

	log.Printf("INFO NFS storage initialized successfully: %s", mountPath)
	return storage, nil
}

// verifyMount checks if the NFS mount is accessible and writable
func (n *NFSStorage) verifyMount() error {
	// Check if mount path exists
	info, err := os.Stat(n.mountPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &StorageError{
				Type:    ErrorTypeNotFound,
				Message: fmt.Sprintf("NFS mount path does not exist: %s", n.mountPath),
				Cause:   err,
			}
		}
		return &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("failed to stat NFS mount path: %s", n.mountPath),
			Cause:   err,
		}
	}

	// Check if it's a directory
	if !info.IsDir() {
		return &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: fmt.Sprintf("NFS mount path is not a directory: %s", n.mountPath),
		}
	}

	// Test write access by creating a temporary test file
	testFile := filepath.Join(n.mountPath, ".virtrigaud-nfs-test")
	f, err := os.Create(testFile)
	if err != nil {
		return &StorageError{
			Type:    ErrorTypePermissionDenied,
			Message: fmt.Sprintf("NFS mount is not writable: %s", n.mountPath),
			Cause:   err,
		}
	}
	f.Close()
	os.Remove(testFile)

	n.verified = true
	return nil
}

// Upload uploads a file to NFS storage
func (n *NFSStorage) Upload(ctx context.Context, req UploadRequest) (UploadResponse, error) {
	log.Printf("INFO Uploading to NFS: %s", req.DestinationURL)

	// Parse NFS path
	destPath, err := n.parseNFSPath(req.DestinationURL)
	if err != nil {
		return UploadResponse{}, err
	}

	// Create destination directory if it doesn't exist
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to create destination directory",
			Cause:   err,
		}
	}

	var reader io.Reader
	var fileSize int64
	var sourceFile *os.File

	// Use provided reader or open file
	if req.Reader != nil {
		reader = req.Reader
		fileSize = req.ContentLength
	} else if req.SourcePath != "" {
		sourceFile, err = os.Open(req.SourcePath)
		if err != nil {
			return UploadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to open source file",
				Cause:   err,
			}
		}
		defer sourceFile.Close()

		fileInfo, err := sourceFile.Stat()
		if err != nil {
			return UploadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to get file info",
				Cause:   err,
			}
		}
		fileSize = fileInfo.Size()
		reader = sourceFile
	} else {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "either SourcePath or Reader must be provided",
		}
	}

	// Create temporary file for atomic write
	tempPath := destPath + ".tmp." + fmt.Sprintf("%d", time.Now().UnixNano())
	destFile, err := os.Create(tempPath)
	if err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to create destination file",
			Cause:   err,
		}
	}

	// Ensure cleanup on error
	var uploadSuccess bool
	defer func() {
		destFile.Close()
		if !uploadSuccess {
			os.Remove(tempPath)
		}
	}()

	// Create progress tracking reader with checksum
	hasher := sha256.New()
	var transferred int64
	buf := make([]byte, n.config.ChunkSize)

	// Copy with progress tracking and checksum calculation
	startTime := time.Now()
	var lastReport time.Time

	for {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return UploadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "upload cancelled",
				Cause:   ctx.Err(),
			}
		default:
		}

		nr, er := reader.Read(buf)
		if nr > 0 {
			// Write to destination
			nw, ew := destFile.Write(buf[0:nr])
			if ew != nil {
				return UploadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "failed to write to destination",
					Cause:   ew,
				}
			}
			if nr != nw {
				return UploadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "short write to destination",
				}
			}

			// Update checksum
			hasher.Write(buf[0:nr])
			transferred += int64(nw)

			// Report progress (throttle to every 500ms)
			if req.ProgressCallback != nil && time.Since(lastReport) > 500*time.Millisecond {
				req.ProgressCallback(transferred, fileSize)
				lastReport = time.Now()
			}
		}

		if er != nil {
			if er != io.EOF {
				return UploadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "failed to read from source",
					Cause:   er,
				}
			}
			break
		}
	}

	// Final progress report
	if req.ProgressCallback != nil {
		req.ProgressCallback(transferred, fileSize)
	}

	// Sync to ensure data is written to disk
	if err := destFile.Sync(); err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to sync destination file",
			Cause:   err,
		}
	}

	// Close before rename
	destFile.Close()

	// Atomic rename to final destination
	if err := os.Rename(tempPath, destPath); err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to rename temporary file to destination",
			Cause:   err,
		}
	}

	uploadSuccess = true

	// Calculate checksum
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// Verify checksum if provided
	if req.Checksum != "" && req.Checksum != checksum {
		// Remove the file on checksum mismatch
		os.Remove(destPath)
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeChecksumMismatch,
			Message: fmt.Sprintf("checksum mismatch: expected %s, got %s", req.Checksum, checksum),
		}
	}

	// Store metadata as extended attributes or sidecar file
	if len(req.Metadata) > 0 || req.Checksum != "" {
		if err := n.storeMetadata(destPath, checksum, req.Metadata); err != nil {
			log.Printf("WARN Failed to store metadata: %v", err)
		}
	}

	duration := time.Since(startTime)
	rate := float64(transferred) / duration.Seconds() / 1024 / 1024 // MB/s

	log.Printf("INFO NFS upload complete: %s (checksum=%s, bytes=%d, rate=%.2f MB/s)",
		destPath, checksum, transferred, rate)

	return UploadResponse{
		URL:              req.DestinationURL,
		Checksum:         checksum,
		BytesTransferred: transferred,
		ETag:             checksum[:16], // Use first 16 chars of checksum as ETag
	}, nil
}

// Download downloads a file from NFS storage
func (n *NFSStorage) Download(ctx context.Context, req DownloadRequest) (DownloadResponse, error) {
	log.Printf("INFO Downloading from NFS: %s", req.SourceURL)

	// Parse NFS path
	sourcePath, err := n.parseNFSPath(req.SourceURL)
	if err != nil {
		return DownloadResponse{}, err
	}

	// Check if source file exists
	fileInfo, err := os.Stat(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeNotFound,
				Message: fmt.Sprintf("source file not found: %s", sourcePath),
				Cause:   err,
			}
		}
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to stat source file",
			Cause:   err,
		}
	}

	contentLength := fileInfo.Size()

	// Open source file
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to open source file",
			Cause:   err,
		}
	}
	defer sourceFile.Close()

	// Seek to resume offset if specified
	if req.ResumeOffset > 0 {
		if _, err := sourceFile.Seek(req.ResumeOffset, 0); err != nil {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to seek to resume offset",
				Cause:   err,
			}
		}
	}

	var writer io.Writer
	var destFile *os.File

	// Use provided writer or create file
	if req.Writer != nil {
		writer = req.Writer
	} else if req.DestinationPath != "" {
		// Create destination directory if needed
		destDir := filepath.Dir(req.DestinationPath)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to create destination directory",
				Cause:   err,
			}
		}

		// Create temporary file
		tempPath := req.DestinationPath + ".tmp." + fmt.Sprintf("%d", time.Now().UnixNano())
		destFile, err = os.Create(tempPath)
		if err != nil {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to create destination file",
				Cause:   err,
			}
		}

		// Ensure cleanup on error
		var downloadSuccess bool
		defer func() {
			if destFile != nil {
				destFile.Close()
			}
			if !downloadSuccess {
				os.Remove(tempPath)
			} else {
				// Atomic rename on success
				os.Rename(tempPath, req.DestinationPath)
			}
		}()

		writer = destFile
	} else {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "either DestinationPath or Writer must be provided",
		}
	}

	// Create progress tracking with checksum
	hasher := sha256.New()
	var transferred int64
	buf := make([]byte, n.config.ChunkSize)

	startTime := time.Now()
	var lastReport time.Time

	// Copy with progress tracking and checksum calculation
	for {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "download cancelled",
				Cause:   ctx.Err(),
			}
		default:
		}

		nr, er := sourceFile.Read(buf)
		if nr > 0 {
			// Write to destination
			nw, ew := writer.Write(buf[0:nr])
			if ew != nil {
				return DownloadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "failed to write to destination",
					Cause:   ew,
				}
			}
			if nr != nw {
				return DownloadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "short write to destination",
				}
			}

			// Update checksum
			hasher.Write(buf[0:nr])
			transferred += int64(nw)

			// Report progress (throttle to every 500ms)
			if req.ProgressCallback != nil && time.Since(lastReport) > 500*time.Millisecond {
				req.ProgressCallback(transferred, contentLength)
				lastReport = time.Now()
			}
		}

		if er != nil {
			if er != io.EOF {
				return DownloadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "failed to read from source",
					Cause:   er,
				}
			}
			break
		}
	}

	// Final progress report
	if req.ProgressCallback != nil {
		req.ProgressCallback(transferred, contentLength)
	}

	// Sync if writing to file
	if destFile != nil {
		if err := destFile.Sync(); err != nil {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to sync destination file",
				Cause:   err,
			}
		}
	}

	// Calculate checksum
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// Verify checksum if requested
	if req.VerifyChecksum && req.ExpectedChecksum != "" {
		if req.ExpectedChecksum != checksum {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeChecksumMismatch,
				Message: fmt.Sprintf("checksum mismatch: expected %s, got %s", req.ExpectedChecksum, checksum),
			}
		}
	}

	// Mark download as successful for defer cleanup
	if destFile != nil {
		destFile.Close()
		destFile = nil
		// Atomic rename will happen in defer
	}

	duration := time.Since(startTime)
	rate := float64(transferred) / duration.Seconds() / 1024 / 1024 // MB/s

	log.Printf("INFO NFS download complete: %s (checksum=%s, bytes=%d, rate=%.2f MB/s)",
		sourcePath, checksum, transferred, rate)

	return DownloadResponse{
		BytesTransferred: transferred,
		Checksum:         checksum,
		ContentLength:    contentLength,
	}, nil
}

// Delete deletes a file from NFS storage
func (n *NFSStorage) Delete(ctx context.Context, url string) error {
	log.Printf("INFO Deleting from NFS: %s", url)

	// Parse NFS path
	filePath, err := n.parseNFSPath(url)
	if err != nil {
		return err
	}

	// Remove the file
	if err := os.Remove(filePath); err != nil {
		if os.IsNotExist(err) {
			// Already deleted, consider it success
			log.Printf("INFO File already deleted: %s", filePath)
			return nil
		}
		return &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to delete file",
			Cause:   err,
		}
	}

	// Also remove metadata file if it exists
	metadataPath := filePath + ".metadata"
	os.Remove(metadataPath) // Ignore errors

	log.Printf("INFO Deleted from NFS: %s", filePath)
	return nil
}

// GetMetadata retrieves metadata about a stored file
func (n *NFSStorage) GetMetadata(ctx context.Context, url string) (FileMetadata, error) {
	// Parse NFS path
	filePath, err := n.parseNFSPath(url)
	if err != nil {
		return FileMetadata{}, err
	}

	// Get file info
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return FileMetadata{}, &StorageError{
				Type:    ErrorTypeNotFound,
				Message: fmt.Sprintf("file not found: %s", filePath),
				Cause:   err,
			}
		}
		return FileMetadata{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to stat file",
			Cause:   err,
		}
	}

	metadata := FileMetadata{
		URL:            url,
		Size:           fileInfo.Size(),
		LastModified:   fileInfo.ModTime().Format(time.RFC3339),
		ContentType:    "application/octet-stream",
		CustomMetadata: make(map[string]string),
	}

	// Try to load metadata from sidecar file
	storedChecksum, customMetadata, err := n.loadMetadata(filePath)
	if err == nil {
		metadata.Checksum = storedChecksum
		metadata.CustomMetadata = customMetadata
		metadata.ETag = storedChecksum[:16]
	}

	return metadata, nil
}

// ValidateURL checks if a URL is valid for NFS storage
func (n *NFSStorage) ValidateURL(url string) error {
	_, err := n.parseNFSPath(url)
	return err
}

// Close closes the NFS storage connection
func (n *NFSStorage) Close() error {
	// No persistent connections to close for NFS
	return nil
}

// parseNFSPath converts a URL to an absolute NFS path
// Supports formats: nfs://path, /path, path
func (n *NFSStorage) parseNFSPath(url string) (string, error) {
	url = strings.TrimSpace(url)

	// Handle nfs:// URL format
	if strings.HasPrefix(url, "nfs://") {
		relativePath := strings.TrimPrefix(url, "nfs://")
		return filepath.Join(n.mountPath, relativePath), nil
	}

	// Handle absolute path
	if strings.HasPrefix(url, "/") {
		// Check if it's within mount path
		absPath := filepath.Clean(url)
		if !strings.HasPrefix(absPath, n.mountPath) {
			// If not, treat as relative to mount
			return filepath.Join(n.mountPath, url), nil
		}
		return absPath, nil
	}

	// Handle relative path
	return filepath.Join(n.mountPath, url), nil
}

// storeMetadata stores metadata as a sidecar JSON file
func (n *NFSStorage) storeMetadata(filePath, checksum string, metadata map[string]string) error {
	metadataPath := filePath + ".metadata"

	// Create metadata content
	content := fmt.Sprintf("checksum=%s\n", checksum)
	for k, v := range metadata {
		content += fmt.Sprintf("%s=%s\n", k, v)
	}

	// Write metadata file
	if err := os.WriteFile(metadataPath, []byte(content), 0644); err != nil {
		return err
	}

	return nil
}

// loadMetadata loads metadata from sidecar file
func (n *NFSStorage) loadMetadata(filePath string) (string, map[string]string, error) {
	metadataPath := filePath + ".metadata"

	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", nil, err
	}

	var checksum string
	metadata := make(map[string]string)

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if key == "checksum" {
			checksum = value
		} else {
			metadata[key] = value
		}
	}

	return checksum, metadata, nil
}

// nfsProgressReader wraps an io.Reader to track progress and calculate checksum
type nfsProgressReader struct {
	reader      io.Reader
	total       int64
	transferred int64
	callback    func(int64, int64)
	hasher      hash.Hash
}

func (pr *nfsProgressReader) Read(p []byte) (int, error) {
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
