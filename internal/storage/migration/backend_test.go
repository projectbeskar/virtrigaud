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
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestEnsurePVCBackend verifies that the legacy pvc path (and the empty
// backend, which means pvc) is accepted while every non-pvc backend is rejected
// with codes.Unimplemented (ADR-0006 Slice 0).
func TestEnsurePVCBackend(t *testing.T) {
	tests := []struct {
		name        string
		backendType string
		wantErr     bool
	}{
		{name: "empty means pvc", backendType: "", wantErr: false},
		{name: "pvc accepted", backendType: BackendPVC, wantErr: false},
		{name: "nfs unimplemented", backendType: BackendNFS, wantErr: true},
		{name: "s3 unimplemented", backendType: BackendS3, wantErr: true},
		{name: "unknown unimplemented", backendType: "gluster", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := EnsurePVCBackend(tt.backendType)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("EnsurePVCBackend(%q) = nil, want error", tt.backendType)
				}
				if got := status.Code(err); got != codes.Unimplemented {
					t.Errorf("EnsurePVCBackend(%q) code = %v, want %v", tt.backendType, got, codes.Unimplemented)
				}
				return
			}
			if err != nil {
				t.Errorf("EnsurePVCBackend(%q) = %v, want nil", tt.backendType, err)
			}
		})
	}
}

// TestPVCOnlyHelpers verifies the honest status-quo advertisement helpers used
// by every provider in ADR-0006 Slice 0.
func TestPVCOnlyHelpers(t *testing.T) {
	if got := PVCOnlyExportBackends(); len(got) != 1 || got[0] != BackendPVC {
		t.Errorf("PVCOnlyExportBackends() = %v, want [%q]", got, BackendPVC)
	}
	if got := PVCOnlyImportBackends(); len(got) != 1 || got[0] != BackendPVC {
		t.Errorf("PVCOnlyImportBackends() = %v, want [%q]", got, BackendPVC)
	}
	if got := RelayOnlyTransferModes(); len(got) != 1 || got[0] != TransferModeRelay {
		t.Errorf("RelayOnlyTransferModes() = %v, want [%q]", got, TransferModeRelay)
	}
}
