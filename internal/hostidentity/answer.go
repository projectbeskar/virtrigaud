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

package hostidentity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Signed host answers. A per-host agent signs an answer about one operation,
// one VM placement or itself with its enrolled host key; the Provider relays
// it unchanged; the manager verifies it here without trusting the Provider.
// The agent (internal/agent/answer) builds the signature input with
// AnswerSignatureInput, so both sides frame the signed bytes the same way.

// AnswerDomainSeparator starts the signature input of every message a host
// signs: the 25 ASCII bytes "virtrigaud.host-signed.v1" and a zero byte.
const AnswerDomainSeparator = "virtrigaud.host-signed.v1\x00"

// The closed set of body types of a signed host answer. Each is the protobuf
// full name of its message.
const (
	// BodyTypeOperationAnswer answers TaskStatus: one operation.
	BodyTypeOperationAnswer = "provider.v1.OperationAnswer"
	// BodyTypeVMAnswer answers Describe: one VM placement.
	BodyTypeVMAnswer = "provider.v1.VMAnswer"
	// BodyTypeHostAnswer answers GetHostInfo: the host.
	BodyTypeHostAnswer = "provider.v1.HostAnswer"
)

// Size limits of a signed host answer.
const (
	// MaxAnswerBodyBytes bounds the body.
	MaxAnswerBodyBytes = 64 << 10
	// MaxAnswerBodyTypeBytes bounds the body type.
	MaxAnswerBodyTypeBytes = 64
	// MaxAnswerSignatureBytes bounds the signature.
	MaxAnswerSignatureBytes = 160
	// MaxAnswerCertBytes bounds the signer certificate.
	MaxAnswerCertBytes = 8192
	// MaxAnswerBlobBytes bounds the encoded SignedBlob in a response field.
	MaxAnswerBlobBytes = 74 << 10
	// MaxAnswerDetailBytes bounds the encoded SignedBlob in a status detail.
	MaxAnswerDetailBytes = 16 << 10
	// AnswerNonceLen is the length of a request nonce.
	AnswerNonceLen = 16
	// ChainPositionLen is the length of a chain position: store id (16),
	// big-endian sequence number (8), head (32).
	ChainPositionLen = 16 + 8 + 32
	// CheckpointInterval is the checkpoint interval of an identity-mode
	// agent's store: it keeps the head of each of its last CheckpointInterval
	// commits and every checkpoint head.
	CheckpointInterval = 1024
)

// QuarantineReasons are the state reasons of a quarantined agent store, the
// only values an answer may carry in state_reason.
var QuarantineReasons = []string{
	"journal_corrupt", "rolled_back", "inconsistent_store", "anchor_missing", "anchor_corrupt", "journal_missing", "io_error",
}

// ErrAnswer is wrapped by every error VerifyAnswer returns. A caller treats
// any of them as "no answer": never negative, never positive.
var ErrAnswer = errors.New("host answer refused")

// answerBodyTypes is the closed set.
var answerBodyTypes = []string{BodyTypeOperationAnswer, BodyTypeVMAnswer, BodyTypeHostAnswer}

// AnswerSignatureInput returns the exact bytes a host signs for body of
// bodyType: AnswerDomainSeparator, the length of bodyType as a 2-byte
// big-endian integer, bodyType, the length of body as a 4-byte big-endian
// integer, body.
func AnswerSignatureInput(bodyType string, body []byte) ([]byte, error) {
	if len(bodyType) == 0 || len(bodyType) > MaxAnswerBodyTypeBytes {
		return nil, fmt.Errorf("body type of %d bytes", len(bodyType))
	}
	if len(body) > MaxAnswerBodyBytes {
		return nil, fmt.Errorf("body of %d bytes exceeds %d", len(body), MaxAnswerBodyBytes)
	}
	m := make([]byte, 0, len(AnswerDomainSeparator)+2+len(bodyType)+4+len(body))
	m = append(m, AnswerDomainSeparator...)
	m = binary.BigEndian.AppendUint16(m, uint16(len(bodyType))) // #nosec G115 -- bounded above
	m = append(m, bodyType...)
	m = binary.BigEndian.AppendUint32(m, uint32(len(body))) // #nosec G115 -- bounded above
	return append(m, body...), nil
}

// AnswerExpectation is what the manager expects of one answer: everything a
// verified answer must match comes from the manager's own state for the
// request it made, never from the answer or the Provider.
type AnswerExpectation struct {
	// BodyType is the body type the request's RPC answers with.
	BodyType string
	// Anchors are the host-agent subordinate CA certificates.
	Anchors *x509.CertPool
	// HostURI is the URI identity of the host the manager chose for the
	// request (HostURI of the trust domain, the Host's namespace and name).
	HostURI string
	// HostName is the Host's name (the only common name a host certificate
	// may carry).
	HostName string
	// HostID is the host identity the answer must name.
	HostID string
	// Pins are the Host's pinned agent keys (SPKIPin).
	Pins []string
	// ProviderUID is the Provider's metadata.uid.
	ProviderUID string
	// InstallID is the manager installation's 16-byte identifier.
	InstallID []byte
	// EnrolmentID is the Host's current 16-byte enrolment identifier.
	EnrolmentID []byte
	// Nonce is the 16-byte nonce the manager drew for this request.
	Nonce []byte
	// Now is the manager's clock.
	Now time.Time
}

// ChainPosition is a position in a host's operation store.
type ChainPosition struct {
	// StoreID identifies the store.
	StoreID [16]byte
	// Seq is the sequence number of the last commit, at least 1.
	Seq uint64
	// Head is the chain head at Seq.
	Head [32]byte
}

// VerifiedAnswer is an answer that passed VerifyAnswer. Exactly one of
// Operation, VM and Host is set, by body type.
type VerifiedAnswer struct {
	// Context is the answer's context.
	Context *providerv1.AnswerContext
	// Position is Context.chain_position decoded.
	Position ChainPosition
	// Operation is the body of an OperationAnswer.
	Operation *providerv1.OperationAnswer
	// VM is the body of a VMAnswer.
	VM *providerv1.VMAnswer
	// Host is the body of a HostAnswer.
	Host *providerv1.HostAnswer
	// SignerPin is the pin of the signer's key.
	SignerPin string
	// BlobSHA256 is the SHA-256 of the deterministic encoding of the
	// SignedBlob (for audit).
	BlobSHA256 [32]byte
}

// Ready reports whether the answer's agent state is READY: only such an
// answer can be definite.
func (v VerifiedAnswer) Ready() bool {
	return v.Context.GetAgentState() == providerv1.AgentState_AGENT_STATE_READY
}

// refuse wraps ErrAnswer with what failed.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAnswer, fmt.Sprintf(format, args...))
}

// VerifyAnswer verifies blob against exp, in this order, each failure being
// "no answer": sizes; no unknown field in the SignedBlob; the body type is in
// the closed set and is the one exp names; the signer certificate is one DER
// ECDSA P-256 certificate with the agent certificate profile
// (CheckLeafProfile) whose URI subject alternative names are exactly the
// host URI exp names and one enrolment record; it chains to exp.Anchors for
// server authentication at exp.Now; its key is one of exp.Pins; the
// signature over AnswerSignatureInput is a low-S DER ECDSA signature that
// verifies; the body decodes with no unknown field in it or any nested
// message, no undefined enum value, and every field rule of its type; the
// nonce is exp.Nonce; the Provider UID, host, installation and enrolment are
// exp's and the certificate's record's. Nothing is decoded before the
// signature verifies.
func VerifyAnswer(blob *providerv1.SignedBlob, exp AnswerExpectation) (VerifiedAnswer, error) {
	if blob == nil {
		return VerifiedAnswer{}, refuse("no signed answer")
	}
	// Sizes.
	switch {
	case len(blob.GetBodyType()) == 0 || len(blob.GetBodyType()) > MaxAnswerBodyTypeBytes:
		return VerifiedAnswer{}, refuse("body type size")
	case len(blob.GetBody()) > MaxAnswerBodyBytes:
		return VerifiedAnswer{}, refuse("body size")
	case len(blob.GetSignature()) == 0 || len(blob.GetSignature()) > MaxAnswerSignatureBytes:
		return VerifiedAnswer{}, refuse("signature size")
	case len(blob.GetSignerCertificate()) == 0 || len(blob.GetSignerCertificate()) > MaxAnswerCertBytes:
		return VerifiedAnswer{}, refuse("signer certificate size")
	case proto.Size(blob) > MaxAnswerBlobBytes:
		return VerifiedAnswer{}, refuse("signed answer size")
	}
	// No unknown field.
	if len(blob.ProtoReflect().GetUnknown()) != 0 {
		return VerifiedAnswer{}, refuse("the signed answer carries unknown fields")
	}
	// Body type.
	if !slices.Contains(answerBodyTypes, blob.GetBodyType()) || blob.GetBodyType() != exp.BodyType {
		return VerifiedAnswer{}, refuse("body type %q is not the one this request answers with", blob.GetBodyType())
	}
	// Signer certificate: profile, names, chain and pin.
	pub, rec, pin, err := verifySigner(blob.GetSignerCertificate(), exp)
	if err != nil {
		return VerifiedAnswer{}, err
	}
	// Signature.
	if err := verifyAnswerSignature(pub, blob); err != nil {
		return VerifiedAnswer{}, err
	}
	// Body.
	out := VerifiedAnswer{SignerPin: pin}
	var body proto.Message
	switch exp.BodyType {
	case BodyTypeOperationAnswer:
		out.Operation = &providerv1.OperationAnswer{}
		body = out.Operation
	case BodyTypeVMAnswer:
		out.VM = &providerv1.VMAnswer{}
		body = out.VM
	default:
		out.Host = &providerv1.HostAnswer{}
		body = out.Host
	}
	if err := proto.Unmarshal(blob.GetBody(), body); err != nil {
		return VerifiedAnswer{}, refuse("the body does not decode as %s", exp.BodyType)
	}
	if err := checkMessage(body.ProtoReflect()); err != nil {
		return VerifiedAnswer{}, err
	}
	// One encoding per answer: the body must be exactly the deterministic
	// encoding of what it decodes to (the agent signs that encoding), so no
	// repeated singular field or other non-canonical bytes ride along into
	// the logged answer.
	if canon, err := (proto.MarshalOptions{Deterministic: true}).Marshal(body); err != nil || !bytes.Equal(canon, blob.GetBody()) {
		return VerifiedAnswer{}, refuse("the body is not canonically encoded")
	}
	switch {
	case out.Operation != nil:
		out.Context, err = out.Operation.GetContext(), checkOperationAnswer(out.Operation)
	case out.VM != nil:
		out.Context, err = out.VM.GetContext(), checkVMAnswer(out.VM)
	default:
		out.Context, err = out.Host.GetContext(), checkHostAnswer(out.Host)
	}
	if err != nil {
		return VerifiedAnswer{}, err
	}
	pos, err := checkContext(out.Context)
	if err != nil {
		return VerifiedAnswer{}, err
	}
	out.Position = pos
	// Nonce.
	if len(exp.Nonce) != AnswerNonceLen || subtle.ConstantTimeCompare(out.Context.GetNonce(), exp.Nonce) != 1 {
		return VerifiedAnswer{}, refuse("the answer does not carry this request's nonce")
	}
	// Bindings to the manager's own state and the certificate.
	ctx := out.Context
	switch {
	case ctx.GetProviderUid() != exp.ProviderUID || rec.ProviderUID != exp.ProviderUID:
		return VerifiedAnswer{}, refuse("the answer names another Provider")
	case ctx.GetHostId() != exp.HostID:
		return VerifiedAnswer{}, refuse("the answer names another host")
	case !bytes.Equal(ctx.GetInstallId(), exp.InstallID) || !bytes.Equal(rec.InstallID, exp.InstallID):
		return VerifiedAnswer{}, refuse("the answer names another installation")
	case !bytes.Equal(ctx.GetEnrolmentId(), exp.EnrolmentID) || !bytes.Equal(rec.EnrolmentID, exp.EnrolmentID):
		return VerifiedAnswer{}, refuse("the answer names another enrolment")
	case out.Host != nil && (out.Host.GetMode() != providerv1.AgentMode_AGENT_MODE_IDENTITY || rec.Mode != ModeIdentity):
		return VerifiedAnswer{}, refuse("the answer's mode is not the certificate's identity mode")
	}
	enc, err := proto.MarshalOptions{Deterministic: true}.Marshal(blob)
	if err != nil {
		return VerifiedAnswer{}, refuse("encode the signed answer")
	}
	out.BlobSHA256 = sha256.Sum256(enc)
	return out, nil
}

// verifySigner checks the signer certificate and returns its key,
// its enrolment record and its key pin.
func verifySigner(der []byte, exp AnswerExpectation) (*ecdsa.PublicKey, Record, string, error) {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, Record{}, "", refuse("the signer certificate is not one DER certificate")
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, Record{}, "", refuse("the signer key is not ECDSA P-256")
	}
	if err := CheckLeafProfile(leaf, exp.HostName); err != nil {
		return nil, Record{}, "", refuse("the signer certificate: %v", err)
	}
	rec, err := RecordFromURIs(leaf.URIs, exp.HostURI)
	if err != nil {
		return nil, Record{}, "", refuse("the signer certificate's URI names are not the host's: %v", err)
	}
	if exp.Anchors == nil {
		return nil, Record{}, "", refuse("no host-agent CA is configured")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: exp.Anchors, CurrentTime: exp.Now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, Record{}, "", refuse("the signer certificate does not chain to the host-agent CA")
	}
	pin, err := SPKIPin(pub)
	if err != nil || !slices.Contains(exp.Pins, pin) {
		return nil, Record{}, "", refuse("the signer key is not pinned for the host")
	}
	return pub, rec, pin, nil
}

// verifyAnswerSignature checks a low-S, exactly DER encoded ECDSA
// signature over SHA-256 of the signature input.
func verifyAnswerSignature(pub *ecdsa.PublicKey, blob *providerv1.SignedBlob) error {
	sig := blob.GetSignature()
	parsed, err := grantverify.ParseECDSASignature(sig)
	if err != nil {
		return refuse("the signature is not a DER ECDSA signature")
	}
	if again, err := asn1.Marshal(parsed); err != nil || !bytes.Equal(again, sig) {
		return refuse("the signature is not exactly DER encoded")
	}
	if !grantverify.IsLowS(sig, elliptic.P256()) {
		return refuse("the signature is not low-S")
	}
	m, err := AnswerSignatureInput(blob.GetBodyType(), blob.GetBody())
	if err != nil {
		return refuse("signature input: %v", err)
	}
	d := sha256.Sum256(m)
	if !ecdsa.VerifyASN1(pub, d[:], sig) {
		return refuse("the signature does not verify")
	}
	return nil
}

// checkMessage refuses unknown fields and undefined enum values in m and in
// every message nested in it.
func checkMessage(m protoreflect.Message) error {
	if len(m.GetUnknown()) != 0 {
		return refuse("%s carries unknown fields", m.Descriptor().FullName())
	}
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		err = checkValue(fd, v)
		return err == nil
	})
	return err
}

// checkValue checks one field value of a message (see checkMessage).
func checkValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	one := func(x protoreflect.Value) error {
		switch fd.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			return checkMessage(x.Message())
		case protoreflect.EnumKind:
			if fd.Enum().Values().ByNumber(x.Enum()) == nil {
				return refuse("%s has an undefined value %d", fd.FullName(), x.Enum())
			}
		}
		return nil
	}
	switch {
	case fd.IsList():
		l := v.List()
		for i := 0; i < l.Len(); i++ {
			if err := one(l.Get(i)); err != nil {
				return err
			}
		}
		return nil
	case fd.IsMap():
		var err error
		v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				err = checkMessage(mv.Message())
			}
			return err == nil
		})
		return err
	}
	return one(v)
}

// reasonRE is the shape of a state or outcome reason.
var reasonRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// answerUIDRE is the shape of a Provider or VM UID in an answer.
var answerUIDRE = regexp.MustCompile(`^[0-9a-z-]{1,128}$`)

// checkContext checks the field rules of an answer context and decodes its
// chain position.
func checkContext(c *providerv1.AnswerContext) (ChainPosition, error) {
	var pos ChainPosition
	switch {
	case c == nil:
		return pos, refuse("the answer has no context")
	case len(c.GetNonce()) != AnswerNonceLen || len(c.GetInstallId()) != IDLen || len(c.GetEnrolmentId()) != IDLen:
		return pos, refuse("a context identifier is not %d bytes", IDLen)
	case !answerUIDRE.MatchString(c.GetProviderUid()) || c.GetHostId() == "":
		return pos, refuse("the context's Provider UID or host is malformed")
	case len(c.GetChainPosition()) != ChainPositionLen:
		return pos, refuse("the chain position is not %d bytes", ChainPositionLen)
	case len(c.GetContinuityHead()) != 0 && len(c.GetContinuityHead()) != sha256.Size:
		return pos, refuse("the continuity head is malformed")
	case c.GetAgentState() == providerv1.AgentState_AGENT_STATE_UNSPECIFIED:
		return pos, refuse("the agent state is unspecified")
	case c.GetStateReason() != "" && !reasonRE.MatchString(c.GetStateReason()):
		return pos, refuse("the state reason is malformed")
	case (c.GetAgentState() == providerv1.AgentState_AGENT_STATE_QUARANTINED) !=
		(c.GetStateReason() != "" && slices.Contains(QuarantineReasons, c.GetStateReason())):
		return pos, refuse("the state reason does not match the agent state")
	}
	raw := c.GetChainPosition()
	copy(pos.StoreID[:], raw[:16])
	pos.Seq = binary.BigEndian.Uint64(raw[16:24])
	copy(pos.Head[:], raw[24:])
	switch {
	case pos.Seq == 0 || pos.StoreID == [16]byte{}:
		return pos, refuse("the chain position has no store or sequence number")
	case c.GetCheckpointSeq() > pos.Seq:
		return pos, refuse("the checkpoint is above the position")
	case (c.GetCheckpointSeq() > 0) != (len(c.GetCheckpointHead()) == sha256.Size) ||
		(c.GetCheckpointSeq() == 0 && len(c.GetCheckpointHead()) != 0):
		return pos, refuse("the checkpoint head is malformed")
	}
	return pos, nil
}

// checkOperationAnswer checks the field rules of an OperationAnswer.
func checkOperationAnswer(a *providerv1.OperationAnswer) error {
	if len(a.GetOpId()) != IDLen || len(a.GetOpEnrolmentId()) != IDLen {
		return refuse("op_id or op_enrolment_id is not %d bytes", IDLen)
	}
	if a.GetOutcomeReason() != "" && !reasonRE.MatchString(a.GetOutcomeReason()) {
		return refuse("the outcome reason is malformed")
	}
	switch s := a.GetState(); s {
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNSPECIFIED:
		return refuse("the operation state is unspecified")
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_NO_RECORD,
		providerv1.OperationRecordState_OPERATION_RECORD_STATE_UNKNOWN_TO_AGENT:
		if a.GetKind() != 0 || a.GetAttempt() != 0 || a.GetVmUid() != "" || len(a.GetBindingId()) != 0 ||
			len(a.GetIntentDigest()) != 0 || a.GetTombstoned() || a.GetOutcomeReason() != "" {
			return refuse("an answer without a record carries record fields")
		}
		return nil
	}
	kind := providerv1.OperationKind(a.GetKind())
	switch {
	case kind == providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED || kind.Descriptor().Values().ByNumber(protoreflect.EnumNumber(kind)) == nil:
		return refuse("the operation kind is not defined")
	case a.GetAttempt() == 0:
		return refuse("the attempt is 0")
	case !answerUIDRE.MatchString(a.GetVmUid()):
		return refuse("the VM UID is malformed")
	case len(a.GetBindingId()) != IDLen || len(a.GetIntentDigest()) != sha256.Size:
		return refuse("the binding id or intent digest is malformed")
	case a.GetTombstoned() && !terminalRecordState(a.GetState()):
		return refuse("a tombstone of a state that is not terminal")
	}
	return nil
}

// terminalRecordState reports whether s is a state a tombstone may record.
func terminalRecordState(s providerv1.OperationRecordState) bool {
	switch s {
	case providerv1.OperationRecordState_OPERATION_RECORD_STATE_COMPLETED, providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_CLEAN,
		providerv1.OperationRecordState_OPERATION_RECORD_STATE_FAILED_DIRTY, providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED:
		return true
	}
	return false
}

// checkVMAnswer checks the field rules of a VMAnswer.
func checkVMAnswer(a *providerv1.VMAnswer) error {
	deleted := a.GetPresence() == providerv1.VMPresence_VM_PRESENCE_DELETED
	switch {
	case a.GetPresence() == providerv1.VMPresence_VM_PRESENCE_UNSPECIFIED:
		return refuse("the VM presence is unspecified")
	case !answerUIDRE.MatchString(a.GetVmUid()) || len(a.GetBindingId()) != IDLen:
		return refuse("the VM UID or binding id is malformed")
	case deleted && (len(a.GetDeleteOpId()) != IDLen || len(a.GetDeleteEnrolmentId()) != IDLen || a.GetDeleteSeq() == 0):
		return refuse("a deleted placement without its Delete")
	case !deleted && (len(a.GetDeleteOpId()) != 0 || len(a.GetDeleteEnrolmentId()) != 0 || a.GetDeleteSeq() != 0):
		return refuse("a placement that is not deleted names a Delete")
	}
	return nil
}

// checkHostAnswer checks the field rules of a HostAnswer. The manager asks
// for no inventory, so an answer that carries one is refused.
func checkHostAnswer(a *providerv1.HostAnswer) error {
	gv := a.GetGrantVersions()
	switch {
	case len(gv) == 0:
		return refuse("no grant version")
	case a.GetAgentApiVersion() == 0:
		return refuse("agent API version 0")
	case a.GetMode() == providerv1.AgentMode_AGENT_MODE_UNSPECIFIED:
		return refuse("the agent mode is unspecified")
	case len(a.GetStagedCertificateSha256()) != 0 && len(a.GetStagedCertificateSha256()) != sha256.Size:
		return refuse("the staged certificate digest is malformed")
	case a.GetInventory() != nil:
		return refuse("an inventory the request did not ask for")
	}
	for i, v := range gv {
		if v == 0 || (i > 0 && v <= gv[i-1]) {
			return refuse("the grant versions are not strictly ascending from 1")
		}
	}
	return nil
}
