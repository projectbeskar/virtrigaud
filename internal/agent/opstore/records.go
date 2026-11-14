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

package opstore

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Buckets of the operation store.
const (
	bucketMeta         = "meta"
	bucketEnrolments   = "enrolments"
	bucketOps          = "ops"
	bucketTombstones   = "tombstones"
	bucketVMs          = "vms"
	bucketVMTombstones = "vm_tombstones"
	bucketAttempts     = "attempts"
	bucketAudit        = "audit"
	idLen              = 16
	digestLen          = 32
	witnessDirName     = "witness"
	maxNameLen         = 256
	maxHolderLen       = 64
)

// keyAgentMeta is the key of the AgentMeta record in bucketMeta.
var keyAgentMeta = []byte("agent")

// keyFreeze is the key of the freeze-in-progress marker in bucketMeta. It
// exists exactly while a re-enrolment's freeze of the predecessor's open
// operations has more chunks to commit.
var keyFreeze = []byte("freeze")

// keyClockFloor is the key of the clock floor record in bucketMeta.
var keyClockFloor = []byte("clock_floor")

// buckets lists every bucket of the operation store.
func buckets() []string {
	return []string{bucketMeta, bucketEnrolments, bucketOps, bucketTombstones, bucketVMs, bucketVMTombstones,
		bucketAttempts, bucketAudit}
}

// marshal is the deterministic encoding used for every record, so that the
// chain hashes the same bytes for the same record.
var marshal = proto.MarshalOptions{Deterministic: true}

// errCorruptRecord reports a record that does not decode.
var errCorruptRecord = errors.New("operation store record does not decode")

// put encodes m and writes it.
func put(tx *journal.Tx, bucket string, key []byte, m proto.Message) error {
	b, err := marshal.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode %s record: %w", bucket, err)
	}
	return tx.Put(bucket, key, b)
}

// get decodes the record at key into m; it reports false when absent.
func get(tx *journal.Tx, bucket string, key []byte, m proto.Message) (bool, error) {
	b, err := tx.Get(bucket, key)
	if err != nil {
		return false, fmt.Errorf("read %s record: %w", bucket, err)
	}
	if b == nil {
		return false, nil
	}
	if err := proto.Unmarshal(b, m); err != nil {
		return false, fmt.Errorf("%w: %s: %w", errCorruptRecord, bucket, err)
	}
	return true, nil
}

// has reports whether bucket holds key. A record that does not verify is an
// error, never "absent".
func has(tx *journal.Tx, bucket string, key []byte) (bool, error) {
	b, err := tx.Get(bucket, key)
	if err != nil {
		return false, fmt.Errorf("read %s record: %w", bucket, err)
	}
	return b != nil, nil
}

// unmarshalRecord decodes one record value.
func unmarshalRecord(b []byte, m proto.Message) error {
	if err := proto.Unmarshal(b, m); err != nil {
		return fmt.Errorf("%w: %w", errCorruptRecord, err)
	}
	return nil
}

// opKey is enrolment_id || op_id.
func opKey(enrolment, opID []byte) []byte {
	k := make([]byte, 0, 2*idLen)
	return append(append(k, enrolment...), opID...)
}

// lp is a length-prefixed string: be16(len) || s.
func lp(s string) []byte {
	b := make([]byte, 2, 2+len(s))
	binary.BigEndian.PutUint16(b, uint16(len(s))) // #nosec G115 -- vm_uid lengths are checked below 64 KiB at admission
	return append(b, s...)
}

// vmTombstoneKey is lp(vm_uid) || binding_id. VM tombstones are not scoped
// to an enrolment: a deleted placement stays deleted across re-enrolments.
func vmTombstoneKey(vmUID string, binding []byte) []byte {
	return append(lp(vmUID), binding...)
}

// attemptKey is enrolment_id || lp(vm_uid) || be32(kind).
func attemptKey(enrolment []byte, vmUID string, kind int32) []byte {
	k := append(append([]byte(nil), enrolment...), lp(vmUID)...)
	var kb [4]byte
	binary.BigEndian.PutUint32(kb[:], uint32(kind)) // #nosec G115 -- kinds are small non-negative enum numbers
	return append(k, kb[:]...)
}

// OpState is the state of an operation at the agent.
type OpState = pb.OpState

// The operation states.
const (
	StateAccepted       = pb.OpState_OP_STATE_ACCEPTED
	StateExecuting      = pb.OpState_OP_STATE_EXECUTING
	StateCompleted      = pb.OpState_OP_STATE_COMPLETED
	StateFailedClean    = pb.OpState_OP_STATE_FAILED_CLEAN
	StateFailedDirty    = pb.OpState_OP_STATE_FAILED_DIRTY
	StateOutcomeUnknown = pb.OpState_OP_STATE_OUTCOME_UNKNOWN
	StateRetired        = pb.OpState_OP_STATE_RETIRED
)

// Terminal reports whether s is a terminal state (one that writes a
// tombstone). OutcomeUnknown is not terminal: it holds the VM.
func Terminal(s OpState) bool {
	switch s {
	case StateCompleted, StateFailedClean, StateFailedDirty, StateRetired:
		return true
	}
	return false
}

// holdsVM reports whether an operation in state s keeps its VM busy: every
// non-terminal state, and a dirty failure (held for inspection).
func holdsVM(s OpState) bool { return !Terminal(s) || s == StateFailedDirty }

// Epoch is an epoch reference.
type Epoch struct {
	// Counter counts from 1.
	Counter uint64
	// Holder identifies the issuer.
	Holder []byte
}

// Request is an operation delivery as the store sees it: the identity fields
// of a verified grant, and nothing of the request itself. The request bytes
// are represented by IntentDigest only.
type Request struct {
	// ProviderUID is the Provider metadata.uid the grant names.
	ProviderUID string
	// EnrolmentID is the enrolment the grant names (16 bytes).
	EnrolmentID []byte
	// OpID is the operation identifier (16 bytes).
	OpID []byte
	// VMUID is the VirtualMachine metadata.uid.
	VMUID string
	// BindingID is the VM placement identifier (16 bytes).
	BindingID []byte
	// Kind is the operation kind.
	Kind providerv1.OperationKind
	// Attempt is the attempt number (from 1).
	Attempt uint64
	// IntentDigest is the salted digest of the request bytes (32 bytes).
	IntentDigest []byte
	// MintEpoch is the epoch at which OpID was minted.
	MintEpoch Epoch
	// Actor names the caller for the audit trail, for example the Provider
	// identity from the peer certificate. It is not an identity field.
	Actor string
}

// ErrInvalidRequest reports a Request with a malformed field.
var ErrInvalidRequest = errors.New("operation request malformed")

func (r Request) validate() error {
	switch {
	case r.ProviderUID == "" || len(r.ProviderUID) > maxNameLen:
		return fmt.Errorf("%w: provider uid empty or too long", ErrInvalidRequest)
	case len(r.Actor) > maxNameLen:
		return fmt.Errorf("%w: actor too long", ErrInvalidRequest)
	case len(r.MintEpoch.Holder) > maxHolderLen:
		return fmt.Errorf("%w: mint epoch holder too long", ErrInvalidRequest)
	case len(r.EnrolmentID) != idLen:
		return fmt.Errorf("%w: enrolment id must be %d bytes", ErrInvalidRequest, idLen)
	case len(r.OpID) != idLen:
		return fmt.Errorf("%w: op id must be %d bytes", ErrInvalidRequest, idLen)
	case r.VMUID == "" || len(r.VMUID) > 1024:
		return fmt.Errorf("%w: vm uid empty or too long", ErrInvalidRequest)
	case len(r.BindingID) != idLen:
		return fmt.Errorf("%w: binding id must be %d bytes", ErrInvalidRequest, idLen)
	case r.Kind == providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED:
		return fmt.Errorf("%w: no kind", ErrInvalidRequest)
	case r.Attempt == 0:
		return fmt.Errorf("%w: attempt counts from 1", ErrInvalidRequest)
	case len(r.IntentDigest) != digestLen:
		return fmt.Errorf("%w: intent digest must be %d bytes", ErrInvalidRequest, digestLen)
	case r.MintEpoch.Counter == 0:
		return fmt.Errorf("%w: mint epoch counts from 1", ErrInvalidRequest)
	}
	return nil
}

// identity converts the request to its stored identity fields.
func (r Request) identity() *pb.OpIdentity {
	return &pb.OpIdentity{
		ProviderUid:  r.ProviderUID,
		EnrolmentId:  append([]byte(nil), r.EnrolmentID...),
		OpId:         append([]byte(nil), r.OpID...),
		VmUid:        r.VMUID,
		BindingId:    append([]byte(nil), r.BindingID...),
		Kind:         int32(r.Kind),
		Attempt:      r.Attempt,
		IntentDigest: append([]byte(nil), r.IntentDigest...),
		MintEpoch:    &pb.Epoch{Counter: r.MintEpoch.Counter, Holder: append([]byte(nil), r.MintEpoch.Holder...)},
	}
}

// sameIdentity reports whether two identities name the same operation: every
// field equal.
func sameIdentity(a, b *pb.OpIdentity) bool { return proto.Equal(a, b) }

// OpRef names an operation in the store.
type OpRef struct {
	// EnrolmentID is the enrolment the operation belongs to.
	EnrolmentID []byte
	// OpID is the operation identifier.
	OpID []byte
}

// String returns the reference in hex, for logs and errors.
func (r OpRef) String() string {
	return hex.EncodeToString(r.EnrolmentID) + "/" + hex.EncodeToString(r.OpID)
}

// OpView is what the store can say about an operation.
type OpView struct {
	// Ref names the operation.
	Ref OpRef
	// VMUID, BindingID, Kind and Attempt are identity fields.
	VMUID     string
	BindingID []byte
	Kind      providerv1.OperationKind
	Attempt   uint64
	// State is the operation's state.
	State OpState
	// Reason qualifies State, for example "witness_no_effect".
	Reason string
	// Irrev is set once a once or irreversible step intent was durable.
	Irrev bool
	// Tombstoned is set when the answer comes from the tombstone (the record
	// payload was compacted).
	Tombstoned bool
	// IntentDigest is the salted digest of the operation's request bytes, an
	// identity field.
	IntentDigest []byte
}

func viewOf(id *pb.OpIdentity, state OpState, reason string, irrev, tomb bool) OpView {
	return OpView{
		Ref:          OpRef{EnrolmentID: id.GetEnrolmentId(), OpID: id.GetOpId()},
		VMUID:        id.GetVmUid(),
		BindingID:    id.GetBindingId(),
		Kind:         providerv1.OperationKind(id.GetKind()),
		Attempt:      id.GetAttempt(),
		State:        state,
		Reason:       reason,
		Irrev:        irrev,
		Tombstoned:   tomb,
		IntentDigest: id.GetIntentDigest(),
	}
}

func viewOfRecord(r *pb.OpRecord) OpView {
	return viewOf(r.GetIdentity(), r.GetState(), r.GetReason(), r.GetIrrev(), false)
}

func viewOfTombstone(t *pb.Tombstone) OpView {
	return viewOf(t.GetIdentity(), t.GetState(), t.GetReason(), t.GetIrrev(), true)
}
