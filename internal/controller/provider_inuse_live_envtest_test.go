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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// blindCacheClient is a client whose field-selected Lists (the cache index
// reads of the in-use check) see nothing: an informer cache that has not
// caught up with what uses a Provider. Every other call goes to Client.
type blindCacheClient struct {
	client.Client
}

func (b *blindCacheClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	lo := &client.ListOptions{}
	lo.ApplyOptions(opts)
	if lo.FieldSelector != nil && !lo.FieldSelector.Empty() {
		return nil
	}
	return b.Client.List(ctx, list, opts...)
}

var _ = Describe("Provider in-use live re-check (envtest)", func() {
	It("holds a Provider whose cache shows no use while the API server does, then releases it", func() {
		ns := inUseTestNamespace("inuse-live-")
		p := inUseEnvProvider(ns)
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
				return err
			}
			p.Finalizers = []string{infravirtrigaudiov1beta1.ProviderInUseFinalizer}
			return k8sClient.Update(ctx, p)
		})).To(Succeed())
		inUseEnvVM(ns, "web", "")
		DeferCleanup(func() {
			vm := &infravirtrigaudiov1beta1.VirtualMachine{}
			if k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web"}, vm) == nil {
				deleteVMThroughProvider(ns, "web")
			}
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				got := &infravirtrigaudiov1beta1.Provider{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
					return client.IgnoreNotFound(err)
				}
				got.Finalizers = nil
				return k8sClient.Update(ctx, got)
			})).To(Succeed())
		})
		Expect(k8sClient.Delete(ctx, p)).To(Succeed())

		r := &ProviderReconciler{Client: &blindCacheClient{Client: k8sClient}, Scheme: k8sClient.Scheme(),
			APIReader: k8sClient, Recorder: record.NewFakeRecorder(100)}
		get := func() (*infravirtrigaudiov1beta1.Provider, bool) {
			got := &infravirtrigaudiov1beta1.Provider{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(p), got)
			if apierrors.IsNotFound(err) {
				return nil, false
			}
			Expect(err).NotTo(HaveOccurred())
			return got, true
		}

		cur, ok := get()
		Expect(ok).To(BeTrue())
		_, err := r.handleDeletion(ctx, cur)
		Expect(err).NotTo(HaveOccurred())
		cur, ok = get()
		Expect(ok).To(BeTrue(), "the live re-check finds the VM the cache missed")
		blocked := meta.FindStatusCondition(cur.Status.Conditions, k8s.ConditionDeleteBlocked)
		Expect(blocked).NotTo(BeNil())
		Expect(blocked.Status).To(Equal(metav1.ConditionTrue))

		deleteVMThroughProvider(ns, "web")
		_, err = r.handleDeletion(ctx, cur)
		Expect(err).NotTo(HaveOccurred())
		_, ok = get()
		Expect(ok).To(BeFalse(), "released once the API server shows no use either")
	})
})
