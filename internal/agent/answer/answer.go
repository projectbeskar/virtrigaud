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

// Package answer makes a per-host agent's signed host answers: the exact
// bytes of an answer body, signed by the host key under a fixed domain
// separator, in a provider.v1.SignedBlob that a Provider relays unchanged and
// a manager verifies without trusting the Provider.
//
// The signature input is the 26 bytes "virtrigaud.host-signed.v1" and a zero
// byte, then the length of body_type as a 2-byte big-endian integer, then
// body_type, then the length of body as a 4-byte big-endian integer, then
// body. The signature is ECDSA P-256 over the SHA-256 digest of that input,
// ASN.1 DER, low-S. The first byte of the input (0x76) is an invalid
// protobuf tag, so the input never parses as a message, and it differs from
// the first byte of what the same key signs in a TLS 1.3 handshake (0x20) or
// in a certificate request (0x30).
//
// An answer is made only for a request that carried a nonce, and only while
// the store's active enrolment is the one the host certificate names; an
// answer never carries request bytes, a secret or key material.
//
// It imports no Kubernetes package.
package answer

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/inventory"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// DomainSeparator starts the signature input of every message a host signs.
const DomainSeparator = "virtrigaud.host-signed.v1\x00"

// The closed set of body types.
const (
	// BodyTypeOperationAnswer is the body type of an answer about one
	// operation.
	BodyTypeOperationAnswer = "provider.v1.OperationAnswer"
	// BodyTypeVMAnswer is the body type of an answer about one VM placement.
	BodyTypeVMAnswer = "provider.v1.VMAnswer"
	// BodyTypeHostAnswer is the body type of an answer about the host.
	BodyTypeHostAnswer = "provider.v1.HostAnswer"
)

// Size limits of a signed answer.
const (
	// MaxBodyBytes bounds the body.
	MaxBodyBytes = 64 << 10
	// MaxBodyTypeBytes bounds the body type.
	MaxBodyTypeBytes = 64
	// MaxSignatureBytes bounds the signature.
	MaxSignatureBytes = 160
	// MaxCertBytes bounds the signer certificate.
	MaxCertBytes = 8192
	// MaxBlobBytes bounds the encoded SignedBlob in a response field.
	MaxBlobBytes = 74 << 10
	// NonceLen is the length of a request nonce.
	NonceLen = 16
)

// Versions this agent reports.
const (
	// AgentAPIVersion is the version of the agent's RPC surface. Version 2
	// adds the host inventory to the answer of GetHostInfo.
	AgentAPIVersion uint32 = 2
	// GrantVersion is the operation grant version the agent implements.
	GrantVersion uint32 = 1
)

// ErrNoAnswer is returned when no answer may be made: the request carried no
// nonce, the store has no active enrolment, or the active enrolment is not
// the one the host certificate names. The caller answers without a signed
// answer.
var ErrNoAnswer = errors.New("no signed answer can be made")

// ErrSign is wrapped by every signing failure.
var ErrSign = errors.New("sign host answer")

// ErrInvalidInventory is wrapped when an inventory breaks a field rule: the
// answer is not made with it, and nothing that breaks a rule is signed. The
// error also wraps inventory.ErrTooLarge when the cause is the size bound.
var ErrInvalidInventory = errors.New("host inventory breaks a field rule")

// SignatureInput returns the exact bytes a host signs for body of bodyType.
func SignatureInput(bodyType string, body []byte) ([]byte, error) {
	if len(bodyType) == 0 || len(bodyType) > MaxBodyTypeBytes {
		return nil, fmt.Errorf("%w: body type length %d", ErrSign, len(bodyType))
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("%w: %w: body of %d bytes exceeds %d", ErrSign, inventory.ErrTooLarge, len(body), MaxBodyBytes)
	}
	m := make([]byte, 0, len(DomainSeparator)+2+len(bodyType)+4+len(body))
	m = append(m, DomainSeparator...)
	m = binary.BigEndian.AppendUint16(m, uint16(len(bodyType))) // #nosec G115 -- bounded by MaxBodyTypeBytes above
	m = append(m, bodyType...)
	m = binary.BigEndian.AppendUint32(m, uint32(len(body))) // #nosec G115 -- bounded by MaxBodyBytes above
	m = append(m, body...)
	return m, nil
}

// bodyTypes is the closed set.
var bodyTypes = []string{BodyTypeOperationAnswer, BodyTypeVMAnswer, BodyTypeHostAnswer}

// Sign serialises body and signs it with signer, whose public key must be the
// ECDSA P-256 key certDER certifies. The signature is verified before it is
// returned.
func Sign(signer crypto.Signer, certDER []byte, body proto.Message) (*providerv1.SignedBlob, error) {
	bodyType := string(body.ProtoReflect().Descriptor().FullName())
	if !slices.Contains(bodyTypes, bodyType) {
		return nil, fmt.Errorf("%w: %s is not a host answer body", ErrSign, bodyType)
	}
	if len(certDER) == 0 || len(certDER) > MaxCertBytes {
		return nil, fmt.Errorf("%w: signer certificate of %d bytes", ErrSign, len(certDER))
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("%w: signer certificate does not parse", ErrSign)
	}
	pub, ok := signer.Public().(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() || !pub.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("%w: the signer is not the ECDSA P-256 key of the certificate", ErrSign)
	}
	if ha, ok := body.(*providerv1.HostAnswer); ok && ha.GetInventory() != nil && !guard.Off(guard.InventoryValidate) {
		// Whatever built the answer, an inventory that breaks a field rule is
		// never signed.
		if err := inventory.Validate(ha.GetInventory(), ha.GetInventory().GetRequestedVersion()); err != nil {
			return nil, fmt.Errorf("%w: %w: %w", ErrSign, ErrInvalidInventory, err)
		}
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal body: %v", ErrSign, err)
	}
	m, err := SignatureInput(bodyType, raw)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(m)
	sig, err := signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSign, err)
	}
	if sig, err = grantverify.NormalizeLowS(sig, elliptic.P256()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSign, err)
	}
	if len(sig) > MaxSignatureBytes || !ecdsa.VerifyASN1(pub, digest[:], sig) {
		return nil, fmt.Errorf("%w: the signature does not verify", ErrSign)
	}
	blob := &providerv1.SignedBlob{Body: raw, Signature: sig, SignerCertificate: bytes.Clone(certDER), BodyType: bodyType}
	if proto.Size(blob) > MaxBlobBytes {
		return nil, fmt.Errorf("%w: %w: signed answer exceeds %d bytes", ErrSign, inventory.ErrTooLarge, MaxBlobBytes)
	}
	return blob, nil
}

// reasonRE is the shape of a state reason.
var reasonRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// lostReasons are the state reasons of a quarantined store.
var lostReasons = []journal.LostReason{
	journal.LostJournalCorrupt, journal.LostRolledBack, journal.LostInconsistentStore, journal.LostAnchorMissing,
	journal.LostAnchorCorrupt, journal.LostJournalMissing, journal.LostIOError,
}

// StoreState maps the journal's state and the operation store's recovery to
// an answer's agent state and state reason: READY only when the journal is
// ready and recovery has run; QUARANTINED, with the loss reason, when the
// journal is lost; UNATTESTED otherwise.
func StoreState(st journal.State, lost journal.LostReason, recovered bool) (providerv1.AgentState, string) {
	switch {
	case st == journal.StateLost:
		return providerv1.AgentState_AGENT_STATE_QUARANTINED, string(lost)
	case st == journal.StateReady && recovered:
		return providerv1.AgentState_AGENT_STATE_READY, ""
	}
	return providerv1.AgentState_AGENT_STATE_UNATTESTED, ""
}

// Host is what a host answer is made from.
type Host struct {
	// Nonce is the request's answer_nonce.
	Nonce []byte
	// View is the store's view, read in one transaction.
	View opstore.HostView
	// HostID is the host identity the answer names.
	HostID string
	// Record is the enrolment record of the host certificate.
	Record hostidentity.Record
	// State and StateReason are the store's trust state (StoreState).
	State       providerv1.AgentState
	StateReason string
	// AgentVersion is the agent's build version.
	AgentVersion string
	// JournalSchemaVersion is the store's schema version.
	JournalSchemaVersion uint32
	// ClockFloor is the host's clock floor; zero when it has none.
	ClockFloor time.Time
	// StagedCertificateSHA256 names a staged renewed certificate; nil when
	// none is staged.
	StagedCertificateSHA256 []byte
	// Now is the host's clock, informational.
	Now time.Time
	// InventoryRequested is the request's inventory_version; 0 when it asked
	// for none.
	InventoryRequested uint32
	// Inventory is the host inventory for a request that asked for one
	// (Collector.Inventory); nil answers without one, which a manager treats
	// as no inventory. It must be nil when InventoryRequested is 0.
	Inventory *providerv1.HostInventory
}

// HostAnswer builds the HostAnswer for h. It returns ErrNoAnswer when h has no
// 16-byte nonce, when the store has no active enrolment, or when the active
// enrolment is not the one the host certificate names (enrolment, Provider,
// installation and host all equal). It returns ErrInvalidInventory when h
// carries an inventory for a request that asked for none, or one that breaks
// an inventory field rule.
func HostAnswer(h Host) (*providerv1.HostAnswer, error) {
	ctx, err := answerContext(h)
	if err != nil {
		return nil, err
	}
	a := &providerv1.HostAnswer{
		Context:                 ctx,
		AgentVersion:            h.AgentVersion,
		AgentApiVersion:         AgentAPIVersion,
		GrantVersions:           []uint32{GrantVersion},
		Mode:                    providerv1.AgentMode_AGENT_MODE_IDENTITY,
		JournalSchemaVersion:    h.JournalSchemaVersion,
		StagedCertificateSha256: bytes.Clone(h.StagedCertificateSHA256),
	}
	if !h.ClockFloor.IsZero() {
		a.ClockFloor = timestamppb.New(h.ClockFloor)
	}
	if len(a.StagedCertificateSha256) != 0 && len(a.StagedCertificateSha256) != sha256.Size {
		return nil, fmt.Errorf("%w: staged certificate digest is not %d bytes", ErrSign, sha256.Size)
	}
	if h.Inventory != nil {
		if h.InventoryRequested == 0 && !guard.Off(guard.InventoryOnlyWhenAsked) {
			return nil, fmt.Errorf("%w: an inventory for a request that asked for none", ErrInvalidInventory)
		}
		if h.InventoryRequested != 0 && !guard.Off(guard.InventoryValidate) {
			if err := inventory.Validate(h.Inventory, h.InventoryRequested); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalidInventory, err)
			}
		}
		a.Inventory = proto.CloneOf(h.Inventory)
	}
	return a, nil
}

// answerContext builds the AnswerContext of h, enforcing when an answer may
// be made and the field rules of the context.
func answerContext(h Host) (*providerv1.AnswerContext, error) {
	if len(h.Nonce) != NonceLen {
		return nil, fmt.Errorf("%w: the request carried no %d-byte nonce", ErrNoAnswer, NonceLen)
	}
	e := h.View.Enrolment
	if e == nil {
		return nil, fmt.Errorf("%w: the store has no active enrolment", ErrNoAnswer)
	}
	if h.Record.Mode != hostidentity.ModeIdentity {
		return nil, fmt.Errorf("%w: the host certificate names no supported mode", ErrNoAnswer)
	}
	if !bytes.Equal(e.ID, h.Record.EnrolmentID) || e.ProviderUID != h.Record.ProviderUID ||
		!bytes.Equal(e.InstallID, h.Record.InstallID) || e.HostID != h.HostID {
		return nil, fmt.Errorf("%w: the store's active enrolment is not the one the host certificate names", ErrNoAnswer)
	}
	pos, err := h.View.Position.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("%w: chain position: %v", ErrSign, err)
	}
	if h.View.Position.Seq == 0 || h.View.Position.StoreID == ([16]byte{}) {
		return nil, fmt.Errorf("%w: the store has no chain position", ErrNoAnswer)
	}
	switch h.State {
	case providerv1.AgentState_AGENT_STATE_QUARANTINED:
		if !slices.Contains(lostReasons, journal.LostReason(h.StateReason)) {
			return nil, fmt.Errorf("%w: quarantine reason %q is not a store loss reason", ErrSign, h.StateReason)
		}
	case providerv1.AgentState_AGENT_STATE_READY, providerv1.AgentState_AGENT_STATE_UNATTESTED:
		if h.StateReason != "" {
			return nil, fmt.Errorf("%w: a state reason without a quarantine", ErrSign)
		}
	default:
		return nil, fmt.Errorf("%w: agent state %v", ErrSign, h.State)
	}
	if h.StateReason != "" && !reasonRE.MatchString(h.StateReason) {
		return nil, fmt.Errorf("%w: state reason %q is malformed", ErrSign, h.StateReason)
	}
	ctx := &providerv1.AnswerContext{
		Nonce:          bytes.Clone(h.Nonce),
		ProviderUid:    e.ProviderUID,
		HostId:         e.HostID,
		InstallId:      bytes.Clone(e.InstallID),
		EnrolmentId:    bytes.Clone(e.ID),
		ChainPosition:  pos,
		AgentState:     h.State,
		StateReason:    h.StateReason,
		AgentTime:      timestamppb.New(h.Now),
		ContinuityHead: bytes.Clone(h.View.ContinuityHead),
		CheckpointSeq:  h.View.CheckpointSeq,
	}
	if h.View.CheckpointSeq > 0 {
		ctx.CheckpointHead = bytes.Clone(h.View.CheckpointHead[:])
	}
	return ctx, nil
}
