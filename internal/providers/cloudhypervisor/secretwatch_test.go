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
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	obsmetrics "github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	testSecretNS   = "infra"
	testSecretName = "ch-hosts"
)

// inventorySecret is the inventory Secret fronting agents.
func inventorySecret(t *testing.T, agents ...*fakeAgent) *corev1.Secret {
	t.Helper()
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	for _, a := range agents {
		inv.Hosts = append(inv.Hosts, a.entry())
	}
	data, err := hostsecret.Marshal(inv)
	require.NoError(t, err)
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testSecretNS, Name: testSecretName},
		Data: map[string][]byte{hostsecret.SecretDataKey: data}}
}

// secretProvider returns a Provider following the inventory Secret through
// cs, closed when t ends.
func secretProvider(t *testing.T, p *pki, cs *fake.Clientset, o Options) *Provider {
	t.Helper()
	o.ClientCertificate = p.client
	if o.Logger == nil {
		o.Logger = quiet()
	}
	prov, err := NewFromSecret(ctx(t), cs.CoreV1().Secrets(testSecretNS), testSecretName, o)
	require.NoError(t, err)
	t.Cleanup(func() { _ = prov.Close() })
	return prov
}

// waitWatching waits until the Provider has a watch open on cs.
func waitWatching(t *testing.T, w *watchCounter) {
	t.Helper()
	require.Eventually(t, func() bool { return w.count() > 0 }, testTimeout, 5*time.Millisecond, "the Secret is watched")
}

// watchCounter counts the watches opened on a fake clientset and records
// their options.
type watchCounter struct {
	mu   sync.Mutex
	n    int
	opts []metav1.ListOptions
	gets int
}

func (w *watchCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

func countWatches(cs *fake.Clientset) *watchCounter {
	w := &watchCounter{}
	cs.PrependWatchReactor("secrets", func(a k8stesting.Action) (bool, watch.Interface, error) {
		w.mu.Lock()
		w.n++
		if wa, ok := a.(k8stesting.WatchActionImpl); ok {
			w.opts = append(w.opts, metav1.ListOptions{FieldSelector: wa.WatchRestrictions.Fields.String(),
				ResourceVersion: wa.WatchRestrictions.ResourceVersion})
		}
		w.mu.Unlock()
		return false, nil, nil
	})
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		w.mu.Lock()
		w.gets++
		w.mu.Unlock()
		return false, nil, nil
	})
	return w
}

// TestSecretWatchRevocationClosesAndRefuses: a host removed from the
// inventory Secret has its connection closed and is refused, without any
// file; the other host keeps its connection.
func TestSecretWatchRevocationClosesAndRefuses(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	b := newFakeAgent(t, p, "host-b", fakeAgentOptions{})
	cs := fake.NewClientset(inventorySecret(t, a, b))
	w := countWatches(cs)
	prov := secretProvider(t, p, cs, Options{})
	client := providerv1.NewProviderClient(served(t, prov))
	for _, h := range []string{"host-a", "host-b"} {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: h})
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, b.lis.open.Load())
	waitWatching(t, w)

	_, err := cs.CoreV1().Secrets(testSecretNS).Update(ctx(t), inventorySecret(t, a), metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return b.lis.open.Load() == 0 }, testTimeout, 5*time.Millisecond,
		"the revoked host's connection is closed")
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
	requireHostUnavailable(t, err, CauseNotFronted)
	_, err = client.Power(ctx(t), signedPower(t, "host-b"))
	requireHostUnavailable(t, err, CauseNotFronted)
	assert.EqualValues(t, 1, b.lis.accepted.Load(), "no new connection to the revoked host")
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, a.lis.accepted.Load(), "the remaining host kept its connection")

	w.mu.Lock()
	defer w.mu.Unlock()
	for _, o := range w.opts {
		assert.Equal(t, "metadata.name="+testSecretName, o.FieldSelector,
			"every watch names the Secret, so a resourceNames-scoped rule authorises it")
	}
}

// TestSecretWatchDeletedFrontsNothing: a deleted inventory Secret fronts no
// host; a recreated one fronts its hosts again.
func TestSecretWatchDeletedFrontsNothing(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	cs := fake.NewClientset(inventorySecret(t, a))
	w := countWatches(cs)
	prov := secretProvider(t, p, cs, Options{})
	client := providerv1.NewProviderClient(served(t, prov))
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	waitWatching(t, w)

	require.NoError(t, cs.CoreV1().Secrets(testSecretNS).Delete(ctx(t), testSecretName, metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { return a.lis.open.Load() == 0 && len(prov.pool.ids()) == 0 }, testTimeout, 5*time.Millisecond)
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	requireHostUnavailable(t, err, CauseNotFronted)

	_, err = cs.CoreV1().Secrets(testSecretNS).Create(ctx(t), inventorySecret(t, a), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(prov.pool.ids()) == 1 }, testTimeout, 5*time.Millisecond)
}

// TestSecretWatchInvalidKeepsLastGood: an invalid document keeps the last
// good host set and logs no Secret content.
func TestSecretWatchInvalidKeepsLastGood(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	cs := fake.NewClientset(inventorySecret(t, a))
	w := countWatches(cs)
	var logs syncBuffer
	prov := secretProvider(t, p, cs, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	waitWatching(t, w)

	const canary = "CANARY-secret-content-7f3a"
	bad := inventorySecret(t, a)
	bad.Data[hostsecret.SecretDataKey] = []byte(`{"schemaVersion":1,"hosts":[{"id":"` + canary + `"`)
	_, err := cs.CoreV1().Secrets(testSecretNS).Update(ctx(t), bad, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return bytes.Contains([]byte(logs.String()), []byte("invalid document")) }, testTimeout, 5*time.Millisecond)
	assert.Equal(t, []string{"host-a"}, prov.pool.ids())
	assert.NotContains(t, logs.String(), canary)
	assert.NotContains(t, logs.String(), string(a.ca.PEM))
}

// TestSecretWatchFailuresRetriedAndReported: a watch that fails is retried
// from a fresh read; Validate reports a Secret not read for three reload
// intervals.
func TestSecretWatchFailuresRetriedAndReported(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	cs := fake.NewClientset(inventorySecret(t, a))
	var mu sync.Mutex
	failing := true
	attempts := 0
	cs.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if failing {
			return true, nil, errors.New("forbidden")
		}
		return false, nil, nil
	})
	var now time.Time
	var nowMu sync.Mutex
	now = time.Now()
	clock := func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
	prov := secretProvider(t, p, cs, Options{InventoryReloadInterval: time.Second, Now: clock})
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return attempts >= 2 }, testTimeout, 5*time.Millisecond,
		"a failed watch is retried")
	vr, err := prov.Validate(context.Background(), &providerv1.ValidateRequest{})
	require.NoError(t, err)
	assert.NotContains(t, vr.GetMessage(), "no valid host inventory was applied")

	nowMu.Lock()
	now = now.Add(4 * time.Second)
	nowMu.Unlock()
	// Gets keep succeeding, so a successful read clears the staleness; make
	// them fail too.
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("unavailable")
	})
	require.Eventually(t, func() bool {
		vr, err := prov.Validate(context.Background(), &providerv1.ValidateRequest{})
		return err == nil && bytes.Contains([]byte(vr.GetMessage()), []byte("no valid host inventory was applied"))
	}, testTimeout, 10*time.Millisecond)
	assert.Equal(t, []string{"host-a"}, prov.pool.ids(), "the last good host set is kept")

	mu.Lock()
	failing = false
	mu.Unlock()
}

// TestNewFromSecretRefusesAtStart: a Secret that cannot be read, has no
// document, or names another Secret is an error at start.
func TestNewFromSecretRefusesAtStart(t *testing.T) {
	p := newPKI(t)
	o := Options{ClientCertificate: p.client, Logger: quiet()}
	_, err := NewFromSecret(ctx(t), fake.NewClientset().CoreV1().Secrets(testSecretNS), testSecretName, o)
	require.Error(t, err)

	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testSecretNS, Name: testSecretName}}
	_, err = NewFromSecret(ctx(t), fake.NewClientset(empty).CoreV1().Secrets(testSecretNS), testSecretName, o)
	require.ErrorContains(t, err, hostsecret.SecretDataKey)

	_, err = NewFromSecret(ctx(t), nil, testSecretName, o)
	require.Error(t, err)
	_, err = NewFromSecret(ctx(t), fake.NewClientset().CoreV1().Secrets(testSecretNS), "", o)
	require.Error(t, err)
}

// TestSecretWatchStalledWatchBounded: a watch that delivers nothing and never
// fails delays a change by at most about one reload interval, because each
// cycle ends and starts again from a fresh read.
func TestSecretWatchStalledWatchBounded(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	b := newFakeAgent(t, p, "host-b", fakeAgentOptions{})
	cs := fake.NewClientset(inventorySecret(t, a, b))
	var mu sync.Mutex
	stalled := 0
	cs.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		stalled++
		mu.Unlock()
		return true, watch.NewFake(), nil // never delivers, never closes
	})
	prov := secretProvider(t, p, cs, Options{InventoryReloadInterval: time.Second})
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return stalled > 0 }, testTimeout, 5*time.Millisecond)

	start := time.Now()
	_, err := cs.CoreV1().Secrets(testSecretNS).Update(ctx(t), inventorySecret(t, a), metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(prov.pool.ids()) == 1 }, testTimeout, 5*time.Millisecond)
	assert.Less(t, time.Since(start), 2*time.Second+500*time.Millisecond, "bounded by the reload interval plus the client-side margin")
}

// controlledWatch makes every watch of the inventory Secret on cs a fake
// watcher the test drives; latest returns the open one.
type controlledWatch struct {
	mu sync.Mutex
	w  *watch.FakeWatcher
}

func (c *controlledWatch) latest() *watch.FakeWatcher {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w
}

func controlWatches(cs *fake.Clientset) *controlledWatch {
	c := &controlledWatch{}
	cs.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFakeWithChanSize(8, false)
		c.mu.Lock()
		c.w = w
		c.mu.Unlock()
		return true, w, nil
	})
	return c
}

// badInventorySecret is the inventory Secret with a document of a schema
// version this Provider does not understand.
func badInventorySecret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testSecretNS, Name: testSecretName},
		Data: map[string][]byte{hostsecret.SecretDataKey: []byte(`{"schemaVersion":99,"hosts":[]}`)}}
}

// validateMessage returns the Provider's Validate answer.
func validateMessage(t *testing.T, prov *Provider) (bool, string) {
	t.Helper()
	vr, err := prov.Validate(context.Background(), &providerv1.ValidateRequest{})
	require.NoError(t, err)
	return vr.GetOk(), vr.GetMessage()
}

// TestSecretWatchBookmarkDoesNotHideInvalid: a bookmark proves the watch is
// alive, nothing more: after an update with an invalid document, a bookmark
// neither clears the failure nor refreshes the inventory's age, and Validate
// reports it once it is stale.
func TestSecretWatchBookmarkDoesNotHideInvalid(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	cs := fake.NewClientset(inventorySecret(t, a))
	cw := controlWatches(cs)
	var nowMu sync.Mutex
	off := time.Duration(0)
	clock := func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return time.Now().Add(off) }
	prov := secretProvider(t, p, cs, Options{InventoryReloadInterval: time.Second, Now: clock})
	require.Eventually(t, func() bool { return cw.latest() != nil }, testTimeout, 5*time.Millisecond)

	bad := badInventorySecret()
	_, err := cs.CoreV1().Secrets(testSecretNS).Update(ctx(t), bad, metav1.UpdateOptions{})
	require.NoError(t, err)
	cw.latest().Modify(bad)
	require.Eventually(t, func() bool { return prov.secret.snapshot().applyErr != "" }, testTimeout, 5*time.Millisecond)
	bookmark := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testSecretNS, Name: testSecretName, ResourceVersion: "999"}}
	cw.latest().Action(watch.Bookmark, bookmark)

	nowMu.Lock()
	off = 4 * time.Second
	nowMu.Unlock()
	require.Never(t, func() bool { return prov.secret.snapshot().applyErr == "" }, 300*time.Millisecond, 10*time.Millisecond,
		"a bookmark never clears an invalid-document failure")
	ok, msg := validateMessage(t, prov)
	assert.True(t, ok, "stale but within the fail-closed bound")
	assert.Contains(t, msg, "no valid host inventory was applied")
	assert.Contains(t, msg, "the inventory document is invalid")
	assert.Equal(t, []string{"host-a"}, prov.pool.ids(), "the last good host set is kept within the bound")
}

// gauge returns the value of a Provider inventory gauge.
func gauge(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := obsmetrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "provider_type" && l.GetValue() == ProviderType {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

// TestSecretWatchFailsClosedOnOutage: when no inventory can be read for
// longer than the fail-closed bound, every host's connection is closed and
// every call refused (InventoryStale) without dialing; Validate is not Ok
// and the metrics say so. The next successful read restores routing.
func TestSecretWatchFailsClosedOnOutage(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	cs := fake.NewClientset(inventorySecret(t, a))
	var mu sync.Mutex
	down := false
	fail := func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	}
	cs.PrependReactor("get", "secrets", fail)
	cs.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	})
	prov := secretProvider(t, p, cs, Options{InventoryReloadInterval: time.Second, InventoryFailClosedAfter: 3 * time.Second})
	client := providerv1.NewProviderClient(served(t, prov))
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	require.EqualValues(t, 1, a.lis.open.Load())

	mu.Lock()
	down = true
	mu.Unlock()
	start := time.Now()
	require.Eventually(t, prov.inventoryFailedClosed, testTimeout, 10*time.Millisecond)
	assert.GreaterOrEqual(t, time.Since(start), 2*time.Second, "not before the bound")
	require.Eventually(t, func() bool { return a.lis.open.Load() == 0 }, testTimeout, 5*time.Millisecond, "connections closed")
	accepted := a.lis.accepted.Load()
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	requireHostUnavailable(t, err, CauseInventoryStale)
	_, err = client.Power(ctx(t), signedPower(t, "host-a"))
	requireHostUnavailable(t, err, CauseInventoryStale)
	assert.Equal(t, accepted, a.lis.accepted.Load(), "no dial while failed closed")
	ok, msg := validateMessage(t, prov)
	assert.False(t, ok)
	assert.Contains(t, msg, "every host is refused")
	assert.Equal(t, 1.0, gauge(t, "virtrigaud_provider_host_inventory_failed_closed"))
	assert.Equal(t, 1.0, gauge(t, "virtrigaud_provider_host_inventory_stale"))
	assert.GreaterOrEqual(t, gauge(t, "virtrigaud_provider_host_inventory_seconds_since_apply"), 3.0)

	mu.Lock()
	down = false
	mu.Unlock()
	require.Eventually(t, func() bool { return !prov.inventoryFailedClosed() }, testTimeout, 10*time.Millisecond, "recovers")
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	ok, _ = validateMessage(t, prov)
	assert.True(t, ok)
	require.Eventually(t, func() bool { return gauge(t, "virtrigaud_provider_host_inventory_failed_closed") == 0 }, testTimeout, 10*time.Millisecond)
}

// TestSecretWatchFailsClosedOnInvalidDocument: an inventory Secret whose
// document stays invalid past the bound fails closed too, although the API
// is reachable; a valid document restores routing.
func TestSecretWatchFailsClosedOnInvalidDocument(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	cs := fake.NewClientset(inventorySecret(t, a))
	w := countWatches(cs)
	prov := secretProvider(t, p, cs, Options{InventoryReloadInterval: time.Second, InventoryFailClosedAfter: 3 * time.Second})
	client := providerv1.NewProviderClient(served(t, prov))
	waitWatching(t, w)

	_, err := cs.CoreV1().Secrets(testSecretNS).Update(ctx(t), badInventorySecret(), metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, prov.inventoryFailedClosed, testTimeout, 10*time.Millisecond)
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	requireHostUnavailable(t, err, CauseInventoryStale)
	_, msg := validateMessage(t, prov)
	assert.Contains(t, msg, "the inventory document is invalid")

	_, err = cs.CoreV1().Secrets(testSecretNS).Update(ctx(t), inventorySecret(t, a), metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !prov.inventoryFailedClosed() }, testTimeout, 10*time.Millisecond)
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
}

// TestNewFromSecretFailClosedBound: a fail-closed bound shorter than two
// reload intervals is refused.
func TestNewFromSecretFailClosedBound(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	_, err := NewFromSecret(ctx(t), fake.NewClientset(inventorySecret(t, a)).CoreV1().Secrets(testSecretNS), testSecretName,
		Options{ClientCertificate: p.client, Logger: quiet(), InventoryReloadInterval: time.Second, InventoryFailClosedAfter: time.Second})
	require.ErrorContains(t, err, "fail-closed bound")
}
