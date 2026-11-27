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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// These specs check that the enrolment reconciler draws no enrolment record
// for a Host whose spec.identity another Host that is not revoked carries,
// in any namespace, and draws one once only one does.
var _ = Describe("Unique host identity at enrolment (envtest)", func() {
	ctx := context.Background()

	var (
		hostNS, otherNS, issuerNS string
		hostR                     *HostEnrolmentReconciler
		rec                       *record.FakeRecorder
		identity                  string
	)

	BeforeEach(func() {
		for _, p := range []string{"uniq-hosts-", "uniq-other-", "uniq-issuer-"} {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: p}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			switch p {
			case "uniq-hosts-":
				hostNS = ns.Name
			case "uniq-other-":
				otherNS = ns.Name
			default:
				issuerNS = ns.Name
			}
		}
		for _, ns := range []string{hostNS, otherNS} {
			prov := envtestProvider(ns, "ch", nil)
			prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
			prov.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
			Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		}
		ca := hostidentitytest.NewConstrainedCA(GinkgoT(), enrolTestTrustDomain)
		cm := enrolTestCMIssuer
		cfg := &HostEnrolmentConfig{Namespace: issuerNS, CertManagerIssuer: &cm, AdminGroup: enrolTestAdminGroup,
			TrustDomain: enrolTestTrustDomain, CABundle: ca.PEM, InstallID: bytes.Repeat([]byte{0x42}, 16)}
		rec = record.NewFakeRecorder(64)
		hostR = &HostEnrolmentReconciler{Client: k8sClient, APIReader: k8sClient, Config: cfg, Recorder: rec}
		var salt [4]byte
		_, err := rand.Read(salt[:])
		Expect(err).NotTo(HaveOccurred())
		identity = "machine-" + hex.EncodeToString(salt[:])
	})

	newHost := func(ns, name string) *infravirtrigaudiov1beta1.Host {
		h := &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "ch"},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "grpc://" + name + ":7443", Identity: identity, Schedulable: true,
			},
		}
		Expect(k8sClient.Create(ctx, h)).To(Succeed())
		return h
	}

	reconcile := func(h *infravirtrigaudiov1beta1.Host) (*infravirtrigaudiov1beta1.Host, *hostEnrolmentState) {
		GinkgoHelper()
		_, err := hostR.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h)})
		Expect(err).NotTo(HaveOccurred())
		got := &infravirtrigaudiov1beta1.Host{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), got)).To(Succeed())
		l, err := loadHostEnrolment(ctx, k8sClient, hostR.Config, got)
		Expect(err).NotTo(HaveOccurred())
		return got, &l.state
	}

	newHostWith := func(ns, name, ident, provider string) *infravirtrigaudiov1beta1.Host {
		h := &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: provider},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "grpc://" + name + ":7443", Identity: ident, Schedulable: true,
			},
		}
		Expect(k8sClient.Create(ctx, h)).To(Succeed())
		return h
	}

	condOf := func(h *infravirtrigaudiov1beta1.Host, typ string) *metav1.Condition {
		return meta.FindStatusCondition(h.Status.Conditions, typ)
	}

	It("refuses a claimant generically, lets the first holder keep its enrolment, and ignores case and other Provider types", func() {
		a := newHost(hostNS, "a")
		b := newHostWith(otherNS, "b", strings.ToUpper(identity), "ch")
		for _, h := range []*infravirtrigaudiov1beta1.Host{a, b} {
			got, st := reconcile(h)
			Expect(st.Pending).To(BeNil(), "no record for a duplicated identity (compared without case)")
			c := condOf(got, infravirtrigaudiov1beta1.HostConditionAgentEnrolled)
			Expect(c).NotTo(BeNil())
			Expect(c.Reason).To(Equal(reasonEnrolRefused))
			Expect(strings.ToLower(c.Message)).NotTo(ContainSubstring(identity), "the identity is not echoed")
			Expect(c.Message).NotTo(ContainSubstring("in use"), "a claimant learns nothing about the identity's use")
			Expect(condOf(got, HostConditionAgentIdentityDuplicated)).To(BeNil(), "not shown to a Host that holds nothing")
		}
		for len(rec.Events) > 0 {
			e := <-rec.Events
			Expect(e).NotTo(ContainSubstring(otherNS), "the other Host is not named")
			Expect(strings.ToLower(e)).NotTo(ContainSubstring(identity))
		}

		// A Host of a Provider that runs no per-host agent does not count.
		lv := envtestProvider(otherNS, "lv", nil)
		Expect(k8sClient.Create(ctx, lv)).To(Succeed())
		newHostWith(otherNS, "libvirt-host", identity, "lv")

		// Revoking one lets the other enrol.
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b), b)).To(Succeed())
		b.Spec.Revoked = true
		Expect(k8sClient.Update(ctx, b)).To(Succeed())
		got, st := reconcile(a)
		Expect(st.Pending).NotTo(BeNil())
		Expect(condOf(got, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).To(Equal(infravirtrigaudiov1beta1.HostReasonAgentAwaitingRequest))

		// a now holds an enrolment: a later claimant never blocks it, and
		// a shows the duplicate.
		prov := &infravirtrigaudiov1beta1.Provider{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: hostNS, Name: "ch"}, prov)).To(Succeed())
		l, err := loadHostEnrolment(ctx, k8sClient, hostR.Config, got)
		Expect(err).NotTo(HaveOccurred())
		l.state.Current = &currentEnrolment{Record: *l.state.Pending, KeyPins: []string{strings.Repeat("a", 64)},
			NotAfter: metav1.NewTime(time.Now().Add(time.Hour))}
		l.state.Pending = nil
		Expect(saveHostEnrolment(ctx, k8sClient, hostR.Config, got, &l)).To(Succeed())
		d := newHostWith(otherNS, "d", identity, "ch")
		_, st = reconcile(d)
		Expect(st.Pending).To(BeNil(), "the claimant is refused")
		got, st = reconcile(a)
		Expect(st.Current).NotTo(BeNil(), "the holder keeps its enrolment")
		Expect(condOf(got, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).To(Equal(infravirtrigaudiov1beta1.HostReasonAgentEnrolled))
		dup := condOf(got, HostConditionAgentIdentityDuplicated)
		Expect(dup).NotTo(BeNil())
		Expect(dup.Status).To(Equal(metav1.ConditionTrue))
		Expect(dup.Message).NotTo(ContainSubstring(otherNS))
		Expect(strings.ToLower(dup.Message)).NotTo(ContainSubstring(identity))

		others, err := hostsWithIdentity(ctx, k8sClient, nil, got)
		Expect(err).NotTo(HaveOccurred())
		Expect(others).To(HaveLen(1), "only d counts: b is revoked and the libvirt Host runs no agent")

		// The claimant goes: the condition clears.
		Expect(k8sClient.Delete(ctx, d)).To(Succeed())
		got, _ = reconcile(a)
		Expect(condOf(got, HostConditionAgentIdentityDuplicated).Status).To(Equal(metav1.ConditionFalse))
	})

	It("decides a draw on a live list, not on a cache index that lags", func() {
		a := newHost(hostNS, "a")
		newHostWith(otherNS, "b", identity, "ch")
		hostR.identityReader = staleIdentityCache{Reader: k8sClient}
		got, st := reconcile(a)
		Expect(st.Pending).To(BeNil(), "the live list sees the claimant the cache does not")
		Expect(condOf(got, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).To(Equal(reasonEnrolRefused))
	})
})

// staleIdentityCache is a cache that has not seen any Host yet: its lists
// are empty.
type staleIdentityCache struct{ client.Reader }

// List implements client.Reader with an empty result.
func (staleIdentityCache) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}
