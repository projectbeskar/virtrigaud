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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

func nfsMigration(server, export, path string) *infrav1beta1.VMMigration {
	return &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "mig", Name: "m1"},
		Spec: infrav1beta1.VMMigrationSpec{
			Storage: &infrav1beta1.MigrationStorage{
				Type: storagemigration.BackendNFS,
				NFS:  &infrav1beta1.NFSStorageConfig{Server: server, Export: export, Path: path},
			},
		},
	}
}

// TestStorageOptionsJSON_NFS verifies the controller ships the NFS coordinates
// (not S3 fields) in storage_options_json for an nfs migration.
func TestStorageOptionsJSON_NFS(t *testing.T) {
	raw, err := storageOptionsJSON(nfsMigration("omv.lab", "/export/virtrigaud", "sub"))
	if err != nil {
		t.Fatal(err)
	}
	opts, err := storagemigration.ParseStorageOptions(raw)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Backend != storagemigration.BackendNFS || opts.Server != "omv.lab" ||
		opts.Export != "/export/virtrigaud" || opts.Path != "sub" {
		t.Errorf("unexpected nfs storage options: %+v", opts)
	}
}

// TestGenerateStorageURL_NFS verifies the controller builds the hardened nfs://
// staging URL for an nfs migration. The staging object is a FLAT filename in the
// export root (not a nested key): NFS requires every parent directory to pre-exist
// and qemu-img/libnfs cannot mkdir, so a nested key fails the mount with
// MNT3ERR_NOENT (ADR-0006 Slice 4, lab-surfaced). ns/name/stage are encoded into
// the single filename.
func TestGenerateStorageURL_NFS(t *testing.T) {
	r := &VMMigrationReconciler{}
	got, err := r.generateStorageURL(context.Background(), nfsMigration("172.16.56.13", "/export/virtrigaud", ""), "export")
	if err != nil {
		t.Fatal(err)
	}
	if want := "nfs://172.16.56.13/export/virtrigaud/vmmigrations-mig-m1-export.qcow2"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// The staged object must be a single path segment under the export — no '/'
	// after the export root — or NFS cannot create it without a pre-made directory.
	if strings.Count(strings.TrimPrefix(got, "nfs://172.16.56.13/export/virtrigaud/"), "/") != 0 {
		t.Errorf("nfs staging key must be flat (no nested dirs), got %q", got)
	}
}

// TestValidateStorageConfig_NFS verifies nfs validation: coords required, and the
// server runs through the same SSRF host gate as the S3 endpoint (ADR-0006 C3).
func TestValidateStorageConfig_NFS(t *testing.T) {
	r := &VMMigrationReconciler{} // nil StorageHostPolicy → deny-dangerous default
	ctx := context.Background()

	if err := r.validateStorageConfig(ctx, nfsMigration("172.16.56.13", "/export/virtrigaud", "").Spec.Storage); err != nil {
		t.Errorf("valid nfs config rejected: %v", err)
	}
	if err := r.validateStorageConfig(ctx, nfsMigration("172.16.56.13", "", "").Spec.Storage); err == nil {
		t.Error("nfs config missing export accepted, want rejection")
	}
	err := r.validateStorageConfig(ctx, nfsMigration("169.254.169.254", "/export/virtrigaud", "").Spec.Storage)
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("SSRF nfs server: err=%v, want 'not permitted'", err)
	}
}
