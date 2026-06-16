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

package migration

import (
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/storage"
)

// S3StorageConfigFromRequest builds a storage.StorageConfig for the s3 backend
// from a provider's ExportDisk/ImportDisk request fields: the non-secret options
// carried in storage_options_json and the credentials carried in the credentials
// map. It is shared by every provider that implements the S3 relay path so the
// JSON shape and credential key spelling never drift (ADR-0006 Slice 1).
//
// Credential values are copied into the returned config and must be handled as
// secret material by the caller; this helper never logs them.
func S3StorageConfigFromRequest(storageOptionsJSON string, creds map[string]string) (storage.StorageConfig, error) {
	opts, err := ParseStorageOptions(storageOptionsJSON)
	if err != nil {
		return storage.StorageConfig{}, err
	}
	if opts.Bucket == "" {
		return storage.StorageConfig{}, fmt.Errorf("s3 storage options missing bucket")
	}

	accessKey := creds[CredKeyAccessKeyID]
	secretKey := creds[CredKeySecretAccessKey]
	if accessKey == "" || secretKey == "" {
		return storage.StorageConfig{}, fmt.Errorf(
			"s3 credentials missing keys %q/%q", CredKeyAccessKeyID, CredKeySecretAccessKey)
	}

	return storage.StorageConfig{
		Type: BackendS3,
		S3: &storage.S3Config{
			Endpoint:        opts.Endpoint,
			Region:          opts.Region,
			Bucket:          opts.Bucket,
			UsePathStyle:    opts.UsePathStyle,
			AccessKeyID:     accessKey,
			SecretAccessKey: secretKey,
			SessionToken:    creds[CredKeySessionToken],
		},
	}, nil
}
