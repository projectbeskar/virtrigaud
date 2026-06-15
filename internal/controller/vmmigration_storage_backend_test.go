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
	"strings"
	"testing"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// providerWithCaps builds a Provider CR with the given reported export/import
// backends and transfer modes. Nil slices leave ReportedCapabilities populated
// but empty, exercising the implicit pvc-only / relay-only default path.
func providerWithCaps(name string, exportBackends, importBackends, transferModes []string) *infrav1beta1.Provider {
	p := &infrav1beta1.Provider{}
	p.Name = name
	p.Status.ReportedCapabilities = &infrav1beta1.ReportedCapabilities{
		SupportedExportBackends: exportBackends,
		SupportedImportBackends: importBackends,
		SupportedTransferModes:  transferModes,
	}
	return p
}

func migrationWithStorage(storage *infrav1beta1.MigrationStorage) *infrav1beta1.VMMigration {
	return &infrav1beta1.VMMigration{Spec: infrav1beta1.VMMigrationSpec{Storage: storage}}
}

// s3MigrationStorage builds a minimal valid s3 VMMigration for gate tests (the
// gate inspects type/transferMode, not the s3 sub-fields).
func s3MigrationStorage(mode string) *infrav1beta1.VMMigration {
	return migrationWithStorage(&infrav1beta1.MigrationStorage{
		Type:         "s3",
		TransferMode: mode,
		S3: &infrav1beta1.S3StorageConfig{
			Bucket:               "virtrigaud",
			CredentialsSecretRef: infrav1beta1.ObjectRef{Name: "s3-creds"},
		},
	})
}

// TestGateMigrationStorageBackend covers the ADR-0006 Validating-phase gate:
// nil/empty/pvc storage proceeds; nfs is rejected as unimplemented; s3 proceeds
// ONLY when the source advertises s3 export AND the target advertises s3 import
// (per-direction, Slice 1); an explicit direct mode is rejected; and a
// backend/mode not advertised by a provider is rejected naming that provider.
func TestGateMigrationStorageBackend(t *testing.T) {
	r := &VMMigrationReconciler{}

	pvcSrc := providerWithCaps("src", []string{"pvc"}, []string{"pvc"}, []string{"relay"})
	pvcTgt := providerWithCaps("tgt", []string{"pvc"}, []string{"pvc"}, []string{"relay"})

	// Slice 1 directional shape: vSphere (SOURCE) exports pvc+s3 but imports
	// pvc-only; libvirt (TARGET) imports pvc+s3 but exports pvc-only.
	vsphereSrc := providerWithCaps("vsphere", []string{"pvc", "s3"}, []string{"pvc"}, []string{"relay"})
	libvirtTgt := providerWithCaps("libvirt", []string{"pvc"}, []string{"pvc", "s3"}, []string{"relay"})

	tests := []struct {
		name        string
		migration   *infrav1beta1.VMMigration
		source      *infrav1beta1.Provider
		target      *infrav1beta1.Provider
		wantOK      bool     // wantOK == true means "" (proceed)
		wantContain []string // substrings the failure message must contain
	}{
		{
			name:      "nil storage defaults to pvc and proceeds",
			migration: migrationWithStorage(nil),
			source:    pvcSrc, target: pvcTgt,
			wantOK: true,
		},
		{
			name:      "explicit pvc proceeds",
			migration: migrationWithStorage(&infrav1beta1.MigrationStorage{Type: "pvc"}),
			source:    pvcSrc, target: pvcTgt,
			wantOK: true,
		},
		{
			name:      "empty type proceeds (implicit pvc)",
			migration: migrationWithStorage(&infrav1beta1.MigrationStorage{Type: ""}),
			source:    pvcSrc, target: pvcTgt,
			wantOK: true,
		},
		{
			name:      "s3 vSphere→libvirt relay proceeds (Slice 1)",
			migration: s3MigrationStorage("relay"),
			source:    vsphereSrc, target: libvirtTgt,
			wantOK: true,
		},
		{
			name:      "s3 vSphere→libvirt auto proceeds (resolves to relay)",
			migration: s3MigrationStorage("auto"),
			source:    vsphereSrc, target: libvirtTgt,
			wantOK: true,
		},
		{
			name:      "s3 reverse direction rejected (libvirt can't export s3)",
			migration: s3MigrationStorage("relay"),
			source:    libvirtTgt, target: vsphereSrc, // swapped: libvirt as source, vsphere as target
			wantOK:      false,
			wantContain: []string{"source provider", `"s3"`, "ADR-0006"},
		},
		{
			name:      "s3 rejected when target cannot import s3",
			migration: s3MigrationStorage("relay"),
			source:    vsphereSrc, target: pvcTgt, // target imports pvc-only
			wantOK:      false,
			wantContain: []string{"target provider", `"s3"`, "ADR-0006"},
		},
		{
			name:      "s3 explicit direct mode rejected (Slice 1 relay-only)",
			migration: s3MigrationStorage("direct"),
			source:    vsphereSrc, target: libvirtTgt,
			wantOK:      false,
			wantContain: []string{"transfer mode", `"direct"`, "ADR-0006"},
		},
		{
			name:      "s3 rejected when providers advertise pvc-only",
			migration: s3MigrationStorage("relay"),
			source:    pvcSrc, target: pvcTgt,
			wantOK:      false,
			wantContain: []string{"source provider", `"s3"`},
		},
		{
			name:      "nfs rejected",
			migration: migrationWithStorage(&infrav1beta1.MigrationStorage{Type: "nfs"}),
			source:    pvcSrc, target: pvcTgt,
			wantOK:      false,
			wantContain: []string{`"nfs"`, "ADR-0006"},
		},
		{
			name:        "pvc rejected when source does not advertise pvc export",
			migration:   migrationWithStorage(&infrav1beta1.MigrationStorage{Type: "pvc"}),
			source:      providerWithCaps("src", []string{"nfs"}, []string{"pvc"}, []string{"relay"}),
			target:      pvcTgt,
			wantOK:      false,
			wantContain: []string{"source provider", `"src"`, "pvc", "ADR-0006"},
		},
		{
			name:        "pvc rejected when target does not advertise pvc import",
			migration:   migrationWithStorage(&infrav1beta1.MigrationStorage{Type: "pvc"}),
			source:      pvcSrc,
			target:      providerWithCaps("tgt", []string{"pvc"}, []string{"nfs"}, []string{"relay"}),
			wantOK:      false,
			wantContain: []string{"target provider", `"tgt"`, "ADR-0006"},
		},
		{
			name: "explicit relay mode proceeds when both advertise relay",
			migration: migrationWithStorage(&infrav1beta1.MigrationStorage{
				Type: "pvc", TransferMode: "relay",
			}),
			source: pvcSrc, target: pvcTgt,
			wantOK: true,
		},
		{
			name: "explicit direct mode rejected when not advertised",
			migration: migrationWithStorage(&infrav1beta1.MigrationStorage{
				Type: "pvc", TransferMode: "direct",
			}),
			source: pvcSrc, target: pvcTgt,
			wantOK:      false,
			wantContain: []string{"transfer mode", `"direct"`, "ADR-0006"},
		},
		{
			name: "auto mode always proceeds",
			migration: migrationWithStorage(&infrav1beta1.MigrationStorage{
				Type: "pvc", TransferMode: "auto",
			}),
			source: pvcSrc, target: pvcTgt,
			wantOK: true,
		},
		{
			name:      "empty reported caps treated as implicit pvc/relay default",
			migration: migrationWithStorage(&infrav1beta1.MigrationStorage{Type: "pvc"}),
			source:    providerWithCaps("src", nil, nil, nil),
			target:    providerWithCaps("tgt", nil, nil, nil),
			wantOK:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := r.gateMigrationStorageBackend(tt.migration, tt.source, tt.target)
			if tt.wantOK {
				if msg != "" {
					t.Fatalf("gateMigrationStorageBackend() = %q, want \"\" (proceed)", msg)
				}
				return
			}
			if msg == "" {
				t.Fatalf("gateMigrationStorageBackend() = \"\", want a failure message")
			}
			for _, want := range tt.wantContain {
				if !strings.Contains(msg, want) {
					t.Errorf("failure message %q does not contain %q", msg, want)
				}
			}
		})
	}
}
