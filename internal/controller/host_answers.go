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
	// Predecessors are earlier enrolments of the host that the host still
	// holds frozen: an attempt under one may be asked about, never re-sent.
	// Empty until a re-enrolment records one.
	Predecessors [][]byte
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
	t := HostTrust{
		Enrolment: operation.Enrolment{EnrolmentID: rec.EnrolmentID,
			Epoch: operation.Epoch{Counter: p.Epoch.Counter, Holder: bytes.Clone(p.Epoch.Holder)}},
		InstallID: bytes.Clone(p.Config.InstallID), Pins: slices.Clone(cur.KeyPins), Anchors: anchors, HostURI: uri, HostName: h.Name,
	}
	for _, s := range l.state.PredecessorEnrolmentIDs {
		if id, err := hostidentity.DecodeID(s); err == nil {
			t.Predecessors = append(t.Predecessors, id)
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
}

// hostStoreStateJSON is the JSON form of a HostStoreState.
type hostStoreStateJSON struct {
	Current              *storeRecordJSON  `json:"current,omitempty"`
	Previous             []storeRecordJSON `json:"previous,omitempty"`
	RegressedReason      string            `json:"regressedReason,omitempty"`
	RegressedEnrolmentID string            `json:"regressedEnrolmentID,omitempty"`
}

func toStoreJSON(r hostidentity.StoreRecord) storeRecordJSON {
	j := storeRecordJSON{StoreID: hex.EncodeToString(r.StoreID[:]), EnrolmentID: hex.EncodeToString(r.EnrolmentID), Seq: r.Seq,
		Head: hex.EncodeToString(r.Head[:]), CheckpointSeq: r.CheckpointSeq, UseCheckpoint: r.UseCheckpoint}
	if r.CheckpointSeq > 0 {
		j.CheckpointHead = hex.EncodeToString(r.CheckpointHead[:])
	}
	return j
}

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
	if j.Current != nil {
		r, err := fromStoreJSON(*j.Current)
		if err != nil {
			return st, err
		}
		st.Current = r
	}
	for _, p := range j.Previous {
		r, err := fromStoreJSON(p)
		if err != nil {
			return st, err
		}
		st.Previous = append(st.Previous, r)
	}
	st.RegressedReason = j.RegressedReason
	if j.RegressedEnrolmentID != "" {
		id, err := hostidentity.DecodeID(j.RegressedEnrolmentID)
		if err != nil {
			return st, errors.New("malformed regressed enrolment")
		}
		st.RegressedEnrolmentID = id
	}
	return st, nil
}

// encodeStoreState encodes st for the store state data key.
func encodeStoreState(st HostStoreState) ([]byte, error) {
	j := hostStoreStateJSON{RegressedReason: st.RegressedReason}
	if !st.Current.Empty() {
		c := toStoreJSON(st.Current)
		j.Current = &c
	}
	for _, p := range st.Previous {
		j.Previous = append(j.Previous, toStoreJSON(p))
	}
	if len(st.RegressedEnrolmentID) > 0 {
		j.RegressedEnrolmentID = hex.EncodeToString(st.RegressedEnrolmentID)
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
	// hostAnswerAt is when the last verified host answer arrived; its
	// enrolment, store and grant versions.
	hostAnswerAt      time.Time
	hostAnswerEnrol   []byte
	hostAnswerStore   [16]byte
	hostGrantVersions []uint32
	hostAgentState    providerv1.AgentState
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

	mu    sync.Mutex
	hosts map[string]*hostAnswerState
}

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

// updateStore applies update to the host's persisted store state, retrying a
// write conflict with a fresh read (every update is a monotonic merge, so a
// retry is idempotent).
func (h *HostAnswers) updateStore(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, hostID string,
	update func(*HostStoreState)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return h.Trust.UpdateStoreState(ctx, provider, hostID, update)
	})
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
	logger := log.FromContext(ctx)
	v, err := hostidentity.VerifyAnswer(blobOf(read.Answer), hr.trust.expectation(provider, hostID, bodyType, read.Nonce, h.now()))
	if err != nil {
		logger.Info("No signed host answer; nothing is concluded", "host", hostID, "check", err.Error())
		return hostidentity.VerifiedAnswer{}, answerNone
	}
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
	now := h.now()
	persist := (mustPersist && s.store.Current.Seq > s.persistedSeq) || now.Sub(s.persistedAt) >= storePersistInterval ||
		s.store.Current.Seq-s.persistedSeq >= hostidentity.CheckpointInterval
	want := s.store
	h.mu.Unlock()
	if !persist {
		return v, answerVerified
	}
	err = h.updateStore(ctx, provider, hostID, func(st *HostStoreState) {
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
	if err := h.updateStore(ctx, provider, hostID, func(st *HostStoreState) {
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
	h.mu.Unlock()
	if want.Empty() || want.Seq <= persisted {
		return nil
	}
	if err := h.updateStore(ctx, provider, hostID, func(st *HostStoreState) {
		if st.Current.Empty() || st.Current.StoreID == want.StoreID {
			st.Current = hostidentity.MergeStoreRecords(st.Current, want)
		}
	}); err != nil {
		return fmt.Errorf("persist the store position of host %s: %w", hostID, err)
	}
	h.mu.Lock()
	if s := h.hosts[hostAnswerKey(provider, hostID)]; s != nil {
		s.persistedSeq, s.persistedAt = max(s.persistedSeq, want.Seq), h.now()
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

// grantReady reports whether new operations may be signed for the host: a
// verified host answer under its current enrolment, younger than
// hostAnswerTTL, from a READY agent that implements the manager's grant
// version.
func (h *HostAnswers) grantReady(provider *infravirtrigaudiov1beta1.Provider, hostID string, enrolment []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.hosts[hostAnswerKey(provider, hostID)]
	switch {
	case s == nil || s.hostAnswerAt.IsZero() || h.now().Sub(s.hostAnswerAt) > hostAnswerTTL:
		return errNoHostAnswer
	case !bytes.Equal(s.hostAnswerEnrol, enrolment):
		return fmt.Errorf("%w under the host's current enrolment", errNoHostAnswer)
	case s.hostAgentState != providerv1.AgentState_AGENT_STATE_READY:
		return fmt.Errorf("the host's agent is %s", s.hostAgentState)
	case !slices.Contains(s.hostGrantVersions, grantverify.GrantVersion):
		return fmt.Errorf("the host's agent implements no grant version this manager signs")
	}
	return nil
}

// RefreshHostAnswer asks the host for a signed host answer through reader
// and, when it verifies, records it (what grantReady reads). It returns the
// plain host information beside it (the Provider's unsigned claim), the
// verified answer and the verdict.
func (h *HostAnswers) RefreshHostAnswer(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, reader contracts.HostAnswerReader,
	hostID string) (contracts.HostInfo, hostidentity.VerifiedAnswer, answerVerdict, error) {
	hr, err := h.begin(ctx, provider, hostID)
	if err != nil {
		if errors.Is(err, errHostRegressed) {
			return contracts.HostInfo{}, hostidentity.VerifiedAnswer{}, answerRegressed, nil
		}
		return contracts.HostInfo{}, hostidentity.VerifiedAnswer{}, answerNone, err
	}
	info, read, rerr := reader.HostAnswer(ctx, hostID, hr.snapshot.ContinuitySeq())
	if rerr != nil {
		return info, hostidentity.VerifiedAnswer{}, answerNone, rerr
	}
	v, verdict := h.finish(ctx, provider, hostID, hr, hostidentity.BodyTypeHostAnswer, read, nil)
	if verdict != answerVerified {
		return info, v, verdict, nil
	}
	h.mu.Lock()
	if s, err := h.state(ctx, provider, hostID); err == nil {
		s.hostAnswerAt, s.hostAnswerEnrol = h.now(), bytes.Clone(v.Context.GetEnrolmentId())
		s.hostAnswerStore, s.hostGrantVersions = v.Position.StoreID, slices.Clone(v.Host.GetGrantVersions())
		s.hostAgentState = v.Context.GetAgentState()
	}
	h.mu.Unlock()
	return info, v, verdict, nil
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
		Epoch: operation.Epoch{Counter: 1, Holder: bytes.Clone(installID)}}}
}

// grantAnchored is a signer that names its grant anchor (the development
// signer).
type grantAnchored interface {
	Anchor() *x509.Certificate
}

// CheckHostAnswerTrust checks, at start-up, that the host-agent CA of cfg
// and the grant anchor of signer share no certificate, key or issuer
// (hostidentity.CheckHierarchySeparation). Nothing to check without either.
func CheckHostAnswerTrust(cfg *HostEnrolmentConfig, signer operation.Signer) error {
	if cfg == nil || signer == nil {
		return nil
	}
	a, ok := signer.(grantAnchored)
	if !ok || a.Anchor() == nil {
		return nil
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
