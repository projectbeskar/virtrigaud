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
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// This spec runs every Secret access of the manager (docs/manager-secret-access.md)
// as the manager's ServiceAccount, holding exactly the RBAC the chart renders
// with rbac.secrets.scope=namespaced, through a manager whose Secret informer
// is restricted the way --secret-cache-namespaces restricts it. Each feature
// works in the namespaces it is granted, and a Secret anywhere else is refused.
var _ = Describe("Manager Secret access with the namespaced grant (envtest)", func() {
	It("keeps every Secret feature working and refuses Secrets outside the granted namespaces", func(_ SpecContext) {
		helm, err := exec.LookPath("helm")
		if err != nil {
			Skip("helm not installed")
		}
		ctx := context.Background()
		newNS := func(prefix string) string {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			return ns.Name
		}
		provNS, workNS, otherNS, relNS := newNS("sec-prov-"), newNS("sec-work-"), newNS("sec-other-"), newNS("sec-rel-")

		// The chart's manager RBAC in namespaced mode, applied as rendered.
		// The release name is the (unique) release namespace, so the
		// cluster-scoped objects do not collide with other specs.
		out, err := exec.Command(helm, "template", relNS, filepath.Join("..", "..", "charts", "virtrigaud"), // #nosec G204 -- test-controlled arguments
			"--namespace", relNS,
			"--show-only", "templates/manager-rbac.yaml", "--show-only", "templates/manager-secrets-rbac.yaml",
			"--set", "rbac.secrets.scope=namespaced",
			"--set", "rbac.secrets.providerNamespaces={"+provNS+"}",
			"--set", "rbac.secrets.workloadNamespaces={"+workNS+"}").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		saName := ""
		for _, u := range secretRBACDocs(GinkgoT(), string(out)) {
			if u.GetKind() == "ServiceAccount" {
				saName = u.GetName()
			}
			Expect(k8sClient.Create(ctx, u)).To(Succeed(), u.GetKind()+" "+u.GetName())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, u) })
		}
		Expect(saName).NotTo(BeEmpty())

		// The manager, as its ServiceAccount, with the restricted Secret informer.
		mcfg := rest.CopyConfig(cfg)
		mcfg.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:" + relNS + ":" + saName,
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + relNS, "system:authenticated"},
		}
		skipNameValidation := true
		mgr, err := ctrl.NewManager(mcfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
			Cache:                  cache.Options{ByObject: SecretCacheByObject([]string{provNS})},
		})
		Expect(err).NotTo(HaveOccurred())
		mctx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mctx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mctx)).To(BeTrue())
		cl, live := mgr.GetClient(), mgr.GetAPIReader()

		secret := func(ns, name string, data map[string]string) {
			s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, StringData: data}
			Expect(k8sClient.Create(ctx, s)).To(Succeed())
		}
		// RBAC objects reach the authorizer asynchronously.
		Eventually(func() error {
			return live.Get(ctx, types.NamespacedName{Namespace: provNS, Name: "absent"}, &corev1.Secret{})
		}, "30s", "200ms").Should(Satisfy(apierrors.IsNotFound))

		By("Provider namespace: the TLS Secret is read through the informer cache")
		secret(provNS, "provider-tls", map[string]string{"tls.crt": "c", "tls.key": "k", "ca.crt": "a"})
		Eventually(func() error {
			return cl.Get(ctx, types.NamespacedName{Namespace: provNS, Name: "provider-tls"}, &corev1.Secret{})
		}, "10s", "100ms").Should(Succeed())

		By("Provider namespace: the host credential Secret is read and the host-inventory Secret written")
		secret(provNS, "creds", map[string]string{credentialSecretKeySSHPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n"})
		prov := envtestProvider(provNS, "clustered", nil)
		prov.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, &infravirtrigaudiov1beta1.HostPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: provNS},
			Spec: infravirtrigaudiov1beta1.HostPoolSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: prov.Name},
				Strategy:    infravirtrigaudiov1beta1.PoolStrategySpread,
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: "kvm-01", Namespace: provNS},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: prov.Name},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "qemu+ssh://virt@kvm-01/system",
				Schedulable: true,
			},
		})).To(Succeed())
		pr := &ProviderReconciler{Client: cl, Scheme: k8sClient.Scheme(), APIReader: live}
		inv := &corev1.Secret{}
		Eventually(func(g Gomega) {
			g.Expect(pr.reconcileHostInventorySecret(ctx, prov)).To(Succeed())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: provNS, Name: pr.getHostInventorySecretName(prov)}, inv)).To(Succeed())
			g.Expect(string(inv.Data[hostsecret.SecretDataKey])).To(ContainSubstring("kvm-01"), "the host is rendered with its credentials")
		}, "10s", "100ms").Should(Succeed())

		By("Provider namespace: operation-intent Secrets are created, read, swept and deleted")
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-1", Namespace: workNS},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: prov.Name, Namespace: provNS},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		intentKey := make([]byte, 32)
		intentKey[0] = 7
		auth, err := operation.NewIntentAuthenticator(intentKey)
		Expect(err).NotTo(HaveOccurred())
		vr := &VirtualMachineReconciler{Client: cl, APIReader: live, Scheme: k8sClient.Scheme(), OperationIntentAuth: auth}
		prepared := func(b byte) contracts.PreparedOperation {
			opID := make([]byte, operation.IDLen)
			opID[0] = b
			return contracts.PreparedOperation{
				Kind:         contracts.OperationKindCreate,
				Identity:     contracts.OperationAttemptIdentity{ProviderUID: string(prov.UID), VMUID: string(vm.UID), OpID: opID},
				RequestBytes: []byte("request"),
				DigestSalt:   []byte("salt"),
			}
		}
		opHex := func(p contracts.PreparedOperation) string { return hex.EncodeToString(p.Identity.OpID) }
		first, second := prepared(1), prepared(2)
		Expect(vr.storeIntent(ctx, prov, vm, first)).To(Succeed())
		got, found, err := vr.loadIntent(ctx, vm, opHex(first), provNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(got.RequestBytes).To(Equal(first.RequestBytes))
		Expect(vr.deleteIntent(ctx, provNS, opHex(first), vm.UID)).To(Succeed())
		_, found, err = vr.loadIntent(ctx, vm, opHex(first), provNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())

		// The sweep lists the Provider namespace's intent Secrets through the
		// cache: an intent no attempt names is removed once past the grace.
		Expect(vr.storeIntent(ctx, prov, vm, second)).To(Succeed())
		Eventually(func(g Gomega) {
			deleted, _, err := sweepOperationIntents(ctx, cl, live, prov, time.Now().Add(operationIntentSweepGrace+time.Minute))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deleted).To(Equal(1))
		}, "10s", "100ms").Should(Succeed())

		By("Workload namespace: cloud-init user data and metadata Secrets are read (get only)")
		secret(workNS, "user-data", map[string]string{"userdata": "#cloud-config\n"})
		secret(workNS, "meta-data", map[string]string{"metadata": "instance-id: vm-1\n"})
		ud, err := vr.resolveCloudInitUserData(ctx, workNS, &infravirtrigaudiov1beta1.CloudInit{
			SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "user-data"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(ud).To(Equal("#cloud-config\n"))
		md, err := vr.resolveCloudInitMetaData(ctx, workNS, &infravirtrigaudiov1beta1.CloudInitMetaData{
			SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "meta-data"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(md).To(Equal("instance-id: vm-1\n"))

		By("Workload namespace: the S3 credentials Secret of a migration is read (get only)")
		secret(workNS, "s3-creds", map[string]string{
			storagemigration.CredKeyAccessKeyID: "AKIA", storagemigration.CredKeySecretAccessKey: "secret"})
		mr := &VMMigrationReconciler{Client: cl, APIReader: live}
		creds, err := mr.loadS3Credentials(ctx, &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: workNS},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{Storage: &infravirtrigaudiov1beta1.MigrationStorage{
				Type: storagemigration.BackendS3,
				S3: &infravirtrigaudiov1beta1.S3StorageConfig{
					CredentialsSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: "s3-creds"}},
			}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(creds).To(HaveKeyWithValue(storagemigration.CredKeyAccessKeyID, "AKIA"))

		By("Workload namespace: no list, watch or write")
		Expect(live.List(ctx, &corev1.SecretList{}, client.InNamespace(workNS))).To(Satisfy(apierrors.IsForbidden))
		Expect(cl.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: workNS}})).
			To(Satisfy(apierrors.IsForbidden))
		Expect(cl.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "user-data", Namespace: workNS}})).
			To(Satisfy(apierrors.IsForbidden))

		By("Any other namespace, and cluster-wide: refused")
		secret(otherNS, "ca-key", map[string]string{"tls.key": "k"})
		Expect(live.Get(ctx, types.NamespacedName{Namespace: otherNS, Name: "ca-key"}, &corev1.Secret{})).
			To(Satisfy(apierrors.IsForbidden))
		Expect(cl.Get(ctx, types.NamespacedName{Namespace: otherNS, Name: "ca-key"}, &corev1.Secret{})).
			To(HaveOccurred(), "the informer does not cover the namespace")
		_, err = vr.resolveCloudInitUserData(ctx, otherNS, &infravirtrigaudiov1beta1.CloudInit{
			SecretRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "ca-key"}})
		Expect(err).To(Satisfy(apierrors.IsForbidden))
		Expect(live.List(ctx, &corev1.SecretList{})).To(Satisfy(apierrors.IsForbidden))
	}, SpecTimeout(2*time.Minute))
})
