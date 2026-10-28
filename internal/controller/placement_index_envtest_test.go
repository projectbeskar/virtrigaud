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
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// This spec pins the one dependency of the direct index read (indexedReader)
// on controller-runtime's internals: the name it gives a field index in the
// informer's indexer and the cluster-wide key it stores ("field:<name>",
// "__all_namespaces/<value>"). With a real manager cache, the direct read
// must return exactly what a List with MatchingFields returns.

var _ = Describe("Placement index read directly from the informer (envtest)", func() {
	It("returns the same VirtualMachines as a List through the cache", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "placement-index-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
		Expect(err).NotTo(HaveOccurred())
		Expect(indexPlacementProvider(mgr)).To(Succeed())
		idx := placementVMIndexer(mgr)
		Expect(idx).NotTo(BeNil(), "the manager's cache exposes the VirtualMachine informer's indexer")

		mgrCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		for _, vm := range []struct{ name, provider string }{{"a", "prov-x"}, {"b", "prov-x"}, {"c", "prov-y"}} {
			obj := &infravirtrigaudiov1beta1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: vm.name, Namespace: ns.Name},
				Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
					ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: vm.provider},
					ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
					ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
				},
			}
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			removeVMFinalizersOnCleanup(obj)
		}

		names := func(provider string, direct bool) []string {
			key := types.NamespacedName{Namespace: ns.Name, Name: provider}
			var out []string
			if direct {
				vms, err := indexedReader{Reader: mgr.GetCache(), vms: idx}.providerVMs(key)
				Expect(err).NotTo(HaveOccurred())
				for _, vm := range vms {
					out = append(out, vm.Name)
				}
			} else {
				vms, err := listProviderVMs(ctx, mgr.GetCache(), key)
				Expect(err).NotTo(HaveOccurred())
				for i := range vms {
					out = append(out, vms[i].Name)
				}
			}
			slices.Sort(out)
			return out
		}
		Eventually(func() []string { return names("prov-x", true) }).Should(Equal([]string{"a", "b"}))
		for _, provider := range []string{"prov-x", "prov-y", "prov-none"} {
			Expect(names(provider, true)).To(Equal(names(provider, false)), "Provider %s", provider)
		}
	})

	// The VirtualMachine controller registers v0.4.1's create-refusal index and
	// the placement index in the same informer, and reads placement scans from
	// that informer's indexer directly. Both must survive SetupWithManager.
	It("keeps the create-refusal and placement indexes when the VirtualMachine controller is set up", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "placement-index-setup-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: k8sClient.Scheme(),
			Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			Controller: ctrlconfig.Controller{SkipNameValidation: &skipNameValidation}})
		Expect(err).NotTo(HaveOccurred())
		r := &VirtualMachineReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			RemoteResolver: &atomicResolver{provider: &rejectingImageProvider{}, namespace: ns.Name}}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		Expect(r.placementIndex).NotTo(BeNil(), "placement scans read the informer's indexer directly")
		indexers := r.placementIndex.GetIndexers()
		Expect(indexers).To(HaveKey("field:"+createRefusalRefIndex), "v0.4.1's create-refusal index is in the same informer")
		Expect(indexers).To(HaveKey("field:"+placementProviderIndex), "the placement index is in the same informer")

		mgrCtx, stop := context.WithCancel(ctx)
		mgrDone := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(mgrDone)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() { stop(); <-mgrDone })
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		obj := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: "prov-x"},
				ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
			},
		}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		removeVMFinalizersOnCleanup(obj)

		key := types.NamespacedName{Namespace: ns.Name, Name: "prov-x"}
		Eventually(func() ([]string, error) {
			vms, err := indexedReader{Reader: mgr.GetCache(), vms: r.placementIndex}.providerVMs(key)
			var out []string
			for _, vm := range vms {
				out = append(out, vm.Name)
			}
			return out, err
		}).Should(Equal([]string{"a"}))
		Expect(mgr.GetCache().List(ctx, &infravirtrigaudiov1beta1.VirtualMachineList{},
			client.MatchingFields{createRefusalRefIndex: consumerRefIndexValue(consumerKindVMClass,
				types.NamespacedName{Namespace: ns.Name, Name: "small"})})).To(Succeed(),
			"a List by the create-refusal index is answered by the cache")
	})
})
