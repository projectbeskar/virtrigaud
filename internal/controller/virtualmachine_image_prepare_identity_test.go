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
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that VMImage prepare state is per Provider IDENTITY
// (<namespace>/<name>, plus the Provider UID it was recorded through): two
// same-named Providers in different namespaces consuming one shared VMImage
// get independent entries and independent prepares, each prepare task is
// polled only through its own Provider, bare-name entries from an earlier
// release are migrated (or dropped) and never satisfy another namespace's
// Provider, and a re-created Provider re-validates what its predecessor
// prepared.

const (
	// idImageNS is the namespace of the shared VMImage.
	idImageNS = "virtrigaud-system"
	// idTeamA and idTeamB each have a Provider named idProviderName.
	idTeamA = "team-a"
	idTeamB = "team-b"
	// idProviderName is the name both teams gave their Provider.
	idProviderName = "vsphere"
)

// identityProvider returns an import-capable Provider ns/name with the UID
// uid-<ns>-<name>.
func identityProvider(ns, name string) *infrav1beta1.Provider {
	p := importCapableProvider(name)
	p.Namespace = ns
	p.UID = types.UID("uid-" + ns + "-" + name)
	return p
}

// sharedOVAImage returns an OVA-sourced (import-style) VMImage in ns, shared
// with every namespace, with the given OnMissing action.
func sharedOVAImage(ns, name string, onMissing infrav1beta1.ImageMissingAction) *infrav1beta1.VMImage {
	img := &infrav1beta1.VMImage{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infrav1beta1.VMImageSpec{
			Source: infrav1beta1.ImageSource{
				VSphere: &infrav1beta1.VSphereImageSource{OVAURL: "https://images.example.com/ubuntu.ova"},
			},
			ConsumerNamespaceSelector: &metav1.LabelSelector{},
		},
	}
	if onMissing != "" {
		img.Spec.Prepare = &infrav1beta1.ImagePrepare{OnMissing: onMissing}
	}
	return img
}

// vmUsing returns a VM in provider's namespace that uses provider and img.
func vmUsing(provider *infrav1beta1.Provider, img *infrav1beta1.VMImage) *infrav1beta1.VirtualMachine {
	return &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: provider.Namespace},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: provider.Name},
			ClassRef:    infrav1beta1.ObjectRef{Name: "small"},
			ImageRef:    &infrav1beta1.ObjectRef{Name: img.Name, Namespace: img.Namespace},
		},
	}
}

// newIdentityReconciler builds a VirtualMachineReconciler on a fake client
// holding img (with the status subresource) and objs, with a fake recorder.
func newIdentityReconciler(t *testing.T, img *infrav1beta1.VMImage, objs ...client.Object) (*VirtualMachineReconciler, *record.FakeRecorder) {
	t.Helper()
	return newIdentityReconcilerWithFuncs(t, interceptor.Funcs{}, img, objs...)
}

// newIdentityReconcilerWithFuncs is newIdentityReconciler with client
// interceptors.
func newIdentityReconcilerWithFuncs(t *testing.T, funcs interceptor.Funcs, img *infrav1beta1.VMImage, objs ...client.Object) (*VirtualMachineReconciler, *record.FakeRecorder) {
	t.Helper()
	scheme := coverageTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append([]client.Object{img}, objs...)...).
		WithStatusSubresource(img).
		WithInterceptorFuncs(funcs).
		Build()
	rec := record.NewFakeRecorder(10)
	return &VirtualMachineReconciler{Client: c, Scheme: scheme, Recorder: rec}, rec
}

// getImage re-reads img from r's client.
func getImage(t *testing.T, r *VirtualMachineReconciler, img *infrav1beta1.VMImage) *infrav1beta1.VMImage {
	t.Helper()
	got := &infrav1beta1.VMImage{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(img), got))
	return got
}

// pollRecorder records the task refs a preparerProvider was asked to poll and
// answers from done.
type pollRecorder struct {
	mu     sync.Mutex
	polled []string
	done   map[string]bool
}

func (p *pollRecorder) isTaskComplete(_ context.Context, ref string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.polled = append(p.polled, ref)
	return p.done[ref], nil
}

func (p *pollRecorder) setDone(ref string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done[ref] = true
}

func (p *pollRecorder) refs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.polled...)
}

func newPollRecorder() *pollRecorder { return &pollRecorder{done: map[string]bool{}} }

// createImageName returns the template name a create request for vm on
// provider would use.
func createImageName(t *testing.T, r *VirtualMachineReconciler, vm *infrav1beta1.VirtualMachine,
	provider *infrav1beta1.Provider, img *infrav1beta1.VMImage) contracts.VMImage {
	t.Helper()
	req, err := r.buildCreateRequest(context.Background(), vm, provider, smallVMClass(vm.Namespace), img, nil)
	require.NoError(t, err)
	return req.Image
}

func TestEnsureImageOnProvider_SameNamedProvidersInTwoNamespaces(t *testing.T) {
	ctx := context.Background()
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	provA := identityProvider(idTeamA, idProviderName)
	provB := identityProvider(idTeamB, idProviderName)
	r, _ := newIdentityReconciler(t, img, provA, provB)
	instA := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu-on-a"}}
	instB := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu-on-b"}}
	vmA, vmB := vmUsing(provA, img), vmUsing(provB, img)

	// team-b prepares first, through its own Provider.
	requeue, err := r.EnsureImageOnProvider(ctx, vmB, getImage(t, r, img), provB, instB)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, 1, instB.calls())

	// team-a's Provider has the same name, but team-b's entry does not satisfy
	// it: team-a prepares through its own Provider.
	current := getImage(t, r, img)
	requeue, err = r.EnsureImageOnProvider(ctx, vmA, current, provA, instA)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, 1, instA.calls(), "team-a must not skip its own prepare because team-b's same-named Provider prepared")
	assert.Equal(t, 1, instB.calls(), "team-a's prepare goes only to team-a's Provider")

	got := getImage(t, r, img)
	keyA, keyB := idTeamA+"/"+idProviderName, idTeamB+"/"+idProviderName
	require.Len(t, got.Status.ProviderStatus, 2)
	assert.Equal(t, "ubuntu-on-a", got.Status.ProviderStatus[keyA].ID)
	assert.Equal(t, string(provA.UID), got.Status.ProviderStatus[keyA].ProviderUID)
	assert.Equal(t, "ubuntu-on-b", got.Status.ProviderStatus[keyB].ID)
	assert.Equal(t, string(provB.UID), got.Status.ProviderStatus[keyB].ProviderUID)
	assert.ElementsMatch(t, []string{keyA, keyB}, got.Status.AvailableOn)
	assert.NotContains(t, got.Status.ProviderStatus, idProviderName, "no bare-name entry is written")

	// Steady state: each is idempotent through its own entry only.
	for _, tc := range []struct {
		vm   *infrav1beta1.VirtualMachine
		prov *infrav1beta1.Provider
		inst *preparerProvider
	}{{vmA, provA, instA}, {vmB, provB, instB}} {
		requeue, err = r.EnsureImageOnProvider(ctx, tc.vm, getImage(t, r, img), tc.prov, tc.inst)
		require.NoError(t, err)
		assert.False(t, requeue)
		assert.Equal(t, 1, tc.inst.calls())
	}

	// Each VM is created from the template its own Provider prepared.
	assert.Equal(t, "ubuntu-on-a", createImageName(t, r, vmA, provA, got).TemplateName)
	assert.Equal(t, "ubuntu-on-b", createImageName(t, r, vmB, provB, got).TemplateName)
}

func TestEnsureImageOnProvider_PrepareTasksArePolledPerProvider(t *testing.T) {
	ctx := context.Background()
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	provA := identityProvider(idTeamA, idProviderName)
	provB := identityProvider(idTeamB, idProviderName)
	r, _ := newIdentityReconciler(t, img, provA, provB)
	pollA, pollB := newPollRecorder(), newPollRecorder()
	instA := &preparerProvider{
		prepareResp:      contracts.ImagePrepareResponse{TaskRef: "task-a", PreparedImageID: "ubuntu-on-a"},
		isTaskCompleteFn: pollA.isTaskComplete,
	}
	instB := &preparerProvider{
		prepareResp:      contracts.ImagePrepareResponse{TaskRef: "task-b", PreparedImageID: "ubuntu-on-b"},
		isTaskCompleteFn: pollB.isTaskComplete,
	}
	vmA, vmB := vmUsing(provA, img), vmUsing(provB, img)
	keyA, keyB := imageProviderKey(provA), imageProviderKey(provB)
	ensure := func(vm *infrav1beta1.VirtualMachine, prov *infrav1beta1.Provider, inst *preparerProvider) bool {
		t.Helper()
		requeue, err := r.EnsureImageOnProvider(ctx, vm, getImage(t, r, img), prov, inst)
		require.NoError(t, err)
		return requeue
	}

	// Both start an asynchronous prepare; each task is recorded in its own entry.
	assert.True(t, ensure(vmB, provB, instB))
	assert.True(t, ensure(vmA, provA, instA))
	got := getImage(t, r, img)
	assert.Equal(t, "task-a", got.Status.ProviderStatus[keyA].TaskRef)
	assert.Equal(t, "task-b", got.Status.ProviderStatus[keyB].TaskRef)
	assert.Empty(t, got.Status.PrepareTaskRef)

	// team-b's task completes. Only team-b's Provider is asked about it.
	pollB.setDone("task-b")
	assert.False(t, ensure(vmB, provB, instB))
	assert.True(t, ensure(vmA, provA, instA), "team-a's prepare is still in flight: team-b's completion says nothing about it")

	got = getImage(t, r, img)
	assert.True(t, got.Status.ProviderStatus[keyB].Available)
	assert.Empty(t, got.Status.ProviderStatus[keyB].TaskRef)
	assert.False(t, got.Status.ProviderStatus[keyA].Available)
	assert.Equal(t, "task-a", got.Status.ProviderStatus[keyA].TaskRef)
	// Ready is the OR across providers: team-a's in-flight prepare does not hide
	// that the image is ready on team-b's Provider.
	assert.True(t, got.Status.Ready)
	assert.Equal(t, infrav1beta1.ImagePhaseReady, got.Status.Phase)
	importing := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMImageConditionImporting)
	require.NotNil(t, importing)
	assert.Equal(t, metav1.ConditionTrue, importing.Status, "team-a's import is still running")

	// team-a's VM is not created from team-b's template while it waits.
	assert.Equal(t, "https://images.example.com/ubuntu.ova", createImageName(t, r, vmA, provA, got).URL)

	pollA.setDone("task-a")
	assert.False(t, ensure(vmA, provA, instA))
	got = getImage(t, r, img)
	assert.True(t, got.Status.ProviderStatus[keyA].Available)
	assert.ElementsMatch(t, []string{keyA, keyB}, got.Status.AvailableOn)
	importing = meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMImageConditionImporting)
	require.NotNil(t, importing)
	assert.Equal(t, metav1.ConditionFalse, importing.Status)

	for _, ref := range pollA.refs() {
		assert.Equal(t, "task-a", ref, "team-a's Provider is only ever asked about team-a's task")
	}
	for _, ref := range pollB.refs() {
		assert.Equal(t, "task-b", ref, "team-b's Provider is only ever asked about team-b's task")
	}
	assert.Equal(t, 1, instA.calls())
	assert.Equal(t, 1, instB.calls())
}

func TestHasLegacyImagePrepareState(t *testing.T) {
	for name, tc := range map[string]struct {
		status infrav1beta1.VMImageStatus
		want   bool
	}{
		"empty": {infrav1beta1.VMImageStatus{}, false},
		"identity keys only": {infrav1beta1.VMImageStatus{
			ProviderStatus: map[string]infrav1beta1.ProviderImageStatus{"ns/p": {Available: true}},
			AvailableOn:    []string{"ns/p"},
		}, false},
		"bare providerStatus key": {infrav1beta1.VMImageStatus{
			ProviderStatus: map[string]infrav1beta1.ProviderImageStatus{"p": {}},
		}, true},
		"bare availableOn element": {infrav1beta1.VMImageStatus{AvailableOn: []string{"p"}}, true},
		"image-wide task ref":      {infrav1beta1.VMImageStatus{PrepareTaskRef: "task-1"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasLegacyImagePrepareState(&tc.status))
		})
	}
}

func TestMigrateLegacyImageStatus(t *testing.T) {
	legacyEntry := infrav1beta1.ProviderImageStatus{Available: true, ID: "tmpl-old", Path: "/old"}

	t.Run("an owned bare name is re-keyed without a UID or task; an unowned one is dropped", func(t *testing.T) {
		img := &infrav1beta1.VMImage{
			ObjectMeta: metav1.ObjectMeta{Name: "ubuntu", Namespace: "default"},
			Status: infrav1beta1.VMImageStatus{
				Ready:          true,
				Phase:          infrav1beta1.ImagePhaseReady,
				PrepareTaskRef: "task-legacy",
				ProviderStatus: map[string]infrav1beta1.ProviderImageStatus{
					"libvirt-1": legacyEntry,
					"gone":      {Available: true, ID: "tmpl-gone"},
				},
				AvailableOn: []string{"libvirt-1", "gone"},
			},
		}
		migrated, dropped := migrateLegacyImageStatus(img, map[string]string{"libvirt-1": "default/libvirt-1"})
		assert.Equal(t, []string{"libvirt-1"}, migrated)
		assert.Equal(t, []string{"gone"}, dropped)

		require.Len(t, img.Status.ProviderStatus, 1)
		ps := img.Status.ProviderStatus["default/libvirt-1"]
		assert.True(t, ps.Available)
		assert.Equal(t, "tmpl-old", ps.ID, "the recorded location is kept for re-validation")
		assert.Empty(t, ps.ProviderUID, "the writer is unknown: the entry is re-validated before use")
		assert.Empty(t, ps.TaskRef)
		assert.Equal(t, []string{"default/libvirt-1"}, img.Status.AvailableOn)
		assert.Empty(t, img.Status.PrepareTaskRef, "the image-wide task ref is cleared, never attributed")
		assert.True(t, img.Status.Ready, "still available on a migrated entry")
	})

	t.Run("an identity entry wins over a bare one", func(t *testing.T) {
		current := infrav1beta1.ProviderImageStatus{Available: true, ProviderUID: "uid-1", ID: "tmpl-new"}
		img := &infrav1beta1.VMImage{Status: infrav1beta1.VMImageStatus{
			ProviderStatus: map[string]infrav1beta1.ProviderImageStatus{
				"libvirt-1":         legacyEntry,
				"default/libvirt-1": current,
			},
			AvailableOn: []string{"default/libvirt-1", "libvirt-1"},
		}}
		migrateLegacyImageStatus(img, map[string]string{"libvirt-1": "default/libvirt-1"})
		assert.Equal(t, map[string]infrav1beta1.ProviderImageStatus{"default/libvirt-1": current}, img.Status.ProviderStatus)
		assert.Equal(t, []string{"default/libvirt-1"}, img.Status.AvailableOn)
	})

	t.Run("dropping every available entry clears Ready", func(t *testing.T) {
		img := &infrav1beta1.VMImage{
			ObjectMeta: metav1.ObjectMeta{Generation: 3},
			Status: infrav1beta1.VMImageStatus{
				Ready:          true,
				Phase:          infrav1beta1.ImagePhaseReady,
				ProviderStatus: map[string]infrav1beta1.ProviderImageStatus{"vsphere": legacyEntry},
				AvailableOn:    []string{"vsphere"},
			},
		}
		_, dropped := migrateLegacyImageStatus(img, map[string]string{})
		assert.Equal(t, []string{"vsphere"}, dropped)
		assert.Empty(t, img.Status.ProviderStatus)
		assert.Empty(t, img.Status.AvailableOn)
		assert.False(t, img.Status.Ready)
		assert.Equal(t, infrav1beta1.ImagePhasePending, img.Status.Phase)
		ready := meta.FindStatusCondition(img.Status.Conditions, infrav1beta1.VMImageConditionReady)
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, imageReasonPrepareStateDropped, ready.Reason)
		assert.EqualValues(t, 3, ready.ObservedGeneration)
	})
}

func TestEnsureImageOnProvider_MigratesLegacyBareNameEntries(t *testing.T) {
	ctx := context.Background()
	legacyStatus := func() infrav1beta1.VMImageStatus {
		return infrav1beta1.VMImageStatus{
			Ready:          true,
			Phase:          infrav1beta1.ImagePhaseReady,
			PrepareTaskRef: "task-legacy",
			ProviderStatus: map[string]infrav1beta1.ProviderImageStatus{
				idProviderName: {Available: true, ID: "tmpl-legacy"},
			},
			AvailableOn: []string{idProviderName},
		}
	}

	t.Run("the Provider exists in the image's namespace: re-keyed, then re-validated through it", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "ubuntu", "")
		img.Status = legacyStatus()
		owner := identityProvider(idImageNS, idProviderName)
		r, _ := newIdentityReconciler(t, img, owner)
		poll := newPollRecorder()
		inst := &preparerProvider{
			prepareResp:      contracts.ImagePrepareResponse{PreparedImageID: "tmpl-confirmed"},
			isTaskCompleteFn: poll.isTaskComplete,
		}

		requeue, err := r.EnsureImageOnProvider(ctx, vmUsing(owner, img), getImage(t, r, img), owner, inst)
		require.NoError(t, err)
		assert.False(t, requeue)
		assert.Equal(t, 1, inst.calls(), "a migrated entry has no Provider UID: it is re-validated by the idempotent prepare")
		assert.Empty(t, poll.refs(), "the image-wide legacy task ref is never polled")

		got := getImage(t, r, img)
		key := imageProviderKey(owner)
		assert.NotContains(t, got.Status.ProviderStatus, idProviderName)
		require.Contains(t, got.Status.ProviderStatus, key)
		assert.True(t, got.Status.ProviderStatus[key].Available)
		assert.Equal(t, string(owner.UID), got.Status.ProviderStatus[key].ProviderUID)
		assert.Equal(t, "tmpl-confirmed", got.Status.ProviderStatus[key].ID)
		assert.Equal(t, []string{key}, got.Status.AvailableOn)
		assert.Empty(t, got.Status.PrepareTaskRef)
	})

	t.Run("no such Provider in the image's namespace: dropped, never used by another namespace's same-named Provider", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "ubuntu", "")
		img.Status = legacyStatus()
		provA := identityProvider(idTeamA, idProviderName)
		r, _ := newIdentityReconciler(t, img, provA)
		inst := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu-on-a"}}
		vmA := vmUsing(provA, img)

		requeue, err := r.EnsureImageOnProvider(ctx, vmA, getImage(t, r, img), provA, inst)
		require.NoError(t, err)
		assert.False(t, requeue)
		assert.Equal(t, 1, inst.calls(), "team-a prepares through its own Provider instead of using the bare-name entry")

		got := getImage(t, r, img)
		assert.Equal(t, []string{imageProviderKey(provA)}, mapKeys(got.Status.ProviderStatus))
		assert.Equal(t, "ubuntu-on-a", createImageName(t, r, vmA, provA, got).TemplateName)
	})

	t.Run("a bare-name entry is ignored even before it is migrated", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "ubuntu", "")
		img.Status = legacyStatus()
		provA := identityProvider(idTeamA, idProviderName)
		r, _ := newIdentityReconciler(t, img)
		assert.Equal(t, "https://images.example.com/ubuntu.ova", createImageName(t, r, vmUsing(provA, img), provA, img).URL)
	})

	t.Run("a Provider lookup error aborts the migration without a status write", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "ubuntu", "")
		img.Status = legacyStatus()
		provA := identityProvider(idTeamA, idProviderName)
		boom := errors.New("apiserver unavailable")
		r, _ := newIdentityReconcilerWithFuncs(t, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*infrav1beta1.Provider); ok {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}, img, provA)
		inst := &preparerProvider{}

		_, err := r.EnsureImageOnProvider(ctx, vmUsing(provA, img), getImage(t, r, img), provA, inst)
		require.ErrorIs(t, err, boom)
		assert.Zero(t, inst.calls())
		assert.Equal(t, legacyStatus().ProviderStatus, getImage(t, r, img).Status.ProviderStatus)
	})
}

func TestEnsureImageOnProvider_RecreatedProvider(t *testing.T) {
	ctx := context.Background()
	// The Provider was deleted and re-created under the same namespace/name:
	// same key, new UID.
	recreated := identityProvider(idTeamA, idProviderName)
	key := imageProviderKey(recreated)
	oldEntry := infrav1beta1.ProviderImageStatus{Available: true, ProviderUID: "uid-before", ID: "tmpl-old"}

	t.Run("onMissing Import re-validates through the new Provider object", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "ubuntu", "")
		img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{key: oldEntry}
		img.Status.AvailableOn = []string{key}
		img.Status.Ready = true
		r, _ := newIdentityReconciler(t, img, recreated)
		inst := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "tmpl-new"}}
		vm := vmUsing(recreated, img)

		// Before re-validation, the old object's entry is not consumed.
		assert.Equal(t, "https://images.example.com/ubuntu.ova", createImageName(t, r, vm, recreated, img).URL)

		requeue, err := r.EnsureImageOnProvider(ctx, vm, getImage(t, r, img), recreated, inst)
		require.NoError(t, err)
		assert.False(t, requeue)
		assert.Equal(t, 1, inst.calls())
		got := getImage(t, r, img)
		assert.Equal(t, string(recreated.UID), got.Status.ProviderStatus[key].ProviderUID)
		assert.Equal(t, "tmpl-new", got.Status.ProviderStatus[key].ID, "the location the new Provider reported replaces the old one")
		assert.Equal(t, "tmpl-new", createImageName(t, r, vm, recreated, got).TemplateName)
	})

	t.Run("a task recorded through the old object is never polled; the prepare is issued again", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "ubuntu", "")
		img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
			key: {ProviderUID: "uid-before", TaskRef: "task-old"},
		}
		r, _ := newIdentityReconciler(t, img, recreated)
		poll := newPollRecorder()
		poll.setDone("task-old")
		inst := &preparerProvider{
			prepareResp:      contracts.ImagePrepareResponse{TaskRef: "task-new"},
			isTaskCompleteFn: poll.isTaskComplete,
		}

		requeue, err := r.EnsureImageOnProvider(ctx, vmUsing(recreated, img), getImage(t, r, img), recreated, inst)
		require.NoError(t, err)
		assert.True(t, requeue)
		assert.Empty(t, poll.refs(), "the old object's task must not mark the image prepared")
		assert.Equal(t, 1, inst.calls())
		got := getImage(t, r, img)
		assert.False(t, got.Status.ProviderStatus[key].Available)
		assert.Equal(t, "task-new", got.Status.ProviderStatus[key].TaskRef)
		assert.Equal(t, string(recreated.UID), got.Status.ProviderStatus[key].ProviderUID)
	})

	for _, onMissing := range []infrav1beta1.ImageMissingAction{infrav1beta1.ImageMissingActionFail, infrav1beta1.ImageMissingActionWait} {
		t.Run("onMissing "+string(onMissing)+" accepts the available entry with a warning", func(t *testing.T) {
			img := sharedOVAImage(idImageNS, "ubuntu", onMissing)
			img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{key: oldEntry}
			img.Status.Ready = true
			r, rec := newIdentityReconciler(t, img, recreated)
			inst := &preparerProvider{}
			vm := vmUsing(recreated, img)

			current := getImage(t, r, img)
			requeue, err := r.EnsureImageOnProvider(ctx, vm, current, recreated, inst)
			require.NoError(t, err, "not held: VMs already running from the image keep reconciling")
			assert.False(t, requeue)
			assert.Zero(t, inst.calls(), "onMissing forbids the prepare")
			got := getImage(t, r, img)
			assert.Equal(t, string(recreated.UID), got.Status.ProviderStatus[key].ProviderUID)
			assert.Equal(t, "tmpl-old", createImageName(t, r, vm, recreated, current).TemplateName)
			require.Len(t, rec.Events, 1)
			ev := <-rec.Events
			assert.True(t, strings.HasPrefix(ev, "Warning "+eventReasonImagePrepareStateAccepted), ev)
		})
	}

	t.Run("onMissing Fail still holds when the old object's entry is not available", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "ubuntu", infrav1beta1.ImageMissingActionFail)
		img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
			key: {ProviderUID: "uid-before", Message: "image source rejected"},
		}
		r, _ := newIdentityReconciler(t, img, recreated)
		inst := &preparerProvider{}

		_, err := r.EnsureImageOnProvider(ctx, vmUsing(recreated, img), getImage(t, r, img), recreated, inst)
		assert.ErrorIs(t, err, errImagePrepareHold)
		assert.Zero(t, inst.calls())
	})
}

// mapKeys returns the keys of m, sorted.
func mapKeys(m map[string]infrav1beta1.ProviderImageStatus) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
