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

		// waitForChartRBAC waits until the Roles and the manager's
		// ClusterRole or Role are in force: a pass before that would prove
		// nothing. managerNS is a namespace the manager's own rules cover.
		waitForChartRBAC := func(c client.Client, managerNS string) {
			Eventually(func() bool {
				r := &authorizationSSAR{c: c}
				return r.allowed(ctx, enrolNS, "create", "")
			}, "30s", "200ms").Should(BeTrue())
			Eventually(func() ([]string, error) {
				return HostEnrolmentWorkloadAccess(ctx, c, &HostEnrolmentConfig{CAKeySecret: &types.NamespacedName{Namespace: managerNS, Name: key.Name}})
			}, "30s", "200ms").Should(ContainElement("create deployments.apps in namespace " + managerNS))
		}

		By("The chart's RBAC with rbac.scope=cluster passes")
		waitForChartRBAC(mgr, keyNS)
		Expect(CheckHostEnrolmentSecretAccess(ctx, mgr, withKey)).To(Succeed(),
			"the chart's manager and enrolment RBAC grant no Secret read outside the enrolment namespace and no escalation primitive")

		By("The chart's cluster-scoped manager may create and change Deployments in the key namespace: a warning, not a refusal")
		workload, err := HostEnrolmentWorkloadAccess(ctx, mgr, withKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(workload).To(Equal([]string{"create deployments.apps in namespace " + keyNS,
			"update deployments.apps in namespace " + keyNS, "patch deployments.apps in namespace " + keyNS}))

		By("The chart's RBAC with rbac.scope=namespace passes, with no workload access in the key namespace")
		namespaced := apply(relNS+"-ns", append([]string{"--set", "rbac.scope=namespace"}, enrolment...)...)
		waitForChartRBAC(namespaced, relNS)
		Expect(CheckHostEnrolmentSecretAccess(ctx, namespaced, withKey)).To(Succeed())
		workload, err = HostEnrolmentWorkloadAccess(ctx, namespaced, withKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(workload).To(BeEmpty())

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

		By("A Role to impersonate ServiceAccounts in the key namespace refuses")
		grant("impersonate-sa", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"impersonate"}},
			"impersonate serviceaccounts in namespace "+keyNS)

		By("Grants limited by resourceNames, which an access review cannot match, refuse through the rules review")
		grant("impersonate-masters", true, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"groups"}, Verbs: []string{"impersonate"},
			ResourceNames: []string{"system:masters"}}, "impersonate groups through an RBAC rule limited to system:masters")
		grant("impersonate-user", true, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"users"}, Verbs: []string{"impersonate"},
			ResourceNames: []string{"admin"}}, "impersonate users through an RBAC rule limited to admin")
		grant("token-one-sa", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, Verbs: []string{"create"},
			ResourceNames: []string{"cert-manager"}}, "create serviceaccounts/token through an RBAC rule limited to cert-manager")
		grant("bind-cluster-admin", true, rbacv1.PolicyRule{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"clusterroles"}, Verbs: []string{"bind"},
			ResourceNames: []string{"cluster-admin"}}, "bind clusterroles.rbac.authorization.k8s.io through an RBAC rule limited to cluster-admin")
		grant("patch-one-binding", false, rbacv1.PolicyRule{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"rolebindings"}, Verbs: []string{"patch"},
			ResourceNames: []string{"cert-manager"}}, "patch rolebindings.rbac.authorization.k8s.io through an RBAC rule limited to cert-manager")

		By("Grants to enter pods in the key namespace refuse, also when limited to one pod")
		grant("exec", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"}},
			"create pods/exec in namespace "+keyNS)
		grant("attach-one-pod", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/attach"}, Verbs: []string{"create"},
			ResourceNames: []string{"cainjector"}}, "create pods/attach through an RBAC rule limited to cainjector")
		grant("ephemeral", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/ephemeralcontainers"}, Verbs: []string{"patch"}},
			"patch pods/ephemeralcontainers in namespace "+keyNS)

		By("Grants to change a running pod in the key namespace refuse, also when limited to one pod")
		grant("patch-pods", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"patch"}},
			"patch pods in namespace "+keyNS)
		grant("update-one-pod", false, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"update"},
			ResourceNames: []string{"cainjector"}}, "update pods through an RBAC rule limited to cainjector")

		By("Grants on nodes/proxy refuse, also when limited to one node")
		grant("node-proxy", true, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes/proxy"}, Verbs: []string{"get"}},
			"get nodes/proxy in all namespaces")
		grant("one-node-proxy", true, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes/proxy"}, Verbs: []string{"create"},
			ResourceNames: []string{"node-1"}}, "create nodes/proxy through an RBAC rule limited to node-1")

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
