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

// TestGateMigrationStorageBackend covers the ADR-0006 Slice 0 Validating-phase
// gate: nil/empty/pvc storage proceeds; s3/nfs are rejected with an actionable,
// ADR-referencing message; and a backend/mode not advertised by a provider is
// rejected naming that provider and its supported set.
func TestGateMigrationStorageBackend(t *testing.T) {
	r := &VMMigrationReconciler{}

	pvcSrc := providerWithCaps("src", []string{"pvc"}, []string{"pvc"}, []string{"relay"})
	pvcTgt := providerWithCaps("tgt", []string{"pvc"}, []string{"pvc"}, []string{"relay"})

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
			name:      "s3 rejected with ADR-referencing message",
			migration: migrationWithStorage(&infrav1beta1.MigrationStorage{Type: "s3"}),
			source:    pvcSrc, target: pvcTgt,
			wantOK:      false,
			wantContain: []string{`"s3"`, "ADR-0006"},
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
