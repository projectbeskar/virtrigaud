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

package contracts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// OperationKind names one of the operations the manager can send as a signed
// operation (ADR-0016): the VM lifecycle kinds. The other operation kinds of
// the provider contract have no signed path in the manager
// (ErrOperationIdentityNotImplemented).
type OperationKind string

// The operation kinds the manager can send as signed operations.
const (
	// OperationKindCreate is Create.
	OperationKindCreate OperationKind = "Create"
	// OperationKindDelete is Delete.
	OperationKindDelete OperationKind = "Delete"
	// OperationKindPowerOn is Power with op On.
	OperationKindPowerOn OperationKind = "PowerOn"
	// OperationKindPowerOff is Power with op Off.
	OperationKindPowerOff OperationKind = "PowerOff"
	// OperationKindReboot is Power with op Reboot.
	OperationKindReboot OperationKind = "Reboot"
	// OperationKindShutdownGraceful is Power with op ShutdownGraceful.
	OperationKindShutdownGraceful OperationKind = "ShutdownGraceful"
	// OperationKindReconfigure is Reconfigure.
	OperationKindReconfigure OperationKind = "Reconfigure"
)

// ErrOperationIdentityRequired is returned by a Provider client that requires
// operation identity when a mutating call is made through the unsigned
// contracts.Provider methods: the call must go through
// OperationSender.BuildOperation and SendOperation. Nothing is sent.
var ErrOperationIdentityRequired = errors.New("the Provider requires operation identity: the call must be sent as a signed operation")

// ErrOperationIdentityNotImplemented is returned for a mutating call that the
// manager has no signed path for, made to a Provider that requires operation
// identity. Nothing is sent, signed or unsigned.
var ErrOperationIdentityNotImplemented = errors.New("the Provider requires operation identity, and the manager cannot send this operation with one")

// OperationEpoch is a manager epoch named in a grant: a counter that counts
// from 1 and a 16-byte random holder.
type OperationEpoch struct {
	// Counter is the epoch counter, at least 1.
	Counter uint64
	// Holder is the 16-byte random holder identity.
	Holder []byte
}

// OperationAttemptIdentity is what one operation attempt's grant names
// (ADR-0016 A1). A re-send of the same attempt reuses every field; a new
// attempt has a new OpID and a higher Attempt.
type OperationAttemptIdentity struct {
	// ProviderUID is the Provider's metadata.uid.
	ProviderUID string
	// HostID is the Host the operation runs on.
	HostID string
	// EnrolmentID is the host's 16-byte enrolment identifier.
	EnrolmentID []byte
	// Epoch is the epoch the grant is issued under.
	Epoch OperationEpoch
	// VMUID is the VirtualMachine's metadata.uid.
	VMUID string
	// BindingID is the VM placement's 16-byte identifier.
	BindingID []byte
	// OpID is the attempt's 16-byte identifier.
	OpID []byte
	// Attempt is the attempt number for this VM and kind, at least 1.
	Attempt uint64
	// MintEpoch is the epoch at which OpID was minted.
	MintEpoch OperationEpoch
}

// OperationRequest is one operation to send as a signed operation.
type OperationRequest struct {
	// Kind is the operation.
	Kind OperationKind
	// VM is the VM the operation acts on (every kind but Create).
	VM VMRef
	// Spec is the VM to create (Create) or its desired configuration
	// (Reconfigure); unused by the other kinds.
	Spec CreateRequest
}

// PreparedOperation is an operation serialised once, ready to be sent and
// re-sent: the exact request bytes the grant covers and the attempt's digest
// salt. A caller that persists it and re-sends it after a restart sends the
// same bytes. RequestBytes can carry secrets (cloud-init user data); a
// PreparedOperation is never logged; String, GoString, MarshalJSON and
// LogValue all render it without the bytes or the salt.
type PreparedOperation struct {
	// Kind is the operation.
	Kind OperationKind
	// Identity is what the grant names.
	Identity OperationAttemptIdentity
	// RequestBytes is the exact serialised request message.
	RequestBytes []byte
	// DigestSalt is the attempt's random digest salt.
	DigestSalt []byte
}

// String implements fmt.Stringer without the request bytes or the salt.
func (p PreparedOperation) String() string {
	return fmt.Sprintf("PreparedOperation{kind: %s, attempt: %d, request: %d bytes (redacted)}",
		p.Kind, p.Identity.Attempt, len(p.RequestBytes))
}

// GoString implements fmt.GoStringer the same way as String.
func (p PreparedOperation) GoString() string { return p.String() }

// preparedOperationRedacted is the only form in which a PreparedOperation is
// rendered as JSON or logged: the kind, the attempt and sizes, never the
// request bytes or the salt.
type preparedOperationRedacted struct {
	Kind         OperationKind `json:"kind"`
	Attempt      uint64        `json:"attempt"`
	RequestBytes int           `json:"requestBytes"`
	Redacted     bool          `json:"redacted"`
}

// redacted is p without its secrets.
func (p PreparedOperation) redacted() preparedOperationRedacted {
	return preparedOperationRedacted{Kind: p.Kind, Attempt: p.Identity.Attempt, RequestBytes: len(p.RequestBytes), Redacted: true}
}

// MarshalJSON implements json.Marshaler with the redacted form: the request
// bytes and the salt are never encoded. A PreparedOperation that must be
// stored is stored through an explicit encoder, not encoding/json.
func (p PreparedOperation) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(p.redacted())
	if err != nil {
		return nil, fmt.Errorf("marshal redacted prepared operation: %w", err)
	}
	return b, nil
}

// LogValue implements slog.LogValuer with the redacted form.
func (p PreparedOperation) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("kind", string(p.Kind)),
		slog.Uint64("attempt", p.Identity.Attempt),
		slog.Int("requestBytes", len(p.RequestBytes)),
		slog.Bool("redacted", true),
	)
}

// OperationAnswer is the Provider's answer to a signed operation. Like every
// Provider answer it is an unsigned claim.
type OperationAnswer struct {
	// ID is the provider's VM identifier (Create).
	ID string
	// TaskRef is the asynchronous task reference, when the provider returned
	// one.
	TaskRef string
	// RestartRequired is ReconfigureResult.RestartRequired (Reconfigure).
	RestartRequired bool
	// Honest is ReconfigureResult.Honest (Reconfigure).
	Honest bool
}

// OperationSender is an optional capability of a Provider client: it sends
// operations as signed operations (ADR-0016), in two phases so the caller can
// persist the exact bytes before the first send. Only a development build
// can send one; a release build refuses.
type OperationSender interface {
	// BuildOperation serialises req once and returns the bytes the grant will
	// cover, with a fresh digest salt. It sends nothing.
	BuildOperation(ctx context.Context, identity OperationAttemptIdentity, req OperationRequest) (PreparedOperation, error)
	// SendOperation mints a fresh grant over p's exact bytes and sends it on
	// the RPC p's kind names, with no other field of the outer request set.
	SendOperation(ctx context.Context, p PreparedOperation) (OperationAnswer, error)
}
