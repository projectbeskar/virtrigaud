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

package grantverify

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// KindRow is one row of the OperationKind table: the RPC a kind is sent on,
// the request message type OperationEnvelope.request is decoded as, and the
// PowerRequest.op the kind fixes (POWER_OP_UNSPECIFIED when it fixes none).
type KindRow struct {
	// Kind is the operation kind.
	Kind providerv1.OperationKind
	// FullMethod is the full gRPC method name of the RPC, for example
	// "/provider.v1.Provider/Create".
	FullMethod string
	// FixedPowerOp is the PowerRequest.op the kind requires, or
	// POWER_OP_UNSPECIFIED when the kind fixes no request field.
	FixedPowerOp providerv1.PowerOp
	// newRequest returns an empty request message of the kind's type.
	newRequest func() proto.Message
}

// NewRequest returns an empty request message of the type the kind names.
// A zero KindRow (for example the result of a RowFor whose ok was ignored)
// has no type; NewRequest then returns nil.
func (r KindRow) NewRequest() proto.Message {
	if r.newRequest == nil {
		return nil
	}
	return r.newRequest()
}

// RequestType is the full name of the request message type the kind names,
// for example "provider.v1.CreateRequest".
func (r KindRow) RequestType() protoreflect.FullName {
	if r.newRequest == nil {
		return ""
	}
	return r.newRequest().ProtoReflect().Descriptor().FullName()
}

// kindTable is the OperationKind table (ADR-0016 A2, "Grant bound to the
// request type"). Every OperationKind value except UNSPECIFIED has exactly one
// row; kinds_test.go pins it to the proto descriptors.
var kindTable = []KindRow{
	{providerv1.OperationKind_OPERATION_KIND_CREATE, providerv1.Provider_Create_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.CreateRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_DELETE, providerv1.Provider_Delete_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.DeleteRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_POWER_ON, providerv1.Provider_Power_FullMethodName, providerv1.PowerOp_POWER_OP_ON,
		func() proto.Message { return &providerv1.PowerRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_POWER_OFF, providerv1.Provider_Power_FullMethodName, providerv1.PowerOp_POWER_OP_OFF,
		func() proto.Message { return &providerv1.PowerRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_REBOOT, providerv1.Provider_Power_FullMethodName, providerv1.PowerOp_POWER_OP_REBOOT,
		func() proto.Message { return &providerv1.PowerRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL, providerv1.Provider_Power_FullMethodName, providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL,
		func() proto.Message { return &providerv1.PowerRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_RECONFIGURE, providerv1.Provider_Reconfigure_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.ReconfigureRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_HARDWARE_UPGRADE, providerv1.Provider_HardwareUpgrade_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.HardwareUpgradeRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_SNAPSHOT_CREATE, providerv1.Provider_SnapshotCreate_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.SnapshotCreateRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_SNAPSHOT_DELETE, providerv1.Provider_SnapshotDelete_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.SnapshotDeleteRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_SNAPSHOT_REVERT, providerv1.Provider_SnapshotRevert_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.SnapshotRevertRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_CLONE, providerv1.Provider_Clone_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.CloneRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_IMAGE_PREPARE, providerv1.Provider_ImagePrepare_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.ImagePrepareRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_EXPORT, providerv1.Provider_ExportDisk_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.ExportDiskRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_IMPORT, providerv1.Provider_ImportDisk_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.ImportDiskRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_DELETE_STAGED_DISK, providerv1.Provider_DeleteStagedDisk_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.DeleteStagedDiskRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_DELETE_LANDED_DISK, providerv1.Provider_DeleteLandedDisk_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.DeleteLandedDiskRequest{} }},
	{providerv1.OperationKind_OPERATION_KIND_ADOPT, providerv1.Provider_TransferOwner_FullMethodName, providerv1.PowerOp_POWER_OP_UNSPECIFIED,
		func() proto.Message { return &providerv1.TransferOwnerRequest{} }},
}

// Rows returns a copy of the OperationKind table, in kind order.
func Rows() []KindRow {
	out := make([]KindRow, len(kindTable))
	copy(out, kindTable)
	return out
}

// RowFor returns the table row of kind. ok is false for
// OPERATION_KIND_UNSPECIFIED and for any value the table does not implement.
func RowFor(kind providerv1.OperationKind) (row KindRow, ok bool) {
	for _, r := range kindTable {
		if r.Kind == kind {
			return r, true
		}
	}
	return KindRow{}, false
}

// KindFor returns the operation kind of a request message that arrives on
// fullMethod: the one row whose RPC is fullMethod, whose request type is
// req's type and whose fixed PowerRequest.op (if any) equals req's. It fails
// for a read RPC, a type that is not the RPC's, and a PowerRequest whose op
// no kind names.
func KindFor(fullMethod string, req proto.Message) (providerv1.OperationKind, error) {
	if req == nil {
		return providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED, fmt.Errorf("operation kind for %s: no request", fullMethod)
	}
	reqType := req.ProtoReflect().Descriptor().FullName()
	for _, r := range kindTable {
		if r.FullMethod != fullMethod || r.RequestType() != reqType {
			continue
		}
		if r.FixedPowerOp != providerv1.PowerOp_POWER_OP_UNSPECIFIED {
			pr, ok := req.(*providerv1.PowerRequest)
			if !ok || pr.GetOp() != r.FixedPowerOp {
				continue
			}
		}
		return r.Kind, nil
	}
	return providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED,
		fmt.Errorf("operation kind for %s with request %s: no operation kind names it", fullMethod, reqType)
}
