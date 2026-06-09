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

package libvirt

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestBuildSnapshotCreateArgs verifies that a memory-inclusive (full system)
// snapshot omits --disk-only and is only chosen when the domain is running,
// while every other combination yields a --disk-only snapshot (#202).
func TestBuildSnapshotCreateArgs(t *testing.T) {
	const vm, name, desc = "vm-1", "snap-1", "a snapshot"
	base := []string{"snapshot-create-as", vm, name, "--description", desc, "--atomic"}

	tests := []struct {
		name          string
		includeMemory bool
		running       bool
		wantMemory    bool
		wantDiskOnly  bool
	}{
		{"memory + running -> full system snapshot", true, true, true, false},
		{"memory + stopped -> disk-only (no RAM to capture)", true, false, false, true},
		{"no-memory + running -> disk-only", false, true, false, true},
		{"no-memory + stopped -> disk-only", false, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, mem := buildSnapshotCreateArgs(vm, name, desc, tt.includeMemory, tt.running)
			assert.Equal(t, tt.wantMemory, mem)
			// Base args always present, in order.
			require.GreaterOrEqual(t, len(args), len(base))
			assert.Equal(t, base, args[:len(base)])
			hasDiskOnly := false
			for _, a := range args {
				if a == "--disk-only" {
					hasDiskOnly = true
				}
			}
			assert.Equal(t, tt.wantDiskOnly, hasDiskOnly,
				"--disk-only must be present iff this is not a memory snapshot")
		})
	}
}

// TestServer_GetCapabilities_MemorySnapshots verifies the libvirt provider now
// advertises memory snapshots honestly (the SnapshotCreate path captures RAM via
// snapshot-create-as without --disk-only for a running VM — issue #202).
func TestServer_GetCapabilities_MemorySnapshots(t *testing.T) {
	s := &Server{}
	caps, err := s.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, caps)
	assert.True(t, caps.SupportsMemorySnapshots,
		"libvirt advertises memory snapshots now that SnapshotCreate honors IncludeMemory for running VMs (#202)")
}
