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

package operation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// wantKindTable is the normative OperationKind table of
// docs/operation-grants-wire-shape.md (ADR-0016 A2), written out by hand so a
// change to kindTable must also change this test.
var wantKindTable = map[providerv1.OperationKind]struct {
	method  string
	reqType protoreflect.FullName
	op      providerv1.PowerOp
}{
	providerv1.OperationKind_OPERATION_KIND_CREATE:             {"/provider.v1.Provider/Create", "provider.v1.CreateRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_DELETE:             {"/provider.v1.Provider/Delete", "provider.v1.DeleteRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_POWER_ON:           {"/provider.v1.Provider/Power", "provider.v1.PowerRequest", providerv1.PowerOp_POWER_OP_ON},
	providerv1.OperationKind_OPERATION_KIND_POWER_OFF:          {"/provider.v1.Provider/Power", "provider.v1.PowerRequest", providerv1.PowerOp_POWER_OP_OFF},
	providerv1.OperationKind_OPERATION_KIND_REBOOT:             {"/provider.v1.Provider/Power", "provider.v1.PowerRequest", providerv1.PowerOp_POWER_OP_REBOOT},
	providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL:  {"/provider.v1.Provider/Power", "provider.v1.PowerRequest", providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL},
	providerv1.OperationKind_OPERATION_KIND_RECONFIGURE:        {"/provider.v1.Provider/Reconfigure", "provider.v1.ReconfigureRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_HARDWARE_UPGRADE:   {"/provider.v1.Provider/HardwareUpgrade", "provider.v1.HardwareUpgradeRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_SNAPSHOT_CREATE:    {"/provider.v1.Provider/SnapshotCreate", "provider.v1.SnapshotCreateRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_SNAPSHOT_DELETE:    {"/provider.v1.Provider/SnapshotDelete", "provider.v1.SnapshotDeleteRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_SNAPSHOT_REVERT:    {"/provider.v1.Provider/SnapshotRevert", "provider.v1.SnapshotRevertRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_CLONE:              {"/provider.v1.Provider/Clone", "provider.v1.CloneRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_IMAGE_PREPARE:      {"/provider.v1.Provider/ImagePrepare", "provider.v1.ImagePrepareRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_EXPORT:             {"/provider.v1.Provider/ExportDisk", "provider.v1.ExportDiskRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_IMPORT:             {"/provider.v1.Provider/ImportDisk", "provider.v1.ImportDiskRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_DELETE_STAGED_DISK: {"/provider.v1.Provider/DeleteStagedDisk", "provider.v1.DeleteStagedDiskRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_DELETE_LANDED_DISK: {"/provider.v1.Provider/DeleteLandedDisk", "provider.v1.DeleteLandedDiskRequest", 0},
	providerv1.OperationKind_OPERATION_KIND_ADOPT:              {"/provider.v1.Provider/TransferOwner", "provider.v1.TransferOwnerRequest", 0},
}

// providerService is the Provider service descriptor.
func providerService(t *testing.T) protoreflect.ServiceDescriptor {
	t.Helper()
	sd := providerv1.File_provider_v1_provider_proto.Services().ByName("Provider")
	require.NotNil(t, sd)
	return sd
}

// fullMethod renders a method descriptor as a gRPC full method name.
func fullMethod(md protoreflect.MethodDescriptor) string {
	return "/" + string(md.Parent().FullName()) + "/" + string(md.Name())
}

// TestKindTable_EqualsTheNormativeTable: kindTable is exactly the published
// table, and has one row per OperationKind value other than UNSPECIFIED.
func TestKindTable_EqualsTheNormativeTable(t *testing.T) {
	enum := providerv1.OperationKind(0).Descriptor()
	require.Equal(t, enum.Values().Len()-1, len(kindTable), "one row per non-UNSPECIFIED OperationKind value")
	seen := map[providerv1.OperationKind]bool{}
	for _, r := range Rows() {
		assert.False(t, seen[r.Kind], "%s has two rows", r.Kind)
		seen[r.Kind] = true
		want, ok := wantKindTable[r.Kind]
		require.True(t, ok, "%s is not in the normative table", r.Kind)
		assert.Equal(t, want.method, r.FullMethod, r.Kind.String())
		assert.Equal(t, want.reqType, r.RequestType(), r.Kind.String())
		assert.Equal(t, want.op, r.FixedPowerOp, r.Kind.String())
	}
	for i := 0; i < enum.Values().Len(); i++ {
		k := providerv1.OperationKind(enum.Values().Get(i).Number())
		if k == providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED {
			_, ok := RowFor(k)
			assert.False(t, ok, "UNSPECIFIED has no row")
			continue
		}
		assert.True(t, seen[k], "%s has no row", k)
	}
}

// TestKindTable_MatchesTheServiceDescriptor: each row's request type is its
// method's input type, and every RPC whose request has an operation_envelope
// field is the RPC of at least one row.
func TestKindTable_MatchesTheServiceDescriptor(t *testing.T) {
	sd := providerService(t)
	byMethod := map[string]protoreflect.MethodDescriptor{}
	for i := 0; i < sd.Methods().Len(); i++ {
		md := sd.Methods().Get(i)
		byMethod[fullMethod(md)] = md
	}
	covered := map[string]bool{}
	for _, r := range Rows() {
		md, ok := byMethod[r.FullMethod]
		require.True(t, ok, "%s: %s is not a Provider method", r.Kind, r.FullMethod)
		assert.Equal(t, md.Input().FullName(), r.RequestType(), r.Kind.String())
		covered[r.FullMethod] = true
	}
	for name, md := range byMethod {
		if md.Input().Fields().ByName(envelopeFieldName) != nil {
			assert.True(t, covered[name], "mutating RPC %s has no OperationKind", name)
		}
	}
}

// TestKindFor: a request on its RPC maps to its kind; a read RPC, a wrong
// type and a Power op no kind names do not.
func TestKindFor(t *testing.T) {
	for _, r := range Rows() {
		got, err := KindFor(r.FullMethod, testRequestOrEmpty(r))
		require.NoError(t, err, r.Kind.String())
		assert.Equal(t, r.Kind, got)
	}
	_, err := KindFor(providerv1.Provider_Describe_FullMethodName, &providerv1.DescribeRequest{})
	assert.Error(t, err)
	_, err = KindFor(providerv1.Provider_Power_FullMethodName, &providerv1.DeleteRequest{})
	assert.Error(t, err)
	_, err = KindFor(providerv1.Provider_Power_FullMethodName, &providerv1.PowerRequest{})
	assert.Error(t, err, "a PowerRequest with op unset has no kind")
	_, err = KindFor(providerv1.Provider_Delete_FullMethodName, nil)
	assert.Error(t, err)
}

// testRequestOrEmpty is a request of r's type with r's fixed op set.
func testRequestOrEmpty(r KindRow) protoMessage {
	m := r.NewRequest()
	if pr, ok := m.(*providerv1.PowerRequest); ok {
		pr.Op = r.FixedPowerOp
	}
	return m
}
