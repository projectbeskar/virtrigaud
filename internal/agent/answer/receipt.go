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

package answer

import (
	"bytes"
	"crypto"
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// BodyTypeOperationReceipt is the body type of a host's receipt for an
// operation it accepted (host ledger protocol). It is signed only by
// SignReceipt, never by Sign, and travels only in CreateResponse and
// TaskResponse.
const BodyTypeOperationReceipt = hostidentity.BodyTypeOperationReceipt

// ReceiptVersion is the receipt_version of the receipts this agent signs.
const ReceiptVersion uint32 = 1

// Receipt is what an operation receipt is made from.
type Receipt struct {
	// Record is the enrolment record of the host certificate: its Provider,
	// installation and enrolment are the receipt's, and must be the
	// operation's.
	Record hostidentity.Record
	// HostID is the host identity.
	HostID string
	// Op is the accepted operation, as the store decided on it.
	Op opstore.OpView
	// Acceptance is where the store accepted it and the attestation session
	// it held when it decided (opstore.Decision.Acceptance).
	Acceptance opstore.Acceptance
	// GrantDigest is grantverify.GrantDigest of the grant of this delivery.
	GrantDigest []byte
}

// OperationReceipt builds the OperationReceipt for r. It returns an error
// wrapping ErrSign when r breaks a field rule: the operation is not
// ACCEPTED, belongs to another enrolment or Provider than the certificate,
// or an identifier, digest, kind, attempt or position is malformed.
func OperationReceipt(r Receipt) (*providerv1.OperationReceipt, error) {
	op, acc := r.Op, r.Acceptance
	switch {
	case op.State != opstore.StateAccepted || op.Tombstoned:
		return nil, fmt.Errorf("%w: a receipt for an operation that is not accepted", ErrSign)
	case !bytes.Equal(op.Ref.EnrolmentID, r.Record.EnrolmentID) || len(op.Ref.EnrolmentID) != idLen:
		return nil, fmt.Errorf("%w: a receipt for another enrolment than the certificate's", ErrSign)
	case r.Record.ProviderUID == "" || !uidRE.MatchString(r.Record.ProviderUID) || r.HostID == "" || len(r.HostID) > hostIDMax:
		return nil, fmt.Errorf("%w: Provider UID or host malformed", ErrSign)
	case len(r.Record.InstallID) != idLen || len(op.Ref.OpID) != idLen || len(op.BindingID) != idLen:
		return nil, fmt.Errorf("%w: an identifier is not %d bytes", ErrSign, idLen)
	case op.Kind == providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED || providerv1.OperationKind_name[int32(op.Kind)] == "":
		return nil, fmt.Errorf("%w: operation kind %d", ErrSign, op.Kind)
	case op.Attempt == 0 || !uidRE.MatchString(op.VMUID) || len(op.IntentDigest) != digestLen:
		return nil, fmt.Errorf("%w: attempt, VM UID or intent digest malformed", ErrSign)
	case acc.Position.Seq == 0 || acc.Position.Seq != op.AcceptedSeq || acc.Position.StoreID == ([16]byte{}):
		return nil, fmt.Errorf("%w: the acceptance position is not the operation's", ErrSign)
	case len(acc.Session) != idLen:
		return nil, fmt.Errorf("%w: attestation session of %d bytes", ErrSign, len(acc.Session))
	case len(r.GrantDigest) != grantverify.DigestLen:
		return nil, fmt.Errorf("%w: grant digest of %d bytes", ErrSign, len(r.GrantDigest))
	}
	return &providerv1.OperationReceipt{
		ReceiptVersion: ReceiptVersion,
		ProviderUid:    r.Record.ProviderUID,
		HostId:         r.HostID,
		InstallId:      bytes.Clone(r.Record.InstallID),
		EnrolmentId:    bytes.Clone(op.Ref.EnrolmentID),
		AttestationId:  bytes.Clone(acc.Session),
		StoreId:        bytes.Clone(acc.Position.StoreID[:]),
		OpId:           bytes.Clone(op.Ref.OpID),
		Kind:           int32(op.Kind),
		Attempt:        op.Attempt,
		VmUid:          op.VMUID,
		BindingId:      bytes.Clone(op.BindingID),
		IntentDigest:   bytes.Clone(op.IntentDigest),
		AcceptedSeq:    acc.Position.Seq,
		AcceptedHead:   bytes.Clone(acc.Position.Head[:]),
		GrantDigest:    bytes.Clone(r.GrantDigest),
	}, nil
}

// hostIDMax bounds the host identity a receipt names.
const hostIDMax = 253

// SignReceipt builds the receipt for r (OperationReceipt) and signs it with
// signer, whose public key must be the ECDSA P-256 key certDER certifies,
// under the host-signed domain separator with body type
// BodyTypeOperationReceipt. The signature is verified before it is
// returned.
func SignReceipt(signer crypto.Signer, certDER []byte, r Receipt) (*providerv1.SignedBlob, error) {
	body, err := OperationReceipt(r)
	if err != nil {
		return nil, err
	}
	return signBody(signer, certDER, BodyTypeOperationReceipt, body, MaxBlobBytes)
}
