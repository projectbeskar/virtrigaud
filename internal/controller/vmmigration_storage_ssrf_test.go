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

package controller

import (
	"context"
	"strings"
	"testing"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

func s3StorageWithEndpoint(endpoint string) *infrav1beta1.MigrationStorage {
	return &infrav1beta1.MigrationStorage{
		Type: "s3",
		S3: &infrav1beta1.S3StorageConfig{
			Bucket:               "virtrigaud",
			Endpoint:             endpoint,
			CredentialsSecretRef: infrav1beta1.ObjectRef{Name: "s3-creds"},
		},
	}
}

// TestValidateStorageConfig_S3EndpointSSRF is the ADR-0006 C3 fix: validateStorageConfig
// rejects an S3 endpoint that targets a metadata/loopback/link-local address
// before the provider pod ever dials it (and presents the S3 credentials there).
// A nil StorageHostPolicy uses the deny-dangerous default, so the gate is active
// even without operator configuration.
func TestValidateStorageConfig_S3EndpointSSRF(t *testing.T) {
	r := &VMMigrationReconciler{} // nil StorageHostPolicy → deny-dangerous default
	ctx := context.Background()

	for _, ep := range []string{
		"http://169.254.169.254:9000", // cloud metadata
		"http://127.0.0.1:9000",       // loopback
		"http://[::1]:9000",           // ipv6 loopback
	} {
		err := r.validateStorageConfig(ctx, s3StorageWithEndpoint(ep))
		if err == nil {
			t.Errorf("validateStorageConfig(endpoint=%q) = nil, want SSRF rejection", ep)
			continue
		}
		if !strings.Contains(err.Error(), "not permitted") {
			t.Errorf("endpoint=%q: error = %v, want 'not permitted'", ep, err)
		}
	}

	// A private storage endpoint passes the gate.
	if err := r.validateStorageConfig(ctx, s3StorageWithEndpoint("http://172.16.56.13:9000")); err != nil {
		t.Errorf("private endpoint rejected: %v", err)
	}
}
