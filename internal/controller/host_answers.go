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

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Signed host answers on the manager side (development builds only). For a
// Provider whose type requires operation identity, the manager acts on what
// a host did only through an answer the host signed for a request the
// manager made: the transport draws a nonce per request and extracts the
// relayed answer (internal/transport/grpc/client_answer.go), the pure
// verifier checks it against the manager's own state
// (hostidentity.VerifyAnswer), and this file decides: it holds what the
// manager trusts about each host (HostTrust, from the Host's pins Secret),
// the last verified position of each host's store (written to the same
// Secret with a resourceVersion precondition), and the last verified host
// answer, without which nothing new is signed for the host.

// HostTrust is what the manager trusts about one host of a Provider whose
// type requires operation identity: all of it comes from the manager's own
// state (the Host's pins Secret and the enrolment configuration), never from
// an answer or the Provider.
type HostTrust struct {
	// Enrolment is the host's current enrolment, which grants name.
	Enrolment operation.Enrolment
	// InstallID is the manager installation's 16-byte identifier.
	InstallID []byte
	// Pins are the host's pinned agent keys.
	Pins []string
	// Anchors are the host-agent subordinate CA certificates.
	Anchors *x509.CertPool
	// HostURI is the host's URI identity.
	HostURI string
	// HostName is the Host's name.
	HostName string
	// HostUID is the UID of the Host object the trust was read for (empty
	// when the source does not know it): answers are recorded per Host
	// object, so a replacement of the same name is told apart.
	HostUID string
	// Predecessors are earlier enrolments of the host that the host still
	// holds frozen: an attempt under one may be asked about, never re-sent.
	// Empty until a re-enrolment records one.
	Predecessors [][]byte
	// Discarded are earlier enrolments the host has discarded after their
	// freeze was confirmed: an attempt under one is held, never re-sent and
	// never taken as tampered.
	Discarded [][]byte
	// RenewalCertificate is the renewed certificate the manager issued and
	// validated for the host's renewal in progress, before its key is pinned,
	// and RenewalKeyPin its key's pin. A host answer whose signer
	// certificate is exactly these bytes is verified with that key pinned
	// too (the evidence that the host put it in use early); nothing else
	// uses them.
	RenewalCertificate []byte
	RenewalKeyPin      string
}

// expectation is the verifier's expectation of an answer of bodyType for a
// request with nonce to the host of t, of Provider provider.
func (t HostTrust) expectation(provider *infravirtrigaudiov1beta1.Provider, hostID, bodyType string, nonce []byte,
	now time.Time) hostidentity.AnswerExpectation {
	return hostidentity.AnswerExpectation{
		BodyType: bodyType, Anchors: t.Anchors, HostURI: t.HostURI, HostName: t.HostName, HostID: hostID, Pins: t.Pins,
		ProviderUID: string(provider.UID), InstallID: t.InstallID, EnrolmentID: t.Enrolment.EnrolmentID, Nonce: nonce, Now: now,
	}
}

// HostStoreState is what the manager persists about one host's store: the
// last verified position and checkpoint of its current store, earlier
// stores' records (kept as evidence), and a regression that holds the host.
type HostStoreState struct {
	// Current is the record of the host's current store.
	Current hostidentity.StoreRecord
	// Previous are earlier stores' records, newest first.
	Previous []hostidentity.StoreRecord
	// RegressedReason names the continuity check that failed; empty when
	// the host is not held.
	RegressedReason string
	// RegressedEnrolmentID is the enrolment under which it failed: the hold
	// ends only with a new enrolment.
	RegressedEnrolmentID []byte
	// LedgerIndex is the host's tagged ledger index (host_ledger.go); nil
	// when the document has none. Only the host ledger writes it; every
	// other write of the store state keeps it as it is.
	LedgerIndex *HostLedgerIndex
	// ledgerIndexRaw is the stored ledger index when it could not be
	// decoded: kept and written back unchanged, and treated as tampered.
	ledgerIndexRaw json.RawMessage
	// MinManagerVersion is the document's downgrade floor
	// (host_state_compat.go): set by the host ledger when it writes the
	// index, so a manager that does not know the index refuses the Host
	// rather than drop it. Every write keeps it.
	MinManagerVersion string

	// unknown are the top-level members of the stored document a newer
	// manager wrote and this one does not know, and recordUnknown those of
	// each store record, by store id: encodeStoreState writes them back
	// (host_state_compat.go).
	unknown       unknownMembers
	recordUnknown map[[16]byte]unknownMembers
}

// HostTrustSource returns what the manager trusts about a host, and keeps
// its store state.
type HostTrustSource interface {
	// HostTrust returns the trust of host hostID of provider, or an error
	// wrapping operation.ErrNoEnrolment when the host has no current
	// enrolment for it.
	HostTrust(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) (HostTrust, error)
	// LoadStoreState returns the persisted store state of the host.
	LoadStoreState(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) (HostStoreState, error)
	// UpdateStoreState applies update to the persisted store state of the
	// host, as one write that fails rather than overwrite a concurrent one.
	UpdateStoreState(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, update func(*HostStoreState)) error
}

// hostStoreStateKey is the pins Secret's data key holding the store state.
const hostStoreStateKey = "store.json"

// maxPreviousStores bounds the earlier stores' records kept per host.
const maxPreviousStores = 8

// PinsHostTrust is the HostTrustSource of a manager with host-agent
// enrolment: the Host's pins Secret (current enrolment, key pins) and the
// enrolment configuration (sub-CA, trust domain, install id). Grants are
// issued under Epoch.
type PinsHostTrust struct {
	// Client writes the pins Secrets and reads Hosts.
	Client client.Client
	// Reader reads the pins Secrets uncached (the API reader), so a
	// read-modify-write of the store state starts from the stored
	// resourceVersion rather than an informer copy that lags the manager's
	// own writes. nil: Client.
	Reader client.Reader
	// Config is the enrolment configuration.
	Config *HostEnrolmentConfig
	// Epoch is the epoch grants are issued under (identity only).
	Epoch operation.Epoch

	once    sync.Once
	anchors *x509.CertPool
	err     error
}

// Compile-time assertion.
var _ HostTrustSource = (*PinsHostTrust)(nil)

// secretReader is Reader, or Client when none is set.
func (p *PinsHostTrust) secretReader() client.Reader {
	if p.Reader != nil {
		return p.Reader
	}
	return p.Client
}

// pool parses the sub-CA bundle once.
func (p *PinsHostTrust) pool() (*x509.CertPool, error) {
	p.once.Do(func() { p.anchors, p.err = hostidentity.RootPool(p.Config.CABundle) })
	return p.anchors, p.err
}

// host reads the Host of a Provider's host id (same namespace).
func (p *PinsHostTrust) host(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) (*infravirtrigaudiov1beta1.Host, error) {
	h := &infravirtrigaudiov1beta1.Host{}
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: hostID}, h); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("Host %s/%s: %w", provider.Namespace, hostID, operation.ErrNoEnrolment)
		}
		return nil, fmt.Errorf("get Host %s/%s: %w", provider.Namespace, hostID, err)
	}
	if h.Spec.ProviderRef.Name != provider.Name || h.Spec.Revoked {
		return nil, fmt.Errorf("Host %s/%s is revoked or not the Provider's: %w", provider.Namespace, hostID, operation.ErrNoEnrolment)
	}
	return h, nil
}

// HostTrust implements HostTrustSource.
func (p *PinsHostTrust) HostTrust(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) (HostTrust, error) {
	anchors, err := p.pool()
	if err != nil {
		return HostTrust{}, fmt.Errorf("host-agent CA bundle: %w", err)
	}
	h, err := p.host(ctx, provider, hostID)
	if err != nil {
		return HostTrust{}, err
	}
	l, err := loadHostEnrolment(ctx, p.Client, p.Config, h)
	if err != nil {
		return HostTrust{}, err
	}
	cur := l.state.Current
	if l.reset || cur == nil || len(cur.KeyPins) == 0 {
		return HostTrust{}, fmt.Errorf("Host %s/%s: %w", h.Namespace, h.Name, operation.ErrNoEnrolment)
	}
	rec, err := cur.Record.toRecord()
	if err != nil || rec.ProviderUID != string(provider.UID) || !bytes.Equal(rec.InstallID, p.Config.InstallID) {
		return HostTrust{}, fmt.Errorf("Host %s/%s: its enrolment is not this Provider's and installation's: %w", h.Namespace, h.Name,
			operation.ErrNoEnrolment)
	}
	uri, err := hostidentity.HostURI(p.Config.TrustDomain, h.Namespace, h.Name)
	if err != nil {
		return HostTrust{}, fmt.Errorf("Host %s/%s: %w", h.Namespace, h.Name, err)
	}
	// Grants under a re-keyed enrolment carry its own, later epoch counter
	// (the host starts each new enrolment at the next start epoch).
	counter := max(p.Epoch.Counter, cur.mintEpoch())
	t := HostTrust{
		Enrolment: operation.Enrolment{EnrolmentID: rec.EnrolmentID,
			Epoch: operation.Epoch{Counter: counter, Holder: bytes.Clone(p.Epoch.Holder)}},
		InstallID: bytes.Clone(p.Config.InstallID), Pins: slices.Clone(cur.KeyPins), Anchors: anchors, HostURI: uri, HostName: h.Name,
		HostUID: string(h.UID),
	}
	for _, s := range l.state.PredecessorEnrolmentIDs {
		if id, err := hostidentity.DecodeID(s); err == nil {
			t.Predecessors = append(t.Predecessors, id)
		}
	}
	if ren := l.state.Renewal; ren != nil && ren.PinnedAt == nil && len(ren.Certificate) > 0 {
		t.RenewalCertificate, t.RenewalKeyPin = bytes.Clone(ren.Certificate), ren.KeyPin
	}
	for _, s := range l.state.DiscardedEnrolmentIDs {
		if id, err := hostidentity.DecodeID(s); err == nil {
			t.Discarded = append(t.Discarded, id)
		}
	}
	return t, nil
}

// storeRecordJSON is the JSON form of a hostidentity.StoreRecord.
type storeRecordJSON struct {
	StoreID        string `json:"storeID"`
	EnrolmentID    string `json:"enrolmentID"`
	Seq            uint64 `json:"seq"`
	Head           string `json:"head"`
	CheckpointSeq  uint64 `json:"checkpointSeq,omitempty"`
	CheckpointHead string `json:"checkpointHead,omitempty"`
	UseCheckpoint  bool   `json:"useCheckpoint,omitempty"`
	// Unknown are the members a newer manager wrote that this one does
	// not know, written back unchanged (host_state_compat.go).
	Unknown unknownMembers `json:"-"`
}

// hostStoreStateJSON is the JSON form of a HostStoreState.
type hostStoreStateJSON struct {
	Current              *storeRecordJSON  `json:"current,omitempty"`
	Previous             []storeRecordJSON `json:"previous,omitempty"`
	RegressedReason      string            `json:"regressedReason,omitempty"`
	RegressedEnrolmentID string            `json:"regressedEnrolmentID,omitempty"`
	LedgerIndex          json.RawMessage   `json:"ledgerIndex,omitempty"`
	MinManagerVersion    string            `json:"minManagerVersion,omitempty"`
	// Unknown are the members a newer manager wrote that this one does
	// not know, written back unchanged (host_state_compat.go).
	Unknown unknownMembers `json:"-"`
}

// toStoreJSON converts a store record to its JSON form, with the unknown
// members kept for its store.
func toStoreJSON(r hostidentity.StoreRecord, unknown unknownMembers) storeRecordJSON {
	j := storeRecordJSON{StoreID: hex.EncodeToString(r.StoreID[:]), EnrolmentID: hex.EncodeToString(r.EnrolmentID), Seq: r.Seq,
		Head: hex.EncodeToString(r.Head[:]), CheckpointSeq: r.CheckpointSeq, UseCheckpoint: r.UseCheckpoint, Unknown: unknown}
	if r.CheckpointSeq > 0 {
		j.CheckpointHead = hex.EncodeToString(r.CheckpointHead[:])
	}
	return j
}

// fromStoreJSON converts the JSON form of a store record.
func fromStoreJSON(j storeRecordJSON) (hostidentity.StoreRecord, error) {
	var r hostidentity.StoreRecord
	dec := func(s string, out []byte) error {
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != len(out) {
			return errors.New("malformed store record")
		}
		copy(out, b)
		return nil
	}
	if err := dec(j.StoreID, r.StoreID[:]); err != nil {
		return r, err
	}
	if err := dec(j.Head, r.Head[:]); err != nil {
		return r, err
	}
	if j.CheckpointSeq > 0 {
		if err := dec(j.CheckpointHead, r.CheckpointHead[:]); err != nil {
			return r, err
		}
	}
	e, err := hostidentity.DecodeID(j.EnrolmentID)
	if err != nil {
		return r, errors.New("malformed store record enrolment")
	}
	r.EnrolmentID, r.Seq, r.CheckpointSeq, r.UseCheckpoint = e, j.Seq, j.CheckpointSeq, j.UseCheckpoint
	return r, nil
}

// decodeStoreState decodes the store state data key; absent data is the
// empty state. A malformed document is an error, never an empty state (that
// would forget the positions a regression is detected against).
func decodeStoreState(data []byte) (HostStoreState, error) {
	var st HostStoreState
	if len(data) == 0 {
		return st, nil
	}
	var j hostStoreStateJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return st, fmt.Errorf("decode the host store state: %w", err)
	}
	st.unknown = j.Unknown
	keep := func(r hostidentity.StoreRecord, unknown unknownMembers) {
		if len(unknown) == 0 {
			return
		}
		if st.recordUnknown == nil {
			st.recordUnknown = map[[16]byte]unknownMembers{}
		}
		if _, ok := st.recordUnknown[r.StoreID]; !ok {
			st.recordUnknown[r.StoreID] = unknown
		}
	}
	if j.Current != nil {
		r, err := fromStoreJSON(*j.Current)
		if err != nil {
			return st, err
		}
		st.Current = r
		keep(r, j.Current.Unknown)
	}
	for _, p := range j.Previous {
		r, err := fromStoreJSON(p)
		if err != nil {
			return st, err
		}
		st.Previous = append(st.Previous, r)
		keep(r, p.Unknown)
	}
	st.RegressedReason = j.RegressedReason
	st.MinManagerVersion = j.MinManagerVersion
	if j.RegressedEnrolmentID != "" {
		id, err := hostidentity.DecodeID(j.RegressedEnrolmentID)
		if err != nil {
			return st, errors.New("malformed regressed enrolment")
		}
		st.RegressedEnrolmentID = id
	}
	if len(j.LedgerIndex) > 0 && !bytes.Equal(bytes.TrimSpace(j.LedgerIndex), []byte("null")) {
		var xj ledgerIndexJSON
		x, err := (*HostLedgerIndex)(nil), json.Unmarshal(j.LedgerIndex, &xj)
		if err == nil {
			x, err = fromIndexJSON(&xj)
		}
		if err != nil {
			// Kept as it is and refused by the host ledger as tampered;
			// the rest of the store state is still usable.
			st.ledgerIndexRaw = bytes.Clone(j.LedgerIndex)
		} else {
			st.LedgerIndex = x
		}
	}
	return st, nil
}

// encodeStoreState encodes st for the store state data key, with the
// members a newer manager wrote that st carries from decodeStoreState: the
// top-level ones, and each store record's for the record of the same store.
func encodeStoreState(st HostStoreState) ([]byte, error) {
	j := hostStoreStateJSON{RegressedReason: st.RegressedReason, MinManagerVersion: st.MinManagerVersion, Unknown: st.unknown}
	if !st.Current.Empty() {
		c := toStoreJSON(st.Current, st.recordUnknown[st.Current.StoreID])
		j.Current = &c
	}
	for _, p := range st.Previous {
		j.Previous = append(j.Previous, toStoreJSON(p, st.recordUnknown[p.StoreID]))
	}
	if len(st.RegressedEnrolmentID) > 0 {
		j.RegressedEnrolmentID = hex.EncodeToString(st.RegressedEnrolmentID)
	}
	switch {
	case st.LedgerIndex != nil:
		raw, err := json.Marshal(toIndexJSON(st.LedgerIndex))
		if err != nil {
			return nil, fmt.Errorf("encode the host ledger index: %w", err)
		}
		j.LedgerIndex = raw
	case len(st.ledgerIndexRaw) > 0:
		j.LedgerIndex = st.ledgerIndexRaw
	}
	b, err := json.Marshal(j)
	if err != nil {
		return nil, fmt.Errorf("encode the host store state: %w", err)
	}
	return b, nil
}

// LoadStoreState implements HostTrustSource.
func (p *PinsHostTrust) LoadStoreState(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) (HostStoreState, error) {
	h, err := p.host(ctx, provider, hostID)
	if err != nil {
		return HostStoreState{}, err
	}
	l, err := loadHostEnrolment(ctx, p.secretReader(), p.Config, h)
	if err != nil {
		return HostStoreState{}, err
	}
	if l.secret == nil || l.reset {
		return HostStoreState{}, nil
	}
	return decodeStoreState(l.secret.Data[hostStoreStateKey])
}

// UpdateStoreState implements HostTrustSource: an uncached read of the pins
// Secret, the update, and a write that carries the read resourceVersion (a
// concurrent writer makes it fail with a conflict, never overwritten; the
// caller retries a conflict with a fresh read). The enrolment state in the
// same Secret is left as it is.
func (p *PinsHostTrust) UpdateStoreState(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
	update func(*HostStoreState)) error {
	h, err := p.host(ctx, provider, hostID)
	if err != nil {
		return err
	}
	l, err := loadHostEnrolment(ctx, p.secretReader(), p.Config, h)
	if err != nil {
		return err
	}
	if l.secret == nil || l.reset {
		return fmt.Errorf("Host %s/%s has no pins Secret: %w", h.Namespace, h.Name, operation.ErrNoEnrolment)
	}
	st, err := decodeStoreState(l.secret.Data[hostStoreStateKey])
	if err != nil {
		return err
	}
	update(&st)
	data, err := encodeStoreState(st)
	if err != nil {
		return err
	}
	sec := l.secret.DeepCopy()
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[hostStoreStateKey] = data
	if err := p.Client.Update(ctx, sec); err != nil {
		return fmt.Errorf("write the store state of Host %s/%s: %w", h.Namespace, h.Name, err)
	}
	return nil
}

// Persistence cadence of the store positions: at most every
// storePersistInterval or every hostidentity.CheckpointInterval sequence
// numbers, and always before a definite answer above the persisted position
// is acted on.
const storePersistInterval = 30 * time.Second

// hostAnswerTTL is how long a verified host answer lets the manager sign new
// operations for its host.
const hostAnswerTTL = 2 * hostHeartbeatInterval

// hostAnswerState is what the manager holds in memory about one host.
type hostAnswerState struct {
	loaded       bool
	store        HostStoreState
	persistedSeq uint64
	persistedAt  time.Time
	// regressionPending reports a store-regression hold that is held in
	// memory but not yet written to the pins Secret: every read of the host
	// retries the write until it lands.
	regressionPending bool
	// verified reports that this process verified an answer of the host.
	verified bool
	// anchored reports that this process has a verified position of the
	// host persisted (written by it, or covered by the persisted one): until
	// then nothing new is signed for the host, so every send of a manager
	// that just started, or just took over, rests on a position in the pins
	// Secret.
	anchored bool
	// recordAnchored names the attempts (op_id hex) whose first answer
	// showing a record was persisted: a later NO_RECORD for them cannot pass
	// a rollback to before that record.
	recordAnchored map[string]bool
	// hostAnswerAt is when the last verified host answer arrived; its
	// enrolment, store and grant versions.
	hostAnswerAt      time.Time
	hostAnswerEnrol   []byte
	hostAnswerStore   [16]byte
	hostGrantVersions []uint32
	hostAgentState    providerv1.AgentState
	// hostAPIVersion is the agent API version of the last verified host
	// answer: a request field is sent only to an agent of the version that
	// introduced it.
	hostAPIVersion uint32
	// mirroredAt is when the host's ledger mirrors were last refreshed
	// (mirrorLedger).
	mirroredAt time.Time
	// hostAnswerHostUID is the UID of the Host object the last verified
	// host answer was read for (empty: not known).
	hostAnswerHostUID string
	// hostSession is the attestation session the last verified host answer
	// named (zero: none, or the request asked for no ledger state).
	hostSession [16]byte
}

// HostAnswers is the manager's shared state of signed host answers, used by
// the Host controller (which asks every host of such a Provider for a host
// answer on its heartbeat) and the VirtualMachine controller (which asks
// about operations and VM placements). The zero value is not usable: set
// Trust.
type HostAnswers struct {
	// Trust is the source of host trust and store state.
	Trust HostTrustSource
	// Now is the clock (nil: time.Now).
	Now func() time.Time

	// Inventory is the host inventory policy (nil: the default policy,
	// DefaultHostInventoryPolicy).
	Inventory *HostInventoryPolicy

	// Ledger is the host ledger of a manager with host ledger keys (nil:
	// none); set it with AttachLedger. Nothing signed reads it yet; the
	// heartbeat mirrors it into Host.status.agentLedger.
	Ledger *HostLedger

	mu    sync.Mutex
	hosts map[string]*hostAnswerState

	wmu     sync.Mutex
	writers map[string]*hostWriter

	// imu guards invBackoff, the inventory backoff of each host. It is kept
	// apart from hosts, so forgetting a host's store positions never resets
	// its backoff.
	imu        sync.Mutex
	invBackoff map[string]*inventoryBackoff

	// freezeProbes is the probe backoff of each confirmed predecessor
	// (checkFreezeDiscards), keyed by provider, host and enrolment id. The
	// zero value is ready.
	freezeProbes sync.Map

	// lmu guards ledgers, the attestation driver's state of each host
	// (host_ledger_driver.go).
	lmu     sync.Mutex
	ledgers map[string]hostLedgerState
	// buckets are the host ledger's split token buckets.
	buckets ledgerBuckets
}

// hostWriter serialises the store-state writes of one host and paces them
// after a failure, so a failing API server sees about one write per host per
// backoff interval, however many VMs of the host are reconciled.
type hostWriter struct {
	mu       sync.Mutex
	failures int
	until    time.Time
}

// Pacing of a host's store-state writes after a failure: from
// storeWriteBackoffMin doubling to storeWriteBackoffMax.
const (
	storeWriteBackoffMin = time.Second
	storeWriteBackoffMax = time.Minute
)

// errStoreWriteBackoff is updateStore's answer while a host's writes are
// paced after a failure: nothing is sent to the API server.
var errStoreWriteBackoff = errors.New("the host's store state writes are paced after a failure")

// now returns the clock.
func (h *HostAnswers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// hostAnswerKey names one host of one Provider.
func hostAnswerKey(provider *infravirtrigaudiov1beta1.Provider, hostID string) string {
	return string(provider.UID) + "/" + hostID
}

// state returns the in-memory state of a host, loading its persisted store
// state the first time. The caller holds h.mu.
func (h *HostAnswers) state(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) (*hostAnswerState, error) {
	if h.hosts == nil {
		h.hosts = map[string]*hostAnswerState{}
	}
	k := hostAnswerKey(provider, hostID)
	s := h.hosts[k]
	if s == nil {
		s = &hostAnswerState{}
		h.hosts[k] = s
	}
	if !s.loaded {
		st, err := h.Trust.LoadStoreState(ctx, provider, hostID)
		if err != nil {
			return nil, err
		}
		if s.regressionPending {
			// A hold this manager has not yet persisted outlives a reload.
			st.RegressedReason, st.RegressedEnrolmentID = s.store.RegressedReason, s.store.RegressedEnrolmentID
		}
		s.store, s.loaded, s.persistedSeq, s.persistedAt = st, true, st.Current.Seq, h.now()
	}
	return s, nil
}

// writer returns the write serialiser of a host.
func (h *HostAnswers) writer(key string) *hostWriter {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	if h.writers == nil {
		h.writers = map[string]*hostWriter{}
	}
	w := h.writers[key]
	if w == nil {
		w = &hostWriter{}
		h.writers[key] = w
	}
	return w
}

// updateStore applies update to the host's persisted store state, retrying a
// write conflict with a fresh read (every update is a monotonic merge, so a
// retry is idempotent). One write per host runs at a time; a caller that
// waited for another's write first checks satisfied (nil: never) and skips
// its own when that write already did what it needs. After a failure the
// host's writes are paced (errStoreWriteBackoff), doubling up to
// storeWriteBackoffMax.
func (h *HostAnswers) updateStore(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
	satisfied func() bool, update func(*HostStoreState)) error {
	w := h.writer(hostAnswerKey(provider, hostID))
	w.mu.Lock()
	defer w.mu.Unlock()
	if satisfied != nil && satisfied() {
		return nil
	}
	now := h.now()
	if now.Before(w.until) {
		return errStoreWriteBackoff
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return h.Trust.UpdateStoreState(ctx, provider, hostID, update)
	})
	if err != nil {
		w.failures++
		wait := storeWriteBackoffMin
		for i := 1; i < w.failures && wait < storeWriteBackoffMax; i++ {
			wait *= 2
		}
		w.until = now.Add(min(wait, storeWriteBackoffMax))
		return err
	}
	w.failures, w.until = 0, time.Time{}
	return nil
}

// positionPersisted is updateStore's satisfied check for a position write:
// the host's persisted position is already at least seq.
func (h *HostAnswers) positionPersisted(provider *infravirtrigaudiov1beta1.Provider, hostID string, seq uint64) func() bool {
	return func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		s := h.hosts[hostAnswerKey(provider, hostID)]
		return s != nil && s.loaded && s.persistedSeq >= seq
	}
}

// recordIsAnchored reports whether the first answer of attempt opID showing a
// record was persisted.
func (h *HostAnswers) recordIsAnchored(provider *infravirtrigaudiov1beta1.Provider, hostID, opID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.hosts[hostAnswerKey(provider, hostID)]
	return s != nil && s.recordAnchored[opID]
}

// setRecordAnchored records (on) or forgets (off) that the first answer of
// attempt opID showing a record was persisted.
func (h *HostAnswers) setRecordAnchored(provider *infravirtrigaudiov1beta1.Provider, hostID, opID string, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.hosts[hostAnswerKey(provider, hostID)]
	if s == nil {
		return
	}
	if !on {
		delete(s.recordAnchored, opID)
		return
	}
	if s.recordAnchored == nil {
		s.recordAnchored = map[string]bool{}
	}
	s.recordAnchored[opID] = true
}

// answerVerdict is what one signed read gave the manager.
type answerVerdict int

const (
	// answerNone: no answer (missing, failed a check, or not READY when
	// that was required). Never negative, never positive.
	answerNone answerVerdict = iota
	// answerVerified: a verified answer, its store continuity checked.
	answerVerified
	// answerRegressed: verified, but the host's store regressed; the host
	// is held.
	answerRegressed
)

// hostRead is the context of one signed read: the host's trust and the
// store record snapshot taken before the request was sent.
type hostRead struct {
	trust    HostTrust
	snapshot hostidentity.StoreRecord
	// inventoryVersion is the inventory version a GetHostInfo read sent
	// (0: none).
	inventoryVersion uint32
	// renewalVersion is the renewal version a GetHostInfo read sent (0:
	// none).
	renewalVersion uint32
	// storeCapacityVersion is the store capacity version a GetHostInfo read
	// sent (0: none).
	storeCapacityVersion uint32
	// ledgerVersion is the host ledger version the read sent (0: none).
	ledgerVersion uint32
}

// errHostTrust wraps begin's failures to read the manager's own trust or
// store state of a host (no enrolment, pins, CA, or an unreadable Secret).
var errHostTrust = errors.New("no trust state for the host")

// errHostRegressed is begin's answer for a host held for a store
// regression under its current enrolment.
var errHostRegressed = errors.New("the host's operation store regressed")

// begin prepares a signed read of a host: its trust and the store record
// snapshot whose continuity_seq the request names. It fails with
// errHostRegressed for a held host, or with the trust source's error.
func (h *HostAnswers) begin(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) (hostRead, error) {
	t, err := h.Trust.HostTrust(ctx, provider, hostID)
	if err != nil {
		return hostRead{}, fmt.Errorf("%w: %w", errHostTrust, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, err := h.state(ctx, provider, hostID)
	if err != nil {
		return hostRead{}, fmt.Errorf("%w: %w", errHostTrust, err)
	}
	if s.store.RegressedReason != "" && bytes.Equal(s.store.RegressedEnrolmentID, t.Enrolment.EnrolmentID) {
		reason, enrol, pending := s.store.RegressedReason, bytes.Clone(s.store.RegressedEnrolmentID), s.regressionPending
		h.mu.Unlock()
		if pending {
			h.persistRegression(ctx, provider, hostID, reason, enrol)
		}
		h.mu.Lock()
		return hostRead{}, fmt.Errorf("%w (%s)", errHostRegressed, reason)
	}
	return hostRead{trust: t, snapshot: s.store.Current}, nil
}

// blobOf is the SignedBlob of a relayed answer (nil for none).
func blobOf(a *contracts.SignedAnswer) *providerv1.SignedBlob {
	if a == nil {
		return nil
	}
	return &providerv1.SignedBlob{BodyType: a.BodyType, Body: a.Body, Signature: a.Signature, SignerCertificate: a.SignerCertificate}
}

// finish verifies the answer of a read begun with hr, checks its store
// continuity and raises the host's record. definite says the caller will act
// on the answer: the raised position is then persisted first, and a failed
// write is no answer.
func (h *HostAnswers) finish(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, hr hostRead,
	bodyType string, read contracts.SignedRead, definite func(hostidentity.VerifiedAnswer) bool) (hostidentity.VerifiedAnswer, answerVerdict) {
	v, err := h.verify(ctx, provider, hostID, hr, bodyType, read)
	if err != nil {
		return hostidentity.VerifiedAnswer{}, answerNone
	}
	return h.finishVerified(ctx, provider, hostID, hr, v, definite)
}

// verify verifies the answer of a read begun with hr; a failure is logged
// and is no answer.
func (h *HostAnswers) verify(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, hr hostRead,
	bodyType string, read contracts.SignedRead) (hostidentity.VerifiedAnswer, error) {
	exp := hr.trust.expectation(provider, hostID, bodyType, read.Nonce, h.now())
	exp.InventoryVersion = hr.inventoryVersion
	exp.RenewalVersion = hr.renewalVersion
	exp.StoreCapacityVersion = hr.storeCapacityVersion
	exp.LedgerVersion, exp.ContinuitySeq = hr.ledgerVersion, hr.snapshot.ContinuitySeq()
	v, err := hostidentity.VerifyAnswer(blobOf(read.Answer), exp)
	if err != nil {
		log.FromContext(ctx).Info("No signed host answer; nothing is concluded", "host", hostID, "check", err.Error())
		return hostidentity.VerifiedAnswer{}, err
	}
	return v, nil
}

// finishVerified checks the store continuity of a verified answer of a read
// begun with hr and raises the host's record (see finish).
func (h *HostAnswers) finishVerified(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string, hr hostRead,
	v hostidentity.VerifiedAnswer, definite func(hostidentity.VerifiedAnswer) bool) (hostidentity.VerifiedAnswer, answerVerdict) {
	logger := log.FromContext(ctx)
	c := hostidentity.CheckContinuity(hr.snapshot, v)
	if c.Regressed {
		h.markRegressed(ctx, provider, hostID, c.Reason, v.Context.GetEnrolmentId())
		return v, answerRegressed
	}
	mustPersist := definite != nil && definite(v)
	h.mu.Lock()
	s, err := h.state(ctx, provider, hostID)
	if err != nil {
		h.mu.Unlock()
		logger.Info("Cannot read the host's store state; nothing is concluded", "host", hostID, "error", err.Error())
		return v, answerNone
	}
	if s.store.Current.StoreID != c.Next.StoreID && !s.store.Current.Empty() {
		s.store.Previous = append([]hostidentity.StoreRecord{s.store.Current}, s.store.Previous...)
		if len(s.store.Previous) > maxPreviousStores {
			s.store.Previous = s.store.Previous[:maxPreviousStores]
		}
	}
	s.store.Current = hostidentity.MergeStoreRecords(s.store.Current, c.Next)
	s.verified = true
	if s.store.Current.Seq <= s.persistedSeq {
		s.anchored = true
	}
	now := h.now()
	persist := (mustPersist && s.store.Current.Seq > s.persistedSeq) || now.Sub(s.persistedAt) >= storePersistInterval ||
		s.store.Current.Seq-s.persistedSeq >= hostidentity.CheckpointInterval
	want := s.store
	h.mu.Unlock()
	if !persist {
		return v, answerVerified
	}
	err = h.updateStore(ctx, provider, hostID, h.positionPersisted(provider, hostID, want.Current.Seq), func(st *HostStoreState) {
		if st.Current.StoreID != want.Current.StoreID && !st.Current.Empty() {
			st.Previous = append([]hostidentity.StoreRecord{st.Current}, st.Previous...)
			if len(st.Previous) > maxPreviousStores {
				st.Previous = st.Previous[:maxPreviousStores]
			}
			st.Current = want.Current
			return
		}
		st.Current = hostidentity.MergeStoreRecords(st.Current, want.Current)
	})
	if err != nil {
		logger.Info("Cannot persist the host's store position", "host", hostID, "error", err.Error())
		if mustPersist {
			// Forget the in-memory state: the next read reloads what is
			// persisted.
			h.forget(provider, hostID)
			return v, answerNone
		}
		return v, answerVerified
	}
	h.mu.Lock()
	if s, ok := h.hosts[hostAnswerKey(provider, hostID)]; ok {
		s.persistedSeq, s.persistedAt = max(s.persistedSeq, want.Current.Seq), now
		s.anchored = true
	}
	h.mu.Unlock()
	return v, answerVerified
}

// forget drops the in-memory store positions of a host, so its next read
// reloads what is persisted. A store-regression hold is kept, persisted or
// not: forgetting positions never lifts a hold.
func (h *HostAnswers) forget(provider *infravirtrigaudiov1beta1.Provider, hostID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.hosts[hostAnswerKey(provider, hostID)]
	if s == nil {
		return
	}
	if s.store.RegressedReason == "" {
		delete(h.hosts, hostAnswerKey(provider, hostID))
		return
	}
	held := hostAnswerState{regressionPending: s.regressionPending}
	held.store.RegressedReason, held.store.RegressedEnrolmentID = s.store.RegressedReason, s.store.RegressedEnrolmentID
	h.hosts[hostAnswerKey(provider, hostID)] = &held
}

// markRegressed holds a host for a store regression under enrolment: in
// memory at once, then persisted. A write that fails leaves the hold pending,
// and every later read of the host retries it (begin), so a manager restart
// never lifts a hold this manager decided.
func (h *HostAnswers) markRegressed(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID, reason string, enrolment []byte) {
	log.FromContext(ctx).Info("The host's operation store regressed; the host is held until it is re-enrolled", "host", hostID,
		"reason", reason)
	h.mu.Lock()
	if h.hosts == nil {
		h.hosts = map[string]*hostAnswerState{}
	}
	k := hostAnswerKey(provider, hostID)
	s := h.hosts[k]
	if s == nil {
		s = &hostAnswerState{}
		h.hosts[k] = s
	}
	s.store.RegressedReason, s.store.RegressedEnrolmentID, s.regressionPending = reason, bytes.Clone(enrolment), true
	h.mu.Unlock()
	h.persistRegression(ctx, provider, hostID, reason, enrolment)
}

// persistRegression writes a store-regression hold to the pins Secret and,
// once written, clears its pending mark.
func (h *HostAnswers) persistRegression(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID, reason string, enrolment []byte) {
	persisted := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		s := h.hosts[hostAnswerKey(provider, hostID)]
		return s != nil && !s.regressionPending && s.store.RegressedReason == reason
	}
	if err := h.updateStore(ctx, provider, hostID, persisted, func(st *HostStoreState) {
		st.RegressedReason, st.RegressedEnrolmentID = reason, bytes.Clone(enrolment)
	}); err != nil {
		log.FromContext(ctx).Info("Cannot persist the host's store regression; it is held in memory and written again on the next read",
			"host", hostID, "error", err.Error())
		return
	}
	h.mu.Lock()
	if s := h.hosts[hostAnswerKey(provider, hostID)]; s != nil && s.store.RegressedReason == reason {
		s.regressionPending = false
	}
	h.mu.Unlock()
}

// persistPosition writes the host's in-memory store position when it is above
// the persisted one: the position whose continuity a later answer is checked
// against survives a restart. It is called before an attempt is first sent.
func (h *HostAnswers) persistPosition(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) error {
	h.mu.Lock()
	s, err := h.state(ctx, provider, hostID)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	want, persisted := s.store.Current, s.persistedSeq
	if !want.Empty() && want.Seq <= persisted && s.verified {
		s.anchored = true
	}
	h.mu.Unlock()
	if want.Empty() || want.Seq <= persisted {
		return nil
	}
	if err := h.updateStore(ctx, provider, hostID, h.positionPersisted(provider, hostID, want.Seq), func(st *HostStoreState) {
		if st.Current.Empty() || st.Current.StoreID == want.StoreID {
			st.Current = hostidentity.MergeStoreRecords(st.Current, want)
		}
	}); err != nil {
		return fmt.Errorf("persist the store position of host %s: %w", hostID, err)
	}
	h.mu.Lock()
	if s := h.hosts[hostAnswerKey(provider, hostID)]; s != nil {
		s.persistedSeq, s.persistedAt = max(s.persistedSeq, want.Seq), h.now()
		s.anchored = s.anchored || s.verified
	}
	h.mu.Unlock()
	return nil
}

// RegressedReason returns why the host is held for a store regression under
// its current enrolment, or "".
func (h *HostAnswers) RegressedReason(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string) string {
	_, err := h.begin(ctx, provider, hostID)
	if errors.Is(err, errHostRegressed) {
		return err.Error()
	}
	return ""
}

// errNoHostAnswer is grantReady's answer for a host without a recent enough
// verified host answer.
var errNoHostAnswer = errors.New("no recent verified host answer")

// grantReady reports whether new operations may be signed for the host,
// and at which grant version: a verified host answer under its current
// enrolment, younger than hostAnswerTTL, from a READY agent. A manager with
// a host ledger signs grant version 2 for an agent that implements it, and
// only while the agent is attested in the ledger's adopted session
// (ledgerGrantReady): never version 1 to such an agent. Otherwise it signs
// version 1 for an agent that implements it, as before.
func (h *HostAnswers) grantReady(provider *infravirtrigaudiov1beta1.Provider, hostID string, enrolment []byte) (uint32, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.hosts[hostAnswerKey(provider, hostID)]
	switch {
	case s == nil || s.hostAnswerAt.IsZero() || h.now().Sub(s.hostAnswerAt) > hostAnswerTTL:
		return 0, errNoHostAnswer
	case !bytes.Equal(s.hostAnswerEnrol, enrolment):
		return 0, fmt.Errorf("%w under the host's current enrolment", errNoHostAnswer)
	case !s.anchored:
		return 0, errors.New("no verified position of the host is persisted yet by this manager")
	case s.hostAgentState != providerv1.AgentState_AGENT_STATE_READY:
		return 0, fmt.Errorf("the host's agent is %s", s.hostAgentState)
	case h.Ledger != nil && s.hostAPIVersion >= ledgerAgentAPIVersion && slices.Contains(s.hostGrantVersions, grantverify.LedgerGrantVersion):
		if err := h.ledgerGrantReady(provider, hostID, s); err != nil {
			return 0, err
		}
		return grantverify.LedgerGrantVersion, nil
	case !slices.Contains(s.hostGrantVersions, grantverify.GrantVersion):
		return 0, fmt.Errorf("the host's agent implements no grant version this manager signs")
	}
	return grantverify.GrantVersion, nil
}

// RefreshHostAnswer asks the host for a signed host answer through reader
// and, when it verifies, records it (what grantReady reads). It returns the
// plain host information beside it (the Provider's unsigned claim), the
// verified answer and the verdict. It asks for the host inventory as
// refreshHost does.
func (h *HostAnswers) RefreshHostAnswer(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, reader contracts.HostAnswerReader,
	hostID string) (contracts.HostInfo, hostidentity.VerifiedAnswer, answerVerdict, error) {
	r, err := h.refreshHost(ctx, provider, reader, hostID)
	return r.info, r.answer, r.verdict, err
}

// hostRefresh is the outcome of one signed host read.
type hostRefresh struct {
	// info is the Provider's unsigned host information.
	info contracts.HostInfo
	// answer is the verified answer (when verdict is answerVerified).
	answer hostidentity.VerifiedAnswer
	// verdict is what the read gave.
	verdict answerVerdict
	// read is the signed read (its relayed answer is logged on an
	// inventory condition transition).
	read contracts.SignedRead
	// asked is the inventory version the read sent (0: none).
	asked uint32
	// inventoryBackoff reports that the read did not ask for the inventory
	// because the host's agent recently signed an invalid one, or that it
	// signed one in this read.
	inventoryBackoff bool
	// verifiedAt is the manager's clock when the answer verified.
	verifiedAt time.Time
}

// refreshHost asks the host for a signed host answer with its host inventory
// (the highest inventory version the manager implements, except while the
// host's agent is in an inventory backoff) and its store capacity, and, when
// it verifies, records it
// (what grantReady reads). An answer without the inventory asked for is still
// a valid answer for everything else. An answer whose inventory does not echo
// the request is no answer and changes nothing else: the next read asks
// again. An answer whose inventory breaks a field rule is no answer and
// starts or extends the host's inventory backoff.
func (h *HostAnswers) refreshHost(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, reader contracts.HostAnswerReader,
	hostID string) (hostRefresh, error) {
	hr, err := h.begin(ctx, provider, hostID)
	if err != nil {
		if errors.Is(err, errHostRegressed) {
			return hostRefresh{verdict: answerRegressed}, nil
		}
		return hostRefresh{verdict: answerNone}, err
	}
	hr.inventoryVersion = h.inventoryAsk(provider, hostID)
	hr.renewalVersion = h.renewalAsk(provider, hostID)
	hr.storeCapacityVersion = hostidentity.StoreCapacityVersion
	hr.ledgerVersion = h.ledgerAsk(provider, hostID)
	out := hostRefresh{asked: hr.inventoryVersion, inventoryBackoff: hr.inventoryVersion == 0}
	info, read, rerr := reader.HostAnswer(ctx, contracts.HostAnswerQuery{HostID: hostID, ContinuitySeq: hr.snapshot.ContinuitySeq(),
		InventoryVersion: hr.inventoryVersion, RenewalVersion: hr.renewalVersion, StoreCapacityVersion: hr.storeCapacityVersion,
		LedgerVersion: hr.ledgerVersion})
	out.info, out.read = info, read
	if rerr != nil {
		out.verdict = answerNone
		return out, rerr
	}
	v, verr := h.verify(ctx, provider, hostID, hr, hostidentity.BodyTypeHostAnswer, read)
	if verr != nil && len(hr.trust.RenewalCertificate) > 0 && read.Answer != nil &&
		bytes.Equal(read.Answer.SignerCertificate, hr.trust.RenewalCertificate) {
		// Signed by exactly the renewed certificate the manager issued for
		// this host and not yet pinned: verified again with its key pinned
		// as well (every other check unchanged), so the renewal sees that
		// the host put it in use early.
		early := hr
		early.trust.Pins = append(slices.Clone(hr.trust.Pins), hr.trust.RenewalKeyPin)
		v, verr = h.verify(ctx, provider, hostID, early, hostidentity.BodyTypeHostAnswer, read)
	}
	if verr != nil {
		if errors.Is(verr, hostidentity.ErrInventoryInvalid) {
			h.inventoryFault(provider, hostID)
			out.inventoryBackoff = true
		}
		out.verdict = answerNone
		return out, nil
	}
	out.verifiedAt = h.now()
	// Until this process has a verified position of the host persisted, the
	// host answer's position is written before it counts.
	unanchored := func(hostidentity.VerifiedAnswer) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		s := h.hosts[hostAnswerKey(provider, hostID)]
		return s == nil || !s.anchored
	}
	v, out.verdict = h.finishVerified(ctx, provider, hostID, hr, v, unanchored)
	out.answer = v
	if out.verdict != answerVerified {
		return out, nil
	}
	h.inventoryAnswered(provider, hostID, v, hr.inventoryVersion)
	h.mu.Lock()
	if s, err := h.state(ctx, provider, hostID); err == nil {
		s.hostAnswerAt, s.hostAnswerEnrol = h.now(), bytes.Clone(v.Context.GetEnrolmentId())
		s.hostAnswerStore, s.hostGrantVersions = v.Position.StoreID, slices.Clone(v.Host.GetGrantVersions())
		s.hostAgentState = v.Context.GetAgentState()
		s.hostAPIVersion = v.Host.GetAgentApiVersion()
		s.hostAnswerHostUID = hr.trust.HostUID
		s.hostSession, _ = sessionOf(v.Context)
	}
	h.mu.Unlock()
	if hr.ledgerVersion > 0 && h.Ledger != nil {
		h.driveLedger(ctx, provider, reader, hostID, hr, v)
	}
	return out, nil
}

// auditSignedDecision writes the audit record of a decision a signed answer
// drove (a definite closure or a finalizer release): the whole signed answer
// in the log, and its digest, signer pin, chain position, enrolment and
// op_ids in what the caller records as an event.
func auditSignedDecision(ctx context.Context, read contracts.SignedRead, v hostidentity.VerifiedAnswer, decision string, opIDs ...string) string {
	enc := ""
	if read.Answer != nil {
		blob := blobOf(read.Answer)
		if raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(blob); err == nil {
			enc = base64.StdEncoding.EncodeToString(raw)
		}
	}
	summary := fmt.Sprintf("%s on a host-signed answer: answer sha256 %s, signer key %s, chain position %s, enrolment %s, op_id %v",
		decision, hex.EncodeToString(v.BlobSHA256[:]), v.SignerPin, hex.EncodeToString(v.Context.GetChainPosition()),
		hex.EncodeToString(v.Context.GetEnrolmentId()), opIDs)
	log.FromContext(ctx).Info("Signed decision", "decision", decision, "summary", summary, "signedAnswer", enc)
	return summary
}

// NewHostAnswers returns the manager's state of signed host answers for the
// enrolment configuration cfg (nil: none, and nothing is sent to a Provider
// that requires operation identity). Grants are issued under an
// identity-only epoch whose holder is the install id.
func NewHostAnswers(c client.Client, reader client.Reader, cfg *HostEnrolmentConfig, installID []byte) *HostAnswers {
	if cfg == nil || len(installID) != operation.IDLen {
		return nil
	}
	return &HostAnswers{Trust: &PinsHostTrust{Client: c, Reader: reader, Config: cfg,
		Epoch: operation.Epoch{Counter: baseMintEpochCounter, Holder: bytes.Clone(installID)}}}
}

// grantAnchored is a signer that names its grant anchor (the development
// signer).
type grantAnchored interface {
	Anchor() *x509.Certificate
}

// errNoGrantAnchor is CheckHostAnswerTrust's answer when enrolment is
// configured but the signer names no grant anchor to check against.
var errNoGrantAnchor = errors.New("per-host agent enrolment is configured, but the operation signer names no grant anchor to check the host-agent CA against")

// CheckHostAnswerTrust checks, at start-up, that the host-agent CA of cfg
// and the grant anchor of signer share no certificate, key or issuer
// (hostidentity.CheckHierarchySeparation). Without enrolment there is
// nothing to check; with it, a signer that names no grant anchor fails
// closed.
func CheckHostAnswerTrust(cfg *HostEnrolmentConfig, signer operation.Signer) error {
	if cfg == nil {
		return nil
	}
	if signer == nil {
		return errNoGrantAnchor
	}
	a, ok := signer.(grantAnchored)
	if !ok || a.Anchor() == nil {
		return errNoGrantAnchor
	}
	hostCAs, err := hostidentity.ParseCertificates(cfg.CABundle)
	if err != nil {
		return fmt.Errorf("host-agent CA bundle: %w", err)
	}
	if err := hostidentity.CheckHierarchySeparation(hostCAs, []*x509.Certificate{a.Anchor()}); err != nil {
		return fmt.Errorf("host-agent CA and operation-grant anchor: %w", err)
	}
	return nil
}
