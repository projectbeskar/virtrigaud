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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the local refusal of the operations that have no signed
// path, for a Provider that requires operation identity: a fixed
// OperationIdentityUnsupported condition (or adoption message), one Warning
// event, and no client resolved, so no RPC of any kind.

// resolveCounter counts the clients it resolves.
type resolveCounter struct {
	n        atomic.Int32
	provider contracts.Provider
}

func (c *resolveCounter) GetProvider(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
	c.n.Add(1)
	return c.provider, nil
}

// identityProviderCR is a running cloudhypervisor Provider ns/name.
func identityProviderCR(ns, name string) *infrav1beta1.Provider {
	p := runningProvider(ns, name)
	p.Spec.Type = infrav1beta1.ProviderTypeCloudHypervisor
	p.UID = "uid-" + types.UID(name)
	return p
}

// requireUnsupported asserts conds carry the fixed refusal of op.
func requireUnsupported(t *testing.T, conds []metav1.Condition, condType, op string) {
	t.Helper()
	c := meta.FindStatusCondition(conds, condType)
	require.NotNil(t, c, "condition %s", condType)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonOperationIdentityUnsupported, c.Reason)
	assert.Equal(t, identityUnsupportedMessage(op), c.Message)
}

// unsupportedEvents counts the OperationIdentityUnsupported events rec holds.
func unsupportedEvents(rec *record.FakeRecorder) int {
	n := 0
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, k8s.ReasonOperationIdentityUnsupported) {
			n++
		}
	}
	return n
}

func TestIdentityUnsupported_RefusalGuard(t *testing.T) {
	require.NoError(t, refuseUnsupportedForIdentity(runningProvider("ns", "p"), identityUnsupportedClone))
	require.NoError(t, refuseUnsupportedForIdentity(nil, identityUnsupportedClone))
	err := refuseUnsupportedForIdentity(identityProviderCR("ns", "p"), identityUnsupportedClone)
	require.Error(t, err)
	assert.True(t, isOperationIdentityUnsupported(err))
	assert.ErrorIs(t, err, contracts.ErrOperationIdentityNotImplemented)
}

func TestIdentityUnsupported_VMSnapshot(t *testing.T) {
	vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
	r, rec := newSnapPhaseReconciler(t, vm, pendingSnapshot(), identityProviderCR(snapPhaseNS, "prov"))
	var resolves atomic.Int32
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
		resolves.Add(1)
		return &snapshotCreateSpy{}, nil
	}
	for range 2 {
		snap := reconcileSnapshot(t, r)
		requireNotReady(t, snap)
		requireUnsupported(t, snap.Status.Conditions, infrav1beta1.VMSnapshotConditionReady, identityUnsupportedSnapshot)
		assert.Empty(t, snap.Status.SnapshotID)
	}
	assert.Zero(t, resolves.Load(), "no client is resolved, so nothing is sent")
	assert.Equal(t, 1, unsupportedEvents(rec), "one Warning event when the refusal begins")
	_, err := r.getProviderInstance(context.Background(), identityProviderCR(snapPhaseNS, "prov"))
	assert.True(t, isOperationIdentityUnsupported(err))
}

func TestIdentityUnsupported_VMClone(t *testing.T) {
	s := cloneTestScheme(t)
	ns := "default"
	prov := identityProviderCR(ns, "prov-1")
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-999"}}
	resolver := &resolveCounter{provider: cp}
	r := newCloneReconciler(s, resolver, prov, src, clone)
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	requireUnsupported(t, got.Status.Conditions, infrav1beta1.VMCloneConditionReady, identityUnsupportedClone)
	assert.Zero(t, resolver.n.Load(), "no client is resolved, so nothing is sent")
	assert.Equal(t, 1, unsupportedEvents(rec))
	target := &infrav1beta1.VirtualMachine{}
	assert.Error(t, r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "clone-target"}, target), "no target is created")
}

func TestIdentityUnsupported_VMMigration(t *testing.T) {
	ns := "default"
	src := sourceVMWithID(ns, "src-vm", "prov-ch", "vm-source-123")
	migration := &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns, UID: "uid-mig"},
		Spec: infrav1beta1.VMMigrationSpec{
			Source: infrav1beta1.MigrationSource{VMRef: infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.MigrationTarget{Name: "dst-vm", ProviderRef: infrav1beta1.ObjectRef{Name: "prov-other"}},
		},
		Status: infrav1beta1.VMMigrationStatus{Phase: infrav1beta1.MigrationPhaseValidating},
	}
	c := fake.NewClientBuilder().WithScheme(capGatingScheme(t)).
		WithObjects(src, migration, identityProviderCR(ns, "prov-ch"), runningProvider(ns, "prov-other")).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).Build()
	var resolves atomic.Int32
	rec := record.NewFakeRecorder(50)
	r := &VMMigrationReconciler{Client: c, Scheme: c.Scheme(), Recorder: rec,
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
			resolves.Add(1)
			return &countingMigrationProvider{}, nil
		}}
	key := client.ObjectKeyFromObject(migration)
	for range 3 {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), key, got))
	requireUnsupported(t, got.Status.Conditions, infrav1beta1.VMMigrationConditionReady, identityUnsupportedMigration)
	assert.Equal(t, infrav1beta1.MigrationPhaseValidating, got.Status.Phase, "the migration does not move on")
	assert.Zero(t, resolves.Load(), "no client is resolved, so nothing is sent")
	assert.Equal(t, 1, unsupportedEvents(rec))
}

func TestIdentityUnsupported_VMAdoption(t *testing.T) {
	prov := identityProviderCR(clusterNS, "prov-ch")
	prov.Annotations = map[string]string{AdoptionAnnotation: "true"}
	prov.Status.Healthy = true
	resolver := &resolveCounter{}
	r := clusteredAdoptionReconciler(t, nil, prov)
	r.RemoteResolver = resolver
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec
	key := client.ObjectKeyFromObject(prov)
	for range 2 {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	got := &infrav1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), key, got))
	require.NotNil(t, got.Status.Adoption)
	assert.Equal(t, identityUnsupportedMessage(identityUnsupportedAdoption), got.Status.Adoption.Message)
	assert.Zero(t, resolver.n.Load(), "no client is resolved, so nothing is listed or sent")
	assert.Equal(t, 1, unsupportedEvents(rec))
	_, err := r.getProviderInstance(context.Background(), prov)
	assert.True(t, isOperationIdentityUnsupported(err))
}

// An identity VM whose image would be prepared on its Provider is refused
// before any prepare (and so before any Create) is sent.
func TestIdentityUnsupported_ImagePrepare(t *testing.T) {
	stub := &identityPreparingStub{identityStub: newIdentityStub()}
	r, req := identityFixture(t, stub)
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec
	p := &infrav1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, p))
	p.Status.ReportedCapabilities = &infrav1beta1.ReportedCapabilities{SupportsImageImport: true}
	require.NoError(t, r.Update(context.Background(), p))
	reconcileN(t, r, req, 2)
	vm := idVM(t, r)
	requireUnsupported(t, vm.Status.Conditions, k8s.ConditionReady, identityUnsupportedImagePrepare)
	assert.Empty(t, stub.sent(), "no Create is sent")
	assert.Empty(t, stub.builds)
	assert.Zero(t, stub.prepares.Load(), "no image prepare is sent")
	assert.Equal(t, 1, unsupportedEvents(rec))
}

// A snapshot revert of an identity VM is refused; nothing is sent.
func TestIdentityUnsupported_SnapshotRevert(t *testing.T) {
	stub := &identityPreparingStub{identityStub: newIdentityStub()}
	r, req := identityFixture(t, stub)
	reconcileN(t, r, req, 1)
	require.Equal(t, "vm-1", idVM(t, r).Status.ID)
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec
	setVMSpec(t, r, func(vm *infrav1beta1.VirtualMachine) {
		vm.Spec.Snapshot = &infrav1beta1.VMSnapshotOperation{RevertToRef: &infrav1beta1.LocalObjectReference{Name: "snap-1"}}
	})
	reconcileN(t, r, req, 2)
	vm := idVM(t, r)
	requireUnsupported(t, vm.Status.Conditions, infrav1beta1.VirtualMachineConditionSnapshotReverted, identityUnsupportedSnapshot)
	assert.NotEqual(t, metav1.ConditionTrue, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady).Status)
	assert.Zero(t, stub.reverts.Load(), "no snapshot revert is sent")
	assert.Len(t, stub.sent(), 1, "only the Create")
	assert.Equal(t, 1, unsupportedEvents(rec))
}

// identityPreparingStub is an identityStub that can prepare images and
// counts the prepares and snapshot reverts it is sent.
type identityPreparingStub struct {
	*identityStub
	prepares atomic.Int32
	reverts  atomic.Int32
}

func (s *identityPreparingStub) PrepareImage(context.Context, contracts.ImagePrepareRequest) (contracts.ImagePrepareResponse, error) {
	s.prepares.Add(1)
	return contracts.ImagePrepareResponse{}, nil
}

func (s *identityPreparingStub) SnapshotRevert(context.Context, contracts.VMRef, string) (string, error) {
	s.reverts.Add(1)
	return "", nil
}

// The sweeper's once-only warnings are forgotten when their Secret is no
// longer kept, when an earlier Provider of the same name had them, and when
// the Provider is gone.
func TestProviderSweep_PrunesKeptIntentWarnings(t *testing.T) {
	r := &ProviderReconciler{}
	for _, k := range []string{
		intentWarningKey("ns", "p", "uid-1", "vrtg-op-a"),
		intentWarningKey("ns", "p", "uid-1", "vrtg-op-b"),
		intentWarningKey("ns", "p", "uid-0", "vrtg-op-c"),
		intentWarningKey("ns", "other", "uid-9", "vrtg-op-d"),
	} {
		r.intentSkipWarned.Store(k, struct{}{})
	}
	r.forgetIntentWarnings("ns", "p", "uid-1", map[string]bool{"vrtg-op-a": true})
	keys := func() []string {
		var out []string
		r.intentSkipWarned.Range(func(k, _ any) bool {
			s, _ := k.(string)
			out = append(out, s)
			return true
		})
		return out
	}
	assert.ElementsMatch(t, []string{intentWarningKey("ns", "p", "uid-1", "vrtg-op-a"), intentWarningKey("ns", "other", "uid-9", "vrtg-op-d")},
		keys())
	r.forgetIntentWarnings("ns", "p", "", nil)
	assert.ElementsMatch(t, []string{intentWarningKey("ns", "other", "uid-9", "vrtg-op-d")}, keys())
}
