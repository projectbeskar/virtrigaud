//go:build virtrigaud_dev

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
	"context"
	"encoding/hex"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostinventory"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/fake"
)

// These specs run the Host controller's heartbeat against the clustered fake
// in operation-identity mode, over the real gRPC client and a real
// apiserver: the signed host answer carries the inventory when the fake's
// hosts report one, and the host is Ready only then.
var _ = Describe("Host inventory over the signed host answer", func() {
	ctx := context.Background()

	fakeAllowList := func() *hostinventory.AllowList {
		a, err := hostinventory.Parse([]byte(fmt.Sprintf(`{"version":1,"required":[%q],"components":[{"name":%q,"version":%q,
			"alternatives":[{"artifactDigest":"sha256:%s","files":[{"name":%q,"digest":"sha256:%s"}]}]}]}`,
			fake.FakeInventoryComponent, fake.FakeInventoryComponent, fake.FakeInventoryVersion,
			hex.EncodeToString(fake.FakeInventoryArtifactSHA256[:]), fake.FakeInventoryFile, hex.EncodeToString(fake.FakeInventoryFileSHA256[:]))))
		Expect(err).NotTo(HaveOccurred())
		return a
	}

	heartbeat := func(e *identityEnv, policy *HostInventoryPolicy) *infravirtrigaudiov1beta1.Host {
		answers := &HostAnswers{Trust: e.trust, Inventory: policy}
		r := &HostReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RemoteResolver: &stubResolver{provider: e.client},
			HostAnswers: answers}
		key := types.NamespacedName{Namespace: e.ns, Name: "host-a"}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		h := &infravirtrigaudiov1beta1.Host{}
		Expect(k8sClient.Get(ctx, key, h)).To(Succeed())
		return h
	}

	condOf := func(h *infravirtrigaudiov1beta1.Host, typ string) *metav1.Condition {
		c := meta.FindStatusCondition(h.Status.Conditions, typ)
		Expect(c).NotTo(BeNil(), typ)
		return c
	}

	It("makes a host whose agent reports an allow-listed inventory Ready", func() {
		e := newIdentityEnv(ctx, fake.WithHostInventory())
		p := DefaultHostInventoryPolicy()
		p.AllowList = fakeAllowList()
		h := heartbeat(e, p)
		Expect(condOf(h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified).Status).To(Equal(metav1.ConditionTrue))
		c := condOf(h, infravirtrigaudiov1beta1.HostConditionComponentsReady)
		Expect(c.Reason).To(Equal(infravirtrigaudiov1beta1.HostReasonComponentsVerified), c.Message)
		Expect(h.Status.Health).To(Equal(infravirtrigaudiov1beta1.HostHealthReady))
		Expect(h.Status.AgentInventory).NotTo(BeNil())
		Expect(h.Status.AgentInventory.Components).To(HaveLen(1))
		Expect(h.Status.AgentInventory.Components[0].AllowList).To(Equal(infravirtrigaudiov1beta1.HostInventoryAllowListAllowed))
	})

	It("keeps a host whose agent reports no inventory, or no allow-list is configured, not Ready", func() {
		e := newIdentityEnv(ctx)
		p := DefaultHostInventoryPolicy()
		p.AllowList = fakeAllowList()
		h := heartbeat(e, p)
		Expect(condOf(h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified).Reason).To(Equal(infravirtrigaudiov1beta1.HostReasonInventoryMissing))
		Expect(condOf(h, HostConditionAgentAnswer).Status).To(Equal(metav1.ConditionTrue), "the answer itself verified")
		Expect(h.Status.Health).To(Equal(infravirtrigaudiov1beta1.HostHealthNotReady))

		e2 := newIdentityEnv(ctx, fake.WithHostInventory())
		h = heartbeat(e2, DefaultHostInventoryPolicy())
		Expect(condOf(h, infravirtrigaudiov1beta1.HostConditionComponentsReady).Reason).To(
			Equal(infravirtrigaudiov1beta1.HostReasonComponentsAllowListNotConfigured))
		Expect(h.Status.Health).To(Equal(infravirtrigaudiov1beta1.HostHealthNotReady))
	})
})
