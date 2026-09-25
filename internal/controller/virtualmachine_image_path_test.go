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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// rejectedPathErr is what the transport client hands the controller when the
// libvirt provider refuses an image path (gRPC InvalidArgument → InvalidSpec).
func rejectedPathErr() error {
	return contracts.NewInvalidSpecError(
		`create: failed to create VM: libvirt image path "/etc/shadow" rejected: it does not resolve to a file directly inside an allowed image directory`,
		errors.New("rpc error: code = InvalidArgument desc = ..."))
}

// TestProviderFailureOutcome pins the classification: a provider InvalidSpec
// rejection is a ValidationError with a slow recheck; anything else keeps the
// ProviderError / 5s cadence.
func TestProviderFailureOutcome(t *testing.T) {
	reason, after := providerFailureOutcome(rejectedPathErr())
	assert.Equal(t, k8s.ReasonValidationError, reason)
	assert.Equal(t, vmCreateInvalidSpecRetryInterval, after)

	reason, after = providerFailureOutcome(fmt.Errorf("prepare image x: %w", rejectedPathErr()))
	assert.Equal(t, k8s.ReasonValidationError, reason, "wrapping must not hide the rejection")
	assert.Equal(t, vmCreateInvalidSpecRetryInterval, after)

	for _, err := range []error{
		errors.New("create failed: connection refused"),
		contracts.NewRetryableError("host unreachable", nil),
	} {
		reason, after = providerFailureOutcome(err)
		assert.Equal(t, k8s.ReasonProviderError, reason)
		assert.Equal(t, providerErrorRetryInterval, after)
	}
}

// TestProviderErrorMessage keeps the condition message to the provider's
// message (no duplicated "caused by: rpc error" tail).
func TestProviderErrorMessage(t *testing.T) {
	msg := providerErrorMessage(fmt.Errorf("wrap: %w", rejectedPathErr()))
	assert.Contains(t, msg, `libvirt image path "/etc/shadow" rejected`)
	assert.NotContains(t, msg, "rpc error")
	assert.Equal(t, "plain", providerErrorMessage(errors.New("plain")))
}

// TestCreateVM_RejectedImagePathIsAValidationFailure proves a Create the
// provider rejected as InvalidSpec surfaces a readable ValidationError
// condition and backs off instead of retrying every 5s.
func TestCreateVM_RejectedImagePathIsAValidationFailure(t *testing.T) {
	const ns = "default"
	s := coverageTestScheme(t)
	providerCR := singleProviderCR("prov-single", ns)
	vm := clusterVM("vm-bad-path", ns, providerCR.Name)
	vmClass := smallVMClass(ns)
	vmImage := minimalVMImage(ns)

	prov := &recordingCreateProvider{err: rejectedPathErr()}
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, providerCR, vmClass, vmImage)

	res, err := r.createVM(context.Background(), vm, prov, providerCR, vmClass, vmImage, nil)
	require.NoError(t, err, "a rejected spec is recorded on status, not returned as a reconcile error")
	assert.Equal(t, vmCreateInvalidSpecRetryInterval, res.RequeueAfter)
	assert.Equal(t, k8s.ReasonValidationError, provisioningReason(vm))
	c := k8s.GetCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
	require.NotNil(t, c)
	assert.Contains(t, c.Message, `libvirt image path "/etc/shadow" rejected`)
	assert.NotContains(t, c.Message, "rpc error")

	// A transient provider failure keeps the fast retry.
	prov.err = errors.New("dial tcp: connection refused")
	res, err = r.createVM(context.Background(), vm, prov, providerCR, vmClass, vmImage, nil)
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
	assert.Equal(t, k8s.ReasonProviderError, provisioningReason(vm))
}

// TestEnsureImageOnProvider_RejectedSourceRecordedOnImage proves an
// ImagePrepare InvalidSpec rejection is written to the VMImage (per-provider
// message + Failed/InvalidSource while not Ready anywhere).
func TestEnsureImageOnProvider_RejectedSourceRecordedOnImage(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	img.Spec.Source.Libvirt.Path = "/etc/shadow"
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{prepareErr: rejectedPathErr()}
	vm := vmForImage(provider.Name, img.Name)

	requeue, err := r.EnsureImageOnProvider(context.Background(), vm, img, provider, inst)
	require.Error(t, err)
	assert.False(t, requeue)
	assert.True(t, contracts.IsInvalidSpec(err), "the caller classifies it as a validation failure")

	got := reloadImage(t, r, img.Name)
	assert.Equal(t, infravirtrigaudiov1beta1.ImagePhaseFailed, got.Status.Phase)
	ps := got.Status.ProviderStatus[provider.Name]
	assert.False(t, ps.Available)
	assert.Contains(t, ps.Message, "rejected")
	cond := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMImageConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, imageReasonInvalidSource, cond.Reason)
	assert.NotContains(t, cond.Message, "rpc error")
}

// TestEnsureImageOnProvider_RejectionDoesNotMaskReadyElsewhere proves a
// rejection on one provider leaves an image that is Ready on another provider
// Ready (Ready is the OR across providers).
func TestEnsureImageOnProvider_RejectionDoesNotMaskReadyElsewhere(t *testing.T) {
	img := imageWithSource("ubuntu", "")
	img.Status = infravirtrigaudiov1beta1.VMImageStatus{
		Ready: true,
		Phase: infravirtrigaudiov1beta1.ImagePhaseReady,
		ProviderStatus: map[string]infravirtrigaudiov1beta1.ProviderImageStatus{
			"libvirt-0": {Available: true, Path: "/var/lib/libvirt/images/ubuntu.qcow2"},
		},
		AvailableOn: []string{"libvirt-0"},
	}
	r, _ := newEnsureReconciler(t, img)
	provider := importCapableProvider("libvirt-1")
	inst := &preparerProvider{prepareErr: rejectedPathErr()}

	_, err := r.EnsureImageOnProvider(context.Background(), vmForImage(provider.Name, img.Name), img, provider, inst)
	require.Error(t, err)

	got := reloadImage(t, r, img.Name)
	assert.True(t, got.Status.Ready)
	assert.Equal(t, infravirtrigaudiov1beta1.ImagePhaseReady, got.Status.Phase)
	assert.True(t, got.Status.ProviderStatus["libvirt-0"].Available)
	assert.Contains(t, got.Status.ProviderStatus[provider.Name].Message, "rejected")
}

// importedDiskPath is the landing disk of the VMMigration fixtures below.
const importedDiskPath = "/var/lib/libvirt/images/web-migrated.qcow2"

// landingMigration returns a VMMigration in ns whose spec targets VM "web" and
// whose (tenant-unwritable) status records importedDiskPath as the imported disk.
func landingMigration(name, ns string) *infravirtrigaudiov1beta1.VMMigration {
	return &infravirtrigaudiov1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
			Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web"},
		},
		Status: infravirtrigaudiov1beta1.VMMigrationStatus{
			ImportID: "web" + contracts.ImportedDiskNameSuffix,
			DiskInfo: &infravirtrigaudiov1beta1.MigrationDiskInfo{TargetPath: importedDiskPath},
		},
	}
}

// TestBuildCreateRequest_ImportedDiskFlag proves ImportedDisk (the provider's
// only attach-in-place case) is set ONLY for a disk a VMMigration in the VM's
// own namespace verifiably imported for this VM — spec.importedDisk alone,
// which any tenant can write, is not enough.
func TestBuildCreateRequest_ImportedDiskFlag(t *testing.T) {
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 1, Memory: resource.MustParse("1Gi")},
	}
	vmFor := func(name, ns, migration, path string) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := baseVM(ns)
		vm.Name = name
		vm.Spec.ImportedDisk = &infravirtrigaudiov1beta1.ImportedDiskRef{
			DiskID: "web" + contracts.ImportedDiskNameSuffix,
			Path:   path,
			Source: "migration",
		}
		if migration != "" {
			vm.Spec.ImportedDisk.MigrationRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: migration}
		}
		return vm
	}
	crossNS := landingMigration("m-cross", "ns-a")
	crossNS.Spec.Target.Namespace = "ns-b" // targets ns-b/web, but lives in ns-a
	otherTargetNS := landingMigration("m-other-target", "ns-b")
	otherTargetNS.Spec.Target.Namespace = "ns-c"
	noDiskInfo := landingMigration("m-no-diskinfo", "ns-a")
	noDiskInfo.Status.DiskInfo = nil

	cases := map[string]struct {
		vm   *infravirtrigaudiov1beta1.VirtualMachine
		want bool
	}{
		"own migration, same namespace, matching path and name": {vmFor("web", "ns-a", "m1", importedDiskPath), true},
		"migrationRef missing":                           {vmFor("web", "ns-a", "", importedDiskPath), false},
		"migration only exists in another namespace":     {vmFor("web", "ns-b", "m1", importedDiskPath), false},
		"migration in another namespace targets this VM": {vmFor("web", "ns-b", "m-cross", importedDiskPath), false},
		"migration targets another namespace":            {vmFor("web", "ns-b", "m-other-target", importedDiskPath), false},
		"mismatched path":                                {vmFor("web", "ns-a", "m1", "/var/lib/libvirt/images/victim-disk.qcow2"), false},
		"mismatched VM name":                             {vmFor("attacker", "ns-a", "m1", importedDiskPath), false},
		"migration has no recorded disk":                 {vmFor("web", "ns-a", "m-no-diskinfo", importedDiskPath), false},
		"migration does not exist":                       {vmFor("web", "ns-a", "nope", importedDiskPath), false},
	}
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil,
		landingMigration("m1", "ns-a"), crossNS, otherTargetNS, noDiskInfo, landingMigration("m1-b", "ns-b"))
	for name, tc := range cases {
		req, err := r.buildCreateRequest(context.Background(), tc.vm, nil, vmClass, nil, nil)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, req.Image.ImportedDisk, name)
		assert.Equal(t, tc.vm.Spec.ImportedDisk.Path, req.Image.Path, name)
	}

	fromImage := baseVM("default")
	fromImage.Spec.ImageRef = &infravirtrigaudiov1beta1.ObjectRef{Name: "img"}
	vmImage := &infravirtrigaudiov1beta1.VMImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VMImageSpec{Source: infravirtrigaudiov1beta1.ImageSource{
			Libvirt: &infravirtrigaudiov1beta1.LibvirtImageSource{Path: "/var/lib/libvirt/images/web-migrated.qcow2"},
		}},
	}
	req, err := r.buildCreateRequest(context.Background(), fromImage, nil, vmClass, vmImage, nil)
	require.NoError(t, err)
	assert.False(t, req.Image.ImportedDisk, "a VMImage path is a base image, never an imported disk")
}

// TestRetryIntervalsOrdered guards the intent of the two cadences: the
// validation recheck must be much slower than the transient-failure retry, or
// a rejected spec would hot-loop the provider.
func TestRetryIntervalsOrdered(t *testing.T) {
	assert.Greater(t, vmCreateInvalidSpecRetryInterval, providerErrorRetryInterval)
	assert.GreaterOrEqual(t, vmCreateConflictRetryInterval, time.Minute)
}
