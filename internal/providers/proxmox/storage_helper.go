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

package proxmox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
)

// StorageManager handles Proxmox storage operations
type StorageManager struct {
	client *pveapi.Client
	node   string
}

// NewStorageManager creates a new Proxmox storage manager
func NewStorageManager(client *pveapi.Client, node string) *StorageManager {
	return &StorageManager{
		client: client,
		node:   node,
	}
}

// DownloadVolume downloads a disk volume from Proxmox storage
// This uses direct file access for directory-based storage
func (sm *StorageManager) DownloadVolume(ctx context.Context, storage, volid string, writer io.Writer, progressCallback func(int64, int64)) error {
	// Try direct file access first (works for dir, nfs storage)
	// Volume ID format examples: 
	//   - local:100/vm-100-disk-0.qcow2
	//   - local:100/base-100-disk-0.qcow2
	
	return sm.downloadDirectFile(ctx, storage, volid, writer, progressCallback)
}

// downloadDirectFile downloads a file directly from directory-based storage
func (sm *StorageManager) downloadDirectFile(ctx context.Context, storage, volid string, writer io.Writer, progressCallback func(int64, int64)) error {
	// Volume ID format: storage:vmid/vm-vmid-disk-N.qcow2
	// We need to construct the path
	// Common paths:
	//   - /var/lib/vz/images/{vmid}/vm-{vmid}-disk-{N}.qcow2 (local storage)
	//   - /mnt/pve/{storage}/images/{vmid}/vm-{vmid}-disk-{N}.qcow2 (nfs)
	
	// Try multiple common paths
	paths := []string{
		fmt.Sprintf("/var/lib/vz/images/%s", volid),
		fmt.Sprintf("/mnt/pve/%s/images/%s", storage, volid),
		fmt.Sprintf("/mnt/pve/%s/%s", storage, volid),
	}

	var file *os.File
	var err error
	for _, path := range paths {
		file, err = os.Open(path)
		if err == nil {
			break
		}
	}

	if err != nil {
		return fmt.Errorf("failed to access volume file (tried common paths): %w", err)
	}
	defer file.Close()

	// Get file size for progress tracking
	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	totalSize := stat.Size()
	var transferred int64

	// Copy with progress tracking
	if progressCallback != nil {
		progressReader := &progressTrackingReader{
			reader: file,
			callback: func(n int) {
				transferred += int64(n)
				progressCallback(transferred, totalSize)
			},
		}
		_, err = io.Copy(writer, progressReader)
	} else {
		_, err = io.Copy(writer, file)
	}

	if err != nil {
		return fmt.Errorf("failed to copy file data: %w", err)
	}

	return nil
}


// UploadVolume uploads a disk volume to Proxmox storage
func (sm *StorageManager) UploadVolume(ctx context.Context, reader io.Reader, storage, filename string, contentLength int64, progressCallback func(int64, int64)) (string, error) {
	// Use direct file access for directory-based storage
	return sm.uploadDirectFile(ctx, reader, storage, filename, contentLength, progressCallback)
}

// uploadDirectFile uploads directly to directory-based storage
func (sm *StorageManager) uploadDirectFile(ctx context.Context, reader io.Reader, storage, filename string, contentLength int64, progressCallback func(int64, int64)) (string, error) {
	// Try multiple common base paths
	basePaths := []string{
		"/var/lib/vz/images",
		fmt.Sprintf("/mnt/pve/%s/images", storage),
		fmt.Sprintf("/mnt/pve/%s", storage),
	}

	var fullPath string
	var dir string
	var err error

	// Try each base path
	for _, basePath := range basePaths {
		fullPath = filepath.Join(basePath, filename)
		dir = filepath.Dir(fullPath)
		
		// Try to create directory
		err = os.MkdirAll(dir, 0755)
		if err == nil {
			break
		}
	}

	if err != nil {
		return "", fmt.Errorf("failed to access storage directory (tried common paths): %w", err)
	}

	// Create temporary file
	tempPath := fullPath + ".tmp"
	file, err := os.Create(tempPath)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer file.Close()

	// Copy with progress tracking
	var transferred int64
	if progressCallback != nil {
		progressReader := &progressTrackingReader{
			reader: reader,
			callback: func(n int) {
				transferred += int64(n)
				progressCallback(transferred, contentLength)
			},
		}
		_, err = io.Copy(file, progressReader)
	} else {
		_, err = io.Copy(file, reader)
	}

	if err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	// Sync to disk
	err = file.Sync()
	if err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("failed to sync file: %w", err)
	}

	// Close before rename
	file.Close()

	// Atomic rename
	_ = os.Rename(tempPath, fullPath)

	// Return volume ID (relative path from images dir)
	volid := fmt.Sprintf("%s:%s", storage, filename)
	return volid, nil
}


// DeleteVolume deletes a volume from Proxmox storage
func (sm *StorageManager) DeleteVolume(ctx context.Context, storage, volid string) error {
	// Try to delete from common paths
	paths := []string{
		fmt.Sprintf("/var/lib/vz/images/%s", volid),
		fmt.Sprintf("/mnt/pve/%s/images/%s", storage, volid),
		fmt.Sprintf("/mnt/pve/%s/%s", storage, volid),
	}

	var lastErr error
	for _, path := range paths {
		err := os.Remove(path)
		if err == nil {
			return nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return fmt.Errorf("failed to delete volume (tried common paths): %w", lastErr)
	}

	return nil
}

// progressTrackingReader wraps an io.Reader to track progress
type progressTrackingReader struct {
	reader   io.Reader
	callback func(int)
}

func (ptr *progressTrackingReader) Read(p []byte) (n int, err error) {
	n, err = ptr.reader.Read(p)
	if n > 0 && ptr.callback != nil {
		ptr.callback(n)
	}
	return n, err
}

