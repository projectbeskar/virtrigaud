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
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ParsedStorageURL contains parsed components of a storage URL
type ParsedStorageURL struct {
	Type     string // s3, http, https, nfs
	Bucket   string // For S3: bucket name
	Path     string // Object path within storage
	Endpoint string // Full endpoint URL (for HTTP/HTTPS)
	Host     string // Hostname (for HTTP/S3)
	Region   string // AWS region (for S3)
}

// ParseStorageURL parses a storage URL and returns its components
func ParseStorageURL(storageURL string) (*ParsedStorageURL, error) {
	if storageURL == "" {
		return nil, fmt.Errorf("storage URL is empty")
	}

	parsed, err := url.Parse(storageURL)
	if err != nil {
		return nil, fmt.Errorf("invalid storage URL: %w", err)
	}

	result := &ParsedStorageURL{}

	switch strings.ToLower(parsed.Scheme) {
	case "s3":
		// Format: s3://bucket/path/to/object
		result.Type = "s3"
		result.Bucket = parsed.Host
		result.Path = strings.TrimPrefix(parsed.Path, "/")
		if result.Bucket == "" {
			return nil, fmt.Errorf("S3 URL must specify bucket: %s", storageURL)
		}

	case "http", "https":
		// Format: http://host:port/path or https://host/path
		result.Type = parsed.Scheme
		result.Endpoint = fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
		result.Host = parsed.Host
		result.Path = strings.TrimPrefix(parsed.Path, "/")

	case "nfs":
		// Format: nfs://path/to/file (path is relative to NFS mount)
		result.Type = "nfs"
		// For NFS, the entire path is used (host part + path part)
		if parsed.Host != "" {
			result.Path = parsed.Host + parsed.Path
		} else {
			result.Path = strings.TrimPrefix(parsed.Path, "/")
		}

	default:
		return nil, fmt.Errorf("unsupported storage scheme: %s (supported: s3, http, https, nfs)", parsed.Scheme)
	}

	return result, nil
}

// CredentialsConfig contains storage credentials from Kubernetes secret
type CredentialsConfig struct {
	AccessKey string
	SecretKey string
	Token     string
	Endpoint  string
	Region    string
	UseSSL    bool
}

// LoadCredentialsFromSecret loads storage credentials from a Kubernetes secret
func LoadCredentialsFromSecret(ctx context.Context, k8sClient client.Client, secretRef types.NamespacedName) (*CredentialsConfig, error) {
	if secretRef.Name == "" {
		return nil, fmt.Errorf("secret name is empty")
	}

	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, secretRef, secret); err != nil {
		return nil, fmt.Errorf("failed to get credentials secret: %w", err)
	}

	config := &CredentialsConfig{
		UseSSL: true, // Default to SSL
	}

	// S3/MinIO credentials
	if accessKey, ok := secret.Data["accessKey"]; ok {
		config.AccessKey = string(accessKey)
	}
	if secretKey, ok := secret.Data["secretKey"]; ok {
		config.SecretKey = string(secretKey)
	}
	if token, ok := secret.Data["token"]; ok {
		config.Token = string(token)
	}
	if endpoint, ok := secret.Data["endpoint"]; ok {
		config.Endpoint = string(endpoint)
	}
	if region, ok := secret.Data["region"]; ok {
		config.Region = string(region)
	}
	if useSSL, ok := secret.Data["useSSL"]; ok {
		config.UseSSL = string(useSSL) != "false"
	}

	return config, nil
}

// NewStorageFromURL creates a Storage instance from a URL and optional credentials
func NewStorageFromURL(ctx context.Context, k8sClient client.Client, storageURL string, credentialsSecretRef *types.NamespacedName) (Storage, error) {
	parsed, err := ParseStorageURL(storageURL)
	if err != nil {
		return nil, err
	}

	var creds *CredentialsConfig
	if credentialsSecretRef != nil && credentialsSecretRef.Name != "" {
		creds, err = LoadCredentialsFromSecret(ctx, k8sClient, *credentialsSecretRef)
		if err != nil {
			return nil, fmt.Errorf("failed to load credentials: %w", err)
		}
	}

	config := StorageConfig{
		Type:     parsed.Type,
		Timeout:  300, // 5 minutes default
		UseSSL:   true,
	}

	switch parsed.Type {
	case "s3":
		config.Bucket = parsed.Bucket
		if creds != nil {
			config.AccessKey = creds.AccessKey
			config.SecretKey = creds.SecretKey
			config.Token = creds.Token
			config.UseSSL = creds.UseSSL
			if creds.Endpoint != "" {
				config.Endpoint = creds.Endpoint
			}
			if creds.Region != "" {
				config.Region = creds.Region
			}
		}
		// Default region if not specified
		if config.Region == "" {
			config.Region = "us-east-1"
		}

	case "http", "https":
		config.Endpoint = parsed.Endpoint
		if creds != nil && creds.Token != "" {
			config.Token = creds.Token
		}

	case "nfs":
		// For NFS, endpoint is the mount path (from credentials or parsed)
		if creds != nil && creds.Endpoint != "" {
			config.Endpoint = creds.Endpoint
		} else {
			// Extract base path from URL (everything before the last component)
			lastSlash := strings.LastIndex(parsed.Path, "/")
			if lastSlash > 0 {
				config.Endpoint = parsed.Path[:lastSlash]
			} else {
				config.Endpoint = "/"
			}
		}
	}

	return NewStorage(config)
}

