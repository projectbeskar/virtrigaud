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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// The host ledger RBAC proof. The host ledger's tag defends against a
// principal that can write Secrets in the enrolment namespace only if that
// principal cannot also read the tag key; so the proof enumerates every
// Role, ClusterRole, RoleBinding and ClusterRoleBinding that the Helm chart
// renders (with per-host agent enrolment and the host ledger key Secret)
// and that config/rbac ships, and proves:
//
//  1. Secret create, update, patch, delete and deletecollection in the
//     enrolment namespace are granted to the manager ServiceAccount alone;
//  2. get, list and watch on the host ledger key Secret are granted to no
//     principal other than the manager ServiceAccount (which needs none: the
//     kubelet mounts it);
//  3. no role that nothing binds, and no role aggregated into the built-in
//     user roles, grants either, since anyone could be given it;
//  4. a binding to a role the set does not define (a built-in role) counts
//     as granting everything.
//
// Wildcards in apiGroups, resources and verbs count as matches; a rule
// limited by resourceNames counts as granting writes (ledger Secret names
// are derived from store ids) and as granting reads when it names the key
// Secret.

// rbacSubject is a binding subject, as kind/namespace/name.
type rbacSubject struct{ kind, namespace, name string }

// String implements fmt.Stringer.
func (s rbacSubject) String() string { return s.kind + ":" + s.namespace + "/" + s.name }

// rbacSet is the RBAC objects of one rendering.
type rbacSet struct {
	clusterRoles map[string]rbacv1.ClusterRole
	roles        map[string]rbacv1.Role // namespace/name
	bindings     []rbacBinding
}

// rbacBinding is a RoleBinding (namespace set) or ClusterRoleBinding.
type rbacBinding struct {
	namespace string // "" for a ClusterRoleBinding
	ref       rbacv1.RoleRef
	subjects  []rbacSubject
	name      string
}

// loadRBACSet collects the RBAC objects of YAML documents. defaultNS is the
// namespace of a namespaced object that names none (a kustomize base).
func loadRBACSet(t *testing.T, docs [][]byte, defaultNS string) *rbacSet {
	t.Helper()
	s := &rbacSet{clusterRoles: map[string]rbacv1.ClusterRole{}, roles: map[string]rbacv1.Role{}}
	nsOf := func(ns string) string {
		if ns == "" {
			return defaultNS
		}
		return ns
	}
	subjects := func(in []rbacv1.Subject, bindingNS string) []rbacSubject {
		var out []rbacSubject
		for _, sub := range in {
			ns := sub.Namespace
			if sub.Kind == rbacv1.ServiceAccountKind && ns == "" {
				ns = bindingNS
			}
			out = append(out, rbacSubject{kind: sub.Kind, namespace: ns, name: sub.Name})
		}
		return out
	}
	for _, doc := range docs {
		u := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal(doc, &u.Object))
		switch u.GetKind() {
		case "ClusterRole":
			var cr rbacv1.ClusterRole
			require.NoError(t, yaml.Unmarshal(doc, &cr))
			s.clusterRoles[cr.Name] = cr
		case "Role":
			var r rbacv1.Role
			require.NoError(t, yaml.Unmarshal(doc, &r))
			r.Namespace = nsOf(r.Namespace)
			s.roles[r.Namespace+"/"+r.Name] = r
		case "RoleBinding":
			var b rbacv1.RoleBinding
			require.NoError(t, yaml.Unmarshal(doc, &b))
			ns := nsOf(b.Namespace)
			s.bindings = append(s.bindings, rbacBinding{namespace: ns, ref: b.RoleRef, subjects: subjects(b.Subjects, ns), name: b.Name})
		case "ClusterRoleBinding":
			var b rbacv1.ClusterRoleBinding
			require.NoError(t, yaml.Unmarshal(doc, &b))
			s.bindings = append(s.bindings, rbacBinding{ref: b.RoleRef, subjects: subjects(b.Subjects, ""), name: b.Name})
		}
	}
	return s
}

// secretRuleGrants reports whether rule grants verb on core Secrets for an
// object named name ("" with forWrite: any object; see the file comment).
func secretRuleGrants(rule rbacv1.PolicyRule, verb, name string, forWrite bool) bool {
	match := func(list []string, v string) bool { return slices.Contains(list, v) || slices.Contains(list, "*") }
	if !match(rule.APIGroups, "") || !match(rule.Resources, "secrets") || !match(rule.Verbs, verb) {
		return false
	}
	return forWrite || len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, name)
}

// rulesGrant reports whether any rule grants verb on Secrets named name.
func rulesGrant(rules []rbacv1.PolicyRule, verb, name string, forWrite bool) bool {
	return slices.ContainsFunc(rules, func(r rbacv1.PolicyRule) bool { return secretRuleGrants(r, verb, name, forWrite) })
}

// holders returns the subjects granted verb on Secrets named name in
// namespace ns, through every binding. A binding to a role the set does not
// define grants everything.
func (s *rbacSet) holders(ns, verb, name string, forWrite bool) map[rbacSubject][]string {
	out := map[rbacSubject][]string{}
	for _, b := range s.bindings {
		if b.namespace != "" && b.namespace != ns {
			continue
		}
		var rules []rbacv1.PolicyRule
		known := true
		switch b.ref.Kind {
		case "ClusterRole":
			cr, ok := s.clusterRoles[b.ref.Name]
			rules, known = cr.Rules, ok
		case "Role":
			r, ok := s.roles[b.namespace+"/"+b.ref.Name]
			rules, known = r.Rules, ok
		default:
			known = false
		}
		if known && !rulesGrant(rules, verb, name, forWrite) {
			continue
		}
		for _, sub := range b.subjects {
			out[sub] = append(out[sub], b.name)
		}
	}
	return out
}

// bound reports whether a binding references the role (namespace "" for a
// ClusterRole).
func (s *rbacSet) bound(namespace, name string) bool {
	return slices.ContainsFunc(s.bindings, func(b rbacBinding) bool {
		if namespace == "" {
			return b.ref.Kind == "ClusterRole" && b.ref.Name == name
		}
		return b.ref.Kind == "Role" && b.namespace == namespace && b.ref.Name == name
	})
}

// secretWriteVerbs are the verbs that change or remove a Secret.
var secretWriteVerbs = []string{"create", "update", "patch", "delete", "deletecollection"}

// secretReadVerbs are the verbs that read a Secret.
var secretReadVerbs = []string{"get", "list", "watch"}

// rbacProofT is what the proof reports to (a *testing.T, or recordingT in
// the proof's own test).
type rbacProofT interface {
	assert.TestingT
	Helper()
}

// recordingT records failures instead of failing a test.
type recordingT struct {
	failed bool
	msgs   []string
}

// Errorf implements assert.TestingT.
func (r *recordingT) Errorf(format string, args ...any) {
	r.failed = true
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

// Helper implements rbacProofT.
func (r *recordingT) Helper() {}

// assertHostLedgerRBAC proves the four properties of the file comment for
// one rendering. managers are the manager ServiceAccount's identities.
func assertHostLedgerRBAC(t *testing.T, s *rbacSet, enrolmentNS, keyNS, keySecret string, managers ...rbacSubject) {
	t.Helper()
	assertHostLedgerRBACInto(t, s, enrolmentNS, keyNS, keySecret, managers...)
}

// assertHostLedgerRBACInto is assertHostLedgerRBAC reporting to t.
func assertHostLedgerRBACInto(t rbacProofT, s *rbacSet, enrolmentNS, keyNS, keySecret string, managers ...rbacSubject) {
	t.Helper()
	isManager := func(sub rbacSubject) bool { return slices.Contains(managers, sub) }
	writers := 0
	for _, verb := range secretWriteVerbs {
		for sub, via := range s.holders(enrolmentNS, verb, "", true) {
			assert.True(t, isManager(sub), "%s may %s Secrets in the enrolment namespace %s through %v", sub, verb, enrolmentNS, via)
			writers++
		}
	}
	assert.Positive(t, writers, "the proof is not vacuous: the manager writes the enrolment namespace's Secrets")
	for _, verb := range secretReadVerbs {
		for sub, via := range s.holders(keyNS, verb, keySecret, false) {
			assert.True(t, isManager(sub), "%s may %s the host ledger key Secret %s/%s through %v", sub, verb, keyNS, keySecret, via)
		}
	}
	grantsAny := func(rules []rbacv1.PolicyRule) bool {
		for _, v := range secretWriteVerbs {
			if rulesGrant(rules, v, "", true) {
				return true
			}
		}
		for _, v := range secretReadVerbs {
			if rulesGrant(rules, v, keySecret, false) {
				return true
			}
		}
		return false
	}
	for name, cr := range s.clusterRoles {
		aggregated := false
		for k := range cr.Labels {
			aggregated = aggregated || strings.HasPrefix(k, "rbac.authorization.k8s.io/aggregate-to-")
		}
		if aggregated || !s.bound("", name) {
			assert.False(t, grantsAny(cr.Rules), "ClusterRole %s is unbound or aggregated into user roles and grants Secret access", name)
		}
		assert.Nil(t, cr.AggregationRule, "ClusterRole %s aggregates other roles; the proof cannot see their rules", name)
	}
	for key, r := range s.roles {
		if !s.bound(r.Namespace, r.Name) {
			assert.False(t, grantsAny(r.Rules), "Role %s is unbound and grants Secret access", key)
		}
	}
}

// TestRBAC_HostLedgerProofChart renders the chart with per-host agent
// enrolment, the host ledger key Secret and every development RBAC option,
// and proves the host ledger properties over everything it renders.
func TestRBAC_HostLedgerProofChart(t *testing.T) {
	base := []string{
		"--set", "experimental.hostEnrolment.enabled=true",
		"--set", "experimental.hostEnrolment.developmentBuild=true",
		"--set", "experimental.hostEnrolment.namespace=host-agent-pki",
		"--set", "experimental.hostEnrolment.signerNames={clusterissuers.cert-manager.io/host-agent-ca}",
		"--set", "experimental.hostEnrolment.caKeySecret=cert-manager/host-agent-ca",
		"--set", "experimental.hostEnrolment.ledgerKeySecret=host-ledger-key",
		"--set", "rbac.secrets.scope=namespaced",
		"--set", "rbac.secrets.providerNamespaces={virt-a,virt-b}",
		"--set", "rbac.secrets.workloadNamespaces={tenant-1}",
		"--set", "experimental.agentProviderInventoryWatch.enabled=true",
		"--set", "experimental.agentProviderInventoryWatch.developmentBuild=true",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].namespace=virt-a",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].name=ch",
	}
	out, err := secretRBACRender(t, base...)
	require.NoError(t, err, out)
	docs := splitYAMLDocs(t, []byte(out))
	set := loadRBACSet(t, docs, "virtrigaud-system")
	require.NotEmpty(t, set.bindings)
	manager := rbacSubject{kind: rbacv1.ServiceAccountKind, namespace: "virtrigaud-system", name: "rel-virtrigaud"}
	assertHostLedgerRBAC(t, set, "host-agent-pki", "virtrigaud-system", "host-ledger-key", manager)

	// The key Secret is mounted into the manager pod and no other.
	mounts := 0
	for _, doc := range docs {
		if strings.Contains(string(doc), "secretName: host-ledger-key") {
			mounts++
			assert.Contains(t, string(doc), "app.kubernetes.io/component: manager")
			assert.Contains(t, string(doc), "defaultMode: 0400")
		}
	}
	assert.Equal(t, 1, mounts)

	// Off by default, and refused where it would be readable by others.
	off, err := secretRBACRender(t)
	require.NoError(t, err, off)
	assert.NotContains(t, off, "host-ledger-key")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"without enrolment": {[]string{"--set", "experimental.hostEnrolment.ledgerKeySecret=host-ledger-key"},
			"needs experimental.hostEnrolment.enabled=true"},
		"not a name": {append(slices.Clone(base), "--set", "experimental.hostEnrolment.ledgerKeySecret=Bad_Name"),
			"is not a Secret name"},
		"in a provider namespace": {append(slices.Clone(base), "--set", "rbac.secrets.providerNamespaces={virtrigaud-system}"),
			"keep the host ledger key Secret"},
		"in a workload namespace": {append(slices.Clone(base), "--set", "rbac.secrets.workloadNamespaces={virtrigaud-system}"),
			"keep the host ledger key Secret"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := secretRBACRender(t, c.args...)
			require.Error(t, err, out)
			assert.Contains(t, out, c.want)
		})
	}
}

// TestRBAC_HostLedgerProofConfig proves the host ledger properties over
// config/rbac (the kustomize RBAC set, before its namespace and name
// prefix) together with config/rbac/host-enrolment, with the key Secret in
// the manager's namespace.
func TestRBAC_HostLedgerProofConfig(t *testing.T) {
	dir := filepath.Join("..", "..", "config", "rbac")
	var k struct {
		Resources []string `json:"resources"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(raw, &k))
	var docs [][]byte
	for _, f := range k.Resources {
		b, err := os.ReadFile(filepath.Join(dir, f)) // #nosec G304 -- files listed by the repository's kustomization
		require.NoError(t, err)
		docs = append(docs, splitYAMLDocs(t, b)...)
	}
	set := loadRBACSet(t, docs, "system")
	he, err := os.ReadFile(filepath.Join(dir, "host-enrolment", "role.yaml"))
	require.NoError(t, err)
	heSet := loadRBACSet(t, splitYAMLDocs(t, he), "virtrigaud-system")
	for n, cr := range heSet.clusterRoles {
		set.clusterRoles[n] = cr
	}
	for n, r := range heSet.roles {
		set.roles[n] = r
	}
	set.bindings = append(set.bindings, heSet.bindings...)
	managers := []rbacSubject{
		{kind: rbacv1.ServiceAccountKind, namespace: "system", name: "controller-manager"},
		{kind: rbacv1.ServiceAccountKind, namespace: "virtrigaud-system", name: "virtrigaud-controller-manager"},
	}
	assertHostLedgerRBAC(t, set, "host-agent-pki", "system", "host-ledger-key", managers...)
	assertHostLedgerRBAC(t, set, "host-agent-pki", "virtrigaud-system", "host-ledger-key", managers...)
}

// The proof itself catches what it is for.
func TestRBAC_HostLedgerProofCatches(t *testing.T) {
	manager := rbacSubject{kind: rbacv1.ServiceAccountKind, namespace: "sys", name: "mgr"}
	mk := func(extra string) *rbacSet {
		docs := []string{`
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: w, namespace: pki}
rules: [{apiGroups: [""], resources: [secrets], verbs: [update]}]`, `
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: w, namespace: pki}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: w}
subjects: [{kind: ServiceAccount, name: mgr, namespace: sys}]`}
		if extra != "" {
			docs = append(docs, extra)
		}
		var raw [][]byte
		for _, d := range docs {
			raw = append(raw, []byte(d))
		}
		return loadRBACSet(t, raw, "sys")
	}
	for name, extra := range map[string]string{
		"another writer": `
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: x, namespace: pki}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: w}
subjects: [{kind: Group, name: ops}]`,
		"a built-in role": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: x}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: edit}
subjects: [{kind: User, name: alice}]`,
		"a key reader": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: r}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: r}
subjects: [{kind: ServiceAccount, name: other, namespace: sys}]
---`,
		"an unbound wildcard role": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: any}
rules: [{apiGroups: ["*"], resources: ["*"], verbs: ["*"]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			set := mk(extra)
			if name == "a key reader" {
				set.clusterRoles["r"] = rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"},
					Verbs: []string{"get"}, ResourceNames: []string{"key"}}}}
			}
			rec := &recordingT{}
			assertHostLedgerRBACInto(rec, set, "pki", "sys", "key", manager)
			assert.True(t, rec.failed, "the proof must fail for %s", name)
		})
	}
	rec := &recordingT{}
	assertHostLedgerRBACInto(rec, mk(""), "pki", "sys", "key", manager)
	assert.False(t, rec.failed, "the manager alone passes: %v", rec.msgs)
}
