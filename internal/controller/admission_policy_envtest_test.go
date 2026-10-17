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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// This envtest spec installs the reserved-keys ValidatingAdmissionPolicy
// shipped in config/admission-policy (the chart renders the same CEL) on the
// envtest apiserver, bound to one test namespace, and drives it as a tenant
// and as the manager's ServiceAccount (impersonated): the tenant cannot add or
// change a manager-owned key, can set the keys users own, can update other
// fields of a VirtualMachine the manager stamped and can remove a stamp; the
// manager can set them all.
var _ = Describe("Reserved virtrigaud.io keys admission policy (envtest)", func() {
	const (
		kustomizeManager = "system:serviceaccount:virtrigaud-system:virtrigaud-controller-manager"
		tenantUser       = "tenant-alice"
		breakGlassGroup  = "vap-test-break-glass"
		breakGlassUser   = "bob-admin"
	)

	It("lets only the manager add or change the manager-owned keys", func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "vap-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns := nsObj.Name
		nsObj.Labels = map[string]string{"vap-test": ns}
		Expect(k8sClient.Update(ctx, nsObj)).To(Succeed())
		managerUser := "system:serviceaccount:" + ns + ":manager"

		// The shipped policy, its manager identity pointed at this spec's
		// ServiceAccount, plus the chart's optional exempt-group condition;
		// the binding limited to this namespace.
		policy, binding := loadReservedKeysPolicy(GinkgoT())
		policy.Name = "reserved-keys-" + ns
		Expect(policy.Spec.MatchConditions).To(HaveLen(1))
		Expect(policy.Spec.MatchConditions[0].Expression).To(ContainSubstring(kustomizeManager))
		policy.Spec.MatchConditions[0].Expression = strings.ReplaceAll(policy.Spec.MatchConditions[0].Expression,
			kustomizeManager, managerUser)
		policy.Spec.MatchConditions = append(policy.Spec.MatchConditions, admissionregistrationv1.MatchCondition{
			Name: "not-an-exempt-group",
			Expression: "!(has(request.userInfo.groups) && request.userInfo.groups.exists(g, g in [\"" +
				breakGlassGroup + "\"]))",
		})
		binding.Name = policy.Name
		binding.Spec.PolicyName = policy.Name
		binding.Spec.MatchResources = &admissionregistrationv1.MatchResources{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"vap-test": ns}},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		Expect(k8sClient.Create(ctx, binding)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, binding)
			_ = k8sClient.Delete(ctx, policy)
		})

		// Every identity used here may manage VirtualMachines in ns (RBAC).
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-editor", Namespace: ns},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{"infra.virtrigaud.io"}, Resources: []string{"virtualmachines"},
				Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}}},
		}
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vm-editor", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
			Subjects: []rbacv1.Subject{
				{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: tenantUser},
				{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: breakGlassUser},
				{Kind: rbacv1.ServiceAccountKind, Name: "manager", Namespace: ns},
			},
		})).To(Succeed())

		as := func(user string, groups ...string) client.Client {
			c := rest.CopyConfig(cfg)
			c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
			cl, err := client.New(c, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			return cl
		}
		tenant, manager, breakGlass := as(tenantUser), as(managerUser), as(breakGlassUser, breakGlassGroup)

		vm := func(name string, annotations, labels map[string]string) *infravirtrigaudiov1beta1.VirtualMachine {
			return &infravirtrigaudiov1beta1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations, Labels: labels},
				Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"},
					ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				},
			}
		}
		expectForbidden := func(err error, key string) {
			ExpectWithOffset(1, err).To(HaveOccurred())
			ExpectWithOffset(1, apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(), err.Error())
			ExpectWithOffset(1, err.Error()).To(ContainSubstring(key))
			ExpectWithOffset(1, err.Error()).To(ContainSubstring("reserved for the VirtRigaud manager"))
		}

		// The policy takes effect asynchronously: probe with a fresh name each
		// time until the policy itself refuses it (a probe admitted before
		// that is simply left behind).
		Eventually(func() string {
			probe := vm("", map[string]string{migrationProvenanceAnnotation: ns + "/mig"}, nil)
			probe.GenerateName = "probe-"
			if err := tenant.Create(ctx, probe); err != nil {
				return err.Error()
			}
			return ""
		}, "30s", "250ms").Should(ContainSubstring("reserved for the VirtRigaud manager"))

		By("a tenant cannot create a VirtualMachine carrying a manager-owned key")
		for _, key := range managerOwnedVMAnnotations {
			expectForbidden(tenant.Create(ctx, vm("planted", map[string]string{key: "forged"}, nil)), key)
		}
		for _, key := range managerOwnedVMLabels {
			expectForbidden(tenant.Create(ctx, vm("planted", nil, map[string]string{key: "true"})), key)
		}

		By("a tenant can set the keys users own")
		userKeys := map[string]string{}
		for _, key := range userSetVMKeys {
			userKeys[key] = "true"
		}
		userKeys["team"] = "a"
		Expect(tenant.Create(ctx, vm("tenant-vm", userKeys, map[string]string{"app": "web"}))).To(Succeed())

		By("the manager can set them; a tenant can then edit other fields and remove a stamp, never add or change one")
		stamped := vm("migrated", map[string]string{
			migrationProvenanceAnnotation: ns + "/mig", migrationTargetClaimAnnotation: "token",
		}, map[string]string{AdoptedLabel: AdoptedLabelValue})
		Expect(manager.Create(ctx, stamped)).To(Succeed())

		edit := func(c client.Client, mutate func(*infravirtrigaudiov1beta1.VirtualMachine)) error {
			cur := &infravirtrigaudiov1beta1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stamped), cur)).To(Succeed())
			mutate(cur)
			return c.Update(ctx, cur)
		}
		Expect(edit(tenant, func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			v.Annotations["team"] = "b"
			v.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
		})).To(Succeed(), "an update leaving the reserved keys as they are")
		expectForbidden(edit(tenant, func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			v.Annotations[migrationProvenanceAnnotation] = ns + "/other"
		}), migrationProvenanceAnnotation)
		expectForbidden(edit(tenant, func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			v.Annotations[migrationCompletedAnnotation] = "true"
		}), migrationCompletedAnnotation)
		expectForbidden(edit(tenant, func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			v.Labels[AdoptedLabel] = "false"
		}), AdoptedLabel)
		Expect(edit(tenant, func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			delete(v.Annotations, migrationTargetClaimAnnotation)
		})).To(Succeed(), "removing a stamp")
		Expect(edit(manager, func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			v.Annotations[migrationCompletedAnnotation] = "true"
		})).To(Succeed(), "the manager marks it completed")

		By("an exempt group may set them (the chart's exemptGroups)")
		Expect(breakGlass.Create(ctx, vm("restored", map[string]string{migrationProvenanceAnnotation: ns + "/mig"}, nil))).
			To(Succeed())
	})
})
