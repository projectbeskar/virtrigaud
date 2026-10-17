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
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/storage/stagedfile"
)

// Constant messages of staged-file failures. They name no path: a refusal can
// be caused by a link the tenant planted, and the message reaches the
// VMMigration's status; the detail (paths, system error) goes to the provider
// log only.
const (
	msgStagedFileRefused       = "refused the staged file"
	msgStagedFileNotFound      = "the staged file was not found"
	msgStagedFileOpenFailed    = "failed to open the staged file"
	msgStagedFileCreateFailed  = "failed to create the staged file"
	msgStagedFileRemoveFailed  = "failed to remove the staged file"
	msgStagedFileInvalidSchema = "invalid PVC URL format: expected pvc://<pvc-name>/<file-path>"
)

// stagedFileError logs the full detail of a stagedfile failure and returns
// the StorageError the caller sees: a constant message by kind, with the
// stagedfile error (whose text is constant too) as Cause so errors.Is still
// matches stagedfile.ErrUnsafe and fs.ErrNotExist.
func stagedFileError(failedMsg string, err error) error {
	switch {
	case stagedfile.IsUnsafe(err):
		log.Printf("WARN Refused a staged file in the PVC: %s", stagedfile.LogDetail(err))
		return &StorageError{Type: ErrorTypeUnsafePath, Message: msgStagedFileRefused, Cause: err}
	case errors.Is(err, fs.ErrNotExist):
		log.Printf("WARN Staged file not found in the PVC: %s", stagedfile.LogDetail(err))
		return &StorageError{Type: ErrorTypeNotFound, Message: msgStagedFileNotFound, Cause: err}
	default:
		log.Printf("ERROR Staged-file operation failed in the PVC: %s", stagedfile.LogDetail(err))
		return &StorageError{Type: ErrorTypeOperationFailed, Message: failedMsg, Cause: err}
	}
}

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
	destPath, destRel, err := p.parsePath(req.DestinationURL)
	if err != nil {
		return UploadResponse{}, err
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

	// Create the destination file and its missing directories beneath the
	// mount without following a symbolic link anywhere: the PVC is writable by
	// the tenant, who could otherwise redirect this write to any path the
	// provider pod can write (stagedfile).
	destFile, err := stagedfile.Create(p.mountPath, destRel, stagedfile.DefaultDirPerm, stagedfile.DefaultFilePerm)
	if err != nil {
		return UploadResponse{}, stagedFileError(msgStagedFileCreateFailed, err)
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
		if rmErr := stagedfile.Remove(p.mountPath, destRel); rmErr != nil {
			log.Printf("WARN Failed to remove the corrupted staged file: %s", stagedfile.LogDetail(rmErr))
		}
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
	srcPath, srcRel, err := p.parsePath(req.SourceURL)
	if err != nil {
		return DownloadResponse{}, err
	}

	// Open the staged file beneath the mount with no symbolic link in any
	// component, and only if it is a regular single-link file: the tenant can
	// write the PVC, and a planted link would otherwise make this read — and
	// the import that follows — return the provider pod's credentials
	// (stagedfile).
	srcFile, err := stagedfile.OpenRead(p.mountPath, srcRel)
	if err != nil {
		return DownloadResponse{}, stagedFileError(msgStagedFileOpenFailed, err)
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

// UploadStream streams the reader to a file under the mount, computing SHA256
// in-stream. It is the streaming-interface counterpart of Upload for the PVC
// backend (ADR-0006); it reuses Upload's file I/O path.
func (p *PVCStorage) UploadStream(ctx context.Context, req StreamUploadRequest) (UploadResponse, error) {
	return p.Upload(ctx, UploadRequest{
		DestinationURL: req.DestinationURL,
		Reader:         req.Reader,
		ContentLength:  req.ContentLength,
	})
}

// DownloadStream streams a file under the mount to the writer, computing SHA256
// in-stream and verifying it against ExpectedChecksum when set. It is the
// streaming-interface counterpart of Download for the PVC backend (ADR-0006).
func (p *PVCStorage) DownloadStream(ctx context.Context, req StreamDownloadRequest) (DownloadResponse, error) {
	return p.Download(ctx, DownloadRequest{
		SourceURL:        req.SourceURL,
		Writer:           req.Writer,
		VerifyChecksum:   req.ExpectedChecksum != "",
		ExpectedChecksum: req.ExpectedChecksum,
	})
}

// Delete removes a file from PVC storage
func (p *PVCStorage) Delete(ctx context.Context, url string) error {
	log.Printf("INFO Deleting from PVC storage: %s", url)

	filePath, fileRel, err := p.parsePath(url)
	if err != nil {
		return err
	}

	// Remove the file beneath the mount without following a symbolic link in
	// any directory on its path (a link at the name itself is removed, never
	// followed).
	if err := stagedfile.Remove(p.mountPath, fileRel); err != nil {
		if !stagedfile.IsUnsafe(err) && errors.Is(err, fs.ErrNotExist) {
			// File doesn't exist, consider it deleted
			log.Printf("WARN File already deleted: %s", filePath)
			return nil
		}
		return stagedFileError(msgStagedFileRemoveFailed, err)
	}

	log.Printf("INFO Successfully deleted: %s", filePath)
	return nil
}

// GetMetadata retrieves metadata about a stored file
func (p *PVCStorage) GetMetadata(ctx context.Context, url string) (FileMetadata, error) {
	_, fileRel, err := p.parsePath(url)
	if err != nil {
		return FileMetadata{}, err
	}

	// Open the file beneath the mount with no symbolic link followed, then
	// stat the open file (never the path, which the tenant can swap).
	file, err := stagedfile.OpenRead(p.mountPath, fileRel)
	if err != nil {
		return FileMetadata{}, stagedFileError(msgStagedFileOpenFailed, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return FileMetadata{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to stat the staged file",
			Cause:   err,
		}
	}

	// Calculate checksum

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
	_, _, err := p.parsePath(url)
	return err
}

// Close closes the PVC storage connection
func (p *PVCStorage) Close() error {
	// No cleanup needed for PVC storage
	// PVC is managed by Kubernetes
	return nil
}

// parsePath converts a URL to an absolute PVC path and the path relative to
// the mount (rel), which every file operation resolves beneath the mount with
// stagedfile. Supports formats: pvc://<pvc-name>/<file-path>, <pvc-name>/<file-path>.
func (p *PVCStorage) parsePath(url string) (abs string, rel string, err error) {
	url = strings.TrimSpace(url)

	// Handle pvc:// URL format: pvc://<pvc-name>/<file-path>
	url = strings.TrimPrefix(url, "pvc://")

	// Ensure path doesn't try to escape the mount
	if strings.Contains(url, "..") {
		return "", "", &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "invalid path: contains '..'",
		}
	}

	// Parse the URL to extract PVC name and file path
	// Format: <pvc-name>/<file-path>
	// Example: rbc-demo-migration-storage/vmmigrations/default/rbc-demo-migration/export.qcow2
	parts := strings.SplitN(url, "/", 2)
	if len(parts) < 2 {
		return "", "", &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: msgStagedFileInvalidSchema,
		}
	}

	pvcName := parts[0]
	filePath := strings.TrimLeft(parts[1], "/")
	if filePath == "" {
		return "", "", &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: msgStagedFileInvalidSchema,
		}
	}

	// Construct the full path using the configured mount path
	// MountPath should already include the PVC name (e.g., /mnt/migration-storage/<pvc-name>)
	// So we just append the file path
	absPath := filepath.Join(p.mountPath, filePath)

	log.Printf("DEBUG Parsed PVC path: url=%s pvc=%s file=%s mount=%s abs=%s", url, pvcName, filePath, p.mountPath, absPath)

	return absPath, filepath.Clean(filePath), nil
}
