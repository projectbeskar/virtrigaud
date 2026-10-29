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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// The clustered-Provider resync (clusteredProviderResync) re-runs an unchanged
// Provider every minute. Against a real apiserver (defaulting included), an
// unchanged reconcile must not roll the provider Deployment (no generation
// bump), nor rewrite the host-inventory Secret, Service or ServiceAccount; a
// shorter requeue (the Deployment not ready yet) keeps priority over the
// resync, and a ready clustered Provider requeues at the resync interval.
var _ = Describe("Clustered Provider periodic resync (envtest)", func() {
	It("reconciles an unchanged clustered Provider without rollouts or rewrites", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "prov-resync-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })

		secret := credSecret("test-creds", sshCredData())
		secret.Namespace = ns.Name
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		host := hostCR("host-a", "resync", nil)
		host.Namespace = ns.Name
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		prov := clusterProvider("resync")
		prov.Namespace = ns.Name
		prov.UID = ""
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		DeferCleanup(func() {
			p := &infravirtrigaudiov1beta1.Provider{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(prov), p); err == nil {
				p.Finalizers = nil
				_ = k8sClient.Update(ctx, p)
				_ = k8sClient.Delete(ctx, p)
			}
		})

		r := &ProviderReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(100)}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(prov)}
		reconcileOnce := func() ctrl.Result {
			res, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			return res
		}

		// Converge: the first reconciles add the finalizer and create the runtime.
		for i := 0; i < 3; i++ {
			reconcileOnce()
		}

		key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: ns.Name, Name: name} }
		snapshot := func() (depGen int64, depRV, secRV, svcRV, saRV string) {
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key(r.getDeploymentName(prov)), dep)).To(Succeed())
			sec := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, key("resync-hosts"), sec)).To(Succeed())
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key(r.getServiceName(prov)), svc)).To(Succeed())
			sa := &corev1.ServiceAccount{}
			Expect(k8sClient.Get(ctx, key(r.getServiceAccountName(prov)), sa)).To(Succeed())
			return dep.Generation, dep.ResourceVersion, sec.ResourceVersion, svc.ResourceVersion, sa.ResourceVersion
		}

		g1, d1, s1, v1, a1 := snapshot()
		// The Deployment has no ready replica in envtest: its 30 s readiness
		// requeue is shorter than the resync and keeps priority.
		res := reconcileOnce()
		Expect(res.RequeueAfter).To(Equal(30 * time.Second))
		reconcileOnce()
		g2, d2, s2, v2, a2 := snapshot()
		Expect(g2).To(Equal(g1), "an unchanged reconcile must not roll the Deployment")
		Expect(d2).To(Equal(d1), "the Deployment update is a no-op")
		Expect(s2).To(Equal(s1), "the host-inventory Secret is not rewritten")
		Expect(v2).To(Equal(v1), "the Service is not rewritten")
		Expect(a2).To(Equal(a1), "the ServiceAccount is not rewritten")

		// Ready: the clustered Provider requeues at the resync interval, and
		// still writes nothing.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key(r.getDeploymentName(prov)), dep)).To(Succeed())
		dep.Status.Replicas, dep.Status.ReadyReplicas = 1, 1
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		res = reconcileOnce()
		Expect(res.RequeueAfter).To(Equal(clusteredProviderResync))
		res = reconcileOnce()
		Expect(res.RequeueAfter).To(Equal(clusteredProviderResync))
		g3, _, s3, v3, a3 := snapshot()
		Expect(g3).To(Equal(g1))
		Expect(s3).To(Equal(s1))
		Expect(v3).To(Equal(v1))
		Expect(a3).To(Equal(a1))
	})
})
