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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the record of a single-host Create: the create marker is
// recorded before the Create is sent, the status write that carries the new
// status.id must not be lost to a Conflict (the VM edited, or its deletion
// requested, during a long Create) nor overwrite an id recorded meanwhile,
// and when it cannot be written at all the manager neither creates a second
// VM nor releases a deleted VirtualMachine without deleting the one it
// created.

// statusPatchHook decides a status write (patch or update) of vm in the
// createRecordFixture: a non-nil error fails it. ctx is the call's context.
type statusPatchHook func(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error

// createRecordFixture seeds a single-host Provider of providerType, its class
// and a creatable VM with the finalizer; hook, when set, may fail a status
// patch. It returns the reconciler, the provider stub and the event recorder.
func createRecordFixture(t *testing.T, providerType infravirtrigaudiov1beta1.ProviderType, hook statusPatchHook) (
	*VirtualMachineReconciler, *routingProvider, *record.FakeRecorder,
) {
	t.Helper()
	s := coverageTestScheme(t)
	prov, class := providerAndClass("default")
	prov.UID = "uid-provider"
	prov.Spec.Type = providerType
	vm := creatableVM("default")
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(prov, class, vm).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if vmObj, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine); ok && hook != nil {
					if err := hook(ctx, vmObj); err != nil {
						return err
					}
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if vmObj, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine); ok && hook != nil {
					if err := hook(ctx, vmObj); err != nil {
						return err
					}
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	rp := &routingProvider{onCreate: func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{ID: "vm-" + req.Name}, nil
	}}
	rec := record.NewFakeRecorder(16)
	r := &VirtualMachineReconciler{
		Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: rp}, Recorder: rec,
		recordBackoff: &wait.Backoff{Steps: 3, Duration: time.Millisecond, Factor: 1},
	}
	return r, rp, rec
}

// failRecordWrites fails every status patch that records an id (the record),
// never the create marker, while failing reports true.
func failRecordWrites(failing *atomic.Bool, err error) statusPatchHook {
	return func(_ context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
		if vm.Status.ID != "" && failing.Load() {
			return err
		}
		return nil
	}
}

// storedVM reads the fixture's VM; ok is false once it is gone.
func storedVM(t *testing.T, r *VirtualMachineReconciler) (*infravirtrigaudiov1beta1.VirtualMachine, bool) {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "web"}, vm)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return vm, true
}

// recordNotSavedEvents returns the VMRecordNotSaved Warning events in rec.
func recordNotSavedEvents(rec *record.FakeRecorder) []string {
	var out []string
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+eventReasonVMRecordNotSaved) {
			out = append(out, e)
		}
	}
	return out
}

var errAPIUnavailable = apierrors.NewServiceUnavailable("etcd leader changed")

// invalidTest is the API server's answer to a failed JSON Patch test.
func invalidTest() error {
	return apierrors.NewInvalid(schema.GroupKind{Group: "infra.virtrigaud.io", Kind: "VirtualMachine"}, "web",
		field.ErrorList{field.Invalid(field.NewPath("status", "id"), "x", "test operation failed")})
}

func TestCreateVM_SingleHost_RecordSurvivesAnEditDuringCreate(t *testing.T) {
	ctx := context.Background()
	r, rp, rec := createRecordFixture(t, "", nil)
	rp.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		// A tenant labels the VM while the provider creates it: the VM's
		// resourceVersion moves on, so a plain status update would conflict.
		vm, _ := storedVM(t, r)
		vm.Labels = map[string]string{"team": "a"}
		require.NoError(t, r.Update(ctx, vm))
		return contracts.CreateResponse{ID: "vm-" + req.Name}, nil
	}

	vm, _ := storedVM(t, r)
	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)

	got, _ := storedVM(t, r)
	assert.Equal(t, "vm-web", got.Status.ID, "the created VM's id is recorded although the VM was edited during the Create")
	require.NotNil(t, got.Status.BoundProvider)
	assert.Equal(t, "uid-provider", got.Status.BoundProvider.UID)
	assert.Equal(t, map[string]string{"team": "a"}, got.Labels, "the edit is kept")
	assert.False(t, createRequested(got), "the marker is replaced by the Create's outcome")
	assert.Equal(t, k8s.ReasonReconcileSuccess, meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionProvisioning).Reason)
	assert.Empty(t, recordNotSavedEvents(rec))
	_, remembered := r.lookupUnsavedCreate(got)
	assert.False(t, remembered)
}

func TestCreateVM_SingleHost_DeleteRequestedDuringCreateDeletesTheVM(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, "", nil)
	rp.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		vm, _ := storedVM(t, r)
		require.NoError(t, r.Delete(ctx, vm), "the owner deletes the VM while it is being created")
		return contracts.CreateResponse{ID: "vm-" + req.Name}, nil
	}

	vm, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	deleting, exists := storedVM(t, r)
	require.True(t, exists)
	require.False(t, deleting.DeletionTimestamp.IsZero())
	assert.Equal(t, "vm-web", deleting.Status.ID, "the id is recorded on the deleting VM")

	_, err = r.handleDeletion(ctx, deleting)
	require.NoError(t, err)
	require.Len(t, rp.deleteRefs, 1, "the created VM is deleted through the provider, not left behind")
	assert.Equal(t, "vm-web", rp.deleteRefs[0].ID)
	_, exists = storedVM(t, r)
	assert.False(t, exists)
}

func TestCreateVM_SingleHost_UnsavedRecordIsNotCreatedTwice(t *testing.T) {
	ctx := context.Background()
	var failing atomic.Bool
	failing.Store(true)
	r, rp, rec := createRecordFixture(t, "", failRecordWrites(&failing, errAPIUnavailable))

	vm, _ := storedVM(t, r)
	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter, "re-checked shortly")
	require.Len(t, rp.createReqs, 1)
	got, _ := storedVM(t, r)
	assert.Empty(t, got.Status.ID, "the record could not be written")
	assert.True(t, createRequested(got), "the durable marker says a Create was requested")
	events := recordNotSavedEvents(rec)
	require.Len(t, events, 1, "a Warning event names the created VM")
	assert.Contains(t, events[0], `"vm-web"`)
	assert.Contains(t, events[0], "default/test-prov")

	// Still failing: no second Create, no second event.
	_, err = r.reconcileVM(ctx, got.DeepCopy())
	require.NoError(t, err)
	assert.Len(t, rp.createReqs, 1, "never a second Create for a VM the provider already made")
	assert.Empty(t, recordNotSavedEvents(rec), "the event is recorded once")

	// The API server recovers: the record is written, still without a Create.
	failing.Store(false)
	got, _ = storedVM(t, r)
	_, err = r.reconcileVM(ctx, got)
	require.NoError(t, err)
	assert.Len(t, rp.createReqs, 1)
	saved, _ := storedVM(t, r)
	assert.Equal(t, "vm-web", saved.Status.ID)
	require.NotNil(t, saved.Status.BoundProvider)
	assert.Equal(t, "test-prov", saved.Status.BoundProvider.Name)
	require.NotNil(t, saved.Status.CurrentResources)
	assert.False(t, createRequested(saved))
	_, remembered := r.lookupUnsavedCreate(saved)
	assert.False(t, remembered, "the in-memory record is dropped once written")
}

func TestCreateVM_SingleHost_UnsavedRecordIsDeletedThroughTheProvider(t *testing.T) {
	ctx := context.Background()
	var failing atomic.Bool
	failing.Store(true)
	r, rp, _ := createRecordFixture(t, "", failRecordWrites(&failing, errAPIUnavailable))

	vm, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	got, _ := storedVM(t, r)
	require.Empty(t, got.Status.ID)

	_, err = r.handleDeletion(ctx, markForDeletion(t, r, got))
	require.NoError(t, err)
	require.Len(t, rp.deleteRefs, 1, "an unbound VM whose Create succeeded is not released silently")
	assert.Equal(t, "vm-web", rp.deleteRefs[0].ID)
	_, exists := storedVM(t, r)
	assert.False(t, exists)
	_, remembered := r.lookupUnsavedCreate(got)
	assert.False(t, remembered)
}

func TestCreateVM_SingleHost_TransientRecordFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	r, rp, rec := createRecordFixture(t, "", func(_ context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
		if vm.Status.ID != "" && calls.Add(1) == 1 {
			return errAPIUnavailable
		}
		return nil
	})

	vm, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	got, _ := storedVM(t, r)
	assert.Equal(t, "vm-web", got.Status.ID)
	assert.Len(t, rp.createReqs, 1)
	assert.Empty(t, recordNotSavedEvents(rec))
}

// TestCreateVM_SingleHost_ReplacedVMIsNotRecorded (review L2): a record whose
// test fails is dropped only once a live read shows the VM gone or replaced.
func TestCreateVM_SingleHost_ReplacedVMIsNotRecorded(t *testing.T) {
	ctx := context.Background()
	r, rp, rec := createRecordFixture(t, "", nil)
	rp.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		// The VirtualMachine is deleted and re-created under the same name.
		old, _ := storedVM(t, r)
		old.Finalizers = nil
		require.NoError(t, r.Update(ctx, old))
		require.NoError(t, r.Delete(ctx, old))
		replacement := creatableVM("default")
		replacement.UID = "uid-replacement"
		require.NoError(t, r.Create(ctx, replacement))
		return contracts.CreateResponse{ID: "vm-" + req.Name}, nil
	}

	vm, _ := storedVM(t, r)
	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
	events := recordNotSavedEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], `"vm-web"`)
	assert.Contains(t, events[0], "deleted or replaced")
	replacement, _ := storedVM(t, r)
	assert.Empty(t, replacement.Status.ID, "a replaced VM never inherits the old object's VM")
	_, remembered := r.lookupUnsavedCreate(vm)
	assert.False(t, remembered)
}

// TestCreateVM_SingleHost_InvalidRecordOnSameVMIsNotDropped (review L2): an
// Invalid answer for a VM that still exists, with the same UID and no id, is
// not taken for a replaced VM: the record is written without the id test.
func TestCreateVM_SingleHost_InvalidRecordOnSameVMIsNotDropped(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	r, _, rec := createRecordFixture(t, "", func(_ context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
		if vm.Status.ID != "" && calls.Add(1) == 1 {
			return invalidTest()
		}
		return nil
	})
	vm, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	got, _ := storedVM(t, r)
	assert.Equal(t, "vm-web", got.Status.ID)
	assert.Empty(t, recordNotSavedEvents(rec))
}

// TestCreateVM_SingleHost_NewerIDIsNotOverwritten (review M1): an id recorded
// on the VM while its Create ran is kept; the VM this Create made is named in
// a Warning event.
func TestCreateVM_SingleHost_NewerIDIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	r, rp, rec := createRecordFixture(t, "", func(_ context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
		if vm.Status.ID == "vm-web" {
			return invalidTest() // the id test fails: another id is stored
		}
		return nil
	})
	rp.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		vm, _ := storedVM(t, r)
		vm.Status.ID = "vm-recorded-meanwhile"
		require.NoError(t, r.Status().Update(ctx, vm))
		return contracts.CreateResponse{ID: "vm-" + req.Name}, nil
	}

	vm, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	got, _ := storedVM(t, r)
	assert.Equal(t, "vm-recorded-meanwhile", got.Status.ID, "the newer id is not overwritten")
	events := recordNotSavedEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], `"vm-web"`)
	assert.Contains(t, events[0], "records another VM id")
}

// TestCreateVM_SingleHost_RecordAttemptsAreBounded (review L3): every attempt
// of the marker and record writes carries its own deadline.
func TestCreateVM_SingleHost_RecordAttemptsAreBounded(t *testing.T) {
	var deadlines []time.Duration
	r, _, _ := createRecordFixture(t, "", func(ctx context.Context, _ *infravirtrigaudiov1beta1.VirtualMachine) error {
		d, ok := ctx.Deadline()
		require.True(t, ok, "a status patch without a deadline")
		deadlines = append(deadlines, time.Until(d))
		return nil
	})
	vm, _ := storedVM(t, r)
	_, err := r.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	require.Len(t, deadlines, 2, "the marker and the record")
	for _, d := range deadlines {
		assert.LessOrEqual(t, d, createRecordAttemptTimeout)
	}
}

func TestVMStatusPatch_TestsTheUIDAndTheID(t *testing.T) {
	vm := creatableVM("default")
	vm.Status.ID = "vm-web"
	patch, err := vmStatusPatch(vm, expectID(""))
	require.NoError(t, err)
	data, err := patch.Data(vm)
	require.NoError(t, err)
	assert.Contains(t, string(data), `{"op":"test","path":"/metadata/uid","value":"6f1c2d6e-0a57-4d0e-9d4b-6f3bb1f1a001"}`)
	assert.Contains(t, string(data), `{"op":"test","path":"/status/id","value":null}`)
	assert.Contains(t, string(data), `"op":"add","path":"/status"`)
	assert.NotContains(t, string(data), "resourceVersion", "no resourceVersion precondition")

	patch, err = vmStatusPatch(vm, expectID("vm-old"))
	require.NoError(t, err)
	data, err = patch.Data(vm)
	require.NoError(t, err)
	assert.Contains(t, string(data), `{"op":"test","path":"/status/id","value":"vm-old"}`)
}

// TestReconcileVM_ForgetsUnsavedRecordOnceAnIDIsRead (review L1).
func TestReconcileVM_ForgetsUnsavedRecordOnceAnIDIsRead(t *testing.T) {
	ctx := context.Background()
	prov := &fakeDescribeProvider{DescribeFn: func(context.Context, string) (contracts.DescribeResponse, error) {
		return contracts.DescribeResponse{Exists: true, PowerState: "On", IPs: []string{"10.0.0.5"}}, nil
	}}
	r, vm := boundSingleHostVM(t, &stubResolver{provider: prov})
	r.unsavedCreates.Store(vm.UID, unsavedCreate{id: "vm-stale"})
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	_, remembered := r.lookupUnsavedCreate(vm)
	assert.False(t, remembered)
}
