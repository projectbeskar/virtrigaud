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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These tests pin the signed host answer wire shape: the messages, enums,
// field numbers and the thirteen additive request and response fields are a
// frozen contract, and a change to any of them must fail here until it has
// been reviewed.

const (
	signedBlobMessageName      = "provider.v1.SignedBlob"
	answerContextMessageName   = "provider.v1.AnswerContext"
	agentStateEnumName         = "provider.v1.AgentState"
	operationRecordStateEnum   = "provider.v1.OperationRecordState"
	vmPresenceEnumName         = "provider.v1.VMPresence"
	agentModeEnumName          = "provider.v1.AgentMode"
	providerGoPackage          = "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1;providerv1"
	hostSignedDomainSeparation = "virtrigaud.host-signed.v1"
)

// hostSignedBodyTypes is the closed set of SignedBlob.body_type values.
var hostSignedBodyTypes = []string{
	"provider.v1.OperationAnswer",
	"provider.v1.VMAnswer",
	"provider.v1.HostAnswer",
}

// fill returns n bytes, each equal to b, so that every byte field in these
// tests has a distinct, recognisable value.
func fill(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

// arbitraryBytes holds every byte value, including ones that are not valid
// UTF-8, so that a bytes field that was mistakenly decoded or re-encoded as a
// string would not round-trip.
func arbitraryBytes() []byte {
	out := make([]byte, 256)
	for i := range out {
		out[i] = byte(255 - i)
	}
	return out
}

func fullAnswerContext() *AnswerContext {
	return &AnswerContext{
		Nonce:          fill(0x01, 16),
		ProviderUid:    "11111111-2222-3333-4444-555555555555",
		HostId:         "host-a",
		InstallId:      fill(0x02, 16),
		EnrolmentId:    fill(0x03, 16),
		ChainPosition:  fill(0x04, 56),
		AgentState:     AgentState_AGENT_STATE_QUARANTINED,
		StateReason:    "rolled_back",
		AgentTime:      timestamppb.New(time.Unix(1700000000, 5)),
		ContinuityHead: fill(0x05, 32),
		CheckpointSeq:  1024,
		CheckpointHead: fill(0x06, 32),
	}
}

func fullSignedBlob() *SignedBlob {
	return &SignedBlob{
		Body:              arbitraryBytes(),
		Signature:         fill(0x30, 72),
		SignerCertificate: fill(0x31, 512),
		BodyType:          "provider.v1.HostAnswer",
	}
}

func fullOperationAnswer() *OperationAnswer {
	return &OperationAnswer{
		Context:       fullAnswerContext(),
		OpId:          fill(0x07, 16),
		OpEnrolmentId: fill(0x08, 16),
		State:         OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY,
		Tombstoned:    true,
		Kind:          int32(OperationKind_OPERATION_KIND_DELETE),
		Attempt:       3,
		VmUid:         "66666666-7777-8888-9999-000000000000",
		BindingId:     fill(0x09, 16),
		IntentDigest:  fill(0x0a, 32),
		OutcomeReason: "witness_no_effect",
	}
}

func fullVMAnswer() *VMAnswer {
	return &VMAnswer{
		Context:           fullAnswerContext(),
		VmUid:             "66666666-7777-8888-9999-000000000000",
		BindingId:         fill(0x0b, 16),
		Presence:          VMPresence_VM_PRESENCE_DELETED,
		DeleteOpId:        fill(0x0c, 16),
		DeleteEnrolmentId: fill(0x0d, 16),
		DeleteSeq:         42,
	}
}

func fullHostAnswer() *HostAnswer {
	return &HostAnswer{
		Context:                 fullAnswerContext(),
		AgentVersion:            "v0.0.1",
		AgentApiVersion:         1,
		GrantVersions:           []uint32{1, 2},
		Mode:                    AgentMode_AGENT_MODE_IDENTITY,
		JournalSchemaVersion:    1,
		ClockFloor:              timestamppb.New(time.Unix(1700000000, 5)),
		StagedCertificateSha256: fill(0x0e, 32),
	}
}

// assertEveryFieldSet fails when a field of m (not recursively) is unset, so
// a round-trip test cannot silently skip a field that was added later.
func assertEveryFieldSet(t *testing.T, m proto.Message) {
	t.Helper()
	r := m.ProtoReflect()
	fields := r.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		if fd := fields.Get(i); !r.Has(fd) {
			t.Errorf("%s.%s is not set in the round-trip fixture", r.Descriptor().FullName(), fd.Name())
		}
	}
}

func TestHostAnswer_NewMessagesRoundTrip(t *testing.T) {
	for _, m := range []proto.Message{
		fullSignedBlob(),
		fullAnswerContext(),
		fullOperationAnswer(),
		fullVMAnswer(),
		fullHostAnswer(),
	} {
		assertEveryFieldSet(t, m)
		roundTrip(t, m)
	}
}

func TestHostAnswer_ChangedMessagesRoundTrip(t *testing.T) {
	// Unset: every new getter returns its zero value.
	plainTS := roundTrip(t, &TaskStatusRequest{Task: &TaskRef{Id: "task-1"}})
	if plainTS.GetAnswerNonce() != nil || plainTS.GetOpId() != nil || plainTS.GetOpEnrolmentId() != nil ||
		plainTS.GetTargetHostId() != "" || plainTS.GetContinuitySeq() != 0 {
		t.Errorf("TaskStatusRequest without the new fields reports %v", plainTS)
	}
	if got := roundTrip(t, &TaskStatusResponse{Done: true}).GetHostAnswer(); got != nil {
		t.Errorf("TaskStatusResponse.host_answer = %v, want nil", got)
	}
	plainD := roundTrip(t, &DescribeRequest{Id: "vm-1", TargetHostId: "host-a"})
	if plainD.GetAnswerNonce() != nil || plainD.GetBindingId() != nil || plainD.GetContinuitySeq() != 0 {
		t.Errorf("DescribeRequest without the new fields reports %v", plainD)
	}
	if got := roundTrip(t, &DescribeResponse{Exists: true}).GetHostAnswer(); got != nil {
		t.Errorf("DescribeResponse.host_answer = %v, want nil", got)
	}
	plainH := roundTrip(t, &GetHostInfoRequest{HostId: "host-a"})
	if plainH.GetAnswerNonce() != nil || plainH.GetContinuitySeq() != 0 {
		t.Errorf("GetHostInfoRequest without the new fields reports %v", plainH)
	}
	if got := roundTrip(t, &HostInfo{Id: "host-a"}).GetHostAnswer(); got != nil {
		t.Errorf("HostInfo.host_answer = %v, want nil", got)
	}

	// Set. A TaskStatusRequest that asks for a signed answer has task unset.
	roundTrip(t, &TaskStatusRequest{
		AnswerNonce:   fill(0x21, 16),
		OpId:          fill(0x22, 16),
		OpEnrolmentId: fill(0x23, 16),
		TargetHostId:  "host-a",
		ContinuitySeq: 7,
	})
	roundTrip(t, &TaskStatusResponse{Done: true, HostAnswer: fullSignedBlob()})
	roundTrip(t, &DescribeRequest{
		Id:            "vm-1",
		TargetHostId:  "host-a",
		Owner:         &ObjectIdentity{Uid: "66666666-7777-8888-9999-000000000000"},
		AnswerNonce:   fill(0x24, 16),
		BindingId:     fill(0x25, 16),
		ContinuitySeq: 9,
	})
	roundTrip(t, &DescribeResponse{Exists: true, HostAnswer: fullSignedBlob()})
	roundTrip(t, &GetHostInfoRequest{HostId: "host-a", AnswerNonce: fill(0x26, 16), ContinuitySeq: 11})
	roundTrip(t, &HostInfo{Id: "host-a", HostAnswer: fullSignedBlob()})
}

func assertBlobBytes(t *testing.T, where string, got, want *SignedBlob) {
	t.Helper()
	if !bytes.Equal(got.GetBody(), want.GetBody()) ||
		!bytes.Equal(got.GetSignature(), want.GetSignature()) ||
		!bytes.Equal(got.GetSignerCertificate(), want.GetSignerCertificate()) ||
		got.GetBodyType() != want.GetBodyType() {
		t.Errorf("%s: SignedBlob bytes changed in transit", where)
	}
}

func TestHostAnswer_SignedBlobBytesExact(t *testing.T) {
	want := fullSignedBlob()

	ts := roundTrip(t, &TaskStatusResponse{HostAnswer: fullSignedBlob()})
	assertBlobBytes(t, "TaskStatusResponse", ts.GetHostAnswer(), want)
	d := roundTrip(t, &DescribeResponse{HostAnswer: fullSignedBlob()})
	assertBlobBytes(t, "DescribeResponse", d.GetHostAnswer(), want)
	h := roundTrip(t, &HostInfo{HostAnswer: fullSignedBlob()})
	assertBlobBytes(t, "HostInfo", h.GetHostAnswer(), want)

	// An error status carries the SignedBlob as a detail (google.rpc.Status).
	st, err := status.New(codes.NotFound, "unknown to agent").WithDetails(fullSignedBlob())
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	raw, err := proto.Marshal(st.Proto())
	if err != nil {
		t.Fatalf("marshal google.rpc.Status: %v", err)
	}
	decoded := st.Proto().ProtoReflect().New()
	if err := proto.Unmarshal(raw, decoded.Interface()); err != nil {
		t.Fatalf("unmarshal google.rpc.Status: %v", err)
	}
	details := decoded.Get(decoded.Descriptor().Fields().ByName("details")).List()
	if details.Len() != 1 {
		t.Fatalf("google.rpc.Status has %d details, want 1", details.Len())
	}
	anyDetail, ok := details.Get(0).Message().Interface().(*anypb.Any)
	if !ok {
		t.Fatalf("detail is %T, want *anypb.Any", details.Get(0).Message().Interface())
	}
	if got, wantURL := anyDetail.GetTypeUrl(), "type.googleapis.com/"+signedBlobMessageName; got != wantURL {
		t.Errorf("detail type URL = %q, want %q", got, wantURL)
	}
	var fromStatus SignedBlob
	if err := anyDetail.UnmarshalTo(&fromStatus); err != nil {
		t.Fatalf("UnmarshalTo SignedBlob: %v", err)
	}
	assertBlobBytes(t, "google.rpc.Status detail", &fromStatus, want)
}

func TestHostAnswer_UnknownFieldsObservable(t *testing.T) {
	extra := protowire.AppendTag(nil, 99, protowire.VarintType)
	extra = protowire.AppendVarint(extra, 1)
	for _, m := range []proto.Message{
		fullSignedBlob(),
		fullAnswerContext(),
		fullOperationAnswer(),
		fullVMAnswer(),
		fullHostAnswer(),
	} {
		raw, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		out := m.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(append(raw, extra...), out); err != nil {
			t.Fatalf("unmarshal %s with field 99: %v", m.ProtoReflect().Descriptor().FullName(), err)
		}
		if len(out.ProtoReflect().GetUnknown()) == 0 {
			t.Errorf("%s: unknown field 99 is not observable", m.ProtoReflect().Descriptor().FullName())
		}
	}
}

func TestHostAnswer_FieldNumbersPinned(t *testing.T) {
	checkFields(t, (&SignedBlob{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"body", 1, protoreflect.BytesKind, "", false},
		{"signature", 2, protoreflect.BytesKind, "", false},
		{"signer_certificate", 3, protoreflect.BytesKind, "", false},
		{"body_type", 4, protoreflect.StringKind, "", false},
	}, true)
	checkFields(t, (&AnswerContext{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"nonce", 1, protoreflect.BytesKind, "", false},
		{"provider_uid", 2, protoreflect.StringKind, "", false},
		{"host_id", 3, protoreflect.StringKind, "", false},
		{"install_id", 4, protoreflect.BytesKind, "", false},
		{"enrolment_id", 5, protoreflect.BytesKind, "", false},
		{"chain_position", 6, protoreflect.BytesKind, "", false},
		{"agent_state", 7, protoreflect.EnumKind, agentStateEnumName, false},
		{"state_reason", 8, protoreflect.StringKind, "", false},
		{"agent_time", 9, protoreflect.MessageKind, timestampMessageName, false},
		{"continuity_head", 10, protoreflect.BytesKind, "", false},
		{"checkpoint_seq", 11, protoreflect.Uint64Kind, "", false},
		{"checkpoint_head", 12, protoreflect.BytesKind, "", false},
	}, true)
	checkFields(t, (&OperationAnswer{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"context", 1, protoreflect.MessageKind, answerContextMessageName, false},
		{"op_id", 2, protoreflect.BytesKind, "", false},
		{"op_enrolment_id", 3, protoreflect.BytesKind, "", false},
		{"state", 4, protoreflect.EnumKind, operationRecordStateEnum, false},
		{"tombstoned", 5, protoreflect.BoolKind, "", false},
		{"kind", 6, protoreflect.Int32Kind, "", false},
		{"attempt", 7, protoreflect.Uint64Kind, "", false},
		{"vm_uid", 8, protoreflect.StringKind, "", false},
		{"binding_id", 9, protoreflect.BytesKind, "", false},
		{"intent_digest", 10, protoreflect.BytesKind, "", false},
		{"outcome_reason", 11, protoreflect.StringKind, "", false},
	}, true)
	checkFields(t, (&VMAnswer{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"context", 1, protoreflect.MessageKind, answerContextMessageName, false},
		{"vm_uid", 2, protoreflect.StringKind, "", false},
		{"binding_id", 3, protoreflect.BytesKind, "", false},
		{"presence", 4, protoreflect.EnumKind, vmPresenceEnumName, false},
		{"delete_op_id", 5, protoreflect.BytesKind, "", false},
		{"delete_enrolment_id", 6, protoreflect.BytesKind, "", false},
		{"delete_seq", 7, protoreflect.Uint64Kind, "", false},
	}, true)
	checkFields(t, (&HostAnswer{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"context", 1, protoreflect.MessageKind, answerContextMessageName, false},
		{"agent_version", 2, protoreflect.StringKind, "", false},
		{"agent_api_version", 3, protoreflect.Uint32Kind, "", false},
		{"grant_versions", 4, protoreflect.Uint32Kind, "", true},
		{"mode", 5, protoreflect.EnumKind, agentModeEnumName, false},
		{"journal_schema_version", 6, protoreflect.Uint32Kind, "", false},
		{"clock_floor", 7, protoreflect.MessageKind, timestampMessageName, false},
		{"staged_certificate_sha256", 8, protoreflect.BytesKind, "", false},
	}, true)
}

// TestHostAnswer_AdditiveRequestFieldsPinned pins the thirteen additive
// fields on six existing messages, the field count of each message (so no
// other field was added with them) and that each added field is the
// highest number of its message.
func TestHostAnswer_AdditiveRequestFieldsPinned(t *testing.T) {
	cases := []struct {
		msg     proto.Message
		added   []fieldSpec
		total   int
		highest protoreflect.Name
	}{
		{&TaskStatusRequest{}, []fieldSpec{
			{"answer_nonce", 2, protoreflect.BytesKind, "", false},
			{"op_id", 3, protoreflect.BytesKind, "", false},
			{"op_enrolment_id", 4, protoreflect.BytesKind, "", false},
			{"target_host_id", 5, protoreflect.StringKind, "", false},
			{"continuity_seq", 6, protoreflect.Uint64Kind, "", false},
		}, 6, "continuity_seq"},
		{&TaskStatusResponse{}, []fieldSpec{
			{"host_answer", 5, protoreflect.MessageKind, signedBlobMessageName, false},
		}, 5, "host_answer"},
		{&DescribeRequest{}, []fieldSpec{
			{"answer_nonce", 4, protoreflect.BytesKind, "", false},
			{"binding_id", 5, protoreflect.BytesKind, "", false},
			{"continuity_seq", 6, protoreflect.Uint64Kind, "", false},
		}, 6, "continuity_seq"},
		{&DescribeResponse{}, []fieldSpec{
			{"host_answer", 8, protoreflect.MessageKind, signedBlobMessageName, false},
		}, 8, "host_answer"},
		{&GetHostInfoRequest{}, []fieldSpec{
			{"answer_nonce", 2, protoreflect.BytesKind, "", false},
			{"continuity_seq", 3, protoreflect.Uint64Kind, "", false},
		}, 3, "continuity_seq"},
		{&HostInfo{}, []fieldSpec{
			{"host_answer", 12, protoreflect.MessageKind, signedBlobMessageName, false},
		}, 12, "host_answer"},
	}
	added := 0
	for _, c := range cases {
		md := c.msg.ProtoReflect().Descriptor()
		checkFields(t, md, c.added, false)
		added += len(c.added)
		if md.Fields().Len() != c.total {
			t.Errorf("%s has %d fields, want %d: update this test only after reviewing the new field", md.FullName(), md.Fields().Len(), c.total)
		}
		if fd := md.Fields().ByName(c.highest); fd == nil || fd.Number() != highestFieldNumber(md) {
			t.Errorf("%s: %s is not the highest field number (%d)", md.FullName(), c.highest, highestFieldNumber(md))
		}
	}
	if added != 13 {
		t.Errorf("pinned %d additive fields, want 13", added)
	}
}

func TestHostAnswer_EnumValuesPinned(t *testing.T) {
	checkEnum(t, AgentState(0).Descriptor(), []protoreflect.Name{
		"AGENT_STATE_UNSPECIFIED",
		"AGENT_STATE_UNATTESTED",
		"AGENT_STATE_READY",
		"AGENT_STATE_QUARANTINED",
	})
	checkEnum(t, OperationRecordState(0).Descriptor(), []protoreflect.Name{
		"OPERATION_RECORD_STATE_UNSPECIFIED",
		"OPERATION_RECORD_STATE_ACCEPTED",
		"OPERATION_RECORD_STATE_EXECUTING",
		"OPERATION_RECORD_STATE_COMPLETED",
		"OPERATION_RECORD_STATE_FAILED_CLEAN",
		"OPERATION_RECORD_STATE_FAILED_DIRTY",
		"OPERATION_RECORD_STATE_OUTCOME_UNKNOWN",
		"OPERATION_RECORD_STATE_RETIRED",
		"OPERATION_RECORD_STATE_NO_RECORD",
		"OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT",
	})
	checkEnum(t, VMPresence(0).Descriptor(), []protoreflect.Name{
		"VM_PRESENCE_UNSPECIFIED",
		"VM_PRESENCE_CREATING",
		"VM_PRESENCE_PRESENT",
		"VM_PRESENCE_DELETED",
		"VM_PRESENCE_OTHER_BINDING",
		"VM_PRESENCE_NO_RECORD",
	})
	checkEnum(t, AgentMode(0).Descriptor(), []protoreflect.Name{
		"AGENT_MODE_UNSPECIFIED",
		"AGENT_MODE_IDENTITY",
	})
}

func TestHostAnswer_GoPackageAndImports(t *testing.T) {
	answerFile := File_provider_v1_host_answer_proto
	providerFile := (&TaskStatusRequest{}).ProtoReflect().Descriptor().ParentFile()
	for _, fd := range []protoreflect.FileDescriptor{answerFile, providerFile} {
		opts, ok := fd.Options().(*descriptorpb.FileOptions)
		if !ok {
			t.Fatalf("%s: options are %T", fd.Path(), fd.Options())
		}
		if got := opts.GetGoPackage(); got != providerGoPackage {
			t.Errorf("%s: go_package = %q, want %q", fd.Path(), got, providerGoPackage)
		}
	}
	// host_answer.proto imports only the Timestamp type, so provider.proto
	// can import it without a cycle.
	imports := answerFile.Imports()
	if imports.Len() != 1 || imports.Get(0).Path() != "google/protobuf/timestamp.proto" {
		t.Errorf("%s imports %d files, want only google/protobuf/timestamp.proto", answerFile.Path(), imports.Len())
	}
	found := false
	for i := 0; i < providerFile.Imports().Len(); i++ {
		if providerFile.Imports().Get(i).Path() == answerFile.Path() {
			found = true
		}
	}
	if !found {
		t.Errorf("%s does not import %s", providerFile.Path(), answerFile.Path())
	}
}

func TestHostAnswer_BodyTypesClosedSet(t *testing.T) {
	want := []protoreflect.FullName{
		(&OperationAnswer{}).ProtoReflect().Descriptor().FullName(),
		(&VMAnswer{}).ProtoReflect().Descriptor().FullName(),
		(&HostAnswer{}).ProtoReflect().Descriptor().FullName(),
	}
	if len(hostSignedBodyTypes) != 3 {
		t.Fatalf("closed body_type set has %d entries, want 3", len(hostSignedBodyTypes))
	}
	for i, bt := range hostSignedBodyTypes {
		if protoreflect.FullName(bt) != want[i] {
			t.Errorf("body_type %q is not the full name %q", bt, want[i])
		}
		if len(bt) > 64 {
			t.Errorf("body_type %q is longer than 64 bytes", bt)
		}
	}
}

// TestHostAnswer_SignatureInputVector pins the published test vector of the
// host-signed signature input (docs/operation-grants-wire-shape.md): the
// framing only. Its body is not a complete answer.
func TestHostAnswer_SignatureInputVector(t *testing.T) {
	bodyType := string((&HostAnswer{}).ProtoReflect().Descriptor().FullName())
	body, err := proto.Marshal(&HostAnswer{AgentVersion: "v0.0.1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := hex.EncodeToString(body), "120676302e302e31"; got != want {
		t.Fatalf("vector body = %s, want %s", got, want)
	}

	input := append([]byte(hostSignedDomainSeparation), 0x00)
	input = binary.BigEndian.AppendUint16(input, uint16(len(bodyType)))
	input = append(input, bodyType...)
	input = binary.BigEndian.AppendUint32(input, uint32(len(body)))
	input = append(input, body...)

	const wantInput = "766972747269676175642e686f73742d7369676e65642e763100" +
		"0016" + "70726f76696465722e76312e486f7374416e73776572" +
		"00000008" + "120676302e302e31"
	if got := hex.EncodeToString(input); got != wantInput {
		t.Errorf("signature input = %s, want %s", got, wantInput)
	}
	if len(input) != 62 {
		t.Errorf("signature input is %d bytes, want 62", len(input))
	}
	digest := sha256.Sum256(input)
	if got, want := hex.EncodeToString(digest[:]), "92b2ed124ec68b2661cef2f6188a1f937121db3f0d25bb0ae8b98319bd696cea"; got != want {
		t.Errorf("SHA-256(signature input) = %s, want %s", got, want)
	}
	// The first byte of the input is never the start of a valid protobuf
	// message: 0x76 is field 14 with the invalid wire type 6.
	if num, typ, n := protowire.ConsumeTag(input); n != 1 || num != 14 || typ != 6 {
		t.Errorf("signature input starts with tag (%d, %d, %d), want field 14 wire type 6", num, typ, n)
	}
	if err := proto.Unmarshal(input, &GrantBody{}); err == nil {
		t.Error("signature input parses as a GrantBody")
	}
}
