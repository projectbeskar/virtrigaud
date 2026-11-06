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

package providerv1

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These tests pin the operation-grant wire shape: the messages, enums, field
// numbers and attachment points are a frozen contract, and a change to any
// of them must fail here until it has been reviewed.

const (
	envelopeFieldName     = "operation_envelope"
	envelopeMessageName   = "provider.v1.OperationEnvelope"
	timestampMessageName  = "google.protobuf.Timestamp"
	epochMessageName      = "provider.v1.Epoch"
	preconditionMsgName   = "provider.v1.GrantPrecondition"
	grantMessageName      = "provider.v1.OperationGrant"
	grantKindEnumName     = "provider.v1.GrantKind"
	operationKindEnumName = "provider.v1.OperationKind"
)

func sampleEpoch(c uint64) *Epoch {
	return &Epoch{Counter: c, Holder: bytes.Repeat([]byte{byte(c)}, 16)}
}

func sampleGrantBody() *GrantBody {
	ts := timestamppb.Now()
	return &GrantBody{
		GrantVersion:        1,
		GrantKind:           GrantKind_GRANT_KIND_OPERATION,
		InstallId:           bytes.Repeat([]byte{1}, 16),
		ProviderUid:         "provider-uid",
		HostId:              "host-a",
		EnrolmentId:         bytes.Repeat([]byte{2}, 16),
		AuthorityGeneration: 3,
		Epoch:               sampleEpoch(4),
		VmUid:               "vm-uid",
		BindingId:           bytes.Repeat([]byte{5}, 16),
		OpId:                bytes.Repeat([]byte{6}, 16),
		Kind:                OperationKind_OPERATION_KIND_POWER_ON,
		Attempt:             7,
		IntentDigest:        bytes.Repeat([]byte{8}, 32),
		DigestSalt:          bytes.Repeat([]byte{9}, 16),
		MintEpoch:           sampleEpoch(10),
		ExpectKnown:         true,
		TargetNamespace:     "ns",
		RequestUid:          "request-uid",
		Action:              "some_action",
		SpecDigest:          bytes.Repeat([]byte{11}, 32),
		SingleUseNonce:      bytes.Repeat([]byte{12}, 16),
		Precondition: &GrantPrecondition{
			StoreId:      bytes.Repeat([]byte{13}, 16),
			Seq:          14,
			Head:         bytes.Repeat([]byte{15}, 32),
			QuarantineId: bytes.Repeat([]byte{16}, 16),
			ClockFloor:   ts,
		},
		EalSeq:               17,
		ResetFloor:           ts,
		CeremonyRecordDigest: bytes.Repeat([]byte{18}, 32),
		NotAfter:             ts,
		IssuedAt:             ts,
	}
}

func sampleEnvelope(t *testing.T) *OperationEnvelope {
	t.Helper()
	body, err := proto.Marshal(sampleGrantBody())
	if err != nil {
		t.Fatalf("marshal GrantBody: %v", err)
	}
	return &OperationEnvelope{
		Request: []byte{0xff, 0x00, 0x01, 0xfe},
		Grant: &OperationGrant{
			Body:            body,
			Signature:       []byte("signature"),
			SignerCertChain: [][]byte{[]byte("leaf"), []byte("issuer")},
		},
	}
}

// roundTrip marshals m, unmarshals the bytes into a new message of the same
// type, checks that the two are equal and returns the new message.
func roundTrip[M proto.Message](t *testing.T, m M) M {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %T: %v", m, err)
	}
	out, ok := m.ProtoReflect().New().Interface().(M)
	if !ok {
		t.Fatalf("new message of %T has another type", m)
	}
	if err := proto.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal %T: %v", m, err)
	}
	if !proto.Equal(m, out) {
		t.Fatalf("%T did not round-trip: got %v, want %v", m, out, m)
	}
	return out
}

// envelopeCarrier is a mutating request message that carries an
// operation_envelope.
type envelopeCarrier interface {
	proto.Message
	GetOperationEnvelope() *OperationEnvelope
}

// mutatingRequests lists the 15 mutating request messages with the field
// number of their operation_envelope field. Each constructor sets the
// envelope when env is non-nil.
var mutatingRequests = []struct {
	number protoreflect.FieldNumber
	make   func(env *OperationEnvelope) envelopeCarrier
}{
	{12, func(e *OperationEnvelope) envelopeCarrier { return &CreateRequest{OperationEnvelope: e} }},
	{4, func(e *OperationEnvelope) envelopeCarrier { return &DeleteRequest{OperationEnvelope: e} }},
	{6, func(e *OperationEnvelope) envelopeCarrier { return &PowerRequest{OperationEnvelope: e} }},
	{5, func(e *OperationEnvelope) envelopeCarrier { return &ReconfigureRequest{OperationEnvelope: e} }},
	{4, func(e *OperationEnvelope) envelopeCarrier { return &HardwareUpgradeRequest{OperationEnvelope: e} }},
	{8, func(e *OperationEnvelope) envelopeCarrier { return &SnapshotCreateRequest{OperationEnvelope: e} }},
	{5, func(e *OperationEnvelope) envelopeCarrier { return &SnapshotDeleteRequest{OperationEnvelope: e} }},
	{5, func(e *OperationEnvelope) envelopeCarrier { return &SnapshotRevertRequest{OperationEnvelope: e} }},
	{11, func(e *OperationEnvelope) envelopeCarrier { return &CloneRequest{OperationEnvelope: e} }},
	{7, func(e *OperationEnvelope) envelopeCarrier { return &ImagePrepareRequest{OperationEnvelope: e} }},
	{13, func(e *OperationEnvelope) envelopeCarrier { return &ExportDiskRequest{OperationEnvelope: e} }},
	{14, func(e *OperationEnvelope) envelopeCarrier { return &ImportDiskRequest{OperationEnvelope: e} }},
	{9, func(e *OperationEnvelope) envelopeCarrier { return &DeleteStagedDiskRequest{OperationEnvelope: e} }},
	{5, func(e *OperationEnvelope) envelopeCarrier { return &DeleteLandedDiskRequest{OperationEnvelope: e} }},
	{6, func(e *OperationEnvelope) envelopeCarrier { return &TransferOwnerRequest{OperationEnvelope: e} }},
}

func TestOperationIdentity_MessagesRoundTrip(t *testing.T) {
	ts := timestamppb.Now()
	for _, m := range []proto.Message{
		sampleEnvelope(t),
		sampleEnvelope(t).GetGrant(),
		sampleGrantBody(),
		sampleEpoch(42),
		&GrantPrecondition{
			StoreId: []byte{1}, Seq: 2, Head: []byte{3}, QuarantineId: []byte{4}, ClockFloor: ts,
		},
	} {
		roundTrip(t, m)
	}
}

func TestOperationIdentity_GrantBodyEveryFieldSet(t *testing.T) {
	// Guards the round-trip test: a new GrantBody field left at its zero value
	// in sampleGrantBody would not be exercised.
	r := sampleGrantBody().ProtoReflect()
	fields := r.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		if fd := fields.Get(i); !r.Has(fd) {
			t.Errorf("sampleGrantBody leaves %s unset", fd.Name())
		}
	}
}

func TestOperationIdentity_RequestsCarryEnvelope(t *testing.T) {
	for _, rq := range mutatingRequests {
		empty := rq.make(nil)
		name := empty.ProtoReflect().Descriptor().FullName()
		t.Run(string(name), func(t *testing.T) {
			if empty.GetOperationEnvelope() != nil {
				t.Fatalf("GetOperationEnvelope() = %v on an unset request, want nil", empty.GetOperationEnvelope())
			}
			if out := roundTrip(t, empty); out.GetOperationEnvelope() != nil {
				t.Fatalf("GetOperationEnvelope() = %v after round trip of an unset request, want nil", out.GetOperationEnvelope())
			}
			env := sampleEnvelope(t)
			out := roundTrip(t, rq.make(env))
			got := out.GetOperationEnvelope()
			if got == nil {
				t.Fatal("GetOperationEnvelope() = nil after round trip, want the envelope")
			}
			if !bytes.Equal(got.GetRequest(), env.GetRequest()) {
				t.Errorf("envelope request bytes changed: got %x, want %x", got.GetRequest(), env.GetRequest())
			}
			if !bytes.Equal(got.GetGrant().GetBody(), env.GetGrant().GetBody()) {
				t.Errorf("grant body bytes changed: got %x, want %x", got.GetGrant().GetBody(), env.GetGrant().GetBody())
			}
		})
	}
}

func TestOperationIdentity_OpaqueBytesSurvive(t *testing.T) {
	// Bytes that are not a valid protobuf message (a truncated tag and an
	// over-long varint) must still be carried byte for byte.
	notAMessage := []byte{0x0a, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	if err := proto.Unmarshal(notAMessage, &GrantBody{}); err == nil {
		t.Fatal("test bytes unexpectedly decode as GrantBody; pick bytes that do not")
	}
	req := &PowerRequest{OperationEnvelope: &OperationEnvelope{
		Request: notAMessage,
		Grant:   &OperationGrant{Body: notAMessage},
	}}
	out := roundTrip(t, req)
	if !bytes.Equal(out.GetOperationEnvelope().GetRequest(), notAMessage) {
		t.Errorf("envelope request changed: %x", out.GetOperationEnvelope().GetRequest())
	}
	if !bytes.Equal(out.GetOperationEnvelope().GetGrant().GetBody(), notAMessage) {
		t.Errorf("grant body changed: %x", out.GetOperationEnvelope().GetGrant().GetBody())
	}
}

func TestOperationIdentity_UnknownFieldsObservable(t *testing.T) {
	b, err := proto.Marshal(&GrantBody{GrantVersion: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b = protowire.AppendTag(b, 99, protowire.VarintType)
	b = protowire.AppendVarint(b, 1)
	var got GrantBody
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.ProtoReflect().GetUnknown()) == 0 {
		t.Fatal("GetUnknown() is empty for a GrantBody with field 99, want the unknown field")
	}
	var clean GrantBody
	if err := proto.Unmarshal(b[:len(b)-3], &clean); err != nil {
		t.Fatalf("unmarshal clean: %v", err)
	}
	if len(clean.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("GetUnknown() is non-empty for a GrantBody with only known fields")
	}
}

// fieldSpec is the expected shape of one field.
type fieldSpec struct {
	name     protoreflect.Name
	number   protoreflect.FieldNumber
	kind     protoreflect.Kind
	typeName protoreflect.FullName // message or enum full name; empty for scalars
	repeated bool
}

func checkFields(t *testing.T, md protoreflect.MessageDescriptor, want []fieldSpec, exact bool) {
	t.Helper()
	for _, w := range want {
		fd := md.Fields().ByName(w.name)
		if fd == nil {
			t.Errorf("%s: field %s missing", md.FullName(), w.name)
			continue
		}
		if fd.Number() != w.number {
			t.Errorf("%s.%s: number %d, want %d", md.FullName(), w.name, fd.Number(), w.number)
		}
		if fd.Kind() != w.kind {
			t.Errorf("%s.%s: kind %v, want %v", md.FullName(), w.name, fd.Kind(), w.kind)
		}
		var typeName protoreflect.FullName
		switch {
		case fd.Message() != nil:
			typeName = fd.Message().FullName()
		case fd.Enum() != nil:
			typeName = fd.Enum().FullName()
		}
		if typeName != w.typeName {
			t.Errorf("%s.%s: type %q, want %q", md.FullName(), w.name, typeName, w.typeName)
		}
		if (fd.Cardinality() == protoreflect.Repeated) != w.repeated {
			t.Errorf("%s.%s: repeated=%v, want %v", md.FullName(), w.name, fd.Cardinality() == protoreflect.Repeated, w.repeated)
		}
	}
	if exact && md.Fields().Len() != len(want) {
		t.Errorf("%s has %d fields, want %d: update this test only after reviewing the new field", md.FullName(), md.Fields().Len(), len(want))
	}
}

func TestOperationIdentity_FieldNumbersPinned(t *testing.T) {
	checkFields(t, (&OperationEnvelope{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"request", 1, protoreflect.BytesKind, "", false},
		{"grant", 2, protoreflect.MessageKind, grantMessageName, false},
	}, true)
	checkFields(t, (&OperationGrant{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"body", 1, protoreflect.BytesKind, "", false},
		{"signature", 2, protoreflect.BytesKind, "", false},
		{"signer_cert_chain", 3, protoreflect.BytesKind, "", true},
	}, true)
	checkFields(t, (&Epoch{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"counter", 1, protoreflect.Uint64Kind, "", false},
		{"holder", 2, protoreflect.BytesKind, "", false},
	}, true)
	checkFields(t, (&GrantPrecondition{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"store_id", 1, protoreflect.BytesKind, "", false},
		{"seq", 2, protoreflect.Uint64Kind, "", false},
		{"head", 3, protoreflect.BytesKind, "", false},
		{"quarantine_id", 4, protoreflect.BytesKind, "", false},
		{"clock_floor", 5, protoreflect.MessageKind, timestampMessageName, false},
	}, true)

	body := (&GrantBody{}).ProtoReflect().Descriptor()
	checkFields(t, body, []fieldSpec{
		{"grant_version", 1, protoreflect.Uint32Kind, "", false},
		{"grant_kind", 2, protoreflect.EnumKind, grantKindEnumName, false},
		{"install_id", 3, protoreflect.BytesKind, "", false},
		{"provider_uid", 4, protoreflect.StringKind, "", false},
		{"host_id", 5, protoreflect.StringKind, "", false},
		{"enrolment_id", 6, protoreflect.BytesKind, "", false},
		{"authority_generation", 7, protoreflect.Uint64Kind, "", false},
		{"epoch", 8, protoreflect.MessageKind, epochMessageName, false},
		{"vm_uid", 10, protoreflect.StringKind, "", false},
		{"binding_id", 11, protoreflect.BytesKind, "", false},
		{"op_id", 12, protoreflect.BytesKind, "", false},
		{"kind", 13, protoreflect.EnumKind, operationKindEnumName, false},
		{"attempt", 14, protoreflect.Uint64Kind, "", false},
		{"intent_digest", 15, protoreflect.BytesKind, "", false},
		{"digest_salt", 16, protoreflect.BytesKind, "", false},
		{"mint_epoch", 17, protoreflect.MessageKind, epochMessageName, false},
		{"expect_known", 18, protoreflect.BoolKind, "", false},
		{"target_namespace", 20, protoreflect.StringKind, "", false},
		{"request_uid", 21, protoreflect.StringKind, "", false},
		{"action", 22, protoreflect.StringKind, "", false},
		{"spec_digest", 23, protoreflect.BytesKind, "", false},
		{"single_use_nonce", 24, protoreflect.BytesKind, "", false},
		{"precondition", 25, protoreflect.MessageKind, preconditionMsgName, false},
		{"eal_seq", 26, protoreflect.Uint64Kind, "", false},
		{"reset_floor", 28, protoreflect.MessageKind, timestampMessageName, false},
		{"ceremony_record_digest", 29, protoreflect.BytesKind, "", false},
		{"not_after", 30, protoreflect.MessageKind, timestampMessageName, false},
		{"issued_at", 31, protoreflect.MessageKind, timestampMessageName, false},
	}, true)
	if !body.ReservedRanges().Has(27) {
		t.Error("GrantBody: field number 27 is not reserved")
	}
	if !body.ReservedNames().Has("ceremony_index") {
		t.Error(`GrantBody: name "ceremony_index" is not reserved`)
	}
	for _, n := range []protoreflect.FieldNumber{9, 19, 27} {
		if fd := body.Fields().ByNumber(n); fd != nil {
			t.Errorf("GrantBody: number %d is used by %s, want unassigned", n, fd.Name())
		}
	}

	for _, rq := range mutatingRequests {
		md := rq.make(nil).ProtoReflect().Descriptor()
		checkFields(t, md, []fieldSpec{
			{envelopeFieldName, rq.number, protoreflect.MessageKind, envelopeMessageName, false},
		}, false)
		if highest := highestFieldNumber(md); highest != rq.number {
			t.Errorf("%s: operation_envelope (%d) is not the highest field number (%d)", md.FullName(), rq.number, highest)
		}
	}

	checkFields(t, (&GetCapabilitiesResponse{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"supports_operation_identity", 31, protoreflect.BoolKind, "", false},
		{"requires_operation_identity", 32, protoreflect.BoolKind, "", false},
		{"supports_fencing", 33, protoreflect.BoolKind, "", false},
		{"requires_fencing", 34, protoreflect.BoolKind, "", false},
	}, false)
}

func highestFieldNumber(md protoreflect.MessageDescriptor) protoreflect.FieldNumber {
	var highest protoreflect.FieldNumber
	for i := 0; i < md.Fields().Len(); i++ {
		if n := md.Fields().Get(i).Number(); n > highest {
			highest = n
		}
	}
	return highest
}

func checkEnum(t *testing.T, ed protoreflect.EnumDescriptor, want []protoreflect.Name) {
	t.Helper()
	if ed.Values().Len() != len(want) {
		t.Errorf("%s has %d values, want %d: update this test only after reviewing the new value", ed.FullName(), ed.Values().Len(), len(want))
	}
	for i, name := range want {
		v := ed.Values().ByName(name)
		if v == nil {
			t.Errorf("%s: value %s missing", ed.FullName(), name)
			continue
		}
		if v.Number() != protoreflect.EnumNumber(i) {
			t.Errorf("%s.%s = %d, want %d", ed.FullName(), name, v.Number(), i)
		}
	}
}

func TestOperationIdentity_EnumValuesPinned(t *testing.T) {
	checkEnum(t, GrantKind(0).Descriptor(), []protoreflect.Name{
		"GRANT_KIND_UNSPECIFIED",
		"GRANT_KIND_OPERATION",
		"GRANT_KIND_ACTIVATE",
		"GRANT_KIND_ATTEST",
		"GRANT_KIND_LEDGER_MARK",
		"GRANT_KIND_RELEASE",
		"GRANT_KIND_RETIRE_IF_UNKNOWN",
		"GRANT_KIND_CANCEL",
		"GRANT_KIND_ADMIN",
		"GRANT_KIND_ENROL",
		"GRANT_KIND_RETIRE_PROVIDER",
		"GRANT_KIND_TIME_SYNC",
	})
	checkEnum(t, OperationKind(0).Descriptor(), []protoreflect.Name{
		"OPERATION_KIND_UNSPECIFIED",
		"OPERATION_KIND_CREATE",
		"OPERATION_KIND_DELETE",
		"OPERATION_KIND_POWER_ON",
		"OPERATION_KIND_POWER_OFF",
		"OPERATION_KIND_REBOOT",
		"OPERATION_KIND_SHUTDOWN_GRACEFUL",
		"OPERATION_KIND_RECONFIGURE",
		"OPERATION_KIND_HARDWARE_UPGRADE",
		"OPERATION_KIND_SNAPSHOT_CREATE",
		"OPERATION_KIND_SNAPSHOT_DELETE",
		"OPERATION_KIND_SNAPSHOT_REVERT",
		"OPERATION_KIND_CLONE",
		"OPERATION_KIND_IMAGE_PREPARE",
		"OPERATION_KIND_EXPORT",
		"OPERATION_KIND_IMPORT",
		"OPERATION_KIND_DELETE_STAGED_DISK",
		"OPERATION_KIND_DELETE_LANDED_DISK",
		"OPERATION_KIND_ADOPT",
	})
}

// operationKindRow maps one OperationKind to its RPC, request message and,
// for Power, the fixed PowerOp.
type operationKindRow struct {
	method  string
	request protoreflect.FullName
	powerOp PowerOp // POWER_OP_UNSPECIFIED when no request field is fixed
}

// operationKindTable is the normative OperationKind to RPC mapping.
var operationKindTable = map[OperationKind]operationKindRow{
	OperationKind_OPERATION_KIND_CREATE:             {Provider_Create_FullMethodName, "provider.v1.CreateRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_DELETE:             {Provider_Delete_FullMethodName, "provider.v1.DeleteRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_POWER_ON:           {Provider_Power_FullMethodName, "provider.v1.PowerRequest", PowerOp_POWER_OP_ON},
	OperationKind_OPERATION_KIND_POWER_OFF:          {Provider_Power_FullMethodName, "provider.v1.PowerRequest", PowerOp_POWER_OP_OFF},
	OperationKind_OPERATION_KIND_REBOOT:             {Provider_Power_FullMethodName, "provider.v1.PowerRequest", PowerOp_POWER_OP_REBOOT},
	OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL:  {Provider_Power_FullMethodName, "provider.v1.PowerRequest", PowerOp_POWER_OP_SHUTDOWN_GRACEFUL},
	OperationKind_OPERATION_KIND_RECONFIGURE:        {Provider_Reconfigure_FullMethodName, "provider.v1.ReconfigureRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_HARDWARE_UPGRADE:   {Provider_HardwareUpgrade_FullMethodName, "provider.v1.HardwareUpgradeRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_SNAPSHOT_CREATE:    {Provider_SnapshotCreate_FullMethodName, "provider.v1.SnapshotCreateRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_SNAPSHOT_DELETE:    {Provider_SnapshotDelete_FullMethodName, "provider.v1.SnapshotDeleteRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_SNAPSHOT_REVERT:    {Provider_SnapshotRevert_FullMethodName, "provider.v1.SnapshotRevertRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_CLONE:              {Provider_Clone_FullMethodName, "provider.v1.CloneRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_IMAGE_PREPARE:      {Provider_ImagePrepare_FullMethodName, "provider.v1.ImagePrepareRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_EXPORT:             {Provider_ExportDisk_FullMethodName, "provider.v1.ExportDiskRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_IMPORT:             {Provider_ImportDisk_FullMethodName, "provider.v1.ImportDiskRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_DELETE_STAGED_DISK: {Provider_DeleteStagedDisk_FullMethodName, "provider.v1.DeleteStagedDiskRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_DELETE_LANDED_DISK: {Provider_DeleteLandedDisk_FullMethodName, "provider.v1.DeleteLandedDiskRequest", PowerOp_POWER_OP_UNSPECIFIED},
	OperationKind_OPERATION_KIND_ADOPT:              {Provider_TransferOwner_FullMethodName, "provider.v1.TransferOwnerRequest", PowerOp_POWER_OP_UNSPECIFIED},
}

// readMethods are the Provider RPCs that take no operation envelope.
var readMethods = map[string]bool{
	Provider_Validate_FullMethodName:        true,
	Provider_Describe_FullMethodName:        true,
	Provider_TaskStatus_FullMethodName:      true,
	Provider_GetCapabilities_FullMethodName: true,
	Provider_GetDiskInfo_FullMethodName:     true,
	Provider_ListVMs_FullMethodName:         true,
	Provider_ListHosts_FullMethodName:       true,
	Provider_GetHostInfo_FullMethodName:     true,
}

func TestOperationIdentity_OperationKindCoverage(t *testing.T) {
	values := OperationKind(0).Descriptor().Values()
	for i := 0; i < values.Len(); i++ {
		k := OperationKind(values.Get(i).Number())
		_, ok := operationKindTable[k]
		if k == OperationKind_OPERATION_KIND_UNSPECIFIED {
			if ok {
				t.Error("OPERATION_KIND_UNSPECIFIED must not map to an RPC")
			}
			continue
		}
		if !ok {
			t.Errorf("%s has no RPC mapping", k)
		}
	}
	if len(operationKindTable) != values.Len()-1 {
		t.Errorf("mapping has %d rows, want %d", len(operationKindTable), values.Len()-1)
	}

	svc := File_provider_v1_provider_proto.Services().ByName("Provider")
	if svc == nil {
		t.Fatal("service provider.v1.Provider not found")
	}
	inputs := map[string]protoreflect.FullName{}
	mutating := map[string]bool{}
	for _, row := range operationKindTable {
		mutating[row.method] = true
	}
	for i := 0; i < svc.Methods().Len(); i++ {
		m := svc.Methods().Get(i)
		full := "/" + string(svc.FullName()) + "/" + string(m.Name())
		inputs[full] = m.Input().FullName()
		switch {
		case readMethods[full] && mutating[full]:
			t.Errorf("%s is listed both as a read RPC and as an operation kind's RPC", full)
		case readMethods[full]:
			if m.Input().Fields().ByName(envelopeFieldName) != nil {
				t.Errorf("read RPC %s input %s has an operation_envelope", full, m.Input().FullName())
			}
		case mutating[full]:
			if m.Input().Fields().ByName(envelopeFieldName) == nil {
				t.Errorf("mutating RPC %s input %s has no operation_envelope", full, m.Input().FullName())
			}
		default:
			t.Errorf("RPC %s is neither a read RPC nor mapped by an OperationKind", full)
		}
	}
	if got, want := svc.Methods().Len(), len(readMethods)+len(mutatingRequests); got != want {
		t.Errorf("service has %d methods, want %d read + mutating", got, want)
	}
	for k, row := range operationKindTable {
		in, ok := inputs[row.method]
		if !ok {
			t.Errorf("%s maps to unknown RPC %s", k, row.method)
			continue
		}
		if in != row.request {
			t.Errorf("%s: RPC %s takes %s, mapping says %s", k, row.method, in, row.request)
		}
		if (row.method == Provider_Power_FullMethodName) != (row.powerOp != PowerOp_POWER_OP_UNSPECIFIED) {
			t.Errorf("%s: a fixed PowerOp is required exactly for Power kinds", k)
		}
	}
}
