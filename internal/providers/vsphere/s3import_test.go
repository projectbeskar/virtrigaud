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

package vsphere

import "testing"

// TestS3ImportSubformatIsStreamOptimized locks in the Bug J fix: the ADR-0006
// Slice 2 vSphere import MUST convert to streamOptimized, because the disk is
// ingested via the vCenter NFC HttpNfcLease (vmdk.Import), which only accepts
// streamOptimized. The earlier monolithicSparse value was rejected by
// CopyVirtualDisk with "A specified parameter was not correct: fileType" for
// EVERY spec/disktype/adapter combination on lab vCenter 8.0.2 (de-risked in
// hack/slice2probe). If this constant ever regresses to monolithicSparse the
// reverse migration breaks again at the import step.
func TestS3ImportSubformatIsStreamOptimized(t *testing.T) {
	if s3ImportSubformat != "streamOptimized" {
		t.Fatalf("s3ImportSubformat = %q, want %q: the NFC lease import (vmdk.Import) only accepts streamOptimized; monolithicSparse triggers Bug J (fileType)", s3ImportSubformat, "streamOptimized")
	}
}
