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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the manager side of the libvirt domain-ownership fix: the VM
// controller sends the VirtualMachine's identity on every Create, and a
// NON-retryable Create rejection (the provider refusing to bind to a domain the
// VM does not own, or an unusable name) is surfaced as a specific Ready=False
// condition with a slow requeue — never a Status.ID, never a 5s hot loop.

// rejectingCreateProvider returns createErr from Create and records every
// request and every Delete, embedding stubProvider for the rest.
type rejectingCreateProvider struct {
	stubProvider
	createErr  error
	createReqs []contracts.CreateRequest
	deleteIDs  []string
}

func (p *rejectingCreateProvider) Create(_ context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	p.createReqs = append(p.createReqs, req)
	if p.createErr != nil {
		return contracts.CreateResponse{}, p.createErr
	}
	return contracts.CreateResponse{ID: req.Name}, nil
}

func (p *rejectingCreateProvider) Delete(_ context.Context, id string) (string, error) {
	p.deleteIDs = append(p.deleteIDs, id)
	return "", nil
}

// creatableVM returns a VM that passes createVM's spec validation, with a UID
// and a generation so identity threading and ObservedGeneration are observable.
func creatableVM(ns string) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := baseVM(ns)
	vm.Name = "web"
	vm.UID = types.UID("6f1c2d6e-0a57-4d0e-9d4b-6f3bb1f1a001")
	vm.Generation = 3
	vm.Spec.ImportedDisk = &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"}
	return vm
}

// wireConflict is what the transport client hands the controller when the
// provider answers codes.AlreadyExists (mapGRPCError).
func wireConflict() error {
	return contracts.NewConflictError(
		`create: libvirt domain "web" already exists on the host and is not owned by this VirtualMachine; refusing to bind to it.`,
		status.Error(codes.AlreadyExists, "raw grpc chain"))
}

func TestBuildCreateRequest_ThreadsOwnerIdentity(t *testing.T) {
	s := coverageTestScheme(t)
	r := newTestReconciler(s, nil)
	vm := creatableVM("team-a")
	_, class := providerAndClass("team-a")

	req, err := r.buildCreateRequest(context.Background(), vm, "", class, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, contracts.ObjectIdentity{
		UID:       "6f1c2d6e-0a57-4d0e-9d4b-6f3bb1f1a001",
		Namespace: "team-a",
		Name:      "web",
	}, req.Owner, "Create must carry the VirtualMachine's UID/namespace/name as the owner identity")
}

func TestCreateVM_ConflictSetsConditionAndBacksOff(t *testing.T) {
	s := coverageTestScheme(t)
	ns := "team-b"
	prov, class := providerAndClass(ns)
	vm := creatableVM(ns)

	cp := &rejectingCreateProvider{createErr: wireConflict()}
	r := newTestReconciler(s, &stubResolver{provider: cp}, prov, class, vm)

	res, err := r.reconcileVM(context.Background(), vm)
	require.NoError(t, err, "a rejected create is a surfaced condition, not a reconcile error")

	require.Len(t, cp.createReqs, 1)
	assert.Equal(t, string(vm.UID), cp.createReqs[0].Owner.UID, "the create must carry this VM's UID")

	// No hot loop: the slow rejected-create cadence, not the 5s transient one.
	assert.Equal(t, vmCreateConflictRetryInterval, res.RequeueAfter)
	assert.GreaterOrEqual(t, res.RequeueAfter, time.Minute)

	// Never bound: no Status.ID, so a later delete cannot reach provider.Delete.
	persisted := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: vm.Name}, persisted))
	assert.Empty(t, persisted.Status.ID, "a VM whose create was refused must not be bound to the colliding domain")

	for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
		c := meta.FindStatusCondition(persisted.Status.Conditions, condType)
		require.NotNil(t, c, "%s condition must be set", condType)
		assert.Equal(t, metav1.ConditionFalse, c.Status, condType)
		assert.Equal(t, k8s.ReasonProviderConflict, c.Reason, condType)
		assert.Equal(t, vm.Generation, c.ObservedGeneration, "%s must record ObservedGeneration", condType)
		assert.Contains(t, c.Message, `libvirt domain "web" already exists`)
		assert.NotContains(t, c.Message, "raw grpc chain", "the condition carries the categorized message, not the gRPC error chain")
	}
}

func TestCreateVM_InvalidSpecSetsConditionAndBacksOff(t *testing.T) {
	s := coverageTestScheme(t)
	ns := "team-b"
	prov, class := providerAndClass(ns)
	vm := creatableVM(ns)

	cp := &rejectingCreateProvider{createErr: contracts.NewInvalidSpecError(
		`create: name "12" cannot be used as a libvirt domain name`, nil)}
	r := newTestReconciler(s, &stubResolver{provider: cp}, prov, class, vm)

	res, err := r.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	assert.Equal(t, vmCreateInvalidSpecRetryInterval, res.RequeueAfter)

	persisted := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: vm.Name}, persisted))
	assert.Empty(t, persisted.Status.ID)
	c := meta.FindStatusCondition(persisted.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonValidationError, c.Reason)
	assert.Equal(t, vm.Generation, c.ObservedGeneration)
}

// TestCreateVM_TransientErrorKeepsFastRetry is the regression guard: an
// ordinary (retryable / uncategorized) create failure keeps the existing 5s
// retry and ProviderError reason — only non-retryable rejections back off.
func TestCreateVM_TransientErrorKeepsFastRetry(t *testing.T) {
	for name, createErr := range map[string]error{
		"retryable":     contracts.NewRetryableError("create: host busy", nil),
		"uncategorized": errors.New("create failed: boom"),
	} {
		t.Run(name, func(t *testing.T) {
			s := coverageTestScheme(t)
			ns := "team-b"
			prov, class := providerAndClass(ns)
			vm := creatableVM(ns)

			cp := &rejectingCreateProvider{createErr: createErr}
			r := newTestReconciler(s, &stubResolver{provider: cp}, prov, class, vm)

			res, err := r.reconcileVM(context.Background(), vm)
			require.NoError(t, err)
			assert.Equal(t, 5*time.Second, res.RequeueAfter)

			c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionProvisioning)
			require.NotNil(t, c)
			assert.Equal(t, k8s.ReasonProviderError, c.Reason)
		})
	}
}

// TestHandleDeletion_RejectedCreateNeverDeletesCollidingDomain proves the
// deletion side of the fix: a VM whose create was refused has no Status.ID, so
// deleting it removes the finalizer WITHOUT calling provider.Delete — it cannot
// be used to destroy the other tenant's same-named domain.
func TestHandleDeletion_RejectedCreateNeverDeletesCollidingDomain(t *testing.T) {
	s := coverageTestScheme(t)
	ns := "team-b"
	prov, class := providerAndClass(ns)
	vm := creatableVM(ns)
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	now := metav1.Now()
	vm.DeletionTimestamp = &now

	cp := &rejectingCreateProvider{}
	r := newTestReconciler(s, &stubResolver{provider: cp}, prov, class, vm)

	_, err := r.handleDeletion(context.Background(), vm)
	require.NoError(t, err)
	assert.Empty(t, cp.deleteIDs, "an unbound VM (empty Status.ID) must never reach provider.Delete")
}

// TestReconcileVM_AdoptedEmptyID_StillSkipsCreate re-pins the adopted-VM
// double-create guard alongside the new rejection handling: adoption is the
// legitimate way to manage a pre-existing domain, so an adopted VM with an empty
// Status.ID must still never be sent to Create (where it would now be refused).
func TestReconcileVM_AdoptedEmptyID_StillSkipsCreate(t *testing.T) {
	s := coverageTestScheme(t)
	ns := "team-a"
	prov, class := providerAndClass(ns)
	vm := creatableVM(ns)
	vm.Labels = map[string]string{AdoptedLabel: AdoptedLabelValue}

	cp := &rejectingCreateProvider{createErr: wireConflict()}
	r := newTestReconciler(s, &stubResolver{provider: cp}, prov, class, vm)

	_, err := r.reconcileVM(context.Background(), vm)
	require.NoError(t, err)
	assert.Empty(t, cp.createReqs, "an adopted VM awaiting its Status.ID must not be created")
}
