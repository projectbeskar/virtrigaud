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

package controller

// The host ledger object (development builds only): what the manager knows
// durably about one host agent operation store, kept in a Secret of its own
// per store in the enrolment namespace.
//
// The ledger of a store records the highest position of the store's chain
// the manager has acknowledged (the ledger position, with the checkpoint
// used at attestation), the agent's attestation sessions, and the store's
// generation. It is keyed by the store, not by the Host object: replacing
// or deleting a Host, or resetting its pins Secret, does not touch it, and
// no older manager reads or writes it. Each object carries an HMAC tag under
// the manager's host ledger tag key (host_ledger_key.go) over every field
// except updatedAt and tagKeyID, so a principal that can write Secrets in
// the enrolment namespace but cannot read the tag key cannot forge or edit
// one. The pins Secret of each Host carries a tagged index of the stores
// that have a ledger object and the highest write counter seen, so a deleted
// or partially restored ledger object is detected.
//
// This file holds the object's form, its validation and its tag; the writes
// are in host_ledger_writes.go.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// Names, labels and keys of a host ledger Secret.
const (
	// hostLedgerSecretPrefix prefixes a ledger Secret's name; the store id
	// (32 lowercase hexadecimal characters) follows.
	hostLedgerSecretPrefix = "virtrigaud-ledger-" // #nosec G101 -- a Secret name prefix, not a credential
	// hostLedgerDataKey is the ledger Secret's data key holding the ledger.
	hostLedgerDataKey = "ledger.json"
	// hostLedgerComponent is the app.kubernetes.io/component label of a
	// ledger Secret.
	hostLedgerComponent = "host-ledger"
	// hostLedgerFormat is the value of the LabelHostLedger label: the
	// ledger format.
	hostLedgerFormat = "v1"
	// LabelHostLedgerEnrolment labels a ledger Secret with the current
	// enrolment id (hexadecimal) of the Host it serves; a lookup hint only.
	LabelHostLedgerEnrolment = "virtrigaud.io/enrolment"
	// LabelHostLedgerIdentityMAC labels a ledger Secret with the first 32
	// hexadecimal characters of the identity MAC of the Host it serves (a
	// label value holds at most 63 characters); a lookup hint only.
	LabelHostLedgerIdentityMAC = "virtrigaud.io/host-identity-mac"
)

// hostLedgerIdentityLabelLen is how many hexadecimal characters of the
// identity MAC the identity label carries.
const hostLedgerIdentityLabelLen = 32

// hostLedgerMinManagerVersion is the downgrade floor written into every
// ledger object: the lowest manager version that implements this ledger
// format. A manager below it (or one that cannot read it) refuses the Host
// the object serves (host_state_compat.go). It is the lowest pre-release of
// the current minor line while the format is only in development builds,
// and is raised to the first release that ships the format.
const hostLedgerMinManagerVersion = "0.4.2-0"

// Bounds of a ledger object.
const (
	// maxLedgerPendingSessions bounds the attestation sessions begun and
	// not yet adopted (the agent's bound on outstanding challenges).
	maxLedgerPendingSessions = 4
	// maxLedgerSessionHistory bounds the earlier sessions kept with the
	// ledger position at which each was fenced.
	maxLedgerSessionHistory = 8
	// maxLedgerGenerationEnrolments bounds the enrolments recorded for the
	// current generation (oldest dropped first).
	maxLedgerGenerationEnrolments = 16
	// maxLedgerIndexStores bounds the stores a pins Secret's ledger index
	// names (oldest dropped first).
	maxLedgerIndexStores = 8
	// maxLedgerSecretBytes bounds a ledger document; larger is malformed.
	maxLedgerSecretBytes = 16 << 10
)

// hostLedgerTimeLayout is the layout of every time in a ledger object
// (seconds, UTC).
const hostLedgerTimeLayout = time.RFC3339

// HostLedgerPosition is a position in a host's operation store chain.
type HostLedgerPosition struct {
	// Seq is the sequence number.
	Seq uint64
	// Head is the chain head at Seq.
	Head [32]byte
}

// HostLedgerPendingSession is an attestation session the manager began
// (its challenge nonce) and has not adopted.
type HostLedgerPendingSession struct {
	// ID is the session id (the consumed challenge nonce).
	ID [16]byte
	// BegunAt is when the session was begun.
	BegunAt time.Time

	unknown unknownMembers
}

// HostLedgerFencedSession is an earlier attestation session and the ledger
// sequence number at the moment a later session replaced it: a receipt of
// the session above FencedAt was never covered by the ledger.
type HostLedgerFencedSession struct {
	// ID is the session id.
	ID [16]byte
	// FencedAt is the ledger sequence number when the session was replaced.
	FencedAt uint64

	unknown unknownMembers
}

// HostLedgerRecord is one verified host ledger object. Records are returned
// by HostLedger only after their tag verified, or as the object of the
// manager's own successful write; a caller signs only from such a record
// and never edits one (a change is a HostLedger write).
type HostLedgerRecord struct {
	// StoreID is the store the ledger is for (the object's key).
	StoreID [16]byte
	// HostIdentityMAC binds the store to the machine: the identity MAC of
	// the Host it serves (HostLedgerKeys.IdentityMAC).
	HostIdentityMAC [32]byte
	// HostUID, HostKey (namespace/name) and ProviderUID name the Host the
	// ledger currently serves.
	HostUID, HostKey, ProviderUID string
	// Position is the ledger position: the highest chain position the
	// manager has acknowledged.
	Position HostLedgerPosition
	// Checkpoint is the checkpoint used at attestation; Seq 0 when none.
	Checkpoint HostLedgerPosition
	// AttestationID is the current attestation session; zero when none.
	AttestationID [16]byte
	// AttestationState is whether the current session was adopted
	// (Attested) or a new one was begun for an agent awaiting attestation
	// (Pending); empty before the first session.
	AttestationState infravirtrigaudiov1beta1.HostAgentLedgerAttestationState
	// PendingSessions are the sessions begun and not adopted, oldest first.
	PendingSessions []HostLedgerPendingSession
	// SessionHistory are earlier sessions, oldest first.
	SessionHistory []HostLedgerFencedSession
	// Generation is the ledger generation: a number never reused for the
	// store, raised whenever the ledger is not continuous with earlier
	// history (a fresh ledger, or one started after an archive).
	Generation uint64
	// GenerationStartedAt is when the generation started (display only).
	GenerationStartedAt time.Time
	// GenerationEnrolments are the enrolments (16-byte ids) under which the
	// ledger was continuous in this generation, oldest first.
	GenerationEnrolments [][]byte
	// WriteCounter is raised by one on every write; the pins Secret's index
	// records the highest seen, so a restored older object is detected.
	WriteCounter uint64
	// MinManagerVersion is the object's downgrade floor.
	MinManagerVersion string
	// UpdatedAt is when the object was last written (not tagged).
	UpdatedAt time.Time
	// Archived reports that the store was replaced by SuccessorStoreID at
	// ArchivedAt; an archived ledger is never written again.
	Archived         bool
	SuccessorStoreID [16]byte
	ArchivedAt       time.Time
	// TagKeyID is the id of the tag key that made Tag.
	TagKeyID string
	// Tag is the HMAC tag over the record.
	Tag [32]byte

	// resourceVersion is the Secret's resourceVersion the record was read
	// from or written as.
	resourceVersion string
	// unknown are the members a newer manager wrote that this one does not
	// know; they are kept, and covered by the tag.
	unknown unknownMembers
}

// StorePrefix returns the first 8 hexadecimal characters of the store id,
// the only part of it shown in status, events and logs.
func (r *HostLedgerRecord) StorePrefix() string { return storeIDPrefix(r.StoreID) }

// HasEnrolment reports whether enrolment is one of the generation's
// enrolments.
func (r *HostLedgerRecord) HasEnrolment(enrolment []byte) bool {
	return slices.ContainsFunc(r.GenerationEnrolments, func(e []byte) bool { return bytes.Equal(e, enrolment) })
}

// HasPendingSession reports whether id is a pending session.
func (r *HostLedgerRecord) HasPendingSession(id [16]byte) bool {
	return slices.ContainsFunc(r.PendingSessions, func(s HostLedgerPendingSession) bool { return s.ID == id })
}

// clone returns a deep copy.
func (r *HostLedgerRecord) clone() *HostLedgerRecord {
	c := *r
	c.PendingSessions = slices.Clone(r.PendingSessions)
	c.SessionHistory = slices.Clone(r.SessionHistory)
	c.GenerationEnrolments = make([][]byte, len(r.GenerationEnrolments))
	for i, e := range r.GenerationEnrolments {
		c.GenerationEnrolments[i] = bytes.Clone(e)
	}
	return &c
}

// storeIDPrefix returns the first 8 hexadecimal characters of a store id.
func storeIDPrefix(id [16]byte) string { return hex.EncodeToString(id[:4]) }

// hostLedgerSecretName returns the name of a store's ledger Secret.
func hostLedgerSecretName(storeID [16]byte) string {
	return hostLedgerSecretPrefix + hex.EncodeToString(storeID[:])
}

// The JSON form of a ledger object (data key ledger.json).

type ledgerPendingJSON struct {
	ID      string `json:"id"`
	BegunAt string `json:"w1At"`
	// Unknown are members a newer manager wrote (host_state_compat.go).
	Unknown unknownMembers `json:"-"`
}

type ledgerFencedJSON struct {
	ID       string `json:"id"`
	FencedAt uint64 `json:"fencedAt"`
	// Unknown are members a newer manager wrote (host_state_compat.go).
	Unknown unknownMembers `json:"-"`
}

type hostLedgerJSON struct {
	StoreID              string              `json:"storeID"`
	HostIdentityMAC      string              `json:"hostIdentityMAC"`
	HostUID              string              `json:"hostUID"`
	HostKey              string              `json:"hostKey"`
	ProviderUID          string              `json:"providerUID"`
	Seq                  uint64              `json:"seq"`
	Head                 string              `json:"head"`
	CheckpointSeq        uint64              `json:"checkpointSeq"`
	CheckpointHead       string              `json:"checkpointHead,omitempty"`
	AttestationID        string              `json:"attestationID,omitempty"`
	AttestationState     string              `json:"attestationState,omitempty"`
	PendingSessions      []ledgerPendingJSON `json:"pendingSessions,omitempty"`
	SessionHistory       []ledgerFencedJSON  `json:"sessionHistory,omitempty"`
	LedgerGeneration     uint64              `json:"ledgerGeneration"`
	GenerationStartedAt  string              `json:"generationStartedAt"`
	GenerationEnrolments []string            `json:"generationEnrolments"`
	WriteCounter         uint64              `json:"writeCounter"`
	MinManagerVersion    string              `json:"minManagerVersion,omitempty"`
	UpdatedAt            string              `json:"updatedAt,omitempty"`
	Archived             bool                `json:"archived,omitempty"`
	SuccessorStoreID     string              `json:"successorStoreID,omitempty"`
	ArchivedAt           string              `json:"archivedAt,omitempty"`
	TagKeyID             string              `json:"tagKeyID"`
	Tag                  string              `json:"tag"`
	// Unknown are members a newer manager wrote (host_state_compat.go).
	Unknown unknownMembers `json:"-"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (p *ledgerPendingJSON) UnmarshalJSON(b []byte) (err error) {
	type plain ledgerPendingJSON
	p.Unknown, err = decodeKeepingUnknown(b, (*plain)(p))
	return err
}

// MarshalJSON implements json.Marshaler.
func (p ledgerPendingJSON) MarshalJSON() ([]byte, error) {
	type plain ledgerPendingJSON
	return encodeWithUnknown(plain(p), p.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (f *ledgerFencedJSON) UnmarshalJSON(b []byte) (err error) {
	type plain ledgerFencedJSON
	f.Unknown, err = decodeKeepingUnknown(b, (*plain)(f))
	return err
}

// MarshalJSON implements json.Marshaler.
func (f ledgerFencedJSON) MarshalJSON() ([]byte, error) {
	type plain ledgerFencedJSON
	return encodeWithUnknown(plain(f), f.Unknown)
}

// UnmarshalJSON implements json.Unmarshaler.
func (l *hostLedgerJSON) UnmarshalJSON(b []byte) (err error) {
	type plain hostLedgerJSON
	l.Unknown, err = decodeKeepingUnknown(b, (*plain)(l))
	return err
}

// MarshalJSON implements json.Marshaler.
func (l hostLedgerJSON) MarshalJSON() ([]byte, error) {
	type plain hostLedgerJSON
	return encodeWithUnknown(plain(l), l.Unknown)
}

// errLedgerMalformed wraps every refusal of a ledger document's form.
var errLedgerMalformed = errors.New("malformed host ledger")

var (
	hex16RE = regexp.MustCompile(`^[0-9a-f]{16}$`)
	hex32RE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// decodeFixedHex decodes lowercase hexadecimal of exactly len(out) bytes.
func decodeFixedHex(field, s string, out []byte) error {
	re := hex32RE
	if len(out) == sha256.Size {
		re = hex64RE
	}
	if !re.MatchString(s) {
		return fmt.Errorf("%w: %s must be %d lowercase hexadecimal characters", errLedgerMalformed, field, 2*len(out))
	}
	b, _ := hex.DecodeString(s)
	copy(out, b)
	return nil
}

// parseLedgerTime parses a ledger time; it must be in the ledger layout
// exactly (so its tag encoding is the stored text).
func parseLedgerTime(field, s string) (time.Time, error) {
	t, err := time.Parse(hostLedgerTimeLayout, s)
	if err != nil || t.UTC().Format(hostLedgerTimeLayout) != s {
		return time.Time{}, fmt.Errorf("%w: %s is not an RFC 3339 UTC time in seconds", errLedgerMalformed, field)
	}
	return t.UTC(), nil
}

// formatLedgerTime formats a ledger time; the zero time is "".
func formatLedgerTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Truncate(time.Second).Format(hostLedgerTimeLayout)
}

// decodeHostLedger decodes and validates a ledger document. It does not
// check the tag (verifyHostLedgerTag).
func decodeHostLedger(data []byte) (*HostLedgerRecord, error) {
	if len(data) == 0 || len(data) > maxLedgerSecretBytes {
		return nil, fmt.Errorf("%w: the document is empty or larger than %d bytes", errLedgerMalformed, maxLedgerSecretBytes)
	}
	var j hostLedgerJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("%w: %w", errLedgerMalformed, err)
	}
	r := &HostLedgerRecord{HostUID: j.HostUID, HostKey: j.HostKey, ProviderUID: j.ProviderUID, WriteCounter: j.WriteCounter,
		MinManagerVersion: j.MinManagerVersion, Archived: j.Archived, TagKeyID: j.TagKeyID, Generation: j.LedgerGeneration,
		unknown: j.Unknown}
	if err := decodeFixedHex("storeID", j.StoreID, r.StoreID[:]); err != nil {
		return nil, err
	}
	if err := decodeFixedHex("hostIdentityMAC", j.HostIdentityMAC, r.HostIdentityMAC[:]); err != nil {
		return nil, err
	}
	if j.HostUID == "" || j.HostKey == "" || j.ProviderUID == "" {
		return nil, fmt.Errorf("%w: hostUID, hostKey and providerUID are required", errLedgerMalformed)
	}
	if j.Seq < 1 {
		return nil, fmt.Errorf("%w: seq must be at least 1", errLedgerMalformed)
	}
	r.Position.Seq = j.Seq
	if err := decodeFixedHex("head", j.Head, r.Position.Head[:]); err != nil {
		return nil, err
	}
	switch {
	case j.CheckpointSeq%hostidentity.CheckpointInterval != 0 || j.CheckpointSeq > j.Seq:
		return nil, fmt.Errorf("%w: checkpointSeq must be a multiple of %d not above seq", errLedgerMalformed, hostidentity.CheckpointInterval)
	case j.CheckpointSeq == 0 && j.CheckpointHead != "":
		return nil, fmt.Errorf("%w: checkpointHead without checkpointSeq", errLedgerMalformed)
	case j.CheckpointSeq > 0:
		r.Checkpoint.Seq = j.CheckpointSeq
		if err := decodeFixedHex("checkpointHead", j.CheckpointHead, r.Checkpoint.Head[:]); err != nil {
			return nil, err
		}
	}
	if j.AttestationID != "" {
		if err := decodeFixedHex("attestationID", j.AttestationID, r.AttestationID[:]); err != nil {
			return nil, err
		}
	}
	switch s := infravirtrigaudiov1beta1.HostAgentLedgerAttestationState(j.AttestationState); s {
	case "", infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending:
		r.AttestationState = s
	case infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested:
		if j.AttestationID == "" {
			return nil, fmt.Errorf("%w: an Attested ledger needs an attestationID", errLedgerMalformed)
		}
		r.AttestationState = s
	default:
		return nil, fmt.Errorf("%w: attestationState %q is not Pending or Attested", errLedgerMalformed, j.AttestationState)
	}
	if len(j.PendingSessions) > maxLedgerPendingSessions || len(j.SessionHistory) > maxLedgerSessionHistory {
		return nil, fmt.Errorf("%w: at most %d pending sessions and %d earlier sessions", errLedgerMalformed,
			maxLedgerPendingSessions, maxLedgerSessionHistory)
	}
	for i, p := range j.PendingSessions {
		s := HostLedgerPendingSession{unknown: p.Unknown}
		if err := decodeFixedHex(fmt.Sprintf("pendingSessions[%d].id", i), p.ID, s.ID[:]); err != nil {
			return nil, err
		}
		t, err := parseLedgerTime(fmt.Sprintf("pendingSessions[%d].w1At", i), p.BegunAt)
		if err != nil {
			return nil, err
		}
		s.BegunAt = t
		if r.HasPendingSession(s.ID) {
			return nil, fmt.Errorf("%w: a pending session is listed twice", errLedgerMalformed)
		}
		r.PendingSessions = append(r.PendingSessions, s)
	}
	for i, h := range j.SessionHistory {
		s := HostLedgerFencedSession{FencedAt: h.FencedAt, unknown: h.Unknown}
		if err := decodeFixedHex(fmt.Sprintf("sessionHistory[%d].id", i), h.ID, s.ID[:]); err != nil {
			return nil, err
		}
		r.SessionHistory = append(r.SessionHistory, s)
	}
	if j.LedgerGeneration < 1 {
		return nil, fmt.Errorf("%w: ledgerGeneration must be at least 1", errLedgerMalformed)
	}
	t, err := parseLedgerTime("generationStartedAt", j.GenerationStartedAt)
	if err != nil {
		return nil, err
	}
	r.GenerationStartedAt = t
	if len(j.GenerationEnrolments) < 1 || len(j.GenerationEnrolments) > maxLedgerGenerationEnrolments {
		return nil, fmt.Errorf("%w: generationEnrolments must list 1 to %d enrolments", errLedgerMalformed, maxLedgerGenerationEnrolments)
	}
	for i, e := range j.GenerationEnrolments {
		id := make([]byte, hostidentity.IDLen)
		if err := decodeFixedHex(fmt.Sprintf("generationEnrolments[%d]", i), e, id); err != nil {
			return nil, err
		}
		r.GenerationEnrolments = append(r.GenerationEnrolments, id)
	}
	if j.UpdatedAt != "" {
		if r.UpdatedAt, err = parseLedgerTime("updatedAt", j.UpdatedAt); err != nil {
			return nil, err
		}
	}
	switch {
	case j.Archived:
		if err := decodeFixedHex("successorStoreID", j.SuccessorStoreID, r.SuccessorStoreID[:]); err != nil {
			return nil, err
		}
		if r.ArchivedAt, err = parseLedgerTime("archivedAt", j.ArchivedAt); err != nil {
			return nil, err
		}
	case j.SuccessorStoreID != "" || j.ArchivedAt != "":
		return nil, fmt.Errorf("%w: successorStoreID and archivedAt are set only on an archived ledger", errLedgerMalformed)
	}
	if !hex16RE.MatchString(j.TagKeyID) {
		return nil, fmt.Errorf("%w: tagKeyID must be 16 lowercase hexadecimal characters", errLedgerMalformed)
	}
	if err := decodeFixedHex("tag", j.Tag, r.Tag[:]); err != nil {
		return nil, err
	}
	return r, nil
}

// toJSON returns the JSON form of r.
func (r *HostLedgerRecord) toJSON() hostLedgerJSON {
	j := hostLedgerJSON{
		StoreID: hex.EncodeToString(r.StoreID[:]), HostIdentityMAC: hex.EncodeToString(r.HostIdentityMAC[:]),
		HostUID: r.HostUID, HostKey: r.HostKey, ProviderUID: r.ProviderUID,
		Seq: r.Position.Seq, Head: hex.EncodeToString(r.Position.Head[:]), CheckpointSeq: r.Checkpoint.Seq,
		AttestationState: string(r.AttestationState), LedgerGeneration: r.Generation,
		GenerationStartedAt: formatLedgerTime(r.GenerationStartedAt), WriteCounter: r.WriteCounter,
		MinManagerVersion: r.MinManagerVersion, UpdatedAt: formatLedgerTime(r.UpdatedAt), Archived: r.Archived,
		TagKeyID: r.TagKeyID, Tag: hex.EncodeToString(r.Tag[:]), Unknown: r.unknown,
	}
	if r.Checkpoint.Seq > 0 {
		j.CheckpointHead = hex.EncodeToString(r.Checkpoint.Head[:])
	}
	if r.AttestationID != [16]byte{} {
		j.AttestationID = hex.EncodeToString(r.AttestationID[:])
	}
	for _, p := range r.PendingSessions {
		j.PendingSessions = append(j.PendingSessions, ledgerPendingJSON{ID: hex.EncodeToString(p.ID[:]),
			BegunAt: formatLedgerTime(p.BegunAt), Unknown: p.unknown})
	}
	for _, h := range r.SessionHistory {
		j.SessionHistory = append(j.SessionHistory, ledgerFencedJSON{ID: hex.EncodeToString(h.ID[:]), FencedAt: h.FencedAt,
			Unknown: h.unknown})
	}
	for _, e := range r.GenerationEnrolments {
		j.GenerationEnrolments = append(j.GenerationEnrolments, hex.EncodeToString(e))
	}
	if r.Archived {
		j.SuccessorStoreID = hex.EncodeToString(r.SuccessorStoreID[:])
		j.ArchivedAt = formatLedgerTime(r.ArchivedAt)
	}
	return j
}

// encode returns the ledger document of r.
func (r *HostLedgerRecord) encode() ([]byte, error) {
	b, err := json.Marshal(r.toJSON())
	if err != nil {
		return nil, fmt.Errorf("encode the host ledger: %w", err)
	}
	return b, nil
}

// tagEncoder builds a tag input: every field is its name and its value, each
// preceded by its length as 4 bytes big endian, so no two different records
// encode alike.
type tagEncoder struct{ buf bytes.Buffer }

// raw appends one length-prefixed byte string.
func (e *tagEncoder) raw(b []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b))) // #nosec G115 -- a ledger document is at most 16 KiB
	e.buf.Write(n[:])
	e.buf.Write(b)
}

// field appends a named byte string.
func (e *tagEncoder) field(name string, value []byte) {
	e.raw([]byte(name))
	e.raw(value)
}

// str appends a named string.
func (e *tagEncoder) str(name, value string) { e.field(name, []byte(value)) }

// u64 appends a named unsigned number as 8 bytes big endian.
func (e *tagEncoder) u64(name string, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	e.field(name, b[:])
}

// list appends a named list: the element count, then each element's own
// encoding as one length-prefixed byte string.
func (e *tagEncoder) list(name string, n int, elem func(i int, sub *tagEncoder)) {
	var sub tagEncoder
	sub.u64("count", uint64(n)) // #nosec G115 -- a length is never negative
	for i := range n {
		var one tagEncoder
		elem(i, &one)
		sub.raw(one.buf.Bytes())
	}
	e.field(name, sub.buf.Bytes())
}

// unknown appends members a newer manager wrote, sorted by name, each value
// in compact JSON, so a record this manager rewrites keeps them covered.
func (e *tagEncoder) unknown(u unknownMembers) {
	names := make([]string, 0, len(u))
	for k := range u {
		names = append(names, k)
	}
	sort.Strings(names)
	e.list("unknown", len(names), func(i int, sub *tagEncoder) {
		var c bytes.Buffer
		if err := json.Compact(&c, u[names[i]]); err != nil {
			c.Reset()
			c.Write(u[names[i]])
		}
		sub.field(names[i], c.Bytes())
	})
}

// tagInput returns the bytes the ledger tag covers: every field except
// updatedAt, tagKeyID and the tag itself, plus the unknown members.
func (r *HostLedgerRecord) tagInput() []byte {
	j := r.toJSON()
	var e tagEncoder
	e.str("storeID", j.StoreID)
	e.str("hostIdentityMAC", j.HostIdentityMAC)
	e.str("hostUID", j.HostUID)
	e.str("hostKey", j.HostKey)
	e.str("providerUID", j.ProviderUID)
	e.u64("seq", j.Seq)
	e.str("head", j.Head)
	e.u64("checkpointSeq", j.CheckpointSeq)
	e.str("checkpointHead", j.CheckpointHead)
	e.str("attestationID", j.AttestationID)
	e.str("attestationState", j.AttestationState)
	e.list("pendingSessions", len(j.PendingSessions), func(i int, sub *tagEncoder) {
		sub.str("id", j.PendingSessions[i].ID)
		sub.str("w1At", j.PendingSessions[i].BegunAt)
		sub.unknown(j.PendingSessions[i].Unknown)
	})
	e.list("sessionHistory", len(j.SessionHistory), func(i int, sub *tagEncoder) {
		sub.str("id", j.SessionHistory[i].ID)
		sub.u64("fencedAt", j.SessionHistory[i].FencedAt)
		sub.unknown(j.SessionHistory[i].Unknown)
	})
	e.u64("ledgerGeneration", j.LedgerGeneration)
	e.str("generationStartedAt", j.GenerationStartedAt)
	e.list("generationEnrolments", len(j.GenerationEnrolments), func(i int, sub *tagEncoder) {
		sub.str("id", j.GenerationEnrolments[i])
	})
	e.u64("writeCounter", j.WriteCounter)
	e.str("minManagerVersion", j.MinManagerVersion)
	archived := []byte{0}
	if j.Archived {
		archived[0] = 1
	}
	e.field("archived", archived)
	e.str("successorStoreID", j.SuccessorStoreID)
	e.str("archivedAt", j.ArchivedAt)
	e.unknown(j.Unknown)
	return e.buf.Bytes()
}

// sign tags r under the current tag key.
func (r *HostLedgerRecord) sign(keys *HostLedgerKeys) {
	r.TagKeyID, r.Tag = keys.tag(hostLedgerTagContext, r.tagInput())
}

// verifyTag reports whether r's tag verifies under the key its tag key id
// names.
func (r *HostLedgerRecord) verifyTag(keys *HostLedgerKeys) bool {
	return keys.verifyTag(r.TagKeyID, hostLedgerTagContext, r.tagInput(), r.Tag[:])
}

// HostLedgerIndex is the ledger index kept in a Host's pins Secret (member
// ledgerIndex of store.json): the stores that have a ledger object and the
// highest ledger write counter seen. Neither is ever lowered. It is tagged
// under the ledger tag key together with the Host's UID, so an index copied
// from another Host's pins Secret does not verify.
type HostLedgerIndex struct {
	// StoreIDs are the stores that have a ledger object, oldest first.
	StoreIDs [][16]byte
	// CounterSeen is the highest ledger write counter seen.
	CounterSeen uint64
	// TagKeyID is the id of the tag key that made Tag.
	TagKeyID string
	// Tag is the HMAC tag over the index.
	Tag [32]byte

	unknown unknownMembers
}

// Names reports whether the index names store.
func (x *HostLedgerIndex) Names(store [16]byte) bool {
	return x != nil && slices.Contains(x.StoreIDs, store)
}

// clone returns a deep copy (nil for nil).
func (x *HostLedgerIndex) clone() *HostLedgerIndex {
	if x == nil {
		return nil
	}
	c := *x
	c.StoreIDs = slices.Clone(x.StoreIDs)
	return &c
}

// tagInput returns the bytes the index tag covers, bound to the Host UID.
func (x *HostLedgerIndex) tagInput(hostUID string) []byte {
	var e tagEncoder
	e.str("hostUID", hostUID)
	e.list("storeIDs", len(x.StoreIDs), func(i int, sub *tagEncoder) { sub.str("id", hex.EncodeToString(x.StoreIDs[i][:])) })
	e.u64("counterSeen", x.CounterSeen)
	e.unknown(x.unknown)
	return e.buf.Bytes()
}

// sign tags x under the current tag key for the Host hostUID.
func (x *HostLedgerIndex) sign(keys *HostLedgerKeys, hostUID string) {
	x.TagKeyID, x.Tag = keys.tag(hostLedgerIndexTagContext, x.tagInput(hostUID))
}

// verifyTag reports whether x's tag verifies for the Host hostUID.
func (x *HostLedgerIndex) verifyTag(keys *HostLedgerKeys, hostUID string) bool {
	return keys.verifyTag(x.TagKeyID, hostLedgerIndexTagContext, x.tagInput(hostUID), x.Tag[:])
}

// ledgerIndexJSON is the JSON form of a HostLedgerIndex.
type ledgerIndexJSON struct {
	StoreIDs    []string `json:"storeIDs,omitempty"`
	CounterSeen uint64   `json:"counterSeen"`
	TagKeyID    string   `json:"tagKeyID"`
	Tag         string   `json:"tag"`
	// Unknown are members a newer manager wrote (host_state_compat.go).
	Unknown unknownMembers `json:"-"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (x *ledgerIndexJSON) UnmarshalJSON(b []byte) (err error) {
	type plain ledgerIndexJSON
	x.Unknown, err = decodeKeepingUnknown(b, (*plain)(x))
	return err
}

// MarshalJSON implements json.Marshaler.
func (x ledgerIndexJSON) MarshalJSON() ([]byte, error) {
	type plain ledgerIndexJSON
	return encodeWithUnknown(plain(x), x.Unknown)
}

// toIndexJSON returns the JSON form of an index (nil for nil).
func toIndexJSON(x *HostLedgerIndex) *ledgerIndexJSON {
	if x == nil {
		return nil
	}
	j := &ledgerIndexJSON{CounterSeen: x.CounterSeen, TagKeyID: x.TagKeyID, Tag: hex.EncodeToString(x.Tag[:]), Unknown: x.unknown}
	for _, s := range x.StoreIDs {
		j.StoreIDs = append(j.StoreIDs, hex.EncodeToString(s[:]))
	}
	return j
}

// fromIndexJSON decodes the JSON form of an index. A malformed index is an
// error; the caller treats it as a tampered one.
func fromIndexJSON(j *ledgerIndexJSON) (*HostLedgerIndex, error) {
	if j == nil {
		return nil, nil
	}
	if len(j.StoreIDs) > maxLedgerIndexStores {
		return nil, fmt.Errorf("%w: the ledger index names more than %d stores", errLedgerMalformed, maxLedgerIndexStores)
	}
	x := &HostLedgerIndex{CounterSeen: j.CounterSeen, TagKeyID: j.TagKeyID, unknown: j.Unknown}
	for i, s := range j.StoreIDs {
		var id [16]byte
		if err := decodeFixedHex(fmt.Sprintf("ledgerIndex.storeIDs[%d]", i), s, id[:]); err != nil {
			return nil, err
		}
		x.StoreIDs = append(x.StoreIDs, id)
	}
	if !hex16RE.MatchString(j.TagKeyID) {
		return nil, fmt.Errorf("%w: ledgerIndex.tagKeyID must be 16 lowercase hexadecimal characters", errLedgerMalformed)
	}
	if err := decodeFixedHex("ledgerIndex.tag", j.Tag, x.Tag[:]); err != nil {
		return nil, err
	}
	return x, nil
}

// isHostLedgerSecret reports whether sec is a ledger Secret by its labels
// (a lookup hint; the body decides).
func isHostLedgerSecret(sec *corev1.Secret) bool {
	l := sec.GetLabels()
	return l[k8sLabelComponent] == hostLedgerComponent && l[LabelHostLedger] != ""
}
