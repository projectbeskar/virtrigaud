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

package common

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/projectbeskar/virtrigaud/internal/storage"
)

// GetStorageFromURL creates a storage client from a URL and optional credentials
// This is a convenience wrapper around storage.NewStorageFromURL for use in providers
func GetStorageFromURL(ctx context.Context, k8sClient client.Client, storageURL string, credentialsSecretRef *types.NamespacedName) (storage.Storage, error) {
	if storageURL == "" {
		return nil, fmt.Errorf("storage URL is required")
	}

	storageClient, err := storage.NewStorageFromURL(ctx, k8sClient, storageURL, credentialsSecretRef)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %w", err)
	}

	return storageClient, nil
}

// ProgressCallbackFunc is a function type for progress reporting
type ProgressCallbackFunc func(bytesTransferred int64, totalBytes int64)

// NoOpProgressCallback is a no-op progress callback for when progress tracking is not needed
func NoOpProgressCallback(bytesTransferred int64, totalBytes int64) {
	// No operation
}

// WrapStorageError wraps a storage error with provider context
func WrapStorageError(operation string, err error) error {
	if err == nil {
		return nil
	}

	// Check if it's already a storage error
	if storageErr, ok := err.(*storage.StorageError); ok {
		return fmt.Errorf("%s failed: %w", operation, storageErr)
	}

	return fmt.Errorf("%s failed: %w", operation, err)
}

// IsRetryableStorageError determines if a storage error is retryable
func IsRetryableStorageError(err error) bool {
	if err == nil {
		return false
	}

	storageErr, ok := err.(*storage.StorageError)
	if !ok {
		// Unknown errors are potentially retryable
		return true
	}

	// Determine which error types are retryable
	switch storageErr.Type {
	case storage.ErrorTypeNetworkError:
		return true
	case storage.ErrorTypeOperationFailed:
		return true
	case storage.ErrorTypeNotFound:
		return false
	case storage.ErrorTypePermissionDenied:
		return false
	case storage.ErrorTypeChecksumMismatch:
		return false
	case storage.ErrorTypeInvalidConfig:
		return false
	default:
		return true
	}
}

