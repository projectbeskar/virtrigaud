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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// This envtest spec runs the HostPool controller in a real manager against a
// real apiserver, so its watches are exercised end to end: a Host joining,
// leaving, moving between pools, changing health, or being deleted re-counts
// every pool it affects, while a Host heartbeat that moves no count writes
// nothing to the pool.
var _ = Describe("HostPool status (envtest)", func() {
	It("counts member Hosts, follows them between pools, and never writes an unchanged status", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "hostpool-status-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "hostpool-other-"}}
		Expect(k8sClient.Create(ctx, other)).To(Succeed())

		startHoldTestManager(func(mgr ctrl.Manager) {
			Expect((&HostPoolReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())
		})

		for _, name := range []string{"pool-a", "pool-b", "pool-empty"} {
			Expect(k8sClient.Create(ctx, &infravirtrigaudiov1beta1.HostPool{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
				Spec: infravirtrigaudiov1beta1.HostPoolSpec{
					ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "prov"},
				},
			})).To(Succeed())
		}
		createHost := func(namespace, name, pool string, schedulable bool, health infravirtrigaudiov1beta1.HostHealth) {
			h := poolHost(namespace, name, pool, "prov", "", schedulable)
			Expect(k8sClient.Create(ctx, h)).To(Succeed())
			setHostHealthEnv(namespace, name, health)
		}
		createHost(ns.Name, "h1", "pool-a", true, infravirtrigaudiov1beta1.HostHealthReady)
		createHost(ns.Name, "h2", "pool-a", false, infravirtrigaudiov1beta1.HostHealthReady)
		createHost(ns.Name, "h3", "pool-a", true, infravirtrigaudiov1beta1.HostHealthNotReady)
		// Same pool name, another namespace: never a member of ns/pool-a.
		createHost(other.Name, "h-foreign", "pool-a", true, infravirtrigaudiov1beta1.HostHealthReady)

		By("pool-a counts its three members; the foreign Host is not counted")
		expectPoolStatus(ns.Name, "pool-a", 3, 2, 1, metav1.ConditionTrue, reasonHostPoolHostsAvailable)
		By("an empty pool is Ready=False/NoMemberHosts")
		expectPoolStatus(ns.Name, "pool-empty", 0, 0, 0, metav1.ConditionFalse, reasonHostPoolNoMemberHosts)
		expectPoolStatus(ns.Name, "pool-b", 0, 0, 0, metav1.ConditionFalse, reasonHostPoolNoMemberHosts)

		By("kubectl get shows the counts as printer columns")
		cols, cells := hostPoolTable(ns.Name, "pool-a")
		Expect(cols).To(ContainElements("Total", "Ready", "Schedulable"))
		Expect(cells["Total"]).To(BeEquivalentTo(3))
		Expect(cells["Ready"]).To(BeEquivalentTo(2))
		Expect(cells["Schedulable"]).To(BeEquivalentTo(1))
		By("an empty pool's counts are written as 0, not left out")
		_, emptyCells := hostPoolTable(ns.Name, "pool-empty")
		for _, col := range []string{"Total", "Ready", "Schedulable"} {
			Expect(emptyCells[col]).NotTo(BeNil(), col)
			Expect(emptyCells[col]).To(BeEquivalentTo(0), col)
		}

		By("Host heartbeats that move no count never write the pool")
		rv := getPoolEnv(ns.Name, "pool-a").ResourceVersion
		for i := 0; i < 3; i++ {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				h := &infravirtrigaudiov1beta1.Host{}
				if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "h1"}, h); err != nil {
					return err
				}
				beat := metav1.NewTime(time.Now().Add(time.Duration(i) * time.Second))
				h.Status.LastHeartbeatTime = &beat
				return k8sClient.Status().Update(ctx, h)
			})).To(Succeed())
		}
		Consistently(func() string { return getPoolEnv(ns.Name, "pool-a").ResourceVersion }, "2s", "200ms").Should(Equal(rv))

		By("moving h1 from pool-a to pool-b recounts both pools")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			h := &infravirtrigaudiov1beta1.Host{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "h1"}, h); err != nil {
				return err
			}
			h.Spec.PoolRef.Name = "pool-b"
			return k8sClient.Update(ctx, h)
		})).To(Succeed())
		expectPoolStatus(ns.Name, "pool-a", 2, 1, 0, metav1.ConditionFalse, reasonHostPoolNoSchedulableHosts)
		expectPoolStatus(ns.Name, "pool-b", 1, 1, 1, metav1.ConditionTrue, reasonHostPoolHostsAvailable)

		By("a health change is counted")
		setHostHealthEnv(ns.Name, "h3", infravirtrigaudiov1beta1.HostHealthReady)
		expectPoolStatus(ns.Name, "pool-a", 2, 2, 1, metav1.ConditionTrue, reasonHostPoolHostsAvailable)

		By("a deleted Host leaves its pool's counts")
		Expect(k8sClient.Delete(ctx, &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "h3"},
		})).To(Succeed())
		expectPoolStatus(ns.Name, "pool-a", 1, 1, 0, metav1.ConditionFalse, reasonHostPoolNoSchedulableHosts)

		By("a pool spec change is recorded as the observed generation")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			p := getPoolEnv(ns.Name, "pool-a")
			p.Spec.Strategy = infravirtrigaudiov1beta1.PoolStrategyBinPack
			return k8sClient.Update(ctx, p)
		})).To(Succeed())
		Eventually(func(g Gomega) {
			p := getPoolEnv(ns.Name, "pool-a")
			g.Expect(p.Generation).To(BeNumerically(">", 1))
			g.Expect(p.Status.ObservedGeneration).To(Equal(p.Generation))
			c := meta.FindStatusCondition(p.Status.Conditions, k8s.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.ObservedGeneration).To(Equal(p.Generation))
		}, "10s", "100ms").Should(Succeed())
	})
})

// setHostHealthEnv writes status.health on a Host in the envtest apiserver.
func setHostHealthEnv(namespace, name string, health infravirtrigaudiov1beta1.HostHealth) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		h := &infravirtrigaudiov1beta1.Host{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, h); err != nil {
			return err
		}
		h.Status.Health = health
		return k8sClient.Status().Update(ctx, h)
	})).To(Succeed())
}

// getPoolEnv reads a HostPool from the envtest apiserver.
func getPoolEnv(namespace, name string) *infravirtrigaudiov1beta1.HostPool {
	p := &infravirtrigaudiov1beta1.HostPool{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, p)).To(Succeed())
	return p
}

// expectPoolStatus waits until the pool reports the given counts and Ready
// condition, with the condition and status stamped with the pool generation.
func expectPoolStatus(namespace, name string, total, ready, schedulable int32, status metav1.ConditionStatus, reason string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		p := getPoolEnv(namespace, name)
		g.Expect(p.Status.TotalHosts).To(Equal(total), "totalHosts")
		g.Expect(p.Status.ReadyHosts).To(Equal(ready), "readyHosts")
		g.Expect(p.Status.SchedulableHosts).To(Equal(schedulable), "schedulableHosts")
		g.Expect(p.Status.ObservedGeneration).To(Equal(p.Generation))
		c := meta.FindStatusCondition(p.Status.Conditions, k8s.ConditionReady)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(status))
		g.Expect(c.Reason).To(Equal(reason))
		g.Expect(c.ObservedGeneration).To(Equal(p.Generation))
	}, "10s", "100ms").Should(Succeed())
}

// hostPoolTable fetches one HostPool as the server-side Table kubectl get
// prints, returning the column names and the row's cells keyed by column.
func hostPoolTable(namespace, name string) ([]string, map[string]interface{}) {
	cs, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	body, err := cs.CoreV1().RESTClient().Get().
		AbsPath("/apis/infra.virtrigaud.io/v1beta1/namespaces", namespace, "hostpools", name).
		SetHeader("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io").
		DoRaw(ctx)
	Expect(err).NotTo(HaveOccurred(), string(body))
	var table metav1.Table
	Expect(json.Unmarshal(body, &table)).To(Succeed())
	Expect(table.Rows).To(HaveLen(1))
	cols := make([]string, 0, len(table.ColumnDefinitions))
	cells := map[string]interface{}{}
	for i, c := range table.ColumnDefinitions {
		cols = append(cols, c.Name)
		cells[c.Name] = table.Rows[0].Cells[i]
	}
	return cols, cells
}
