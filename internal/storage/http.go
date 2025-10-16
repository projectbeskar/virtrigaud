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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HTTPStorage implements the Storage interface for HTTP/HTTPS storage
// Suitable for simple file servers and read-only scenarios
type HTTPStorage struct {
	client *http.Client
	config StorageConfig
}

// NewHTTPStorage creates a new HTTP storage backend
func NewHTTPStorage(config StorageConfig) (*HTTPStorage, error) {
	log.Printf("INFO Initializing HTTP storage: endpoint=%s", config.Endpoint)

	// Set defaults
	if config.Timeout == 0 {
		config.Timeout = 300 // 5 minutes default
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}

	// Create HTTP client
	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	if config.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	client := &http.Client{
		Timeout:   time.Duration(config.Timeout) * time.Second,
		Transport: transport,
	}

	storage := &HTTPStorage{
		client: client,
		config: config,
	}

	log.Printf("INFO HTTP storage initialized successfully")
	return storage, nil
}

// Upload uploads a file via HTTP PUT
// Note: This requires the HTTP server to support PUT requests
func (h *HTTPStorage) Upload(ctx context.Context, req UploadRequest) (UploadResponse, error) {
	log.Printf("INFO Uploading via HTTP PUT: %s", req.DestinationURL)

	// Parse and validate URL
	if err := h.ValidateURL(req.DestinationURL); err != nil {
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
	progressReader := &httpProgressReader{
		reader:   reader,
		total:    fileSize,
		callback: req.ProgressCallback,
		hasher:   sha256.New(),
	}

	// Create HTTP PUT request
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, req.DestinationURL, progressReader)
	if err != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to create HTTP request",
			Cause:   err,
		}
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	if fileSize > 0 {
		httpReq.Header.Set("Content-Length", strconv.FormatInt(fileSize, 10))
	}
	if req.Checksum != "" {
		httpReq.Header.Set("X-Checksum-SHA256", req.Checksum)
	}
	if h.config.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+h.config.Token)
	}

	// Add custom metadata as headers
	for k, v := range req.Metadata {
		httpReq.Header.Set("X-Metadata-"+k, v)
	}

	// Execute request with retry logic
	var resp *http.Response
	var lastErr error

	for attempt := 0; attempt <= h.config.MaxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("WARN Retrying upload (attempt %d/%d)", attempt+1, h.config.MaxRetries+1)
			time.Sleep(time.Duration(attempt) * 2 * time.Second) // Exponential backoff
		}

		resp, lastErr = h.client.Do(httpReq)
		if lastErr == nil && resp.StatusCode < 500 {
			break // Success or client error (4xx)
		}
		if resp != nil {
			resp.Body.Close()
		}
	}

	if lastErr != nil {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeNetworkError,
			Message: "HTTP request failed after retries",
			Cause:   lastErr,
		}
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("HTTP upload failed with status %d: %s", resp.StatusCode, string(body)),
		}
	}

	// Get checksum from hasher
	checksum := hex.EncodeToString(progressReader.hasher.Sum(nil))

	// Verify checksum if provided
	if req.Checksum != "" && req.Checksum != checksum {
		return UploadResponse{}, &StorageError{
			Type:    ErrorTypeChecksumMismatch,
			Message: fmt.Sprintf("checksum mismatch: expected %s, got %s", req.Checksum, checksum),
		}
	}

	log.Printf("INFO HTTP upload complete: %s (checksum=%s, bytes=%d)", req.DestinationURL, checksum, progressReader.transferred)

	return UploadResponse{
		URL:              req.DestinationURL,
		Checksum:         checksum,
		BytesTransferred: progressReader.transferred,
		ETag:             resp.Header.Get("ETag"),
	}, nil
}

// Download downloads a file via HTTP GET
func (h *HTTPStorage) Download(ctx context.Context, req DownloadRequest) (DownloadResponse, error) {
	log.Printf("INFO Downloading via HTTP GET: %s", req.SourceURL)

	// Parse and validate URL
	if err := h.ValidateURL(req.SourceURL); err != nil {
		return DownloadResponse{}, err
	}

	// Create HTTP GET request
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.SourceURL, nil)
	if err != nil {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to create HTTP request",
			Cause:   err,
		}
	}

	// Set headers
	if h.config.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+h.config.Token)
	}

	// Support resume if offset is specified
	if req.ResumeOffset > 0 {
		httpReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", req.ResumeOffset))
	}

	// Execute request with retry logic
	var resp *http.Response
	var lastErr error

	for attempt := 0; attempt <= h.config.MaxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("WARN Retrying download (attempt %d/%d)", attempt+1, h.config.MaxRetries+1)
			time.Sleep(time.Duration(attempt) * 2 * time.Second) // Exponential backoff
		}

		resp, lastErr = h.client.Do(httpReq)
		if lastErr == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) {
			break // Success
		}
		if resp != nil && resp.StatusCode < 500 {
			resp.Body.Close()
			break // Client error, don't retry
		}
		if resp != nil {
			resp.Body.Close()
		}
	}

	if lastErr != nil {
		return DownloadResponse{}, &StorageError{
			Type:    ErrorTypeNetworkError,
			Message: "HTTP request failed after retries",
			Cause:   lastErr,
		}
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(resp.Body)
		errorType := ErrorTypeOperationFailed
		if resp.StatusCode == http.StatusNotFound {
			errorType = ErrorTypeNotFound
		} else if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			errorType = ErrorTypePermissionDenied
		}
		return DownloadResponse{}, &StorageError{
			Type:    errorType,
			Message: fmt.Sprintf("HTTP download failed with status %d: %s", resp.StatusCode, string(body)),
		}
	}

	// Get content length
	contentLength := resp.ContentLength

	var writer io.Writer

	// Use provided writer or create file
	if req.Writer != nil {
		writer = req.Writer
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

		file, err := os.Create(req.DestinationPath)
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
	hasher := sha256.New()
	var transferred int64

	// Create a multi-writer for progress and checksum
	progressWriter := io.MultiWriter(writer, hasher)

	// Copy data with progress reporting
	buf := make([]byte, 32*1024) // 32KB buffer
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			nw, ew := progressWriter.Write(buf[0:nr])
			if nw > 0 {
				transferred += int64(nw)
				if req.ProgressCallback != nil {
					req.ProgressCallback(transferred, contentLength)
				}
			}
			if ew != nil {
				return DownloadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "failed to write data",
					Cause:   ew,
				}
			}
			if nr != nw {
				return DownloadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "short write",
				}
			}
		}
		if er != nil {
			if er != io.EOF {
				return DownloadResponse{}, &StorageError{
					Type:    ErrorTypeOperationFailed,
					Message: "failed to read data",
					Cause:   er,
				}
			}
			break
		}
	}

	// Get checksum
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

	log.Printf("INFO HTTP download complete: %s (checksum=%s, bytes=%d)", req.SourceURL, checksum, transferred)

	return DownloadResponse{
		BytesTransferred: transferred,
		Checksum:         checksum,
		ContentLength:    contentLength,
	}, nil
}

// Delete deletes a file via HTTP DELETE
// Note: This requires the HTTP server to support DELETE requests
func (h *HTTPStorage) Delete(ctx context.Context, url string) error {
	log.Printf("INFO Deleting via HTTP DELETE: %s", url)

	// Create HTTP DELETE request
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to create HTTP request",
			Cause:   err,
		}
	}

	if h.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.config.Token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return &StorageError{
			Type:    ErrorTypeNetworkError,
			Message: "HTTP DELETE request failed",
			Cause:   err,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound {
			// Already deleted, consider it success
			return nil
		}
		body, _ := io.ReadAll(resp.Body)
		return &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: fmt.Sprintf("HTTP DELETE failed with status %d: %s", resp.StatusCode, string(body)),
		}
	}

	log.Printf("INFO Deleted via HTTP: %s", url)
	return nil
}

// GetMetadata retrieves metadata via HTTP HEAD request
func (h *HTTPStorage) GetMetadata(ctx context.Context, url string) (FileMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return FileMetadata{}, &StorageError{
			Type:    ErrorTypeOperationFailed,
			Message: "failed to create HTTP request",
			Cause:   err,
		}
	}

	if h.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.config.Token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return FileMetadata{}, &StorageError{
			Type:    ErrorTypeNetworkError,
			Message: "HTTP HEAD request failed",
			Cause:   err,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return FileMetadata{}, &StorageError{
			Type:    ErrorTypeNotFound,
			Message: fmt.Sprintf("HTTP HEAD failed with status %d", resp.StatusCode),
		}
	}

	metadata := FileMetadata{
		URL:            url,
		Size:           resp.ContentLength,
		ETag:           resp.Header.Get("ETag"),
		ContentType:    resp.Header.Get("Content-Type"),
		LastModified:   resp.Header.Get("Last-Modified"),
		Checksum:       resp.Header.Get("X-Checksum-SHA256"),
		CustomMetadata: make(map[string]string),
	}

	// Extract custom metadata from X-Metadata-* headers
	for key, values := range resp.Header {
		if strings.HasPrefix(key, "X-Metadata-") && len(values) > 0 {
			metaKey := strings.TrimPrefix(key, "X-Metadata-")
			metadata.CustomMetadata[metaKey] = values[0]
		}
	}

	return metadata, nil
}

// ValidateURL checks if a URL is valid for HTTP storage
func (h *HTTPStorage) ValidateURL(urlStr string) error {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: "invalid URL format",
			Cause:   err,
		}
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return &StorageError{
			Type:    ErrorTypeInvalidConfig,
			Message: fmt.Sprintf("invalid URL scheme: %s (expected http or https)", parsed.Scheme),
		}
	}

	return nil
}

// Close closes the HTTP storage connection
func (h *HTTPStorage) Close() error {
	// Close idle connections
	h.client.CloseIdleConnections()
	return nil
}

// httpProgressReader wraps an io.Reader to track progress and calculate checksum
type httpProgressReader struct {
	reader      io.Reader
	total       int64
	transferred int64
	callback    func(int64, int64)
	hasher      hash.Hash
}

func (pr *httpProgressReader) Read(p []byte) (int, error) {
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

