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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the create marker of a single-host VirtualMachine
// (Provisioning=True/CreateRequested): recorded and confirmed before the
// Create is sent, never a blind second Create on a provider that cannot find
// the VM it may have made, and never a silent release on delete.

// listingProvider is a routingProvider whose ListVMs returns vms and whose
// Describe answers each domain name from describe (a missing name: a definite
// NotFound), recording the names described.
type listingProvider struct {
	routingProvider
	vms       []contracts.VMInfo
	describe  map[string]func() (contracts.DescribeResponse, error)
	described []string
}

func (p *listingProvider) ListVMs(context.Context) (contracts.VMList, error) {
	return contracts.VMList{VMs: p.vms}, nil
}

func (p *listingProvider) Describe(_ context.Context, vm contracts.VMRef) (contracts.DescribeResponse, error) {
	p.described = append(p.described, vm.ID)
	if answer, ok := p.describe[vm.ID]; ok {
		return answer()
	}
	return contracts.DescribeResponse{}, contracts.NewNotFoundError("describe: domain "+vm.ID+" not found", nil)
}

// seedMarker records the create marker on the fixture's VM, as a Create
// requested `ago` and never recorded.
func seedMarker(t *testing.T, r *VirtualMachineReconciler, ago time.Duration) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	vm, _ := storedVM(t, r)
	setCreateRequested(vm, time.Now().Add(-ago))
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: "test-prov", UID: "uid-provider"}
	require.NoError(t, r.Status().Update(context.Background(), vm))
	got, _ := storedVM(t, r)
	require.True(t, createRequested(got))
	return got
}

// eventsWithPrefix counts the events in rec that start with prefix.
func eventsWithPrefix(rec *record.FakeRecorder, prefix string) int {
	n := 0
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func TestCreateMarker_PersistedBeforeTheCreate(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, "", nil)
	rp.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		stored, _ := storedVM(t, r)
		assert.True(t, createRequested(stored), "the marker is stored before the Create is sent")
		c := meta.FindStatusCondition(stored.Status.Conditions, k8s.ConditionProvisioning)
		assert.Equal(t, metav1.ConditionTrue, c.Status)
		assert.Equal(t, createRequestedMessage, c.Message)
		assert.Equal(t, stored.Generation, c.ObservedGeneration)
		require.NotNil(t, stored.Status.BoundProvider, "with the Provider the Create goes through")
		assert.Equal(t, "test-prov", stored.Status.BoundProvider.Name)
		return contracts.CreateResponse{ID: "vm-" + req.Name}, nil
	}
	vm, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	require.Len(t, rp.createReqs, 1)
}

func TestCreateMarker_FailedWriteMeansNoCreate(t *testing.T) {
	r, rp, _ := createRecordFixture(t, "", func(_ context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
		if createRequested(vm) {
			return errAPIUnavailable
		}
		return nil
	})
	vm, _ := storedVM(t, r)
	res, err := r.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
	assert.Empty(t, rp.createReqs, "no Create without a confirmed marker")
}

func TestCreateMarker_UnknownOutcomeKeepsTheMarker(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, "", nil)
	rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, transportUnavailable("create")
	}
	vm, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	got, _ := storedVM(t, r)
	assert.True(t, createRequested(got), "a Create whose outcome is unknown keeps the marker")
	assert.Equal(t, k8s.ReasonProviderError, meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady).Reason)

	// A refusal replaces it: the provider made nothing.
	r2, rp2, _ := createRecordFixture(t, "", nil)
	rp2.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, wireConflict()
	}
	vm2, _ := storedVM(t, r2)
	_, err = r2.reconcileVM(ctx, vm2)
	require.NoError(t, err)
	got2, _ := storedVM(t, r2)
	assert.False(t, createRequested(got2))
	assert.Equal(t, k8s.ReasonProviderConflict, meta.FindStatusCondition(got2.Status.Conditions, k8s.ConditionProvisioning).Reason)
}

func TestCreateMarker_NoBlindRecreateWithoutOwnerStamps(t *testing.T) {
	for _, pt := range []infravirtrigaudiov1beta1.ProviderType{""} {
		t.Run("type "+string(pt), func(t *testing.T) {
			ctx := context.Background()
			r, rp, rec := createRecordFixture(t, pt, nil)
			marked := seedMarker(t, r, time.Minute)

			res, err := r.reconcileVM(ctx, marked)
			require.NoError(t, err)
			assert.Equal(t, createOutcomeUnknownRetryInterval, res.RequeueAfter)
			assert.Empty(t, rp.createReqs, "no second Create on a provider that cannot find the VM it made")
			held, _ := storedVM(t, r)
			ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, k8s.ReasonCreateOutcomeUnknown, ready.Reason)
			assert.Equal(t, createOutcomeUnknownMessage, ready.Message)
			assert.True(t, createRequested(held), "the marker is kept")
			assert.Equal(t, 1, eventsWithPrefix(rec, "Warning "+k8s.ReasonCreateOutcomeUnknown))

			// A repeated hold writes nothing and records no event.
			_, err = r.reconcileVM(ctx, held.DeepCopy())
			require.NoError(t, err)
			again, _ := storedVM(t, r)
			assert.Equal(t, held.ResourceVersion, again.ResourceVersion)
			assert.True(t, equality.Semantic.DeepEqual(held.Status, again.Status))
			assert.Zero(t, eventsWithPrefix(rec, "Warning "+k8s.ReasonCreateOutcomeUnknown))

			// An operator confirms that no VM was made: it is created, once.
			again.Annotations = map[string]string{createOutcomeAnnotation: createOutcomeNotCreatedPrefix + markerAttemptID(again)}
			require.NoError(t, r.Update(ctx, again))
			resolved, _ := storedVM(t, r)
			_, err = r.reconcileVM(ctx, resolved)
			require.NoError(t, err)
			require.Len(t, rp.createReqs, 1)
			done, _ := storedVM(t, r)
			assert.Equal(t, "vm-web", done.Status.ID)
			assert.NotContains(t, done.Annotations, createOutcomeAnnotation, "the confirmation is used once")
		})
	}
}

func TestCreateMarker_RebindingProvidersAreSentTheCreateAgain(t *testing.T) {
	for _, pt := range []infravirtrigaudiov1beta1.ProviderType{infravirtrigaudiov1beta1.ProviderTypeVSphere,
		infravirtrigaudiov1beta1.ProviderTypeLibvirt, infravirtrigaudiov1beta1.ProviderTypeProxmox} {
		t.Run(string(pt), func(t *testing.T) {
			r, rp, _ := createRecordFixture(t, pt, nil)
			marked := seedMarker(t, r, time.Minute)
			_, err := r.reconcileVM(context.Background(), marked)
			require.NoError(t, err)
			require.Len(t, rp.createReqs, 1, "the Create is re-sent: the provider re-binds the VM it stamped")
			assert.Equal(t, string(marked.UID), rp.createReqs[0].Owner.UID)
			got, _ := storedVM(t, r)
			assert.Equal(t, "vm-web", got.Status.ID)
			assert.False(t, createRequested(got))
		})
	}
}

// TestCreateMarker_StaleCacheNeverCreates (review M1): the live re-read right
// before the Create sees what a stale cached copy does not.
func TestCreateMarker_StaleCacheNeverCreates(t *testing.T) {
	ctx := context.Background()

	t.Run("a marker the cache does not show", func(t *testing.T) {
		r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
		stale, _ := storedVM(t, r)
		seedMarker(t, r, time.Minute)
		res, err := r.reconcileVM(ctx, stale)
		require.NoError(t, err)
		assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
		assert.Empty(t, rp.createReqs)
	})

	t.Run("an id the cache does not show", func(t *testing.T) {
		r, rp, _ := createRecordFixture(t, "", nil)
		stale, _ := storedVM(t, r)
		recorded := stale.DeepCopy()
		recorded.Status.ID = "vm-recorded"
		require.NoError(t, r.Status().Update(ctx, recorded))
		_, err := r.reconcileVM(ctx, stale)
		require.NoError(t, err)
		assert.Empty(t, rp.createReqs)
		got, _ := storedVM(t, r)
		assert.Equal(t, "vm-recorded", got.Status.ID)
	})
}

func TestCreateMarker_DeleteFindsTheVMByOwnerStamp(t *testing.T) {
	ctx := context.Background()
	r, _, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
	lp := &listingProvider{}
	r.RemoteResolver = &stubResolver{provider: lp}
	marked := seedMarker(t, r, time.Minute)
	lp.vms = []contracts.VMInfo{
		{ID: "default.other", ProviderRaw: map[string]string{contracts.VMInfoOwnerUIDKey: "uid-someone-else"}},
		{ID: "default.web", ProviderRaw: map[string]string{contracts.VMInfoOwnerUIDKey: string(marked.UID)}},
	}

	_, err := r.handleDeletion(ctx, markForDeletion(t, r, marked))
	require.NoError(t, err)
	require.Len(t, lp.deleteRefs, 1)
	assert.Equal(t, "default.web", lp.deleteRefs[0].ID, "the VM stamped with the VirtualMachine's UID is deleted")
	_, exists := storedVM(t, r)
	assert.False(t, exists)
}

func TestCreateMarker_DeleteWithNoStampedVM(t *testing.T) {
	ctx := context.Background()

	t.Run("settled, every domain name confirmed absent: released", func(t *testing.T) {
		r, _, rec := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
		lp := &listingProvider{}
		r.RemoteResolver = &stubResolver{provider: lp}
		marked := seedMarker(t, r, time.Hour)
		_, err := r.handleDeletion(ctx, markForDeletion(t, r, marked))
		require.NoError(t, err)
		assert.Empty(t, lp.deleteRefs)
		assert.Equal(t, []string{"default.web", "web"}, lp.described, "the namespaced and the legacy domain names")
		_, exists := storedVM(t, r)
		assert.False(t, exists, "the provider confirms no such VM, and no Create can still be running")
		assert.Equal(t, 1, eventsWithPrefix(rec, "Normal "+eventReasonCreateConfirmedAbsent))
	})

	// review HIGH: a ListVMs that lists nothing proves nothing (it skips a
	// domain it cannot read); without a definite not-found for every name the
	// delete is held.
	for name, answer := range map[string]func() (contracts.DescribeResponse, error){
		"the domain exists (unlisted)": func() (contracts.DescribeResponse, error) {
			return contracts.DescribeResponse{Exists: true}, nil
		},
		"the provider cannot tell (untyped error)": func() (contracts.DescribeResponse, error) {
			return contracts.DescribeResponse{}, errors.New("failed to describe VM: failed to get domain info")
		},
		"the provider is unreachable": func() (contracts.DescribeResponse, error) {
			return contracts.DescribeResponse{}, transportUnavailable("describe")
		},
	} {
		t.Run("settled, "+name+": held", func(t *testing.T) {
			r, _, rec := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
			lp := &listingProvider{describe: map[string]func() (contracts.DescribeResponse, error){"default.web": answer}}
			r.RemoteResolver = &stubResolver{provider: lp}
			marked := seedMarker(t, r, time.Hour)
			_, err := r.handleDeletion(ctx, markForDeletion(t, r, marked))
			require.NoError(t, err)
			assert.Empty(t, lp.deleteRefs)
			held, exists := storedVM(t, r)
			require.True(t, exists, "never released without a definite not-found")
			assert.Equal(t, k8s.ReasonCreateOutcomeUnknown, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
			assert.Zero(t, eventsWithPrefix(rec, "Normal "+eventReasonCreateConfirmedAbsent))
		})
	}

	t.Run("settled, the legacy name exists: held", func(t *testing.T) {
		r, _, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
		lp := &listingProvider{describe: map[string]func() (contracts.DescribeResponse, error){
			"web": func() (contracts.DescribeResponse, error) { return contracts.DescribeResponse{Exists: true}, nil },
		}}
		r.RemoteResolver = &stubResolver{provider: lp}
		marked := seedMarker(t, r, time.Hour)
		_, err := r.handleDeletion(ctx, markForDeletion(t, r, marked))
		require.NoError(t, err)
		_, exists := storedVM(t, r)
		assert.True(t, exists)
	})

	t.Run("recent: held", func(t *testing.T) {
		r, _, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
		r.RemoteResolver = &stubResolver{provider: &listingProvider{}}
		marked := seedMarker(t, r, time.Minute)
		_, err := r.handleDeletion(ctx, markForDeletion(t, r, marked))
		require.NoError(t, err)
		held, exists := storedVM(t, r)
		require.True(t, exists, "a Create may still be running")
		assert.Equal(t, k8s.ReasonCreateOutcomeUnknown, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
	})
}

func TestCreateMarker_DeleteIsHeldWithoutOwnerLookup(t *testing.T) {
	ctx := context.Background()
	// vSphere does not report owner stamps; Proxmox lists none here.
	for _, pt := range []infravirtrigaudiov1beta1.ProviderType{infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeProxmox} {
		t.Run(string(pt), func(t *testing.T) {
			r, rp, rec := createRecordFixture(t, pt, nil)
			marked := seedMarker(t, r, time.Hour)
			res, err := r.handleDeletion(ctx, markForDeletion(t, r, marked))
			require.NoError(t, err)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter)
			assert.Empty(t, rp.deleteRefs)
			held, exists := storedVM(t, r)
			require.True(t, exists, "never released silently")
			ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
			assert.Equal(t, k8s.ReasonCreateOutcomeUnknown, ready.Reason)
			assert.Equal(t, createOutcomeUnknownDeleteMessage, ready.Message)
			assert.Equal(t, 1, eventsWithPrefix(rec, "Warning "+k8s.ReasonCreateOutcomeUnknown))

			// Repeated: no write, no event.
			_, err = r.handleDeletion(ctx, held.DeepCopy())
			require.NoError(t, err)
			again, _ := storedVM(t, r)
			assert.Equal(t, held.ResourceVersion, again.ResourceVersion)
			assert.Zero(t, eventsWithPrefix(rec, "Warning "+k8s.ReasonCreateOutcomeUnknown))

			// force-delete releases it, with a VMLeftOnHypervisor event.
			again.Annotations = map[string]string{forceDeleteAnnotation: "true"}
			require.NoError(t, r.Update(ctx, again))
			forced, _ := storedVM(t, r)
			_, err = r.handleDeletion(ctx, forced)
			require.NoError(t, err)
			_, exists = storedVM(t, r)
			assert.False(t, exists)
			assert.Equal(t, 1, eventsWithPrefix(rec, "Warning "+eventReasonVMLeftOnHypervisor))
		})
	}
}

func TestCreateMarker_DeleteReleasedOnNotCreated(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
	marked := seedMarker(t, r, time.Hour)
	marked.Annotations = map[string]string{createOutcomeAnnotation: createOutcomeNotCreatedPrefix + markerAttemptID(marked)}
	require.NoError(t, r.Update(ctx, marked))
	stored, _ := storedVM(t, r)
	_, err := r.handleDeletion(ctx, markForDeletion(t, r, stored))
	require.NoError(t, err)
	assert.Empty(t, rp.deleteRefs)
	_, exists := storedVM(t, r)
	assert.False(t, exists)
}

func TestCreateMarker_ProviderDeletingNeverWritesIt(t *testing.T) {
	ctx := context.Background()

	t.Run("an unbound VM", func(t *testing.T) {
		r, rp, _ := createRecordFixture(t, "", nil)
		providerCR := &infravirtrigaudiov1beta1.Provider{}
		require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-prov"}, providerCR))
		providerCR.Finalizers = []string{"infra.virtrigaud.io/in-use"}
		require.NoError(t, r.Update(ctx, providerCR))
		require.NoError(t, r.Delete(ctx, providerCR))
		vm, _ := storedVM(t, r)
		_, err := r.reconcileVM(ctx, vm)
		require.NoError(t, err)
		held, _ := storedVM(t, r)
		assert.False(t, createRequested(held), "the hold never records a Create")
		assert.Equal(t, k8s.ReasonProviderDeleting, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionProvisioning).Reason)
		assert.Empty(t, rp.createReqs)
	})

	t.Run("a VM with the marker keeps it", func(t *testing.T) {
		r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
		marked := seedMarker(t, r, time.Minute)
		providerCR := &infravirtrigaudiov1beta1.Provider{}
		require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-prov"}, providerCR))
		providerCR.Finalizers = []string{"infra.virtrigaud.io/in-use"}
		require.NoError(t, r.Update(ctx, providerCR))
		require.NoError(t, r.Delete(ctx, providerCR))
		_, err := r.reconcileVM(ctx, marked)
		require.NoError(t, err)
		held, _ := storedVM(t, r)
		assert.True(t, createRequested(held), "the marker (the VM may exist) is kept")
		assert.Equal(t, k8s.ReasonProviderDeleting, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
		assert.Empty(t, rp.createReqs)
	})
}

func TestCreateMarker_ProviderRefChangedIsRefused(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
	marked := seedMarker(t, r, time.Minute)
	marked.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: "elsewhere"}
	require.NoError(t, r.Status().Update(ctx, marked))
	got, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, got)
	require.NoError(t, err)
	assert.Empty(t, rp.createReqs, "a Create requested through another Provider is never re-sent through this one")
	held, _ := storedVM(t, r)
	assert.Equal(t, k8s.ReasonProviderRefMismatch, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
}

func TestVMUpdateNeedsReconcile_CreateOutcomeAnnotation(t *testing.T) {
	old := creatableVM("default")
	annotated := old.DeepCopy()
	annotated.Annotations = map[string]string{createOutcomeAnnotation: createOutcomeNotCreatedPrefix + "2026-10-11T10:00:00Z"}
	assert.True(t, vmUpdateNeedsReconcile(old, annotated))
	other := old.DeepCopy()
	other.Annotations = map[string]string{"example.com/x": "y"}
	assert.False(t, vmUpdateNeedsReconcile(old, other))
}

// TestCreateMarker_PreRequestFailureKeepsIt (review M2): a re-send that fails
// before any provider call (here an invalid spec) records the failure on
// Ready only; the marker of the earlier attempt is kept.
func TestCreateMarker_PreRequestFailureKeepsIt(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
	marked := seedMarker(t, r, time.Minute)
	marked.Spec.ImportedDisk = nil
	require.NoError(t, r.Update(ctx, marked))
	stored, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, stored)
	require.Error(t, err)
	assert.Empty(t, rp.createReqs)
	held, _ := storedVM(t, r)
	assert.True(t, createRequested(held), "the marker of the earlier attempt is kept")
	assert.Equal(t, k8s.ReasonProviderError, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)

	// Without a marker the failure is on Provisioning, as before.
	plain := creatableVM("default")
	plain.Spec.ImportedDisk = nil
	setCreateFailure(plain, k8s.ReasonProviderError, "boom")
	assert.Equal(t, k8s.ReasonProviderError, meta.FindStatusCondition(plain.Status.Conditions, k8s.ConditionProvisioning).Reason)
}

// TestCreateMarker_ConflictOnAResendIsNotARefusal (review M1): vSphere answers
// Conflict for the VM an earlier attempt made while its clone is still in
// progress, so a Conflict answering a re-send keeps the marker. A Conflict
// answering a first Create is still a refusal.
func TestCreateMarker_ConflictOnAResendIsNotARefusal(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeVSphere, nil)
	rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, wireConflict()
	}
	marked := seedMarker(t, r, time.Minute)
	res, err := r.reconcileVM(ctx, marked)
	require.NoError(t, err)
	assert.Equal(t, vmCreateConflictRetryInterval, res.RequeueAfter)
	require.Len(t, rp.createReqs, 1)
	held, _ := storedVM(t, r)
	assert.True(t, createRequested(held), "the outcome of the re-send is unknown: the marker stays")
	assert.Equal(t, k8s.ReasonProviderConflict, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)

	assert.False(t, createOutcomeKnown(wireConflict(), createAttempt{resend: true}))
	assert.False(t, createOutcomeKnown(wireConflict(), createAttempt{priorID: "vm-old"}))
	assert.True(t, createOutcomeKnown(wireConflict(), createAttempt{}), "a first Create's Conflict is a refusal")
}

// TestCreateNothingDone: only CREATE_NOT_PERFORMED (and a non-retryable
// refusal) says the provider made nothing. VM_OPERATION_FAILED,
// VM_DISK_CHECK_FAILED and HOST_UNAVAILABLE may come from an operation that
// can still complete on the host, so they leave the outcome unknown.
func TestCreateNothingDone(t *testing.T) {
	notPerformed := contracts.MarkCreateNotPerformed(contracts.NewRetryableError("create: clone refused", nil))
	assert.True(t, createDefinitelyRefused(notPerformed))
	assert.True(t, createDefinitelyRefused(contracts.MarkCreateNotPerformed(errors.New("create failed: x"))))
	assert.False(t, createDefinitelyRefused(operationFailedCreate(t)))
	assert.False(t, createDefinitelyRefused(diskCheckFailedCreate()))
	assert.False(t, createDefinitelyRefused(contracts.NewHostUnavailableError("create: host down", nil)))
	assert.False(t, createDefinitelyRefused(contracts.NewRetryableError("create: host busy", nil)))
	assert.False(t, createDefinitelyRefused(transportUnavailable("create")))
	// M1 under these rules: a Conflict answering a re-send stays unknown.
	assert.False(t, createOutcomeKnown(wireConflict(), createAttempt{resend: true}))
	assert.False(t, createOutcomeKnown(notPerformed, createAttempt{resend: true}),
		"CREATE_NOT_PERFORMED answering a re-send says nothing about the first attempt")
	assert.True(t, createOutcomeKnown(notPerformed, createAttempt{}), "it is definite for a first Create")
}

// TestCreateMarker_CreateNotPerformedReplacesIt: a Create answered with
// CREATE_NOT_PERFORMED made nothing, so the marker is replaced by the failure
// and the next reconcile creates normally (no CreateOutcomeUnknown hold).
func TestCreateMarker_CreateNotPerformedReplacesIt(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, "", nil)
	rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, contracts.MarkCreateNotPerformed(contracts.NewRetryableError("create: nothing remains", nil))
	}
	vm, _ := storedVM(t, r)
	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
	got, _ := storedVM(t, r)
	assert.False(t, createRequested(got))
	assert.Equal(t, k8s.ReasonProviderError, meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionProvisioning).Reason)

	rp.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{ID: "vm-" + req.Name}, nil
	}
	_, err = r.reconcileVM(ctx, got)
	require.NoError(t, err)
	require.Len(t, rp.createReqs, 2, "created normally, not held for an operator")
	done, _ := storedVM(t, r)
	assert.Equal(t, "vm-web", done.Status.ID)
}

// TestCreateOutcomeAnnotation_BoundToTheAttempt (review M3): a confirmation
// names one attempt; a value for an older attempt (or a malformed one) is
// ignored and removed, and authorizes nothing.
func TestCreateOutcomeAnnotation_BoundToTheAttempt(t *testing.T) {
	ctx := context.Background()
	r, rp, rec := createRecordFixture(t, "", nil)
	marked := seedMarker(t, r, time.Minute)
	older := createOutcomeNotCreatedPrefix + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, value := range []string{older, "not-created", "yes"} {
		stored, _ := storedVM(t, r)
		stored.Annotations = map[string]string{createOutcomeAnnotation: value}
		require.NoError(t, r.Update(ctx, stored))
		stored, _ = storedVM(t, r)
		_, err := r.reconcileVM(ctx, stored)
		require.NoError(t, err)
		assert.Empty(t, rp.createReqs, "%q confirms nothing", value)
		after, _ := storedVM(t, r)
		assert.NotContains(t, after.Annotations, createOutcomeAnnotation, "%q is removed", value)
		assert.True(t, createRequested(after))
	}
	assert.Zero(t, eventsWithPrefix(rec, "Warning "+eventReasonCreateOutcomeConfirmed))

	// The value naming this attempt authorizes the create, with an audit event.
	stored, _ := storedVM(t, r)
	stored.Annotations = map[string]string{createOutcomeAnnotation: createOutcomeNotCreatedPrefix + markerAttemptID(marked)}
	require.NoError(t, r.Update(ctx, stored))
	stored, _ = storedVM(t, r)
	_, err := r.reconcileVM(ctx, stored)
	require.NoError(t, err)
	require.Len(t, rp.createReqs, 1)
	assert.Equal(t, 1, eventsWithPrefix(rec, "Warning "+eventReasonCreateOutcomeConfirmed))
	done, _ := storedVM(t, r)
	assert.NotContains(t, done.Annotations, createOutcomeAnnotation, "removed before the new attempt")
}

// TestCreateOutcomeAnnotation_DeleteIsAudited (review M3): a confirmation
// that releases a deleting VM records the audit event; one for another
// attempt does not release it.
func TestCreateOutcomeAnnotation_DeleteIsAudited(t *testing.T) {
	ctx := context.Background()
	r, _, rec := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
	marked := seedMarker(t, r, time.Hour)
	marked.Annotations = map[string]string{createOutcomeAnnotation: createOutcomeNotCreatedPrefix + "2020-01-01T00:00:00Z"}
	require.NoError(t, r.Update(ctx, marked))
	stored, _ := storedVM(t, r)
	_, err := r.handleDeletion(ctx, markForDeletion(t, r, stored))
	require.NoError(t, err)
	held, exists := storedVM(t, r)
	require.True(t, exists, "a confirmation of another attempt releases nothing")
	assert.NotContains(t, held.Annotations, createOutcomeAnnotation)

	held.Annotations = map[string]string{createOutcomeAnnotation: createOutcomeNotCreatedPrefix + markerAttemptID(held)}
	require.NoError(t, r.Update(ctx, held))
	confirmed, _ := storedVM(t, r)
	_, err = r.handleDeletion(ctx, confirmed)
	require.NoError(t, err)
	_, exists = storedVM(t, r)
	assert.False(t, exists)
	assert.Equal(t, 1, eventsWithPrefix(rec, "Warning "+eventReasonCreateOutcomeConfirmed))
}

// TestCreateMarker_LiveCheckOfTheDecision (review L2): a re-send or a
// confirmed create proceeds only while the live object still carries the
// marker attempt — and, for a confirmation, the confirmation — it was decided
// on.
func TestCreateMarker_LiveCheckOfTheDecision(t *testing.T) {
	ctx := context.Background()

	t.Run("another attempt recorded meanwhile", func(t *testing.T) {
		r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
		cached := seedMarker(t, r, time.Hour)
		seedMarker(t, r, time.Minute) // a newer attempt, unseen by the cached copy
		_, err := r.reconcileVM(ctx, cached)
		require.NoError(t, err)
		assert.Empty(t, rp.createReqs)
	})

	t.Run("confirmation removed meanwhile", func(t *testing.T) {
		r, rp, _ := createRecordFixture(t, "", nil)
		marked := seedMarker(t, r, time.Minute)
		marked.Annotations = map[string]string{createOutcomeAnnotation: createOutcomeNotCreatedPrefix + markerAttemptID(marked)}
		require.NoError(t, r.Update(ctx, marked))
		cached, _ := storedVM(t, r)
		live := cached.DeepCopy()
		live.Annotations = nil
		require.NoError(t, r.Update(ctx, live))
		_, err := r.reconcileVM(ctx, cached)
		require.NoError(t, err)
		assert.Empty(t, rp.createReqs, "the live object no longer confirms it")
	})
}

func TestSetCreateRequested_AttemptIDsIncrease(t *testing.T) {
	vm := creatableVM("default")
	now := time.Now()
	setCreateRequested(vm, now)
	first := markerAttemptID(vm)
	setCreateRequested(vm, now) // the same second
	second := markerAttemptID(vm)
	assert.NotEqual(t, first, second)
	assert.Greater(t, second, first)
}

// TestCreateMarker_ProxmoxOwnerLookup (P): Proxmox stamps its VMs, so a
// deleting marker VM is found by its stamp and deleted by id; with none
// listed it is held (a VMID cannot be confirmed absent by name), and a listed
// VM whose stamp cannot be relied on makes the answer ambiguous.
func TestCreateMarker_ProxmoxOwnerLookup(t *testing.T) {
	ctx := context.Background()

	t.Run("found by its stamp: deleted by id", func(t *testing.T) {
		r, _, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
		lp := &listingProvider{}
		r.RemoteResolver = &stubResolver{provider: lp}
		marked := seedMarker(t, r, time.Hour)
		lp.vms = []contracts.VMInfo{{ID: "104", ProviderRaw: map[string]string{contracts.VMInfoOwnerUIDKey: string(marked.UID)}}}
		_, err := r.handleDeletion(ctx, markForDeletion(t, r, marked))
		require.NoError(t, err)
		require.Len(t, lp.deleteRefs, 1)
		assert.Equal(t, "104", lp.deleteRefs[0].ID)
		_, exists := storedVM(t, r)
		assert.False(t, exists)
	})

	t.Run("none listed: held, no name confirmation", func(t *testing.T) {
		r, _, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeProxmox, nil)
		lp := &listingProvider{}
		r.RemoteResolver = &stubResolver{provider: lp}
		marked := seedMarker(t, r, time.Hour)
		_, err := r.handleDeletion(ctx, markForDeletion(t, r, marked))
		require.NoError(t, err)
		assert.Empty(t, lp.described, "a Proxmox VMID is not confirmed absent by name")
		_, exists := storedVM(t, r)
		assert.True(t, exists)
	})

	for _, pt := range []infravirtrigaudiov1beta1.ProviderType{infravirtrigaudiov1beta1.ProviderTypeProxmox, infravirtrigaudiov1beta1.ProviderTypeLibvirt} {
		t.Run(string(pt)+": an unreliable stamp is ambiguous", func(t *testing.T) {
			r, _, _ := createRecordFixture(t, pt, nil)
			lp := &listingProvider{}
			r.RemoteResolver = &stubResolver{provider: lp}
			marked := seedMarker(t, r, time.Hour)
			lp.vms = []contracts.VMInfo{
				{ID: "other", ProviderRaw: map[string]string{contracts.VMInfoOwnerStampStateKey: "unreadable"}},
			}
			_, err := r.handleDeletion(ctx, markForDeletion(t, r, marked))
			require.NoError(t, err)
			assert.Empty(t, lp.deleteRefs)
			assert.Empty(t, lp.described, "an ambiguous listing concludes nothing")
			held, exists := storedVM(t, r)
			require.True(t, exists, "a VM whose stamp cannot be read may be this one")
			assert.Equal(t, k8s.ReasonCreateOutcomeUnknown, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
		})
	}
}

// TestCreateMarker_RefusedResendKeepsIt (review H1): a refusal answering a
// re-send says nothing about what the first attempt made, so the marker is
// kept and the answer recorded on Ready only.
func TestCreateMarker_RefusedResendKeepsIt(t *testing.T) {
	permissionDenied := fmt.Errorf("create failed: permission VM.Allocate denied: %w",
		status.Error(codes.PermissionDenied, "permission VM.Allocate denied"))
	for name, refusal := range map[string]struct {
		err    error
		reason string
	}{
		"InvalidSpec":          {err: contracts.NewInvalidSpecError("create: bad template", nil), reason: k8s.ReasonValidationError},
		"PermissionDenied":     {err: permissionDenied, reason: k8s.ReasonCreateRefused},
		"CREATE_NOT_PERFORMED": {err: contracts.MarkCreateNotPerformed(contracts.NewRetryableError("create: nothing made", nil)), reason: k8s.ReasonProviderError},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeVSphere, nil)
			rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
				return contracts.CreateResponse{}, refusal.err
			}
			marked := seedMarker(t, r, time.Hour)
			_, err := r.reconcileVM(ctx, marked)
			require.NoError(t, err)
			require.Len(t, rp.createReqs, 1, "the Create was re-sent")
			held, _ := storedVM(t, r)
			assert.True(t, createRequested(held), "the first attempt's outcome is still unknown: the marker stays")
			assert.Equal(t, refusal.reason, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
		})
	}
}

// TestCreateMarker_ResendsAreBackedOff (review L1): re-sends of a requested
// Create are paced from the marker's last attempt, by how long the failing
// streak has lasted — no re-send (and no status write) every few seconds.
func TestCreateMarker_ResendsAreBackedOff(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, nil)
	marked := seedMarker(t, r, 5*time.Second)
	res, err := r.reconcileVM(ctx, marked)
	require.NoError(t, err)
	assert.Empty(t, rp.createReqs, "the last attempt was 5 s ago: not yet")
	assert.InDelta(t, (10 * time.Second).Seconds(), res.RequeueAfter.Seconds(), 2)
	unchanged, _ := storedVM(t, r)
	assert.Equal(t, marked.ResourceVersion, unchanged.ResourceVersion, "waiting writes nothing")

	now := time.Now()
	cases := []struct {
		name            string
		lastAttemptAgo  time.Duration
		streakAgo       time.Duration
		wantWaitAtLeast time.Duration
		due             bool
	}{
		{name: "first re-send after 15 s", lastAttemptAgo: 20 * time.Second, due: true},
		{name: "a 4 min streak waits 4 min after the last attempt", lastAttemptAgo: time.Minute, streakAgo: 4 * time.Minute, wantWaitAtLeast: 2 * time.Minute},
		{name: "a marker found long after its attempt is re-sent at once", lastAttemptAgo: time.Hour, due: true},
	}
	for _, tc := range cases {
		vm := creatableVM("default")
		setCreateRequested(vm, now.Add(-tc.lastAttemptAgo))
		if tc.streakAgo > 0 {
			vm.Status.Conditions = append(vm.Status.Conditions, metav1.Condition{Type: k8s.ConditionReady, Status: metav1.ConditionFalse,
				Reason: k8s.ReasonProviderError, LastTransitionTime: metav1.NewTime(now.Add(-tc.streakAgo))})
		}
		wait := resendWait(vm, now)
		if tc.due {
			assert.LessOrEqual(t, wait, time.Duration(0), tc.name)
		} else {
			assert.GreaterOrEqual(t, wait, tc.wantWaitAtLeast, tc.name)
		}
	}
}

// TestCreateMarker_LiveDeletionOrNewerSpecNeverCreates (review L2): the live
// re-read before the Create also refuses a VM being deleted, and one whose
// spec changed since the reconcile read it.
func TestCreateMarker_LiveDeletionOrNewerSpecNeverCreates(t *testing.T) {
	ctx := context.Background()

	t.Run("being deleted", func(t *testing.T) {
		r, rp, _ := createRecordFixture(t, "", nil)
		cached, _ := storedVM(t, r)
		require.NoError(t, r.Delete(ctx, cached.DeepCopy()))
		res, err := r.reconcileVM(ctx, cached)
		require.NoError(t, err)
		assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
		assert.Empty(t, rp.createReqs)
	})

	t.Run("newer spec", func(t *testing.T) {
		r, rp, _ := createRecordFixture(t, "", nil)
		cached, _ := storedVM(t, r)
		edited := cached.DeepCopy()
		edited.Generation = cached.Generation + 1
		require.NoError(t, r.Update(ctx, edited))
		res, err := r.reconcileVM(ctx, cached)
		require.NoError(t, err)
		assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
		assert.Empty(t, rp.createReqs, "the request was built from an older spec")
	})
}

// TestCreateOutcomeConfirmed_AuditedOnlyWhenActedOn (review L3): the audit
// event of a confirmation is recorded only once the new marker is written; a
// failed marker write records none.
func TestCreateOutcomeConfirmed_AuditedOnlyWhenActedOn(t *testing.T) {
	ctx := context.Background()
	var failMarker bool
	r, rp, rec := createRecordFixture(t, "", func(_ context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
		if failMarker && createRequested(vm) {
			return errAPIUnavailable
		}
		return nil
	})
	marked := seedMarker(t, r, time.Minute)
	marked.Annotations = map[string]string{createOutcomeAnnotation: createOutcomeNotCreatedPrefix + markerAttemptID(marked)}
	require.NoError(t, r.Update(ctx, marked))

	failMarker = true
	stored, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, stored)
	require.NoError(t, err)
	assert.Empty(t, rp.createReqs)
	assert.Zero(t, eventsWithPrefix(rec, "Warning "+eventReasonCreateOutcomeConfirmed), "not acted on: no audit event")

	failMarker = false
	stored, _ = storedVM(t, r)
	stored.Annotations = map[string]string{createOutcomeAnnotation: createOutcomeNotCreatedPrefix + markerAttemptID(stored)}
	require.NoError(t, r.Update(ctx, stored))
	stored, _ = storedVM(t, r)
	_, err = r.reconcileVM(ctx, stored)
	require.NoError(t, err)
	require.Len(t, rp.createReqs, 1)
	assert.Equal(t, 1, eventsWithPrefix(rec, "Warning "+eventReasonCreateOutcomeConfirmed))
}

// TestTruncateForLog (review L4): a tenant-written annotation value is
// bounded in the manager log.
func TestTruncateForLog(t *testing.T) {
	assert.Equal(t, "short", truncateForLog("short", maxLoggedAnnotationBytes))
	long := strings.Repeat("é", 200) // 400 bytes
	got := truncateForLog(long, maxLoggedAnnotationBytes)
	assert.LessOrEqual(t, len(got), maxLoggedAnnotationBytes+len("…"))
	assert.True(t, strings.HasSuffix(got, "…"))
	assert.True(t, utf8.ValidString(got), "cut on a rune boundary")
}
