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

// PVCStorage implements the Storage interface using a Kubernetes PVC
// The PVC must be mounted to the pod at the configured MountPath
type PVCStorage struct {
	config    StorageConfig
	mountPath string
	verified  bool
}

// NewPVCStorage creates a new PVC storage backend
func NewPVCStorage(config StorageConfig) (*PVCStorage, error) {
	log.Printf("INFO Initializing PVC storage: pvc=%s/%s mount=%s",
		config.PVCNamespace, config.PVCName, config.MountPath)

	// Set default mount path
	mountPath := config.MountPath
	if mountPath == "" {
		mountPath = "/mnt/migration-storage"
	}

	// Clean up path
	mountPath = filepath.Clean(mountPath)

	storage := &PVCStorage{
		config:    config,
		mountPath: mountPath,
		verified:  false,
	}

	// Verify mount is accessible
	if err := storage.verifyMount(); err != nil {
		return nil, err
	}

	log.Printf("INFO PVC storage initialized successfully: %s", mountPath)
	return storage, nil
}

// verifyMount checks if the PVC mount is accessible and writable
func (p *PVCStorage) verifyMount() error {
	// Check if mount path exists
	info, err := os.Stat(p.mountPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &StorageError{
				Type:    ErrorTypeNotFound,
				Message: fmt.Sprintf("PVC mount path does not exist: %s (PVC may not be mounted to pod)", p.mountPath),
				Cause:   err,
			}
		}
		return &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("failed to stat PVC mount path: %s", p.mountPath),
			Cause:   err,
		}
	}

	// Check if it's a directory
	if !info.IsDir() {
		return &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: fmt.Sprintf("PVC mount path is not a directory: %s", p.mountPath),
		}
	}

	// Note: We don't test write access to the parent mount path here
	// because PVCs are mounted as subdirectories (e.g., /mnt/migration-storage/<pvc-name>)
	// Write access will be tested when actually writing files

	p.verified = true
	log.Printf("INFO PVC mount path verified: %s", p.mountPath)
	return nil
}

// Upload uploads a file to PVC storage
func (p *PVCStorage) Upload(ctx context.Context, req UploadRequest) (UploadResponse, error) {
	log.Printf("INFO Uploading to PVC storage: %s", req.DestinationURL)

	// Parse destination path
	destPath, err := p.parsePath(req.DestinationURL)
	if err != nil {
		return UploadResponse{}, err
	}

	// Ensure destination directory exists
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("failed to create destination directory: %s", destDir),
			Cause:   err,
		}
	}

	var reader io.Reader
	var contentLength int64

	if req.SourcePath != "" {
		// Open source file
		srcFile, err := os.Open(req.SourcePath)
		if err != nil {
			return UploadResponse{}, &StorageError{
				Type:    ErrorTypeNotFound,
				Message: fmt.Sprintf("failed to open source file: %s", req.SourcePath),
				Cause:   err,
			}
		}
		defer srcFile.Close()

		// Get file size
		info, err := srcFile.Stat()
		if err != nil {
			return UploadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "failed to stat source file",
				Cause:   err,
			}
		}
		contentLength = info.Size()
		reader = srcFile
	} else if req.Reader != nil {
		reader = req.Reader
		contentLength = req.ContentLength
	} else {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "either SourcePath or Reader must be provided",
		}
	}

	// Create destination file
	destFile, err := os.Create(destPath)
	if err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("failed to create destination file: %s", destPath),
			Cause:   err,
		}
	}
	defer destFile.Close()

	// Copy data with checksum calculation
	hasher := sha256.New()
	var bytesTransferred int64
	startTime := time.Now()

	// Use a multi-writer to write to file and calculate checksum simultaneously
	multiWriter := io.MultiWriter(destFile, hasher)

	// Copy with progress reporting
	buffer := make([]byte, 32*1024*1024) // 32MB buffer
	for {
		nr, er := reader.Read(buffer)
		if nr > 0 {
			nw, ew := multiWriter.Write(buffer[0:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = fmt.Errorf("invalid write result")
				}
			}
			bytesTransferred += int64(nw)
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
					Message: "short write",
				}
			}

			// Progress callback
			if req.ProgressCallback != nil && contentLength > 0 {
				req.ProgressCallback(bytesTransferred, contentLength)
			}
		}
		if er != nil {
			if er != io.EOF {
				return UploadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "failed to read source",
					Cause:   er,
				}
			}
			break
		}
	}

	// Sync to disk
	if err := destFile.Sync(); err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to sync file to disk",
			Cause:   err,
		}
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	duration := time.Since(startTime)

	// Verify checksum if provided
	if req.Checksum != "" && req.Checksum != checksum {
		// Remove corrupted file
		os.Remove(destPath)
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeChecksumMismatch,
			Message: fmt.Sprintf("checksum mismatch: expected=%s actual=%s", req.Checksum, checksum),
		}
	}

	log.Printf("INFO Upload completed: %s (%d bytes in %v)", destPath, bytesTransferred, duration)

	return UploadResponse{
		URL:              req.DestinationURL,
		Checksum:         checksum,
		BytesTransferred: bytesTransferred,
	}, nil
}

// Download downloads a file from PVC storage
func (p *PVCStorage) Download(ctx context.Context, req DownloadRequest) (DownloadResponse, error) {
	log.Printf("INFO Downloading from PVC storage: %s", req.SourceURL)

	// Parse source path
	srcPath, err := p.parsePath(req.SourceURL)
	if err != nil {
		return DownloadResponse{}, err
	}

	// Open source file
	srcFile, err := os.Open(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeNotFound,
				Message: fmt.Sprintf("source file not found: %s", srcPath),
				Cause:   err,
			}
		}
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("failed to open source file: %s", srcPath),
			Cause:   err,
		}
	}
	defer srcFile.Close()

	// Get file size
	info, err := srcFile.Stat()
	if err != nil {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to stat source file",
			Cause:   err,
		}
	}
	contentLength := info.Size()

	var writer io.Writer
	var bytesTransferred int64
	startTime := time.Now()

	// Setup destination
	if req.DestinationPath != "" {
		// Ensure destination directory exists
		destDir := filepath.Dir(req.DestinationPath)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: fmt.Sprintf("failed to create destination directory: %s", destDir),
				Cause:   err,
			}
		}

		destFile, err := os.Create(req.DestinationPath)
		if err != nil {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: fmt.Sprintf("failed to create destination file: %s", req.DestinationPath),
				Cause:   err,
			}
		}
		defer destFile.Close()
		writer = destFile
	} else if req.Writer != nil {
		writer = req.Writer
	} else {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "either DestinationPath or Writer must be provided",
		}
	}

	// Setup checksum if verification is enabled
	var hasher hash.Hash
	if req.VerifyChecksum {
		hasher = sha256.New()
		writer = io.MultiWriter(writer, hasher)
	}

	// Copy data with progress reporting
	buffer := make([]byte, 32*1024*1024) // 32MB buffer
	for {
		nr, er := srcFile.Read(buffer)
		if nr > 0 {
			nw, ew := writer.Write(buffer[0:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = fmt.Errorf("invalid write result")
				}
			}
			bytesTransferred += int64(nw)
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
					Message: "short write",
				}
			}

			// Progress callback
			if req.ProgressCallback != nil {
				req.ProgressCallback(bytesTransferred, contentLength)
			}
		}
		if er != nil {
			if er != io.EOF {
				return DownloadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "failed to read source",
					Cause:   er,
				}
			}
			break
		}
	}

	var checksum string
	if req.VerifyChecksum && hasher != nil {
		checksum = hex.EncodeToString(hasher.Sum(nil))

		// Verify checksum if expected value provided
		if req.ExpectedChecksum != "" && req.ExpectedChecksum != checksum {
			return DownloadResponse{}, &StorageError{
				Type:    ErrorTypeChecksumMismatch,
				Message: fmt.Sprintf("checksum mismatch: expected=%s actual=%s", req.ExpectedChecksum, checksum),
			}
		}
	}

	duration := time.Since(startTime)
	log.Printf("INFO Download completed: %s (%d bytes in %v)", srcPath, bytesTransferred, duration)

	return DownloadResponse{
		BytesTransferred: bytesTransferred,
		Checksum:         checksum,
		ContentLength:    contentLength,
	}, nil
}

// Delete removes a file from PVC storage
func (p *PVCStorage) Delete(ctx context.Context, url string) error {
	log.Printf("INFO Deleting from PVC storage: %s", url)

	filePath, err := p.parsePath(url)
	if err != nil {
		return err
	}

	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		// File doesn't exist, consider it deleted
		log.Printf("WARN File already deleted: %s", filePath)
		return nil
	}

	// Remove file
	if err := os.Remove(filePath); err != nil {
		return &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("failed to delete file: %s", filePath),
			Cause:   err,
		}
	}

	log.Printf("INFO Successfully deleted: %s", filePath)
	return nil
}

// GetMetadata retrieves metadata about a stored file
func (p *PVCStorage) GetMetadata(ctx context.Context, url string) (FileMetadata, error) {
	filePath, err := p.parsePath(url)
	if err != nil {
		return FileMetadata{}, err
	}

	// Get file info
	info, err := os.Stat(filePath)
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
			Message: fmt.Sprintf("failed to stat file: %s", filePath),
			Cause:   err,
		}
	}

	// Calculate checksum
	file, err := os.Open(filePath)
	if err != nil {
		return FileMetadata{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to open file for checksum",
			Cause:   err,
		}
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return FileMetadata{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to calculate checksum",
			Cause:   err,
		}
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))

	return FileMetadata{
		URL:          url,
		Size:         info.Size(),
		Checksum:     checksum,
		LastModified: info.ModTime().Format(time.RFC3339),
	}, nil
}

// ValidateURL checks if a URL is valid for PVC storage
func (p *PVCStorage) ValidateURL(url string) error {
	_, err := p.parsePath(url)
	return err
}

// Close closes the PVC storage connection
func (p *PVCStorage) Close() error {
	// No cleanup needed for PVC storage
	// PVC is managed by Kubernetes
	return nil
}

// parsePath converts a URL to an absolute PVC path
// Supports formats: pvc://path, /path, path
func (p *PVCStorage) parsePath(url string) (string, error) {
	url = strings.TrimSpace(url)

	// Handle pvc:// URL format: pvc://<pvc-name>/<file-path>
	url = strings.TrimPrefix(url, "pvc://")

	// Ensure path doesn't try to escape the mount
	if strings.Contains(url, "..") {
		return "", &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "invalid path: contains '..'",
		}
	}

	// Parse the URL to extract PVC name and file path
	// Format: <pvc-name>/<file-path>
	// Example: rbc-demo-migration-storage/vmmigrations/default/rbc-demo-migration/export.qcow2
	parts := strings.SplitN(url, "/", 2)
	if len(parts) < 2 {
		return "", &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "invalid PVC URL format: expected pvc://<pvc-name>/<file-path>",
		}
	}

	pvcName := parts[0]
	filePath := parts[1]

	// Construct the full path: /mnt/migration-storage/<pvc-name>/<file-path>
	absPath := filepath.Join("/mnt/migration-storage", pvcName, filePath)

	log.Printf("DEBUG Parsed PVC path: url=%s pvc=%s file=%s abs=%s", url, pvcName, filePath, absPath)

	return absPath, nil
}
