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
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// These specs check the Host.status.agentInventory schema against a real
// apiserver: its bounds mirror the signed inventory's, so a status the
// manager writes always fits, and anything outside them is refused.
var _ = Describe("Host status agentInventory schema", func() {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("ab", 32)

	newHost := func() *infravirtrigaudiov1beta1.Host {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "host-inventory-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
		h := &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "ch"},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "grpc://h:7443",
				Schedulable: true,
			},
		}
		Expect(k8sClient.Create(ctx, h)).To(Succeed())
		return h
	}

	valid := func() *infravirtrigaudiov1beta1.HostAgentInventoryStatus {
		now := metav1.Now()
		return &infravirtrigaudiov1beta1.HostAgentInventoryStatus{
			ObservedTime: &now, AnswerSHA256: strings.Repeat("0", 64), AllowListSHA256: strings.Repeat("1", 64),
			InventoryVersion: 1, SupportedVersion: 1, State: "Complete", SnapshotAgeSeconds: 4294967295, ProcessIdentityTier: "PidfsHandle",
			Components: []infravirtrigaudiov1beta1.HostInventoryComponent{{
				Name: "cloud-hypervisor", State: "Ready", ObservedVersion: "1.0.0", DeclaredVersion: "1.0.0", Source: "sysext",
				ManifestOrigin: "ReadOnly", ArtifactDigest: digest, AllowList: infravirtrigaudiov1beta1.HostInventoryAllowListAllowed,
				Files: []infravirtrigaudiov1beta1.HostInventoryFile{{Name: "cloud-hypervisor", Digest: digest}},
			}},
			Features:             []infravirtrigaudiov1beta1.HostInventoryFeature{{Name: "kvm", Result: "Present"}},
			UnexpectedExtensions: []infravirtrigaudiov1beta1.HostInventoryExtension{{Name: "x.raw", Kind: "UndeclaredImage", ImageDigest: digest}},
		}
	}

	write := func(h *infravirtrigaudiov1beta1.Host, inv *infravirtrigaudiov1beta1.HostAgentInventoryStatus) error {
		h.Status.AgentInventory = inv
		return k8sClient.Status().Update(ctx, h)
	}

	It("accepts an inventory at its bounds", func() {
		h := newHost()
		inv := valid()
		for i := len(inv.Components); i < 32; i++ {
			c := inv.Components[0]
			c.Name = fmt.Sprintf("c%02d", i)
			c.Files = nil
			for k := 0; k < 8; k++ {
				c.Files = append(c.Files, infravirtrigaudiov1beta1.HostInventoryFile{Name: fmt.Sprintf("f%d", k), Digest: digest})
			}
			inv.Components = append(inv.Components, c)
		}
		Expect(write(h, inv)).To(Succeed())
	})

	DescribeTable("refuses an inventory outside its bounds",
		func(edit func(*infravirtrigaudiov1beta1.HostAgentInventoryStatus)) {
			h := newHost()
			inv := valid()
			edit(inv)
			Expect(write(h, inv)).NotTo(Succeed())
		},
		Entry("33 components", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			for k := 0; k < 33; k++ {
				i.Components = append(i.Components, i.Components[0])
			}
		}),
		Entry("9 files", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			for k := 0; k < 9; k++ {
				i.Components[0].Files = append(i.Components[0].Files, i.Components[0].Files[0])
			}
		}),
		Entry("33 features", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			for k := 0; k < 33; k++ {
				i.Features = append(i.Features, i.Features[0])
			}
		}),
		Entry("17 extensions", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			for k := 0; k < 17; k++ {
				i.UnexpectedExtensions = append(i.UnexpectedExtensions, i.UnexpectedExtensions[0])
			}
		}),
		Entry("a file name that is a path", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			i.Components[0].Files[0].Name = "/usr/bin/ch"
		}),
		Entry("a digest in another form", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			i.Components[0].ArtifactDigest = "md5:00"
		}),
		Entry("an uppercase component name", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			i.Components[0].Name = "CH"
		}),
		Entry("an unknown state", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) { i.State = "Done" }),
		Entry("an unknown allow-list result", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			i.Components[0].AllowList = "Maybe"
		}),
		Entry("a reason with spaces", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			i.Components[0].Reason = "it broke"
		}),
		Entry("a version too long", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			i.Components[0].ObservedVersion = "1" + strings.Repeat("0", 64)
		}),
		Entry("an age above 32 bits", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			i.SnapshotAgeSeconds = 1 << 32
		}),
		Entry("a malformed answer digest", func(i *infravirtrigaudiov1beta1.HostAgentInventoryStatus) {
			i.AnswerSHA256 = "xyz"
		}),
	)
})
