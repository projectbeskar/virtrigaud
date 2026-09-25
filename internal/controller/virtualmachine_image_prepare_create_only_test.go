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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin WHEN a VM prepares its image: only right before a create
// (an unbound VM, or a re-create of a VM missing on its hypervisor), never for
// a VM that exists — so nothing about its image (a source the provider now
// refuses, a dropped or unverified entry, OnMissing Fail/Wait) can stop a
// running VM's describe and power. They also pin that concurrent prepares of
// one image through one Provider object share one call, that an entry with no
// Provider UID is never trusted under OnMissing Fail/Wait, that creates hold
// while the VMImage CRD cannot record the state, and that an older task's
// completion never clears a newer task.

// preparingRoutingProvider is a routingProvider that is also an
// ImagePreparer.
type preparingRoutingProvider struct {
	routingProvider

	mu          sync.Mutex
	prepareReqs []contracts.ImagePrepareRequest
	prepareResp contracts.ImagePrepareResponse
	prepareErr  error
}

func (p *preparingRoutingProvider) PrepareImage(_ context.Context, req contracts.ImagePrepareRequest) (contracts.ImagePrepareResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prepareReqs = append(p.prepareReqs, req)
	return p.prepareResp, p.prepareErr
}

func (p *preparingRoutingProvider) prepares() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.prepareReqs)
}

var _ contracts.ImagePreparer = (*preparingRoutingProvider)(nil)

// coProvider is the import-capable Provider "shared" in bpNS.
func coProvider() *infrav1beta1.Provider {
	p := withRuntime(singleProviderCR("shared", bpNS))
	p.UID = types.UID("uid-" + bpNS + "-shared")
	p.Status.ReportedCapabilities = &infrav1beta1.ReportedCapabilities{SupportsImageImport: true}
	return p
}

// coVM returns a VM in bpNS using coProvider, VMClass "shared" and img; bound
// (status.id set) when bound is true.
func coVM(img *infrav1beta1.VMImage, bound bool) *infrav1beta1.VirtualMachine {
	vm := cgVM("", "")
	vm.Spec.ImportedDisk = nil
	vm.Spec.ImageRef = &infrav1beta1.ObjectRef{Name: img.Name, Namespace: img.Namespace}
	if bound {
		p := coProvider()
		vm.Status.ID = "vm-100"
		vm.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: p.Namespace, Name: p.Name, UID: string(p.UID)}
	}
	return vm
}

// coReconciler builds a VM reconciler over a fake client (status subresource
// for VirtualMachine and VMImage) seeded with coProvider, the class, img and
// vm, resolving every Provider to prov.
func coReconciler(t *testing.T, prov contracts.Provider, img *infrav1beta1.VMImage, vm *infrav1beta1.VirtualMachine) *VirtualMachineReconciler {
	t.Helper()
	s := coverageTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(coProvider(), grantedClass(bpNS, "shared", nil), img, vm).
		WithStatusSubresource(&infrav1beta1.VirtualMachine{}, &infrav1beta1.VMImage{}).
		Build()
	return &VirtualMachineReconciler{
		Client: c, Scheme: s, RemoteResolver: &stubResolver{provider: prov}, Recorder: record.NewFakeRecorder(20),
	}
}

// legacyImage returns the shared OVA image with a bare-name "shared" entry
// (as an earlier release wrote it) and the given OnMissing action.
func legacyImage(onMissing infrav1beta1.ImageMissingAction) *infrav1beta1.VMImage {
	img := sharedOVAImage(idImageNS, "ubuntu", onMissing)
	img.Status = infrav1beta1.VMImageStatus{
		Ready:          true,
		Phase:          infrav1beta1.ImagePhaseReady,
		ProviderStatus: map[string]infrav1beta1.ProviderImageStatus{"shared": {Available: true, ID: "tmpl-legacy"}},
		AvailableOn:    []string{"shared"},
	}
	return img
}

func vmReady(t *testing.T, vm *infrav1beta1.VirtualMachine) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c)
	return c
}

func TestReconcileVM_BoundVMNeverPreparesItsImage(t *testing.T) {
	inUse := contracts.NewInvalidSpecError(
		`prepare: libvirt image "/var/lib/libvirt/images/ubuntu.qcow2" is in use as a VM disk`,
		errors.New("rpc error: code = InvalidArgument desc = ..."))
	unverified := func(onMissing infrav1beta1.ImageMissingAction) *infrav1beta1.VMImage {
		img := sharedOVAImage(idImageNS, "ubuntu", onMissing)
		img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
			bpNS + "/shared": {Available: true, ID: "tmpl-legacy"}, // migrated: no providerUID
		}
		return img
	}
	for name, tc := range map[string]struct {
		img        *infrav1beta1.VMImage
		prepareErr error
	}{
		"a legacy entry whose source the provider now refuses (libvirt image in use as a VM disk)": {legacyImage(""), inUse},
		"an unverified migrated entry the provider now refuses":                                    {unverified(""), inUse},
		"onMissing Fail, legacy entry that the migration would drop":                               {legacyImage(infrav1beta1.ImageMissingActionFail), nil},
		"onMissing Wait, legacy entry that the migration would drop":                               {legacyImage(infrav1beta1.ImageMissingActionWait), nil},
		"onMissing Wait, entry without a Provider UID":                                             {unverified(infrav1beta1.ImageMissingActionWait), nil},
		"no entry at all, template deleted out of band (a prepare would import)":                   {sharedOVAImage(idImageNS, "ubuntu", ""), nil},
	} {
		t.Run(name, func(t *testing.T) {
			prov := &preparingRoutingProvider{prepareErr: tc.prepareErr}
			prov.describeResp = contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}
			vm := coVM(tc.img, true)
			r := coReconciler(t, prov, tc.img, vm)
			before := getImage(t, r, tc.img)

			_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
			require.NoError(t, err)
			assert.Zero(t, prov.prepares(), "a VM that exists never prepares its image")
			require.Len(t, prov.describeRefs, 1, "describe runs")
			assert.Empty(t, prov.createReqs)
			after := getBPVM(t, r, vm.Name)
			assert.Equal(t, metav1.ConditionTrue, vmReady(t, after).Status, "the running VM stays Ready")
			assert.Equal(t, "vm-100", after.Status.ID)
			assert.Equal(t, before.Status, getImage(t, r, tc.img).Status, "the VMImage status is not touched")
		})
	}
}

func TestReconcileVM_UnboundVMPreparesThenCreates(t *testing.T) {
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	prov := &preparingRoutingProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu-on-a"}}
	vm := coVM(img, false)
	r := coReconciler(t, prov, img, vm)

	_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	assert.Equal(t, 1, prov.prepares())
	require.Len(t, prov.createReqs, 1)
	assert.Equal(t, "ubuntu-on-a", prov.createReqs[0].Image.TemplateName, "created from what this Provider prepared")
	assert.True(t, getImage(t, r, img).Status.ProviderStatus[bpNS+"/shared"].Available)
}

func TestReconcileVM_RecreateRevalidatesTheImageFirst(t *testing.T) {
	img := legacyImage("")
	prov := &preparingRoutingProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "tmpl-confirmed"}}
	prov.describeResp = contracts.DescribeResponse{Exists: false}
	vm := coVM(img, true)
	r := coReconciler(t, prov, img, vm)

	_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	assert.Equal(t, 1, prov.prepares(), "a re-create is a create: the migrated entry is re-validated first")
	require.Len(t, prov.createReqs, 1)
	assert.Equal(t, "tmpl-confirmed", prov.createReqs[0].Image.TemplateName)
}

func TestReconcileVM_WaitWithoutProviderUIDHoldsTheCreate(t *testing.T) {
	for _, onMissing := range []infrav1beta1.ImageMissingAction{infrav1beta1.ImageMissingActionFail, infrav1beta1.ImageMissingActionWait} {
		t.Run(string(onMissing), func(t *testing.T) {
			img := sharedOVAImage(idImageNS, "ubuntu", onMissing)
			img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
				bpNS + "/shared": {Available: true, ID: "tmpl-out-of-band"}, // no providerUID
			}
			prov := &preparingRoutingProvider{}
			vm := coVM(img, false)
			r := coReconciler(t, prov, img, vm)

			result, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
			require.NoError(t, err)
			assert.Equal(t, imagePrepareRequeueAfter, result.RequeueAfter)
			assert.Zero(t, prov.prepares())
			assert.Empty(t, prov.createReqs, "an entry without a Provider UID is never used")
			ready := vmReady(t, getBPVM(t, r, vm.Name))
			assert.Equal(t, k8s.ReasonWaitingForDependencies, ready.Reason)
			assert.Contains(t, ready.Message, "providerUID")

			got := getImage(t, r, img)
			cond := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMImageConditionReady)
			require.NotNil(t, cond)
			assert.Equal(t, imageReasonProviderUIDMissing, cond.Reason)
			assert.Contains(t, cond.Message, "set its providerUID")
			assert.Empty(t, got.Status.ProviderStatus[bpNS+"/shared"].ProviderUID, "nothing is adopted")
		})
	}
}

// crdReporter is a VMImageCRDFeatureReporter with a fixed answer.
type crdReporter bool

func (c crdReporter) VMImagePrepareStateMissing(context.Context) bool { return bool(c) }

func TestReconcileVM_HoldsCreatesWhileTheVMImageCRDIsOutdated(t *testing.T) {
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	prov := &preparingRoutingProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu-on-a"}}
	vm := coVM(img, false)
	r := coReconciler(t, prov, img, vm)
	r.ImageCRDFeatures = crdReporter(true)
	before := getImage(t, r, img)

	result, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	assert.Equal(t, imageCRDOutdatedRequeueAfter, result.RequeueAfter)
	assert.Zero(t, prov.prepares(), "no provider call")
	assert.Empty(t, prov.createReqs)
	assert.Equal(t, before.ResourceVersion, getImage(t, r, img).ResourceVersion, "no VMImage status write")
	assert.Contains(t, vmReady(t, getBPVM(t, r, vm.Name)).Message, "upgrade the CRDs")

	// A reference-style source needs no prepare, so it is not held.
	r.ImageCRDFeatures = crdReporter(true)
	tmpl := sharedOVAImage(idImageNS, "ubuntu", "")
	tmpl.Spec.Source.VSphere = &infrav1beta1.VSphereImageSource{TemplateName: "ubuntu-tmpl"}
	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, tmpl, coProvider(), prov)
	require.NoError(t, err)
	assert.False(t, requeue)

	// Once the CRD is upgraded the create proceeds.
	r.ImageCRDFeatures = crdReporter(false)
	_, err = r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	assert.Equal(t, 1, prov.prepares())
	assert.Len(t, prov.createReqs, 1)
}

func TestVMCRDFeatureChecker_VMImagePrepareStateMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		reader *stubCRDReader
		want   bool
	}{
		"current CRDs": {&stubCRDReader{crd: generatedVMCRD(t), t: t}, false},
		"VMImage CRD without providerUID": {&stubCRDReader{crd: generatedVMCRD(t), t: t,
			others: map[string]*unstructured.Unstructured{VMImageCRDName: olderVMImageCRD(t, "providerUID")}}, true},
		"VMImage CRD without taskRef": {&stubCRDReader{crd: generatedVMCRD(t), t: t,
			others: map[string]*unstructured.Unstructured{VMImageCRDName: olderVMImageCRD(t, "taskRef")}}, true},
		"only another CRD is outdated": {&stubCRDReader{crd: generatedVMCRD(t), t: t,
			others: map[string]*unstructured.Unstructured{ProviderCRDName: olderConsumerCRD(t, ProviderCRDName)}}, false},
		"CRDs cannot be read (unknown)": {&stubCRDReader{crd: generatedVMCRD(t), t: t,
			otherErr: forbiddenCRDRead()}, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, NewVMCRDFeatureChecker(tc.reader).VMImagePrepareStateMissing(context.Background()))
		})
	}
}

// blockingPreparer is an ImagePreparer whose PrepareImage blocks until
// release is closed, counting calls.
type blockingPreparer struct {
	stubProvider
	calls   atomic.Int32
	started chan struct{}
	once    sync.Once
	release chan struct{}
	resp    contracts.ImagePrepareResponse
}

func (b *blockingPreparer) PrepareImage(_ context.Context, _ contracts.ImagePrepareRequest) (contracts.ImagePrepareResponse, error) {
	b.calls.Add(1)
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.resp, nil
}

// arrivalReporter is a VMImageCRDFeatureReporter (CRD current) that marks
// each reconcile reaching the point right before its prepare.
type arrivalReporter struct{ arrived *sync.WaitGroup }

func (a arrivalReporter) VMImagePrepareStateMissing(context.Context) bool {
	a.arrived.Done()
	return false
}

func TestEnsureImageOnProvider_ConcurrentPreparesShareOneCall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resp     contracts.ImagePrepareResponse
		inFlight bool
	}{
		{"synchronous prepare", contracts.ImagePrepareResponse{PreparedImageID: "ubuntu-on-a"}, false},
		{"asynchronous prepare", contracts.ImagePrepareResponse{TaskRef: "task-a", PreparedImageID: "ubuntu-on-a"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := sharedOVAImage(idImageNS, "ubuntu", "")
			provA := identityProvider(idTeamA, idProviderName)
			var statusWrites atomic.Int32
			r, _ := newIdentityReconcilerWithFuncs(t, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if _, ok := obj.(*infrav1beta1.VMImage); ok {
						statusWrites.Add(1)
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}, img, provA)
			const reconciles = 6
			var arrived sync.WaitGroup
			arrived.Add(reconciles)
			r.ImageCRDFeatures = arrivalReporter{arrived: &arrived}
			inst := &blockingPreparer{started: make(chan struct{}), release: make(chan struct{}), resp: tc.resp}
			stale := getImage(t, r, img)

			var wg sync.WaitGroup
			copies := make([]*infrav1beta1.VMImage, reconciles)
			requeues := make([]bool, reconciles)
			errs := make([]error, reconciles)
			for i := 0; i < reconciles; i++ {
				copies[i] = stale.DeepCopy() // each reconcile has its own, equally stale, copy
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					requeues[i], errs[i] = r.EnsureImageOnProvider(context.Background(), vmUsing(provA, img), copies[i], provA, inst)
				}(i)
			}
			// One reconcile is inside PrepareImage and every reconcile has
			// reached its prepare: release the provider only now, so they all
			// overlap with the call in flight.
			<-inst.started
			arrived.Wait()
			close(inst.release)
			wg.Wait()

			key := imageProviderKey(provA)
			for i := 0; i < reconciles; i++ {
				require.NoError(t, errs[i])
				assert.Equal(t, tc.inFlight, requeues[i], "reconcile %d", i)
				assert.Equal(t, "ubuntu-on-a", copies[i].Status.ProviderStatus[key].ID,
					"reconcile %d sees the recorded outcome", i)
			}
			// Whether a reconcile joined the call in flight or arrived after it,
			// the outcome was recorded once, inside the shared call: exactly one
			// provider call and one status write, so the reconciles never race
			// each other to write it.
			assert.EqualValues(t, 1, inst.calls.Load(), "one PrepareImage for all concurrent reconciles")
			assert.EqualValues(t, 1, statusWrites.Load(), "one VMImage status write for all concurrent reconciles")
			got := getImage(t, r, img).Status.ProviderStatus[key]
			assert.Equal(t, !tc.inFlight, got.Available)
			assert.Equal(t, tc.resp.TaskRef, got.TaskRef)
		})
	}
}

// TestWriteImageStatus_ContendedWritersAllLand runs many writers of distinct
// providerStatus entries of one VMImage at once: every write lands (the
// conflict retry re-reads and re-applies each mutation, so none is lost) and
// none fails on an exhausted retry.
func TestWriteImageStatus_ContendedWritersAllLand(t *testing.T) {
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	r, _ := newIdentityReconciler(t, img)
	const writers = 16
	var start, wg sync.WaitGroup
	start.Add(1)
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			p := identityProvider(fmt.Sprintf("team-%02d", i), idProviderName)
			_, errs[i] = r.markImagePrepared(context.Background(), getImage(t, r, img), p,
				&preparedLocation{id: fmt.Sprintf("tmpl-%02d", i)}, "")
		}(i)
	}
	start.Done()
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "writer %d", i)
	}
	got := getImage(t, r, img)
	require.Len(t, got.Status.ProviderStatus, writers, "no update is lost")
	for i := 0; i < writers; i++ {
		key := fmt.Sprintf("team-%02d/%s", i, idProviderName)
		assert.Equal(t, fmt.Sprintf("tmpl-%02d", i), got.Status.ProviderStatus[key].ID)
		assert.Contains(t, got.Status.AvailableOn, key)
	}
}

func TestEnsureImageOnProvider_StaleReadDoesNotPrepareAgain(t *testing.T) {
	ctx := context.Background()
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	provA := identityProvider(idTeamA, idProviderName)
	r, _ := newIdentityReconciler(t, img, provA)
	inst := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu-on-a"}}
	stale := getImage(t, r, img) // read before the prepare below

	_, err := r.EnsureImageOnProvider(ctx, vmUsing(provA, img), getImage(t, r, img), provA, inst)
	require.NoError(t, err)
	require.Equal(t, 1, inst.calls())

	requeue, err := r.EnsureImageOnProvider(ctx, vmUsing(provA, img), stale, provA, inst)
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, 1, inst.calls(), "the re-read finds the recorded prepare: no second call")
	assert.Equal(t, "ubuntu-on-a", stale.Status.ProviderStatus[imageProviderKey(provA)].ID,
		"the caller's copy is refreshed so the create consumes the prepared location")

	// An asynchronous prepare recorded meanwhile is polled, not issued again.
	img2 := sharedOVAImage(idImageNS, "noble", "")
	r2, _ := newIdentityReconciler(t, img2, provA)
	async := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{TaskRef: "task-1"}}
	stale2 := getImage(t, r2, img2)
	_, err = r2.EnsureImageOnProvider(ctx, vmUsing(provA, img2), getImage(t, r2, img2), provA, async)
	require.NoError(t, err)
	requeue, err = r2.EnsureImageOnProvider(ctx, vmUsing(provA, img2), stale2, provA, async)
	require.NoError(t, err)
	assert.True(t, requeue)
	assert.Equal(t, 1, async.calls())
}

func TestMarkImagePrepared_DoesNotClearANewerTask(t *testing.T) {
	ctx := context.Background()
	provA := identityProvider(idTeamA, idProviderName)
	key := imageProviderKey(provA)
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
		key: {ProviderUID: string(provA.UID), TaskRef: "task-new"},
	}
	r, _ := newIdentityReconciler(t, img, provA)
	current := getImage(t, r, img)

	applied, err := r.markImagePrepared(ctx, current, provA, nil, "task-old")
	require.NoError(t, err)
	assert.False(t, applied)
	got := getImage(t, r, img)
	assert.Equal(t, "task-new", got.Status.ProviderStatus[key].TaskRef, "the newer task is kept")
	assert.False(t, got.Status.ProviderStatus[key].Available)
	assert.Equal(t, current.ResourceVersion, got.ResourceVersion, "nothing is written")

	applied, err = r.markImagePrepared(ctx, current, provA, nil, "task-new")
	require.NoError(t, err)
	assert.True(t, applied)
	got = getImage(t, r, img)
	assert.True(t, got.Status.ProviderStatus[key].Available)
	assert.Empty(t, got.Status.ProviderStatus[key].TaskRef)
}

// forbiddenCRDRead is the error an RBAC-restricted CRD read returns.
func forbiddenCRDRead() error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
		VMImageCRDName, errors.New("no"))
}
