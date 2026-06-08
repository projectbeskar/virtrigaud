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

package capabilities

import (
	"context"
	"reflect"
	"testing"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestBuilder_DiskMigrationCapabilities verifies the disk export/import +
// compression builder methods surface on the gRPC GetCapabilitiesResponse (#198).
// Before this, the Manager mapping omitted these fields entirely, so any provider
// using the builder (e.g. Proxmox) could not advertise disk migration even when
// the RPCs were implemented.
func TestBuilder_DiskMigrationCapabilities(t *testing.T) {
	m := NewBuilder().
		Core().
		DiskExport("qcow2", "raw", "vmdk").
		DiskImport("qcow2", "raw").
		ExportCompression().
		Build()

	resp, err := m.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if !resp.SupportsDiskExport {
		t.Error("SupportsDiskExport should be true")
	}
	if !resp.SupportsDiskImport {
		t.Error("SupportsDiskImport should be true")
	}
	if !resp.SupportsExportCompression {
		t.Error("SupportsExportCompression should be true")
	}
	if want := []string{"qcow2", "raw", "vmdk"}; !reflect.DeepEqual(resp.SupportedExportFormats, want) {
		t.Errorf("SupportedExportFormats = %v, want %v", resp.SupportedExportFormats, want)
	}
	if want := []string{"qcow2", "raw"}; !reflect.DeepEqual(resp.SupportedImportFormats, want) {
		t.Errorf("SupportedImportFormats = %v, want %v", resp.SupportedImportFormats, want)
	}
}

// TestBuilder_DiskMigrationDefaultsFalse verifies the new flags default to false
// when not advertised, so a provider that does not support disk migration is not
// over-reported.
func TestBuilder_DiskMigrationDefaultsFalse(t *testing.T) {
	m := NewBuilder().Core().Snapshots().Build()
	resp, err := m.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if resp.SupportsDiskExport || resp.SupportsDiskImport || resp.SupportsExportCompression {
		t.Error("disk export/import/compression should default to false when not advertised")
	}
	if len(resp.SupportedExportFormats) != 0 || len(resp.SupportedImportFormats) != 0 {
		t.Error("export/import format lists should be empty when not advertised")
	}
}
