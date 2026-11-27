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
//     principal at all, the manager included (the kubelet mounts it);
//  3. no principal holds a path to the key through a workload in the key's
//     namespace (create, update or patch of pods, deployments, statefulsets,
//     daemonsets, replicasets, jobs or cronjobs; pods/exec or pods/attach;
//     serviceaccounts/token), or anywhere through impersonation or bind or
//     escalate on roles, except the entries of keyPathExceptions: the
//     manager's own pre-existing workload rights, which expose the sub-CA
//     key in the same way and are a stated custody limit;
//  4. no role that nothing binds, and no role aggregated into the built-in
//     user roles, grants any of the above, since anyone could be given it;
//  5. a binding to a role the set does not define (a built-in role) counts
//     as granting everything.
//
// Wildcards in apiGroups, resources and verbs count as matches; a rule
// limited by resourceNames counts as granting writes and workload paths, and
// as granting reads when it names the key Secret.

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

// rbacAccess is one access the proof looks for.
type rbacAccess struct {
	group, resource, verb string
	// name is the object name ("" with anyName: any object).
	name string
	// anyName counts a rule limited by resourceNames as granting.
	anyName bool
}

// String implements fmt.Stringer.
func (a rbacAccess) String() string {
	what := a.resource
	if a.group != "" {
		what += "." + a.group
	}
	if a.name != "" {
		what += " " + a.name
	}
	return a.verb + " " + what
}

// ruleGrants reports whether rule grants a.
func ruleGrants(rule rbacv1.PolicyRule, a rbacAccess) bool {
	match := func(list []string, v string) bool { return slices.Contains(list, v) || slices.Contains(list, "*") }
	resourceMatch := slices.ContainsFunc(rule.Resources, func(r string) bool {
		if r == "*" || r == a.resource {
			return true
		}
		if _, sub, ok := strings.Cut(a.resource, "/"); ok && r == "*/"+sub {
			return true
		}
		return false
	})
	if !match(rule.APIGroups, a.group) || !resourceMatch || !match(rule.Verbs, a.verb) {
		return false
	}
	return a.anyName || len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, a.name)
}

// rulesGrant reports whether any rule grants a.
func rulesGrant(rules []rbacv1.PolicyRule, a rbacAccess) bool {
	return slices.ContainsFunc(rules, func(r rbacv1.PolicyRule) bool { return ruleGrants(r, a) })
}

// grantVia is how a subject holds an access: the binding and the role.
type grantVia struct{ binding, roleKind, role string }

// holders returns the subjects granted a in namespace ns ("" : cluster
// scope only, through ClusterRoleBindings), with how. A binding to a role the
// set does not define grants everything.
func (s *rbacSet) holders(ns string, a rbacAccess) map[rbacSubject][]grantVia {
	out := map[rbacSubject][]grantVia{}
	for _, b := range s.bindings {
		if b.namespace != "" && b.namespace != ns {
			continue
		}
		var rules []rbacv1.PolicyRule
		var known bool
		switch b.ref.Kind {
		case "ClusterRole":
			cr, ok := s.clusterRoles[b.ref.Name]
			rules, known = cr.Rules, ok
		case "Role":
			r, ok := s.roles[b.namespace+"/"+b.ref.Name]
			rules, known = r.Rules, ok
		}
		if known && !rulesGrant(rules, a) {
			continue
		}
		for _, sub := range b.subjects {
			out[sub] = append(out[sub], grantVia{binding: b.name, roleKind: b.ref.Kind, role: b.ref.Name})
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

// keyPathAccesses are the accesses in the key Secret's namespace through
// which a principal can read the key without Secret RBAC (a workload that
// mounts it, a running pod entered, a token of a ServiceAccount there), and
// the cluster-wide ones that let a principal grant itself any access.
func keyPathAccesses() (inKeyNamespace, clusterWide []rbacAccess) {
	writes := []string{"create", "update", "patch"}
	for _, w := range []struct{ group, resource string }{
		{"", "pods"}, {"apps", "deployments"}, {"apps", "statefulsets"}, {"apps", "daemonsets"},
		{"apps", "replicasets"}, {"batch", "jobs"}, {"batch", "cronjobs"},
	} {
		for _, v := range writes {
			inKeyNamespace = append(inKeyNamespace, rbacAccess{group: w.group, resource: w.resource, verb: v, anyName: true})
		}
	}
	for _, sub := range []string{"pods/exec", "pods/attach"} {
		for _, v := range []string{"create", "get"} {
			inKeyNamespace = append(inKeyNamespace, rbacAccess{resource: sub, verb: v, anyName: true})
		}
	}
	inKeyNamespace = append(inKeyNamespace, rbacAccess{resource: "serviceaccounts/token", verb: "create", anyName: true},
		rbacAccess{resource: "serviceaccounts", verb: "impersonate", anyName: true})
	for _, r := range []string{"users", "groups", "serviceaccounts"} {
		clusterWide = append(clusterWide, rbacAccess{resource: r, verb: "impersonate", anyName: true})
	}
	for _, r := range []string{"roles", "clusterroles"} {
		for _, v := range []string{"bind", "escalate"} {
			a := rbacAccess{group: "rbac.authorization.k8s.io", resource: r, verb: v, anyName: true}
			clusterWide = append(clusterWide, a)
			inKeyNamespace = append(inKeyNamespace, a)
		}
	}
	return inKeyNamespace, clusterWide
}

// keyPathException is one access through which a principal could reach the
// key and that the proof accepts by name: the manager's own pre-existing
// rights, documented in docs/host-agent-enrolment.md ("Host ledger keys")
// as a custody limit shared with the sub-CA key.
type keyPathException struct {
	// The holder is always one of the manager identities passed to the
	// proof.
	group    string
	resource string
	verbs    []string
	// roleSuffix names the granting ClusterRole by its suffix (the chart's
	// is <release>-virtrigaud-manager, config/rbac's manager-role).
	roleSuffixes []string
	// why documents the exception.
	why string
}

// keyPathExceptions are the accepted key paths. The provider controller
// creates and updates provider Deployments, so the manager may create or
// change a Deployment in any namespace, the key's included; a stolen manager
// ServiceAccount can therefore mount the key Secret into a workload. That
// is the same limit that applies to the sub-CA key Secret, and stays until
// the key is held by a custody service.
var keyPathExceptions = []keyPathException{{
	group: "apps", resource: "deployments", verbs: []string{"create", "update", "patch"},
	roleSuffixes: []string{"-virtrigaud-manager", "manager-role"},
	why:          "the provider controller reconciles provider Deployments in every namespace (pre-existing manager right)",
}}

// excepted reports whether a holder of a is accepted.
func excepted(sub rbacSubject, isManager func(rbacSubject) bool, a rbacAccess, via []grantVia) bool {
	if !isManager(sub) {
		return false
	}
	for _, e := range keyPathExceptions {
		if e.group != a.group || e.resource != a.resource || !slices.Contains(e.verbs, a.verb) {
			continue
		}
		all := true
		for _, v := range via {
			ok := v.roleKind == "ClusterRole" && slices.ContainsFunc(e.roleSuffixes, func(suf string) bool { return strings.HasSuffix(v.role, suf) })
			all = all && ok
		}
		if all {
			return true
		}
	}
	return false
}

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

// assertHostLedgerRBAC proves the properties of the file comment for one
// rendering. managers are the manager ServiceAccount's identities.
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
		for sub, via := range s.holders(enrolmentNS, rbacAccess{resource: "secrets", verb: verb, anyName: true}) {
			assert.True(t, isManager(sub), "%s may %s Secrets in the enrolment namespace %s through %v", sub, verb, enrolmentNS, via)
			writers++
		}
	}
	assert.Positive(t, writers, "the proof is not vacuous: the manager writes the enrolment namespace's Secrets")
	for _, verb := range secretReadVerbs {
		for sub, via := range s.holders(keyNS, rbacAccess{resource: "secrets", verb: verb, name: keySecret}) {
			t.Errorf("%s may %s the host ledger key Secret %s/%s through the API (%v); no principal may, the manager included", sub, verb,
				keyNS, keySecret, via)
		}
	}
	inKeyNS, clusterWide := keyPathAccesses()
	check := func(ns string, a rbacAccess) {
		for sub, via := range s.holders(ns, a) {
			if !excepted(sub, isManager, a, via) {
				t.Errorf("%s may %s in the key's namespace %s through %v: a path to the host ledger key", sub, a, keyNS, via)
			}
		}
	}
	for _, a := range inKeyNS {
		check(keyNS, a)
	}
	for _, a := range clusterWide {
		check("", a)
	}
	grantsAny := func(rules []rbacv1.PolicyRule) bool {
		for _, v := range secretWriteVerbs {
			if rulesGrant(rules, rbacAccess{resource: "secrets", verb: v, anyName: true}) {
				return true
			}
		}
		for _, v := range secretReadVerbs {
			if rulesGrant(rules, rbacAccess{resource: "secrets", verb: v, name: keySecret}) {
				return true
			}
		}
		for _, a := range append(slices.Clone(inKeyNS), clusterWide...) {
			if rulesGrant(rules, a) {
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
			assert.False(t, grantsAny(cr.Rules), "ClusterRole %s is unbound or aggregated into user roles and grants a key path", name)
		}
		assert.Nil(t, cr.AggregationRule, "ClusterRole %s aggregates other roles; the proof cannot see their rules", name)
	}
	for key, r := range s.roles {
		if !s.bound(r.Namespace, r.Name) {
			assert.False(t, grantsAny(r.Rules), "Role %s is unbound and grants a key path", key)
		}
	}
}

// hostLedgerChartValues are the chart values of the proof's rendering: per-host
// agent enrolment, the key Secret, the namespaced Secret grant and the
// inventory watch.
var hostLedgerChartValues = []string{
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

// TestRBAC_HostLedgerProofChart renders the chart with per-host agent
// enrolment, the host ledger key Secret and every development RBAC option,
// and proves the host ledger properties over everything it renders.
func TestRBAC_HostLedgerProofChart(t *testing.T) {
	base := hostLedgerChartValues
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
			assert.Contains(t, string(doc), "fsGroup: 65532", "the 0400 key is readable by the manager through its fsGroup")
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
		"in the enrolment namespace": {append(slices.Clone(base), "--set", "experimental.hostEnrolment.namespace=virtrigaud-system"),
			"is the release namespace"},
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
// the manager's namespace. config/rbac/role.yaml grants the manager Secret
// access in all namespaces, which per-host agent enrolment requires to be
// replaced with per-namespace Roles first (docs/manager-secret-access.md,
// "Kustomize"; the manager refuses enrolment otherwise); the proof removes
// exactly that rule, and checks it is the only Secret rule it removes.
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
	mgrRole, ok := set.clusterRoles["manager-role"]
	require.True(t, ok)
	removed := 0
	var kept []rbacv1.PolicyRule
	for _, r := range mgrRole.Rules {
		if slices.Contains(r.Resources, "secrets") {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	require.Equal(t, 1, removed, "only the documented all-namespaces Secret rule is replaced")
	mgrRole.Rules = kept
	set.clusterRoles["manager-role"] = mgrRole
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
subjects: [{kind: ServiceAccount, name: mgr, namespace: sys}]`, `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: rel-virtrigaud-manager}
rules: [{apiGroups: [apps], resources: [deployments], verbs: [create, update, patch]}]`, `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: m}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: rel-virtrigaud-manager}
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
	binding := func(name, kind, role, subjects string) string {
		return `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: ` + name + `}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ` + kind + `, name: ` + role + `}
subjects: ` + subjects
	}
	for name, c := range map[string]struct {
		extra string
		role  *rbacv1.ClusterRole
	}{
		"another writer": {extra: `
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: x, namespace: pki}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: w}
subjects: [{kind: Group, name: ops}]`},
		"a built-in role": {extra: binding("x", "ClusterRole", "edit", "[{kind: User, name: alice}]")},
		"a key reader": {extra: binding("r", "ClusterRole", "r", "[{kind: ServiceAccount, name: other, namespace: sys}]"),
			role: &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"},
				Verbs: []string{"get"}, ResourceNames: []string{"key"}}}}},
		"the manager reading the key": {extra: binding("r", "ClusterRole", "r", "[{kind: ServiceAccount, name: mgr, namespace: sys}]"),
			role: &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"list"}}}}},
		"another principal creating pods": {extra: binding("r", "ClusterRole", "r", "[{kind: User, name: bob}]"),
			role: &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create"}}}}},
		"the manager creating jobs (not excepted)": {extra: binding("r", "ClusterRole", "r", "[{kind: ServiceAccount, name: mgr, namespace: sys}]"),
			role: &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create"}}}}},
		"the manager deploying through another role": {extra: binding("r", "ClusterRole", "r", "[{kind: ServiceAccount, name: mgr, namespace: sys}]"),
			role: &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"patch"}}}}},
		"exec into pods": {extra: binding("r", "ClusterRole", "r", "[{kind: Group, name: ops}]"),
			role: &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"*/exec"}, Verbs: []string{"create"}}}}},
		"service account tokens": {extra: binding("r", "ClusterRole", "r", "[{kind: Group, name: ops}]"),
			role: &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, Verbs: []string{"create"}}}}},
		"impersonation": {extra: binding("r", "ClusterRole", "r", "[{kind: ServiceAccount, name: mgr, namespace: sys}]"),
			role: &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"users"}, Verbs: []string{"impersonate"}}}}},
		"escalate": {extra: binding("r", "ClusterRole", "r", "[{kind: Group, name: ops}]"),
			role: &rbacv1.ClusterRole{Rules: []rbacv1.PolicyRule{{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterroles"}, Verbs: []string{"escalate"}}}}},
		"an unbound wildcard role": {extra: `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: any}
rules: [{apiGroups: ["*"], resources: ["*"], verbs: ["*"]}]`},
	} {
		t.Run(name, func(t *testing.T) {
			set := mk(c.extra)
			if c.role != nil {
				set.clusterRoles["r"] = *c.role
			}
			rec := &recordingT{}
			assertHostLedgerRBACInto(rec, set, "pki", "sys", "key", manager)
			assert.True(t, rec.failed, "the proof must fail for %s", name)
		})
	}
	rec := &recordingT{}
	assertHostLedgerRBACInto(rec, mk(""), "pki", "sys", "key", manager)
	assert.False(t, rec.failed, "the manager alone, with its excepted Deployment right, passes: %v", rec.msgs)
}
