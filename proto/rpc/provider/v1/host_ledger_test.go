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

package providerv1

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// These tests pin the reserved host ledger wire shape: the new messages,
// enums and service, the fields added to existing messages, and their
// numbers. Nothing implements the protocol yet; the shape is fixed so that a
// later version adds it without renumbering, and a change to it must fail
// here until it has been reviewed.

const (
	ledgerPositionMessageName   = "provider.v1.LedgerPosition"
	hostLedgerStateMessageName  = "provider.v1.HostLedgerState"
	ledgerSessionStateEnumName  = "provider.v1.LedgerSessionState"
	challengePurposeEnumName    = "provider.v1.ChallengePurpose"
	authorityOutcomeEnumName    = "provider.v1.AuthorityOutcome"
	operationGrantMessageName   = "provider.v1.OperationGrant"
	authorityProtoPath          = "provider/v1/authority.proto"
	hostAuthorityServiceName    = "provider.v1.HostAuthority"
	reservedReceiptBodyType     = "provider.v1.OperationReceipt"
	reservedChallengeBodyType   = "provider.v1.ChallengeAnswer"
	reservedAuthorityBodyType   = "provider.v1.AuthorityAnswer"
	ledgerFieldReservedCountMsg = "update this test only after reviewing the new field"
)

func fullLedgerPosition(b byte) *LedgerPosition {
	return &LedgerPosition{StoreId: fill(b, 16), Seq: 4096, Head: fill(b+1, 32)}
}

func fullHostLedgerState() *HostLedgerState {
	return &HostLedgerState{
		RequestedVersion:              1,
		Version:                       1,
		State:                         LedgerSessionState_LEDGER_SESSION_STATE_ATTESTED,
		AttestationId:                 fill(0x41, 16),
		Mark:                          fullLedgerPosition(0x42),
		PendingReceipts:               3,
		StoreOriginEnrolmentId:        fill(0x44, 16),
		StoreEnrolmentCount:           2,
		ParkedRequestBytes:            1 << 20,
		ParkedRequestBudgetBytes:      256 << 20,
		StoreEverAttested:             true,
		StoreFirstAttestedSeq:         17,
		StoreFirstAttestedEnrolmentId: fill(0x44, 16),
		StoreAttestedRecordSeq:        18,
	}
}

func fullOperationReceipt() *OperationReceipt {
	return &OperationReceipt{
		ReceiptVersion: 1,
		ProviderUid:    "11111111-2222-3333-4444-555555555555",
		HostId:         "host-a",
		InstallId:      fill(0x51, 16),
		EnrolmentId:    fill(0x52, 16),
		AttestationId:  fill(0x53, 16),
		StoreId:        fill(0x54, 16),
		OpId:           fill(0x55, 16),
		Kind:           int32(OperationKind_OPERATION_KIND_CREATE),
		Attempt:        1,
		VmUid:          "66666666-7777-8888-9999-000000000000",
		BindingId:      fill(0x56, 16),
		IntentDigest:   fill(0x57, 32),
		AcceptedSeq:    99,
		AcceptedHead:   fill(0x58, 32),
		GrantDigest:    fill(0x59, 32),
	}
}

func fullChallengeAnswer() *ChallengeAnswer {
	return &ChallengeAnswer{
		Context:         fullAnswerContext(),
		Purpose:         ChallengePurpose_CHALLENGE_PURPOSE_REATTEST,
		ChallengeNonce:  fill(0x61, 16),
		ValidForSeconds: 300,
		NotIssuedReason: "quarantined",
		Ledger:          fullHostLedgerState(),
	}
}

func fullAuthorityAnswer() *AuthorityAnswer {
	return &AuthorityAnswer{
		Context:       fullAnswerContext(),
		GrantKind:     int32(GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN),
		GrantDigest:   fill(0x71, 32),
		Outcome:       AuthorityOutcome_AUTHORITY_OUTCOME_NOT_APPLICABLE,
		OutcomeReason: "op_known",
		Mark:          fullLedgerPosition(0x72),
		OpId:          fill(0x74, 16),
		OpState:       OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD,
	}
}

func sampleOperationGrant() *OperationGrant {
	return &OperationGrant{Body: fill(0x81, 40), Signature: fill(0x82, 72), SignerCertChain: [][]byte{fill(0x83, 8), fill(0x84, 8)}}
}

func TestHostLedger_MessagesRoundTrip(t *testing.T) {
	for _, m := range []proto.Message{
		fullLedgerPosition(0x01),
		fullHostLedgerState(),
		fullOperationReceipt(),
		fullChallengeAnswer(),
		fullAuthorityAnswer(),
		&ChallengeRequest{TargetHostId: "host-a", AnswerNonce: fill(0x91, 16), Purpose: ChallengePurpose_CHALLENGE_PURPOSE_ATTEST},
		&ChallengeResponse{HostAnswer: fullSignedBlob()},
		&AttestRequest{TargetHostId: "host-a", AnswerNonce: fill(0x92, 16), Grant: sampleOperationGrant()},
		&LedgerMarkRequest{TargetHostId: "host-a", AnswerNonce: fill(0x93, 16), Grant: sampleOperationGrant()},
		&RetireIfUnknownRequest{TargetHostId: "host-a", AnswerNonce: fill(0x94, 16), Grant: sampleOperationGrant()},
		&AuthorityResponse{HostAnswer: fullSignedBlob()},
		&CreateResponse{Id: "vm-1", Task: &TaskRef{Id: "t"}, OperationReceipt: fullSignedBlob()},
		&TaskResponse{Task: &TaskRef{Id: "t"}, RestartRequired: true, HonestResult: true, OperationReceipt: fullSignedBlob()},
	} {
		assertEveryFieldSet(t, m)
		roundTrip(t, m)
	}
}

func TestHostLedger_FieldNumbersPinned(t *testing.T) {
	checkFields(t, (&LedgerPosition{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"store_id", 1, protoreflect.BytesKind, "", false},
		{"seq", 2, protoreflect.Uint64Kind, "", false},
		{"head", 3, protoreflect.BytesKind, "", false},
	}, true)
	checkFields(t, (&HostLedgerState{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"requested_version", 1, protoreflect.Uint32Kind, "", false},
		{"version", 2, protoreflect.Uint32Kind, "", false},
		{"state", 3, protoreflect.EnumKind, ledgerSessionStateEnumName, false},
		{"attestation_id", 4, protoreflect.BytesKind, "", false},
		{"mark", 5, protoreflect.MessageKind, ledgerPositionMessageName, false},
		{"pending_receipts", 6, protoreflect.Uint32Kind, "", false},
		{"store_origin_enrolment_id", 7, protoreflect.BytesKind, "", false},
		{"store_enrolment_count", 8, protoreflect.Uint32Kind, "", false},
		{"parked_request_bytes", 9, protoreflect.Uint64Kind, "", false},
		{"parked_request_budget_bytes", 10, protoreflect.Uint64Kind, "", false},
		{"store_ever_attested", 11, protoreflect.BoolKind, "", false},
		{"store_first_attested_seq", 12, protoreflect.Uint64Kind, "", false},
		{"store_first_attested_enrolment_id", 13, protoreflect.BytesKind, "", false},
		{"store_attested_record_seq", 14, protoreflect.Uint64Kind, "", false},
	}, true)
	checkFields(t, (&OperationReceipt{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"receipt_version", 1, protoreflect.Uint32Kind, "", false},
		{"provider_uid", 2, protoreflect.StringKind, "", false},
		{"host_id", 3, protoreflect.StringKind, "", false},
		{"install_id", 4, protoreflect.BytesKind, "", false},
		{"enrolment_id", 5, protoreflect.BytesKind, "", false},
		{"attestation_id", 6, protoreflect.BytesKind, "", false},
		{"store_id", 7, protoreflect.BytesKind, "", false},
		{"op_id", 8, protoreflect.BytesKind, "", false},
		{"kind", 9, protoreflect.Int32Kind, "", false},
		{"attempt", 10, protoreflect.Uint64Kind, "", false},
		{"vm_uid", 11, protoreflect.StringKind, "", false},
		{"binding_id", 12, protoreflect.BytesKind, "", false},
		{"intent_digest", 13, protoreflect.BytesKind, "", false},
		{"accepted_seq", 14, protoreflect.Uint64Kind, "", false},
		{"accepted_head", 15, protoreflect.BytesKind, "", false},
		{"grant_digest", 16, protoreflect.BytesKind, "", false},
	}, true)
	checkFields(t, (&ChallengeAnswer{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"context", 1, protoreflect.MessageKind, answerContextMessageName, false},
		{"purpose", 2, protoreflect.EnumKind, challengePurposeEnumName, false},
		{"challenge_nonce", 3, protoreflect.BytesKind, "", false},
		{"valid_for_seconds", 4, protoreflect.Uint32Kind, "", false},
		{"not_issued_reason", 5, protoreflect.StringKind, "", false},
		{"ledger", 6, protoreflect.MessageKind, hostLedgerStateMessageName, false},
	}, true)
	checkFields(t, (&AuthorityAnswer{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"context", 1, protoreflect.MessageKind, answerContextMessageName, false},
		{"grant_kind", 2, protoreflect.Int32Kind, "", false},
		{"grant_digest", 3, protoreflect.BytesKind, "", false},
		{"outcome", 4, protoreflect.EnumKind, authorityOutcomeEnumName, false},
		{"outcome_reason", 5, protoreflect.StringKind, "", false},
		{"mark", 6, protoreflect.MessageKind, ledgerPositionMessageName, false},
		{"op_id", 7, protoreflect.BytesKind, "", false},
		{"op_state", 8, protoreflect.EnumKind, operationRecordStateEnum, false},
	}, true)

	checkFields(t, (&ChallengeRequest{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"target_host_id", 1, protoreflect.StringKind, "", false},
		{"answer_nonce", 2, protoreflect.BytesKind, "", false},
		{"purpose", 3, protoreflect.EnumKind, challengePurposeEnumName, false},
	}, true)
	for _, m := range []proto.Message{&ChallengeResponse{}, &AuthorityResponse{}} {
		checkFields(t, m.ProtoReflect().Descriptor(), []fieldSpec{
			{"host_answer", 1, protoreflect.MessageKind, signedBlobMessageName, false},
		}, true)
	}
	for _, m := range []proto.Message{&AttestRequest{}, &LedgerMarkRequest{}, &RetireIfUnknownRequest{}} {
		checkFields(t, m.ProtoReflect().Descriptor(), []fieldSpec{
			{"target_host_id", 1, protoreflect.StringKind, "", false},
			{"answer_nonce", 2, protoreflect.BytesKind, "", false},
			{"grant", 3, protoreflect.MessageKind, operationGrantMessageName, false},
		}, true)
	}

	// The receipts on the accept responses: each the highest number of its
	// message, nothing else added with it.
	for _, c := range []struct {
		msg   proto.Message
		num   protoreflect.FieldNumber
		total int
	}{
		{&CreateResponse{}, 3, 3},
		{&TaskResponse{}, 4, 4},
	} {
		md := c.msg.ProtoReflect().Descriptor()
		checkFields(t, md, []fieldSpec{{"operation_receipt", c.num, protoreflect.MessageKind, signedBlobMessageName, false}}, false)
		if md.Fields().Len() != c.total {
			t.Errorf("%s has %d fields, want %d: %s", md.FullName(), md.Fields().Len(), c.total, ledgerFieldReservedCountMsg)
		}
		if highestFieldNumber(md) != c.num {
			t.Errorf("%s: operation_receipt is not the highest field number", md.FullName())
		}
	}

	// GrantBody: 19 stays unassigned, 27 reserved, 32-34 the ledger fields.
	body := (&GrantBody{}).ProtoReflect().Descriptor()
	if highestFieldNumber(body) != 34 {
		t.Errorf("GrantBody's highest field number is %d, want 34", highestFieldNumber(body))
	}
	if fd := body.Fields().ByNumber(19); fd != nil {
		t.Errorf("GrantBody: number 19 is used by %s, want unassigned", fd.Name())
	}
}

func TestHostLedger_EnumValuesPinned(t *testing.T) {
	checkEnum(t, LedgerSessionState(0).Descriptor(), []protoreflect.Name{
		"LEDGER_SESSION_STATE_UNSPECIFIED",
		"LEDGER_SESSION_STATE_AWAITING_ATTESTATION",
		"LEDGER_SESSION_STATE_ATTESTED",
	})
	checkEnum(t, ChallengePurpose(0).Descriptor(), []protoreflect.Name{
		"CHALLENGE_PURPOSE_UNSPECIFIED",
		"CHALLENGE_PURPOSE_ATTEST",
		"CHALLENGE_PURPOSE_REATTEST",
	})
	checkEnum(t, AuthorityOutcome(0).Descriptor(), []protoreflect.Name{
		"AUTHORITY_OUTCOME_UNSPECIFIED",
		"AUTHORITY_OUTCOME_APPLIED",
		"AUTHORITY_OUTCOME_ALREADY_APPLIED",
		"AUTHORITY_OUTCOME_NOT_APPLICABLE",
		"AUTHORITY_OUTCOME_REFUSED",
		"AUTHORITY_OUTCOME_QUARANTINED",
	})
}

// TestHostLedger_ServicePinned: HostAuthority is its own service in its own
// file, with exactly its four methods and their request and response types;
// the Provider service gained none of them.
func TestHostLedger_ServicePinned(t *testing.T) {
	fd := File_provider_v1_authority_proto
	if fd.Path() != authorityProtoPath || fd.Package() != "provider.v1" {
		t.Fatalf("authority file = %s, package %s", fd.Path(), fd.Package())
	}
	if fd.Services().Len() != 1 {
		t.Fatalf("%s defines %d services, want 1", fd.Path(), fd.Services().Len())
	}
	sd := fd.Services().Get(0)
	if sd.FullName() != hostAuthorityServiceName {
		t.Fatalf("service = %s, want %s", sd.FullName(), hostAuthorityServiceName)
	}
	want := []struct {
		name, in, out protoreflect.FullName
		full          string
	}{
		{"Challenge", "provider.v1.ChallengeRequest", "provider.v1.ChallengeResponse", HostAuthority_Challenge_FullMethodName},
		{"Attest", "provider.v1.AttestRequest", "provider.v1.AuthorityResponse", HostAuthority_Attest_FullMethodName},
		{"LedgerMark", "provider.v1.LedgerMarkRequest", "provider.v1.AuthorityResponse", HostAuthority_LedgerMark_FullMethodName},
		{"RetireIfUnknown", "provider.v1.RetireIfUnknownRequest", "provider.v1.AuthorityResponse", HostAuthority_RetireIfUnknown_FullMethodName},
	}
	if sd.Methods().Len() != len(want) {
		t.Errorf("%s has %d methods, want %d", sd.FullName(), sd.Methods().Len(), len(want))
	}
	for _, w := range want {
		m := sd.Methods().ByName(protoreflect.Name(w.name))
		if m == nil {
			t.Errorf("%s.%s missing", sd.FullName(), w.name)
			continue
		}
		if m.Input().FullName() != w.in || m.Output().FullName() != w.out || m.IsStreamingClient() || m.IsStreamingServer() {
			t.Errorf("%s.%s: %s -> %s (streaming %v/%v), want unary %s -> %s", sd.FullName(), w.name,
				m.Input().FullName(), m.Output().FullName(), m.IsStreamingClient(), m.IsStreamingServer(), w.in, w.out)
		}
		if got := "/" + string(sd.FullName()) + "/" + string(w.name); got != w.full {
			t.Errorf("full method name %q, want %q", w.full, got)
		}
		if p := File_provider_v1_provider_proto.Services().ByName("Provider").Methods().ByName(protoreflect.Name(w.name)); p != nil {
			t.Errorf("the Provider service has a method %s", w.name)
		}
	}
	if opts, ok := fd.Options().(*descriptorpb.FileOptions); !ok || opts.GetGoPackage() != providerGoPackage {
		t.Errorf("%s go_package is not %q", fd.Path(), providerGoPackage)
	}
}

// TestHostLedger_ReservedBodyTypes: the three reserved signed body names are
// the full names of their messages, fit the body_type bound, and are not in
// the closed set of body types a verifier accepts today.
func TestHostLedger_ReservedBodyTypes(t *testing.T) {
	for _, c := range []struct {
		name string
		msg  proto.Message
	}{
		{reservedReceiptBodyType, &OperationReceipt{}},
		{reservedChallengeBodyType, &ChallengeAnswer{}},
		{reservedAuthorityBodyType, &AuthorityAnswer{}},
	} {
		if got := string(c.msg.ProtoReflect().Descriptor().FullName()); got != c.name {
			t.Errorf("full name %q, want %q", got, c.name)
		}
		if len(c.name) > 64 {
			t.Errorf("%q is longer than 64 bytes", c.name)
		}
		for _, bt := range hostSignedBodyTypes {
			if bt == c.name {
				t.Errorf("%q is in the accepted closed set", c.name)
			}
		}
	}
}

// TestHostLedger_UnsetFieldsEncodeAsBefore: a message that leaves the
// reserved fields unset encodes byte for byte as it did before they were
// added, so every body an agent signs today is unchanged; and each reserved
// field, when set, follows the older fields in the deterministic encoding.
func TestHostLedger_UnsetFieldsEncodeAsBefore(t *testing.T) {
	det := proto.MarshalOptions{Deterministic: true}
	for _, c := range []struct {
		name  string
		plain proto.Message
		set   func(proto.Message)
		num   protowire.Number
	}{
		{"AnswerContext.attestation_id", plainAnswerContext(), func(m proto.Message) { m.(*AnswerContext).AttestationId = fill(1, 16) }, 13},
		{"AnswerContext.ledger_version_echo", plainAnswerContext(), func(m proto.Message) { m.(*AnswerContext).LedgerVersionEcho = 1 }, 14},
		{"OperationAnswer.accepted_seq", plainOperationAnswer(), func(m proto.Message) { m.(*OperationAnswer).AcceptedSeq = 1 }, 12},
		{"OperationAnswer.accepted_head", plainOperationAnswer(), func(m proto.Message) { m.(*OperationAnswer).AcceptedHead = fill(1, 32) }, 13},
		{"OperationAnswer.awaiting_request", plainOperationAnswer(), func(m proto.Message) { m.(*OperationAnswer).AwaitingRequest = true }, 14},
		{"HostAnswer.ledger", plainHostAnswer(), func(m proto.Message) { m.(*HostAnswer).Ledger = fullHostLedgerState() }, 12},
		{"GrantBody.ledger", plainGrantBody(), func(m proto.Message) { m.(*GrantBody).Ledger = fullLedgerPosition(1) }, 32},
		{"GrantBody.ledger_checkpoint", plainGrantBody(), func(m proto.Message) { m.(*GrantBody).LedgerCheckpoint = fullLedgerPosition(1) }, 33},
		{"GrantBody.attestation_id", plainGrantBody(), func(m proto.Message) { m.(*GrantBody).AttestationId = fill(1, 16) }, 34},
		{"TaskStatusRequest.ledger_version", &TaskStatusRequest{AnswerNonce: fill(1, 16), OpId: fill(2, 16), OpEnrolmentId: fill(3, 16), TargetHostId: "h", ContinuitySeq: 5},
			func(m proto.Message) { m.(*TaskStatusRequest).LedgerVersion = 1 }, 7},
		{"DescribeRequest.ledger_version", &DescribeRequest{Id: "vm", AnswerNonce: fill(1, 16), BindingId: fill(2, 16), ContinuitySeq: 5},
			func(m proto.Message) { m.(*DescribeRequest).LedgerVersion = 1 }, 7},
		{"GetHostInfoRequest.ledger_version", &GetHostInfoRequest{HostId: "h", AnswerNonce: fill(1, 16), StoreCapacityVersion: 1},
			func(m proto.Message) { m.(*GetHostInfoRequest).LedgerVersion = 1 }, 7},
		{"CreateResponse.operation_receipt", &CreateResponse{Id: "vm", Task: &TaskRef{Id: "t"}},
			func(m proto.Message) { m.(*CreateResponse).OperationReceipt = fullSignedBlob() }, 3},
		{"TaskResponse.operation_receipt", &TaskResponse{Task: &TaskRef{Id: "t"}, HonestResult: true},
			func(m proto.Message) { m.(*TaskResponse).OperationReceipt = fullSignedBlob() }, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			plain, err := det.Marshal(c.plain)
			if err != nil {
				t.Fatal(err)
			}
			with := proto.Clone(c.plain)
			c.set(with)
			b, err := det.Marshal(with)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(b, plain) || len(b) == len(plain) {
				t.Fatal("the reserved field does not follow the older fields in the deterministic encoding")
			}
			num, _, n := protowire.ConsumeTag(b[len(plain):])
			if n < 0 || num != c.num {
				t.Fatalf("trailing field %d, want %d", num, c.num)
			}
		})
	}
}

// plainAnswerContext is fullAnswerContext without the reserved fields.
func plainAnswerContext() *AnswerContext {
	c := fullAnswerContext()
	c.AttestationId, c.LedgerVersionEcho = nil, 0
	return c
}

// plainOperationAnswer is fullOperationAnswer without the reserved fields.
func plainOperationAnswer() *OperationAnswer {
	a := fullOperationAnswer()
	a.Context = plainAnswerContext()
	a.AcceptedSeq, a.AcceptedHead, a.AwaitingRequest = 0, nil, false
	return a
}

// plainHostAnswer is fullHostAnswer without the reserved fields.
func plainHostAnswer() *HostAnswer {
	a := fullHostAnswer()
	a.Context = plainAnswerContext()
	a.Ledger = nil
	return a
}

// plainGrantBody is sampleGrantBody without the reserved fields.
func plainGrantBody() *GrantBody {
	b := sampleGrantBody()
	b.Ledger, b.LedgerCheckpoint, b.AttestationId = nil, nil, nil
	return b
}
