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
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These specs run the single-host create path against a real API server: a
// VirtualMachine is created with no status (the status strategy drops it), so
// the create marker is its first status write. They pin that such a VM is
// created exactly once, that a marker write losing a resourceVersion race
// sends no Create, and that the post-Create record lands — also on an object
// whose status was never written.

// bumpingReader is an uncached reader that, the first time it returns a
// VirtualMachine, edits the stored object afterwards: the resourceVersion the
// caller read is then stale.
type bumpingReader struct {
	client.Reader
	bumped atomic.Bool
}

func (b *bumpingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := b.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if _, isVM := obj.(*infravirtrigaudiov1beta1.VirtualMachine); isVM && b.bumped.CompareAndSwap(false, true) {
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			fresh := &infravirtrigaudiov1beta1.VirtualMachine{}
			if err := k8sClient.Get(ctx, key, fresh); err != nil {
				return err
			}
			fresh.Labels = map[string]string{"example.com/edited": "during-create"}
			return k8sClient.Update(ctx, fresh)
		})).To(Succeed())
	}
	return nil
}

var _ = Describe("Single-host create marker (envtest)", func() {
	// setup creates a namespace with a single-host Provider (runtime status
	// set), a VMClass and a creatable VirtualMachine with no status, and
	// returns the VM's request and the provider stub.
	setup := func() (ctrl.Request, *routingProvider) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "create-marker-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: prov.Name},
				ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		// The API server outlives the spec, and other specs run managers that
		// reconcile every VirtualMachine: remove this one (and its finalizer)
		// when the spec ends, so it never reaches their providers.
		DeferCleanup(func() {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				got := &infravirtrigaudiov1beta1.VirtualMachine{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), got); err != nil {
					return client.IgnoreNotFound(err)
				}
				got.Finalizers = nil
				return k8sClient.Update(ctx, got)
			})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vm))).To(Succeed())
		})
		stored := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), stored)).To(Succeed())
		Expect(stored.Status.Conditions).To(BeEmpty(), "the API server stores no status at create")
		rp := &routingProvider{onCreate: func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
			return contracts.CreateResponse{ID: req.Owner.Namespace + "." + req.Name}, nil
		}}
		return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}, rp
	}

	reconciler := func(rp *routingProvider, reader client.Reader) *VirtualMachineReconciler {
		return &VirtualMachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: reader,
			RemoteResolver: &stubResolver{provider: rp}, Recorder: record.NewFakeRecorder(100),
		}
	}

	get := func(req ctrl.Request) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, vm)).To(Succeed())
		return vm
	}

	It("creates a fresh VM with no stored status exactly once and records its id", func() {
		req, rp := setup()
		r := reconciler(rp, k8sClient)

		_, err := r.Reconcile(ctx, req) // adds the finalizer
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, req) // marker, Create, record
		Expect(err).NotTo(HaveOccurred())
		Expect(rp.createReqs).To(HaveLen(1))

		vm := get(req)
		Expect(vm.Status.ID).To(Equal(req.Namespace+".web"), "the post-Create record lands")
		Expect(vm.Status.BoundProvider).NotTo(BeNil())
		Expect(createRequested(vm)).To(BeFalse(), "the marker is replaced by the Create's outcome")
		Expect(meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning).Reason).To(Equal(k8s.ReasonReconcileSuccess))

		rp.describeResp = contracts.DescribeResponse{Exists: true, PowerState: "On", IPs: []string{"10.0.0.5"}}
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(rp.createReqs).To(HaveLen(1), "exactly one Create")
	})

	It("sends no Create when the marker write loses a resourceVersion race", func() {
		req, rp := setup()
		reader := &bumpingReader{Reader: k8sClient}
		r := reconciler(rp, reader)

		_, err := r.Reconcile(ctx, req) // adds the finalizer
		Expect(err).NotTo(HaveOccurred())
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(reader.bumped.Load()).To(BeTrue())
		Expect(rp.createReqs).To(BeEmpty(), "a conflicting marker write sends no Create")
		Expect(res.RequeueAfter).To(Equal(providerErrorRetryInterval))
		vm := get(req)
		Expect(vm.Status.ID).To(BeEmpty())
		Expect(createRequested(vm)).To(BeFalse())

		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(rp.createReqs).To(HaveLen(1), "the next reconcile creates it")
		Expect(get(req).Status.ID).To(Equal(req.Namespace + ".web"))
	})

	It("records a created VM's id on an object whose status was never written", func() {
		req, rp := setup()
		r := reconciler(rp, k8sClient)
		vm := get(req)
		vm.Status.ID = "vm-recorded"
		recordBoundProvider(vm, holdTestProvider(req.Namespace))
		setCreateOutcome(vm, false)
		_, err := r.recordCreatedVM(ctx, vm)
		Expect(err).NotTo(HaveOccurred())
		Expect(get(req).Status.ID).To(Equal("vm-recorded"))
		_, remembered := r.lookupUnsavedCreate(vm)
		Expect(remembered).To(BeFalse())
	})
})
