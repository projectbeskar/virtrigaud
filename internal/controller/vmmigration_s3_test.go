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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

func s3Migration(ns, name, mode string) *infrav1beta1.VMMigration {
	return &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infrav1beta1.VMMigrationSpec{
			Storage: &infrav1beta1.MigrationStorage{
				Type:         "s3",
				TransferMode: mode,
				S3: &infrav1beta1.S3StorageConfig{
					Bucket:               "virtrigaud",
					Endpoint:             "http://rustfs.lab.k8:9000",
					Region:               "us-east-1",
					Prefix:               "prod/",
					UsePathStyle:         true,
					CredentialsSecretRef: infrav1beta1.ObjectRef{Name: "s3-creds"},
				},
			},
		},
	}
}

// TestResolveTransferMode verifies auto collapses to relay (Slice 1) and an
// explicit relay passes through.
func TestResolveTransferMode(t *testing.T) {
	if got := resolveTransferMode(s3Migration("ns", "m", "auto")); got != storagemigration.TransferModeRelay {
		t.Errorf("resolveTransferMode(auto) = %q, want relay", got)
	}
	if got := resolveTransferMode(s3Migration("ns", "m", "relay")); got != storagemigration.TransferModeRelay {
		t.Errorf("resolveTransferMode(relay) = %q, want relay", got)
	}
	// Unset storage defaults to auto → relay.
	if got := resolveTransferMode(&infrav1beta1.VMMigration{}); got != storagemigration.TransferModeRelay {
		t.Errorf("resolveTransferMode(unset) = %q, want relay", got)
	}
}

// TestGenerateStorageURLS3 verifies the s3:// URL is built from bucket+prefix and
// stages a vmdk (the SOURCE's native format, ADR D4).
func TestGenerateStorageURLS3(t *testing.T) {
	r := &VMMigrationReconciler{}
	m := s3Migration("ns1", "mig1", "auto")

	url, err := r.generateStorageURL(context.Background(), m, "export")
	if err != nil {
		t.Fatalf("generateStorageURL: %v", err)
	}
	want := "s3://virtrigaud/prod/vmmigrations/ns1/mig1/export.vmdk"
	if url != want {
		t.Errorf("generateStorageURL = %q, want %q", url, want)
	}

	// No prefix → no leading slash in the key.
	m.Spec.Storage.S3.Prefix = ""
	url, _ = r.generateStorageURL(context.Background(), m, "export")
	if want := "s3://virtrigaud/vmmigrations/ns1/mig1/export.vmdk"; url != want {
		t.Errorf("generateStorageURL (no prefix) = %q, want %q", url, want)
	}
}

// TestLoadS3CredentialsFromSecret verifies credentials are read from the
// referenced Secret in the migration's namespace and mapped to the ADR-0006
// credential keys, including the optional session token.
func TestLoadS3CredentialsFromSecret(t *testing.T) {
	s := runtime.NewScheme()
	if err := infrav1beta1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: "ns1"},
		Data: map[string][]byte{
			storagemigration.CredKeyAccessKeyID:     []byte("AKIAEXAMPLE"),
			storagemigration.CredKeySecretAccessKey: []byte("topsecret"),
			storagemigration.CredKeySessionToken:    []byte("sess-tok"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	r := &VMMigrationReconciler{Client: c}

	creds, err := r.loadS3Credentials(context.Background(), s3Migration("ns1", "mig1", "auto"))
	if err != nil {
		t.Fatalf("loadS3Credentials: %v", err)
	}
	if creds[storagemigration.CredKeyAccessKeyID] != "AKIAEXAMPLE" ||
		creds[storagemigration.CredKeySecretAccessKey] != "topsecret" ||
		creds[storagemigration.CredKeySessionToken] != "sess-tok" {
		t.Errorf("creds not mapped correctly: %+v", creds)
	}

	// Missing required keys must error.
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: "ns2"},
		Data:       map[string][]byte{storagemigration.CredKeyAccessKeyID: []byte("AKIA")},
	}
	c2 := fake.NewClientBuilder().WithScheme(s).WithObjects(bad).Build()
	r2 := &VMMigrationReconciler{Client: c2}
	if _, err := r2.loadS3Credentials(context.Background(), s3Migration("ns2", "mig2", "auto")); err == nil {
		t.Error("expected error for secret missing secretAccessKey")
	}

	// pvc backend returns an empty map and no error (no Secret read).
	pvcMig := &infrav1beta1.VMMigration{Spec: infrav1beta1.VMMigrationSpec{
		Storage: &infrav1beta1.MigrationStorage{Type: "pvc"},
	}}
	if got, err := r.loadS3Credentials(context.Background(), pvcMig); err != nil || len(got) != 0 {
		t.Errorf("loadS3Credentials(pvc) = (%v,%v), want empty,nil", got, err)
	}
}

// TestS3StorageOptionsJSON verifies the non-secret options JSON carries the
// endpoint/bucket/region/prefix/path-style and never any credential material.
func TestS3StorageOptionsJSON(t *testing.T) {
	raw, err := storageOptionsJSON(s3Migration("ns", "m", "auto"))
	if err != nil {
		t.Fatalf("storageOptionsJSON: %v", err)
	}
	opts, err := storagemigration.ParseStorageOptions(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.Bucket != "virtrigaud" || opts.Endpoint != "http://rustfs.lab.k8:9000" ||
		opts.Region != "us-east-1" || opts.Prefix != "prod/" || !opts.UsePathStyle {
		t.Errorf("options not built correctly: %+v", opts)
	}
	// No credential material may appear in the options JSON.
	for _, banned := range []string{"accessKey", "secretAccess", "sessionToken", "AKIA"} {
		if strings.Contains(raw, banned) {
			t.Errorf("storage options JSON leaked %q: %s", banned, raw)
		}
	}

	// pvc backend yields empty options.
	pvcMig := &infrav1beta1.VMMigration{Spec: infrav1beta1.VMMigrationSpec{
		Storage: &infrav1beta1.MigrationStorage{Type: "pvc"},
	}}
	if raw, err := storageOptionsJSON(pvcMig); err != nil || raw != "" {
		t.Errorf("storageOptionsJSON(pvc) = (%q,%v), want empty,nil", raw, err)
	}
}
