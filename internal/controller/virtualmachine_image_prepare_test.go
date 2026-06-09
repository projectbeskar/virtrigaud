/*
Copyright 2025.

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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// preparerProvider is a contracts.Provider that ALSO implements
// contracts.ImagePreparer. It records every PrepareImage call and returns a
// configurable response/error, and lets a test override IsTaskComplete. It
// embeds stubProvider (defined in virtualmachine_controller_test.go) so it
// satisfies the full contracts.Provider interface with no extra boilerplate.
type preparerProvider struct {
	stubProvider

	mu               sync.Mutex
	prepareCalls     int
	lastPrepareReq   contracts.ImagePrepareRequest
	prepareResp      contracts.ImagePrepareResponse
	prepareErr       error
	isTaskCompleteFn func(ctx context.Context, ref string) (bool, error)
}

// PrepareImage makes preparerProvider a contracts.ImagePreparer.
func (p *preparerProvider) PrepareImage(_ context.Context, req contracts.ImagePrepareRequest) (contracts.ImagePrepareResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prepareCalls++
	p.lastPrepareReq = req
	return p.prepareResp, p.prepareErr
}

// IsTaskComplete overrides stubProvider's (which always returns true) when a
// test supplies isTaskCompleteFn.
func (p *preparerProvider) IsTaskComplete(ctx context.Context, ref string) (bool, error) {
	if p.isTaskCompleteFn != nil {
		return p.isTaskCompleteFn(ctx, ref)
	}
	return p.stubProvider.IsTaskComplete(ctx, ref)
}

func (p *preparerProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prepareCalls
}

// Compile-time assertions that the test fakes satisfy the relevant interfaces.
var (
	_ contracts.Provider      = (*preparerProvider)(nil)
	_ contracts.ImagePreparer = (*preparerProvider)(nil)
)

// importCapableProvider returns a Provider CR that advertises SupportsImageImport.
func importCapableProvider(name string) *infrav1beta1.Provider {
	return &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: infrav1beta1.ProviderStatus{
			ReportedCapabilities: &infrav1beta1.ReportedCapabilities{SupportsImageImport: true},
		},
	}
}

// imageWithSource returns a VMImage with a libvirt URL source and the given
// OnMissing action ("" leaves Prepare unset → defaults to Import).
func imageWithSource(name string, onMissing infrav1beta1.ImageMissingAction) *infrav1beta1.VMImage {
	img := &infrav1beta1.VMImage{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: infrav1beta1.VMImageSpec{
			Source: infrav1beta1.ImageSource{
				Libvirt: &infrav1beta1.LibvirtImageSource{
					URL:    "https://images.example.com/jammy.qcow2",
					Format: infrav1beta1.ImageFormatQCOW2,
				},
			},
		},
	}
	if onMissing != "" {
		img.Spec.Prepare = &infrav1beta1.ImagePrepare{OnMissing: onMissing}
	}
	return img
}

// vmForImage returns a VirtualMachine that references imageName on providerName.
func vmForImage(providerName, imageName string) *infrav1beta1.VirtualMachine {
	return &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-1", Namespace: "default"},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: providerName},
			ClassRef:    infrav1beta1.ObjectRef{Name: "small"},
			ImageRef:    &infrav1beta1.ObjectRef{Name: imageName},
		},
	}
}

// newEnsureReconciler builds a VirtualMachineReconciler backed by a fake client
// seeded with the given image (with status subresource enabled).
func newEnsureReconciler(t *testing.T, img *infrav1beta1.VMImage) (*VirtualMachineReconciler, *fake.ClientBuilder) {
	t.Helper()
	scheme := capGatingScheme(t)
	b := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(img).
		WithStatusSubresource(img)
	c := b.Build()
	return &VirtualMachineReconciler{Client: c, Scheme: scheme}, b
}

// reloadImage re-reads the VMImage from the fake client.
func reloadImage(t *testing.T, r *VirtualMachineReconciler, name string) *infrav1beta1.VMImage {
	t.Helper()
	got := &infrav1beta1.VMImage{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got))
	return got
}

// (a) Synchronous prepare: empty TaskRef → ProviderStatus[p].Available + AvailableOn
// + Ready, and create proceeds (requeue=false, err=nil).
func TestEnsureImageOnProvider_SyncPrepare(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{TaskRef: ""}}
	vm := vmForImage(provider.Name, img.Name)

	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, inst)
	require.NoError(t, err)
	assert.False(t, requeue, "synchronous prepare must let create proceed immediately")
	assert.Equal(t, 1, inst.calls())

	// The provider received the JSON-encoded spec and the image name as target.
	assert.Equal(t, "ubuntu", inst.lastPrepareReq.TargetName)
	assert.Contains(t, inst.lastPrepareReq.ImageJSON, `"source"`)
	assert.Empty(t, inst.lastPrepareReq.StorageHint)

	persisted := reloadImage(t, r, img.Name)
	require.Contains(t, persisted.Status.ProviderStatus, provider.Name)
	assert.True(t, persisted.Status.ProviderStatus[provider.Name].Available)
	assert.Contains(t, persisted.Status.AvailableOn, provider.Name)
	assert.True(t, persisted.Status.Ready)
	assert.Equal(t, infrav1beta1.ImagePhaseReady, persisted.Status.Phase)
	assert.Empty(t, persisted.Status.PrepareTaskRef)
}

// (b) Asynchronous prepare: TaskRef set → PrepareTaskRef + Phase=Importing +
// requeue; then on a second call with IsTaskComplete=true → stamped + create
// proceeds.
func TestEnsureImageOnProvider_AsyncPrepareThenComplete(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("vsphere-1")
	vm := vmForImage(provider.Name, img.Name)

	// First pass: provider returns an async task and reports it incomplete.
	taskDone := false
	inst := &preparerProvider{
		prepareResp:      contracts.ImagePrepareResponse{TaskRef: "task-abc"},
		isTaskCompleteFn: func(_ context.Context, _ string) (bool, error) { return taskDone, nil },
	}

	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, inst)
	require.NoError(t, err)
	assert.True(t, requeue, "async prepare in flight must requeue, not create")
	assert.Equal(t, 1, inst.calls())

	persisted := reloadImage(t, r, img.Name)
	assert.Equal(t, "task-abc", persisted.Status.PrepareTaskRef)
	assert.Equal(t, infrav1beta1.ImagePhaseImporting, persisted.Status.Phase)
	assert.False(t, persisted.Status.Ready)

	// Second pass while task still running → still requeue, no new PrepareImage.
	requeue, err = r.EnsureImageOnProvider(context.Background(), vm, persisted, provider, inst)
	require.NoError(t, err)
	assert.True(t, requeue)
	assert.Equal(t, 1, inst.calls(), "must NOT re-trigger prepare while polling")

	// Task completes → stamped, create proceeds, still no new PrepareImage.
	taskDone = true
	persisted = reloadImage(t, r, img.Name)
	requeue, err = r.EnsureImageOnProvider(context.Background(), vm, persisted, provider, inst)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, 1, inst.calls())

	final := reloadImage(t, r, img.Name)
	assert.True(t, final.Status.ProviderStatus[provider.Name].Available)
	assert.Contains(t, final.Status.AvailableOn, provider.Name)
	assert.Empty(t, final.Status.PrepareTaskRef)
	assert.True(t, final.Status.Ready)
	assert.Equal(t, infrav1beta1.ImagePhaseReady, final.Status.Phase)
}

// (c) Idempotency: an image already Available on the provider → no PrepareImage
// call, create proceeds.
func TestEnsureImageOnProvider_IdempotentAlreadyAvailable(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	provider := importCapableProvider("libvirt-1")
	img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
		provider.Name: {Available: true},
	}
	img.Status.AvailableOn = []string{provider.Name}
	img.Status.Ready = true
	r, _ := newEnsureReconciler(t, img)
	inst := &preparerProvider{}
	vm := vmForImage(provider.Name, img.Name)

	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, inst)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, 0, inst.calls(), "already-available image must not be re-prepared")
}

// (d) No regression: a provider that does NOT implement ImagePreparer → no
// prepare, create proceeds unchanged.
func TestEnsureImageOnProvider_NotAnImagePreparer(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1") // advertises the flag...
	vm := vmForImage(provider.Name, img.Name)

	// ...but the instance is a plain stubProvider (NOT an ImagePreparer).
	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, &stubProvider{})
	require.NoError(t, err)
	assert.False(t, requeue)

	persisted := reloadImage(t, r, img.Name)
	assert.Empty(t, persisted.Status.ProviderStatus, "no status write when not a preparer")
	assert.Empty(t, persisted.Status.AvailableOn)
}

// (d') No regression: provider implements ImagePreparer but the CR does NOT
// advertise SupportsImageImport → no prepare, create proceeds unchanged.
func TestEnsureImageOnProvider_CapabilityFlagFalse(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	provider := &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "libvirt-1", Namespace: "default"},
		// No ReportedCapabilities → SupportsImageImport reads false.
	}
	inst := &preparerProvider{}
	vm := vmForImage(provider.Name, img.Name)

	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, inst)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, 0, inst.calls(), "must not prepare when capability flag is false")
}

// Skip path: ImportedDisk VM (ImageRef nil) / nil image → no prepare.
func TestEnsureImageOnProvider_SkipsWhenNoImage(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{}

	// ImageRef nil (ImportedDisk path).
	vm := vmForImage(provider.Name, img.Name)
	vm.Spec.ImageRef = nil
	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, inst)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, 0, inst.calls())

	// vmImage nil.
	vm2 := vmForImage(provider.Name, img.Name)
	requeue, err = r.EnsureImageOnProvider(context.Background(), vm2, nil, provider, inst)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, 0, inst.calls())
}

// (e) OnMissing=Fail → no prepare, errImagePrepareHold sentinel, Ready=False
// condition + Phase=Failed recorded on the VMImage.
func TestEnsureImageOnProvider_OnMissingFail(t *testing.T) {
	img := imageWithSource("ubuntu", infrav1beta1.ImageMissingActionFail)
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{}
	vm := vmForImage(provider.Name, img.Name)

	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, inst)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errImagePrepareHold), "must return the hold sentinel")
	assert.False(t, requeue)
	assert.Equal(t, 0, inst.calls(), "OnMissing=Fail must not prepare")

	persisted := reloadImage(t, r, img.Name)
	assert.False(t, persisted.Status.Ready)
	assert.Equal(t, infrav1beta1.ImagePhaseFailed, persisted.Status.Phase)
	ready := readyCondition(persisted.Status.Conditions, infrav1beta1.VMImageConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, imageReasonMissingOnProvider, ready.Reason)
}

// (e') OnMissing=Wait → no prepare, hold sentinel, Phase=Pending, Ready=False.
func TestEnsureImageOnProvider_OnMissingWait(t *testing.T) {
	img := imageWithSource("ubuntu", infrav1beta1.ImageMissingActionWait)
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{}
	vm := vmForImage(provider.Name, img.Name)

	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, inst)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errImagePrepareHold))
	assert.False(t, requeue)
	assert.Equal(t, 0, inst.calls())

	persisted := reloadImage(t, r, img.Name)
	assert.False(t, persisted.Status.Ready)
	assert.Equal(t, infrav1beta1.ImagePhasePending, persisted.Status.Phase)
}

// (f) Two providers preparing the SAME image concurrently must not clobber each
// other's ProviderStatus entry — the per-provider entries and AvailableOn both
// survive because writeImageStatus re-GETs under RetryOnConflict. This exercises
// the single-writer-per-(image,provider), conflict-safe merge path.
func TestEnsureImageOnProvider_ConcurrentProvidersNoClobber(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)

	pA := importCapableProvider("libvirt-a")
	pB := importCapableProvider("libvirt-b")
	instA := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{TaskRef: ""}}
	instB := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{TaskRef: ""}}
	vmA := vmForImage(pA.Name, img.Name)
	vmB := vmForImage(pB.Name, img.Name)

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make([]error, 2)
	go func() {
		defer wg.Done()
		// Each goroutine uses its own in-memory image copy, as separate reconciles would.
		imgCopy := img.DeepCopy()
		_, errs[0] = r.EnsureImageOnProvider(context.Background(), vmA, imgCopy, pA, instA)
	}()
	go func() {
		defer wg.Done()
		imgCopy := img.DeepCopy()
		_, errs[1] = r.EnsureImageOnProvider(context.Background(), vmB, imgCopy, pB, instB)
	}()
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	final := reloadImage(t, r, img.Name)
	require.Contains(t, final.Status.ProviderStatus, pA.Name)
	require.Contains(t, final.Status.ProviderStatus, pB.Name)
	assert.True(t, final.Status.ProviderStatus[pA.Name].Available)
	assert.True(t, final.Status.ProviderStatus[pB.Name].Available)
	assert.Contains(t, final.Status.AvailableOn, pA.Name)
	assert.Contains(t, final.Status.AvailableOn, pB.Name)
}

// Provider PrepareImage error surfaces as a real error (not the hold sentinel).
func TestEnsureImageOnProvider_PrepareError(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{prepareErr: errors.New("download failed")}
	vm := vmForImage(provider.Name, img.Name)

	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, inst)
	require.Error(t, err)
	assert.False(t, errors.Is(err, errImagePrepareHold))
	assert.False(t, requeue)
}
