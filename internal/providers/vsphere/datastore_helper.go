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

package vsphere

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/soap"
)

// DatastoreFileManager handles datastore file operations
type DatastoreFileManager struct {
	provider *Provider
}

// NewDatastoreFileManager creates a new datastore file manager
func NewDatastoreFileManager(provider *Provider) *DatastoreFileManager {
	return &DatastoreFileManager{
		provider: provider,
	}
}

// DownloadFile downloads a file from a datastore
func (dfm *DatastoreFileManager) DownloadFile(ctx context.Context, datastorePath string, writer io.Writer, progressCallback func(int64, int64)) error {
	if dfm.provider.client == nil {
		return fmt.Errorf("vSphere client not initialized")
	}

	// Parse the datastore path (format: "[datastoreName] path/to/file")
	dsName, filePath, err := parseDatastorePath(datastorePath)
	if err != nil {
		return fmt.Errorf("invalid datastore path: %w", err)
	}

	// Find the datastore
	datastore, err := dfm.provider.finder.Datastore(ctx, dsName)
	if err != nil {
		return fmt.Errorf("failed to find datastore %s: %w", dsName, err)
	}

	// Download the file using datastore.Download
	rc, _, err := datastore.Download(ctx, filePath, &soap.DefaultDownload)
	if err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer rc.Close()

	resp := rc
	// Copy with progress tracking
	if progressCallback != nil {
		var transferred int64
		totalSize := int64(-1) // Unknown size for datastore downloads

		// Create progress reader
		progressReader := &progressTrackingReader{
			reader: resp,
			callback: func(n int) {
				transferred += int64(n)
				progressCallback(transferred, totalSize)
			},
		}

		_, err = io.Copy(writer, progressReader)
	} else {
		_, err = io.Copy(writer, resp)
	}

	if err != nil {
		return fmt.Errorf("failed to copy file data: %w", err)
	}

	dfm.provider.logger.Info("File downloaded from datastore", "path", datastorePath)
	return nil
}

// UploadFile uploads a file to a datastore
func (dfm *DatastoreFileManager) UploadFile(ctx context.Context, reader io.Reader, datastorePath string, contentLength int64, progressCallback func(int64, int64)) error {
	if dfm.provider.client == nil {
		return fmt.Errorf("vSphere client not initialized")
	}

	// Parse the datastore path
	dsName, filePath, err := parseDatastorePath(datastorePath)
	if err != nil {
		return fmt.Errorf("invalid datastore path: %w", err)
	}

	// Find the datastore
	datastore, err := dfm.provider.finder.Datastore(ctx, dsName)
	if err != nil {
		return fmt.Errorf("failed to find datastore %s: %w", dsName, err)
	}

	// Ensure the directory exists
	dirPath := path.Dir(filePath)
	if dirPath != "." && dirPath != "/" {
		err = dfm.createDirectory(ctx, datastore, dirPath)
		if err != nil {
			dfm.provider.logger.Warn("Failed to create directory (may already exist)", "dir", dirPath, "error", err)
		}
	}

	// Construct upload URL
	dsURL := datastore.NewURL(filePath)

	// Wrap reader with progress tracking
	var uploadReader io.Reader = reader
	if progressCallback != nil {
		var transferred int64
		uploadReader = &progressTrackingReader{
			reader: reader,
			callback: func(n int) {
				transferred += int64(n)
				progressCallback(transferred, contentLength)
			},
		}
	}

	// Upload using datastore.Upload
	param := &soap.DefaultUpload
	if contentLength > 0 {
		param.ContentLength = contentLength
	}

	err = datastore.Upload(ctx, uploadReader, dsURL.Path, param)
	if err != nil {
		return fmt.Errorf("failed to upload file: %w", err)
	}

	dfm.provider.logger.Info("File uploaded to datastore", "path", datastorePath)
	return nil
}

// DeleteFile deletes a file from a datastore
func (dfm *DatastoreFileManager) DeleteFile(ctx context.Context, datastorePath string) error {
	if dfm.provider.client == nil {
		return fmt.Errorf("vSphere client not initialized")
	}

	// Parse the datastore path
	dsName, filePath, err := parseDatastorePath(datastorePath)
	if err != nil {
		return fmt.Errorf("invalid datastore path: %w", err)
	}

	// Find the datastore
	datastore, err := dfm.provider.finder.Datastore(ctx, dsName)
	if err != nil {
		return fmt.Errorf("failed to find datastore %s: %w", dsName, err)
	}

	// Get datacenter for file manager
	datacenter, err := dfm.provider.finder.DefaultDatacenter(ctx)
	if err != nil {
		return fmt.Errorf("failed to get datacenter: %w", err)
	}

	// Create file manager and delete
	fileManager := object.NewFileManager(dfm.provider.client.Client)
	task, err := fileManager.DeleteDatastoreFile(ctx, datastore.Path(filePath), datacenter)
	if err != nil {
		return fmt.Errorf("failed to start delete operation: %w", err)
	}

	err = task.Wait(ctx)
	if err != nil {
		return fmt.Errorf("delete operation failed: %w", err)
	}

	dfm.provider.logger.Info("File deleted from datastore", "path", datastorePath)
	return nil
}

// createDirectory creates a directory in the datastore
func (dfm *DatastoreFileManager) createDirectory(ctx context.Context, datastore *object.Datastore, dirPath string) error {
	// Get datacenter for file manager
	datacenter, err := dfm.provider.finder.DefaultDatacenter(ctx)
	if err != nil {
		return fmt.Errorf("failed to get datacenter: %w", err)
	}

	// Create file manager
	fileManager := object.NewFileManager(dfm.provider.client.Client)

	// Create directory
	err = fileManager.MakeDirectory(ctx, datastore.Path(dirPath), datacenter, true)
	if err != nil {
		// Ignore error if directory already exists
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	return nil
}

// parseDatastorePath parses a datastore path like "[datastore1] path/to/file"
func parseDatastorePath(datastorePath string) (datastoreName string, filePath string, err error) {
	// Check for standard format: [datastoreName] path
	if !strings.HasPrefix(datastorePath, "[") {
		return "", "", fmt.Errorf("datastore path must start with [: %s", datastorePath)
	}

	closeIdx := strings.Index(datastorePath, "]")
	if closeIdx == -1 {
		return "", "", fmt.Errorf("datastore path missing closing ]: %s", datastorePath)
	}

	datastoreName = datastorePath[1:closeIdx]
	filePath = strings.TrimSpace(datastorePath[closeIdx+1:])

	// Remove leading slash if present
	filePath = strings.TrimPrefix(filePath, "/")

	return datastoreName, filePath, nil
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
