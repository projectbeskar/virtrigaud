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
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/mock"
	transportgrpc "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the manager side of ADR-0009 (prepared-image artifact
// identity, Slice 2): the D7 gate order (fall through, hold, call with the
// identity and an empty target name), the stamp echo check, the D8 source
// digest recorded and required wherever an entry is consumed, the
// SourceDigestMissing / ProviderLacksArtifactIdentity / ArtifactConflict
// holds with their reasons, event and metric (D11), and the asynchronous
// prepare confirmed by a second call. The mock provider is driven over gRPC
// for the end-to-end cases.

// artifactOutcomeFamily is the D11 manager counter.
const artifactOutcomeFamily = "virtrigaud_image_prepare_artifact_total"

// artifactOutcome reads virtrigaud_image_prepare_artifact_total for
// providerType and outcome.
func artifactOutcome(t *testing.T, providerType, outcome string) float64 {
	t.Helper()
	return counterSample(t, artifactOutcomeFamily, map[string]string{"provider_type": providerType, "outcome": outcome})
}

// typedProvider is importCapableProvider(name) with provider type typ (the
// metric label) and the given capabilities.
func typedProvider(name, typ string, imageImport, artifactIdentity bool) *infrav1beta1.Provider {
	p := importCapableProvider(name)
	p.Spec.Type = infrav1beta1.ProviderType(typ)
	p.Status.ReportedCapabilities = &infrav1beta1.ReportedCapabilities{
		SupportsImageImport:           imageImport,
		SupportsImageArtifactIdentity: artifactIdentity,
	}
	return p
}

// recordedEvent is one event an objectRecorder received.
type recordedEvent struct {
	kind, namespace, name, eventType, reason, message string
}

// objectRecorder is a record.EventRecorder that keeps the object each event
// is recorded on.
type objectRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

// Event records one event.
func (o *objectRecorder) Event(object runtime.Object, eventType, reason, message string) {
	ev := recordedEvent{eventType: eventType, reason: reason, message: message}
	switch obj := object.(type) {
	case *infrav1beta1.VMImage:
		ev.kind, ev.namespace, ev.name = "VMImage", obj.Namespace, obj.Name
	case *infrav1beta1.VirtualMachine:
		ev.kind, ev.namespace, ev.name = "VirtualMachine", obj.Namespace, obj.Name
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, ev)
}

// Eventf records one event.
func (o *objectRecorder) Eventf(object runtime.Object, eventType, reason, messageFmt string, args ...any) {
	o.Event(object, eventType, reason, fmt.Sprintf(messageFmt, args...))
}

// AnnotatedEventf records one event (annotations are dropped).
func (o *objectRecorder) AnnotatedEventf(object runtime.Object, _ map[string]string, eventType, reason, messageFmt string, args ...any) {
	o.Event(object, eventType, reason, fmt.Sprintf(messageFmt, args...))
}

// withReason returns the events with reason.
func (o *objectRecorder) withReason(reason string) []recordedEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []recordedEvent
	for _, ev := range o.events {
		if ev.reason == reason {
			out = append(out, ev)
		}
	}
	return out
}

// startMockImageProvider serves a mock provider whose imports take delay (0:
// synchronous) on a loopback gRPC server and returns it with a manager-side
// client connected to it. Both are stopped when the test ends.
func startMockImageProvider(t *testing.T, delay time.Duration) (*mock.Provider, *transportgrpc.Client) {
	t.Helper()
	t.Setenv("MOCK_FAILURE_MODE", "")
	t.Setenv("MOCK_SLOW_MODE", "")
	prov, cli, stop, err := serveMockImageProvider(delay)
	require.NoError(t, err)
	t.Cleanup(stop)
	return prov, cli
}

// serveMockImageProvider serves a mock provider whose imports take delay on
// a loopback gRPC server and returns it, a manager-side client connected to
// it, and the function that stops both.
func serveMockImageProvider(delay time.Duration) (*mock.Provider, *transportgrpc.Client, func(), error) {
	prov := mock.NewProvider(mock.WithImagePrepareDelay(delay), mock.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listen: %w", err)
	}
	srv := grpc.NewServer()
	providerv1.RegisterProviderServer(srv, prov)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(lis)
	}()
	stopServer := func() {
		srv.Stop()
		<-served
	}
	cli, err := transportgrpc.NewClient(context.Background(), lis.Addr().String(), "mock", "mock", nil, nil)
	if err != nil {
		stopServer()
		return nil, nil, nil, fmt.Errorf("connect to the mock provider: %w", err)
	}
	return prov, cli, func() {
		_ = cli.Close()
		stopServer()
	}, nil
}

// mockArtifactName is the artifact name the mock derives for img's current
// source.
func mockArtifactName(t *testing.T, img *infrav1beta1.VMImage) string {
	t.Helper()
	name, err := imageartifact.ArtifactName(imageartifact.NameRuleLibvirt,
		contracts.ObjectIdentity{UID: string(img.UID), Namespace: img.Namespace, Name: img.Name}, digestOf(t, img))
	require.NoError(t, err)
	return name
}

// ─── D7 gate order ───────────────────────────────────────────────────────────

func TestEnsureImageOnProvider_ArtifactIdentityGateOrder(t *testing.T) {
	ctx := context.Background()

	t.Run("step 1: not an ImagePreparer falls through unchanged", func(t *testing.T) {
		img := imageWithSource("ubuntu", "")
		r, _ := newEnsureReconciler(t, img)
		provider := typedProvider("libvirt-1", "gate-test", true, true)
		before := reloadImage(t, r, img.Name)

		requeue, err := r.EnsureImageOnProvider(ctx, vmForImage(provider.Name, img.Name), img, provider, &stubProvider{})
		require.NoError(t, err)
		assert.False(t, requeue)
		assert.Equal(t, before.ResourceVersion, reloadImage(t, r, img.Name).ResourceVersion, "no VMImage status write")
	})

	t.Run("step 1: no image import support falls through unchanged, whatever else is advertised", func(t *testing.T) {
		img := imageWithSource("ubuntu", "")
		r, _ := newEnsureReconciler(t, img)
		provider := typedProvider("libvirt-clustered", "gate-test", false, true)
		inst := &preparerProvider{}
		before := reloadImage(t, r, img.Name)

		requeue, err := r.EnsureImageOnProvider(ctx, vmForImage(provider.Name, img.Name), img, provider, inst)
		require.NoError(t, err, "e.g. clustered libvirt: the by-reference create runs as before")
		assert.False(t, requeue)
		assert.Zero(t, inst.calls())
		assert.Equal(t, before.ResourceVersion, reloadImage(t, r, img.Name).ResourceVersion, "no VMImage status write")
	})

	t.Run("step 2: import without artifact identity holds, and nothing is sent", func(t *testing.T) {
		img := imageWithSource("ubuntu", "")
		r, _ := newEnsureReconciler(t, img)
		provider := typedProvider("libvirt-1", "gate-test", true, false)
		inst := &preparerProvider{}
		vm := vmForImage(provider.Name, img.Name)

		requeue, err := r.EnsureImageOnProvider(ctx, vm, img, provider, inst)
		require.ErrorIs(t, err, errImagePrepareHold)
		assert.False(t, requeue)
		assert.Zero(t, inst.calls(), "no RPC to a provider without artifact identity")
		assert.Equal(t, imageArtifactIdentityHoldRequeueAfter, imageHoldRequeueAfter(err))
		assert.Contains(t, err.Error(), "upgrade the provider")

		got := reloadImage(t, r, img.Name)
		ps := got.Status.ProviderStatus[imageProviderKey(provider)]
		assert.False(t, ps.Available)
		assert.Empty(t, ps.SourceDigest, "nothing was prepared")
		assert.Contains(t, ps.Message, "supportsImageArtifactIdentity")
		ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMImageConditionReady)
		require.NotNil(t, ready)
		assert.Equal(t, imageReasonProviderLacksArtifactIdentity, ready.Reason)
		assert.Equal(t, infrav1beta1.ImagePhasePending, got.Status.Phase)

		// Retried on every requeue, the hold does not rewrite the VMImage.
		_, err = r.EnsureImageOnProvider(ctx, vm, got, provider, inst)
		require.ErrorIs(t, err, errImagePrepareHold)
		assert.Equal(t, got.ResourceVersion, reloadImage(t, r, img.Name).ResourceVersion)
	})

	t.Run("step 3: import with artifact identity sends the identity, the digest and an empty target name", func(t *testing.T) {
		img := imageWithSource("ubuntu", "")
		r, _ := newEnsureReconciler(t, img)
		provider := typedProvider("libvirt-1", "gate-test", true, true)
		inst := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "default.ubuntu_0123456789abcdef"}}

		requeue, err := r.EnsureImageOnProvider(ctx, vmForImage(provider.Name, img.Name), img, provider, inst)
		require.NoError(t, err)
		assert.False(t, requeue)
		require.Equal(t, 1, inst.calls())
		req := inst.lastPrepareReq
		assert.Empty(t, req.TargetName, "an old provider refuses an empty target name instead of importing under a bare name")
		assert.Equal(t, contracts.ObjectIdentity{UID: string(img.UID), Namespace: img.Namespace, Name: img.Name}, req.Image)
		assert.Equal(t, digestOf(t, img), req.SourceDigest)
		assert.Equal(t, contracts.ObjectIdentity{UID: string(provider.UID), Namespace: provider.Namespace, Name: provider.Name}, req.Provider)
	})
}

func TestEnsureImageOnProvider_LacksArtifactIdentityHoldsOnlyWhatWouldBeSent(t *testing.T) {
	ctx := context.Background()
	provider := typedProvider("vsphere-1", "gate-test", true, false)

	t.Run("a reference-style source is not held", func(t *testing.T) {
		img := imageWithSource("tmpl", "")
		img.Spec.Source = infrav1beta1.ImageSource{VSphere: &infrav1beta1.VSphereImageSource{TemplateName: "golden"}}
		r, _ := newEnsureReconciler(t, img)
		inst := &preparerProvider{}
		requeue, err := r.EnsureImageOnProvider(ctx, vmForImage(provider.Name, img.Name), img, provider, inst)
		require.NoError(t, err)
		assert.False(t, requeue)
		assert.Zero(t, inst.calls())
	})

	t.Run("onMissing Fail keeps its own reason", func(t *testing.T) {
		img := imageWithSource("ubuntu", infrav1beta1.ImageMissingActionFail)
		r, _ := newEnsureReconciler(t, img)
		inst := &preparerProvider{}
		_, err := r.EnsureImageOnProvider(ctx, vmForImage(provider.Name, img.Name), img, provider, inst)
		require.ErrorIs(t, err, errImagePrepareHold)
		ready := meta.FindStatusCondition(reloadImage(t, r, img.Name).Status.Conditions, infrav1beta1.VMImageConditionReady)
		require.NotNil(t, ready)
		assert.Equal(t, imageReasonMissingOnProvider, ready.Reason)
		assert.Zero(t, inst.calls())
	})

	t.Run("an entry already prepared for the current source is used as is", func(t *testing.T) {
		img := imageWithSource("ubuntu", "")
		img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
			imageProviderKey(provider): {Available: true, ProviderUID: string(provider.UID), SourceDigest: digestOf(t, img), ID: "x"},
		}
		r, _ := newEnsureReconciler(t, img)
		inst := &preparerProvider{}
		requeue, err := r.EnsureImageOnProvider(ctx, vmForImage(provider.Name, img.Name), img, provider, inst)
		require.NoError(t, err, "the artifact was verified when it was prepared")
		assert.False(t, requeue)
		assert.Zero(t, inst.calls())
	})
}

// ─── the stamp echo ──────────────────────────────────────────────────────────

func TestEnsureImageOnProvider_AnswerMustConfirmTheIdentity(t *testing.T) {
	ctx := context.Background()
	otherDigest := "sha256:" + strings.Repeat("3", 64)
	for name, tc := range map[string]struct {
		artifact *contracts.PreparedArtifact
		noEcho   bool
	}{
		"no artifact echo (an older provider, or legacy mode)": {noEcho: true},
		"an echo for another VMImage UID": {artifact: &contracts.PreparedArtifact{
			Name: "a", Image: contracts.ObjectIdentity{UID: "uid-someone-else", Namespace: "default", Name: "ubuntu"}}},
		"an echo for another source digest": {artifact: &contracts.PreparedArtifact{
			Name: "a", Image: contracts.ObjectIdentity{UID: string(testImageUID("default", "ubuntu"))}, SourceDigest: otherDigest}},
		"an echo without an artifact name": {artifact: &contracts.PreparedArtifact{
			Image: contracts.ObjectIdentity{UID: string(testImageUID("default", "ubuntu"))}}},
	} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s (async=%t)", name, async), func(t *testing.T) {
				img := imageWithSource("ubuntu", "")
				r, _ := newEnsureReconciler(t, img)
				provider := importCapableProvider("libvirt-1")
				resp := contracts.ImagePrepareResponse{PreparedImageID: "ubuntu", PreparedImagePath: "/pool/ubuntu.qcow2", Artifact: tc.artifact}
				if async {
					resp.TaskRef = "task-1"
				}
				if tc.artifact != nil && tc.artifact.SourceDigest == "" && tc.artifact.Name != "" {
					tc.artifact.SourceDigest = digestOf(t, img)
				}
				inst := &preparerProvider{prepareResp: resp, noEcho: tc.noEcho}

				requeue, err := r.EnsureImageOnProvider(ctx, vmForImage(provider.Name, img.Name), img, provider, inst)
				require.ErrorIs(t, err, errImageArtifactNotConfirmed)
				require.ErrorIs(t, err, errImagePrepareHold, "a hold with a backoff, not a 5-second provider error")
				assert.Equal(t, notConfirmedBackoff.Base, imageHoldRequeueAfter(err))
				assert.False(t, requeue)
				assert.Equal(t, 1, inst.calls())
				after := reloadImage(t, r, img.Name)
				ps := after.Status.ProviderStatus[imageProviderKey(provider)]
				assert.False(t, ps.Available, "an unconfirmed answer is never recorded as prepared")
				assert.Empty(t, ps.SourceDigest)
				assert.Empty(t, ps.ID, "nothing of the answer is recorded")
				assert.Empty(t, ps.TaskRef)
				assert.NotContains(t, ps.Message, "uid-someone-else", "no echoed data is shown")
				cond := meta.FindStatusCondition(after.Status.Conditions, infrav1beta1.VMImageConditionReady)
				require.NotNil(t, cond)
				assert.Equal(t, imageReasonArtifactNotConfirmed, cond.Reason)
				assert.Equal(t, "https://images.example.com/jammy.qcow2",
					createImageName(t, r, vmForImage(provider.Name, img.Name), provider, after).URL,
					"the create uses the source as written")
			})
		}
	}
}

func TestEnsureImageOnProvider_StaleCapabilityOldProviderRefusesTheEmptyTargetName(t *testing.T) {
	// A provider image rolled back under a Provider whose status still shows
	// the capability: it refuses an empty target name, as every pre-ADR-0009
	// provider does, and nothing is recorded as prepared.
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1")
	old := &oldStylePreparer{}

	_, err := r.EnsureImageOnProvider(context.Background(), vmForImage(provider.Name, img.Name), img, provider, old)
	require.Error(t, err)
	assert.True(t, contracts.IsInvalidSpec(err), "%v", err)
	assert.Equal(t, 1, old.calls)
	ps := reloadImage(t, r, img.Name).Status.ProviderStatus[imageProviderKey(provider)]
	assert.False(t, ps.Available, "never recorded as prepared")
	assert.Empty(t, ps.SourceDigest)
	assert.Contains(t, ps.Message, "target name")
}

// oldStylePreparer answers like a provider older than ADR-0009: it ignores
// the identity and refuses an empty target name.
type oldStylePreparer struct {
	stubProvider
	calls int
}

// PrepareImage implements contracts.ImagePreparer.
func (o *oldStylePreparer) PrepareImage(_ context.Context, req contracts.ImagePrepareRequest) (contracts.ImagePrepareResponse, error) {
	o.calls++
	if req.TargetName == "" {
		return contracts.ImagePrepareResponse{}, contracts.NewInvalidSpecError("image prepare: target name is required", nil)
	}
	return contracts.ImagePrepareResponse{PreparedImageID: req.TargetName}, nil
}

// ─── D8: the source digest ───────────────────────────────────────────────────

func TestEnsureImageOnProvider_EntryForAnotherSourceIsPreparedAgain(t *testing.T) {
	ctx := context.Background()
	otherDigest := "sha256:" + strings.Repeat("4", 64)
	for name, digest := range map[string]string{
		"a changed spec.source (another digest)":       otherDigest,
		"an entry from an earlier release (no digest)": "",
	} {
		t.Run(name, func(t *testing.T) {
			img := imageWithSource("ubuntu", "")
			provider := importCapableProvider("libvirt-1")
			key := imageProviderKey(provider)
			img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
				key: {Available: true, ProviderUID: string(provider.UID), SourceDigest: digest, ID: "old", Path: "/pool/old.qcow2"},
			}
			img.Status.AvailableOn = []string{key}
			r, _ := newEnsureReconciler(t, img)
			inst := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "new", PreparedImagePath: "/pool/new.qcow2"}}
			vm := vmForImage(provider.Name, img.Name)

			// Before: the old entry is not consumed.
			assert.Equal(t, "https://images.example.com/jammy.qcow2", createImageName(t, r, vm, provider, img).URL)

			requeue, err := r.EnsureImageOnProvider(ctx, vm, img, provider, inst)
			require.NoError(t, err)
			assert.False(t, requeue)
			assert.Equal(t, 1, inst.calls(), "prepared again for the current spec.source before the create")
			got := reloadImage(t, r, img.Name)
			assert.Equal(t, digestOf(t, img), got.Status.ProviderStatus[key].SourceDigest)
			assert.Equal(t, "/pool/new.qcow2", createImageName(t, r, vm, provider, got).Path)
		})
	}
}

func TestEnsureImageOnProvider_FailWaitWithoutSourceDigestHolds(t *testing.T) {
	ctx := context.Background()
	for _, onMissing := range []infrav1beta1.ImageMissingAction{infrav1beta1.ImageMissingActionFail, infrav1beta1.ImageMissingActionWait} {
		t.Run(string(onMissing), func(t *testing.T) {
			img := imageWithSource("ubuntu", onMissing)
			provider := importCapableProvider("libvirt-1")
			key := imageProviderKey(provider)
			entry := infrav1beta1.ProviderImageStatus{Available: true, ProviderUID: string(provider.UID), ID: "legacy", Path: "/pool/ubuntu.qcow2"}
			img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{key: entry}
			r, _ := newEnsureReconciler(t, img)
			inst := &preparerProvider{}

			requeue, err := r.EnsureImageOnProvider(ctx, vmForImage(provider.Name, img.Name), img, provider, inst)
			require.ErrorIs(t, err, errImagePrepareHold)
			assert.False(t, requeue)
			assert.Zero(t, inst.calls(), "onMissing forbids the prepare")

			got := reloadImage(t, r, img.Name)
			cond := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMImageConditionReady)
			require.NotNil(t, cond)
			assert.Equal(t, imageReasonSourceDigestMissing, cond.Reason)
			assert.Contains(t, cond.Message, "set spec.prepare.onMissing to Import")
			// VMImage status has a single writer (ADR-0005): the hold never
			// asks anyone to write it.
			assert.NotContains(t, cond.Message, "set its sourceDigest")
			assert.NotContains(t, cond.Message, "edit")
			assert.Equal(t, entry, got.Status.ProviderStatus[key], "the entry is neither adopted nor changed")
			assert.Equal(t, "/pool/ubuntu.qcow2", entry.Path)
			assert.Equal(t, "https://images.example.com/jammy.qcow2",
				createImageName(t, r, vmForImage(provider.Name, img.Name), provider, got).URL, "and never consumed")
		})
	}
}

func TestReconcileVM_EntryForAnotherSourceNeverTouchesARunningVM(t *testing.T) {
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
		bpNS + "/shared": {Available: true, ProviderUID: string(coProvider().UID), SourceDigest: "sha256:" + strings.Repeat("5", 64), ID: "old"},
	}
	prov := &preparingRoutingProvider{}
	prov.describeResp = contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}
	vm := coVM(img, true)
	r := coReconciler(t, prov, img, vm)
	before := getImage(t, r, img)

	_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
	require.NoError(t, err)
	assert.Zero(t, prov.prepares(), "a VM that exists never prepares its image")
	assert.Empty(t, prov.createReqs)
	assert.Equal(t, before.Status, getImage(t, r, img).Status)
}

func TestReconcileVM_SourceKindSwitchDoesNotReuseTheOldArtifact(t *testing.T) {
	ova := sharedOVAImage(idImageNS, "ubuntu", "")
	entry := infrav1beta1.ProviderImageStatus{
		Available: true, ProviderUID: string(coProvider().UID), SourceDigest: digestOf(t, ova), ID: "virtrigaud-system.ubuntu_0123456789abcdef",
	}

	t.Run("while the source is the OVA, the create clones its prepared artifact", func(t *testing.T) {
		img := ova.DeepCopy()
		img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{bpNS + "/shared": entry}
		prov := &preparingRoutingProvider{}
		vm := coVM(img, false)
		r := coReconciler(t, prov, img, vm)

		_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		assert.Zero(t, prov.prepares())
		require.Len(t, prov.createReqs, 1)
		assert.Equal(t, entry.ID, prov.createReqs[0].Image.TemplateName)
	})

	t.Run("switched to templateName, the create uses the template as written", func(t *testing.T) {
		img := ova.DeepCopy()
		img.Spec.Source = infrav1beta1.ImageSource{VSphere: &infrav1beta1.VSphereImageSource{TemplateName: "golden"}}
		img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{bpNS + "/shared": entry}
		prov := &preparingRoutingProvider{}
		vm := coVM(img, false)
		r := coReconciler(t, prov, img, vm)

		_, err := r.reconcileVM(context.Background(), getBPVM(t, r, vm.Name))
		require.NoError(t, err)
		assert.Zero(t, prov.prepares(), "a reference-style source never prepares")
		require.Len(t, prov.createReqs, 1)
		assert.Equal(t, "golden", prov.createReqs[0].Image.TemplateName, "the OVA's artifact is not reused")
	})
}

// ─── D4/D11: conflict and in progress ────────────────────────────────────────

func TestEnsureImageOnProvider_ArtifactConflictHolds(t *testing.T) {
	ctx := context.Background()
	const providerType = "artifact-conflict-test"
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	rec := &objectRecorder{}
	r.Recorder = rec
	provider := typedProvider("libvirt-1", providerType, true, true)
	refusal := contracts.NewConflictError(`image prepare: a prepared-image artifact named "default.ubuntu_0123456789abcdef" `+
		"exists at this Provider's image location but was not prepared for this VMImage; refusing to use or replace it", nil)
	inst := &preparerProvider{prepareErr: refusal}
	vm := vmForImage(provider.Name, img.Name)
	before := artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeConflict)

	for i := 0; i < 2; i++ {
		done, res, err := r.prepareImageForCreate(ctx, vm, vm.Status.DeepCopy(), reloadImage(t, r, img.Name), provider, inst)
		require.NoError(t, err)
		assert.True(t, done, "the create is held")
		assert.Equal(t, imageArtifactConflictRequeueAfter, res.RequeueAfter, "an operator must act: long requeue")
	}
	assert.Equal(t, 2, inst.calls(), "each retry asks the provider again")
	assert.Equal(t, before+2, artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeConflict))

	ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonWaitingForDependencies, ready.Reason)

	got := reloadImage(t, r, img.Name)
	ps := got.Status.ProviderStatus[imageProviderKey(provider)]
	assert.False(t, ps.Available)
	assert.Contains(t, ps.Message, "not prepared for this VMImage")
	cond := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMImageConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, imageReasonArtifactConflict, cond.Reason)
	assert.Equal(t, infrav1beta1.ImagePhaseFailed, got.Status.Phase)

	events := rec.withReason(eventReasonImageArtifactConflict)
	require.Len(t, events, 1, "one event per change of state, not per retry")
	assert.Equal(t, recordedEvent{kind: "VMImage", namespace: img.Namespace, name: img.Name, eventType: "Warning",
		reason: eventReasonImageArtifactConflict, message: ps.Message}, events[0])
}

func TestEnsureImageOnProvider_ArtifactConflictDoesNotMaskReadyElsewhere(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	img.Status = infrav1beta1.VMImageStatus{
		Ready: true,
		Phase: infrav1beta1.ImagePhaseReady,
		ProviderStatus: map[string]infrav1beta1.ProviderImageStatus{
			"default/libvirt-0": {Available: true, ProviderUID: "uid-default-libvirt-0", SourceDigest: digestOf(t, img)},
		},
		AvailableOn: []string{"default/libvirt-0"},
	}
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{prepareErr: contracts.NewConflictError("refusing to use or replace it", nil)}

	_, err := r.EnsureImageOnProvider(context.Background(), vmForImage(provider.Name, img.Name), img, provider, inst)
	require.ErrorIs(t, err, errImagePrepareHold)
	got := reloadImage(t, r, img.Name)
	assert.True(t, got.Status.Ready, "Ready is the OR across providers")
	assert.Contains(t, got.Status.ProviderStatus[imageProviderKey(provider)].Message, "not prepared for this VMImage")
}

// testClock is a settable clock for the reconciler's backoff and stall
// bounds.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestEnsureImageOnProvider_ArtifactInProgressWaitsThenReportsAStall(t *testing.T) {
	ctx := context.Background()
	const providerType = "artifact-inprogress-test"
	img := imageWithSource("ubuntu", "")
	img.Spec.Prepare = &infrav1beta1.ImagePrepare{Timeout: &metav1.Duration{Duration: 90 * time.Minute}}
	r, _ := newEnsureReconciler(t, img)
	clock := newTestClock()
	r.clock = clock.now
	rec := &objectRecorder{}
	r.Recorder = rec
	provider := typedProvider("libvirt-1", providerType, true, true)
	key := imageProviderKey(provider)
	inst := &preparerProvider{prepareErr: contracts.NewInProgressError("image prepare: still being prepared", nil)}
	before := artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeInProgress)
	vm := vmForImage(provider.Name, img.Name)
	ensure := func() error {
		t.Helper()
		requeue, err := r.EnsureImageOnProvider(ctx, vm, reloadImage(t, r, img.Name), provider, inst)
		require.ErrorIs(t, err, errImagePrepareHold)
		assert.False(t, requeue)
		assert.Equal(t, imageArtifactInProgressRequeueAfter, imageHoldRequeueAfter(err))
		return err
	}

	// The first answer records when the wait began; the image is importing.
	ensure()
	assert.Equal(t, before+1, artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeInProgress))
	got := reloadImage(t, r, img.Name)
	ps := got.Status.ProviderStatus[key]
	assert.False(t, ps.Available)
	assert.Contains(t, ps.Message, "by another request")
	require.NotNil(t, ps.LastUpdated)
	began := ps.LastUpdated.Time
	assert.Equal(t, infrav1beta1.ImagePhaseImporting, got.Status.Phase)

	// Waiting within the bound writes nothing.
	clock.advance(2 * time.Hour)
	ensure()
	assert.Equal(t, got.ResourceVersion, reloadImage(t, r, img.Name).ResourceVersion)

	// Beyond max(2 × 90m, 2h) = 3h the wait is reported as stalled, once.
	clock.advance(time.Hour + time.Second)
	err := ensure()
	assert.Contains(t, err.Error(), "stuck or abandoned")
	got = reloadImage(t, r, img.Name)
	ps = got.Status.ProviderStatus[key]
	assert.Contains(t, ps.Message, "more than 3h0m0s")
	assert.True(t, ps.LastUpdated.Time.Equal(began), "lastUpdated keeps when the wait began")
	cond := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMImageConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, imageReasonArtifactPrepareStalled, cond.Reason)
	clock.advance(time.Hour)
	ensure()
	assert.Equal(t, got.ResourceVersion, reloadImage(t, r, img.Name).ResourceVersion)
	require.Len(t, rec.withReason(eventReasonImageArtifactPrepareStalled), 1, "one event per change of state")
	assert.Equal(t, 4, inst.calls(), "each retry asks the provider")
}

// ─── against the mock provider over gRPC ─────────────────────────────────────

func TestEnsureImageOnProvider_MockProvider_IdentityEndToEnd(t *testing.T) {
	ctx := context.Background()
	mockProv, cli := startMockImageProvider(t, 0)
	const providerType = "artifact-mock-sync"

	t.Run("two namespaces' ubuntu images get distinct artifacts", func(t *testing.T) {
		names := map[string]bool{}
		for _, ns := range []string{idTeamA, idTeamB} {
			img := sharedOVAImage(ns, "ubuntu", "")
			provider := typedProvider("mock", providerType, true, true)
			provider.Namespace, provider.UID = ns, types.UID("uid-"+ns+"-mock")
			r, _ := newIdentityReconciler(t, img, provider)

			requeue, err := r.EnsureImageOnProvider(ctx, vmUsing(provider, img), getImage(t, r, img), provider, cli)
			require.NoError(t, err)
			assert.False(t, requeue)
			ps := getImage(t, r, img).Status.ProviderStatus[imageProviderKey(provider)]
			require.True(t, ps.Available)
			assert.Equal(t, mockArtifactName(t, img), ps.ID)
			assert.Equal(t, digestOf(t, img), ps.SourceDigest)
			names[ps.ID] = true
		}
		assert.Len(t, names, 2)
	})

	t.Run("a shared image through two Providers at one location: one artifact, reused", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "noble", "")
		provA := typedProvider("mock", providerType, true, true)
		provA.Namespace, provA.UID = idTeamA, "uid-team-a-mock-2"
		provB := typedProvider("mock", providerType, true, true)
		provB.Namespace, provB.UID = idTeamB, "uid-team-b-mock-2"
		r, _ := newIdentityReconciler(t, img, provA, provB)
		created := artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeCreated)
		reused := artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeReused)

		_, err := r.EnsureImageOnProvider(ctx, vmUsing(provA, img), getImage(t, r, img), provA, cli)
		require.NoError(t, err)
		_, err = r.EnsureImageOnProvider(ctx, vmUsing(provB, img), getImage(t, r, img), provB, cli)
		require.NoError(t, err)

		got := getImage(t, r, img)
		assert.Equal(t, mockArtifactName(t, img), got.Status.ProviderStatus[imageProviderKey(provA)].ID)
		assert.Equal(t, mockArtifactName(t, img), got.Status.ProviderStatus[imageProviderKey(provB)].ID)
		assert.Equal(t, created+1, artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeCreated))
		assert.Equal(t, reused+1, artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeReused))
	})

	t.Run("a foreign artifact at the derived name is a Conflict hold, never adopted", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "planted", "")
		provider := typedProvider("mock", providerType, true, true)
		provider.Namespace, provider.UID = idTeamA, "uid-team-a-mock-3"
		r, _ := newIdentityReconciler(t, img, provider)
		rec := &objectRecorder{}
		r.Recorder = rec
		mockProv.PlantImageArtifact(mockArtifactName(t, img), nil)

		_, err := r.EnsureImageOnProvider(ctx, vmUsing(provider, img), getImage(t, r, img), provider, cli)
		require.ErrorIs(t, err, errImagePrepareHold)
		assert.Equal(t, imageArtifactConflictRequeueAfter, imageHoldRequeueAfter(err))
		ps := getImage(t, r, img).Status.ProviderStatus[imageProviderKey(provider)]
		assert.False(t, ps.Available)
		assert.Empty(t, ps.ID, "the planted artifact is never recorded")
		assert.Len(t, rec.withReason(eventReasonImageArtifactConflict), 1)
	})
}

func TestEnsureImageOnProvider_MockProvider_AsyncPrepareIsConfirmed(t *testing.T) {
	ctx := context.Background()
	mockProv, cli := startMockImageProvider(t, time.Hour)
	const providerType = "artifact-mock-async"
	provider := typedProvider("mock", providerType, true, true)
	provider.Namespace, provider.UID = idTeamA, "uid-team-a-mock-async"
	key := imageProviderKey(provider)

	t.Run("a completed task is confirmed by a second call before the create", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "ubuntu", "")
		r, _ := newIdentityReconciler(t, img, provider)
		vm := vmUsing(provider, img)
		created := artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeCreated)
		reused := artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeReused)

		requeue, err := r.EnsureImageOnProvider(ctx, vm, getImage(t, r, img), provider, cli)
		require.NoError(t, err)
		assert.True(t, requeue)
		ps := getImage(t, r, img).Status.ProviderStatus[key]
		require.NotEmpty(t, ps.TaskRef)
		assert.False(t, ps.Available)
		assert.Equal(t, digestOf(t, img), ps.SourceDigest, "the task is recorded for the source it prepares")
		task := ps.TaskRef

		requeue, err = r.EnsureImageOnProvider(ctx, vm, getImage(t, r, img), provider, cli)
		require.NoError(t, err)
		assert.True(t, requeue, "still importing")
		assert.Equal(t, task, getImage(t, r, img).Status.ProviderStatus[key].TaskRef)

		require.True(t, mockProv.FinishTask(task, ""))
		requeue, err = r.EnsureImageOnProvider(ctx, vm, getImage(t, r, img), provider, cli)
		require.NoError(t, err)
		assert.False(t, requeue)
		ps = getImage(t, r, img).Status.ProviderStatus[key]
		assert.True(t, ps.Available)
		assert.Empty(t, ps.TaskRef)
		assert.Equal(t, mockArtifactName(t, img), ps.ID)
		assert.Equal(t, mockArtifactName(t, img), createImageName(t, r, vm, provider, getImage(t, r, img)).TemplateName)
		assert.Equal(t, created+1, artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeCreated))
		assert.Equal(t, reused, artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeReused),
			"the confirmation of our own import is not counted as a reuse")
	})

	t.Run("another Provider at the same location waits for the import in progress", func(t *testing.T) {
		img := sharedOVAImage(idImageNS, "jammy", "")
		other := typedProvider("mock", providerType, true, true)
		other.Namespace, other.UID = idTeamB, "uid-team-b-mock-async"
		r, _ := newIdentityReconciler(t, img, provider, other)
		inProgress := artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeInProgress)

		requeue, err := r.EnsureImageOnProvider(ctx, vmUsing(provider, img), getImage(t, r, img), provider, cli)
		require.NoError(t, err)
		require.True(t, requeue)

		_, err = r.EnsureImageOnProvider(ctx, vmUsing(other, img), getImage(t, r, img), other, cli)
		require.ErrorIs(t, err, errImagePrepareHold, "the provider's IMAGE_ARTIFACT_IN_PROGRESS is a hold, not a failure")
		assert.Equal(t, imageArtifactInProgressRequeueAfter, imageHoldRequeueAfter(err))
		assert.Equal(t, inProgress+1, artifactOutcome(t, providerType, metrics.ImageArtifactOutcomeInProgress))
		waiting := getImage(t, r, img).Status.ProviderStatus[imageProviderKey(other)]
		assert.False(t, waiting.Available)
		assert.Empty(t, waiting.SourceDigest, "nothing is recorded as prepared")
		assert.Contains(t, waiting.Message, "by another request")
	})
}

// countingPreparer is the manager's client to a mock provider, counting the
// PrepareImage calls it sends.
type countingPreparer struct {
	*transportgrpc.Client
	prepares atomic.Int32
}

// PrepareImage counts the call and forwards it.
func (c *countingPreparer) PrepareImage(ctx context.Context, req contracts.ImagePrepareRequest) (contracts.ImagePrepareResponse, error) {
	c.prepares.Add(1)
	return c.Client.PrepareImage(ctx, req)
}

func TestEnsureImageOnProvider_MockProvider_AlwaysFailingImportBacksOff(t *testing.T) {
	ctx := context.Background()
	mockProv, cli := startMockImageProvider(t, time.Hour)
	inst := &countingPreparer{Client: cli}
	provider := typedProvider("mock", "artifact-mock-failing", true, true)
	provider.Namespace, provider.UID = idTeamA, "uid-team-a-mock-failing"
	key := imageProviderKey(provider)
	img := sharedOVAImage(idImageNS, "broken", "")
	r, _ := newIdentityReconciler(t, img, provider)
	clock := newTestClock()
	r.clock = clock.now
	vm := vmUsing(provider, img)
	failTask := func() {
		t.Helper()
		task := getImage(t, r, img).Status.ProviderStatus[key].TaskRef
		require.NotEmpty(t, task)
		require.True(t, mockProv.FinishTask(task, "download failed: HTTP 404 from https://user:s3cret@images.example.com/broken.ova"))
	}
	holds := func(r *VirtualMachineReconciler, prepares int32) error {
		t.Helper()
		requeue, err := r.EnsureImageOnProvider(ctx, vm, getImage(t, r, img), provider, inst)
		require.ErrorIs(t, err, errImagePrepareHold)
		assert.False(t, requeue)
		assert.Equal(t, prepares, inst.prepares.Load(), "no prepare is sent while backing off")
		return err
	}
	sends := func(prepares int32) {
		t.Helper()
		requeue, err := r.EnsureImageOnProvider(ctx, vm, getImage(t, r, img), provider, inst)
		require.NoError(t, err)
		assert.True(t, requeue, "a new import is in flight")
		assert.Equal(t, prepares, inst.prepares.Load())
	}

	sends(1)
	failTask()

	// The failure is surfaced, sanitized, and nothing is sent at once.
	err := holds(r, 1)
	assert.Equal(t, importFailedBackoff.Base, imageHoldRequeueAfter(err))
	assert.Contains(t, err.Error(), "HTTP 404")
	assert.NotContains(t, err.Error(), "s3cret")
	got := getImage(t, r, img)
	ps := got.Status.ProviderStatus[key]
	assert.False(t, ps.Available)
	assert.Empty(t, ps.TaskRef)
	assert.Equal(t, digestOf(t, img), ps.SourceDigest, "the failed import's source")
	assert.Contains(t, ps.Message, "failed")
	assert.NotContains(t, ps.Message, "s3cret")
	cond := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMImageConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, imageReasonImportFailed, cond.Reason)

	// At most one prepare per backoff window: 1 minute, then 2.
	for _, step := range []time.Duration{10 * time.Second, 30 * time.Second, 19 * time.Second} {
		clock.advance(step)
		holds(r, 1)
	}
	clock.advance(2 * time.Second)
	sends(2)
	failTask()
	err = holds(r, 2)
	assert.Equal(t, 2*importFailedBackoff.Base, imageHoldRequeueAfter(err), "the wait doubles")
	clock.advance(61 * time.Second)
	holds(r, 2)
	clock.advance(60 * time.Second)
	sends(3)

	// A manager restart loses the count, not the first wait: the entry's
	// record of the failed import still holds for importFailedBackoff.Base.
	failTask()
	holds(r, 3)
	restarted := &VirtualMachineReconciler{Client: r.Client, Scheme: r.Scheme, clock: clock.now}
	clock.advance(30 * time.Second)
	holds(restarted, 3)
	clock.advance(31 * time.Second)
	requeue, err := restarted.EnsureImageOnProvider(ctx, vm, getImage(t, r, img), provider, inst)
	require.NoError(t, err)
	assert.True(t, requeue)
	assert.EqualValues(t, 4, inst.prepares.Load())
}

func TestEnsureImageOnProvider_UnconfirmedAnswerBacksOff(t *testing.T) {
	ctx := context.Background()
	img := imageWithSource("ubuntu", "")
	r, _ := newEnsureReconciler(t, img)
	clock := newTestClock()
	r.clock = clock.now
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu"}, noEcho: true}
	// Two VMs reconciling the same image: the backoff is per image, Provider
	// and source, not per VM.
	vms := []*infrav1beta1.VirtualMachine{vmForImage(provider.Name, img.Name), vmForImage(provider.Name, img.Name)}
	vms[1].Name = "vm-2"
	ensure := func(vm *infrav1beta1.VirtualMachine) error {
		t.Helper()
		_, err := r.EnsureImageOnProvider(ctx, vm, reloadImage(t, r, img.Name), provider, inst)
		require.ErrorIs(t, err, errImagePrepareHold)
		return err
	}

	err := ensure(vms[0])
	require.ErrorIs(t, err, errImageArtifactNotConfirmed)
	assert.Equal(t, 30*time.Second, imageHoldRequeueAfter(err))
	assert.Equal(t, 1, inst.calls())
	ensure(vms[1])
	assert.Equal(t, 1, inst.calls(), "another VM does not ask again within the window")

	clock.advance(31 * time.Second)
	err = ensure(vms[1])
	assert.Equal(t, 2, inst.calls())
	assert.Equal(t, time.Minute, imageHoldRequeueAfter(err), "the wait doubles")
	for i := 0; i < 6; i++ {
		clock.advance(5 * time.Minute)
		err = ensure(vms[0])
	}
	assert.Equal(t, 5*time.Minute, imageHoldRequeueAfter(err), "bounded at 5 minutes")

	// A confirmed answer ends the backoff.
	inst.mu.Lock()
	inst.noEcho = false
	inst.mu.Unlock()
	clock.advance(5 * time.Minute)
	requeue, err := r.EnsureImageOnProvider(ctx, vms[0], reloadImage(t, r, img.Name), provider, inst)
	require.NoError(t, err)
	assert.False(t, requeue)
	wait, _ := r.prepareBackoff.wait(imagePrepareBackoffKey(img, provider, digestOf(t, img)), clock.now())
	assert.Zero(t, wait)
}

func TestEnsureImageOnProvider_ConcurrentObserversOfAFailedTaskRecordItOnce(t *testing.T) {
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	provA := identityProvider(idTeamA, idProviderName)
	key := imageProviderKey(provA)
	img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
		key: {ProviderUID: string(provA.UID), TaskRef: "task-1", SourceDigest: digestOf(t, img)},
	}
	var committed atomic.Int32
	r, _ := newIdentityReconcilerWithFuncs(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			err := c.SubResource(sub).Update(ctx, obj, opts...)
			if _, ok := obj.(*infrav1beta1.VMImage); ok && err == nil {
				committed.Add(1)
			}
			return err
		},
	}, img, provA)
	clock := newTestClock()
	r.clock = clock.now
	inst := &preparerProvider{isTaskCompleteFn: func(context.Context, string) (bool, error) {
		return true, errors.New("task failed: checksum mismatch")
	}}
	stale := getImage(t, r, img)

	const reconciles = 8
	var start, wg sync.WaitGroup
	start.Add(1)
	errs := make([]error, reconciles)
	for i := 0; i < reconciles; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			_, errs[i] = r.EnsureImageOnProvider(context.Background(), vmUsing(provA, img), stale.DeepCopy(), provA, inst)
		}(i)
	}
	start.Done()
	wg.Wait()

	for i, err := range errs {
		require.ErrorIs(t, err, errImagePrepareHold, "reconcile %d", i)
	}
	assert.Zero(t, inst.calls(), "no prepare is sent at once")
	assert.EqualValues(t, 1, committed.Load(), "the failure is recorded once")
	wait, _ := r.prepareBackoff.wait(imagePrepareBackoffKey(img, provA, digestOf(t, img)), clock.now())
	assert.Equal(t, importFailedBackoff.Base, wait, "the failed task is counted once")
	assert.Contains(t, getImage(t, r, img).Status.ProviderStatus[key].Message, "checksum mismatch")
}

func TestImagePrepareBackoffPolicy(t *testing.T) {
	p := imagePrepareBackoffPolicy{Base: time.Minute, Max: 30 * time.Minute}
	for failures, want := range map[int]time.Duration{
		1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 5: 16 * time.Minute, 6: 30 * time.Minute, 60: 30 * time.Minute,
	} {
		assert.Equal(t, want, p.delay(failures), "failures=%d", failures)
	}

	var b imagePrepareBackoff
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	n, _ := b.fail(backoffImportFailed, "k", "task-1", "m", p, now)
	assert.Equal(t, 1, n)
	n, _ = b.fail(backoffImportFailed, "k", "task-1", "m", p, now)
	assert.Equal(t, 1, n, "the same failed task is counted once")
	n, notBefore := b.fail(backoffImportFailed, "k", "task-2", "m", p, now)
	assert.Equal(t, 2, n)
	assert.Equal(t, now.Add(2*time.Minute), notBefore)
	later := notBefore.Add(imagePrepareBackoffForget + time.Second)
	n, _ = b.fail(backoffImportFailed, "k", "task-3", "m", p, later)
	assert.Equal(t, 1, n, "an old failure no longer lengthens the wait")
	b.clear("k")
	wait, _ := b.wait("k", later)
	assert.Zero(t, wait)
}

// ─── #344 invariants under the confirm call ──────────────────────────────────

func TestEnsureImageOnProvider_ConcurrentConfirmsShareOneCall(t *testing.T) {
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	provA := identityProvider(idTeamA, idProviderName)
	key := imageProviderKey(provA)
	img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
		key: {ProviderUID: string(provA.UID), TaskRef: "task-1", SourceDigest: digestOf(t, img), ID: "ubuntu-on-a"},
	}
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
	// The task is complete (stubProvider); the confirming call reuses it.
	inst := &blockingPreparer{started: make(chan struct{}), release: make(chan struct{}),
		resp: *reusedResponse("ubuntu-on-a")}
	stale := getImage(t, r, img)

	var wg sync.WaitGroup
	copies := make([]*infrav1beta1.VMImage, reconciles)
	requeues := make([]bool, reconciles)
	errs := make([]error, reconciles)
	for i := 0; i < reconciles; i++ {
		copies[i] = stale.DeepCopy()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			requeues[i], errs[i] = r.EnsureImageOnProvider(context.Background(), vmUsing(provA, img), copies[i], provA, inst)
		}(i)
	}
	<-inst.started
	arrived.Wait()
	close(inst.release)
	wg.Wait()

	for i := 0; i < reconciles; i++ {
		require.NoError(t, errs[i])
		assert.False(t, requeues[i], "reconcile %d", i)
		assert.True(t, copies[i].Status.ProviderStatus[key].Available, "reconcile %d sees the confirmed artifact", i)
	}
	assert.EqualValues(t, 1, inst.calls.Load(), "one confirming PrepareImage for all concurrent reconciles")
	assert.EqualValues(t, 1, statusWrites.Load(), "one VMImage status write for all concurrent reconciles")
	got := getImage(t, r, img).Status.ProviderStatus[key]
	assert.True(t, got.Available)
	assert.Empty(t, got.TaskRef)
	assert.Equal(t, digestOf(t, img), got.SourceDigest)
}

func TestEnsureImageOnProvider_ConfirmNeverOverwritesANewerTask(t *testing.T) {
	// A reconcile confirming task-1 whose entry was meanwhile replaced by
	// task-2 records nothing and keeps polling.
	ctx := context.Background()
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	provA := identityProvider(idTeamA, idProviderName)
	key := imageProviderKey(provA)
	digest := digestOf(t, img)
	img.Status.ProviderStatus = map[string]infrav1beta1.ProviderImageStatus{
		key: {ProviderUID: string(provA.UID), TaskRef: "task-1", SourceDigest: digest},
	}
	r, _ := newIdentityReconciler(t, img, provA)
	stale := getImage(t, r, img)
	// Another reconcile replaced the task.
	require.NoError(t, r.writeImageStatus(ctx, getImage(t, r, img), func(i *infrav1beta1.VMImage) {
		ps := i.Status.ProviderStatus[key]
		ps.TaskRef = "task-2"
		i.Status.ProviderStatus[key] = ps
	}))
	inst := &preparerProvider{prepareResp: *reusedResponse("ubuntu-on-a")}

	requeue, err := r.EnsureImageOnProvider(ctx, vmUsing(provA, img), stale, provA, inst)
	require.NoError(t, err)
	assert.True(t, requeue, "the newer task is polled")
	assert.Zero(t, inst.calls(), "the re-read finds the newer task: nothing is confirmed")
	assert.Equal(t, "task-2", getImage(t, r, img).Status.ProviderStatus[key].TaskRef)
}

func TestWriteImageStatus_RefusesARecreatedVMImage(t *testing.T) {
	ctx := context.Background()
	img := sharedOVAImage(idImageNS, "ubuntu", "")
	provA := identityProvider(idTeamA, idProviderName)
	r, _ := newIdentityReconciler(t, img, provA)
	// The reconcile read the previous object of this name.
	previous := getImage(t, r, img)
	previous.UID = "uid-the-deleted-one"
	before := getImage(t, r, img)

	err := r.writeImageStatus(ctx, previous, func(i *infrav1beta1.VMImage) { i.Status.Message = "decided for the previous object" })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "re-created")
	assert.Equal(t, before.ResourceVersion, getImage(t, r, img).ResourceVersion, "nothing is written on the successor")

	inst := &preparerProvider{}
	_, err = r.EnsureImageOnProvider(ctx, vmUsing(provA, img), previous, provA, inst)
	require.Error(t, err)
	assert.Zero(t, inst.calls(), "the successor is never prepared on the predecessor's identity")
}

func TestImagePrepareRequest_RefusesAnImageWithoutUID(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	img.UID = ""
	_, err := imagePrepareRequest(img, importCapableProvider("p"), digestOf(t, img))
	require.Error(t, err)

	img.UID = "uid-1"
	provider := importCapableProvider("p")
	provider.UID = ""
	req, err := imagePrepareRequest(img, provider, digestOf(t, img))
	require.NoError(t, err)
	assert.True(t, req.Provider.IsZero(), "the informational Provider identity is sent only with its UID")
	assert.NotContains(t, req.ImageJSON, "consumerNamespaceSelector")
}

func TestEnsureImageOnProvider_ProviderDetailIsSanitized(t *testing.T) {
	// A provider's error text reaches VMImage status (read in other
	// namespaces for a shared image) and events only sanitized and capped.
	detail := "refused https://user:s3cret@images.example.com/x.ova\nINJECTED LINE " + strings.Repeat("x", 1000)
	for name, perr := range map[string]error{
		"conflict":       contracts.NewConflictError(detail, nil),
		"invalid source": contracts.NewInvalidSpecError(detail, nil),
	} {
		t.Run(name, func(t *testing.T) {
			img := imageWithSource("ubuntu", "")
			r, _ := newEnsureReconciler(t, img)
			rec := &objectRecorder{}
			r.Recorder = rec
			provider := importCapableProvider("libvirt-1")
			inst := &preparerProvider{prepareErr: perr}

			_, err := r.EnsureImageOnProvider(context.Background(), vmForImage(provider.Name, img.Name), img, provider, inst)
			require.Error(t, err)
			got := reloadImage(t, r, img.Name)
			msgs := []string{got.Status.ProviderStatus[imageProviderKey(provider)].Message, got.Status.Message}
			for _, ev := range rec.events {
				msgs = append(msgs, ev.message)
			}
			for _, m := range msgs {
				assert.NotContains(t, m, "s3cret")
				assert.NotContains(t, m, "\n")
				assert.Less(t, len(m), 600, "the provider detail is capped")
			}
			assert.Contains(t, msgs[0], "https://images.example.com/x.ova")
		})
	}
}
