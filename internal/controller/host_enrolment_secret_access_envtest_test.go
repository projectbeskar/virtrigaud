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
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This spec runs the per-host agent enrolment Secret access check as the
// manager's ServiceAccount against a real API server's RBAC authorizer, with
// the RBAC the chart renders: it passes with the namespaced Secret grant plus
// the enrolment Role (full Secret access in the enrolment namespace only),
// refuses once the manager may read the sub-CA key Secret (get, a field-selected
// list, an update that returns it) or holds an escalation primitive (token
// minting, impersonation), passes again once the grant is gone, and refuses
// with the chart's default cluster-wide Secret grant.
var _ = Describe("Per-host agent enrolment Secret access check (envtest)", func() {
	It("passes with the namespaced grant and refuses any read of the sub-CA key", func(_ SpecContext) {
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
		provNS, enrolNS, keyNS, relNS := newNS("he-prov-"), newNS("he-pki-"), newNS("he-cm-"), newNS("he-rel-")
		key := types.NamespacedName{Namespace: keyNS, Name: "host-agent-ca"}

		// apply renders the chart (release name = the unique release
		// namespace) and creates the given templates as rendered; it returns
		// a client acting as the manager's ServiceAccount.
		apply := func(release string, args ...string) client.Client {
			all := append([]string{"template", release, filepath.Join("..", "..", "charts", "virtrigaud"),
				"--namespace", relNS,
				"--show-only", "templates/manager-rbac.yaml"}, args...)
			out, err := exec.Command(helm, all...).CombinedOutput() // #nosec G204 -- test-controlled arguments
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
			mcfg := rest.CopyConfig(cfg)
			mcfg.Impersonate = rest.ImpersonationConfig{
				UserName: "system:serviceaccount:" + relNS + ":" + saName,
				Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + relNS, "system:authenticated"},
			}
			c, err := client.New(mcfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			return c
		}
		enrolment := []string{
			"--show-only", "templates/manager-secrets-rbac.yaml", "--show-only", "templates/host-enrolment-rbac.yaml",
			"--set", "rbac.secrets.scope=namespaced",
			"--set", "rbac.secrets.providerNamespaces={" + provNS + "}",
			"--set", "experimental.hostEnrolment.enabled=true",
			"--set", "experimental.hostEnrolment.developmentBuild=true",
			"--set", "experimental.hostEnrolment.namespace=" + enrolNS,
			"--set", "experimental.hostEnrolment.caKeySecret=" + key.String(),
			"--set", "experimental.hostEnrolment.signerNames={clusterissuers.cert-manager.io/host-agent-ca}",
		}
		mgr := apply(relNS, enrolment...)
		withKey := &HostEnrolmentConfig{Namespace: enrolNS, CAKeySecret: &key}

		By("The namespaced grant and the enrolment Role pass, with and without a declared key")
		// RBAC objects reach the authorizer asynchronously: wait until the
		// enrolment Role is in force.
		Eventually(func() bool {
			r := &authorizationSSAR{c: mgr}
			return r.allowed(ctx, enrolNS, "create", "")
		}, "30s", "200ms").Should(BeTrue())
		Expect(CheckHostEnrolmentSecretAccess(ctx, mgr, withKey)).To(Succeed(),
			"the chart's manager and enrolment RBAC grant no Secret read outside the enrolment namespace and no escalation primitive")

		By("The chart's cluster-scoped manager may create Deployments in the key namespace: a warning, not a refusal")
		workload, err := HostEnrolmentWorkloadAccess(ctx, mgr, withKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(workload).To(Equal([]string{"create deployments.apps in namespace " + keyNS}))

		// grant binds a Role (in keyNS, or a ClusterRole when cluster) to
		// the manager's ServiceAccounts, waits until the check refuses with
		// want, then removes it and waits until the check passes again.
		grant := func(name string, cluster bool, rule rbacv1.PolicyRule, want string) {
			var role, binding client.Object
			if cluster {
				role = &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: relNS + "-" + name}, Rules: []rbacv1.PolicyRule{rule}}
				binding = &rbacv1.ClusterRoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: relNS + "-" + name},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: relNS + "-" + name},
					Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:serviceaccounts:" + relNS}},
				}
			} else {
				role = &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: keyNS}, Rules: []rbacv1.PolicyRule{rule}}
				binding = &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: keyNS},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
					Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:serviceaccounts:" + relNS}},
				}
			}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			Expect(k8sClient.Create(ctx, binding)).To(Succeed())
			Eventually(func() error { return CheckHostEnrolmentSecretAccess(ctx, mgr, withKey) }, "30s", "200ms").
				Should(MatchError(ErrHostEnrolmentSecretAccess))
			err := CheckHostEnrolmentSecretAccess(ctx, mgr, withKey)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(err.Error()).NotTo(ContainSubstring("secrets in all namespaces"))
			Expect(k8sClient.Delete(ctx, binding)).To(Succeed())
			Expect(k8sClient.Delete(ctx, role)).To(Succeed())
			Eventually(func() error { return CheckHostEnrolmentSecretAccess(ctx, mgr, withKey) }, "30s", "200ms").Should(Succeed())
		}

		By("A grant to get the exact sub-CA key Secret refuses")
		grant("get-ca-key", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"},
			ResourceNames: []string{key.Name}}, "get secrets named "+key.String())

		By("A resourceNames grant to list the sub-CA key Secret (a field-selected list) refuses")
		grant("list-ca-key", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"list"},
			ResourceNames: []string{key.Name}}, "list secrets named "+key.String())

		By("A grant to update Secrets in the key namespace (the response carries the data) refuses")
		grant("update-secrets", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"update"}},
			"update secrets in namespace "+keyNS)

		By("A grant to mint ServiceAccount tokens in the key namespace refuses")
		grant("tokens", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, Verbs: []string{"create"}},
			"create serviceaccounts/token in namespace "+keyNS)

		By("A cluster-wide grant to impersonate ServiceAccounts refuses")
		grant("impersonate", true, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"impersonate"}},
			"impersonate serviceaccounts in all namespaces")

		By("The chart's default cluster-wide Secret grant refuses")
		other := apply(relNS + "-cluster")
		Eventually(func() error {
			return CheckHostEnrolmentSecretAccess(ctx, other, withKey)
		}, "30s", "200ms").
			Should(MatchError(ContainSubstring("get secrets in all namespaces, list secrets in all namespaces, watch secrets in all namespaces")))
	})
})

// authorizationSSAR asks one SelfSubjectAccessReview on Secrets.
type authorizationSSAR struct{ c client.Client }

// allowed reports whether the client's user may use verb on Secrets in ns
// (name: one Secret, or "" for all).
func (r *authorizationSSAR) allowed(ctx context.Context, ns, verb, name string) bool {
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Verb: verb, Resource: secretsResource, Name: name},
	}}
	return r.c.Create(ctx, review) == nil && review.Status.Allowed
}
