/*
Copyright 2026.

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

package mock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestDeleteStagedDisk_UnimplementedAndNotAdvertised: the mock stages only to
// pvc, so it advertises no staged-disk delete backend and answers a valid
// request Unimplemented (ADR-0011 D7); a malformed one is InvalidArgument.
func TestDeleteStagedDisk_UnimplementedAndNotAdvertised(t *testing.T) {
	p := NewProvider()
	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.Empty(t, caps.GetSupportedStagedDiskDeleteBackends())

	req := &providerv1.DeleteStagedDiskRequest{
		Migration:          &providerv1.ObjectIdentity{Uid: "u", Namespace: "team-a", Name: "web"},
		Stage:              migration.StagingStageExport,
		BackendType:        migration.BackendS3,
		StorageOptionsJson: `{"backend":"s3","bucket":"b"}`,
	}
	_, err = p.DeleteStagedDisk(context.Background(), req)
	assert.Equal(t, codes.Unimplemented, status.Code(err))

	req.Migration.Name = "../x"
	_, err = p.DeleteStagedDisk(context.Background(), req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestDeleteLandedDisk_UnimplementedAndNotAdvertised: the mock lands no disk,
// so it does not report the landed-disk delete and answers Unimplemented (rc2
// R7).
func TestDeleteLandedDisk_UnimplementedAndNotAdvertised(t *testing.T) {
	p := NewProvider()
	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.False(t, caps.GetSupportsLandedDiskDelete())

	_, err = p.DeleteLandedDisk(context.Background(), &providerv1.DeleteLandedDiskRequest{
		TargetVm: &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"},
	})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}
