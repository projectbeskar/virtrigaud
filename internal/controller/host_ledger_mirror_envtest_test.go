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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// These specs check the schema of the reserved ledger fields against a real
// apiserver: Host.status.agentLedger and agentLastStore (display mirrors that
// never carry a full store id or a chain head), and the operation attempt's
// Accepted phase, receipt and ledger generation. They are optional: objects
// written without them, as every current writer does, are unchanged.
var _ = Describe("Reserved host ledger status schema", func() {
	ctx := context.Background()

	newNamespace := func(prefix string) string {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
		return ns.Name
	}

	Context("Host.status.agentLedger and agentLastStore", func() {
		newHost := func() *infravirtrigaudiov1beta1.Host {
			h := &infravirtrigaudiov1beta1.Host{
				ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: newNamespace("host-ledger-")},
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

		valid := func() (*infravirtrigaudiov1beta1.HostAgentLedgerStatus, *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
			now := metav1.Now()
			return &infravirtrigaudiov1beta1.HostAgentLedgerStatus{
					StoreIDPrefix: "0a1b2c3d", Seq: 4096, CheckpointSeq: 3072,
					AttestationState: infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested,
					AttestedAt:       &now, LedgerGeneration: 1, UpdatedAt: &now,
				}, &infravirtrigaudiov1beta1.HostAgentLastStoreStatus{
					StoreIDPrefix: "0a1b2c3d", Seq: 4100, CheckpointSeq: 3072, UpdatedAt: &now,
				}
		}

		write := func(h *infravirtrigaudiov1beta1.Host, l *infravirtrigaudiov1beta1.HostAgentLedgerStatus,
			s *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) error {
			h.Status.AgentLedger, h.Status.AgentLastStore = l, s
			return k8sClient.Status().Update(ctx, h)
		}

		It("accepts both mirrors, round-trips them, and accepts a status without them", func() {
			h := newHost()
			Expect(write(h, nil, nil)).To(Succeed())
			l, s := valid()
			Expect(write(h, l, s)).To(Succeed())
			got := &infravirtrigaudiov1beta1.Host{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: h.Namespace, Name: h.Name}, got)).To(Succeed())
			Expect(got.Status.AgentLedger).NotTo(BeNil())
			Expect(got.Status.AgentLedger.StoreIDPrefix).To(Equal("0a1b2c3d"))
			Expect(got.Status.AgentLedger.AttestationState).To(Equal(infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested))
			Expect(got.Status.AgentLedger.LedgerGeneration).To(Equal(int64(1)))
			Expect(got.Status.AgentLastStore).NotTo(BeNil())
			Expect(got.Status.AgentLastStore.Seq).To(Equal(int64(4100)))
			Expect(got.Status.Agent).To(BeNil(), "outside status.agent")
		})

		It("accepts the Pending attestation state and empty mirrors", func() {
			h := newHost()
			Expect(write(h, &infravirtrigaudiov1beta1.HostAgentLedgerStatus{
				AttestationState: infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending,
			}, &infravirtrigaudiov1beta1.HostAgentLastStoreStatus{})).To(Succeed())
		})

		DescribeTable("refuses a mirror outside its bounds",
			func(edit func(*infravirtrigaudiov1beta1.HostAgentLedgerStatus, *infravirtrigaudiov1beta1.HostAgentLastStoreStatus)) {
				h := newHost()
				l, s := valid()
				edit(l, s)
				Expect(write(h, l, s)).NotTo(Succeed())
			},
			Entry("a full store id in the ledger mirror", func(l *infravirtrigaudiov1beta1.HostAgentLedgerStatus, _ *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				l.StoreIDPrefix = strings.Repeat("0a", 16)
			}),
			Entry("a full store id in the last-store mirror", func(_ *infravirtrigaudiov1beta1.HostAgentLedgerStatus, s *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				s.StoreIDPrefix = strings.Repeat("0a", 16)
			}),
			Entry("a short store id prefix", func(l *infravirtrigaudiov1beta1.HostAgentLedgerStatus, _ *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				l.StoreIDPrefix = "0a1b2c3"
			}),
			Entry("an uppercase store id prefix", func(_ *infravirtrigaudiov1beta1.HostAgentLedgerStatus, s *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				s.StoreIDPrefix = "0A1B2C3D"
			}),
			Entry("an unknown attestation state", func(l *infravirtrigaudiov1beta1.HostAgentLedgerStatus, _ *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				l.AttestationState = "Adopted"
			}),
			Entry("a negative ledger seq", func(l *infravirtrigaudiov1beta1.HostAgentLedgerStatus, _ *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				l.Seq = -1
			}),
			Entry("a negative ledger checkpoint", func(l *infravirtrigaudiov1beta1.HostAgentLedgerStatus, _ *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				l.CheckpointSeq = -1
			}),
			Entry("a negative last-store seq", func(_ *infravirtrigaudiov1beta1.HostAgentLedgerStatus, s *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				s.Seq = -1
			}),
			Entry("a negative last-store checkpoint", func(_ *infravirtrigaudiov1beta1.HostAgentLedgerStatus, s *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				s.CheckpointSeq = -1
			}),
			Entry("a negative ledger generation", func(l *infravirtrigaudiov1beta1.HostAgentLedgerStatus, _ *infravirtrigaudiov1beta1.HostAgentLastStoreStatus) {
				l.LedgerGeneration = -1
			}),
		)
	})

	Context("VirtualMachine.status.operationAttempts", func() {
		newVM := func() *infravirtrigaudiov1beta1.VirtualMachine {
			vm := &infravirtrigaudiov1beta1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: newNamespace("attempt-receipt-")},
				Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "ch"},
					ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
					ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2",
						Source: "manual"},
				},
			}
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			return vm
		}

		valid := func() infravirtrigaudiov1beta1.OperationAttempt {
			return infravirtrigaudiov1beta1.OperationAttempt{
				Kind: infravirtrigaudiov1beta1.OperationAttemptKindCreate, Attempt: 1, OpID: strings.Repeat("ab", 16),
				Phase: infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted, LastTransitionTime: metav1.Now(),
				Receipt: &infravirtrigaudiov1beta1.OperationAttemptReceipt{
					StoreID: strings.Repeat("0a", 16), Seq: 1, Head: strings.Repeat("0b", 32),
					AttestationID: strings.Repeat("0c", 16), ReceiptTag: strings.Repeat("0d", 32),
				},
				LedgerGeneration: 2,
			}
		}

		write := func(vm *infravirtrigaudiov1beta1.VirtualMachine, a infravirtrigaudiov1beta1.OperationAttempt) error {
			vm.Status.OperationAttempts = []infravirtrigaudiov1beta1.OperationAttempt{a}
			return k8sClient.Status().Update(ctx, vm)
		}

		It("accepts an Accepted attempt with a receipt and ledger generation, and round-trips it", func() {
			vm := newVM()
			Expect(write(vm, valid())).To(Succeed())
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Name}, got)).To(Succeed())
			Expect(got.Status.OperationAttempts).To(HaveLen(1))
			a := got.Status.OperationAttempts[0]
			Expect(a.Phase).To(Equal(infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted))
			Expect(a.Receipt).To(Equal(valid().Receipt))
			Expect(a.LedgerGeneration).To(Equal(int64(2)))
		})

		It("accepts the existing phases without a receipt, as every current writer writes them", func() {
			for _, phase := range []infravirtrigaudiov1beta1.OperationAttemptPhase{
				infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, infravirtrigaudiov1beta1.OperationAttemptPhaseSent,
				infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal,
			} {
				vm := newVM()
				a := valid()
				a.Phase, a.Receipt, a.LedgerGeneration = phase, nil, 0
				Expect(write(vm, a)).To(Succeed(), "phase %s", phase)
			}
		})

		DescribeTable("refuses an attempt outside its bounds",
			func(edit func(*infravirtrigaudiov1beta1.OperationAttempt)) {
				vm := newVM()
				a := valid()
				edit(&a)
				Expect(write(vm, a)).NotTo(Succeed())
			},
			Entry("an unknown phase", func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.Phase = "Acknowledged" }),
			Entry("a negative ledger generation", func(a *infravirtrigaudiov1beta1.OperationAttempt) {
				a.LedgerGeneration = -1
			}),
			Entry("a receipt seq of 0", func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.Receipt.Seq = 0 }),
			Entry("a short store id", func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.Receipt.StoreID = "0a1b2c3d" }),
			Entry("an uppercase head", func(a *infravirtrigaudiov1beta1.OperationAttempt) {
				a.Receipt.Head = strings.Repeat("0B", 32)
			}),
			Entry("a long attestation id", func(a *infravirtrigaudiov1beta1.OperationAttempt) {
				a.Receipt.AttestationID = strings.Repeat("0c", 32)
			}),
			Entry("a missing receipt tag", func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.Receipt.ReceiptTag = "" }),
			Entry("a missing head", func(a *infravirtrigaudiov1beta1.OperationAttempt) { a.Receipt.Head = "" }),
		)
	})
})
