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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// These tests pin the placement assumption lifecycle against a lagging
// informer cache: a reservation whose pendingHost write was sent ends only on
// evidence — the cache showing the record, a definitive rejection, or an
// authoritative read resolving an ambiguous write — never on its TTL alone
// (G1), and a VM is never placed again while such a write is unresolved (G2).

// laggingClientWith is newLaggingClient whose live store (writes and the
// authoritative reads) runs funcs.
func laggingClientWith(t *testing.T, s *runtime.Scheme, funcs interceptor.Funcs, objs ...client.Object) *laggingClient {
	t.Helper()
	build := func(withFuncs bool) client.Client {
		copies := make([]client.Object, 0, len(objs))
		for _, o := range objs {
			c, ok := o.DeepCopyObject().(client.Object)
			require.True(t, ok, "deep copy of %T is not a client.Object", o)
			copies = append(copies, c)
		}
		b := fake.NewClientBuilder().WithScheme(s).WithObjects(copies...).
			WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
			WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementHostIndex, placementHostIndexValue).
			WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{})
		if withFuncs {
			b = b.WithInterceptorFuncs(funcs)
		}
		return b.Build()
	}
	return &laggingClient{Client: build(true), scheme: s, reader: build(false)}
}

// ambiguousOnce fails the first pendingHost status write of the VM named name
// with a timeout. When store is true that write is stored first (it landed,
// but the answer was lost); otherwise it is not. Every other write passes.
func ambiguousOnce(name string, store bool) interceptor.Funcs {
	var once sync.Once
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			fail := false
			if obj.GetName() == name {
				once.Do(func() { fail = true })
			}
			if !fail {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			if store {
				if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
					return err
				}
			}
			return apierrors.NewTimeoutError("pendingHost write timed out", 1)
		},
	}
}

// safetyReconciler is a reconciler on lc: cached reads lag, writes and
// authoritative reads (APIReader) go to the live store.
func safetyReconciler(lc *laggingClient, now *time.Time) *VirtualMachineReconciler {
	r := &VirtualMachineReconciler{Client: lc, Scheme: lc.scheme, APIReader: lc.Client}
	r.clock = func() time.Time { return *now }
	return r
}

// assumptionOf returns the VM's assumption.
func assumptionOf(t *testing.T, r *VirtualMachineReconciler, name string) (assume.Assumption, uint64) {
	t.Helper()
	a, token, ok := r.placementAssumptions().Lookup("uid-" + name)
	require.True(t, ok, "%s has an assumption", name)
	return a, token
}

// createOnce runs createVM for name from the (lagging) cache.
func createOnce(t *testing.T, r *VirtualMachineReconciler, lc *laggingClient, prov *concurrentCreateProvider, name string) (time.Duration, error) {
	t.Helper()
	res, err := r.createVM(context.Background(), readVM(t, lc, name), prov, clusteredProviderCR("prov-cluster", capNS),
		smallVMClass(capNS), minimalVMImage(capNS), nil)
	return res.RequeueAfter, err
}

// TestAssumptionSafety_G1_StoredWriteOutlivesItsTTL: a pendingHost write that
// landed keeps its host reserved while the informer cache does not show it,
// however long past the assumption's TTL; once the cache shows the record,
// the record counts instead.
func TestAssumptionSafety_G1_StoredWriteOutlivesItsTTL(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	objs := append(capBase(), capHost("host-alpha", 2), capVM("a"), capVM("b"))
	lc := newLaggingClient(t, coverageTestScheme(t), objs...)
	r := safetyReconciler(lc, &now)
	prov := &concurrentCreateProvider{}

	_, err := createOnce(t, r, lc, prov, "a")
	require.NoError(t, err)
	require.Equal(t, []string{"host-alpha"}, prov.created())
	a, _ := assumptionOf(t, r, "a")
	assert.Equal(t, assume.WriteStored, a.Write)

	now = now.Add(placementAssumeTTL + 10*time.Minute)
	host, _ := resolve(t, r, readVM(t, lc, "b"))
	assert.Empty(t, host, "the cache still lags the write long past the TTL: the host stays reserved")
	assert.Contains(t, assumedUIDs(r), "uid-a")

	lc.catchUp(t)
	host, _ = resolve(t, r, readVM(t, lc, "b"))
	assert.Empty(t, host, "the record now counts")
	assert.NotContains(t, assumedUIDs(r), "uid-a", "and settles the assumption")
}

// TestAssumptionSafety_G2_AmbiguousWriteThatLandedIsNeverReplaced: a
// pendingHost write that landed but answered a timeout is retried before the
// cache shows it. The VM is not placed again: an authoritative read finds the
// record, the original host stays reserved, a competing VM goes elsewhere, a
// third finds no room, and exactly one Create is sent, on the original host.
func TestAssumptionSafety_G2_AmbiguousWriteThatLandedIsNeverReplaced(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	objs := append(capBase(), capHost("host-alpha", 2), capHost("host-bravo", 2), capVM("a"), capVM("b"), capVM("c"))
	lc := laggingClientWith(t, coverageTestScheme(t), ambiguousOnce("a", true), objs...)
	r := safetyReconciler(lc, &now)
	prov := &concurrentCreateProvider{}

	_, err := createOnce(t, r, lc, prov, "a")
	require.Error(t, err, "the timeout is returned")
	require.Empty(t, prov.created(), "no Create without a confirmed pending host")
	first, token := assumptionOf(t, r, "a")
	assert.Equal(t, assume.WriteInFlight, first.Write)
	live := readVM(t, lc.Client, "a")
	require.Equal(t, first.HostID, pendingHost(live), "the write landed")

	// The retry, before the cache shows the write.
	requeue, err := createOnce(t, r, lc, prov, "a")
	require.NoError(t, err)
	assert.Equal(t, placementWriteRecheckInterval, requeue)
	assert.Empty(t, prov.created())
	again, sameToken := assumptionOf(t, r, "a")
	assert.Equal(t, first.HostID, again.HostID, "the original host stays reserved")
	assert.Equal(t, token, sameToken, "the same pick")
	assert.Equal(t, assume.WriteStored, again.Write, "resolved from the API server")

	other, _ := resolve(t, r, readVM(t, lc, "b"))
	require.NotEmpty(t, other)
	assert.NotEqual(t, first.HostID, other, "a competing VM does not overbook the reserved host")
	none, _ := resolve(t, r, readVM(t, lc, "c"))
	assert.Empty(t, none, "no host has room for a third")

	lc.catchUp(t)
	_, err = createOnce(t, r, lc, prov, "a")
	require.NoError(t, err)
	assert.Equal(t, []string{first.HostID}, prov.created(), "one Create, on the original host")
}

// TestAssumptionSafety_G2_AmbiguousWriteThatDidNotLandIsSentAgain: the stored
// object is still at the write's precondition, so the write could still land:
// it is sent again unchanged, never placed elsewhere.
func TestAssumptionSafety_G2_AmbiguousWriteThatDidNotLandIsSentAgain(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	objs := append(capBase(), capHost("host-alpha", 2), capHost("host-bravo", 2), capVM("a"))
	lc := laggingClientWith(t, coverageTestScheme(t), ambiguousOnce("a", false), objs...)
	r := safetyReconciler(lc, &now)
	prov := &concurrentCreateProvider{}

	_, err := createOnce(t, r, lc, prov, "a")
	require.Error(t, err)
	first, token := assumptionOf(t, r, "a")
	require.Empty(t, pendingHost(readVM(t, lc.Client, "a")), "the write did not land")

	requeue, err := createOnce(t, r, lc, prov, "a")
	require.NoError(t, err)
	assert.Equal(t, placementWriteRecheckInterval, requeue)
	again, sameToken := assumptionOf(t, r, "a")
	assert.Equal(t, token, sameToken)
	assert.Equal(t, assume.WriteStored, again.Write)
	assert.Equal(t, first.HostID, pendingHost(readVM(t, lc.Client, "a")), "the same write, sent again, landed")
	assert.Empty(t, prov.created())

	lc.catchUp(t)
	_, err = createOnce(t, r, lc, prov, "a")
	require.NoError(t, err)
	assert.Equal(t, []string{first.HostID}, prov.created())
}

// TestAssumptionSafety_G2_NewerStoredVersionResolvesTheWrite: the stored
// object moved past the ambiguous write's precondition without recording its
// host, so that write can never land: the reservation is released and the VM
// is placed again as usual.
func TestAssumptionSafety_G2_NewerStoredVersionResolvesTheWrite(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	objs := append(capBase(), capHost("host-alpha", 2), capVM("a"))
	lc := laggingClientWith(t, coverageTestScheme(t), ambiguousOnce("a", false), objs...)
	r := safetyReconciler(lc, &now)
	prov := &concurrentCreateProvider{}
	ctx := context.Background()

	_, err := createOnce(t, r, lc, prov, "a")
	require.Error(t, err)
	_, token := assumptionOf(t, r, "a")
	edited := readVM(t, lc.Client, "a")
	edited.Labels = map[string]string{"edited": "by-another-writer"}
	require.NoError(t, lc.Update(ctx, edited))

	// The retry (cache still lagging): the write is resolved as not landed,
	// and the fresh placement's own write loses the race with the stale copy.
	_, err = createOnce(t, r, lc, prov, "a")
	require.NoError(t, err)
	assert.False(t, r.placementAssumptions().ForgetIf("uid-a", token), "the ambiguous pick was released")
	assert.Empty(t, prov.created())

	lc.catchUp(t)
	_, err = createOnce(t, r, lc, prov, "a")
	require.NoError(t, err)
	assert.Equal(t, []string{"host-alpha"}, prov.created(), "placed and created once")
}

// TestAssumptionSafety_UnobservedVMIsNotTakenAsGone: a reservation for a VM
// the cache has not shown yet (a clone target created a moment ago) keeps
// counting while the VM is missing from the snapshot.
func TestAssumptionSafety_UnobservedVMIsNotTakenAsGone(t *testing.T) {
	objs := append(capBase(), capHost("host-alpha", 2), capVM("b"))
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)
	cache := r.placementAssumptions()
	token, ok := cache.Assume(capNS+"/prov-cluster", assume.Assumption{
		UID: "uid-fresh", Namespace: capNS, Name: "fresh", HostID: "host-alpha",
		Resources: scheduler.ResourceRequest{CPU: 2, MemoryMiB: 4096},
	})
	require.True(t, ok)
	require.True(t, cache.BeginWrite("uid-fresh", token))

	host, _ := resolve(t, r, readVM(t, r, "b"))
	assert.Empty(t, host, "the unobserved VM's reservation still counts")
	assert.Contains(t, assumedUIDs(r), "uid-fresh")
}

// TestAssumptionSafety_RejectedWriteReleasesOnlyItsOwnPick: a definitive
// rejection releases the pick it belongs to, never a newer one of the VM.
func TestAssumptionSafety_RejectedWriteReleasesOnlyItsOwnPick(t *testing.T) {
	cache := assume.New(placementAssumeTTL, nil)
	old, _ := cache.Assume("p", assume.Assumption{UID: "vm", HostID: "h1"})
	newer, _ := cache.Assume("p", assume.Assumption{UID: "vm", HostID: "h2"}) // old never wrote
	assert.False(t, cache.WriteRejected("vm", old))
	got, ok := cache.TokenFor("vm", "h2")
	require.True(t, ok)
	assert.Equal(t, newer, got)
}

// TestAssumptionSafety_StoredWriteReleasedByAnotherControllerIsResolved: the
// pendingHost write was stored, then another controller released the pending
// host (a migration releasing its target, ADR-0007 Addendum B) before any
// decision saw the record. The VM is not held for ever waiting for a record
// that is gone: the authoritative read releases the reservation and the VM is
// placed again.
func TestAssumptionSafety_StoredWriteReleasedByAnotherControllerIsResolved(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	objs := append(capBase(), capHost("host-alpha", 2), capVM("a"))
	lc := newLaggingClient(t, coverageTestScheme(t), objs...)
	r := safetyReconciler(lc, &now)
	ctx := context.Background()

	host, _ := resolve(t, r, readVM(t, lc, "a"))
	require.Equal(t, "host-alpha", host)
	token, ok := r.placementAssumptions().TokenFor("uid-a", host)
	require.True(t, ok)
	require.True(t, r.placementAssumptions().BeginWrite("uid-a", token))
	r.placementAssumptions().WriteStored("uid-a", token)

	released := readVM(t, lc.Client, "a")
	released.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{ExcludedHosts: []string{"host-alpha"}}
	require.NoError(t, lc.Status().Update(ctx, released))
	lc.catchUp(t)

	_, held, err := r.resolveUnsettledPlacement(ctx, readVM(t, lc, "a"), clusteredProviderCR("prov-cluster", capNS))
	require.NoError(t, err)
	assert.False(t, held, "the VM is placed again")
	assert.False(t, r.placementAssumptions().ForgetIf("uid-a", token), "the released pick no longer counts")
}

// TestAssumptionSafety_ExpiredPickIsNeverWritten: a pick whose pendingHost
// write was not sent before its TTL passed is dropped by the next decision,
// which may give its host to another VM; the expired pick's write is then
// refused (its token no longer matches), so it never records a host that
// nothing reserves.
func TestAssumptionSafety_ExpiredPickIsNeverWritten(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	objs := append(capBase(), capHost("host-alpha", 2), capVM("a"), capVM("b"))
	lc := newLaggingClient(t, coverageTestScheme(t), objs...)
	r := safetyReconciler(lc, &now)
	providerCR := clusteredProviderCR("prov-cluster", capNS)

	vmA := readVM(t, lc, "a")
	p, _, err := r.resolveClusterPlacement(context.Background(), vmA, providerCR, capCreateReq(), nil)
	require.NoError(t, err)
	require.NotNil(t, p)
	require.Equal(t, "host-alpha", p.hostID)

	now = now.Add(placementAssumeTTL)
	host, _ := resolve(t, r, readVM(t, lc, "b"))
	require.Equal(t, "host-alpha", host, "the expired pick no longer reserves the host")
	assert.NotContains(t, assumedUIDs(r), "uid-a")

	_, recorded, err := r.recordPendingHost(context.Background(), vmA, providerCR, p)
	require.NoError(t, err)
	assert.False(t, recorded, "the expired pick's write is not sent")
	live := readVM(t, lc.Client, "a")
	assert.Empty(t, placementHosts(live), "no host is recorded for it")
	_, ok := r.placementAssumptions().TokenFor("uid-b", "host-alpha")
	assert.True(t, ok, "the newer reservation is untouched")
}
