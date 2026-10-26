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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the backoff of a VM Create the provider refused as an
// invalid specification (v0.4.1): the re-sends grow from 30 s to 10 min,
// nothing is sent while the wait runs, and anything the answer depends on
// changing sends it again at once.

func TestCreateRefusalDelay_GrowsAndCaps(t *testing.T) {
	want := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		10 * time.Minute, 10 * time.Minute, 10 * time.Minute,
	}
	for i, w := range want {
		assert.Equal(t, w, createRefusalDelay(i+1), "refusal %d", i+1)
		assert.Equal(t, min(i+1, 6), createRefusalsFor(w), "the inverse of refusal %d", i+1)
	}
	assert.Equal(t, createRefusalRetryBase, createRefusalDelay(0), "never below the base")
	assert.Equal(t, createRefusalRetryMax, createRefusalDelay(1000), "never above the cap")
}

func TestCreateRefusalMessage_RoundTrip(t *testing.T) {
	next := time.Date(2026, 10, 26, 10, 4, 30, 0, time.UTC)
	detail := `create: libvirt rejected the domain definition: unsupported configuration: x (y): z`
	msg := createRefusalMessage(4*time.Minute, next, detail)
	assert.True(t, strings.HasPrefix(msg, "Provider rejected VM create (non-retryable; re-checking in 4m0s, at 2026-10-26T10:04:30Z"), msg)
	assert.True(t, strings.HasSuffix(msg, detail), msg)

	wait, at, gotDetail, ok := parseCreateRefusalMessage(msg)
	require.True(t, ok)
	assert.Equal(t, 4*time.Minute, wait)
	assert.True(t, next.Equal(at))
	assert.Equal(t, detail, gotDetail)

	for _, other := range []string{
		"",
		"Provider rejected VM create (non-retryable; re-checking every 30s): old message of v0.4.0",
		"Provider rejected VM create (non-retryable; re-checking in soon, at 2026-10-26T10:04:30Z" + createRefusalMessageTail + "x",
		"Provider rejected VM create (non-retryable; re-checking in 30s, at yesterday" + createRefusalMessageTail + "x",
		"Failed to build create request: memory out of range",
	} {
		_, _, _, ok := parseCreateRefusalMessage(other)
		assert.False(t, ok, other)
	}
}

// refusalHarness drives reconcileVM for one VM whose Create the provider
// refuses, at times the test sets.
type refusalHarness struct {
	t   *testing.T
	r   *VirtualMachineReconciler
	cp  *rejectingCreateProvider
	now time.Time
	key types.NamespacedName
}

func newRefusalHarness(t *testing.T, createErr error) *refusalHarness {
	t.Helper()
	const ns = "team-r"
	prov, class := providerAndClass(ns)
	prov.UID, prov.Generation = "prov-uid", 1
	class.UID, class.Generation = "class-uid", 1
	vm := creatableVM(ns)
	cp := &rejectingCreateProvider{createErr: createErr}
	h := &refusalHarness{
		t:   t,
		cp:  cp,
		now: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
		key: types.NamespacedName{Namespace: ns, Name: vm.Name},
	}
	h.r = newTestReconciler(coverageTestScheme(t), &stubResolver{provider: cp}, prov, class, vm)
	h.r.clock = func() time.Time { return h.now }
	return h
}

// vm reads the VM as the cache would show it.
func (h *refusalHarness) vm() *infravirtrigaudiov1beta1.VirtualMachine {
	h.t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(h.t, h.r.Get(context.Background(), h.key, vm))
	return vm
}

// reconcile runs reconcileVM at the harness time with vm (as read, when nil).
func (h *refusalHarness) reconcile(vm *infravirtrigaudiov1beta1.VirtualMachine) ctrl.Result {
	h.t.Helper()
	if vm == nil {
		vm = h.vm()
	}
	res, err := h.r.reconcileVM(context.Background(), vm)
	require.NoError(h.t, err)
	return res
}

// bumpGeneration records an edit of the VM's spec (a new generation).
func (h *refusalHarness) bumpGeneration() {
	h.t.Helper()
	vm := h.vm()
	vm.Generation++
	require.NoError(h.t, h.r.Update(context.Background(), vm))
}

// creates is the number of Create calls sent so far.
func (h *refusalHarness) creates() int { return len(h.cp.createReqs) }

// readyMessage is the persisted Ready message.
func (h *refusalHarness) readyMessage() string {
	h.t.Helper()
	c := meta.FindStatusCondition(h.vm().Status.Conditions, k8s.ConditionReady)
	require.NotNil(h.t, c)
	return c.Message
}

func invalidDefinition() error {
	return contracts.NewInvalidSpecError("create: libvirt rejected the domain definition", nil)
}

func TestReconcileVM_RefusedCreate_BacksOffAndCaps(t *testing.T) {
	h := newRefusalHarness(t, invalidDefinition())

	res := h.reconcile(nil)
	require.Equal(t, 1, h.creates())
	assert.Equal(t, 30*time.Second, res.RequeueAfter)
	vm := h.vm()
	assert.Empty(t, vm.Status.ID)
	for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
		c := meta.FindStatusCondition(vm.Status.Conditions, condType)
		require.NotNil(t, c, condType)
		assert.Equal(t, metav1.ConditionFalse, c.Status, condType)
		assert.Equal(t, k8s.ReasonValidationError, c.Reason, condType)
		assert.Equal(t, vm.Generation, c.ObservedGeneration, condType)
		assert.Equal(t, "Provider rejected VM create (non-retryable; re-checking in 30s, at 2026-10-26T09:00:30Z"+
			createRefusalMessageTail+"create: libvirt rejected the domain definition", c.Message, condType)
	}

	// The refusals space out: 30 s, 1 min, 2 min, ... at most 10 min. Each
	// reconcile in between (any event) sends nothing and waits out the rest.
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	wait := res.RequeueAfter
	for i, next := range want {
		h.now = h.now.Add(wait / 2)
		held := h.reconcile(nil)
		assert.Equal(t, i+1, h.creates(), "nothing is sent half-way through the wait")
		assert.Equal(t, wait-wait/2, held.RequeueAfter, "the rest of the wait, never a tight loop")

		h.now = h.now.Add(wait - wait/2)
		res = h.reconcile(nil)
		assert.Equal(t, i+2, h.creates(), "sent again when the wait is over")
		assert.Equal(t, next, res.RequeueAfter)
		assert.Contains(t, h.readyMessage(), "re-checking in "+next.String()+", at "+h.now.Add(next).Format(time.RFC3339))
		wait = next
	}
}

func TestReconcileVM_RefusedCreate_SpecChangeRetriesAtOnce(t *testing.T) {
	h := newRefusalHarness(t, invalidDefinition())
	h.reconcile(nil)
	h.now = h.now.Add(30 * time.Second)
	res := h.reconcile(nil)
	require.Equal(t, 2, h.creates())
	require.Equal(t, time.Minute, res.RequeueAfter)

	// An edit of the VM is a new generation: sent at once, and the run
	// starts again from 30 s.
	h.now = h.now.Add(time.Second)
	h.bumpGeneration()
	res = h.reconcile(nil)
	assert.Equal(t, 3, h.creates(), "a spec change retries at once")
	assert.Equal(t, 30*time.Second, res.RequeueAfter, "and starts the backoff again")
}

func TestReconcileVM_RefusedCreate_DependencyChangeRetriesAtOnce(t *testing.T) {
	for name, edit := range map[string]func(t *testing.T, c client.Client, ns string){
		"VMClass": func(t *testing.T, c client.Client, ns string) {
			class := &infravirtrigaudiov1beta1.VMClass{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "test-class"}, class))
			class.Generation++
			require.NoError(t, c.Update(context.Background(), class))
		},
		"Provider": func(t *testing.T, c client.Client, ns string) {
			prov := &infravirtrigaudiov1beta1.Provider{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "test-prov"}, prov))
			prov.Generation++
			require.NoError(t, c.Update(context.Background(), prov))
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRefusalHarness(t, invalidDefinition())
			h.reconcile(nil)
			h.now = h.now.Add(30 * time.Second)
			h.reconcile(nil)
			require.Equal(t, 2, h.creates())

			h.now = h.now.Add(time.Second)
			h.reconcile(nil)
			require.Equal(t, 2, h.creates(), "held while nothing changed")

			edit(t, h.r.Client, h.key.Namespace)
			res := h.reconcile(nil)
			assert.Equal(t, 3, h.creates(), "a %s spec change retries at once", name)
			assert.Equal(t, 30*time.Second, res.RequeueAfter)
		})
	}
}

func TestReconcileVM_RefusedCreate_NewAnswerStartsAgain(t *testing.T) {
	h := newRefusalHarness(t, invalidDefinition())
	h.reconcile(nil)
	h.now = h.now.Add(30 * time.Second)
	res := h.reconcile(nil)
	require.Equal(t, time.Minute, res.RequeueAfter)

	h.cp.createErr = contracts.NewInvalidSpecError("create: another reason", nil)
	h.now = h.now.Add(time.Minute)
	res = h.reconcile(nil)
	assert.Equal(t, 3, h.creates())
	assert.Equal(t, 30*time.Second, res.RequeueAfter, "a different refusal starts the backoff again")
	assert.True(t, strings.HasSuffix(h.readyMessage(), "create: another reason"))
}

func TestReconcileVM_RefusedCreate_SurvivesManagerRestart(t *testing.T) {
	h := newRefusalHarness(t, invalidDefinition())
	h.reconcile(nil)
	h.now = h.now.Add(30 * time.Second)
	h.reconcile(nil)
	h.now = h.now.Add(time.Minute)
	res := h.reconcile(nil)
	require.Equal(t, 3, h.creates())
	require.Equal(t, 2*time.Minute, res.RequeueAfter)

	// A new manager: nothing in memory; the Ready message names the next
	// re-check.
	restarted := &VirtualMachineReconciler{Client: h.r.Client, Scheme: h.r.Scheme, RemoteResolver: h.r.RemoteResolver}
	restarted.clock = func() time.Time { return h.now }
	h.r = restarted

	h.now = h.now.Add(20 * time.Second)
	held := h.reconcile(nil)
	assert.Equal(t, 3, h.creates(), "the first reconcile after a restart keeps the wait")
	assert.Equal(t, 100*time.Second, held.RequeueAfter)

	h.now = h.now.Add(100 * time.Second)
	res = h.reconcile(nil)
	assert.Equal(t, 4, h.creates())
	assert.Equal(t, 4*time.Minute, res.RequeueAfter, "the run goes on where it was")

	// A spec change while no manager ran is a new generation: not held.
	restarted = &VirtualMachineReconciler{Client: h.r.Client, Scheme: h.r.Scheme, RemoteResolver: h.r.RemoteResolver}
	restarted.clock = func() time.Time { return h.now }
	h.r = restarted
	h.bumpGeneration()
	h.reconcile(nil)
	assert.Equal(t, 5, h.creates(), "a VM edited while no manager ran is sent at once")
}

func TestReconcileVM_RefusedCreate_RestartWithOldMessageSendsOnce(t *testing.T) {
	// A VM refused by a manager before v0.4.1 ("re-checking every 30s") is
	// sent once, then backs off.
	h := newRefusalHarness(t, invalidDefinition())
	vm := h.vm()
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type: k8s.ConditionReady, Status: metav1.ConditionFalse, Reason: k8s.ReasonValidationError,
		Message: "Provider rejected VM create (non-retryable; re-checking every 30s): x", ObservedGeneration: vm.Generation,
	})
	require.NoError(t, h.r.Status().Update(context.Background(), vm))
	res := h.reconcile(nil)
	assert.Equal(t, 1, h.creates())
	assert.Equal(t, 30*time.Second, res.RequeueAfter)
	h.reconcile(nil)
	assert.Equal(t, 1, h.creates())
}

func TestReconcileVM_RefusedCreate_SuccessForgetsTheRun(t *testing.T) {
	h := newRefusalHarness(t, invalidDefinition())
	h.reconcile(nil)
	h.now = h.now.Add(30 * time.Second)
	h.cp.createErr = nil
	h.reconcile(nil)
	require.Equal(t, 2, h.creates())
	require.NotEmpty(t, h.vm().Status.ID)
	h.reconcile(nil) // bound: the run is dropped
	assert.False(t, h.r.createRefusals.known(h.vm().UID))
}

func TestReconcileVM_ConflictKeepsFixedCadence(t *testing.T) {
	h := newRefusalHarness(t, wireConflict())
	res := h.reconcile(nil)
	assert.Equal(t, vmCreateConflictRetryInterval, res.RequeueAfter)
	assert.Contains(t, h.readyMessage(), "re-checking every 2m0s", "a Conflict is unchanged")
	assert.False(t, h.r.createRefusals.known(h.vm().UID))
	h.now = h.now.Add(vmCreateConflictRetryInterval)
	res = h.reconcile(nil)
	assert.Equal(t, 2, h.creates())
	assert.Equal(t, vmCreateConflictRetryInterval, res.RequeueAfter)
}

func TestCreateRefusalBackoff_PrunesAndDropsOtherInputs(t *testing.T) {
	var b createRefusalBackoff
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	assert.Equal(t, 30*time.Second, b.refused("a", "in-1", "d", now))
	assert.Equal(t, 30*time.Second, b.wait("a", "in-1", now))
	assert.Zero(t, b.wait("a", "in-2", now), "other inputs end the wait")
	assert.False(t, b.known("a"), "and drop the run")

	b.refused("old", "in", "d", now)
	later := now.Add(createRefusalRetryBase + createRefusalForget + time.Second)
	b.refused("b", "in", "d", later)
	assert.False(t, b.known("old"), "a run idle past createRefusalForget is pruned")
	assert.True(t, b.known("b"))
}

func TestCreateRefusalIndexValues(t *testing.T) {
	refused := creatableVM("team-a")
	refused.Spec.ImportedDisk = nil
	refused.Spec.ImageRef = &infravirtrigaudiov1beta1.ObjectRef{Name: "jammy", Namespace: "images"}
	refused.Status.Conditions = []metav1.Condition{{Type: k8s.ConditionReady, Status: metav1.ConditionFalse, Reason: k8s.ReasonValidationError}}
	assert.ElementsMatch(t, []string{
		consumerRefIndexValue(consumerKindProvider, types.NamespacedName{Namespace: "team-a", Name: "test-prov"}),
		consumerRefIndexValue(consumerKindVMClass, types.NamespacedName{Namespace: "team-a", Name: "test-class"}),
		consumerRefIndexValue(consumerKindVMImage, types.NamespacedName{Namespace: "images", Name: "jammy"}),
	}, createRefusalIndexValues(refused))

	marker := refused.DeepCopy()
	marker.Status.Conditions = []metav1.Condition{{Type: k8s.ConditionProvisioning, Status: metav1.ConditionFalse, Reason: k8s.ReasonValidationError}}
	assert.Len(t, createRefusalIndexValues(marker), 3, "a refusal recorded on Provisioning counts too")

	bound := refused.DeepCopy()
	bound.Status.ID = "vm-1"
	assert.Empty(t, createRefusalIndexValues(bound), "a created VM is not indexed")

	other := refused.DeepCopy()
	other.Status.Conditions[0].Reason = k8s.ReasonProviderError
	assert.Empty(t, createRefusalIndexValues(other), "only a refusal is indexed")
	assert.Empty(t, createRefusalIndexValues(&infravirtrigaudiov1beta1.Provider{}))
}

func TestVMsForCreateRefusalChange(t *testing.T) {
	s := coverageTestScheme(t)
	refused := creatableVM("team-a")
	refused.Status.Conditions = []metav1.Condition{{Type: k8s.ConditionReady, Status: metav1.ConditionFalse, Reason: k8s.ReasonValidationError}}
	healthy := creatableVM("team-a")
	healthy.Name, healthy.UID = "healthy", "healthy-uid"
	healthy.Status.ID = "healthy"
	otherClass := creatableVM("team-a")
	otherClass.Name, otherClass.UID = "other", "other-uid"
	otherClass.Spec.ClassRef.Name = "large"
	otherClass.Status.Conditions = refused.Status.Conditions
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(refused, healthy, otherClass).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, createRefusalRefIndex, createRefusalIndexValues).Build()
	r := &VirtualMachineReconciler{Client: c, Scheme: s}

	class := &infravirtrigaudiov1beta1.VMClass{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "test-class"}}
	reqs := r.vmsForCreateRefusalChange(context.Background(), class)
	require.Len(t, reqs, 1, "only the refused VM that references the class")
	assert.Equal(t, types.NamespacedName{Namespace: "team-a", Name: "web"}, reqs[0].NamespacedName)

	prov := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "test-prov"}}
	assert.Len(t, r.vmsForCreateRefusalChange(context.Background(), prov), 2)
	assert.Empty(t, r.vmsForCreateRefusalChange(context.Background(), &infravirtrigaudiov1beta1.VirtualMachine{}))
}

func TestSpecChangedPredicate(t *testing.T) {
	p := specChanged()
	oldClass := &infravirtrigaudiov1beta1.VMClass{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	newClass := oldClass.DeepCopy()
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: oldClass, ObjectNew: newClass}), "a status-only update")
	newClass.Generation = 2
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: oldClass, ObjectNew: newClass}), "a spec change")
	assert.False(t, p.Create(event.CreateEvent{Object: newClass}))
	assert.False(t, p.Delete(event.DeleteEvent{Object: newClass}))
	assert.False(t, p.Generic(event.GenericEvent{Object: newClass}))
}

// TestEnsureImageOnProvider_RejectedSourceBacksOff proves an image source the
// provider refuses as invalid is not prepared again (downloaded again, for
// libvirt) on every VM re-check: 30 s doubling to 10 min, and at once for a
// changed spec.source.
func TestEnsureImageOnProvider_RejectedSourceBacksOff(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	r.clock = func() time.Time { return now }
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{prepareErr: contracts.NewInvalidSpecError("image prepare: checksum mismatch", nil)}
	vm := vmForImage(provider.Name, img.Name)
	ensure := func() error {
		t.Helper()
		_, err := r.EnsureImageOnProvider(context.Background(), vm, reloadImage(t, r, img.Name), provider, inst)
		require.Error(t, err)
		return err
	}

	assert.True(t, contracts.IsInvalidSpec(ensure()))
	require.Equal(t, 1, inst.prepareCalls)

	held := ensure()
	assert.True(t, errors.Is(held, errImagePrepareHold), "held, not sent again: %v", held)
	assert.Equal(t, 30*time.Second, imageHoldRequeueAfter(held))
	assert.Contains(t, held.Error(), "image source rejected by provider")
	assert.Equal(t, 1, inst.prepareCalls)

	now = now.Add(30 * time.Second)
	assert.True(t, contracts.IsInvalidSpec(ensure()))
	assert.Equal(t, 2, inst.prepareCalls)
	now = now.Add(59 * time.Second)
	assert.True(t, errors.Is(ensure(), errImagePrepareHold), "the second wait is a minute")
	assert.Equal(t, 2, inst.prepareCalls)
	now = now.Add(time.Second)
	assert.True(t, contracts.IsInvalidSpec(ensure()), "sent again when the wait is over")
	assert.Equal(t, 3, inst.prepareCalls)

	// A corrected spec.source is a new digest: prepared at once.
	fixed := reloadImage(t, r, img.Name)
	fixed.Spec.Source.Libvirt.URL = "https://images.example.com/noble.qcow2"
	require.NoError(t, r.Update(context.Background(), fixed))
	assert.True(t, contracts.IsInvalidSpec(ensure()), "a new source is sent at once")
	assert.Equal(t, 4, inst.prepareCalls)
}
