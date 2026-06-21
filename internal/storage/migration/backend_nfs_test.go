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

import "testing"

func TestEnsurePVCS3OrNFSBackend(t *testing.T) {
	for _, b := range []string{"", BackendPVC, BackendS3, BackendNFS} {
		if err := EnsurePVCS3OrNFSBackend(b); err != nil {
			t.Errorf("EnsurePVCS3OrNFSBackend(%q) = %v, want nil", b, err)
		}
	}
	if err := EnsurePVCS3OrNFSBackend("gluster"); err == nil {
		t.Error("EnsurePVCS3OrNFSBackend(gluster) = nil, want error")
	}
}

func TestEnsureS3OrNFSBackend(t *testing.T) {
	for _, b := range []string{BackendS3, BackendNFS} {
		if err := EnsureS3OrNFSBackend(b); err != nil {
			t.Errorf("EnsureS3OrNFSBackend(%q) = %v, want nil", b, err)
		}
	}
	// pvc and empty (legacy pvc) are NOT accepted for an S3/NFS-only provider.
	for _, b := range []string{"", BackendPVC, "gluster"} {
		if err := EnsureS3OrNFSBackend(b); err == nil {
			t.Errorf("EnsureS3OrNFSBackend(%q) = nil, want error", b)
		}
	}
}

func TestNFSBackendSets(t *testing.T) {
	if got := PVCS3AndNFSExportBackends(); len(got) != 3 || got[0] != BackendPVC || got[1] != BackendS3 || got[2] != BackendNFS {
		t.Errorf("PVCS3AndNFSExportBackends() = %v", got)
	}
	if got := PVCS3AndNFSImportBackends(); len(got) != 3 || got[2] != BackendNFS {
		t.Errorf("PVCS3AndNFSImportBackends() = %v", got)
	}
	if got := S3AndNFSExportBackends(); len(got) != 2 || got[0] != BackendS3 || got[1] != BackendNFS {
		t.Errorf("S3AndNFSExportBackends() = %v", got)
	}
	if got := S3AndNFSImportBackends(); len(got) != 2 || got[0] != BackendS3 || got[1] != BackendNFS {
		t.Errorf("S3AndNFSImportBackends() = %v", got)
	}
}
