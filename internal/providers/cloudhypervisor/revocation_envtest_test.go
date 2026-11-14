//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// revocationBound is the revocation latency the Provider is held to: from
// the write that removes a host from the inventory Secret to its
// connection closed and its calls refused. The measured values are logged.
const revocationBound = 2 * time.Second

// revocationRounds is how many revocations are measured.
const revocationRounds = 20

// TestRevocationThroughSecretWatchEnvtest runs the Provider against a real
// API server, as its own ServiceAccount, with only the RBAC the chart
// renders for it (get and watch on its inventory Secret). It checks that
// the Provider cannot read another Secret or list Secrets, and measures the
// revocation latency: each round removes a host from the Secret and times
// until that host's connection is closed and a call for it is refused, then
// puts the host back.
func TestRevocationThroughSecretWatchEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI: the test applies the chart's RBAC")
		}
		t.Skip("helm not installed")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })
	admin, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	c := context.Background()

	const ns, provider = "infra", "ch"
	secretName := provider + "-hosts"
	sa := "virtrigaud-provider-" + ns + "-" + provider
	_, err = admin.CoreV1().Namespaces().Create(c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = admin.CoreV1().ServiceAccounts(ns).Create(c, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa}}, metav1.CreateOptions{})
	require.NoError(t, err)
	applyChartRBAC(t, helm, cfg, ns, provider)

	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	b := newFakeAgent(t, p, "host-b", fakeAgentOptions{})
	both, onlyA := inventorySecret(t, a, b), inventorySecret(t, a)
	both.Namespace, both.Name, onlyA.Name = ns, secretName, secretName
	_, err = admin.CoreV1().Secrets(ns).Create(c, both, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = admin.CoreV1().Secrets(ns).Create(c, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other"},
		StringData: map[string]string{"k": "v"}}, metav1.CreateOptions{})
	require.NoError(t, err)

	pcfg := rest.CopyConfig(cfg)
	pcfg.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + ns + ":" + sa,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"},
	}
	pcs, err := kubernetes.NewForConfig(pcfg)
	require.NoError(t, err)
	secrets := pcs.CoreV1().Secrets(ns)
	// RBAC objects reach the authorizer asynchronously.
	require.Eventually(t, func() bool { _, err := secrets.Get(c, secretName, metav1.GetOptions{}); return err == nil },
		30*time.Second, 100*time.Millisecond)

	_, err = secrets.Get(c, "other", metav1.GetOptions{})
	assert.True(t, apierrors.IsForbidden(err), "another Secret is not readable: %v", err)
	_, err = secrets.List(c, metav1.ListOptions{})
	assert.True(t, apierrors.IsForbidden(err), "Secrets cannot be listed: %v", err)
	_, err = secrets.List(c, metav1.ListOptions{FieldSelector: "metadata.name=" + secretName})
	assert.True(t, apierrors.IsForbidden(err), "not even the inventory Secret can be listed: %v", err)
	_, err = secrets.Update(c, onlyA, metav1.UpdateOptions{})
	assert.True(t, apierrors.IsForbidden(err), "the inventory Secret cannot be written: %v", err)

	prov, err := NewFromSecret(c, secrets, secretName, Options{ClientCertificate: p.client, Logger: quiet()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = prov.Close() })
	client := providerv1.NewProviderClient(served(t, prov))
	assert.Equal(t, []string{"host-a", "host-b"}, prov.pool.ids())

	var latencies []time.Duration
	for round := 0; round < revocationRounds; round++ {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
		require.NoError(t, err, "round %d: the host is routed", round)
		require.EqualValues(t, 1, b.lis.open.Load(), "round %d", round)
		accepted := b.lis.accepted.Load()

		start := time.Now()
		_, err = admin.CoreV1().Secrets(ns).Update(c, onlyA.DeepCopy(), metav1.UpdateOptions{})
		require.NoError(t, err)
		latencies = append(latencies, waitRevoked(t, start, func() bool { return b.lis.open.Load() == 0 && len(prov.pool.ids()) == 1 }))
		_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
		requireHostUnavailable(t, err, CauseNotFronted)
		_, err = client.Power(ctx(t), signedPower(t, "host-b"))
		requireHostUnavailable(t, err, CauseNotFronted)
		assert.Equal(t, accepted, b.lis.accepted.Load(), "round %d: no new connection to the revoked host", round)

		_, err = admin.CoreV1().Secrets(ns).Update(c, both.DeepCopy(), metav1.UpdateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return len(prov.pool.ids()) == 2 }, testTimeout, time.Millisecond)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50, p95, worst := latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)-1]
	t.Logf("revocation latency over %d rounds (Secret write to connection closed and host refused): p50 %s, p95 %s, max %s",
		len(latencies), p50, p95, worst)
	assert.Less(t, worst, revocationBound, "every revocation within the bound")
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, a.lis.accepted.Load(), "the other host kept its connection throughout")
}

// waitRevoked polls done every millisecond and returns the time from start
// until it held; it fails the test after ten times the bound.
func waitRevoked(t *testing.T, start time.Time, done func() bool) time.Duration {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(10*revocationBound - time.Since(start))
	defer deadline.Stop()
	for !done() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("the revoked host was not refused")
		}
	}
	return time.Since(start)
}

// applyChartRBAC creates the RBAC objects the chart renders for the
// Provider's inventory Secret watch.
func applyChartRBAC(t *testing.T, helm string, cfg *rest.Config, ns, provider string) {
	t.Helper()
	out, err := exec.Command(helm, "template", "t", filepath.Join("..", "..", "..", "charts", "virtrigaud"), // #nosec G204 -- test-controlled arguments
		"--show-only", "templates/agent-provider-inventory-rbac.yaml",
		"--set", "experimental.agentProviderInventoryWatch.enabled=true",
		"--set", "experimental.agentProviderInventoryWatch.developmentBuild=true",
		"--set", "experimental.agentProviderInventoryWatch.providers[0].namespace="+ns,
		"--set", "experimental.agentProviderInventoryWatch.providers[0].name="+provider).CombinedOutput()
	require.NoError(t, err, string(out))
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	n := 0
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); err != nil {
			break
		}
		if len(u.Object) == 0 {
			continue
		}
		gvr := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: map[string]string{
			"Role": "roles", "RoleBinding": "rolebindings"}[u.GetKind()]}
		require.NotEmpty(t, gvr.Resource, u.GetKind())
		_, err := dyn.Resource(gvr).Namespace(u.GetNamespace()).Create(context.Background(), u, metav1.CreateOptions{})
		require.NoError(t, err)
		n++
	}
	require.Equal(t, 2, n, "a Role and a RoleBinding")
}
