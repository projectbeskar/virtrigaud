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
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// These specs run the Host enrolment reconciler against a real apiserver on a
// pins Secret a newer manager wrote: members and data keys it added survive
// this manager's writes, and a downgrade floor above this manager's version
// stops it from writing at all.
var _ = Describe("Per-host state written by a newer manager (envtest)", func() {
	ctx := context.Background()

	var (
		hostNS, issuerNS string
		cfg              *HostEnrolmentConfig
		rec              *record.FakeRecorder
		hostR            *HostEnrolmentReconciler
	)

	BeforeEach(func() {
		prev := buildVersion
		buildVersion = func() string { return "v0.4.2" }
		DeferCleanup(func() { buildVersion = prev })
		for _, p := range []string{"compat-hosts-", "compat-issuer-"} {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: p}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			if p == "compat-hosts-" {
				hostNS = ns.Name
			} else {
				issuerNS = ns.Name
			}
		}
		prov := envtestProvider(hostNS, "ch", nil)
		prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
		prov.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		ca := hostidentitytest.NewConstrainedCA(GinkgoT(), enrolTestTrustDomain)
		cm := enrolTestCMIssuer
		cfg = &HostEnrolmentConfig{Namespace: issuerNS, CertManagerIssuer: &cm, AdminGroup: enrolTestAdminGroup,
			TrustDomain: enrolTestTrustDomain, CABundle: ca.PEM, InstallID: bytes.Repeat([]byte{0x42}, 16)}
		rec = record.NewFakeRecorder(64)
		hostR = &HostEnrolmentReconciler{Client: k8sClient, Config: cfg, Recorder: rec}
	})

	// enrolledHost creates a Host, reconciles it once (its pins Secret holds a
	// pending record) and returns it.
	enrolledHost := func(name string) *infravirtrigaudiov1beta1.Host {
		h := &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hostNS},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "ch"},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "grpc://" + name + ":7443", Identity: "serial-" + name, Schedulable: true,
			},
		}
		Expect(k8sClient.Create(ctx, h)).To(Succeed())
		_, err := hostR.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), h)).To(Succeed())
		return h
	}

	pinsKey := func(name string) types.NamespacedName {
		return types.NamespacedName{Namespace: issuerNS, Name: hostEnrolmentSecretName(hostNS, name)}
	}

	// rewrite plays a newer manager: it adds members to the enrolment state
	// (at the top level and nested in the pending record) and a data key.
	rewrite := func(name string, top map[string]any) *corev1.Secret {
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, pinsKey(name), sec)).To(Succeed())
		doc := map[string]any{}
		Expect(json.Unmarshal(sec.Data[hostEnrolmentStateKey], &doc)).To(Succeed())
		pending, ok := doc["pending"].(map[string]any)
		Expect(ok).To(BeTrue())
		pending["futureRecordMember"] = "nested"
		for k, v := range top {
			doc[k] = v
		}
		b, err := json.Marshal(doc)
		Expect(err).NotTo(HaveOccurred())
		sec.Data[hostEnrolmentStateKey] = b
		sec.Data["future.json"] = []byte(`{"minManagerVersion":"0.1.0","entries":[1]}`)
		Expect(k8sClient.Update(ctx, sec)).To(Succeed())
		return sec
	}

	reconcile := func(h *infravirtrigaudiov1beta1.Host) *infravirtrigaudiov1beta1.Host {
		_, err := hostR.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h)})
		Expect(err).NotTo(HaveOccurred())
		got := &infravirtrigaudiov1beta1.Host{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(h), got)).To(Succeed())
		return got
	}

	drainEvents := func() []string {
		var out []string
		for {
			select {
			case e := <-rec.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	It("keeps the members and data keys a newer manager wrote when the floor allows this manager", func() {
		h := enrolledHost("host-a")
		rewrite("host-a", map[string]any{"minManagerVersion": "0.4.2", "futureTopMember": map[string]any{"a": 1}})
		// Revoking retires the pending record: a write of the enrolment state.
		h.Spec.Revoked = true
		Expect(k8sClient.Update(ctx, h)).To(Succeed())
		h = reconcile(h)
		Expect(meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled).Reason).
			To(Equal(infravirtrigaudiov1beta1.HostReasonAgentRevoked))

		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, pinsKey("host-a"), sec)).To(Succeed())
		Expect(string(sec.Data["future.json"])).To(Equal(`{"minManagerVersion":"0.1.0","entries":[1]}`))
		doc := map[string]any{}
		Expect(json.Unmarshal(sec.Data[hostEnrolmentStateKey], &doc)).To(Succeed())
		Expect(doc).To(HaveKeyWithValue("minManagerVersion", "0.4.2"))
		Expect(doc).To(HaveKeyWithValue("futureTopMember", map[string]any{"a": float64(1)}))
		Expect(doc).NotTo(HaveKey("pending"), "the record was retired")
		Expect(doc["retiredEnrolmentIDs"]).NotTo(BeEmpty())
		Expect(sec.Labels).To(HaveKeyWithValue(k8sLabelComponent, hostEnrolmentComponent))
	})

	It("keeps a nested unknown member of a record it does not change", func() {
		h := enrolledHost("host-b")
		rewrite("host-b", map[string]any{"minManagerVersion": "0.4.0"})
		// A read-modify-write through the manager's own load and save.
		l, err := loadHostEnrolment(ctx, k8sClient, cfg, h)
		Expect(err).NotTo(HaveOccurred())
		l.state.LastIssuedEpoch = 7
		Expect(saveHostEnrolment(ctx, k8sClient, cfg, h, &l)).To(Succeed())
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, pinsKey("host-b"), sec)).To(Succeed())
		Expect(string(sec.Data[hostEnrolmentStateKey])).To(ContainSubstring(`"futureRecordMember":"nested"`))
		Expect(string(sec.Data[hostEnrolmentStateKey])).To(ContainSubstring(`"lastIssuedEpoch":7`))
		Expect(sec.Data).To(HaveKey("future.json"))
	})

	DescribeTable("refuses a Host whose state requires a newer manager, and never writes its pins Secret",
		func(name string, setup func(name string)) {
			h := enrolledHost(name)
			setup(name)
			before := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, pinsKey(name), before)).To(Succeed())
			drainEvents()

			h.Spec.Revoked = true // would retire the pending record
			Expect(k8sClient.Update(ctx, h)).To(Succeed())
			h = reconcile(h)
			cond := meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reasonEnrolManagerTooOld))
			events := drainEvents()
			Expect(events).To(ContainElement(ContainSubstring(reasonEnrolManagerTooOld)))
			// A second visit reports nothing new.
			reconcile(h)
			Expect(drainEvents()).To(BeEmpty())

			after := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, pinsKey(name), after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "the pins Secret is not written")

			// Nor deleted when the Host goes.
			Expect(k8sClient.Delete(ctx, h)).To(Succeed())
			_, err := hostR.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h)})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, pinsKey(name), after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
		},
		Entry("a floor above in the enrolment state", "host-c", func(name string) {
			rewrite(name, map[string]any{"minManagerVersion": "99.0.0"})
		}),
		Entry("a malformed floor", "host-d", func(name string) {
			rewrite(name, map[string]any{"minManagerVersion": "not-a-version"})
		}),
		Entry("a floor above in the store state", "host-e", func(name string) {
			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, pinsKey(name), sec)).To(Succeed())
			sec.Data[hostStoreStateKey] = []byte(`{"minManagerVersion":"1.0.0"}`)
			Expect(k8sClient.Update(ctx, sec)).To(Succeed())
		}),
		Entry("a floor above in a host ledger Secret of the Host", "host-f", func(name string) {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "ledger-" + strings.ToLower(name), Namespace: issuerNS,
					Labels:      map[string]string{LabelHostLedger: "true"},
					Annotations: map[string]string{certrequest.AnnotationHost: hostNS + "/" + name}},
				Data: map[string][]byte{"ledger.json": []byte(`{"minManagerVersion":"2.0.0"}`)},
			})).To(Succeed())
		}),
	)
})
