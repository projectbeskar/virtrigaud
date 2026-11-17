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
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
)

// These specs apply the deny-only admission sample for the manager's own
// CertificateRequests (config/rbac/host-enrolment/admission) to a real API
// server, with its placeholders replaced: the test's own user stands for the
// manager's ServiceAccount, and the policy is narrowed to this spec's two
// namespaces so that it cannot touch other specs.
var _ = Describe("Manager CertificateRequest admission sample (envtest)", func() {
	ctx := context.Background()

	It("denies every request of the manager's identity but a renewal of the manager's shape", func() {
		review := &authenticationv1.SelfSubjectReview{}
		Expect(k8sClient.Create(ctx, review)).To(Succeed())
		user := review.Status.UserInfo.Username
		Expect(user).NotTo(BeEmpty())

		var nss []string
		for _, p := range []string{"adm-pki-", "adm-other-"} {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: p}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
			nss = append(nss, ns.Name)
		}
		pki, other := nss[0], nss[1]

		raw, err := os.ReadFile(filepath.Join(hostEnrolmentAdmissionDir, "manager-certificaterequests.yaml"))
		Expect(err).NotTo(HaveOccurred())
		suffix := "-" + pki
		text := strings.NewReplacer(
			"system:serviceaccount:virtrigaud-system:virtrigaud-controller-manager", user,
			"'host-agent-pki'", "'"+pki+"'",
			"name: virtrigaud-host-agent-manager-requests", "name: virtrigaud-host-agent-manager-requests"+suffix,
			"policyName: virtrigaud-host-agent-manager-requests", "policyName: virtrigaud-host-agent-manager-requests"+suffix,
		).Replace(string(raw))
		docs := strings.Split(text, "\n---\n")
		Expect(docs).To(HaveLen(2))
		vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
		Expect(yaml.UnmarshalStrict([]byte(docs[0]), vap)).To(Succeed())
		vap.Spec.MatchConditions = append(vap.Spec.MatchConditions, admissionregistrationv1.MatchCondition{
			Name: "this-spec-only", Expression: "request.namespace == '" + pki + "' || request.namespace == '" + other + "'"})
		bind := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
		Expect(yaml.UnmarshalStrict([]byte(docs[1]), bind)).To(Succeed())
		Expect(k8sClient.Create(ctx, vap)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, vap) })
		Expect(k8sClient.Create(ctx, bind)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, bind) })

		renewal := func(ns string, mutate func(u *unstructured.Unstructured)) *unstructured.Unstructured {
			u := certrequest.NewRenewal(certrequest.RenewalSpec{
				Namespace: ns, Name: "", HostNamespace: "infra", HostName: "kvm-01", HostHash: strings.Repeat("ab", 16),
				IssuerRef: certrequest.IssuerRef{Group: certrequest.Group, Kind: "ClusterIssuer", Name: "host-agent-ca"},
				CSRPEM:    []byte("-----BEGIN CERTIFICATE REQUEST-----\nAA==\n-----END CERTIFICATE REQUEST-----\n"),
				Duration:  30 * 24 * time.Hour, Username: user,
			})
			u.SetName("")
			u.SetGenerateName("adm-")
			if mutate != nil {
				mutate(u)
			}
			return u
		}
		set := func(u *unstructured.Unstructured, v any, path ...string) {
			Expect(unstructured.SetNestedField(u.Object, v, append([]string{"spec"}, path...)...)).To(Succeed())
		}
		denied := func(u *unstructured.Unstructured) bool {
			err := k8sClient.Create(ctx, u)
			if err == nil {
				_ = k8sClient.Delete(ctx, u)
				return false
			}
			return apierrors.IsInvalid(err) || apierrors.IsForbidden(err)
		}

		// The policy is applied asynchronously: wait until it denies.
		Eventually(func() bool {
			return denied(renewal(pki, func(u *unstructured.Unstructured) { set(u, true, "isCA") }))
		}, 30*time.Second, 200*time.Millisecond).Should(BeTrue(), "a CA request is denied")

		Expect(k8sClient.Create(ctx, renewal(pki, nil))).To(Succeed(), "the manager's renewal request is admitted")

		for name, mutate := range map[string]func(u *unstructured.Unstructured){
			"another issuer": func(u *unstructured.Unstructured) {
				set(u, "other", "issuerRef", "name")
			},
			"a namespaced Issuer of the same name": func(u *unstructured.Unstructured) {
				set(u, "Issuer", "issuerRef", "kind")
			},
			"an extra usage": func(u *unstructured.Unstructured) {
				set(u, []any{"digital signature", "server auth", "client auth"}, "usages")
			},
			"a missing usage": func(u *unstructured.Unstructured) {
				set(u, []any{"server auth"}, "usages")
			},
			"more than 30 days": func(u *unstructured.Unstructured) { set(u, "721h0m0s", "duration") },
			"no lifetime":       func(u *unstructured.Unstructured) { unstructured.RemoveNestedField(u.Object, "spec", "duration") },
			"no renewal label": func(u *unstructured.Unstructured) {
				u.SetLabels(map[string]string{certrequest.LabelAgentEnrolment: "true"})
			},
			"no Host annotation": func(u *unstructured.Unstructured) { u.SetAnnotations(nil) },
		} {
			Expect(denied(renewal(pki, mutate))).To(BeTrue(), name)
		}
		Expect(denied(renewal(other, nil))).To(BeTrue(), "outside the enrolment namespace")
	})
})
