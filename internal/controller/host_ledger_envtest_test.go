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
// hold and where it is kept, tag tampering, key rotation and the re-tag
// sweep, regression and partial-restore detection (a restored older ledger,
// a deleted ledger, a removed or replayed index), resourceVersion
// conflicts, and each rebind condition.
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

	// initialise starts the ledger of in.StoreID for host name through l
	// from ledger evidence: a host answer signed by a stand-in agent with
	// what in names, verified by the answer verifier, and observed by l as
	// the host's latest ledger evidence.
	var initialise func(l *HostLedger, name string, in testLedgerInit) (HostLedgerInitResult, error)
	var evidenceFor func(l *HostLedger, name string, in testLedgerInit) hostidentity.LedgerEvidence

	newLedger := func(k *HostLedgerKeys) *HostLedger {
		l := &HostLedger{Client: k8sClient, APIReader: k8sClient, Trust: trust, Keys: k, Config: cfg, Recorder: rec,
			Now: func() time.Time { return now }, RetagInterval: time.Millisecond}
		l.lastAnswer = func(_, _, hostUID string) time.Time { return answers[hostUID] }
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
		evidenceFor = func(l *HostLedger, name string, in testLedgerInit) hostidentity.LedgerEvidence {
			GinkgoHelper()
			t, err := trust.HostTrust(ctx, provider, name)
			Expect(err).NotTo(HaveOccurred())
			return signedLedgerEvidence(GinkgoT(), provider, name, t.Enrolment.EnrolmentID, cfg.InstallID, now, in)
		}
		initialise = func(l *HostLedger, name string, in testLedgerInit) (HostLedgerInitResult, error) {
			GinkgoHelper()
			ev := evidenceFor(l, name, in)
			l.Observe(provider, name, ev)
			return l.Initialise(ctx, provider, name, HostLedgerInit{StoreID: in.StoreID, Evidence: ev})
		}
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

	ledgerOf := func(store [16]byte) *HostLedgerRecord {
		GinkgoHelper()
		r, err := decodeHostLedger(ledgerSecret(store).Data[hostLedgerDataKey])
		Expect(err).NotTo(HaveOccurred())
		return r
	}

	storeState := func(h *infravirtrigaudiov1beta1.Host) HostStoreState {
		GinkgoHelper()
		st, err := trust.LoadStoreState(ctx, provider, h.Name)
		Expect(err).NotTo(HaveOccurred())
		return st
	}

	pinsKey := func(h *infravirtrigaudiov1beta1.Host) types.NamespacedName {
		return types.NamespacedName{Namespace: enrolNS, Name: hostEnrolmentSecretName(hostNS, h.Name)}
	}

	// editStore rewrites the pins Secret's store.json document directly, as
	// a principal that can write Secrets there would.
	editStore := func(h *infravirtrigaudiov1beta1.Host, edit func(map[string]json.RawMessage)) {
		GinkgoHelper()
		pins := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, pinsKey(h), pins)).To(Succeed())
		var doc map[string]json.RawMessage
		Expect(json.Unmarshal(pins.Data[hostStoreStateKey], &doc)).To(Succeed())
		edit(doc)
		pins.Data[hostStoreStateKey], _ = json.Marshal(doc)
		Expect(k8sClient.Update(ctx, pins)).To(Succeed())
	}

	// firstContact initialises a fresh store (chain position 3) from first
	// contact under enrolment.
	firstContact := func(l *HostLedger, h *infravirtrigaudiov1beta1.Host, store [16]byte, enrolment []byte) HostLedgerInitResult {
		GinkgoHelper()
		res, err := initialise(l, h.Name, testLedgerInit{Attestation: neverAttested, StoreID: store, Position: HostLedgerPosition{Seq: 3, Head: head(1)},
			Origin: &testLedgerOrigin{OriginEnrolmentID: enrolment, EnrolmentCount: 1}})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	// attested drives a first-contact ledger to Attested in session s and
	// raises it to position 2050.
	attested := func(l *HostLedger, h *infravirtrigaudiov1beta1.Host, store, s [16]byte, enrolment []byte) {
		GinkgoHelper()
		firstContact(l, h, store, enrolment)
		_, err := l.BeginSession(ctx, provider, h.Name, store, s, LedgerSessionAttest, HostLedgerPosition{Seq: 3, Head: head(1)})
		Expect(err).NotTo(HaveOccurred())
		_, err = l.Adopt(ctx, provider, h.Name, store, s, LedgerEvidenceChallengeAnswer)
		Expect(err).NotTo(HaveOccurred())
		_, err = l.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2050, Head: head(1)}}})
		Expect(err).NotTo(HaveOccurred())
	}

	Context("initialise", func() {
		It("refuses evidence of another store or of an earlier request, and writes nothing", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, other := random16(), random16()
			in := testLedgerInit{StoreID: store, Position: HostLedgerPosition{Seq: 3, Head: head(1)}, Attestation: neverAttested,
				Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}}
			otherStore := in
			otherStore.StoreID = other
			ev := evidenceFor(ledger, h.Name, otherStore)
			ledger.Observe(provider, h.Name, ev)
			_, err := ledger.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store, Evidence: ev})
			Expect(err).To(MatchError(errHostLedgerEvidence), "evidence of another store")

			older := evidenceFor(ledger, h.Name, in)
			ledger.Observe(provider, h.Name, older)
			ledger.Observe(provider, h.Name, evidenceFor(ledger, h.Name, in))
			_, err = ledger.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store, Evidence: older})
			Expect(err).To(MatchError(errHostLedgerEvidence), "evidence of an earlier request")

			_, err = ledger.Initialise(ctx, provider, h.Name, HostLedgerInit{StoreID: store})
			Expect(err).To(MatchError(errHostLedgerEvidence), "no evidence")
			sec := &corev1.Secret{}
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: enrolNS, Name: hostLedgerSecretName(store)}, sec))).
				To(BeTrue(), "nothing was written")
			res, err := initialise(ledger, h.Name, in)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Source).To(Equal(LedgerInitFirstContact))
		})

		It("starts from the last verified position only once the host confirms it", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			last := hostidentity.StoreRecord{StoreID: store, EnrolmentID: e, Seq: 3000, Head: head(3), CheckpointSeq: 2048, CheckpointHead: head(2)}
			Expect(trust.UpdateStoreState(ctx, provider, h.Name, func(st *HostStoreState) { st.Current = last })).To(Succeed())

			in := testLedgerInit{StoreID: store, Position: HostLedgerPosition{Seq: 3001, Head: head(9)}, Attestation: neverAttested}
			_, err := initialise(ledger, h.Name, in)
			expectHold(err, LedgerReasonUnconfirmed, false)
			Expect(sawEvent(LedgerReasonUnconfirmed)).To(BeTrue())
			in.Continuity = &testLedgerContinuity{Seq: 3000, Head: head(4)}
			_, err = initialise(ledger, h.Name, in)
			expectHold(err, LedgerReasonUnconfirmed, false)
			in.Continuity = &testLedgerContinuity{Seq: 2048, Head: head(2)}
			_, err = initialise(ledger, h.Name, in)
			expectHold(err, LedgerReasonUnconfirmed, false)
			in.Origin = &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}
			_, err = initialise(ledger, h.Name, in)
			expectHold(err, LedgerReasonUnconfirmed, false)
			in.Attestation = hostidentity.StoreAttestation{}

			in.Continuity = &testLedgerContinuity{Seq: 3000, Head: head(3)}
			_, err = initialise(ledger, h.Name, in)
			expectHold(err, LedgerReasonMissing, false) // confirmed, but the agent does not report its record
			in.Attestation = neverAttested              // an upgraded store that was never attested
			res, err := initialise(ledger, h.Name, in)
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

			st := storeState(h)
			Expect(st.LedgerIndex.Names(store)).To(BeTrue(), "the index names the store")
			Expect(st.LedgerIndex.verifyTag(keys, string(h.UID))).To(BeTrue())
			e1, _ := st.LedgerIndex.entry(store)
			Expect(e1.Counter).To(Equal(uint64(1)))
			Expect(st.LedgerIndex.MaxGeneration).To(Equal(r.Generation), "the index keeps the generation high-water mark")
			Expect(st.MinManagerVersion).To(Equal(hostLedgerMinManagerVersion), "store.json carries the format's floor")

			again, err := initialise(ledger, h.Name, in)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.Source).To(Equal(LedgerInitExisting))
			Expect(again.Record.WriteCounter).To(Equal(uint64(1)), "an existing ledger is not written")
		})

		It("starts from first contact only for a fresh store created under the current enrolment alone", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			for _, c := range []struct {
				o   *testLedgerOrigin
				seq uint64
			}{
				{nil, 3},
				{&testLedgerOrigin{OriginEnrolmentID: bytes.Repeat([]byte{9}, 16), EnrolmentCount: 1}, 3},
				{&testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 2}, 3},
				{&testLedgerOrigin{}, 3},
				{&testLedgerOrigin{EnrolmentCount: 1}, 3},
			} {
				_, err := initialise(ledger, h.Name, testLedgerInit{Attestation: neverAttested, StoreID: store,
					Position: HostLedgerPosition{Seq: c.seq, Head: head(5)}, Origin: c.o})
				expectHold(err, LedgerReasonMissing, false)
			}
			// An agent that does not report its record: held, recomputed
			// on every call (an upgraded agent then reports it).
			_, err := initialise(ledger, h.Name, testLedgerInit{StoreID: store,
				Position: HostLedgerPosition{Seq: 3, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonMissing, false)
			// A fresh store whose chain grew through several restarts (clock
			// floor commits) before its first attestation: first contact.
			res, err := initialise(ledger, h.Name, testLedgerInit{Attestation: neverAttested, StoreID: store,
				Position: HostLedgerPosition{Seq: 9, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Source).To(Equal(LedgerInitFirstContact))
			Expect(res.Record.Position).To(Equal(HostLedgerPosition{Seq: 9, Head: head(1)}))
			Expect(ledger.HeldReason(provider, h.Name)).To(BeEmpty(), "a success clears a transient hold")
		})

		It("never restarts a lost ledger", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e)
			Expect(k8sClient.Delete(ctx, ledgerSecret(store))).To(Succeed())
			// A fresh manager (no memory) refuses: the tagged index names it.
			_, err := initialise(newLedger(keys), h.Name, testLedgerInit{Attestation: neverAttested, StoreID: store,
				Position: HostLedgerPosition{Seq: 3, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonMissing, false)
			// The manager that held it sees a regression, persistently.
			_, err = ledger.Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonRegressed, true)
			st := storeState(h)
			Expect(st.RegressedReason).To(Equal(LedgerReasonRegressed))
			Expect(st.LedgerIndex.HeldReason).To(Equal(LedgerReasonRegressed), "the hold is kept in the tagged index too")
			Expect(st.LedgerIndex.HeldEnrolment).To(Equal(e))
		})

		It("refuses a store.json whose index was removed once it is known the Host had one", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e)
			editStore(h, func(doc map[string]json.RawMessage) { delete(doc, "ledgerIndex") })
			// A restarted manager: the ledger serves this Host, so the index
			// must exist.
			fresh := newLedger(keys)
			_, err := fresh.Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonTampered, false)
			e2 := enrol(h)
			_, err = initialise(fresh, h.Name, testLedgerInit{Attestation: neverAttested, StoreID: random16(),
				Position: HostLedgerPosition{Seq: 3, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e2, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonTampered, false) // nothing is started for a Host whose index is gone
			// The running manager saw the index: a persistent hold.
			_, err = ledger.Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonTampered, true)
		})

		It("does not restart the ledger of an attested store after a replayed older index", func() {
			h := newHost("h", "serial")
			e1 := enrol(h)
			a := random16()
			firstContact(ledger, h, a, e1)
			pins := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, pinsKey(h), pins)).To(Succeed())
			oldStore := bytes.Clone(pins.Data[hostStoreStateKey])
			e2 := enrol(h)
			b, s := random16(), random16()
			attested(ledger, h, b, s, e2) // store b runs operations: its chain is at 2050
			// A Secret writer replays the older index and deletes b's ledger.
			Expect(k8sClient.Get(ctx, pinsKey(h), pins)).To(Succeed())
			pins.Data[hostStoreStateKey] = oldStore
			Expect(k8sClient.Update(ctx, pins)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ledgerSecret(b))).To(Succeed())
			// A restarted manager: the agent signs that store b was attested.
			_, err := initialise(newLedger(keys), h.Name, testLedgerInit{Attestation: wasAttested, StoreID: b,
				Position: HostLedgerPosition{Seq: 2050, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e2, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonMissing, true)
			// The running manager, which held b, keeps the Host held.
			_, err = ledger.Load(ctx, provider, h.Name, b)
			expectHold(err, LedgerReasonMissing, true)
		})

		It("never restarts the ledger of an attested store whose object and index were deleted, after a restart", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			Expect(k8sClient.Delete(ctx, ledgerSecret(store))).To(Succeed())
			editStore(h, func(doc map[string]json.RawMessage) { delete(doc, "ledgerIndex") })
			fresh := newLedger(keys) // no memory of the index or the object
			_, err := initialise(fresh, h.Name, testLedgerInit{Attestation: wasAttested, StoreID: store,
				Position: HostLedgerPosition{Seq: 2050, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonMissing, true)
			Expect(storeState(h).RegressedReason).To(Equal(LedgerReasonMissing))
			// The agent's signed record keeps refusing it, whatever the
			// Secrets say.
			editStore(h, func(doc map[string]json.RawMessage) {
				delete(doc, "ledgerIndex")
				delete(doc, "regressedReason")
				delete(doc, "regressedEnrolmentID")
			})
			_, err = initialise(newLedger(keys), h.Name, testLedgerInit{Attestation: wasAttested, StoreID: store,
				Position: HostLedgerPosition{Seq: 2050, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonMissing, true)
		})

		It("holds a ledger behind the store's own first attestation", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			ahead := hostidentity.StoreAttestation{Reported: true, EverAttested: true, FirstSeq: 4000, RecordSeq: 4001}
			_, err := initialise(newLedger(keys), h.Name, testLedgerInit{Attestation: ahead, StoreID: store,
				Position: HostLedgerPosition{Seq: 4100, Head: head(1)}})
			expectHold(err, LedgerReasonRegressed, true)
			res, err := initialise(newLedger(keys), h.Name, testLedgerInit{Attestation: wasAttested, StoreID: store,
				Position: HostLedgerPosition{Seq: 2050, Head: head(1)}})
			Expect(err).To(MatchError(ErrHostLedgerHeld), "the hold is persistent")
			Expect(res.Record).To(BeNil())
		})

		It("holds a second store under one enrolment persistently, in the tagged objects too", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			first := random16()
			firstContact(ledger, h, first, e)
			other := random16()
			_, err := initialise(ledger, h.Name, testLedgerInit{Attestation: neverAttested, StoreID: other, Position: HostLedgerPosition{Seq: 1, Head: head(1)},
				Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonStoreMismatch, true)
			st := storeState(h)
			Expect(st.RegressedReason).To(Equal(LedgerReasonStoreMismatch))
			Expect(st.RegressedEnrolmentID).To(Equal(e))
			Expect(ledgerOf(first).HeldReason).To(Equal(LedgerReasonStoreMismatch), "the hold is in the tagged ledger")
			// Clearing the pins Secret's hold does not lift it.
			editStore(h, func(doc map[string]json.RawMessage) {
				delete(doc, "regressedReason")
				delete(doc, "regressedEnrolmentID")
			})
			_, err = initialise(newLedger(keys), h.Name, testLedgerInit{Attestation: neverAttested, StoreID: other,
				Position: HostLedgerPosition{Seq: 1, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonStoreMismatch, true)
			// A re-enrolment with a fresh store ends it.
			e2 := enrol(h)
			res := firstContact(newLedger(keys), h, other, e2)
			Expect(res.Archived).To(HaveLen(1))
		})

		It("archives the earlier store's ledger when a new enrolment brings a new store", func() {
			h := newHost("h", "serial")
			e1 := enrol(h)
			oldStore := random16()
			old := firstContact(ledger, h, oldStore, e1).Record
			e2 := enrol(h)
			newStore := random16()
			_, err := initialise(ledger, h.Name, testLedgerInit{Attestation: neverAttested, StoreID: newStore, Position: HostLedgerPosition{Seq: 1, Head: head(1)},
				Origin: &testLedgerOrigin{OriginEnrolmentID: e2, EnrolmentCount: 2}})
			expectHold(err, LedgerReasonMissing, false)
			Expect(ledgerOf(oldStore).Archived).To(BeFalse(), "nothing archived without evidence")
			res := firstContact(ledger, h, newStore, e2)
			Expect(res.Archived).To(HaveLen(1))
			a := res.Archived[0]
			Expect(a.Archived).To(BeTrue())
			Expect(a.SuccessorStoreID).To(Equal(newStore))
			Expect(a.Position).To(Equal(old.Position), "an archive keeps the position")
			Expect(res.Record.Generation).To(BeNumerically(">", old.Generation))
			Expect(sawEvent(eventReasonLedgerArchived)).To(BeTrue())
			_, err = ledger.BeginSession(ctx, provider, h.Name, oldStore, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 5000})
			Expect(err).To(MatchError(ErrHostLedgerArchived))
			st := storeState(h)
			Expect(st.LedgerIndex.Stores).To(HaveLen(2))
			Expect(st.LedgerIndex.Stores[0].ID).To(Equal(oldStore))
			Expect(st.LedgerIndex.Stores[1].ID).To(Equal(newStore))
			eo, _ := st.LedgerIndex.entry(oldStore)
			Expect(eo.Counter).To(Equal(a.WriteCounter), "each store keeps its own counter")
			Expect(st.LedgerIndex.MaxGeneration).To(Equal(res.Record.Generation))
		})

		It("refuses a ledger written with a downgrade floor above this manager", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			newer := newLedger(keys)
			newer.MinManagerVersion = "9.0.0"
			newer.ManagerVersion = func() string { return "v9.0.0" }
			_, err := initialise(newer, h.Name, testLedgerInit{Attestation: neverAttested, StoreID: store, Position: HostLedgerPosition{Seq: 3, Head: head(1)},
				Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
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
			firstContact(ledger, h, store, e)
			s1, s2 := random16(), random16()
			indexCounter := func() uint64 {
				x, _ := storeState(h).LedgerIndex.entry(store)
				return x.Counter
			}

			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s1, Position: HostLedgerPosition{Seq: 2100}}})
			Expect(err).To(MatchError(ErrHostLedgerNotAttested))
			_, err = ledger.BeginSession(ctx, provider, h.Name, store, s1, LedgerSessionReattest, HostLedgerPosition{Seq: 3})
			Expect(err).To(MatchError(ErrHostLedgerNotAttested), "a re-attestation needs an adopted session")
			_, err = ledger.BeginSession(ctx, provider, h.Name, store, s1, LedgerSessionAttest, HostLedgerPosition{Seq: 2})
			expectHold(err, LedgerReasonAhead, false)

			r, err := ledger.BeginSession(ctx, provider, h.Name, store, s1, LedgerSessionAttest, HostLedgerPosition{Seq: 3, Head: head(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.AttestationState).To(Equal(infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending))
			Expect(r.HasPendingSession(s1)).To(BeTrue())
			Expect(indexCounter()).To(Equal(r.WriteCounter), "a session-changing write records its counter at once")
			counter := r.WriteCounter
			r, err = ledger.BeginSession(ctx, provider, h.Name, store, s1, LedgerSessionAttest, HostLedgerPosition{Seq: 3})
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
			Expect(indexCounter()).To(Equal(r.WriteCounter))

			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{
				{Session: s1, Position: HostLedgerPosition{Seq: 2100, Head: head(21)}},
				{Session: s1, Position: HostLedgerPosition{Seq: 2080, Head: head(20)}, Checkpoint: HostLedgerPosition{Seq: 2048, Head: head(48)}},
				{Session: s2, Position: HostLedgerPosition{Seq: 9999, Head: head(99)}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Position).To(Equal(HostLedgerPosition{Seq: 2100, Head: head(21)}))
			Expect(r.Checkpoint).To(Equal(HostLedgerPosition{Seq: 2048, Head: head(48)}))
			Expect(indexCounter()).To(Equal(r.WriteCounter), "a receipt raise records its counter at once")
			counter = r.WriteCounter
			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s2, Position: HostLedgerPosition{Seq: 3000}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.WriteCounter).To(Equal(counter), "nothing to raise, nothing written")
			_, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s1, Position: HostLedgerPosition{Seq: 2101},
				Checkpoint: HostLedgerPosition{Seq: 1000}}})
			Expect(err).To(MatchError(errHostLedgerInput))

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
			x, _ := storeState(h).LedgerIndex.entry(store)
			Expect(x.Counter).To(BeNumerically("<", r.WriteCounter), "a heartbeat raise records its counter lazily")
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

			e2 := enrol(h)
			r, err = ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2500}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.GenerationEnrolments).To(Equal([][]byte{e, e2}))
			Expect(ledgerSecret(store).Labels).To(HaveKeyWithValue(LabelHostLedgerEnrolment, hex.EncodeToString(e2)))
		})

		It("holds a fork persistently, in the tagged ledger, and clearing the pins Secret does not lift it", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2050, Head: head(77)}}})
			expectHold(err, LedgerReasonFork, true)
			Expect(storeState(h).RegressedReason).To(Equal(LedgerReasonFork))
			Expect(ledgerOf(store).HeldReason).To(Equal(LedgerReasonFork))
			editStore(h, func(doc map[string]json.RawMessage) {
				delete(doc, "regressedReason")
				delete(doc, "regressedEnrolmentID")
			})
			_, err = newLedger(keys).Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2060, Head: head(1)}}})
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

		It("reports a persistent hold it could not write, and keeps it in memory", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			wc, err := client.NewWithWatch(cfg2(), client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			failing := interceptor.NewClient(wc, interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					return errors.New("injected write failure")
				},
			})
			l := newLedger(keys)
			l.Client = failing
			l.updateStoreState = func(context.Context, *infravirtrigaudiov1beta1.Provider, string, func(*HostStoreState)) error {
				return errors.New("injected write failure")
			}
			_, err = l.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2050, Head: head(77)}}})
			expectHold(err, LedgerReasonFork, true)
			Expect(err).To(MatchError(errHostLedgerHoldNotPersisted))
			Expect(l.HeldReason(provider, h.Name)).To(Equal(LedgerReasonFork))
			_, err = l.Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonFork, true)
			Expect(l.HeldReason(provider, h.Name)).To(Equal(LedgerReasonFork), "a persistent hold is never cleared by a later call")
		})
	})

	Context("tampering, regression and rotation", func() {
		It("holds on a ledger that fails its tag, never on one this manager wrote", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e)
			sec := ledgerSecret(store)
			var doc map[string]any
			Expect(json.Unmarshal(sec.Data[hostLedgerDataKey], &doc)).To(Succeed())
			doc["seq"] = 99999
			sec.Data[hostLedgerDataKey], _ = json.Marshal(doc)
			Expect(k8sClient.Update(ctx, sec)).To(Succeed())
			_, err := newLedger(keys).Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonTampered, false)

			forgedKeys, err := NewHostLedgerKeys(randomLedgerKey(GinkgoT()), nil, rawIdentity)
			Expect(err).NotTo(HaveOccurred())
			forger := newLedger(forgedKeys)
			h2 := newHost("h2", "serial-two")
			e2 := enrol(h2)
			store2 := random16()
			firstContact(forger, h2, store2, e2)
			_, err = ledger.Load(ctx, provider, h2.Name, store2)
			expectHold(err, LedgerReasonTampered, false)
			Expect(sawEvent(LedgerReasonTampered)).To(BeTrue())
			_, err = ledger.BeginSession(ctx, provider, h2.Name, store2, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 5})
			expectHold(err, LedgerReasonTampered, false)

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
			firstContact(ledger, h, store, e)
			editStore(h, func(doc map[string]json.RawMessage) {
				var idx map[string]any
				Expect(json.Unmarshal(doc["ledgerIndex"], &idx)).To(Succeed())
				idx["stores"] = []any{}
				doc["ledgerIndex"], _ = json.Marshal(idx)
			})
			Expect(k8sClient.Delete(ctx, ledgerSecret(store))).To(Succeed())
			_, err := initialise(newLedger(keys), h.Name, testLedgerInit{Attestation: neverAttested, StoreID: store,
				Position: HostLedgerPosition{Seq: 3, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonTampered, false)

			editStore(h, func(doc map[string]json.RawMessage) { doc["ledgerIndex"] = json.RawMessage(`"not an index"`) })
			_, err = initialise(newLedger(keys), h.Name, testLedgerInit{Attestation: neverAttested, StoreID: store,
				Position: HostLedgerPosition{Seq: 3, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonTampered, false)
		})

		It("holds when a store the index names is not found by its labels", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			a := random16()
			firstContact(ledger, h, a, e)
			sec := ledgerSecret(a)
			delete(sec.Labels, LabelHostLedgerIdentityMAC)
			delete(sec.Labels, LabelHostLedgerEnrolment)
			Expect(k8sClient.Update(ctx, sec)).To(Succeed())
			e2 := enrol(h)
			_, err := initialise(newLedger(keys), h.Name, testLedgerInit{Attestation: neverAttested, StoreID: random16(),
				Position: HostLedgerPosition{Seq: 3, Head: head(1)}, Origin: &testLedgerOrigin{OriginEnrolmentID: e2, EnrolmentCount: 1}})
			expectHold(err, LedgerReasonTampered, false)
		})

		It("verifies under the previous key after a rotation, and the re-tag sweep moves everything to the current key", func() {
			h := newHost("h", "serial")
			e1 := enrol(h)
			oldStore := random16()
			firstContact(ledger, h, oldStore, e1)
			e2 := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e2) // archives oldStore
			oldID := keys.CurrentKeyID()
			rotatedKeys, err := NewHostLedgerKeys(randomLedgerKey(GinkgoT()), keys.current, rawIdentity)
			Expect(err).NotTo(HaveOccurred())
			rotated := newLedger(rotatedKeys)
			r, err := rotated.Load(ctx, provider, h.Name, store)
			Expect(err).NotTo(HaveOccurred(), "a rotation never fails a tag")
			Expect(r.TagKeyID).To(Equal(oldID))

			res, err := rotated.RetagSweep(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Ledgers).To(Equal(2), "archived ledgers are re-tagged too")
			Expect(res.Indexes).To(BeNumerically("<=", 1), "an index is re-tagged by the ledger pass that records its counter, or by its own")
			Expect(res.Remaining).To(BeZero())
			Expect(ledgerOf(oldStore).TagKeyID).To(Equal(rotatedKeys.CurrentKeyID()))
			Expect(ledgerOf(store).TagKeyID).To(Equal(rotatedKeys.CurrentKeyID()))
			Expect(storeState(h).LedgerIndex.TagKeyID).To(Equal(rotatedKeys.CurrentKeyID()))
			again, err := rotated.RetagSweep(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(again).To(Equal(HostLedgerRetagResult{}), "a second sweep finds nothing to do")

			// The previous key can go now.
			alone, err := NewHostLedgerKeys(rotatedKeys.current, nil, rawIdentity)
			Expect(err).NotTo(HaveOccurred())
			_, err = newLedger(alone).Load(ctx, provider, h.Name, store)
			Expect(err).NotTo(HaveOccurred())

			// A manager that no longer holds the key a tag names refuses it.
			dropped, err := NewHostLedgerKeys(randomLedgerKey(GinkgoT()), nil, rawIdentity)
			Expect(err).NotTo(HaveOccurred())
			_, err = newLedger(dropped).Load(ctx, provider, h.Name, store)
			expectHold(err, LedgerReasonTampered, false)
		})

		It("re-tags nothing that a read would refuse: a restored older copy under the previous key holds its Host", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			snapshot := ledgerSecret(store).Data[hostLedgerDataKey] // under the old key, counter N-1
			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2100}}})
			Expect(err).NotTo(HaveOccurred())
			x, _ := storeState(h).LedgerIndex.entry(store)
			Expect(x.Counter).To(Equal(ledgerCounter(snapshot) + 1)) // the index says N
			sec := ledgerSecret(store)
			sec.Data[hostLedgerDataKey] = snapshot
			Expect(k8sClient.Update(ctx, sec)).To(Succeed())

			rotatedKeys, err := NewHostLedgerKeys(randomLedgerKey(GinkgoT()), keys.current, rawIdentity)
			Expect(err).NotTo(HaveOccurred())
			res, err := newLedger(rotatedKeys).RetagSweep(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Ledgers).To(BeZero())
			Expect(res.Remaining).To(BeNumerically(">=", 1), "the sweep refuses the restored copy")
			Expect(ledgerOf(store).TagKeyID).To(Equal(keys.CurrentKeyID()), "not laundered under the current key")
			Expect(ledgerCounter(ledgerSecret(store).Data[hostLedgerDataKey])).NotTo(BeNumerically(">=", x.Counter),
				"its counter is not raised past the index")
			Expect(storeState(h).RegressedReason).To(Equal(LedgerReasonRegressed), "and the Host is held, persistently")
		})

		It("re-tags a ledger whose Host is gone only when its last index does not refuse it", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			snapshot := ledgerSecret(store).Data[hostLedgerDataKey]
			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2100}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), h)).To(Succeed())
			h.Spec.Revoked = true
			Expect(k8sClient.Update(ctx, h)).To(Succeed())
			sec := ledgerSecret(store)
			sec.Data[hostLedgerDataKey] = snapshot
			Expect(k8sClient.Update(ctx, sec)).To(Succeed())
			rotatedKeys, err := NewHostLedgerKeys(randomLedgerKey(GinkgoT()), keys.current, rawIdentity)
			Expect(err).NotTo(HaveOccurred())
			res, err := newLedger(rotatedKeys).RetagSweep(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Remaining).To(BeNumerically(">=", 1))
			Expect(ledgerOf(store).TagKeyID).To(Equal(keys.CurrentKeyID()))
		})

		It("detects a restored older ledger in this process and after a restart", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			attested(ledger, h, store, s, e)
			snapshot := ledgerSecret(store).Data[hostLedgerDataKey]
			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2100}}})
			Expect(err).NotTo(HaveOccurred())
			x, _ := storeState(h).LedgerIndex.entry(store)
			Expect(x.Counter).To(Equal(ledgerCounter(snapshot) + 1))

			sec := ledgerSecret(store)
			sec.Data[hostLedgerDataKey] = snapshot
			Expect(k8sClient.Update(ctx, sec)).To(Succeed())
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
			now = now.Add(time.Minute)
			_, err := ledger.Raise(ctx, provider, h.Name, store, []HostLedgerRaise{{Session: s, Position: HostLedgerPosition{Seq: 2100}, Heartbeat: true}})
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
			firstContact(ledger, h, store, e)
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
			r, err := l.BeginSession(ctx, provider, h.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 3})
			Expect(err).NotTo(HaveOccurred())
			Expect(raced.Load()).To(BeTrue())
			sec := ledgerSecret(store)
			Expect(sec.Annotations).To(HaveKeyWithValue("example.test/concurrent", "kept"), "the concurrent write survives")
			Expect(sec.ResourceVersion).To(Equal(r.resourceVersion), "the record returned is the object of the manager's own write")

			stale := sec.DeepCopy()
			_, err = l.BeginSession(ctx, provider, h.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 3})
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
					res, err := initialise(l, h.Name, testLedgerInit{Attestation: neverAttested, StoreID: store, Position: HostLedgerPosition{Seq: 3, Head: head(1)},
						Origin: &testLedgerOrigin{OriginEnrolmentID: e, EnrolmentCount: 1}})
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
			store, s := random16(), random16()
			attested(ledger, h1, store, s, e1)
			orig := ledgerOf(store)

			h2 := newHost("h2", "machine")
			e2 := enrol(h2)
			_, err := ledger.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // the bound Host is live and not revoked

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h1), h1)).To(Succeed())
			h1.Spec.Revoked = true
			Expect(k8sClient.Update(ctx, h1)).To(Succeed())
			_, err = ledger.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // released less than the answer lifetime ago

			now = now.Add(hostAnswerTTL + time.Second)
			answers[string(h1.UID)] = now.Add(-time.Second)
			_, err = ledger.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // the bound Host answered recently
			delete(answers, string(h1.UID))

			h3 := newHost("h3", "MACHINE")
			_, err = ledger.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // another live Host carries the identity (any case)
			Expect(k8sClient.Delete(ctx, h3)).To(Succeed())

			h4 := newHost("h4", "other-machine")
			enrol(h4)
			_, err = ledger.Rebind(ctx, provider, h4.Name, store)
			expectHold(err, LedgerReasonStoreForeign, false)

			restarted := newLedger(keys)
			_, err = restarted.Rebind(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // a restarted manager waits the quiet period again

			r, err := ledger.Rebind(ctx, provider, h2.Name, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HostUID).To(Equal(string(h2.UID)))
			Expect(r.HostKey).To(Equal(hostNS + "/h2"))
			Expect(r.Position).To(Equal(orig.Position), "the position is inherited")
			Expect(r.Generation).To(Equal(orig.Generation), "never a fresh start")
			Expect(r.AttestationState).To(Equal(infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending), "the new Host's agent attests again")
			Expect(sawEvent(eventReasonLedgerRebound)).To(BeTrue())
			Expect(storeState(h2).LedgerIndex.Names(store)).To(BeTrue(), "the replacement's index names the store")
			counter := r.WriteCounter
			r, err = ledger.Rebind(ctx, provider, h2.Name, store)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.WriteCounter).To(Equal(counter), "a ledger bound to the Host is unchanged")

			_, err = ledger.BeginSession(ctx, provider, h1.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 500})
			Expect(err).To(HaveOccurred(), "the revoked Host is served no more")
			_, err = ledger.BeginSession(ctx, provider, h4.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 5000})
			Expect(err).To(MatchError(ErrHostLedgerNotBound))

			// The new Host works its ledger, read by a restarted manager too
			// (its index has its own counter for the store).
			s2 := random16()
			_, err = newLedger(keys).BeginSession(ctx, provider, h2.Name, store, s2, LedgerSessionAttest, HostLedgerPosition{Seq: 2050})
			Expect(err).NotTo(HaveOccurred())
			r, err = newLedger(keys).Adopt(ctx, provider, h2.Name, store, s2, LedgerEvidenceHostAnswer)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.AttestationID).To(Equal(s2))
			_, err = newLedger(keys).Raise(ctx, provider, h2.Name, store, []HostLedgerRaise{{Session: s2, Position: HostLedgerPosition{Seq: 2100}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(ledgerOf(store).GenerationEnrolments).To(ContainElement(e2))
			// The first manager did not write those session changes: it
			// refuses the ledger from now on.
			_, err = ledger.Load(ctx, provider, h2.Name, store)
			expectHold(err, LedgerReasonTampered, true)
		})

		It("moves a ledger whose Host was deleted, after the quiet period", func() {
			h1 := newHost("h1", "machine")
			e1 := enrol(h1)
			store := random16()
			firstContact(ledger, h1, store, e1)
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

		It("moves a ledger to a replacement of the same name whose own answers keep arriving", func() {
			h := newHost("h", "machine")
			e := enrol(h)
			store := random16()
			firstContact(ledger, h, store, e)
			oldUID := string(h.UID)
			Expect(k8sClient.Delete(ctx, h)).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), &infravirtrigaudiov1beta1.Host{}))
			}).Should(BeTrue())
			repl := newHost("h", "machine")
			enrol(repl)
			// The heartbeat records the replacement's answers under the same
			// name, every few seconds.
			ha := &HostAnswers{Trust: trust, Now: func() time.Time { return now }}
			ha.hosts = map[string]*hostAnswerState{hostAnswerKey(provider, "h"): {hostAnswerAt: now, hostAnswerHostUID: string(repl.UID)}}
			ledger.lastAnswer = ha.lastVerifiedAnswer
			_, err := ledger.Rebind(ctx, provider, repl.Name, store)
			expectHold(err, LedgerReasonStoreBound, false) // quiet period
			now = now.Add(hostAnswerTTL + time.Second)
			ha.hosts[hostAnswerKey(provider, "h")].hostAnswerAt = now
			r, err := ledger.Rebind(ctx, provider, repl.Name, store)
			Expect(err).NotTo(HaveOccurred(), "the replacement's answers do not keep the old object fresh")
			Expect(r.HostUID).To(Equal(string(repl.UID)))

			// An answer recorded for the old object does.
			h2 := newHost("h2", "machine-two")
			e2 := enrol(h2)
			store2 := random16()
			firstContact(ledger, h2, store2, e2)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h2), h2)).To(Succeed())
			h2.Spec.Revoked = true
			Expect(k8sClient.Update(ctx, h2)).To(Succeed())
			h2b := newHost("h2b", "machine-two")
			enrol(h2b)
			ha.hosts[hostAnswerKey(provider, "h2")] = &hostAnswerState{hostAnswerAt: now, hostAnswerHostUID: string(h2.UID)}
			_, err = ledger.Rebind(ctx, provider, h2b.Name, store2)
			expectHold(err, LedgerReasonStoreBound, false)
			Expect(oldUID).NotTo(Equal(string(repl.UID)))
		})
	})

	Context("status mirror", func() {
		It("mirrors the ledger with a store id prefix only, and clears the held condition with the hold", func() {
			h := newHost("h", "serial")
			e := enrol(h)
			store, s := random16(), random16()
			answersState := &HostAnswers{Trust: trust, Now: func() time.Time { return now }}
			answersState.AttachLedger(ledger)
			ledger.lastAnswer = func(_, _, hostUID string) time.Time { return answers[hostUID] }
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

			// A transient hold shows, and clears with the hold.
			_, err = ledger.BeginSession(ctx, provider, h.Name, store, random16(), LedgerSessionAttest, HostLedgerPosition{Seq: 1})
			expectHold(err, LedgerReasonAhead, false)
			now = now.Add(hostLedgerMirrorInterval)
			answersState.mirrorLedger(ctx, provider, h)
			Expect(condOfType(h, HostConditionAgentLedgerHeld).Status).To(Equal(metav1.ConditionTrue))
			_, err = ledger.Load(ctx, provider, h.Name, store)
			Expect(err).NotTo(HaveOccurred())
			now = now.Add(hostLedgerMirrorInterval)
			answersState.mirrorLedger(ctx, provider, h)
			Expect(condOfType(h, HostConditionAgentLedgerHeld).Status).To(Equal(metav1.ConditionFalse))
		})
	})
})

// condOfType returns a Host condition (nil when absent).
func condOfType(h *infravirtrigaudiov1beta1.Host, typ string) *metav1.Condition {
	for i := range h.Status.Conditions {
		if h.Status.Conditions[i].Type == typ {
			return &h.Status.Conditions[i]
		}
	}
	return nil
}

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

// The store attestation records the agent signs, as decoded from a verified
// answer: a store never attested, and one first attested at position 3.
var (
	neverAttested = hostidentity.StoreAttestation{Reported: true}
	wasAttested   = hostidentity.StoreAttestation{Reported: true, EverAttested: true, FirstSeq: 3, RecordSeq: 4}
)
