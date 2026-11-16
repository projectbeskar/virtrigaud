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
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// These tests pin the certificate renewal wire shape: RenewalRequest, carried
// as a host-signed SignedBlob in HostAnswer.pending_renewal, asked for by
// GetHostInfoRequest.renewal_version.

const renewalRequestMessageName = "provider.v1.RenewalRequest"

func fullRenewalRequest() *RenewalRequest {
	return &RenewalRequest{
		RenewalVersion:           1,
		ProviderUid:              "11111111-2222-3333-4444-555555555555",
		HostId:                   "host-a",
		InstallId:                fill(0x02, 16),
		EnrolmentId:              fill(0x03, 16),
		CurrentCertificateSha256: fill(0x40, 32),
		CertificateRequest:       arbitraryBytes(),
	}
}

func fullRenewalBlob() *SignedBlob {
	return &SignedBlob{
		Body:              arbitraryBytes(),
		Signature:         fill(0x41, 72),
		SignerCertificate: fill(0x31, 512),
		BodyType:          renewalRequestMessageName,
	}
}

func TestRenewalRequest_RoundTrip(t *testing.T) {
	r := fullRenewalRequest()
	assertEveryFieldSet(t, r)
	roundTrip(t, r)
	if got := roundTrip(t, &GetHostInfoRequest{HostId: "h"}).GetRenewalVersion(); got != 0 {
		t.Errorf("unset renewal_version = %d, want 0", got)
	}
	if got := roundTrip(t, &GetHostInfoRequest{HostId: "h", AnswerNonce: fill(0x01, 16), RenewalVersion: 1}).GetRenewalVersion(); got != 1 {
		t.Errorf("renewal_version = %d, want 1", got)
	}
	if (&HostAnswer{}).GetPendingRenewal() != nil {
		t.Error("an unset pending renewal is not nil")
	}
}

func TestRenewalRequest_FieldNumbersPinned(t *testing.T) {
	checkFields(t, (&RenewalRequest{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"renewal_version", 1, protoreflect.Uint32Kind, "", false},
		{"provider_uid", 2, protoreflect.StringKind, "", false},
		{"host_id", 3, protoreflect.StringKind, "", false},
		{"install_id", 4, protoreflect.BytesKind, "", false},
		{"enrolment_id", 5, protoreflect.BytesKind, "", false},
		{"current_certificate_sha256", 6, protoreflect.BytesKind, "", false},
		{"certificate_request", 7, protoreflect.BytesKind, "", false},
	}, true)
	if got := string((&RenewalRequest{}).ProtoReflect().Descriptor().FullName()); got != renewalRequestMessageName {
		t.Errorf("RenewalRequest full name = %q", got)
	}
}
