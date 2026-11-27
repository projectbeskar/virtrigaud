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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
)

// These specs run the host ledger library against a real apiserver: every
// write (initialise, archive, rebind, begin session, raise, adopt), every
// hold, tag tampering, key rotation, regression and partial-restore
// detection, resourceVersion conflicts, and each rebind condition.
var _ = Describe("Host ledger (envtest)", func() {
	ctx := context.Background()

	var (
		hostNS, enrolNS string
		provider        *infravirtrigaudiov1beta1.Provider
		cfg             *HostEnrolmentConfig
		trust           *PinsHostTrust
		keys            *HostLedgerKeys
		rawIdentity     []byte
		rec             *record.FakeRecorder
		now             time.Time
		answers         map[string]time.Time
		ledger          *HostLedger
		identitySalt    string
	)

	random16 := func() [16]byte {
		var b [16]byte
		_, err := rand.Read(b[:])
		Expect(err).NotTo(HaveOccurred())
		return b
	}
	head := func(n byte) [32]byte { return [32]byte{n, 0xee} }

	newLedger := func(k *HostLedgerKeys) *HostLedger {
		l := &HostLedger{Client: k8sClient, APIReader: k8sClient, Trust: trust, Keys: k, Config: cfg, Recorder: rec,
			Now: func() time.Time { return now }}
		l.lastAnswer = func(_, hostID string) time.Time { return answers[hostID] }
		return l
	}

	BeforeEach(func() {
		prev := buildVersion
		buildVersion = func() string { return "v0.5.0" }
		DeferCleanup(func() { buildVersion = prev })
		for _, p := range []string{"ledger-hosts-", "ledger-enrol-"} {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: p}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			if p == "ledger-hosts-" {
				hostNS = ns.Name
			} else {
				enrolNS = ns.Name
			}
		}
		provider = envtestProvider(hostNS, "ch", nil)
		provider.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
		provider.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
		Expect(k8sClient.Create(ctx, provider)).To(Succeed())
		ca := hostidentitytest.NewConstrainedCA(GinkgoT(), enrolTestTrustDomain)
		install := bytes.Repeat([]byte{0x42}, 16)
		cfg = &HostEnrolmentConfig{Namespace: enrolNS, TrustDomain: enrolTestTrustDomain, CABundle: ca.PEM, InstallID: install}
		trust = &PinsHostTrust{Client: k8sClient, Reader: k8sClient, Config: cfg, Epoch: operation.Epoch{Counter: 1, Holder: install}}
		rawIdentity = randomLedgerKey(GinkgoT())
		var err error
		keys, err = NewHostLedgerKeys(randomLedgerKey(GinkgoT()), nil, rawIdentity)
		Expect(err).NotTo(HaveOccurred())
		rec = record.NewFakeRecorder(256)
		now = time.Now().UTC().Truncate(time.Second)
		answers = map[string]time.Time{}
		ledger = newLedger(keys)
		salt := random16()
		identitySalt = hex.EncodeToString(salt[:4])
	})

	// enrol writes the Host's pins Secret with a current enrolment (a new
	// one each call: a re-enrolment) and returns its id.
	enrol := func(h *infravirtrigaudiov1beta1.Host) []byte {
		id := random16()
		l, err := loadHostEnrolment(ctx, k8sClient, cfg, h)
		Expect(err).NotTo(HaveOccurred())
		l.state.Current = &currentEnrolment{
			Record: enrolmentRecord{EnrolmentID: hex.EncodeToString(id[:]), ProviderUID: string(provider.UID),
				InstallID: hex.EncodeToString(cfg.InstallID)},
			KeyPins: []string{"sha256/" + strings.Repeat("A", 43) + "="}, NotAfter: metav1.NewTime(time.Now().Add(time.Hour)),
		}
		Expect(saveHostEnrolment(ctx, k8sClient, cfg, h, &l)).To(Succeed())
		return id[:]
	}

	newHost := func(name, identity string) *infravirtrigaudiov1beta1.Host {
		h := &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hostNS},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "ch"},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "grpc://" + name + ":7443", Identity: identity + "-" + identitySalt, Schedulable: true,
			},
		}
		Expect(k8sClient.Create(ctx, h)).To(Succeed())
		return h
	}

	expectHold := func(err error, reason string, persistent bool) {
		GinkgoHelper()
		var h *HostLedgerHoldError
		Expect(errors.As(err, &h)).To(BeTrue(), "want a hold, got %v", err)
		Expect(h.Reason).To(Equal(reason))
		Expect(h.Persistent).To(Equal(persistent))
		Expect(err).To(MatchError(ErrHostLedgerHeld))
	}

	sawEvent := func(fragment string) bool {
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, fragment) {
					return true
				}
			default:
				return false
			}
		}
	}

	ledgerSecret := func(store [16]byte) *corev1.Secret {
		GinkgoHelper()
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: enrolNS, Name: hostLedgerSecretName(store)}, sec)).To(Succeed())
		return sec
	}

	storeState := func(h *infravirtrigaudiov1beta1.Host) HostStoreState {
		GinkgoHelper()
		st, err := trust.LoadStoreState(ctx, provider, h.Name)
		Expect(err).NotTo(HaveOccurred())
		return st
	}

	// firstContact initialises a store from first contact under enrolment.
	firstContact := func(l *HostLedger, h *infravirtrigaudiov1beta1.Host, store [16]byte, enrolment []byte, seq uint64) HostLedgerInitResult {
		GinkgoHelper()
		res, err := l.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store, Position: HostLedgerPosition{Seq: seq, Head: head(1)},
			Origin: &HostLedgerOrigin{OriginEnrolmentID: enrolment, EnrolmentCount: 1}})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	// attested drives a first-contact ledger to Attested in session s.
	attested := func(l *HostLedger, h *infravirtrigaudiov1beta1.Host, store, s [16]byte, enrolment []byte) {
		GinkgoHelper()
		firstContact(l, h, store, enrolment, 2050)
		_, err := l.BeginSession(ctx, provider, h.Name, store, s, LedgerSessionAttest, HostLedgerPosition{Seq: 2050, Head: head(1)})
		Expect(err).NotTo(HaveOccurred())
		_, err = l.Adopt(ctx, provider, h.Name, store, s, LedgerEvidenceChallengeAnswer)
		Expect(err).NotTo(HaveOccurred())
	}

	Context("initialise", func() {
		It("starts from the last verified position only once the host confirms it", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			last := hostidentity.StoreRecord{StoreID: store, EnrolmentID: e, Seq: 3000, Head: head(3), CheckpointSeq: 2048, CheckpointHead: head(2)}
			Expect(trust.UpdateStoreState(ctx, provider, h.Name, func(st *HostStoreState) { st.Current = last })).To(Succeed())

			in := HostLedgerInit{StoreID: store, Position: HostLedgerPosition{Seq: 3001, Head: head(9)}}
			_, err := ledger.Initialise(ctx, provider, h.Name, in)
			expectHold(err, LedgerReasonUnconfirmed, false)
			Expect(sawEvent(LedgerReasonUnconfirmed)).To(BeTrue())
			in.Continuity = &HostLedgerContinuity{Seq: 3000, Head: head(4)}
			_, err = ledger.Initialise(ctx, provider, h.Name, in)
			expectHold(err, LedgerReasonUnconfirmed, false)
			in.Continuity = &HostLedgerContinuity{Seq: 2048, Head: head(2)}
			_, err = ledger.Initialise(ctx, provider, h.Name, in)
			expectHold(err, LedgerReasonUnconfirmed, false)
			// First contact evidence does not replace the confirmation.
			in.Origin = &HostLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}
			_, err = ledger.Initialise(ctx, provider, h.Name, in)
			expectHold(err, LedgerReasonUnconfirmed, false)

			in.Continuity = &HostLedgerContinuity{Seq: 3000, Head: head(3)}
			res, err := ledger.Initialise(ctx, provider, h.Name, in)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Source).To(Equal(LedgerInitLastPosition))
			r := res.Record
			Expect(r.Position).To(Equal(HostLedgerPosition{Seq: 3000, Head: head(3)}), "the ledger starts at the last position, not the agent's")
			Expect(r.Checkpoint).To(Equal(HostLedgerPosition{Seq: 2048, Head: head(2)}))
			Expect(r.Generation).To(BeNumerically(">=", uint64(now.Unix())))
			Expect(r.GenerationEnrolments).To(Equal([][]byte{e}))
			Expect(r.HostUID).To(Equal(string(h.UID)))
			Expect(r.HostKey).To(Equal(hostNS + "/h"))
			Expect(r.WriteCounter).To(Equal(uint64(1)))
			Expect(r.TagKeyID).To(Equal(keys.CurrentKeyID()))
			Expect(sawEvent(eventReasonLedgerInitialised)).To(BeTrue())

			sec := ledgerSecret(store)
			Expect(sec.OwnerReferences).To(BeEmpty(), "never deleted with a Host")
			Expect(sec.Labels).To(HaveKeyWithValue(k8sLabelComponent, hostLedgerComponent))
			Expect(sec.Labels).To(HaveKeyWithValue(k8sLabelManagedBy, k8sManagedByVirtRigaud))
			Expect(sec.Labels).To(HaveKeyWithValue(LabelHostLedger, hostLedgerFormat))
			Expect(sec.Labels).To(HaveKeyWithValue(LabelHostLedgerEnrolment, hex.EncodeToString(e)))
			mac := keys.IdentityMAC(cfg.InstallID, h.Spec.Identity)
			Expect(sec.Labels).To(HaveKeyWithValue(LabelHostLedgerIdentityMAC, hex.EncodeToString(mac[:])[:32]))
			Expect(string(sec.Data[hostLedgerDataKey])).NotTo(ContainSubstring(h.Spec.Identity), "the identity is keyed, never stored")
			Expect(sec.Data).To(HaveLen(1))
			var doc map[string]any
			Expect(json.Unmarshal(sec.Data[hostLedgerDataKey], &doc)).To(Succeed())
			Expect(doc).To(HaveKeyWithValue("minManagerVersion", hostLedgerMinManagerVersion))
			for _, k := range []string{"storeID", "hostIdentityMAC", "hostUID", "hostKey", "providerUID", "seq", "head", "checkpointSeq",
				"checkpointHead", "ledgerGeneration", "generationStartedAt", "generationEnrolments", "writeCounter", "updatedAt",
				"tagKeyID", "tag"} {
				Expect(doc).To(HaveKey(k))
			}

			st := storeState(h)
			Expect(st.LedgerIndex.Names(store)).To(BeTrue(), "the index names the store")
			Expect(st.LedgerIndex.verifyTag(keys, string(h.UID))).To(BeTrue())
			Expect(st.LedgerIndex.CounterSeen).To(Equal(uint64(1)))

			again, err := ledger.Initialise(ctx, provider, h.Name, in)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.Source).To(Equal(LedgerInitExisting))
			Expect(again.Record.WriteCounter).To(Equal(uint64(1)), "an existing ledger is not written")
		})

		It("starts from first contact only for a store created under the current enrolment alone", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			for name, o := range map[string]*HostLedgerOrigin{
				"no evidence":             nil,
				"another enrolment":       {OriginEnrolmentID: bytes.Repeat([]byte{9}, 16), EnrolmentCount: 1},
				"two enrolments":          {OriginEnrolmentID: e, EnrolmentCount: 2},
				"predates the record":     {},
				"count without enrolment": {EnrolmentCount: 1},
			} {
				_, err := ledger.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store,
					Position: HostLedgerPosition{Seq: 5, Head: head(5)}, Origin: o})
				expectHold(err, LedgerReasonMissing, false)
				_ = name
			}
			res := firstContact(ledger, h, store, e, 5)
			Expect(res.Source).To(Equal(LedgerInitFirstContact))
			Expect(res.Record.Position).To(Equal(HostLedgerPosition{Seq: 5, Head: head(1)}))
			Expect(ledger.HeldReason(provider, h.Name)).To(BeEmpty(), "a success clears the in-memory hold")
		})

		It("never restarts a lost ledger", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e, 7)
			Expect(k8sClient.Delete(ctx, ledgerSecret(store))).To(Succeed())
			_, err := ledger.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store, Position: HostLedgerPosition{Seq: 9, Head: head(1)},
				Origin: &HostLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonMissing, false)
			// A fresh manager (no memory) refuses too: the tagged index names it.
			_, err = newLedger(keys).Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store,
				Position: HostLedgerPosition{Seq: 9, Head: head(1)}, Origin: &HostLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonMissing, false)
		})

		It("holds a second store under one enrolment, persistently", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			firstContact(ledger, h, random16(), e, 7)
			other := random16()
			_, err := ledger.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: other, Position: HostLedgerPosition{Seq: 1, Head: head(1)},
				Origin: &HostLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonStoreMismatch, true)
			st := storeState(h)
			Expect(st.RegressedReason).To(Equal(LedgerReasonStoreMismatch))
			Expect(st.RegressedEnrolmentID).To(Equal(e))
			_, err = ledger.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: other, Position: HostLedgerPosition{Seq: 1, Head: head(1)},
				Origin: &HostLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonStoreMismatch, true)
			// A re-enrolment with a fresh store ends it.
			e2 := enrol(h)
			res := firstContact(ledger, h, other, e2, 1)
			Expect(res.Archived).To(HaveLen(1))
		})

		It("archives the earlier store's ledger when a new enrolment brings a new store", func() {
			h := newHost("h", "serial")
			e1 := enrol(h)
			oldStore := random16()
			old := firstContact(ledger, h, oldStore, e1, 4000).Record
			e2 := enrol(h)
			newStore := random16()
			_, err := ledger.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: newStore, Position: HostLedgerPosition{Seq: 1, Head: head(1)},
				Origin: &HostLedgerOrigin{OriginEnrolmentID: e2, EnrolmentCount: 2}})
			expectHold(err, LedgerReasonMissing, false)
			Expect(ledgerSecret(oldStore).Data[hostLedgerDataKey]).NotTo(ContainSubstring(`"archived"`), "nothing archived without evidence")
			res := firstContact(ledger, h, newStore, e2, 1)
			Expect(res.Archived).To(HaveLen(1))
			a := res.Archived[0]
			Expect(a.Archived).To(BeTrue())
			Expect(a.SuccessorStoreID).To(Equal(newStore))
			Expect(a.Position).To(Equal(old.Position), "an archive keeps the position")
			Expect(res.Record.Generation).To(BeNumerically(">", old.Generation))
			Expect(res.Record.WriteCounter).To(BeNumerically(">", old.WriteCounter), "write counters continue across a host's stores")
			Expect(sawEvent(eventReasonLedgerArchived)).To(BeTrue())
			_, err = ledger.BeginSession(ctx, provider, h.Name, oldStore, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 5000})
			Expect(err).To(MatchError(ErrHostLedgerArchived))
			st := storeState(h)
			Expect(st.LedgerIndex.StoreIDs).To(Equal([][16]byte{oldStore, newStore}))
		})

		It("refuses a ledger written with a downgrade floor above this manager", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			newer := newLedger(keys)
			newer.MinManagerVersion = "9.0.0"
			newer.ManagerVersion = func() string { return "v9.0.0" }
			_, err := newer.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store, Position: HostLedgerPosition{Seq: 3, Head: head(1)},
				Origin: &HostLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			Expect(err).To(MatchError(errManagerTooOld), "this build's pins Secret reader honours the floor too")
			ledgerSecret(store)
			_, err = ledger.Load(ctx, provider, h.Name, store)
			Expect(err).To(MatchError(errManagerTooOld))
		})
	})

	Context("begin session, raise and adopt", func() {
		It("runs the attestation and raise flow with every precondition", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e, 2050)
			s1, s2 := random16(), random16()

			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s1, Position: HostLedgerPosition{Seq: 2100}}})
			Expect(err).To(MatchError(ErrHostLedgerNotAttested))
			_, err = ledger.BeginSession(ctx, provider, h.Name, store, s1, LedgerSessionReattest, HostLedgerPosition{Seq: 2050})
			Expect(err).To(MatchError(ErrHostLedgerNotAttested), "a re-attestation needs an adopted session")
			_, err = ledger.BeginSession(ctx, provider, h.Name, store, s1, LedgerSessionAttest, HostLedgerPosition{Seq: 2049})
			expectHold(err, LedgerReasonAhead, false)

			r, err := ledger.BeginSession(ctx, provider, h.Name, store, s1, LedgerSessionAttest, HostLedgerPosition{Seq: 2050, Head: head(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.AttestationState).To(Equal(infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending))
			Expect(r.HasPendingSession(s1)).To(BeTrue())
			counter := r.WriteCounter
			r, err = ledger.BeginSession(ctx, provider, h.Name, store, s1, LedgerSessionAttest, HostLedgerPosition{Seq: 2050})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.WriteCounter).To(Equal(counter), "a pending session is not added twice")

			_, err = ledger.Adopt(ctx, provider, h.Name, store, s2, LedgerEvidenceHostAnswer)
			Expect(err).To(MatchError(ErrHostLedgerSessionNotPending))
			for _, ev := range []HostLedgerAdoptEvidence{0, LedgerEvidenceAuthorityAnswer + 1, -1} {
				_, err = ledger.Adopt(ctx, provider, h.Name, store, s1, ev)
				Expect(err).To(MatchError(errHostLedgerInput), "only nonce-bound answer evidence adopts")
			}
			r, err = ledger.Adopt(ctx, provider, h.Name, store, s1, LedgerEvidenceHostAnswer)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.AttestationID).To(Equal(s1))
			Expect(r.AttestationState).To(Equal(infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested))
			Expect(r.PendingSessions).To(BeEmpty())
			Expect(r.SessionHistory).To(BeEmpty())

			// Raise: positions of other sessions are dropped; max wins.
			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{
				{Session: s1, Position: HostLedgerPosition{Seq: 2100, Head: head(21)}},
				{Session: s1, Position: HostLedgerPosition{Seq: 2080, Head: head(20)}, Checkpoint: HostLedgerPosition{Seq: 2048, Head: head(48)}},
				{Session: s2, Position: HostLedgerPosition{Seq: 9999, Head: head(99)}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Position).To(Equal(HostLedgerPosition{Seq: 2100, Head: head(21)}))
			Expect(r.Checkpoint).To(Equal(HostLedgerPosition{Seq: 2048, Head: head(48)}))
			counter = r.WriteCounter
			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s2, Position: HostLedgerPosition{Seq: 3000}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.WriteCounter).To(Equal(counter), "nothing to raise, nothing written")
			_, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s1, Position: HostLedgerPosition{Seq: 2101},
				Checkpoint: HostLedgerPosition{Seq: 1000}}})
			Expect(err).To(MatchError(errHostLedgerInput))

			// A re-attestation keeps the working session current.
			r, err = ledger.BeginSession(ctx, provider, h.Name, store, s2, LedgerSessionReattest, HostLedgerPosition{Seq: 2100})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.AttestationID).To(Equal(s1))
			Expect(r.AttestationState).To(Equal(infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested))
			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s1, Position: HostLedgerPosition{Seq: 2200, Head: head(22)}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Position.Seq).To(Equal(uint64(2200)), "receipts of the working session are still ledgered")
			r, err = ledger.Adopt(ctx, provider, h.Name, store, s2, LedgerEvidenceOperationAnswer)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.AttestationID).To(Equal(s2))
			Expect(r.SessionHistory).To(Equal([]HostLedgerFencedSession{{ID: s1, FencedAt: 2200}}), "fenced at the position when replaced")
			counter = r.WriteCounter
			r, err = ledger.Adopt(ctx, provider, h.Name, store, s2, LedgerEvidenceVMAnswer)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.WriteCounter).To(Equal(counter), "adopting the current session again changes nothing")

			// Bounds: four pending sessions, eight earlier sessions.
			var begun [][16]byte
			for range 6 {
				s := random16()
				begun = append(begun, s)
				r, err = ledger.BeginSession(ctx, provider, h.Name, store, s, LedgerSessionReattest, HostLedgerPosition{Seq: 2200})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(pendingIDs(r)).To(Equal(begun[2:]), "the oldest pending sessions are evicted")
			for range 9 {
				s := random16()
				_, err = ledger.BeginSession(ctx, provider, h.Name, store, s, LedgerSessionReattest, HostLedgerPosition{Seq: 2200})
				Expect(err).NotTo(HaveOccurred())
				r, err = ledger.Adopt(ctx, provider, h.Name, store, s, LedgerEvidenceAuthorityAnswer)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(r.SessionHistory).To(HaveLen(maxLedgerSessionHistory))

			// An agent restart: a new attestation makes the ledger Pending.
			s3 := random16()
			r, err = ledger.BeginSession(ctx, provider, h.Name, store, s3, LedgerSessionAttest, HostLedgerPosition{Seq: 2300})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.AttestationState).To(Equal(infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending))
			_, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: r.AttestationID, Position: HostLedgerPosition{Seq: 2400}}})
			Expect(err).To(MatchError(ErrHostLedgerNotAttested), "nothing is raised before the new session is adopted")
		})

		It("raises from heartbeat positions at most every 30 seconds and records the enrolment", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			r, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2300}, Heartbeat: true}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Position.Seq).To(Equal(uint64(2300)))
			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2400}, Heartbeat: true}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Position.Seq).To(Equal(uint64(2300)), "a second heartbeat raise within 30 s is skipped")
			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2350}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Position.Seq).To(Equal(uint64(2350)), "receipt raises are not limited")
			now = now.Add(31 * time.Second)
			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2400}, Heartbeat: true}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Position.Seq).To(Equal(uint64(2400)))

			// A re-key: the next raise records the new enrolment.
			e2 := enrol(h)
			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2500}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.GenerationEnrolments).To(Equal([][]byte{e, e2}))
			Expect(ledgerSecret(store).Labels).To(HaveKeyWithValue(LabelHostLedgerEnrolment, hex.EncodeToString(e2)))
		})

		It("holds a fork persistently", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2050, Head: head(77)}}})
			expectHold(err, LedgerReasonFork, true)
			Expect(storeState(h).RegressedReason).To(Equal(LedgerReasonFork))
			_, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2060, Head: head(1)}}})
			expectHold(err, LedgerReasonFork, true)

			h2 := newHost("h2", "serial-two")
			e2 := enrol(h2)
			store2, s2 := random16(), random16()
			attested(ledger, h2, store2, s2, e2)
			_, err = ledger.Raise(ctx, provider, h2.Name, store2, []HostLedgerRaise{
				{Session: s2, Position: HostLedgerPosition{Seq: 3000, Head: head(30)}},
				{Session: s2, Position: HostLedgerPosition{Seq: 3000, Head: head(31)}},
			})
			expectHold(err, LedgerReasonFork, true)
		})
	})

	Context("tampering, regression and rotation", func() {
		It("holds on a ledger that fails its tag, never on one this manager wrote", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e, 100)
			sec := ledgerSecret(store)
			var doc map[string]any
			Expect(json.Unmarshal(sec.Data[hostLedgerDataKey], &doc)).To(Succeed())
			doc["seq"] = 99999
			sec.Data[hostLedgerDataKey], _ = json.Marshal(doc)
			Expect(k8sClient.Update(ctx, sec)).To(Succeed())
			_, err := newLedger(keys).Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonTampered, false)

			// A forged ledger under another key.
			forgedKeys, err := NewHostLedgerKeys(randomLedgerKey(GinkgoT()), nil, rawIdentity)
			Expect(err).NotTo(HaveOccurred())
			forger := newLedger(forgedKeys)
			h2 := newHost("h2", "serial-two")
			e2 := enrol(h2)
			store2 := random16()
			firstContact(forger, h2, store2, e2, 5)
			_, err = ledger.Load(ctx, provider, h2.Name, store2)
			expectHold(err, LedgerReasonTampered, false)
			Expect(sawEvent(LedgerReasonTampered)).To(BeTrue())
			_, err = ledger.BeginSession(ctx, provider, h2.Name, store2, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 5})
			expectHold(err, LedgerReasonTampered, false)

			// A Secret with a ledger's name that is not a ledger.
			h3 := newHost("h3", "serial-three")
			enrol(h3)
			store3 := random16()
			Expect(k8sClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hostLedgerSecretName(store3), Namespace: enrolNS},
				Data: map[string][]byte{hostLedgerDataKey: []byte(`{}`)}})).To(Succeed())
			_, err = ledger.Load(ctx, provider, h3.Name, store3)
			expectHold(err, LedgerReasonTampered, false)
		})

		It("holds on an index that fails its tag or form, which blocks initialise", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e, 100)
			pins := &corev1.Secret{}
			key := types.NamespacedName{Namespace: enrolNS, Name: hostEnrolmentSecretName(hostNS, h.Name)}
			Expect(k8sClient.Get(ctx, key, pins)).To(Succeed())
			var doc map[string]json.RawMessage
			Expect(json.Unmarshal(pins.Data[hostStoreStateKey], &doc)).To(Succeed())
			var idx map[string]any
			Expect(json.Unmarshal(doc["ledgerIndex"], &idx)).To(Succeed())
			idx["storeIDs"] = []string{} // an emptied index, its tag unchanged
			doc["ledgerIndex"], _ = json.Marshal(idx)
			pins.Data[hostStoreStateKey], _ = json.Marshal(doc)
			Expect(k8sClient.Update(ctx, pins)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ledgerSecret(store))).To(Succeed())
			_, err := newLedger(keys).Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store,
				Position: HostLedgerPosition{Seq: 9, Head: head(1)}, Origin: &HostLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonTampered, false)

			Expect(k8sClient.Get(ctx, key, pins)).To(Succeed())
			doc["ledgerIndex"] = json.RawMessage(`"not an index"`)
			pins.Data[hostStoreStateKey], _ = json.Marshal(doc)
			Expect(k8sClient.Update(ctx, pins)).To(Succeed())
			_, err = newLedger(keys).Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store,
				Position: HostLedgerPosition{Seq: 9, Head: head(1)}, Origin: &HostLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonTampered, false)
		})

		It("verifies under the previous key after a rotation and re-tags on the next write", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e, 100)
			oldID := keys.CurrentKeyID()
			rotatedKeys, err := NewHostLedgerKeys(randomLedgerKey(GinkgoT()), keys.current, rawIdentity)
			Expect(err).NotTo(HaveOccurred())
			rotated := newLedger(rotatedKeys)
			r, err := rotated.Load(ctx, provider, h.Name, store)
			Expect(err).NotTo(HaveOccurred(), "a rotation never fails a tag")
			Expect(r.TagKeyID).To(Equal(oldID))
			r, err = rotated.BeginSession(ctx, provider, h.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 100})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.TagKeyID).To(Equal(rotatedKeys.CurrentKeyID()), "re-tagged with the current key")
			Expect(rotated.HeldReason(provider, h.Name)).To(BeEmpty())

			// A manager that no longer holds the key a tag names refuses it.
			dropped, err := NewHostLedgerKeys(randomLedgerKey(GinkgoT()), nil, rawIdentity)
			Expect(err).NotTo(HaveOccurred())
			_, err = newLedger(dropped).Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonTampered, false)
		})

		It("detects a restored older ledger in this process and after a restart", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			snapshot := ledgerSecret(store).Data[hostLedgerDataKey]
			now = now.Add(time.Minute) // the next write also records its counter in the index
			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2100}}})
			Expect(err).NotTo(HaveOccurred())
			idx := storeState(h).LedgerIndex
			Expect(idx.CounterSeen).To(Equal(ledgerCounter(snapshot) + 1))

			restore := func() {
				sec := ledgerSecret(store)
				sec.Data[hostLedgerDataKey] = snapshot
				Expect(k8sClient.Update(ctx, sec)).To(Succeed())
			}
			restore()
			// A restarted manager (no memory) detects it from the index.
			_, err = newLedger(keys).Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonRegressed, true)
			Expect(storeState(h).RegressedReason).To(Equal(LedgerReasonRegressed))
			_, err = ledger.Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonRegressed, true)
		})

		It("detects a regression or a session change it did not write while running", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			snapshot := ledgerSecret(store).Data[hostLedgerDataKey]
			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2100}}})
			Expect(err).NotTo(HaveOccurred())
			sec := ledgerSecret(store)
			sec.Data[hostLedgerDataKey] = snapshot
			Expect(k8sClient.Update(ctx, sec)).To(Succeed())
			_, err = ledger.Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonRegressed, true)

			h2 := newHost("h2", "serial-two")
			e2 := enrol(h2)
			store2, s2 := random16(), random16()
			attested(ledger, h2, store2, s2, e2)
			// Another writer holding the tag key changes the sessions.
			other := newLedger(keys)
			_, err = other.BeginSession(ctx, provider, h2.Name, store2, random16(), LedgerSessionReattest, HostLedgerPosition{Seq: 2050})
			Expect(err).NotTo(HaveOccurred())
			_, err = ledger.Load(ctx, provider, h2.Name, store2)
			expectHold(err, LedgerReasonTampered, true)
		})
	})

	Context("resourceVersion preconditions", func() {
		It("retries a conflicting write from a fresh read and never overwrites the concurrent write", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e, 100)
			wc, err := client.NewWithWatch(cfg2(), client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			var raced atomic.Bool
			racing := interceptor.NewClient(wc, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if sec, ok := obj.(*corev1.Secret); ok && sec.Name == hostLedgerSecretName(store) && raced.CompareAndSwap(false, true) {
						fresh := &corev1.Secret{}
						Expect(c.Get(ctx, client.ObjectKeyFromObject(sec), fresh)).To(Succeed())
						fresh.Annotations["example.test/concurrent"] = "kept"
						Expect(c.Update(ctx, fresh)).To(Succeed())
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			l := newLedger(keys)
			l.Client = racing
			r, err := l.BeginSession(ctx, provider, h.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 100})
			Expect(err).NotTo(HaveOccurred())
			Expect(raced.Load()).To(BeTrue())
			sec := ledgerSecret(store)
			Expect(sec.Annotations).To(HaveKeyWithValue("example.test/concurrent", "kept"), "the concurrent write survives")
			Expect(sec.ResourceVersion).To(Equal(r.resourceVersion), "the record returned is the object of the manager's own write")

			// A write that read a stale resourceVersion fails; it never
			// overwrites.
			stale := sec.DeepCopy()
			_, err = l.BeginSession(ctx, provider, h.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 100})
			Expect(err).NotTo(HaveOccurred())
			stale.Data[hostLedgerDataKey] = []byte(`{}`)
			Expect(apierrors.IsConflict(k8sClient.Update(ctx, stale))).To(BeTrue())
		})

		It("lets only one of two concurrent initialisations create the ledger", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			a, b := newLedger(keys), newLedger(keys)
			done := make(chan HostLedgerInitResult, 2)
			for _, l := range []*HostLedger{a, b} {
				go func() {
					defer GinkgoRecover()
					res, err := l.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store, Position: HostLedgerPosition{Seq: 7, Head: head(1)},
						Origin: &HostLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
					Expect(err).NotTo(HaveOccurred())
					done <- res
				}()
			}
			r1, r2 := <-done, <-done
			Expect([]HostLedgerInitSource{r1.Source, r2.Source}).To(ContainElement(LedgerInitExisting))
			Expect(r1.Record.Generation).To(Equal(r2.Record.Generation), "both use the one ledger")
		})
	})

	Context("rebind", func() {
		It("moves a ledger to a replacement Host only once every condition holds", func() {
			h1 := newHost("h1", "machine")
			e1 := enrol(h1)
			store := random16()
			orig := firstContact(ledger, h1, store, e1, 500).Record

			h2 := newHost("h2", "machine")
			enrol(h2)
			_, err := ledger.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // the bound Host is live and not revoked

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h1), h1)).To(Succeed())
			h1.Spec.Revoked = true
			Expect(k8sClient.Update(ctx, h1)).To(Succeed())
			_, err = ledger.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // released less than the answer lifetime ago

			now = now.Add(hostAnswerTTL + time.Second)
			answers[h1.Name] = now.Add(-time.Second)
			_, err = ledger.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // the bound Host answered recently
			delete(answers, h1.Name)

			h3 := newHost("h3", "machine")
			_, err = ledger.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // another live Host carries the identity
			Expect(k8sClient.Delete(ctx, h3)).To(Succeed())

			h4 := newHost("h4", "other-machine")
			enrol(h4)
			_, err = ledger.Rebind(ctx, provider, h4.Name, store)
			expectHold(err, LedgerReasonStoreForeign, false)

			// A restarted manager waits the quiet period again.
			restarted := newLedger(keys)
			_, err = restarted.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false)

			r, err := ledger.Rebind(ctx, provider, h2.Name, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HostUID).To(Equal(string(h2.UID)))
			Expect(r.HostKey).To(Equal(hostNS + "/h2"))
			Expect(r.Position).To(Equal(orig.Position), "the position is inherited")
			Expect(r.Generation).To(Equal(orig.Generation), "never a fresh start")
			Expect(r.HostIdentityMAC).To(Equal(orig.HostIdentityMAC))
			Expect(sawEvent(eventReasonLedgerRebound)).To(BeTrue())
			Expect(storeState(h2).LedgerIndex.Names(store)).To(BeTrue(), "the replacement's index names the store")
			counter := r.WriteCounter
			r, err = ledger.Rebind(ctx, provider, h2.Name, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.WriteCounter).To(Equal(counter), "a ledger bound to the Host is unchanged")

			_, err = ledger.BeginSession(ctx, provider, h1.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 500})
			Expect(err).To(HaveOccurred(), "the revoked Host is served no more")
			_, err = ledger.BeginSession(ctx, provider, h4.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 500})
			Expect(err).To(MatchError(ErrHostLedgerNotBound))
		})

		It("moves a ledger whose Host was deleted, after the quiet period", func() {
			h1 := newHost("h1", "machine")
			e1 := enrol(h1)
			store := random16()
			firstContact(ledger, h1, store, e1, 500)
			Expect(k8sClient.Delete(ctx, h1)).To(Succeed())
			h2 := newHost("h2", "machine")
			enrol(h2)
			_, err := ledger.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false)
			now = now.Add(hostAnswerTTL + time.Second)
			r, err := ledger.Rebind(ctx, provider, h2.Name, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HostUID).To(Equal(string(h2.UID)))
		})
	})

	Context("status mirror", func() {
		It("mirrors the ledger with a store id prefix only", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			answersState := &HostAnswers{Trust: trust, Now: func() time.Time { return now }}
			answersState.AttachLedger(ledger)
			ledger.lastAnswer = func(_, hostID string) time.Time { return answers[hostID] }
			attested(ledger, h, store, s, e)
			answersState.hosts = map[string]*hostAnswerState{hostAnswerKey(provider, h.Name): {loaded: true,
				store: HostStoreState{Current: hostidentity.StoreRecord{StoreID: store, EnrolmentID: e, Seq: 2060, Head: head(1)}}}}
			answersState.mirrorLedger(ctx, provider, h)
			Expect(h.Status.AgentLedger).NotTo(BeNil())
			Expect(h.Status.AgentLedger.StoreIDPrefix).To(Equal(hex.EncodeToString(store[:4])))
			Expect(h.Status.AgentLedger.Seq).To(Equal(int64(2050)))
			Expect(h.Status.AgentLedger.AttestationState).To(Equal(infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested))
			Expect(h.Status.AgentLedger.AttestedAt).NotTo(BeNil())
			Expect(h.Status.AgentLastStore.Seq).To(Equal(int64(2060)))
			Expect(k8sClient.Status().Update(ctx, h)).To(Succeed(), "the CRD accepts the mirror")
			raw, err := json.Marshal(h.Status)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw)).NotTo(ContainSubstring(hex.EncodeToString(store[:])), "never the full store id")
			Expect(string(raw)).NotTo(ContainSubstring(hex.EncodeToString(s[:])), "never a session id")
		})
	})
})

// cfg2 is the envtest REST config (cfg is shadowed in the ledger specs).
func cfg2() *rest.Config { return cfg }

// ledgerCounter returns the write counter of a ledger document.
func ledgerCounter(doc []byte) uint64 {
	var j struct {
		WriteCounter uint64 `json:"writeCounter"`
	}
	_ = json.Unmarshal(doc, &j)
	return j.WriteCounter
}
