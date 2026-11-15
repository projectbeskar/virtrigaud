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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
)

const (
	enrolTestAdminGroup  = "virtrigaud:host-enrolment-admins"
	enrolTestTrustDomain = "cluster.test"
)

var (
	enrolTestCMIssuer  = certrequest.IssuerRef{Group: certrequest.Group, Kind: "Issuer", Name: "host-agent-ca"}
	enrolTestExtIssuer = certrequest.IssuerRef{Group: "external-pki.example.test", Kind: "ExternalIssuer", Name: "enterprise"}
)

// These specs run per-host agent enrolment, manager side, against a real
// apiserver: the Host reconciler and the request reconciler are called
// directly, and the test plays cert-manager (its admission webhook records
// spec.username/spec.groups; its CA issuer signs approved requests).
var _ = Describe("Per-host agent enrolment (envtest)", func() {
	ctx := context.Background()

	type env struct {
		hostNS, issuerNS string
		provider         *infravirtrigaudiov1beta1.Provider
		ca               *hostidentitytest.CA
		cfg              *HostEnrolmentConfig
		hostR            *HostEnrolmentReconciler
		reqR             *HostEnrolmentRequestReconciler
	}

	newEnv := func() *env {
		e := &env{}
		for _, p := range []string{"enrol-hosts-", "enrol-issuer-"} {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: p}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			if p == "enrol-hosts-" {
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
		cm, ext := enrolTestCMIssuer, enrolTestExtIssuer
		e.cfg = &HostEnrolmentConfig{
			Namespace: e.issuerNS, CertManagerIssuer: &cm, ExternalPKIIssuer: &ext,
			AdminGroup: enrolTestAdminGroup, TrustDomain: enrolTestTrustDomain,
			CABundle: e.ca.PEM, InstallID: bytes.Repeat([]byte{0x42}, 16),
		}
		e.hostR = &HostEnrolmentReconciler{Client: k8sClient, Config: e.cfg}
		e.reqR = &HostEnrolmentRequestReconciler{Client: k8sClient, Config: e.cfg}
		// The administrator "alice" may enrol and re-key Hosts in the hosts
		// namespace (the enrol and rekey verbs, checked by a
		// SubjectAccessReview); "dave" may enrol only; "carol" is in the
		// administrator group but has no such grant.
		Expect(k8sClient.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "host-enrolers", Namespace: e.hostNS},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{infravirtrigaudiov1beta1.GroupVersion.Group}, Resources: []string{"hosts"},
				Verbs: []string{infravirtrigaudiov1beta1.HostEnrolVerb, infravirtrigaudiov1beta1.HostRekeyVerb}}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "host-enrol-only", Namespace: e.hostNS},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{infravirtrigaudiov1beta1.GroupVersion.Group}, Resources: []string{"hosts"},
				Verbs: []string{infravirtrigaudiov1beta1.HostEnrolVerb}}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "host-enrol-only", Namespace: e.hostNS},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "host-enrol-only"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "dave"}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "host-enrolers", Namespace: e.hostNS},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "host-enrolers"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "alice"}},
		})).To(Succeed())
		return e
	}

	createHost := func(e *env, name, identity, providerName string) *infravirtrigaudiov1beta1.Host {
		h := &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.hostNS},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: providerName},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "grpc://" + name + ":7443",
				Identity:    identity,
				Schedulable: true,
			},
		}
		Expect(k8sClient.Create(ctx, h)).To(Succeed())
		return h
	}

	reconcileHost := func(e *env, name string) *infravirtrigaudiov1beta1.Host {
		_, err := e.hostR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.hostNS, Name: name}})
		Expect(err).NotTo(HaveOccurred())
		h := &infravirtrigaudiov1beta1.Host{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.hostNS, Name: name}, h)).To(Succeed())
		return h
	}

	type crOpts struct {
		groups   []string
		username string
		duration time.Duration
		isCA     bool
		usages   []string
		issuer   certrequest.IssuerRef
		hostName string
	}
	adminOpts := func() crOpts {
		return crOpts{groups: []string{"system:authenticated", enrolTestAdminGroup}, username: "alice",
			duration: 30 * 24 * time.Hour, issuer: enrolTestCMIssuer}
	}

	// submit creates an enrolment request as an administrator would, then
	// fills spec.username/spec.groups as cert-manager's webhook would.
	submit := func(e *env, hostName string, csr []byte, o crOpts) *unstructured.Unstructured {
		if o.hostName != "" {
			hostName = o.hostName
		}
		cr := certrequest.New(certrequest.Spec{
			Namespace: e.issuerNS, GenerateName: "enrol-" + hostName + "-", HostNamespace: e.hostNS, HostName: hostName,
			IssuerRef: o.issuer, CSRPEM: csr, Duration: o.duration,
		})
		spec, ok := cr.Object["spec"].(map[string]any)
		Expect(ok).To(BeTrue())
		spec["username"] = o.username
		groups := make([]any, 0, len(o.groups))
		for _, g := range o.groups {
			groups = append(groups, g)
		}
		spec["groups"] = groups
		spec["isCA"] = o.isCA
		if o.usages != nil {
			us := make([]any, 0, len(o.usages))
			for _, u := range o.usages {
				us = append(us, u)
			}
			spec["usages"] = us
		}
		if o.duration == 0 {
			delete(spec, "duration")
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		return cr
	}

	reconcileCR := func(e *env, cr *unstructured.Unstructured) *unstructured.Unstructured {
		_, err := e.reqR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cr.GetNamespace(), Name: cr.GetName()}})
		Expect(err).NotTo(HaveOccurred())
		got := certrequest.NewObject()
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), got)).To(Succeed())
		return got
	}

	// issue plays cert-manager's CA issuer: sign and set Ready=True.
	issue := func(e *env, cr *unstructured.Unstructured, csr []byte, o hostidentitytest.IssueOptions) {
		Expect(certrequest.SetCertificate(cr, e.ca.Issue(GinkgoT(), csr, o), e.ca.PEM)).To(Succeed())
		Expect(certrequest.SetCondition(cr, certrequest.ConditionReady, certrequest.StatusTrue, certrequest.ReasonIssued, "issued", time.Now())).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
	}

	loadState := func(e *env, name string) (hostEnrolmentState, *corev1.Secret) {
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.issuerNS, Name: hostEnrolmentSecretName(e.hostNS, name)}, sec)).To(Succeed())
		var st hostEnrolmentState
		Expect(json.Unmarshal(sec.Data[hostEnrolmentStateKey], &st)).To(Succeed())
		return st, sec
	}

	csrFor := func(h *infravirtrigaudiov1beta1.Host, mutate func(*hostidentitytest.CSROptions)) []byte {
		o := hostidentitytest.CSROptions{URIs: h.Status.Agent.RequestURIs}
		if mutate != nil {
			mutate(&o)
		}
		return hostidentitytest.NewCSR(GinkgoT(), hostidentitytest.NewKey(GinkgoT()), o)
	}

	expectDenied := func(cr *unstructured.Unstructured, reason string) {
		s, r, found := certrequest.Condition(cr, certrequest.ConditionDenied)
		Expect(found).To(BeTrue(), "the request must be denied")
		Expect(s).To(Equal(certrequest.StatusTrue))
		Expect(r).To(Equal(reason))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeFalse())
	}

	// enrol drives a host to Enrolled through the cert-manager path and
	// returns the pinned key's pin and the issued certificate.
	enrol := func(e *env, name string) (string, []byte) {
		h := reconcileHost(e, name)
		key := hostidentitytest.NewKey(GinkgoT())
		csr := hostidentitytest.NewCSR(GinkgoT(), key, hostidentitytest.CSROptions{CommonName: name, URIs: h.Status.Agent.RequestURIs})
		cr := reconcileCR(e, submit(e, name, csr, adminOpts()))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		cert := e.ca.Issue(GinkgoT(), csr, hostidentitytest.IssueOptions{})
		Expect(certrequest.SetCertificate(cr, cert, e.ca.PEM)).To(Succeed())
		Expect(certrequest.SetCondition(cr, certrequest.ConditionReady, certrequest.StatusTrue, certrequest.ReasonIssued, "issued", time.Now())).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
		reconcileCR(e, cr)
		pin, err := hostidentity.SPKIPin(&key.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		return pin, cert
	}

	rekeySeq := 0
	rekeyBase := time.Now()
	// requestRekey opens a re-key window on an enrolled Host, as an
	// administrator would (annotation set to now; each call a new value), and
	// returns the Host after its reconcile.
	requestRekey := func(e *env, name string) *infravirtrigaudiov1beta1.Host {
		h := &infravirtrigaudiov1beta1.Host{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.hostNS, Name: name}, h)).To(Succeed())
		if h.Annotations == nil {
			h.Annotations = map[string]string{}
		}
		rekeySeq++
		// Distinct values, each a recent time.
		h.Annotations[infravirtrigaudiov1beta1.HostRekeyRequestAnnotation] = rekeyBase.Add(-time.Duration(rekeySeq) * time.Second).UTC().Format(time.RFC3339)
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		return reconcileHost(e, name)
	}
	// rekey re-keys an enrolled Host end to end and returns the new pin.
	rekey := func(e *env, name string) string {
		requestRekey(e, name)
		pin, _ := enrol(e, name)
		return pin
	}

	It("enrols a host end to end with the cert-manager issuer and derives the record from the certificate's SANs", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")

		h := reconcileHost(e, "host-a")
		Expect(h.Status.Agent).NotTo(BeNil())
		Expect(h.Status.Agent.PendingEnrolmentID).To(MatchRegexp(`^[0-9a-f]{32}$`))
		Expect(h.Status.Agent.RequestURIs).To(HaveLen(5))
		Expect(h.Status.Agent.RequestURIs[0]).To(Equal("spiffe://cluster.test/virtrigaud/host/" + e.hostNS + "/host-a"))
		Expect(meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).
			To(Equal(infravirtrigaudiov1beta1.HostReasonAgentAwaitingRequest))
		pendingID := h.Status.Agent.PendingEnrolmentID

		pin, cert := enrol(e, "host-a")

		st, sec := loadState(e, "host-a")
		Expect(sec.Namespace).To(Equal(e.issuerNS), "the pins Secret lives in the enrolment namespace, never the Host's")
		Expect(sec.OwnerReferences).To(BeEmpty())
		Expect(st.Current).NotTo(BeNil())
		Expect(st.Current.KeyPins).To(Equal([]string{pin}))
		Expect(st.Current.Record.EnrolmentID).To(Equal(pendingID))
		Expect(st.Pending).To(BeNil())
		Expect(st.RetiredEnrolmentIDs).To(ContainElement(pendingID))

		// The record derived from the issued certificate's SANs is the
		// pinned one: enrolment id, Provider UID, install id, mode.
		certs, err := hostidentity.ParseCertificates(cert)
		Expect(err).NotTo(HaveOccurred())
		rec, err := hostidentity.RecordFromURIs(certs[0].URIs, h.Status.Agent.RequestURIs[0])
		Expect(err).NotTo(HaveOccurred())
		pinned, err := st.Current.Record.toRecord()
		Expect(err).NotTo(HaveOccurred())
		Expect(rec.Equal(pinned)).To(BeTrue())
		Expect(rec.ProviderUID).To(Equal(string(e.provider.UID)))

		h = reconcileHost(e, "host-a")
		Expect(h.Status.Agent.KeyPins).To(Equal([]string{pin}))
		Expect(h.Status.Agent.EnrolmentID).To(Equal(pendingID))
		Expect(h.Status.Agent.CertificateExpiry).NotTo(BeNil())
		c := meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled)
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(infravirtrigaudiov1beta1.HostReasonAgentEnrolled))
		Expect(c.ObservedGeneration).To(Equal(h.Generation))
	})

	DescribeTable("denies a request that breaks an authorisation rule and leaves the pins unchanged",
		func(mutateCSR func(*hostidentitytest.CSROptions), mutateCR func(*crOpts), reason string) {
			e := newEnv()
			createHost(e, "host-a", "serial-0001", "ch")
			createHost(e, "host-b", "serial-0002", "ch")
			h := reconcileHost(e, "host-a")
			reconcileHost(e, "host-b")
			before, _ := loadState(e, "host-a")

			var csr []byte
			if mutateCSR != nil {
				csr = csrFor(h, mutateCSR)
			} else {
				csr = csrFor(h, nil)
			}
			o := adminOpts()
			if mutateCR != nil {
				mutateCR(&o)
			}
			cr := reconcileCR(e, submit(e, "host-a", csr, o))
			expectDenied(cr, reason)

			after, _ := loadState(e, "host-a")
			Expect(after.Current).To(BeNil(), "no pin")
			Expect(after.Authorised).To(BeNil(), "nothing authorised")
			Expect(after.Pending).To(Equal(before.Pending), "the pending record is unchanged")
			afterB, _ := loadState(e, "host-b")
			Expect(afterB.Current).To(BeNil(), "the other host gains no pin")
		},
		Entry("a request from another principal", nil, func(o *crOpts) {
			o.username, o.groups = "mallory", []string{"system:authenticated", "developers"}
		}, hostidentity.ReasonRequesterNotAuthorised),
		Entry("a SAN for another host", func(c *hostidentitytest.CSROptions) {
			c.URIs = append([]string{"spiffe://cluster.test/virtrigaud/host/other/host-b"}, c.URIs[1:]...)
		}, nil, hostidentity.ReasonSubjectAltNames),
		Entry("a missing proof of possession", func(c *hostidentitytest.CSROptions) { c.BreakSignature = true }, nil,
			hostidentity.ReasonProofOfPossession),
		Entry("a lifetime over thirty days", nil, func(o *crOpts) { o.duration = 31 * 24 * time.Hour }, hostidentity.ReasonLifetime),
		Entry("no lifetime (cert-manager would default to ninety days)", nil, func(o *crOpts) { o.duration = 0 }, hostidentity.ReasonLifetime),
		Entry("the CA bit in the request spec", nil, func(o *crOpts) { o.isCA = true }, hostidentity.ReasonCARequested),
		Entry("the CA bit in the CSR", func(c *hostidentitytest.CSROptions) { c.RequestCA = true }, nil, hostidentity.ReasonCARequested),
		Entry("usages without server authentication", nil, func(o *crOpts) {
			o.usages = []string{hostidentity.UsageDigitalSignature}
		}, hostidentity.ReasonUsages),
		Entry("client authentication requested", nil, func(o *crOpts) {
			o.usages = []string{hostidentity.UsageClientAuth, hostidentity.UsageServerAuth}
		}, hostidentity.ReasonUsages),
	)

	It("denies a first enrolment requested by a non-administrator", func() {
		e := newEnv()
		h := createHost(e, "host-a", "serial-0001", "ch")
		h = reconcileHost(e, h.Name)
		o := adminOpts()
		o.username, o.groups = "bob", []string{"system:authenticated"}
		cr := reconcileCR(e, submit(e, h.Name, csrFor(h, nil), o))
		expectDenied(cr, hostidentity.ReasonRequesterNotAuthorised)
		// A non-administrator's request leaves no trace on the Host.
		h = reconcileHost(e, h.Name)
		Expect(meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).
			To(Equal(infravirtrigaudiov1beta1.HostReasonAgentAwaitingRequest))
		st, _ := loadState(e, h.Name)
		Expect(st.LastDenial).To(BeNil())
	})

	It("denies a request for a revoked Host, even one carrying the pending record", func() {
		e := newEnv()
		h := createHost(e, "host-a", "serial-0001", "ch")
		h = reconcileHost(e, h.Name)
		csr := csrFor(h, nil)
		h.Spec.Revoked = true
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		cr := reconcileCR(e, submit(e, h.Name, csr, adminOpts()))
		expectDenied(cr, reasonEnrolHostRevoked)
		h = reconcileHost(e, h.Name)
		Expect(meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).
			To(Equal(infravirtrigaudiov1beta1.HostReasonAgentRevoked))
	})

	It("does not pin a certificate issued after the Host was revoked", func() {
		e := newEnv()
		h := reconcileHost(e, createHost(e, "host-a", "serial-0001", "ch").Name)
		csr := csrFor(h, nil)
		cr := reconcileCR(e, submit(e, h.Name, csr, adminOpts()))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		h.Spec.Revoked = true
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		issue(e, cr, csr, hostidentitytest.IssueOptions{})
		reconcileCR(e, cr)
		st, _ := loadState(e, h.Name)
		Expect(st.Current).To(BeNil())
		Expect(st.LastRejection).NotTo(BeNil())
	})

	It("denies requests naming a missing Host, a Host without identity, or a libvirt Host", func() {
		e := newEnv()
		lib := envtestProvider(e.hostNS, "libvirt", nil)
		lib.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
		Expect(k8sClient.Create(ctx, lib)).To(Succeed())
		createHost(e, "no-identity", "", "ch")
		createHost(e, "lv", "serial-9", "libvirt")
		csr := hostidentitytest.NewCSR(GinkgoT(), hostidentitytest.NewKey(GinkgoT()), hostidentitytest.CSROptions{})

		expectDenied(reconcileCR(e, submit(e, "ghost", csr, adminOpts())), reasonEnrolHostNotFound)
		expectDenied(reconcileCR(e, submit(e, "no-identity", csr, adminOpts())), reasonEnrolIdentityMissing)
		expectDenied(reconcileCR(e, submit(e, "lv", csr, adminOpts())), reasonEnrolHostNotAgent)

		// A libvirt host gains no agent status and no pins Secret.
		lv := reconcileHost(e, "lv")
		Expect(lv.Status.Agent).To(BeNil())
		Expect(meta.FindStatusCondition(lv.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled)).To(BeNil())
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: e.issuerNS, Name: hostEnrolmentSecretName(e.hostNS, "lv")}, &corev1.Secret{})
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		Expect(err).To(HaveOccurred())
	})

	It("re-keys an enrolled Host for an administrator, in a requested window, at the next start epoch", func() {
		e := newEnv()
		rec := record.NewFakeRecorder(64)
		e.reqR.Recorder = rec
		createHost(e, "host-a", "serial-0001", "ch")
		pin1, _ := enrol(e, "host-a")
		st1, _ := loadState(e, "host-a")
		first := st1.Current.Record.EnrolmentID
		Expect(st1.Current.mintEpoch()).To(Equal(uint64(1)))

		// Enrolled and no window requested: no record.
		h := reconcileHost(e, "host-a")
		Expect(h.Status.Agent.PendingEnrolmentID).To(BeEmpty())
		Expect(h.Status.Agent.RequestURIs).To(BeEmpty())

		h = requestRekey(e, "host-a")
		Expect(h.Status.Agent.PendingEnrolmentID).To(MatchRegexp(`^[0-9a-f]{32}$`))
		Expect(h.Status.Agent.PendingEnrolmentID).NotTo(Equal(first))
		Expect(h.Status.Agent.EnrolmentID).To(Equal(first))
		Expect(h.Status.Agent.RequestURIs).To(HaveLen(6))
		Expect(h.Status.Agent.RequestURIs[5]).To(Equal(hostidentity.URIEpochPrefix+"2"), "the record carries the next start epoch")
		cond := meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled)
		Expect(cond.Reason).To(Equal(infravirtrigaudiov1beta1.HostReasonAgentEnrolled))
		Expect(cond.Message).To(ContainSubstring("re-key window is open"))
		second := h.Status.Agent.PendingEnrolmentID

		pin2, _ := enrol(e, "host-a")
		Expect(pin2).NotTo(Equal(pin1))
		st, sec := loadState(e, "host-a")
		Expect(st.Current.Record.EnrolmentID).To(Equal(second))
		Expect(st.Current.KeyPins).To(Equal([]string{pin2}), "the old key is no longer pinned")
		Expect(st.Current.mintEpoch()).To(Equal(uint64(2)))
		Expect(st.PredecessorEnrolmentIDs).To(Equal([]string{first}))
		Expect(st.PreviousKeyPins).To(Equal([]string{pin1}))
		Expect(st.RetiredEnrolmentIDs).To(ContainElements(first, second), "neither id is ever drawn again")
		Expect(st.Rekey).To(BeNil(), "the re-key closes its window")
		Expect(string(sec.Data[hostEnrolmentStateKey])).NotTo(ContainSubstring("PRIVATE KEY"))

		// The audit event names the requester and identifiers only.
		var rekeyed string
		for len(rec.Events) > 0 {
			if ev := <-rec.Events; strings.Contains(ev, eventReasonAgentRekeyed) {
				rekeyed = ev
			}
		}
		Expect(rekeyed).To(ContainSubstring("by alice"))
		Expect(rekeyed).To(ContainSubstring(second))
		Expect(rekeyed).To(ContainSubstring(first))
		Expect(rekeyed).NotTo(ContainSubstring("BEGIN"))

		// The grant source names the new enrolment at its record's epoch,
		// and keeps the replaced one as a predecessor.
		src := &PinsHostTrust{Client: k8sClient, Config: e.cfg, Epoch: operation.Epoch{Counter: baseMintEpochCounter, Holder: e.cfg.InstallID}}
		trust, err := src.HostTrust(ctx, e.provider, "host-a")
		Expect(err).NotTo(HaveOccurred())
		Expect(hex.EncodeToString(trust.Enrolment.EnrolmentID)).To(Equal(second))
		Expect(trust.Enrolment.Epoch.Counter).To(Equal(uint64(2)))
		Expect(trust.Predecessors).To(HaveLen(1))
		Expect(hex.EncodeToString(trust.Predecessors[0])).To(Equal(first))
		if devgate.Compiled {
			creds, reason := resolveHostAgentTLS(ctx, k8sClient, e.cfg, h, time.Now())
			Expect(reason).To(BeEmpty())
			Expect(creds.AgentTLS.KeyPins).To(Equal([]string{pin2}), "the inventory renders the new key only")
		}

		// The window closed with the re-key: no new record until another
		// request.
		Expect(reconcileHost(e, "host-a").Status.Agent.PendingEnrolmentID).To(BeEmpty())
		rekey(e, "host-a")
		st, _ = loadState(e, "host-a")
		Expect(st.Current.mintEpoch()).To(Equal(uint64(3)))
		Expect(st.PredecessorEnrolmentIDs).To(Equal([]string{first, second}))
	})

	It("refuses a re-key without the rekey verb, or by anyone else, and keeps the pins", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		pin, _ := enrol(e, "host-a")
		h := requestRekey(e, "host-a")
		notAdmin := adminOpts()
		notAdmin.username, notAdmin.groups = "bob", []string{"system:authenticated"}
		expectDenied(reconcileCR(e, submit(e, "host-a", csrFor(h, nil), notAdmin)), hostidentity.ReasonRequesterNotAuthorised)
		noGrant := adminOpts()
		noGrant.username = "carol"
		expectDenied(reconcileCR(e, submit(e, "host-a", csrFor(h, nil), noGrant)), reasonEnrolNotAllowedForHost)
		enrolOnly := adminOpts()
		enrolOnly.username = "dave"
		expectDenied(reconcileCR(e, submit(e, "host-a", csrFor(h, nil), enrolOnly)), reasonEnrolNotAllowedToRekey)
		st, _ := loadState(e, "host-a")
		Expect(st.Current.KeyPins).To(Equal([]string{pin}))
		Expect(st.PredecessorEnrolmentIDs).To(BeEmpty())

		// The enrol verb alone still enrols a Host that is not enrolled.
		createHost(e, "host-b", "serial-0002", "ch")
		hb := reconcileHost(e, "host-b")
		cr := reconcileCR(e, submit(e, "host-b", csrFor(hb, nil), enrolOnly))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
	})

	It("opens a re-key window only on a fresh request, and closes it after an hour", func() {
		e := newEnv()
		rec := record.NewFakeRecorder(64)
		e.hostR.Recorder = rec
		createHost(e, "host-a", "serial-0001", "ch")
		enrol(e, "host-a")
		h := reconcileHost(e, "host-a")
		// No request: a re-key request is denied.
		stale := hostidentitytest.NewCSR(GinkgoT(), hostidentitytest.NewKey(GinkgoT()), hostidentitytest.CSROptions{})
		expectDenied(reconcileCR(e, submit(e, "host-a", stale, adminOpts())), reasonEnrolNoRekeyWindow)

		// A request older than the window opens nothing.
		h.Annotations = map[string]string{infravirtrigaudiov1beta1.HostRekeyRequestAnnotation: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)}
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		Expect(reconcileHost(e, "host-a").Status.Agent.PendingEnrolmentID).To(BeEmpty())

		// A fresh request opens a window; past its end the record is retired.
		requested := time.Now().Add(-59 * time.Minute).UTC().Truncate(time.Second)
		h = &infravirtrigaudiov1beta1.Host{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.hostNS, Name: "host-a"}, h)).To(Succeed())
		h.Annotations[infravirtrigaudiov1beta1.HostRekeyRequestAnnotation] = requested.Format(time.RFC3339)
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		h = reconcileHost(e, "host-a")
		pending := h.Status.Agent.PendingEnrolmentID
		Expect(pending).NotTo(BeEmpty())
		st, _ := loadState(e, "host-a")
		Expect(st.Rekey.Until.Time).To(BeTemporally("==", requested.Add(time.Hour)))

		e.hostR.Now = func() time.Time { return requested.Add(time.Hour + time.Second) }
		e.reqR.Now = e.hostR.Now
		h = reconcileHost(e, "host-a")
		Expect(h.Status.Agent.PendingEnrolmentID).To(BeEmpty(), "the expired window's record is retired")
		st, _ = loadState(e, "host-a")
		Expect(st.Rekey).To(BeNil())
		Expect(st.RetiredEnrolmentIDs).To(ContainElement(pending))
		Expect(st.LastRekeyRequest).To(Equal(requested.Format(time.RFC3339)))
		var expired bool
		for len(rec.Events) > 0 {
			if ev := <-rec.Events; strings.Contains(ev, "expired") && strings.Contains(ev, requested.Format(time.RFC3339)) {
				expired = true
			}
		}
		Expect(expired).To(BeTrue(), "the expiry is reported, naming the request")
		// The same request does not open another window.
		Expect(reconcileHost(e, "host-a").Status.Agent.PendingEnrolmentID).To(BeEmpty())
	})

	It("opens no window for a request time too far in the future", func() {
		e := newEnv()
		rec := record.NewFakeRecorder(16)
		e.hostR.Recorder = rec
		createHost(e, "host-a", "serial-0001", "ch")
		enrol(e, "host-a")
		h := &infravirtrigaudiov1beta1.Host{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.hostNS, Name: "host-a"}, h)).To(Succeed())
		h.Annotations = map[string]string{infravirtrigaudiov1beta1.HostRekeyRequestAnnotation: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)}
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		Expect(reconcileHost(e, "host-a").Status.Agent.PendingEnrolmentID).To(BeEmpty())
		var refused bool
		for len(rec.Events) > 0 {
			if ev := <-rec.Events; strings.Contains(ev, "too old or in the future") {
				refused = true
			}
		}
		Expect(refused).To(BeTrue())
	})

	It("refuses and marks a re-key certificate issued after its window closed", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		pin, _ := enrol(e, "host-a")
		h := requestRekey(e, "host-a")
		csr := csrFor(h, nil)
		cr := reconcileCR(e, submit(e, "host-a", csr, adminOpts()))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		// The window closes (time moves past it) before the issuance; the
		// request reconciler sees the issued certificate first.
		later := time.Now().Add(2 * time.Hour)
		e.reqR.Now = func() time.Time { return later }
		issue(e, cr, csr, hostidentitytest.IssueOptions{})
		cr = reconcileCR(e, cr)
		Expect(certrequest.IsTrue(cr, certrequest.ConditionRefused)).To(BeTrue())
		st, _ := loadState(e, "host-a")
		Expect(st.Current.KeyPins).To(Equal([]string{pin}))
		Expect(st.LastRejection).NotTo(BeNil())
		Expect(st.LastRejection.Message).To(ContainSubstring("window closed"))
	})

	It("honours the start-epoch floor only for a fresh Host, and only up to its cap", func() {
		e := newEnv()
		rec := record.NewFakeRecorder(32)
		e.hostR.Recorder = rec
		// Above the cap: ignored and reported.
		h := createHost(e, "host-a", "serial-0001", "ch")
		h.Annotations = map[string]string{hostidentity.AnnotationStartEpochFloor: "70000"}
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		h = reconcileHost(e, "host-a")
		Expect(h.Status.Agent.RequestURIs).To(HaveLen(5), "an epoch-1 record")
		var invalid bool
		for len(rec.Events) > 0 {
			if ev := <-rec.Events; strings.Contains(ev, "ignored") {
				invalid = true
			}
		}
		Expect(invalid).To(BeTrue())
		// Enrolled: a floor set during a re-key window does not raise the
		// re-key record's epoch.
		enrol(e, "host-a")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), h)).To(Succeed())
		h.Annotations[hostidentity.AnnotationStartEpochFloor] = "100"
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		h = requestRekey(e, "host-a")
		Expect(h.Status.Agent.RequestURIs).To(ContainElement(hostidentity.URIEpochPrefix+"2"), "the floor is ignored once enrolled")
		// A fresh Host within the cap: applied, with an event.
		hb := createHost(e, "host-b", "serial-0002", "ch")
		hb.Annotations = map[string]string{hostidentity.AnnotationStartEpochFloor: "40"}
		Expect(k8sClient.Update(ctx, hb)).To(Succeed())
		hb = reconcileHost(e, "host-b")
		Expect(hb.Status.Agent.RequestURIs).To(ContainElement(hostidentity.URIEpochPrefix+"40"))
		var applied bool
		for len(rec.Events) > 0 {
			if ev := <-rec.Events; strings.Contains(ev, "starts at epoch 40") {
				applied = true
			}
		}
		Expect(applied).To(BeTrue())
	})

	It("refuses a re-key to a key that is or was pinned", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		h := reconcileHost(e, "host-a")
		key1 := hostidentitytest.NewKey(GinkgoT())
		csr1 := hostidentitytest.NewCSR(GinkgoT(), key1, hostidentitytest.CSROptions{URIs: h.Status.Agent.RequestURIs})
		cr := reconcileCR(e, submit(e, "host-a", csr1, adminOpts()))
		issue(e, cr, csr1, hostidentitytest.IssueOptions{})
		reconcileCR(e, cr)
		h = requestRekey(e, "host-a")
		same := hostidentitytest.NewCSR(GinkgoT(), key1, hostidentitytest.CSROptions{URIs: h.Status.Agent.RequestURIs})
		expectDenied(reconcileCR(e, submit(e, "host-a", same, adminOpts())), reasonEnrolKeyPreviouslyPinned)
		rekey(e, "host-a") // a new key: re-keyed
		h = requestRekey(e, "host-a")
		back := hostidentitytest.NewCSR(GinkgoT(), key1, hostidentitytest.CSROptions{URIs: h.Status.Agent.RequestURIs})
		expectDenied(reconcileCR(e, submit(e, "host-a", back, adminOpts())), reasonEnrolKeyPreviouslyPinned)
	})

	It("refuses a re-key that would change the Provider; the Host is reported and not re-enrolled", func() {
		e := newEnv()
		other := envtestProvider(e.hostNS, "ch2", nil)
		other.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
		other.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
		createHost(e, "host-a", "serial-0001", "ch")
		pin, _ := enrol(e, "host-a")
		h := requestRekey(e, "host-a")
		staleURIs := h.Status.Agent.RequestURIs

		h.Spec.ProviderRef.Name = "ch2"
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		h = reconcileHost(e, "host-a")
		Expect(meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).
			To(Equal(reasonEnrolProviderChanged))
		Expect(h.Status.Agent.PendingEnrolmentID).To(BeEmpty(), "no record is drawn for another Provider")
		csr := hostidentitytest.NewCSR(GinkgoT(), hostidentitytest.NewKey(GinkgoT()), hostidentitytest.CSROptions{URIs: staleURIs})
		expectDenied(reconcileCR(e, submit(e, "host-a", csr, adminOpts())), reasonEnrolProviderChanged)
		st, _ := loadState(e, "host-a")
		Expect(st.Current.KeyPins).To(Equal([]string{pin}))
		Expect(st.PredecessorEnrolmentIDs).To(BeEmpty())
		_, err := (&PinsHostTrust{Client: k8sClient, Config: e.cfg, Epoch: operation.Epoch{Counter: 1, Holder: e.cfg.InstallID}}).
			HostTrust(ctx, other, "host-a")
		Expect(err).To(MatchError(operation.ErrNoEnrolment), "the new Provider never trusts the old enrolment")
	})

	It("re-checks at issuance: a Provider change or a full predecessor list after approval refuses the certificate and marks the request", func() {
		for _, c := range []string{"provider", "bound"} {
			e := newEnv()
			other := envtestProvider(e.hostNS, "ch2", nil)
			other.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
			other.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			createHost(e, "host-a", "serial-0001", "ch")
			pin, _ := enrol(e, "host-a")
			h := requestRekey(e, "host-a")
			csr := csrFor(h, nil)
			cr := reconcileCR(e, submit(e, "host-a", csr, adminOpts()))
			Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue(), c)
			switch c {
			case "provider":
				h.Spec.ProviderRef.Name = "ch2"
				Expect(k8sClient.Update(ctx, h)).To(Succeed())
			case "bound":
				l, err := loadHostEnrolment(ctx, k8sClient, e.cfg, h)
				Expect(err).NotTo(HaveOccurred())
				for i := 0; i < maxPredecessorEnrolmentIDs; i++ {
					l.state.PredecessorEnrolmentIDs = append(l.state.PredecessorEnrolmentIDs, fmt.Sprintf("%032x", i+1))
				}
				Expect(saveHostEnrolment(ctx, k8sClient, e.cfg, h, &l)).To(Succeed())
			}
			issue(e, cr, csr, hostidentitytest.IssueOptions{})
			cr = reconcileCR(e, cr)
			st, _ := loadState(e, "host-a")
			Expect(st.Current.KeyPins).To(Equal([]string{pin}), c)
			Expect(st.LastRejection).NotTo(BeNil(), c)
			Expect(certrequest.IsTrue(cr, certrequest.ConditionRefused)).To(BeTrue(), c+": the refused certificate is marked on the request")
			Expect(st.LastIssuedEpoch).To(Equal(uint64(2)), c+": its epoch is never drawn again")
		}
	})

	It("refuses a second re-key request while one is authorised", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		enrol(e, "host-a")
		h := requestRekey(e, "host-a")
		cr1 := reconcileCR(e, submit(e, "host-a", csrFor(h, nil), adminOpts()))
		Expect(certrequest.IsTrue(cr1, certrequest.ConditionApproved)).To(BeTrue())
		expectDenied(reconcileCR(e, submit(e, "host-a", csrFor(h, nil), adminOpts())), reasonEnrolInFlight)
	})

	It("re-keys under a new installation id; the old enrolment is not trusted meanwhile", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		enrol(e, "host-a")
		e.cfg.InstallID = bytes.Repeat([]byte{0x43}, 16)
		src := &PinsHostTrust{Client: k8sClient, Config: e.cfg, Epoch: operation.Epoch{Counter: 1, Holder: e.cfg.InstallID}}
		_, err := src.HostTrust(ctx, e.provider, "host-a")
		Expect(err).To(MatchError(operation.ErrNoEnrolment), "an enrolment of another installation is not trusted")
		h := requestRekey(e, "host-a")
		Expect(h.Status.Agent.RequestURIs).To(ContainElement(hostidentity.URIInstallPrefix + hex.EncodeToString(e.cfg.InstallID)))
		enrol(e, "host-a")
		trust, err := src.HostTrust(ctx, e.provider, "host-a")
		Expect(err).NotTo(HaveOccurred())
		Expect(trust.Enrolment.Epoch.Counter).To(Equal(uint64(2)))
	})

	It("refuses a replay of an earlier enrolment request after a re-key", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		h := reconcileHost(e, "host-a")
		key := hostidentitytest.NewKey(GinkgoT())
		oldCSR := hostidentitytest.NewCSR(GinkgoT(), key, hostidentitytest.CSROptions{CommonName: "host-a", URIs: h.Status.Agent.RequestURIs})
		oldCR := reconcileCR(e, submit(e, "host-a", oldCSR, adminOpts()))
		issue(e, oldCR, oldCSR, hostidentitytest.IssueOptions{})
		oldCR = reconcileCR(e, oldCR)
		pin2 := rekey(e, "host-a")
		st, _ := loadState(e, "host-a")

		// The old, completed request is reconciled again: the pins do not
		// change, and its certificate, replaced, is marked never to install.
		oldCR = reconcileCR(e, oldCR)
		Expect(certrequest.IsTrue(oldCR, certrequest.ConditionRefused)).To(BeTrue())
		// The old request sent again (same CSR, the replaced record), while
		// a new window is open: denied.
		Expect(requestRekey(e, "host-a").Status.Agent.PendingEnrolmentID).NotTo(BeEmpty())
		expectDenied(reconcileCR(e, submit(e, "host-a", oldCSR, adminOpts())), hostidentity.ReasonSubjectAltNames)
		after, _ := loadState(e, "host-a")
		Expect(after.Current).To(Equal(st.Current))
		Expect(after.Current.KeyPins).To(Equal([]string{pin2}))
		Expect(after.PredecessorEnrolmentIDs).To(Equal(st.PredecessorEnrolmentIDs))
	})

	It("reaches the predecessor bound through re-keys, then refuses, dropping none", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		pin, _ := enrol(e, "host-a")
		for i := 0; i < maxPredecessorEnrolmentIDs; i++ {
			pin = rekey(e, "host-a")
		}
		st, _ := loadState(e, "host-a")
		Expect(st.PredecessorEnrolmentIDs).To(HaveLen(maxPredecessorEnrolmentIDs))
		Expect(st.Current.mintEpoch()).To(Equal(uint64(maxPredecessorEnrolmentIDs + 1)))
		h := requestRekey(e, "host-a")
		expectDenied(reconcileCR(e, submit(e, "host-a", csrFor(h, nil), adminOpts())), reasonEnrolTooManyPredecessors)
		st, _ = loadState(e, "host-a")
		Expect(st.Current.KeyPins).To(Equal([]string{pin}))
		Expect(st.PredecessorEnrolmentIDs).To(HaveLen(maxPredecessorEnrolmentIDs))
	})

	It("draws a re-created Host's first record at epoch 1, raised by the start-epoch floor annotation", func() {
		e := newEnv()
		h := createHost(e, "host-a", "serial-0001", "ch")
		enrol(e, "host-a")
		rekey(e, "host-a") // the host's store is now at epoch 2
		Expect(k8sClient.Delete(ctx, h)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), &infravirtrigaudiov1beta1.Host{}))
		}).Should(BeTrue())
		_, err := e.hostR.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h)})
		Expect(err).NotTo(HaveOccurred())

		createHost(e, "host-a", "serial-0001", "ch")
		h = reconcileHost(e, "host-a")
		Expect(h.Status.Agent.RequestURIs).To(HaveLen(5), "the state is gone: a first record at epoch 1, which the host refuses to install")
		h.Annotations = map[string]string{hostidentity.AnnotationStartEpochFloor: "3"}
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		h = reconcileHost(e, "host-a")
		Expect(h.Status.Agent.RequestURIs).To(ContainElement(hostidentity.URIEpochPrefix+"3"), "the floor raises the record's epoch")
		enrol(e, "host-a")
		st, _ := loadState(e, "host-a")
		Expect(st.Current.mintEpoch()).To(Equal(uint64(3)))
	})

	It("refuses an issued certificate that does not match the authorisation and pins nothing", func() {
		e := newEnv()
		h := reconcileHost(e, createHost(e, "host-a", "serial-0001", "ch").Name)
		csr := csrFor(h, nil)
		cr := reconcileCR(e, submit(e, h.Name, csr, adminOpts()))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		// A certificate for another key (a substituted key).
		issue(e, cr, csr, hostidentitytest.IssueOptions{PublicKey: &hostidentitytest.NewKey(GinkgoT()).PublicKey})
		reconcileCR(e, cr)
		st, _ := loadState(e, h.Name)
		Expect(st.Current).To(BeNil())
		Expect(st.Authorised).To(BeNil())
		Expect(st.Pending).To(BeNil(), "the record of a refused issuance is retired")
		h = reconcileHost(e, h.Name)
		Expect(h.Status.Agent.PendingEnrolmentID).NotTo(BeEmpty(), "a fresh record is drawn")
		Expect(meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).
			To(Equal(infravirtrigaudiov1beta1.HostReasonAgentCertificateRejected))
	})

	It("denies a second request while one is authorised and in flight", func() {
		e := newEnv()
		h := reconcileHost(e, createHost(e, "host-a", "serial-0001", "ch").Name)
		first := reconcileCR(e, submit(e, h.Name, csrFor(h, nil), adminOpts()))
		Expect(certrequest.IsTrue(first, certrequest.ConditionApproved)).To(BeTrue())
		second := reconcileCR(e, submit(e, h.Name, csrFor(h, nil), adminOpts()))
		expectDenied(second, reasonEnrolInFlight)
	})

	It("enrols through the external-PKI path only after validating the returned certificate", func() {
		e := newEnv()
		h := reconcileHost(e, createHost(e, "host-a", "serial-0001", "ch").Name)
		o := adminOpts()
		o.issuer = enrolTestExtIssuer

		// A wrong certificate (another host's SAN) is refused and marked failed.
		bad := csrFor(h, nil)
		cr := reconcileCR(e, submit(e, h.Name, bad, o))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		other := append([]string{"spiffe://cluster.test/virtrigaud/host/" + e.hostNS + "/host-b"}, h.Status.Agent.RequestURIs[1:]...)
		Expect(certrequest.SetCertificate(cr, e.ca.Issue(GinkgoT(), bad, hostidentitytest.IssueOptions{URIs: other}), nil)).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
		cr = reconcileCR(e, cr)
		s, r, _ := certrequest.Condition(cr, certrequest.ConditionReady)
		Expect(s).To(Equal(certrequest.StatusFalse))
		Expect(r).To(Equal(certrequest.ReasonFailed))
		st, _ := loadState(e, h.Name)
		Expect(st.Current).To(BeNil())

		// A matching certificate, built for the freshly drawn record, is
		// pinned and marked issued.
		h = reconcileHost(e, h.Name)
		key := hostidentitytest.NewKey(GinkgoT())
		good := hostidentitytest.NewCSR(GinkgoT(), key, hostidentitytest.CSROptions{URIs: h.Status.Agent.RequestURIs})
		cr = reconcileCR(e, submit(e, h.Name, good, o))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		Expect(certrequest.SetCertificate(cr, e.ca.Issue(GinkgoT(), good, hostidentitytest.IssueOptions{}), nil)).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, cr)).To(Succeed())
		cr = reconcileCR(e, cr)
		Expect(certrequest.IsTrue(cr, certrequest.ConditionReady)).To(BeTrue())
		pin, err := hostidentity.SPKIPin(&key.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		st, _ = loadState(e, h.Name)
		Expect(st.Current).NotTo(BeNil())
		Expect(st.Current.KeyPins).To(Equal([]string{pin}))
	})

	It("restores a lost status from the authoritative pins Secret", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		pin, _ := enrol(e, "host-a")
		h := reconcileHost(e, "host-a")
		Expect(h.Status.Agent.KeyPins).To(Equal([]string{pin}))

		// Status loss (a restore without status, or a status writer wiping it).
		h.Status.Agent = nil
		h.Status.Conditions = nil
		Expect(k8sClient.Status().Update(ctx, h)).To(Succeed())

		h = reconcileHost(e, "host-a")
		Expect(h.Status.Agent).NotTo(BeNil())
		Expect(h.Status.Agent.KeyPins).To(Equal([]string{pin}))
		Expect(meta.IsStatusConditionTrue(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled)).To(BeTrue())

		// A forged status pin is overwritten from the Secret, and the render
		// reads the Secret, never the status.
		h.Status.Agent.KeyPins = []string{"00" + pin[2:]}
		Expect(k8sClient.Status().Update(ctx, h)).To(Succeed())
		h = reconcileHost(e, "host-a")
		Expect(h.Status.Agent.KeyPins).To(Equal([]string{pin}))
		creds, reason := resolveHostAgentTLS(ctx, k8sClient, e.cfg, h, time.Now())
		if reason == "" {
			Expect(creds.AgentTLS.KeyPins).To(Equal([]string{pin}))
		}
	})

	It("does not let a replaced host with a new identity inherit the old pins", func() {
		e := newEnv()
		old := createHost(e, "host-a", "serial-0001", "ch")
		oldPin, _ := enrol(e, "host-a")
		oldState, _ := loadState(e, "host-a")

		// The machine is replaced: the Host is deleted and created again
		// with a new identity before the manager saw the deletion, so the
		// old pins Secret (bound to the old Host UID) is still there.
		Expect(k8sClient.Delete(ctx, old)).To(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(old), &infravirtrigaudiov1beta1.Host{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
		createHost(e, "host-a", "serial-0002", "ch")

		h := reconcileHost(e, "host-a")
		Expect(h.Status.Agent.KeyPins).To(BeEmpty())
		Expect(h.Status.Agent.EnrolmentID).To(BeEmpty())
		Expect(h.Status.Agent.PendingEnrolmentID).NotTo(Equal(oldState.Current.Record.EnrolmentID))
		st, sec := loadState(e, "host-a")
		Expect(st.Current).To(BeNil())
		Expect(st.HostUID).To(Equal(string(h.UID)))
		Expect(st.Identity).To(Equal("serial-0002"))
		Expect(sec.Data[hostEnrolmentStateKey]).NotTo(ContainSubstring(oldPin))
		_, reason := resolveHostAgentTLS(ctx, k8sClient, e.cfg, h, time.Now())
		Expect(reason).NotTo(BeEmpty(), "the replaced host renders no agent credentials")
	})

	It("denies an administrator-group member who may not enrol this Host, without reading or writing its state", func() {
		e := newEnv()
		h := reconcileHost(e, createHost(e, "host-a", "serial-0001", "ch").Name)
		_, before := loadState(e, h.Name)
		o := adminOpts()
		o.username = "carol"
		cr := reconcileCR(e, submit(e, h.Name, csrFor(h, nil), o))
		expectDenied(cr, reasonEnrolNotAllowedForHost)
		_, after := loadState(e, h.Name)
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "no state write for an unauthorised requester")

		// A non-administrator naming a Host that does not exist learns
		// nothing about it: the denial is the requester's.
		o = adminOpts()
		o.username, o.groups = "mallory", []string{"system:authenticated"}
		expectDenied(reconcileCR(e, submit(e, "ghost", csrFor(h, nil), o)), hostidentity.ReasonRequesterNotAuthorised)
		_, after = loadState(e, h.Name)
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
	})

	It("never trusts a tenant-written pins Secret in the Host's namespace", func() {
		e := newEnv()
		h := reconcileHost(e, createHost(e, "host-a", "serial-0001", "ch").Name)
		// A tenant with Secret write access in the Host namespace forges a
		// pins Secret with the right labels, annotation, an owner reference
		// to the Host and a "current" enrolment with its own key pin, under
		// both the hashed name and a guessable one.
		forged, err := json.Marshal(hostEnrolmentState{HostUID: string(h.UID), Identity: h.Spec.Identity, Current: &currentEnrolment{
			Record:   enrolmentRecord{EnrolmentID: strings.Repeat("ab", 16), ProviderUID: string(e.provider.UID), InstallID: strings.Repeat("42", 16)},
			KeyPins:  []string{strings.Repeat("ee", 32)},
			NotAfter: metav1.NewTime(time.Now().Add(time.Hour)),
		}})
		Expect(err).NotTo(HaveOccurred())
		for _, name := range []string{hostEnrolmentSecretName(e.hostNS, h.Name), "virtrigaud-agent-pins-" + h.Name} {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.hostNS,
					Labels:          map[string]string{k8sLabelComponent: hostEnrolmentComponent, k8sLabelManagedBy: k8sManagedByVirtRigaud},
					Annotations:     map[string]string{certrequest.AnnotationHost: e.hostNS + "/" + h.Name},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: infravirtrigaudiov1beta1.GroupVersion.String(), Kind: "Host", Name: h.Name, UID: h.UID}}},
				Data: map[string][]byte{hostEnrolmentStateKey: forged},
			})).To(Succeed())
		}
		_, reason := resolveHostAgentTLS(ctx, k8sClient, e.cfg, h, time.Now())
		if devgate.Compiled {
			Expect(reason).To(Equal("host agent is not enrolled"))
		} else {
			Expect(reason).NotTo(BeEmpty())
		}
		h = reconcileHost(e, h.Name)
		Expect(h.Status.Agent.KeyPins).To(BeEmpty())
		st, _ := loadState(e, h.Name)
		Expect(st.Current).To(BeNil())
	})

	It("renders the configured CA bundle and a recomputed SAN, whatever the pins Secret holds", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		pin, _ := enrol(e, "host-a")
		h := reconcileHost(e, "host-a")
		creds, reason := resolveHostAgentTLS(ctx, k8sClient, e.cfg, h, time.Now())
		if !devgate.Compiled {
			Expect(reason).NotTo(BeEmpty())
			return
		}
		Expect(reason).To(BeEmpty())
		Expect(creds.AgentTLS.CABundle).To(Equal(e.ca.PEM))
		Expect(creds.AgentTLS.ExpectedSAN).To(Equal("spiffe://cluster.test/virtrigaud/host/" + e.hostNS + "/host-a"))
		Expect(creds.AgentTLS.KeyPins).To(Equal([]string{pin}))
		_, reason = resolveHostAgentTLS(ctx, k8sClient, e.cfg, h, time.Now().Add(31*24*time.Hour))
		Expect(reason).To(Equal("host agent certificate expired"), "an expired enrolment is not rendered")
		e.hostR.Now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
		h = reconcileHost(e, "host-a")
		Expect(meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).
			To(Equal(infravirtrigaudiov1beta1.HostReasonAgentCertificateExpired))
	})

	It("retires the enrolment of a revoked Host, removes its pins, and refuses un-revoking (CEL)", func() {
		e := newEnv()
		createHost(e, "host-a", "serial-0001", "ch")
		pin, _ := enrol(e, "host-a")
		before, _ := loadState(e, "host-a")
		h := reconcileHost(e, "host-a")
		h.Spec.Revoked = true
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		h = reconcileHost(e, "host-a")

		st, sec := loadState(e, "host-a")
		Expect(st.Current).To(BeNil(), "the pins are removed")
		Expect(st.Pending).To(BeNil())
		Expect(st.RetiredEnrolmentIDs).To(ContainElement(before.Current.Record.EnrolmentID))
		Expect(sec.Data[hostEnrolmentStateKey]).NotTo(ContainSubstring(pin))
		Expect(h.Status.Agent.KeyPins).To(BeEmpty())
		_, reason := resolveHostAgentTLS(ctx, k8sClient, e.cfg, h, time.Now())
		Expect(reason).NotTo(BeEmpty(), "a revoked Host's old key is never rendered again")

		h.Spec.Revoked = false
		err := k8sClient.Update(ctx, h)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.revoked cannot be cleared once set"))
		_, reason = resolveHostAgentTLS(ctx, k8sClient, e.cfg, h, time.Now())
		Expect(reason).NotTo(BeEmpty())
	})

	It("draws a fresh record and retires the old one after a rejected or abandoned issuance", func() {
		e := newEnv()
		h := reconcileHost(e, createHost(e, "host-a", "serial-0001", "ch").Name)
		first := h.Status.Agent.PendingEnrolmentID

		// Rejected: a certificate for a substituted key.
		csr := csrFor(h, nil)
		cr := reconcileCR(e, submit(e, h.Name, csr, adminOpts()))
		issue(e, cr, csr, hostidentitytest.IssueOptions{PublicKey: &hostidentitytest.NewKey(GinkgoT()).PublicKey})
		reconcileCR(e, cr)
		h = reconcileHost(e, h.Name)
		second := h.Status.Agent.PendingEnrolmentID
		Expect(second).NotTo(BeEmpty())
		Expect(second).NotTo(Equal(first))
		st, _ := loadState(e, h.Name)
		Expect(st.RetiredEnrolmentIDs).To(ContainElement(first))

		// Abandoned: the authorised request is deleted before issuance.
		cr = reconcileCR(e, submit(e, h.Name, csrFor(h, nil), adminOpts()))
		Expect(certrequest.IsTrue(cr, certrequest.ConditionApproved)).To(BeTrue())
		Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
		h = reconcileHost(e, h.Name)
		third := h.Status.Agent.PendingEnrolmentID
		Expect(third).NotTo(BeEmpty())
		Expect(third).NotTo(Equal(second))
		st, _ = loadState(e, h.Name)
		Expect(st.RetiredEnrolmentIDs).To(ContainElements(first, second))
		Expect(st.Authorised).To(BeNil())
	})

	It("keeps the pins Secret when a gone-Host sweep finds the Host alive", func() {
		e := newEnv()
		h := createHost(e, "host-a", "serial-0001", "ch")
		enrol(e, "host-a")
		// A stale cache reported the Host gone; the live re-read finds it.
		e.hostR.APIReader = k8sClient
		Expect(e.hostR.forgetGoneHost(ctx, client.ObjectKeyFromObject(h))).To(Succeed())
		st, _ := loadState(e, "host-a")
		Expect(st.Current).NotTo(BeNil())

		// A deleting Host whose pins Secret holds another Host UID's state
		// does not delete it.
		other := h.DeepCopy()
		other.UID = "00000000-0000-0000-0000-000000000000"
		Expect(deleteHostEnrolment(ctx, k8sClient, k8sClient, e.cfg, h.Namespace, h.Name, other.UID)).To(Succeed())
		st, _ = loadState(e, "host-a")
		Expect(st.Current).NotTo(BeNil())
	})

	It("decides nothing while the admission check fails, and decides once it passes", func() {
		e := newEnv()
		h := reconcileHost(e, createHost(e, "host-a", "serial-0001", "ch").Name)
		e.reqR.Admission = &AdmissionGate{Reader: k8sClient, Namespace: e.issuerNS}
		cr := submit(e, h.Name, csrFor(h, nil), adminOpts())
		res, err := e.reqR.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		got := certrequest.NewObject()
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), got)).To(Succeed())
		Expect(certrequest.Decided(got)).To(BeFalse(), "no approval while the webhooks are not verified")
		e.reqR.Admission.ok.Store(true)
		Expect(certrequest.IsTrue(reconcileCR(e, cr), certrequest.ConditionApproved)).To(BeTrue())
	})

	It("deletes the pins Secret when its Host is deleted", func() {
		e := newEnv()
		h := createHost(e, "host-a", "serial-0001", "ch")
		enrol(e, "host-a")
		Expect(k8sClient.Delete(ctx, h)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), &infravirtrigaudiov1beta1.Host{}))
		}).Should(BeTrue())
		_, err := e.hostR.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h)})
		Expect(err).NotTo(HaveOccurred())
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: e.issuerNS, Name: hostEnrolmentSecretName(e.hostNS, "host-a")}, &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("keeps spec.identity immutable once set (CEL)", func() {
		e := newEnv()
		h := createHost(e, "host-a", "", "ch")
		h.Spec.Identity = "serial-0001"
		Expect(k8sClient.Update(ctx, h)).To(Succeed(), "setting the identity once is allowed")

		h.Spec.Identity = "serial-0002"
		err := k8sClient.Update(ctx, h)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.identity is immutable once set"))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), h)).To(Succeed())
		h.Spec.Identity = ""
		err = k8sClient.Update(ctx, h)
		Expect(err).To(HaveOccurred(), "removing the identity is refused too")
		Expect(err.Error()).To(ContainSubstring("spec.identity is immutable once set"))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), h)).To(Succeed())
		h.Spec.Schedulable = false
		Expect(k8sClient.Update(ctx, h)).To(Succeed(), "other fields stay mutable")
	})
})
