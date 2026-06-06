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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestServer_Clone_ReturnsUnimplemented verifies that the libvirt Clone RPC
// returns a gRPC Unimplemented status rather than a fabricated task reference.
// Clone is not implemented for libvirt (issue #153); returning Unimplemented is
// the honest contract until a real volume-clone implementation exists.
func TestServer_Clone_ReturnsUnimplemented(t *testing.T) {
	s := &Server{}

	resp, err := s.Clone(context.Background(), &providerv1.CloneRequest{
		SourceVmId: "vm-source",
		TargetName: "vm-target",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unimplemented, status.Code(err),
		"Clone must report Unimplemented so callers do not treat a no-op as success")
}

// TestServer_ImagePrepare_ReturnsUnimplemented verifies that the libvirt
// ImagePrepare RPC returns a gRPC Unimplemented status rather than a fabricated
// task reference. ImagePrepare is not implemented for libvirt (issue #154).
func TestServer_ImagePrepare_ReturnsUnimplemented(t *testing.T) {
	s := &Server{}

	resp, err := s.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		TargetName: "fedora-tmpl",
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, codes.Unimplemented, status.Code(err),
		"ImagePrepare must report Unimplemented so callers do not treat a no-op as success")
}

// TestServer_GetCapabilities_HonestFlags verifies that the libvirt provider
// advertises capabilities that match its actual behavior: linked clones and
// image import are reported as unsupported (issues #153/#154) while snapshots
// remain supported.
func TestServer_GetCapabilities_HonestFlags(t *testing.T) {
	s := &Server{}

	caps, err := s.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})

	require.NoError(t, err)
	require.NotNil(t, caps)
	assert.False(t, caps.SupportsLinkedClones,
		"libvirt must not advertise linked clones while Clone is unimplemented (issue #153)")
	assert.False(t, caps.SupportsImageImport,
		"libvirt must not advertise image import while ImagePrepare is unimplemented (issue #154)")
	assert.True(t, caps.SupportsSnapshots,
		"libvirt snapshots are implemented and must remain advertised")
}
