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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the pacing of the definite create refusals other than
// InvalidSpec and Conflict (v0.4.1): NotFound, NotSupported (Unimplemented),
// Unauthorized and QuotaExceeded were re-sent every 5 s; they now follow the
// create-refusal backoff, keeping their reason (ProviderError) and condition.

// pacedRefusals are the answers createRefusalPaced selects.
func pacedRefusals() map[string]error {
	return map[string]error{
		"NotFound":      contracts.NewNotFoundError("create: storage pool \"vms\" not found", nil),
		"NotSupported":  contracts.NewNotSupportedError("create: Unimplemented"),
		"Unauthorized":  contracts.NewUnauthorizedError("create: not authorized", nil),
		"QuotaExceeded": contracts.NewQuotaExceededError("create: quota exceeded", nil),
	}
}

func TestCreateRefusalPaced_Classification(t *testing.T) {
	for name, err := range pacedRefusals() {
		assert.True(t, createRefusalPaced(err), name)
		assert.True(t, createRefusalPaced(errors.Join(errors.New("wrapped"), err)), "%s, wrapped", name)
	}
	for name, err := range map[string]error{
		"nil":                         nil,
		"InvalidSpec (own handling)":  contracts.NewInvalidSpecError("bad", nil),
		"Conflict (fixed re-check)":   wireConflict(),
		"retryable":                   contracts.NewRetryableError("host busy", nil),
		"uncategorized":               errors.New("boom"),
		"unavailable":                 contracts.NewUnavailableError("down", nil),
		"timeout":                     contracts.NewTimeoutError("slow", nil),
		"untyped FailedPrecondition":  status.Error(codes.FailedPrecondition, "operator must act"),
		"untyped PermissionDenied":    status.Error(codes.PermissionDenied, "denied"),
		"CREATE_NOT_PERFORMED":        contracts.MarkCreateNotPerformed(contracts.NewNotFoundError("nothing made", nil)),
		"VM_DISK_CHECK_FAILED":        contracts.NewRetryableError("disk check", contracts.ErrVMDiskCheckFailed),
		"previous incarnation":        contracts.NewConflictError("held", contracts.ErrVMPreviousIncarnation),
		"rate limited (retryable)":    contracts.NewRateLimitError("slow down", nil),
		"host unavailable (clusters)": contracts.NewHostUnavailableError("host down", nil),
	} {
		assert.False(t, createRefusalPaced(err), name)
	}
}

func TestReconcileVM_PacedRefusal_BacksOffKeepingItsReason(t *testing.T) {
	for name, refusal := range pacedRefusals() {
		t.Run(name, func(t *testing.T) {
			h := newRefusalHarness(t, refusal)
			res := h.reconcile(nil)
			require.Equal(t, 1, h.creates())
			assert.Equal(t, 30*time.Second, res.RequeueAfter, "not the 5 s transient cadence")

			vm := h.vm()
			assert.Empty(t, vm.Status.ID)
			prov := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
			require.NotNil(t, prov)
			assert.Equal(t, metav1.ConditionFalse, prov.Status)
			assert.Equal(t, k8s.ReasonProviderError, prov.Reason, "the reason is unchanged")
			assert.Equal(t, vm.Generation, prov.ObservedGeneration)
			assert.True(t, strings.HasPrefix(prov.Message,
				"Failed to create VM (non-retryable; re-checking in 30s, at 2026-10-26T09:00:30Z"), prov.Message)
			assert.True(t, strings.HasSuffix(prov.Message, providerFailureDetail(refusal)), prov.Message)

			h.now = h.now.Add(10 * time.Second)
			held := h.reconcile(nil)
			assert.Equal(t, 1, h.creates(), "nothing is sent while the wait runs")
			assert.Equal(t, 20*time.Second, held.RequeueAfter)

			h.now = h.now.Add(20 * time.Second)
			res = h.reconcile(nil)
			assert.Equal(t, 2, h.creates())
			assert.Equal(t, time.Minute, res.RequeueAfter)

			// A spec change retries at once.
			h.bumpGeneration()
			res = h.reconcile(nil)
			assert.Equal(t, 3, h.creates())
			assert.Equal(t, 30*time.Second, res.RequeueAfter)
		})
	}
}

func TestReconcileVM_PacedRefusal_SurvivesRestart(t *testing.T) {
	h := newRefusalHarness(t, contracts.NewNotFoundError("create: network \"lan\" not found", nil))
	h.reconcile(nil)
	h.now = h.now.Add(30 * time.Second)
	res := h.reconcile(nil)
	require.Equal(t, time.Minute, res.RequeueAfter)

	// Recovered from Provisioning: Ready does not record this refusal.
	h.r = &VirtualMachineReconciler{Client: h.r.Client, Scheme: h.r.Scheme, RemoteResolver: h.r.RemoteResolver,
		clock: func() time.Time { return h.now }}
	h.now = h.now.Add(15 * time.Second)
	held := h.reconcile(nil)
	assert.Equal(t, 2, h.creates(), "the restarted manager keeps the wait")
	assert.Equal(t, 45*time.Second, held.RequeueAfter)
}

func TestReconcileVM_TransientCreateFailuresKeepTheirCadence(t *testing.T) {
	for name, createErr := range map[string]error{
		"retryable":            contracts.NewRetryableError("create: host busy", nil),
		"uncategorized":        errors.New("create failed: boom"),
		"CREATE_NOT_PERFORMED": contracts.MarkCreateNotPerformed(contracts.NewRetryableError("create: nothing made", nil)),
	} {
		t.Run(name, func(t *testing.T) {
			h := newRefusalHarness(t, createErr)
			res := h.reconcile(nil)
			assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
			assert.False(t, h.r.createRefusals.known(h.vm().UID), "not a paced refusal")
		})
	}
}

func TestPaceRefusedCreate_KeepMarkerRecordsReadyOnly(t *testing.T) {
	vm := creatableVM("team-k")
	k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonCreateRequested, "marker", vm.Generation)
	r := newTestReconciler(coverageTestScheme(t), nil, vm)
	r.clock = func() time.Time { return time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC) }

	res := r.paceRefusedCreate(context.Background(), vm, "in", contracts.NewNotFoundError("create: gone", nil), true)
	assert.Equal(t, 30*time.Second, res.RequeueAfter)
	prov := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
	require.NotNil(t, prov)
	assert.Equal(t, k8s.ReasonCreateRequested, prov.Reason, "an answer to a re-send keeps the create marker")
	ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonProviderError, ready.Reason)
	assert.True(t, strings.HasPrefix(ready.Message, createRefusalHeadFailed+createRefusalMessageWait), ready.Message)
}

func TestHandleClusteredCreateError_PacesDefiniteRefusals(t *testing.T) {
	for name, refusal := range pacedRefusals() {
		t.Run(name, func(t *testing.T) {
			vm := creatableVM("team-c")
			r := newTestReconciler(coverageTestScheme(t), nil, vm)
			r.clock = func() time.Time { return time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC) }
			res, err := r.handleClusteredCreateError(context.Background(), vm, "host-a", "in", refusal)
			require.NoError(t, err)
			assert.Equal(t, 30*time.Second, res.RequeueAfter, "not the 5 s transient cadence")
			prov := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
			require.NotNil(t, prov)
			assert.Equal(t, k8s.ReasonProviderError, prov.Reason)
			assert.True(t, strings.HasPrefix(prov.Message, createRefusalHeadFailed+createRefusalMessageWait), prov.Message)
			assert.True(t, r.createRefusals.known(vm.UID))
		})
	}
	vm := creatableVM("team-c")
	r := newTestReconciler(coverageTestScheme(t), nil, vm)
	res, err := r.handleClusteredCreateError(context.Background(), vm, "host-a", "in", errors.New("boom"))
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter, "a transient failure keeps its cadence")
}

func TestCreateRefusalIndexValues_PacedRefusal(t *testing.T) {
	vm := creatableVM("team-a")
	vm.Status.Conditions = []metav1.Condition{{
		Type: k8s.ConditionProvisioning, Status: metav1.ConditionFalse, Reason: k8s.ReasonProviderError,
		Message: createRefusalMessage(createRefusalHeadFailed, time.Minute, time.Now(), "create: not found"),
	}}
	assert.Len(t, createRefusalIndexValues(vm), 2, "a paced refusal is re-driven by a Provider or VMClass change")
	vm.Status.Conditions[0].Message = "Failed to create VM: boom"
	assert.Empty(t, createRefusalIndexValues(vm), "a transient ProviderError is not")
}

func TestEnsureImageOnProvider_DefiniteRefusalIsPaced(t *testing.T) {
	for name, refusal := range map[string]error{
		"NotFound":         contracts.NewNotFoundError("image prepare: storage pool missing", nil),
		"NotSupported":     contracts.NewNotSupportedError("image prepare: Unimplemented"),
		"PermissionDenied": status.Error(codes.PermissionDenied, "image prepare: denied"),
	} {
		t.Run(name, func(t *testing.T) {
			img := imageWithSource("ubuntu", "")
			r, _ := newEnsureReconciler(t, img)
			now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
			r.clock = func() time.Time { return now }
			provider := importCapableProvider("libvirt-1")
			inst := &preparerProvider{prepareErr: refusal}
			vm := vmForImage(provider.Name, img.Name)
			ensure := func() error {
				t.Helper()
				_, err := r.EnsureImageOnProvider(context.Background(), vm, reloadImage(t, r, img.Name), provider, inst)
				require.Error(t, err)
				return err
			}

			before := reloadImage(t, r, img.Name).Status
			first := ensure()
			require.Equal(t, 1, inst.prepareCalls)
			assert.False(t, errors.Is(first, errImagePrepareHold))
			var ra *retryAfterError
			require.True(t, errors.As(first, &ra))
			assert.Equal(t, 30*time.Second, ra.after)

			held := ensure()
			assert.True(t, errors.Is(held, errImagePrepareHold), "held, not sent again: %v", held)
			assert.Contains(t, held.Error(), "image prepare refused by provider")
			assert.Equal(t, 1, inst.prepareCalls)

			now = now.Add(30 * time.Second)
			require.Error(t, ensure())
			assert.Equal(t, 2, inst.prepareCalls)

			// The VMImage's status is left as it was for these (no
			// InvalidSource).
			assert.Equal(t, before, reloadImage(t, r, img.Name).Status)
		})
	}
}

func TestImagePrepareRefused_Classification(t *testing.T) {
	assert.True(t, imagePrepareRefused(contracts.NewNotFoundError("x", nil)))
	assert.True(t, imagePrepareRefused(contracts.NewNotSupportedError("x")))
	assert.True(t, imagePrepareRefused(contracts.NewUnauthorizedError("x", nil)))
	assert.True(t, imagePrepareRefused(contracts.NewQuotaExceededError("x", nil)))
	assert.True(t, imagePrepareRefused(status.Error(codes.PermissionDenied, "x")))
	for _, err := range []error{
		nil,
		contracts.NewInvalidSpecError("x", nil),
		contracts.NewConflictError("x", nil),
		contracts.NewInProgressError("x", nil),
		contracts.NewRetryableError("x", nil),
		contracts.NewUnavailableError("x", nil),
		status.Error(codes.Unavailable, "x"),
		errors.New("x"),
	} {
		assert.False(t, imagePrepareRefused(err), "%v", err)
	}
}
