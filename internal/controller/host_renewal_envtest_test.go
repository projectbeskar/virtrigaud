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
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// renewalManager is the manager identity the renewal specs run as.
const renewalManager = "system:serviceaccount:virtrigaud-system:virtrigaud-manager"

// renewingHost is a per-host agent stand-in for the renewal specs: it signs
// its answers and its co-signed renewal request with the agent's own answer
// package (answer.Sign, answer.RenewalRequest, answer.SignRenewalRequest),
// stages a renewed certificate and puts it in use, as the agent does.
type renewingHost struct {
	mu          sync.Mutex
	key         *ecdsa.PrivateKey
	cert        []byte
	newKey      *ecdsa.PrivateKey
	csr         []byte
	staged      []byte
	rec         hostidentity.Record
	hostID      string
	storeID     [16]byte
	seq         uint64
	apiVersion  uint32
	forgeSigner *ecdsa.PrivateKey // co-sign the renewal with this key instead
	forgeURIs   []string          // request these URIs instead
}

// answer is the host's signed answer to a GetHostInfo.
func (h *renewingHost) answer(q contracts.HostAnswerQuery, nonce []byte) *contracts.SignedAnswer {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	pos := append(append(append([]byte(nil), h.storeID[:]...), binary.BigEndian.AppendUint64(nil, h.seq)...), bytes.Repeat([]byte{byte(h.seq)}, 32)...)
	ha := &providerv1.HostAnswer{
		Context: &providerv1.AnswerContext{Nonce: nonce, ProviderUid: h.rec.ProviderUID, HostId: h.hostID, InstallId: h.rec.InstallID,
			EnrolmentId: h.rec.EnrolmentID, ChainPosition: pos, AgentState: providerv1.AgentState_AGENT_STATE_READY, AgentTime: timestamppb.Now()},
		AgentApiVersion: h.apiVersion, GrantVersions: []uint32{1}, Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY,
	}
	if q.ContinuitySeq > 0 && q.ContinuitySeq <= h.seq {
		ha.Context.ContinuityHead = bytes.Repeat([]byte{byte(q.ContinuitySeq)}, 32)
	}
	if len(h.staged) > 0 {
		sum := sha256.Sum256(h.staged)
		ha.StagedCertificateSha256 = sum[:]
	}
	if q.RenewalVersion > 0 && h.csr != nil {
		body, err := answer.RenewalRequest(answer.Renewal{Requested: q.RenewalVersion, HostID: h.hostID, Record: h.rec,
			CertificateDER: h.cert, CSRDER: h.csr})
		Expect(err).NotTo(HaveOccurred())
		signer := h.key
		if h.forgeSigner != nil {
			signer = h.forgeSigner
		}
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
		Expect(err).NotTo(HaveOccurred())
		ha.PendingRenewal = signRenewal(signer, h.cert, raw)
	}
	blob, err := answer.Sign(h.key, h.cert, ha)
	Expect(err).NotTo(HaveOccurred())
	return &contracts.SignedAnswer{BodyType: blob.GetBodyType(), Body: blob.GetBody(), Signature: blob.GetSignature(),
		SignerCertificate: blob.GetSignerCertificate()}
}

// signRenewal co-signs raw as a host would: with the agent's own signing
// when key is the certificate's, and raw (a forger's key) otherwise.
func signRenewal(key *ecdsa.PrivateKey, cert, raw []byte) *providerv1.SignedBlob {
	if key.PublicKey.Equal(mustCertKey(cert)) {
		body := &providerv1.RenewalRequest{}
		Expect(proto.Unmarshal(raw, body)).To(Succeed())
		blob, err := answer.SignRenewalRequest(key, cert, body)
		Expect(err).NotTo(HaveOccurred())
		return blob
	}
	m, err := hostidentity.AnswerSignatureInput(hostidentity.BodyTypeRenewalRequest, raw)
	Expect(err).NotTo(HaveOccurred())
	d := sha256.Sum256(m)
	sig, err := ecdsa.SignASN1(rand.Reader, key, d[:])
	Expect(err).NotTo(HaveOccurred())
	return &providerv1.SignedBlob{Body: raw, Signature: sig, SignerCertificate: cert, BodyType: hostidentity.BodyTypeRenewalRequest}
}

// mustCertKey returns a DER certificate's key.
func mustCertKey(der []byte) *ecdsa.PublicKey {
	c, err := x509.ParseCertificate(der)
	Expect(err).NotTo(HaveOccurred())
	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	Expect(ok).To(BeTrue())
	return pub
}

// startRenewal makes the host's pending renewal: a new key and its request
// for exactly its current URIs (or forgeURIs).
func (h *renewingHost) startRenewal(hostURI, hostName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.newKey = hostidentitytest.NewKey(GinkgoT())
	uris := hostidentity.RequestURIs(hostURI, h.rec)
	if h.forgeURIs != nil {
		uris = h.forgeURIs
	}
	block, _ := pem.Decode(hostidentitytest.NewCSR(GinkgoT(), h.newKey, hostidentitytest.CSROptions{CommonName: hostName, URIs: uris}))
	h.csr = block.Bytes
}

// stage stages a renewed certificate (DER).
func (h *renewingHost) stage(der []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.staged = bytes.Clone(der)
}

// activate puts the staged certificate and the new key in use.
func (h *renewingHost) activate() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.key, h.cert, h.staged, h.csr, h.newKey = h.newKey, h.staged, nil, nil, nil
}

// renewalReader is the Provider client the heartbeat reads through: it
// relays the host's signed answer, or (stale) an answer for another nonce.
type renewalReader struct {
	contracts.HostAnswerReader
	host  *renewingHost
	stale bool
	asked []uint32
}

func (r *renewalReader) HostAnswer(_ context.Context, q contracts.HostAnswerQuery) (contracts.HostInfo, contracts.SignedRead, error) {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	r.asked = append(r.asked, q.RenewalVersion)
	answerNonce := nonce
	if r.stale {
		answerNonce = bytes.Repeat([]byte{0x99}, 16)
	}
	return contracts.HostInfo{ID: q.HostID}, contracts.SignedRead{Nonce: nonce, Answer: r.host.answer(q, answerNonce)}, nil
}

var _ = Describe("Per-host agent certificate renewal (envtest)", func() {
	ctx := context.Background()

	type renewalEnv struct {
		hostNS, issuerNS string
		provider         *infravirtrigaudiov1beta1.Provider
		ca               *hostidentitytest.CA
		cfg              *HostEnrolmentConfig
		hostR            *HostEnrolmentReconciler
		reqR             *HostEnrolmentRequestReconciler
		answers          *HostAnswers
		reader           *renewalReader
		host             *renewingHost
		hostURI          string
		oldPin           string
		rec              *record.FakeRecorder
		now              time.Time
	}

	BeforeEach(func() {
		if !devgate.Compiled {
			Skip("per-host agent renewal is served only by development builds")
		}
	})

	// newRenewalEnvLiving builds the environment with a current certificate
	// that lives life.
	newRenewalEnvLiving := func(life time.Duration) *renewalEnv {
		e := &renewalEnv{now: time.Now(), rec: record.NewFakeRecorder(64)}
		for _, p := range []string{"renew-hosts-", "renew-issuer-"} {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: p}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			if p == "renew-hosts-" {
				e.hostNS = ns.Name
			} else {
				e.issuerNS = ns.Name
			}
		}
		prov := envtestProvider(e.hostNS, "ch", nil)
		prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
		prov.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		e.provider = prov
		e.ca = hostidentitytest.NewCA(GinkgoT())
		cm := enrolTestCMIssuer
		e.cfg = &HostEnrolmentConfig{
			Namespace: e.issuerNS, CertManagerIssuer: &cm, AdminGroup: enrolTestAdminGroup, TrustDomain: enrolTestTrustDomain,
			CABundle: e.ca.PEM, InstallID: bytes.Repeat([]byte{0x42}, 16),
		}
		e.cfg.SetManagerIdentity(ManagerIdentity{Username: renewalManager, Groups: []string{"system:serviceaccounts", "system:authenticated"}, UID: "mgr-uid"})
		clock := func() time.Time { return e.now }
		e.hostR = &HostEnrolmentReconciler{Client: k8sClient, APIReader: k8sClient, Config: e.cfg, Recorder: e.rec, Now: clock}
		e.reqR = &HostEnrolmentRequestReconciler{Client: k8sClient, Config: e.cfg, Recorder: e.rec, Now: clock}
		e.answers = NewHostAnswers(k8sClient, k8sClient, e.cfg, e.cfg.InstallID)
		e.answers.Now = clock

		// An enrolled Host at start epoch 3: its pins Secret holds the current
		// enrolment, and the host holds the matching key and certificate.
		h := &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: "host-a", Namespace: e.hostNS},
			Spec: infravirtrigaudiov1beta1.HostSpec{ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "ch"},
				PoolRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"}, Endpoint: "grpc://host-a:7443",
				Identity: "serial-a", Schedulable: true},
		}
		Expect(k8sClient.Create(ctx, h)).To(Succeed())
		uri, err := hostidentity.HostURI(enrolTestTrustDomain, e.hostNS, "host-a")
		Expect(err).NotTo(HaveOccurred())
		e.hostURI = uri
		rec := hostidentity.Record{EnrolmentID: bytes.Repeat([]byte{0x5a}, 16), ProviderUID: string(prov.UID), InstallID: e.cfg.InstallID,
			Mode: hostidentity.ModeIdentity, StartEpoch: 3}
		key := hostidentitytest.NewKey(GinkgoT())
		csr := hostidentitytest.NewCSR(GinkgoT(), key, hostidentitytest.CSROptions{CommonName: "host-a", URIs: hostidentity.RequestURIs(uri, rec)})
		// The current certificate lives 20 days, the renewed ones 30, so the
		// renewed expiry is distinguishable.
		cb, _ := pem.Decode(e.ca.Issue(GinkgoT(), csr, hostidentitytest.IssueOptions{Lifetime: life}))
		leaf, err := x509.ParseCertificate(cb.Bytes)
		Expect(err).NotTo(HaveOccurred())
		e.oldPin, err = hostidentity.SPKIPin(&key.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		l, err := loadHostEnrolment(ctx, k8sClient, e.cfg, h)
		Expect(err).NotTo(HaveOccurred())
		l.state.Current = &currentEnrolment{Record: enrolmentRecord{EnrolmentID: hex.EncodeToString(rec.EnrolmentID),
			ProviderUID: rec.ProviderUID, InstallID: hex.EncodeToString(rec.InstallID), StartEpoch: 3},
			KeyPins: []string{e.oldPin}, NotAfter: metav1.NewTime(leaf.NotAfter.UTC().Truncate(time.Second))}
		l.state.LastIssuedEpoch = 3
		Expect(saveHostEnrolment(ctx, k8sClient, e.cfg, h, &l)).To(Succeed())
		e.host = &renewingHost{key: key, cert: cb.Bytes, rec: rec, hostID: "host-a", seq: 1, apiVersion: answer.AgentAPIVersion}
		_, _ = rand.Read(e.host.storeID[:])
		e.reader = &renewalReader{host: e.host}
		return e
	}
	newRenewalEnv := func() *renewalEnv { return newRenewalEnvLiving(20 * 24 * time.Hour) }

	state := func(e *renewalEnv) hostEnrolmentState {
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.issuerNS, Name: hostEnrolmentSecretName(e.hostNS, "host-a")}, sec)).To(Succeed())
		var st hostEnrolmentState
		Expect(json.Unmarshal(sec.Data[hostEnrolmentStateKey], &st)).To(Succeed())
		return st
	}

	hostObj := func(e *renewalEnv) *infravirtrigaudiov1beta1.Host {
		h := &infravirtrigaudiov1beta1.Host{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.hostNS, Name: "host-a"}, h)).To(Succeed())
		return h
	}

	// heartbeat is the Host controller's signed read of the host and the
	// renewal step on its verified answer. It returns the verdict.
	heartbeat := func(e *renewalEnv) answerVerdict {
		r, err := e.answers.refreshHost(ctx, e.provider, e.reader, "host-a")
		Expect(err).NotTo(HaveOccurred())
		if r.verdict == answerVerified {
			for _, ev := range e.answers.checkRenewal(ctx, e.provider, "host-a", r.answer) {
				e.rec.Event(hostObj(e), ev.typ, ev.reason, ev.message)
			}
		}
		return r.verdict
	}

	reconcileHost := func(e *renewalEnv) *infravirtrigaudiov1beta1.Host {
		_, err := e.hostR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.hostNS, Name: "host-a"}})
		Expect(err).NotTo(HaveOccurred())
		return hostObj(e)
	}

	getCR := func(e *renewalEnv, name string) *unstructured.Unstructured {
		cr := certrequest.NewObject()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.issuerNS, Name: name}, cr)).To(Succeed())
		return cr
	}

	reconcileCR := func(e *renewalEnv, cr *unstructured.Unstructured) *unstructured.Unstructured {
		_, err := e.reqR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cr.GetNamespace(), Name: cr.GetName()}})
		Expect(err).NotTo(HaveOccurred())
		return getCR(e, cr.GetName())
	}

	issue := func(e *renewalEnv, cr *unstructured.Unstructured, o hostidentitytest.IssueOptions) []byte {
		csr, err := certrequest.CSR(cr)
		Expect(err).NotTo(HaveOccurred())
		certPEM := e.ca.Issue(GinkgoT(), csr, o)
		Expect(certrequest.SetCertificate(cr, certPEM, e.ca.PEM)).To(Succeed())
		Expect(certrequest.SetCondition(cr, certrequest.ConditionReady, certrequest.StatusTrue, certrequest.ReasonIssued, "issued", time.Now())).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
		b, _ := pem.Decode(certPEM)
		return b.Bytes
	}

	renewalRequests := func(e *renewalEnv) []string {
		list := certrequest.NewList()
		Expect(k8sClient.List(ctx, list, client.InNamespace(e.issuerNS), client.MatchingLabels{certrequest.LabelAgentRenewal: "true"})).To(Succeed())
		var out []string
		for _, i := range list.Items {
			out = append(out, i.GetName())
		}
		return out
	}

	expectDenied := func(cr *unstructured.Unstructured, reason string) {
		s, r, found := certrequest.Condition(cr, certrequest.ConditionDenied)
		Expect(found).To(BeTrue(), "the request must be denied")
		Expect(s).To(Equal(certrequest.StatusTrue))
		Expect(r).To(Equal(reason))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeFalse())
	}

	// requested runs the renewal up to a created CertificateRequest.
	requested := func(e *renewalEnv) *unstructured.Unstructured {
		Expect(heartbeat(e)).To(Equal(answerVerified), "the first heartbeat learns the agent's API version")
		e.host.startRenewal(e.hostURI, "host-a")
		Expect(heartbeat(e)).To(Equal(answerVerified))
		st := state(e)
		Expect(st.Renewal).NotTo(BeNil(), "the co-signed renewal request is recorded")
		reconcileHost(e)
		st = state(e)
		Expect(st.Renewal.CertificateRequest).NotTo(BeEmpty(), "the request's name is recorded first")
		Expect(st.Renewal.CertificateRequestUID).To(BeEmpty(), "and the request is created only after that")
		Expect(hostObj(e).Status.Agent.Renewal.CertificateRequest).To(BeEmpty(), "the name is mirrored only once the request exists")
		list := certrequest.NewList()
		Expect(k8sClient.List(ctx, list, client.InNamespace(e.issuerNS))).To(Succeed())
		Expect(list.Items).To(BeEmpty())
		h := reconcileHost(e)
		st = state(e)
		Expect(st.Renewal.CertificateRequestUID).NotTo(BeEmpty(), "the manager created its request")
		Expect(h.Status.Agent.Renewal).NotTo(BeNil())
		Expect(h.Status.Agent.Renewal.Phase).To(Equal(infravirtrigaudiov1beta1.HostAgentRenewalRequested))
		Expect(h.Status.Agent.Renewal.CertificateRequest).To(Equal(st.Renewal.CertificateRequest))
		_, name, _ := strings.Cut(st.Renewal.CertificateRequest, "/")
		cr := getCR(e, name)
		Expect(cr.GetOwnerReferences()).To(HaveLen(1), "owned by the pins Secret")
		Expect(cr.GetOwnerReferences()[0].Kind).To(Equal("Secret"))
		return cr
	}

	It("rotates the key end to end: request, approval, issuance, stage, pin, activate, unpin", func() {
		e := newRenewalEnv()
		Expect(heartbeat(e)).To(Equal(answerVerified))
		Expect(e.reader.asked).To(Equal([]uint32{0}), "no renewal asked before the agent's API version is known")
		cr := requested(e)
		Expect(e.reader.asked[len(e.reader.asked)-1]).To(Equal(hostidentity.RenewalVersion))

		// The request: the manager's identity, renewal and Host labels, the
		// host's CSR, agent usages and the current lifetime.
		Expect(certrequest.IsRenewal(cr)).To(BeTrue())
		Expect(cr.GetLabels()[certrequest.LabelHostHash]).To(Equal(hostHashLabel(e.hostNS, "host-a")))
		in, err := certrequest.RequestOf(cr)
		Expect(err).NotTo(HaveOccurred())
		Expect(in.Username).To(Equal(renewalManager))
		csr, err := hostidentity.ParseCSR(in.CSRPEM)
		Expect(err).NotTo(HaveOccurred())
		Expect(csr.Raw).To(Equal(e.host.csr))

		cr = reconcileCR(e, cr)
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue(), "auto-approved: all five conditions hold")
		der := issue(e, cr, hostidentitytest.IssueOptions{})
		reconcileCR(e, cr)
		st := state(e)
		Expect(st.Renewal.Certificate).To(Equal(der))
		Expect(st.Current.KeyPins).To(Equal([]string{e.oldPin}), "nothing is pinned at issuance")
		h := reconcileHost(e)
		Expect(h.Status.Agent.Renewal.Phase).To(Equal(infravirtrigaudiov1beta1.HostAgentRenewalIssued))
		Expect(resolveHostRenewal(ctx, k8sClient, e.cfg, h)).To(Equal(der), "published for the Provider")

		// The Provider stages it; the manager's own read sees the digest and
		// pins the new key beside the old one.
		e.host.stage(der)
		Expect(heartbeat(e)).To(Equal(answerVerified))
		st = state(e)
		newPin, err := hostidentity.SPKIPin(&e.host.newKey.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Current.KeyPins).To(Equal([]string{e.oldPin, newPin}))
		Expect(st.Renewal.PinnedAt).NotTo(BeNil())
		h = reconcileHost(e)
		Expect(h.Status.Agent.KeyPins).To(Equal([]string{e.oldPin, newPin}))
		Expect(h.Status.Agent.Renewal.Phase).To(Equal(infravirtrigaudiov1beta1.HostAgentRenewalOverlap))
		Expect(h.Status.Agent.Renewal.OverlapUntil).NotTo(BeNil())
		creds, skip := resolveHostAgentTLS(ctx, k8sClient, e.cfg, h, time.Now())
		Expect(skip).To(BeEmpty())
		Expect(creds.AgentTLS.KeyPins).To(Equal([]string{e.oldPin, newPin}), "both pins rendered during the overlap")

		// The manager restarts between "pin added" and "old pin removed": a
		// fresh HostAnswers reads the persisted state.
		e.answers = NewHostAnswers(k8sClient, k8sClient, e.cfg, e.cfg.InstallID)
		e.answers.Now = func() time.Time { return e.now }
		// The Provider activates; the next verified answer of the manager's
		// own request is signed by the new key and names nothing staged.
		oldKey, oldCert := e.host.key, e.host.cert
		e.host.activate()
		Expect(heartbeat(e)).To(Equal(answerVerified))
		st = state(e)
		Expect(st.Current.KeyPins).To(Equal([]string{newPin}), "the old pin is removed")
		Expect(st.PreviousKeyPins).To(ContainElement(e.oldPin))
		Expect(st.Renewal).To(BeNil())
		Expect(st.Current.Record.EnrolmentID).To(Equal(hex.EncodeToString(e.host.rec.EnrolmentID)), "same enrolment")
		Expect(st.Current.Record.StartEpoch).To(Equal(uint64(3)), "same epoch: a renewal is not a re-key")
		Expect(st.PredecessorEnrolmentIDs).To(BeEmpty(), "no predecessor")
		h = reconcileHost(e)
		Expect(h.Status.Agent.Renewal).To(BeNil())
		Expect(h.Status.Agent.KeyPins).To(Equal([]string{newPin}))
		Expect(resolveHostRenewal(ctx, k8sClient, e.cfg, h)).To(BeNil())
		Expect(renewalRequests(e)).To(HaveLen(1), "the finished renewal request is retained for audit")

		// A manager restart reconciles the finished request again, with no
		// renewal in progress: its certificate is the host's pinned one and
		// is never marked "never install it".
		e.reqR = &HostEnrolmentRequestReconciler{Client: e.reqR.Client, APIReader: e.reqR.APIReader, Config: e.reqR.Config,
			Recorder: e.reqR.Recorder, Now: e.reqR.Now}
		finished := reconcileCR(e, getCR(e, renewalRequests(e)[0]))
		Expect(certrequest.IsTrue(finished, certrequest.ConditionRefused)).To(BeFalse(), "the current certificate's request is not refused")

		// Answers by the old key are no answers now.
		e.host.mu.Lock()
		e.host.key, e.host.cert = oldKey, oldCert
		e.host.mu.Unlock()
		Expect(heartbeat(e)).NotTo(Equal(answerVerified))
		close(e.rec.Events)
		var reasons []string
		for ev := range e.rec.Events {
			reasons = append(reasons, ev)
			Expect(ev).NotTo(ContainSubstring("BEGIN"), "no PEM in events")
		}
		Expect(strings.Join(reasons, "\n")).To(And(ContainSubstring(eventReasonAgentRenewalRequested), ContainSubstring(eventReasonAgentRenewalApproved),
			ContainSubstring(eventReasonAgentRenewalIssued), ContainSubstring(eventReasonAgentRenewalPinned), ContainSubstring(eventReasonAgentRenewed)))
	})

	It("renews a short-lived certificate with every step after the request driven by the pins Secret watch, not the heartbeat", func() {
		life := hostidentity.MinLifetime
		e := newRenewalEnvLiving(life)
		pinsSecret := func() *corev1.Secret {
			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.issuerNS, Name: hostEnrolmentSecretName(e.hostNS, "host-a")}, sec)).To(Succeed())
			return sec
		}
		watch := pinsAdvanceHost(func() time.Time { return e.now })
		// pollIfWatched runs the Host controller's poll only when the pins
		// Secret change since before is one its watch enqueues, and fails
		// otherwise (the step would wait for the next heartbeat). It returns
		// the Secret the poll was enqueued for, so the next step's change
		// includes what the poll wrote.
		pollIfWatched := func(before *corev1.Secret, step string) *corev1.Secret {
			after := pinsSecret()
			Expect(watch.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after})).To(BeTrue(),
				"%s: the Host's poll follows the pins Secret change at once", step)
			Expect(heartbeat(e)).To(Equal(answerVerified))
			return after
		}
		Expect(heartbeat(e)).To(Equal(answerVerified), "the first heartbeat learns the agent's API version")
		// The one heartbeat the renewal waits for: the one that collects the
		// host's request.
		e.host.startRenewal(e.hostURI, "host-a")
		start := e.now
		s0 := pinsSecret()
		Expect(heartbeat(e)).To(Equal(answerVerified))
		Expect(state(e).Renewal).NotTo(BeNil())
		s1 := pollIfWatched(s0, "renewal recorded")
		reconcileHost(e)
		reconcileHost(e)
		s2 := pollIfWatched(s1, "request created")
		_, name, _ := strings.Cut(state(e).Renewal.CertificateRequest, "/")
		cr := reconcileCR(e, getCR(e, name))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		s3 := pollIfWatched(s2, "approved")
		der := issue(e, cr, hostidentitytest.IssueOptions{Lifetime: time.Hour})
		reconcileCR(e, cr)
		Expect(state(e).Renewal.Certificate).To(Equal(der))
		// The poll that follows the issuance stages it on the host (the
		// Provider's call), and the next answer pins the new key.
		e.host.stage(der)
		s4 := pollIfWatched(s3, "issued")
		newPin, err := hostidentity.SPKIPin(&e.host.newKey.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(state(e).Current.KeyPins).To(ContainElement(newPin), "pinned on the poll that followed the issuance")
		e.host.activate()
		pollIfWatched(s4, "pinned")
		st := state(e)
		Expect(st.Current.KeyPins).To(Equal([]string{newPin}), "renewed: the old pin is removed")
		Expect(st.Renewal).To(BeNil())
		Expect(e.now).To(Equal(start), "no heartbeat interval passed: every step followed the watch")
		Expect(e.now.Sub(start)).To(BeNumerically("<", life/6), "well within the agent's discard deadline")
	})

	It("applies its own rules to a renewal request another approver approved first, and publishes only what it would approve", func() {
		approveElsewhere := func(cr *unstructured.Unstructured) {
			Expect(certrequest.SetCondition(cr, certrequest.ConditionApproved, certrequest.StatusTrue, "policy.cert-manager.io",
				"Approved by CertificateRequestPolicy", time.Now())).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
		}

		// The manager's rules hold: it records its approval and publishes.
		e := newRenewalEnv()
		cr := requested(e)
		approveElsewhere(cr)
		cr = reconcileCR(e, cr)
		Expect(certrequest.IsTrue(cr, certrequest.ConditionDenied)).To(BeFalse(), "an approved request is never also denied")
		Expect(state(e).Renewal.Approved).To(BeTrue(), "the manager's own rules hold: its approval is recorded")
		der := issue(e, cr, hostidentitytest.IssueOptions{})
		reconcileCR(e, cr)
		Expect(state(e).Renewal.Certificate).To(Equal(der), "published")
		Expect(drainEvents(e.rec)).To(ContainElement(ContainSubstring(eventReasonAgentRenewalApproved)),
			"the manager's approval is in the audit trail, as on the undecided path")

		// The manager's rules deny it (the Host is revoked): nothing is
		// recorded as its approval and the certificate is never published.
		e = newRenewalEnv()
		cr = requested(e)
		h := hostObj(e)
		h.Spec.Revoked = true
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		approveElsewhere(cr)
		cr = reconcileCR(e, cr)
		Expect(certrequest.IsTrue(cr, certrequest.ConditionDenied)).To(BeFalse())
		Expect(state(e).Renewal.Approved).To(BeFalse(), "approved by someone else is never the manager's approval")
		issue(e, cr, hostidentitytest.IssueOptions{})
		cr = reconcileCR(e, cr)
		Expect(certrequest.IsTrue(cr, certrequest.ConditionRefused)).To(BeTrue(), "a certificate the manager would deny is marked refused")
		Expect(state(e).Renewal == nil || len(state(e).Renewal.Certificate) == 0).To(BeTrue(), "and never published")
		Expect(drainEvents(e.rec)).To(ContainElement(ContainSubstring(eventReasonAgentRenewalDenied)), "the Host says the manager refused it")
	})

	It("keeps a renewal request it refused after another approver approved it refused, and judges none while approvals are suspended", func() {
		approveElsewhere := func(cr *unstructured.Unstructured) {
			Expect(certrequest.SetCondition(cr, certrequest.ConditionApproved, certrequest.StatusTrue, "policy.cert-manager.io",
				"Approved by CertificateRequestPolicy", time.Now())).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
		}

		// Refused while a rule fails (the renewal's key is recorded as
		// previously pinned); once the rule passes again, still refused.
		e := newRenewalEnv()
		cr := requested(e)
		setPreviouslyPinned := func(on bool) {
			h := hostObj(e)
			l, err := loadHostEnrolment(ctx, k8sClient, e.cfg, h)
			Expect(err).NotTo(HaveOccurred())
			pin := l.state.Renewal.KeyPin
			l.state.PreviousKeyPins = slices.DeleteFunc(l.state.PreviousKeyPins, func(p string) bool { return p == pin })
			if on {
				l.state.PreviousKeyPins = append(l.state.PreviousKeyPins, pin)
			}
			Expect(saveHostEnrolment(ctx, k8sClient, e.cfg, h, &l)).To(Succeed())
		}
		setPreviouslyPinned(true)
		approveElsewhere(cr)
		cr = reconcileCR(e, cr)
		Expect(certrequest.IsTrue(cr, certrequest.ConditionRefused)).To(BeTrue(), "refused at once")
		setPreviouslyPinned(false) // the rule passes again
		cr = reconcileCR(e, cr)
		Expect(state(e).Renewal.Approved).To(BeFalse(), "a refused request is not judged again")
		issue(e, cr, hostidentitytest.IssueOptions{})
		cr = reconcileCR(e, cr)
		Expect(certrequest.IsTrue(cr, certrequest.ConditionRefused)).To(BeTrue(), "still refused")
		st := state(e)
		Expect(st.Renewal == nil || (!st.Renewal.Approved && len(st.Renewal.Certificate) == 0)).To(BeTrue(), "never approved, never published")

		// While approvals are suspended, a request another approver approved
		// is not judged: no approval of the manager's is recorded.
		e = newRenewalEnv()
		cr = requested(e)
		approveElsewhere(cr)
		e.reqR.Admission = &AdmissionGate{} // never checked: suspended
		res, err := e.reqR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cr.GetNamespace(), Name: cr.GetName()}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(admissionSuspendedRequeue))
		Expect(state(e).Renewal.Approved).To(BeFalse(), "nothing recorded while approvals are suspended")
		Expect(certrequest.IsTrue(getCR(e, cr.GetName()), certrequest.ConditionRefused)).To(BeFalse())
	})

	It("revisits an issued renewal request as soon as its pins Secret changes, and marks it refused once the renewal is withdrawn", func() {
		e := newRenewalEnv()
		cr := reconcileCR(e, requested(e))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		issue(e, cr, hostidentitytest.IssueOptions{})
		cr = reconcileCR(e, cr)
		Expect(state(e).Renewal.Certificate).NotTo(BeEmpty(), "issued, its key not pinned yet")
		// A revocation withdraws the renewal: the change lands on the Host
		// and the pins Secret, not on the request.
		h := hostObj(e)
		h.Spec.Revoked = true
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		reconcileHost(e)
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.issuerNS, Name: hostEnrolmentSecretName(e.hostNS, "host-a")}, sec)).To(Succeed())
		// The pins Secret's change maps to the request it owns (the watch the
		// manager registers), and that reconcile marks it refused.
		var reqs []ctrl.Request
		Eventually(func() []ctrl.Request {
			reqs = e.reqR.requestsOwnedBy(ctx, sec)
			return reqs
		}).Should(ContainElement(ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cr.GetNamespace(), Name: cr.GetName()}}))
		for _, rq := range reqs {
			_, err := e.reqR.Reconcile(ctx, rq)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(certrequest.IsTrue(getCR(e, cr.GetName()), certrequest.ConditionRefused)).To(BeTrue(), "marked without a restart or a resync")
	})

	It("reports its approval once, when it is saved, even when another approver wins the condition write", func() {
		e := newRenewalEnv()
		cr := requested(e)
		// Another approver approves the request between the manager's
		// judgement and its condition write, which then conflicts.
		e.reqR.Client = &approveFirstClient{Client: k8sClient}
		res, err := e.reqR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cr.GetNamespace(), Name: cr.GetName()}})
		Expect(err).NotTo(HaveOccurred(), "a lost condition write is retried quietly")
		Expect(res.Requeue).To(BeTrue(), "the condition write lost to the other approver") //nolint:staticcheck // Requeue is what the reconciler returns
		Expect(state(e).Renewal.Approved).To(BeTrue(), "the manager's approval was saved before the lost write")
		approved := func(evs []string) int {
			n := 0
			for _, ev := range evs {
				if strings.Contains(ev, eventReasonAgentRenewalApproved) {
					n++
				}
			}
			return n
		}
		evs := drainEvents(e.rec)
		Expect(approved(evs)).To(Equal(1), "reported when the approval was saved: %v", evs)
		// The retry finds the request approved elsewhere and the approval
		// recorded: it reports nothing again, and publishes.
		e.reqR.Client = k8sClient
		cr = reconcileCR(e, getCR(e, cr.GetName()))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		der := issue(e, cr, hostidentitytest.IssueOptions{})
		reconcileCR(e, cr)
		Expect(state(e).Renewal.Certificate).To(Equal(der), "published")
		reconcileCR(e, cr)
		Expect(approved(drainEvents(e.rec))).To(BeZero(), "the approval is reported exactly once")
	})

	It("tells the named Host when a request that was never its renewal is refused, once", func() {
		e := newRenewalEnv()
		cr := requested(e)
		csr, err := certrequest.CSR(cr)
		Expect(err).NotTo(HaveOccurred())
		forged := certrequest.NewRenewal(certrequest.RenewalSpec{Namespace: e.issuerNS, Name: "forged-approved", HostNamespace: e.hostNS,
			HostName: "host-a", HostHash: hostHashLabel(e.hostNS, "host-a"), IssuerRef: enrolTestCMIssuer, CSRPEM: csr,
			Duration: 24 * time.Hour, Username: "mallory", Groups: []string{"system:authenticated"}})
		Expect(k8sClient.Create(ctx, forged)).To(Succeed())
		forged = getCR(e, "forged-approved")
		Expect(certrequest.SetCondition(forged, certrequest.ConditionApproved, certrequest.StatusTrue, "policy.cert-manager.io",
			"Approved by CertificateRequestPolicy", time.Now())).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, forged)).To(Succeed())
		drainEvents(e.rec)
		issue(e, forged, hostidentitytest.IssueOptions{})
		forged = reconcileCR(e, forged)
		Expect(certrequest.IsTrue(forged, certrequest.ConditionRefused)).To(BeTrue())
		evs := drainEvents(e.rec)
		Expect(evs).To(ContainElement(SatisfyAll(ContainSubstring("Warning "+eventReasonAgentRenewalDenied),
			ContainSubstring("forged-approved"), ContainSubstring("was not the Host's renewal"))))
		for _, ev := range evs {
			Expect(ev).NotTo(ContainSubstring("BEGIN"), "no certificate or request material in the event")
		}
		reconcileCR(e, forged)
		Expect(drainEvents(e.rec)).NotTo(ContainElement(ContainSubstring(eventReasonAgentRenewalDenied)), "only the marking reconcile reports it")
		Expect(state(e).Renewal.CertificateRequest).To(Equal(cr.GetNamespace()+"/"+cr.GetName()), "the Host's own renewal is untouched")
	})

	It("marks an issued renewal certificate refused once its renewal is withdrawn and its key was never pinned", func() {
		e := newRenewalEnv()
		cr := reconcileCR(e, requested(e))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		// The renewal is withdrawn (a re-key or a restart dropped it) before
		// the issuance is seen; the issued key was never pinned.
		h := hostObj(e)
		l, err := loadHostEnrolment(ctx, k8sClient, e.cfg, h)
		Expect(err).NotTo(HaveOccurred())
		l.state.Renewal = nil
		Expect(saveHostEnrolment(ctx, k8sClient, e.cfg, h, &l)).To(Succeed())
		issue(e, cr, hostidentitytest.IssueOptions{})
		drainEvents(e.rec)
		cr = reconcileCR(e, cr)
		Expect(certrequest.IsTrue(cr, certrequest.ConditionRefused)).To(BeTrue(), "a certificate never published is marked refused")
		Expect(drainEvents(e.rec)).NotTo(ContainElement(ContainSubstring("was not the Host's renewal")),
			"a withdrawn renewal of the Host is not reported as a request that never was one")
	})

	It("never records a renewal co-signed by another key, or one that changes the identities", func() {
		e := newRenewalEnv()
		Expect(heartbeat(e)).To(Equal(answerVerified))
		e.host.forgeSigner = hostidentitytest.NewKey(GinkgoT())
		e.host.startRenewal(e.hostURI, "host-a")
		Expect(heartbeat(e)).NotTo(Equal(answerVerified), "a renewal co-signed by an unpinned key fails the whole answer")
		Expect(state(e).Renewal).To(BeNil())

		e.host.forgeSigner = nil
		other := e.host.rec
		other.EnrolmentID = bytes.Repeat([]byte{0x11}, 16)
		e.host.forgeURIs = hostidentity.RequestURIs(e.hostURI, other)
		e.host.startRenewal(e.hostURI, "host-a")
		Expect(heartbeat(e)).NotTo(Equal(answerVerified), "changed SANs fail the whole answer")
		epoch := e.host.rec
		epoch.StartEpoch = 4
		e.host.forgeURIs = hostidentity.RequestURIs(e.hostURI, epoch)
		e.host.startRenewal(e.hostURI, "host-a")
		Expect(heartbeat(e)).NotTo(Equal(answerVerified), "a changed epoch fails the whole answer")
		Expect(state(e).Renewal).To(BeNil())
		Expect(reconcileHost(e).Status.Agent.Renewal).To(BeNil())
	})

	It("never auto-approves a renewal request created by another principal", func() {
		e := newRenewalEnv()
		cr := requested(e)
		csr, err := certrequest.CSR(cr)
		Expect(err).NotTo(HaveOccurred())
		for _, who := range []struct {
			user   string
			groups []string
		}{
			{"mallory", []string{"system:authenticated"}},
			{"alice", []string{"system:authenticated", enrolTestAdminGroup}},
			{"system:serviceaccount:other:other", []string{"system:serviceaccounts", "system:authenticated"}},
		} {
			forged := certrequest.NewRenewal(certrequest.RenewalSpec{Namespace: e.issuerNS, Name: "forged-" + strings.ReplaceAll(strings.ReplaceAll(who.user, ":", "-"), "system-", ""),
				HostNamespace: e.hostNS, HostName: "host-a", HostHash: hostHashLabel(e.hostNS, "host-a"), IssuerRef: enrolTestCMIssuer,
				CSRPEM: csr, Duration: 24 * time.Hour, Username: who.user, Groups: who.groups})
			Expect(k8sClient.Create(ctx, forged)).To(Succeed())
			expectDenied(reconcileCR(e, forged), reasonRenewalRequester)
		}
		// A renewal request without the enrolment label is still judged as a
		// renewal.
		bare := certrequest.NewRenewal(certrequest.RenewalSpec{Namespace: e.issuerNS, Name: "forged-bare", HostNamespace: e.hostNS, HostName: "host-a",
			IssuerRef: enrolTestCMIssuer, CSRPEM: csr, Duration: 24 * time.Hour, Username: "mallory"})
		labels := bare.GetLabels()
		delete(labels, certrequest.LabelAgentEnrolment)
		bare.SetLabels(labels)
		Expect(k8sClient.Create(ctx, bare)).To(Succeed())
		expectDenied(reconcileCR(e, bare), reasonRenewalRequester)
		// The manager's own request names its renewal; another of its
		// requests that is not the Host's renewal in progress is denied.
		mine := certrequest.NewRenewal(certrequest.RenewalSpec{Namespace: e.issuerNS, Name: "manager-other", HostNamespace: e.hostNS,
			HostName: "host-a", IssuerRef: enrolTestCMIssuer, CSRPEM: csr, Duration: 24 * time.Hour, Username: renewalManager})
		Expect(k8sClient.Create(ctx, mine)).To(Succeed())
		expectDenied(reconcileCR(e, mine), reasonRenewalNotPending)
		Expect(state(e).Renewal.Approved).To(BeFalse())
	})

	It("denies the renewal of a revoked Host, or once the current certificate expired", func() {
		e := newRenewalEnv()
		cr := requested(e)
		h := hostObj(e)
		h.Spec.Revoked = true
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		expectDenied(reconcileCR(e, cr), reasonEnrolHostRevoked)
		reconcileHost(e)
		Expect(state(e).Renewal).To(BeNil(), "a revocation clears the renewal")

		e2 := newRenewalEnv()
		cr2 := requested(e2)
		e2.now = state(e2).Current.NotAfter.Add(time.Minute)
		expectDenied(reconcileCR(e2, cr2), reasonRenewalNotVerified)
	})

	It("never pins a renewed key on a stale answer or on another certificate's digest", func() {
		e := newRenewalEnv()
		cr := reconcileCR(e, requested(e))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		der := issue(e, cr, hostidentitytest.IssueOptions{})
		reconcileCR(e, cr)

		// Another certificate for the same key staged: not the issued one.
		other := e.ca.Issue(GinkgoT(), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: e.host.csr}),
			hostidentitytest.IssueOptions{Lifetime: 29 * 24 * time.Hour})
		ob, _ := pem.Decode(other)
		e.host.stage(ob.Bytes)
		Expect(heartbeat(e)).To(Equal(answerVerified))
		Expect(state(e).Current.KeyPins).To(Equal([]string{e.oldPin}), "another digest pins nothing")

		// The right digest, but in an answer to another request's nonce.
		e.host.stage(der)
		e.reader.stale = true
		Expect(heartbeat(e)).NotTo(Equal(answerVerified))
		Expect(state(e).Current.KeyPins).To(Equal([]string{e.oldPin}), "a stale answer pins nothing")
		e.reader.stale = false
		Expect(heartbeat(e)).To(Equal(answerVerified))
		Expect(state(e).Current.KeyPins).To(HaveLen(2))
	})

	It("keeps both pins past the overlap and unpins the old key only when the old certificate expires", func() {
		e := newRenewalEnv()
		cr := reconcileCR(e, requested(e))
		der := issue(e, cr, hostidentitytest.IssueOptions{})
		reconcileCR(e, cr)
		e.host.stage(der)
		Expect(heartbeat(e)).To(Equal(answerVerified))
		st := state(e)
		Expect(st.Current.KeyPins).To(HaveLen(2))
		newPin := st.Renewal.KeyPin

		e.now = st.Renewal.OverlapUntil.Add(time.Minute)
		reconcileHost(e)
		st = state(e)
		Expect(st.Current.KeyPins).To(Equal([]string{e.oldPin, newPin}), "an expired overlap removes no pin")
		Expect(st.Renewal.OverlapExpiredReported).To(BeTrue())
		Expect(drainRenewalEvents(e.rec)).To(ContainSubstring(eventReasonAgentRenewalOverlap))

		e.now = st.Current.NotAfter.Add(time.Minute)
		h := reconcileHost(e)
		st = state(e)
		Expect(st.Current.KeyPins).To(Equal([]string{newPin}), "at the old certificate's expiry only the new key stays pinned")
		Expect(st.Renewal).To(BeNil())
		Expect(h.Status.Agent.CertificateExpiry.Time).To(BeTemporally(">", e.now), "the expiry is the renewed certificate's")
	})

	It("creates nothing while the manager does not know its identity, says so once, and goes on once it does", func() {
		e := newRenewalEnv()
		e.cfg.SetManagerIdentity(ManagerIdentity{})
		Expect(heartbeat(e)).To(Equal(answerVerified))
		e.host.startRenewal(e.hostURI, "host-a")
		Expect(heartbeat(e)).To(Equal(answerVerified))
		reconcileHost(e)
		reconcileHost(e)
		Expect(state(e).Renewal.CertificateRequest).To(BeEmpty())
		Expect(state(e).Renewal.IdentityReported).To(BeTrue())
		Expect(strings.Count(drainRenewalEvents(e.rec), eventReasonAgentRenewalNoIdentity)).To(Equal(1), "reported once")
		list := certrequest.NewList()
		Expect(k8sClient.List(ctx, list, client.InNamespace(e.issuerNS))).To(Succeed())
		Expect(list.Items).To(BeEmpty())

		res := &ManagerIdentityResolver{Client: k8sClient, Config: e.cfg, Every: 10 * time.Millisecond}
		Expect(res.Start(ctx)).To(Succeed())
		_, ok := e.cfg.managerIdentity()
		Expect(ok).To(BeTrue())
		reconcileHost(e)
		reconcileHost(e)
		Expect(state(e).Renewal.CertificateRequestUID).NotTo(BeEmpty(), "the request is created once the identity is known")
	})

	// issueAndRefuse drives the renewal in progress to an issued certificate
	// that the manager refuses (another CA signed it).
	issueAndRefuse := func(e *renewalEnv, cr *unstructured.Unstructured) {
		foreign := hostidentitytest.NewCA(GinkgoT())
		csr, err := certrequest.CSR(cr)
		Expect(err).NotTo(HaveOccurred())
		Expect(certrequest.SetCertificate(cr, foreign.Issue(GinkgoT(), csr, hostidentitytest.IssueOptions{}), foreign.PEM)).To(Succeed())
		Expect(certrequest.SetCondition(cr, certrequest.ConditionReady, certrequest.StatusTrue, certrequest.ReasonIssued, "issued", time.Now())).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
		reconcileCR(e, cr)
	}

	It("backs off a renewal whose issued certificate is refused, gives the key up after five failures, and deletes the requests", func() {
		e := newRenewalEnv()
		cr := reconcileCR(e, requested(e))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		issueAndRefuse(e, cr)
		st := state(e)
		Expect(st.Renewal).To(BeNil())
		Expect(st.RenewalAttempts).To(HaveLen(1))
		Expect(st.RenewalAttempts[0].Failures).To(Equal(1))
		Expect(drainRenewalEvents(e.rec)).To(ContainSubstring(eventReasonAgentRenewalRestarted))
		reconcileHost(e)
		Expect(renewalRequests(e)).To(HaveLen(1), "a finished request is retained (the newest few)")

		// Within the backoff the same key is not recorded again.
		Expect(heartbeat(e)).To(Equal(answerVerified))
		Expect(state(e).Renewal).To(BeNil(), "held back")

		for i := 2; i <= maxRenewalFailures; i++ {
			e.now = state(e).RenewalAttempts[0].NextTry.Add(time.Second)
			Expect(heartbeat(e)).To(Equal(answerVerified))
			Expect(state(e).Renewal).NotTo(BeNil(), "recorded again after the backoff (failure %d)", i)
			reconcileHost(e)
			reconcileHost(e)
			st = state(e)
			_, name, _ := strings.Cut(st.Renewal.CertificateRequest, "/")
			cr = reconcileCR(e, getCR(e, name))
			issueAndRefuse(e, cr)
			Expect(state(e).RenewalAttempts[0].Failures).To(Equal(i))
		}
		Expect(drainRenewalEvents(e.rec)).To(ContainSubstring(eventReasonAgentRenewalGivenUp))
		e.now = e.now.Add(48 * time.Hour)
		Expect(heartbeat(e)).To(Equal(answerVerified))
		Expect(state(e).Renewal).To(BeNil(), "a given-up key is never renewed again")
		reconcileHost(e)
		Expect(renewalRequests(e)).To(HaveLen(renewalRequestsRetained), "only the newest finished requests are kept")

		// The host draws a new key: it is renewed.
		e.host.startRenewal(e.hostURI, "host-a")
		Expect(heartbeat(e)).To(Equal(answerVerified))
		Expect(state(e).Renewal).NotTo(BeNil())
	})

	It("restarts a renewal whose request is denied, failed, deleted or unlabelled", func() {
		for _, breakIt := range []func(e *renewalEnv, cr *unstructured.Unstructured){
			func(_ *renewalEnv, cr *unstructured.Unstructured) {
				Expect(certrequest.SetCondition(cr, certrequest.ConditionDenied, certrequest.StatusTrue, "Other", "denied", time.Now())).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
			},
			func(_ *renewalEnv, cr *unstructured.Unstructured) {
				Expect(certrequest.SetCondition(cr, certrequest.ConditionReady, certrequest.StatusFalse, certrequest.ReasonFailed, "failed", time.Now())).To(Succeed())
				Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
			},
			func(_ *renewalEnv, cr *unstructured.Unstructured) { Expect(k8sClient.Delete(ctx, cr)).To(Succeed()) },
			func(_ *renewalEnv, cr *unstructured.Unstructured) {
				l := cr.GetLabels()
				delete(l, certrequest.LabelAgentRenewal)
				cr.SetLabels(l)
				Expect(k8sClient.Update(ctx, cr)).To(Succeed())
			},
		} {
			e := newRenewalEnv()
			cr := requested(e)
			oldName := cr.GetName()
			breakIt(e, cr)
			reconcileHost(e)
			st := state(e)
			Expect(st.Renewal).To(BeNil(), "the renewal restarts")
			Expect(st.RenewalAttempts).To(HaveLen(1))
			e.now = st.RenewalAttempts[0].NextTry.Add(time.Second)
			Expect(heartbeat(e)).To(Equal(answerVerified))
			reconcileHost(e)
			reconcileHost(e)
			Expect(state(e).Renewal.CertificateRequestUID).NotTo(BeEmpty(), "a new request is created")
			Expect(state(e).Renewal.CertificateRequest).NotTo(HaveSuffix("/"+oldName), "under a new name")
		}
	})

	It("draws another name when a request of the drawn name exists and is not the manager's", func() {
		e := newRenewalEnv()
		Expect(heartbeat(e)).To(Equal(answerVerified))
		e.host.startRenewal(e.hostURI, "host-a")
		Expect(heartbeat(e)).To(Equal(answerVerified))
		reconcileHost(e)
		st := state(e)
		_, name, _ := strings.Cut(st.Renewal.CertificateRequest, "/")
		body, _, err := st.Renewal.body()
		Expect(err).NotTo(HaveOccurred())
		squat := certrequest.NewRenewal(certrequest.RenewalSpec{Namespace: e.issuerNS, Name: name, HostNamespace: e.hostNS, HostName: "host-a",
			HostHash: hostHashLabel(e.hostNS, "host-a"), IssuerRef: enrolTestCMIssuer, Duration: time.Hour, Username: "mallory",
			CSRPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: body.GetCertificateRequest()})})
		Expect(k8sClient.Create(ctx, squat)).To(Succeed())
		reconcileHost(e)
		st2 := state(e)
		Expect(st2.Renewal).NotTo(BeNil(), "the renewal is kept")
		Expect(st2.Renewal.CertificateRequest).NotTo(Equal(st.Renewal.CertificateRequest), "another name is drawn")
		reconcileHost(e)
		Expect(state(e).Renewal.CertificateRequestUID).NotTo(BeEmpty())
		Expect(getCR(e, name).GetUID()).To(Equal(squat.GetUID()), "the other principal's object is left alone")
	})

	It("pins the issued key at the old certificate's expiry when no evidence arrived, on live state only", func() {
		issued := func(e *renewalEnv) []byte {
			cr := reconcileCR(e, requested(e))
			der := issue(e, cr, hostidentitytest.IssueOptions{})
			reconcileCR(e, cr)
			return der
		}
		// The Provider activated early and the evidence never arrived.
		e := newRenewalEnv()
		der := issued(e)
		newPin := state(e).Renewal.KeyPin
		e.host.stage(der)
		e.host.activate()
		e.now = state(e).Current.NotAfter.Add(-time.Minute)
		reconcileHost(e)
		Expect(state(e).Current.KeyPins).To(Equal([]string{e.oldPin}), "nothing before the expiry")
		e.now = state(e).Current.NotAfter.Add(time.Minute)
		h := reconcileHost(e)
		st := state(e)
		Expect(st.Current.KeyPins).To(Equal([]string{newPin}), "the issued key is the only pin")
		Expect(st.Renewal).To(BeNil())
		Expect(h.Status.Agent.KeyPins).To(Equal([]string{newPin}))
		Expect(drainRenewalEvents(e.rec)).To(ContainSubstring(eventReasonAgentRenewalPinnedExpiry))

		// A revocation before the expiry prevents it.
		e2 := newRenewalEnv()
		issued(e2)
		h2 := hostObj(e2)
		h2.Spec.Revoked = true
		Expect(k8sClient.Update(ctx, h2)).To(Succeed())
		reconcileHost(e2)
		e2.now = time.Now().Add(21 * 24 * time.Hour)
		reconcileHost(e2)
		Expect(state(e2).Current).To(BeNil())

		// A replaced renewal (the host offered another key) prevents it.
		e3 := newRenewalEnv()
		issued(e3)
		e3.host.startRenewal(e3.hostURI, "host-a")
		Expect(heartbeat(e3)).To(Equal(answerVerified))
		Expect(state(e3).Renewal.Certificate).To(BeEmpty(), "the renewal restarted for the new key")
		e3.now = state(e3).Current.NotAfter.Add(time.Minute)
		reconcileHost(e3)
		Expect(state(e3).Current.KeyPins).To(Equal([]string{e3.oldPin}), "no pin without an issued certificate")

		// A re-key before the expiry prevents it.
		e4 := newRenewalEnv()
		issued(e4)
		h4 := hostObj(e4)
		l, err := loadHostEnrolment(ctx, k8sClient, e4.cfg, h4)
		Expect(err).NotTo(HaveOccurred())
		rekeyed := strings.Repeat("dd", 32)
		next := &currentEnrolment{Record: l.state.Current.Record, KeyPins: []string{rekeyed}, NotAfter: metav1.NewTime(time.Now().Add(20 * 24 * time.Hour))}
		next.Record.EnrolmentID = strings.Repeat("6b", 16)
		next.Record.StartEpoch = 4
		l.state.rekey(next, time.Now())
		Expect(saveHostEnrolment(ctx, k8sClient, e4.cfg, h4, &l)).To(Succeed())
		e4.now = time.Now().Add(21 * 24 * time.Hour)
		reconcileHost(e4)
		Expect(state(e4).Current.KeyPins).To(Equal([]string{rekeyed}), "the re-keyed enrolment's pin only")
	})

	It("pins and completes on an answer signed by exactly the issued certificate when the host activated early", func() {
		e := newRenewalEnv()
		cr := reconcileCR(e, requested(e))
		der := issue(e, cr, hostidentitytest.IssueOptions{})
		reconcileCR(e, cr)
		newPin := state(e).Renewal.KeyPin
		// A faulty Provider activates before the manager pinned the new key.
		e.host.stage(der)
		e.host.activate()
		Expect(heartbeat(e)).To(Equal(answerVerified), "verified with the issued certificate's key")
		st := state(e)
		Expect(st.Current.KeyPins).To(Equal([]string{newPin}))
		Expect(st.Renewal).To(BeNil())
		Expect(drainRenewalEvents(e.rec)).To(ContainSubstring(eventReasonAgentRenewalEarly))

		// Another certificate for the same new key, not the issued one, is
		// never evidence.
		e2 := newRenewalEnv()
		cr2 := reconcileCR(e2, requested(e2))
		issue(e2, cr2, hostidentitytest.IssueOptions{})
		reconcileCR(e2, cr2)
		other := e2.ca.Issue(GinkgoT(), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: e2.host.csr}),
			hostidentitytest.IssueOptions{Lifetime: 29 * 24 * time.Hour})
		ob, _ := pem.Decode(other)
		e2.host.stage(ob.Bytes)
		e2.host.activate()
		Expect(heartbeat(e2)).NotTo(Equal(answerVerified))
		Expect(state(e2).Current.KeyPins).To(HaveLen(1))
		Expect(state(e2).Current.KeyPins[0]).To(Equal(e2.oldPin))
	})

	It("reads the manager's own identity from the API server", func() {
		id, err := ResolveManagerIdentity(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(id.Username).NotTo(BeEmpty())
	})
})

// drainRenewalEvents returns the recorded events so far.
func drainRenewalEvents(rec *record.FakeRecorder) string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return strings.Join(out, "\n")
		}
	}
}

// approveFirstClient lets another approver approve a CertificateRequest just
// before the reconciler's own status write, once, so that write conflicts.
type approveFirstClient struct {
	client.Client
	done bool
}

func (c *approveFirstClient) Status() client.SubResourceWriter {
	return &approveFirstStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type approveFirstStatus struct {
	client.SubResourceWriter
	c *approveFirstClient
}

func (s *approveFirstStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if u, ok := obj.(*unstructured.Unstructured); ok && !s.c.done && u.GetKind() == certrequest.NewObject().GetKind() {
		s.c.done = true
		other := certrequest.NewObject()
		if err := s.c.Get(ctx, client.ObjectKeyFromObject(u), other); err != nil {
			return err
		}
		if err := certrequest.SetCondition(other, certrequest.ConditionApproved, certrequest.StatusTrue, "policy.cert-manager.io",
			"Approved by CertificateRequestPolicy", time.Now()); err != nil {
			return err
		}
		if err := s.c.Client.Status().Update(ctx, other); err != nil {
			return err
		}
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}
