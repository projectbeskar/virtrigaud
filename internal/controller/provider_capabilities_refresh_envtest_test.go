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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These envtest specs pin the v0.4.1-rc1 lab finding B2 end to end: after the
// runtime image of a Provider changes, status.reportedCapabilities is read
// again from the new runtime once its rollout is complete, never while a pod
// of the superseded ReplicaSet can still answer, without a hot loop; and
// status.healthy follows the Deployment's ready replicas. envtest runs no
// deployment controller: the specs write the Deployment status a rollout
// would.

// Image tags of the two runtimes in the specs, and what each answers.
const (
	capsEnvOldImage = "virtrigaud/provider-proxmox:old"
	capsEnvNewImage = "virtrigaud/provider-proxmox:new"
)

var (
	capsEnvOld = contracts.Capabilities{SupportsLinkedClones: true, SupportsImageImport: true, SupportsSnapshots: true}
	capsEnvNew = contracts.Capabilities{SupportsLinkedClones: false, SupportsImageImport: false, SupportsSnapshots: true,
		SupportsHonestReconfigure: true}
)

// serviceCapabilityReader models a capability read through the provider's
// Service, from the runtime Deployment as the apiserver holds it: while the
// rollout is not complete the answer comes from the old runtime (the pod a
// connection could still reach) and is counted as a mid-rollout read; once it
// is complete, from the runtime of the Deployment's current image.
type serviceCapabilityReader struct {
	reads           atomic.Int32
	midRolloutReads atomic.Int32
}

func (s *serviceCapabilityReader) ReadCapabilities(ctx context.Context, p *infravirtrigaudiov1beta1.Provider) (contracts.Capabilities, error) {
	s.reads.Add(1)
	dep := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, capsEnvDeploymentKey(p.Namespace, p.Name), dep); err != nil {
		return contracts.Capabilities{}, err
	}
	if !deploymentRolloutComplete(dep) {
		s.midRolloutReads.Add(1)
		return capsEnvOld, nil
	}
	if dep.Spec.Template.Spec.Containers[0].Image == capsEnvNewImage {
		return capsEnvNew, nil
	}
	return capsEnvOld, nil
}

// capsEnvDeploymentKey is the runtime Deployment of Provider ns/name.
func capsEnvDeploymentKey(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: fmt.Sprintf("virtrigaud-provider-%s-%s", ns, name)}
}

// startCapabilityRefreshManager starts a manager running only the Provider
// controller with reader, a 1s settle delay and a 1h refresh interval, its
// cache restricted to ns.
func startCapabilityRefreshManager(ns string, reader ProviderCapabilityReader) {
	mgrCtx, stop := context.WithCancel(ctx)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 k8sClient.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect((&ProviderReconciler{
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		Recorder:                  mgr.GetEventRecorderFor("provider-controller"),
		CapabilityReader:          reader,
		capabilitySettleDelay:     time.Second,
		capabilityRefreshInterval: time.Hour,
	}).SetupWithManager(mgr)).To(Succeed())
	mgrDone := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(mgrDone)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
	DeferCleanup(func() { stop(); <-mgrDone })
}

// capsEnvProvider creates a plaintext Proxmox Provider prov-1 in ns running
// image (a RollingUpdate runtime, as in the lab finding).
func capsEnvProvider(ns, image string) {
	p := holdTestProvider(ns)
	p.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeProxmox
	p.Spec.Endpoint = "https://pve.example.com:8006"
	p.Spec.Runtime.Image = image
	p.Spec.Runtime.Service = &infravirtrigaudiov1beta1.ProviderServiceSpec{
		Port: 9443, TLS: &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false},
	}
	Expect(k8sClient.Create(ctx, p)).To(Succeed())
}

// setDeploymentStatus writes the runtime Deployment's status through mutate,
// as the deployment controller would, once its spec is at image.
func setDeploymentStatus(ns, image string, mutate func(dep *appsv1.Deployment)) {
	key := capsEnvDeploymentKey(ns, "prov-1")
	Eventually(func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(image))
	}, "20s", "100ms").Should(Succeed())
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, key, dep); err != nil {
			return err
		}
		mutate(dep)
		return k8sClient.Status().Update(ctx, dep)
	})).To(Succeed())
}

// rolloutComplete is the status of a complete rollout of one replica.
func rolloutComplete(dep *appsv1.Deployment) {
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Replicas, dep.Status.UpdatedReplicas = 1, 1
	dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1
}

// getCapsEnvProvider reads prov-1 in ns from the apiserver.
func getCapsEnvProvider(g Gomega, ns string) *infravirtrigaudiov1beta1.Provider {
	p := &infravirtrigaudiov1beta1.Provider{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "prov-1"}, p)).To(Succeed())
	return p
}

var _ = Describe("Provider capability refresh after a runtime roll (envtest)", func() {
	It("re-reads reportedCapabilities from the new runtime after its rollout, never mid-rollout, without a hot loop", func() {
		ns := inUseTestNamespace("pcaps-roll-")
		reader := &serviceCapabilityReader{}
		startCapabilityRefreshManager(ns, reader)
		capsEnvProvider(ns, capsEnvOldImage)

		By("the old runtime is rolled out and its capabilities recorded")
		setDeploymentStatus(ns, capsEnvOldImage, rolloutComplete)
		Eventually(func(g Gomega) {
			p := getCapsEnvProvider(g, ns)
			g.Expect(p.Status.Healthy).To(BeTrue())
			g.Expect(p.Status.ReportedCapabilities).NotTo(BeNil())
			g.Expect(p.Status.ReportedCapabilities.SupportsLinkedClones).To(BeTrue())
		}, "20s", "100ms").Should(Succeed())
		// The confirming read, after the (1s) settle delay.
		Eventually(reader.reads.Load, "10s", "100ms").Should(BeNumerically(">=", 2))

		By("the Provider's image changes: the runtime Deployment rolls")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			p := &infravirtrigaudiov1beta1.Provider{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "prov-1"}, p); err != nil {
				return err
			}
			p.Spec.Runtime.Image = capsEnvNewImage
			return k8sClient.Update(ctx, p)
		})).To(Succeed())

		By("mid-rollout (the new pod ready, the old pod still there): nothing is read")
		setDeploymentStatus(ns, capsEnvNewImage, func(dep *appsv1.Deployment) {
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas, dep.Status.UpdatedReplicas = 2, 1
			dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 2, 2
		})
		Consistently(func(g Gomega) {
			g.Expect(reader.midRolloutReads.Load()).To(BeZero(), "no read while a superseded pod can answer")
			p := getCapsEnvProvider(g, ns)
			g.Expect(p.Status.Healthy).To(BeTrue(), "the old pod still serves")
			g.Expect(p.Status.ReportedCapabilities.SupportsLinkedClones).To(BeTrue())
		}, "2s", "100ms").Should(Succeed())

		By("the rollout completes: the new runtime's capabilities are recorded")
		readsBefore := reader.reads.Load()
		setDeploymentStatus(ns, capsEnvNewImage, rolloutComplete)
		var refreshed *infravirtrigaudiov1beta1.Provider
		Eventually(func(g Gomega) {
			refreshed = getCapsEnvProvider(g, ns)
			g.Expect(refreshed.Status.ReportedCapabilities).NotTo(BeNil())
			g.Expect(refreshed.Status.ReportedCapabilities.SupportsLinkedClones).To(BeFalse())
			g.Expect(refreshed.Status.ReportedCapabilities.SupportsHonestReconfigure).To(BeTrue())
		}, "20s", "100ms").Should(Succeed())
		Expect(reader.midRolloutReads.Load()).To(BeZero())
		cond := meta.FindStatusCondition(refreshed.Status.Conditions, providerConditionCapabilitiesReported)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.ObservedGeneration).To(Equal(refreshed.Generation))

		By("gates that read the Provider status see the new runtime's capabilities")
		Expect(providerAdvertisesImageImport(refreshed)).To(BeFalse())
		Expect(providerAdvertisesHonestReconfigure(refreshed)).To(BeTrue())

		By("the confirming read follows the settle delay; then no further read and at most one trailing status write (no hot loop)")
		Eventually(reader.reads.Load, "10s", "100ms").Should(Equal(readsBefore + 2))
		versions := map[string]bool{}
		Consistently(func(g Gomega) {
			g.Expect(reader.reads.Load()).To(Equal(readsBefore+2), "settled: the next read is the hourly one")
			versions[getCapsEnvProvider(g, ns).ResourceVersion] = true
		}, "3s", "100ms").Should(Succeed())
		Expect(len(versions)).To(BeNumerically("<=", 2), "the Provider is not rewritten in a loop")
	})

	It("sets ProviderAvailable=False and healthy=false when the runtime has no ready pod, and True again when it has", func() {
		ns := inUseTestNamespace("pcaps-health-")
		startCapabilityRefreshManager(ns, &serviceCapabilityReader{})
		capsEnvProvider(ns, capsEnvOldImage)

		setDeploymentStatus(ns, capsEnvOldImage, rolloutComplete)
		Eventually(func(g Gomega) {
			g.Expect(getCapsEnvProvider(g, ns).Status.Healthy).To(BeTrue())
		}, "20s", "100ms").Should(Succeed())

		By("the only pod stops being ready")
		setDeploymentStatus(ns, capsEnvOldImage, func(dep *appsv1.Deployment) {
			dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 0, 0
		})
		Eventually(func(g Gomega) {
			p := getCapsEnvProvider(g, ns)
			g.Expect(p.Status.Healthy).To(BeFalse())
			g.Expect(p.Status.Runtime.Phase).To(Equal(infravirtrigaudiov1beta1.ProviderRuntimePhasePending))
			cond := meta.FindStatusCondition(p.Status.Conditions, "ProviderAvailable")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("DeploymentNotReady"))
		}, "20s", "100ms").Should(Succeed())

		By("it is ready again")
		setDeploymentStatus(ns, capsEnvOldImage, rolloutComplete)
		Eventually(func(g Gomega) {
			p := getCapsEnvProvider(g, ns)
			g.Expect(p.Status.Healthy).To(BeTrue())
			g.Expect(meta.IsStatusConditionTrue(p.Status.Conditions, "ProviderAvailable")).To(BeTrue())
		}, "20s", "100ms").Should(Succeed())
	})
})

// Compile-time check: the reader the envtest specs use is a capability reader.
var _ ProviderCapabilityReader = (*serviceCapabilityReader)(nil)
