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

package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// errorClass is the class of a Create/Clone failure the VM controller relies
// on (create_errors.go).
type errorClass string

const (
	classInvalidSpec      errorClass = "InvalidSpec"
	classPermissionDenied errorClass = "PermissionDenied"
	// classNothingRemainsVM: CREATE_NOT_PERFORMED + VM_OPERATION_FAILED (one
	// VM's failure, kept out of the circuit breaker).
	classNothingRemainsVM errorClass = "Unavailable+CREATE_NOT_PERFORMED+VM_OPERATION_FAILED"
	// classNothingRemainsProvider: CREATE_NOT_PERFORMED alone (a
	// provider-wide failure, counted by the circuit breaker).
	classNothingRemainsProvider errorClass = "Unavailable+CREATE_NOT_PERFORMED"
	classOutcomeUnknown         errorClass = "outcome unknown (no CREATE_NOT_PERFORMED)"
)

func classOf(t *testing.T, err error) errorClass {
	t.Helper()
	require.Error(t, err)
	vmScoped := contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason)
	switch code := status.Code(err); {
	case code == codes.InvalidArgument:
		return classInvalidSpec
	case code == codes.PermissionDenied:
		return classPermissionDenied
	case contracts.IsCreateNotPerformed(err):
		require.Equal(t, codes.Unavailable, code)
		if vmScoped {
			return classNothingRemainsVM
		}
		return classNothingRemainsProvider
	case code == codes.Unavailable || code == codes.Internal:
		require.False(t, vmScoped, "an unknown outcome never carries VM_OPERATION_FAILED: %v", err)
		return classOutcomeUnknown
	default:
		t.Fatalf("unexpected error class %s: %v", code, err)
		return ""
	}
}

func TestCreateErrors_ClientNotConfigured(t *testing.T) {
	p := New()
	p.client = nil
	_, err := p.Create(context.Background(), createReq(testOwner(), ""))
	assert.Equal(t, classNothingRemainsProvider, classOf(t, err))
	_, err = p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "100", TargetName: "x"})
	assert.Equal(t, classNothingRemainsProvider, classOf(t, err))
}

// TestCreateErrors_PreMutationFailures: a failure before the first mutating
// PVE call is CREATE_NOT_PERFORMED, and nothing is sent that changes PVE. A
// provider-wide one counts toward the circuit breaker; one VM's does not.
func TestCreateErrors_PreMutationFailures(t *testing.T) {
	cases := map[string]struct {
		fault pvefake.Fault
		want  errorClass
	}{
		"cluster/nextid":    {pvefake.Fault{Method: http.MethodGet, PathContains: "/cluster/nextid"}, classNothingRemainsProvider},
		"node discovery":    {pvefake.Fault{Method: http.MethodGet, PathContains: "/api2/json/nodes"}, classNothingRemainsProvider},
		"owner lookup":      {pvefake.Fault{Method: http.MethodGet, PathContains: "/cluster/resources"}, classNothingRemainsProvider},
		"template read":     {pvefake.Fault{Method: http.MethodGet, PathContains: fmt.Sprintf("/qemu/%d/config", seededTemplateID)}, classNothingRemainsVM},
		"owner stamp reads": {pvefake.Fault{Method: http.MethodGet, PathContains: "/qemu/4300/config"}, classNothingRemainsVM},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			// A same-named VM whose stamp must be read by the owner lookup.
			srv.PutVM(pvefake.VM{VMID: 4300, Name: testOwnerName, Node: "pve", Status: "stopped"})
			f := tc.fault
			f.Status, f.Body = http.StatusInternalServerError, "boom"
			srv.InjectFault(f)
			_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
			assert.Equal(t, tc.want, classOf(t, err))
			assert.Empty(t, srv.MutatingRequests())
		})
	}
}

func TestCreateErrors_RequestErrorsAreInvalidSpec(t *testing.T) {
	p, srv := newStampTestProvider(t)
	_, err := p.Create(context.Background(), createReq(testOwner(), `{"TemplateName":"ubuntu"}`))
	assert.Equal(t, classInvalidSpec, classOf(t, err), "a template id that is not a number")

	_, err = p.Create(context.Background(), createReq(testOwner(), `{"TemplateName":"77777"}`))
	assert.Equal(t, classInvalidSpec, classOf(t, err), "a template that does not exist")
	assert.Empty(t, srv.MutatingRequests())
}

// TestCreateErrors_SubmitFailures: the answer to the request that makes the VM
// (template clone, or plain create) is classified by what PVE said.
func TestCreateErrors_SubmitFailures(t *testing.T) {
	cases := []struct {
		name  string
		image string
		fault pvefake.Fault
		want  errorClass
		vmsAt int // VMs named like the request afterwards
	}{
		{"clone 400", templateImageJSON, pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusBadRequest, Body: "parameter verification failed"}, classInvalidSpec, 0},
		{"clone 403", templateImageJSON, pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusForbidden, Body: "Permission check failed"}, classPermissionDenied, 0},
		{"clone other 4xx", templateImageJSON, pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusConflict, Body: "busy"}, classOutcomeUnknown, 0},
		{"clone already exists, VMID free (contradictory)", templateImageJSON, pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusInternalServerError, Body: "unable to create VM 200001: config file already exists"}, classOutcomeUnknown, 0},
		{"clone 500", templateImageJSON, pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusInternalServerError, Body: "proxy error"}, classOutcomeUnknown, 0},
		{"clone 500 after PVE acted", templateImageJSON, pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusBadGateway, Body: "lost", AfterApply: true}, classOutcomeUnknown, 1},
		{"create 400", "", pvefake.Fault{Method: http.MethodPost, PathContains: "/nodes/pve/qemu", Status: http.StatusBadRequest, Body: "bad"}, classInvalidSpec, 0},
		{"create 500", "", pvefake.Fault{Method: http.MethodPost, PathContains: "/nodes/pve/qemu", Status: http.StatusInternalServerError, Body: "boom"}, classOutcomeUnknown, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			tc.fault.Times = 1
			srv.InjectFault(tc.fault)
			_, err := p.Create(context.Background(), createReq(testOwner(), tc.image))
			assert.Equal(t, tc.want, classOf(t, err), "error: %v", err)
			assert.Len(t, srv.VMsNamed(testOwnerName), tc.vmsAt)
		})
	}
}

// TestCreateErrors_LostAnswerThenRebind: PVE made the VM but its answer was
// lost (outcome unknown); the re-sent Create finds the stamped VM, finishes
// it and makes no second one.
func TestCreateErrors_LostAnswerThenRebind(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusBadGateway, Body: "lost", AfterApply: true, Times: 1})
	_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	require.Equal(t, classOutcomeUnknown, classOf(t, err))

	resp, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	require.NoError(t, err)
	vms := srv.VMsNamed(testOwnerName)
	require.Len(t, vms, 1)
	assert.Equal(t, fmt.Sprint(vms[0].VMID), resp.Id)
	assert.Equal(t, ownerStampPhaseReady, stampOf(t, vms[0]).phase)
	assert.Equal(t, 4, vms[0].CPUs)
}

// TestCreateErrors_FailedCloneTask: a clone task that finished with a failure
// (PVE removed its VM) is VM_OPERATION_FAILED.
func TestCreateErrors_FailedCloneTask(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.FailTasks("qmclone")
	_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	assert.Equal(t, classNothingRemainsVM, classOf(t, err))
	assert.Empty(t, srv.VMsNamed(testOwnerName))
}

// TestCreateErrors_FailedStepIsRolledBack: a step after the clone fails; the
// VM, verifiably this request's, is removed: VM_OPERATION_FAILED.
func TestCreateErrors_FailedStepIsRolledBack(t *testing.T) {
	for name, owned := range map[string]bool{"with owner": true, "without owner": false} {
		t.Run(name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			srv.InjectFault(pvefake.Fault{Method: http.MethodPut, PathContains: "/config", Status: http.StatusInternalServerError, Body: "boom", Times: 1})
			req := createReq(testOwner(), templateImageJSON)
			if !owned {
				req.Owner = nil
			}
			_, err := p.Create(context.Background(), req)
			assert.Equal(t, classNothingRemainsVM, classOf(t, err))
			assert.Empty(t, srv.VMsNamed(testOwnerName), "the partial VM was removed")
		})
	}
}

// TestCreateErrors_FailedStepNotRolledBack: when the partial VM cannot be
// removed the outcome is unknown; the re-sent Create finishes that VM.
func TestCreateErrors_FailedStepNotRolledBack(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.InjectFault(pvefake.Fault{Method: http.MethodPut, PathContains: "/config", Status: http.StatusInternalServerError, Body: "boom", Times: 1})
	srv.InjectFault(pvefake.Fault{Method: http.MethodDelete, PathContains: "/qemu/", Status: http.StatusInternalServerError, Body: "cannot remove", Times: 1})
	_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	vms := srv.VMsNamed(testOwnerName)
	require.Len(t, vms, 1)
	assert.Equal(t, ownerStampPhaseCreating, stampOf(t, vms[0]).phase)

	resp, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprint(vms[0].VMID), resp.Id)
	vms = srv.VMsNamed(testOwnerName)
	require.Len(t, vms, 1)
	assert.Equal(t, ownerStampPhaseReady, stampOf(t, vms[0]).phase)
	assert.Equal(t, 4, vms[0].CPUs)
}

// TestCreateErrors_RollbackRefusesAVMThatIsNotThisRequests: the rollback
// removes a VM only when it carries this request's stamp.
func TestCreateErrors_RollbackRefusesAVMThatIsNotThisRequests(t *testing.T) {
	p, srv := newStampTestProvider(t)
	mine := testStamp(testOwnerUID, ownerStampPhaseCreating)
	srv.PutVM(pvefake.VM{VMID: 4400, Name: testOwnerName, Node: "pve", Status: "stopped",
		Description: testStamp(testOtherUID, ownerStampPhaseCreating).line()})
	err := p.rollbackCreatedVM(context.Background(), "pve", 4400, testOwnerName, &mine, true)
	require.Error(t, err)
	_, exists := srv.GetVM(4400)
	assert.True(t, exists)

	// An owner-less request's VM must be unstamped and carry its name.
	err = p.rollbackCreatedVM(context.Background(), "pve", 4400, testOwnerName, nil, true)
	require.Error(t, err)

	// A running VM is never removed.
	srv.PutVM(pvefake.VM{VMID: 4401, Name: testOwnerName, Node: "pve", Status: "running", Description: mine.line()})
	require.Error(t, p.rollbackCreatedVM(context.Background(), "pve", 4401, testOwnerName, &mine, true))

	// Gone after a finished task: nothing remains. Gone while the task may
	// still run: unknown.
	require.NoError(t, p.rollbackCreatedVM(context.Background(), "pve", 4499, testOwnerName, &mine, true))
	require.Error(t, p.rollbackCreatedVM(context.Background(), "pve", 4499, testOwnerName, &mine, false))
}

// TestCloneErrors: the Clone RPC uses the same classes.
func TestCloneErrors(t *testing.T) {
	t.Run("source read", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/qemu/100/config", Status: http.StatusInternalServerError, Body: "boom"})
		_, err := p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "100", TargetName: "c"})
		assert.Equal(t, classNothingRemainsVM, classOf(t, err))
		assert.Empty(t, srv.MutatingRequests())
	})
	t.Run("bad source reference", func(t *testing.T) {
		p, _ := newStampTestProvider(t)
		_, err := p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "a:b:c", TargetName: "c"})
		assert.Equal(t, classInvalidSpec, classOf(t, err))
	})
	t.Run("clone 500", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusInternalServerError, Body: "boom"})
		_, err := p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "100", TargetName: "c"})
		assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	})
	t.Run("failed step rolled back", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		srv.InjectFault(pvefake.Fault{Method: http.MethodPut, PathContains: "/config", Status: http.StatusInternalServerError, Body: "boom", Times: 1})
		target := testOwner()
		_, err := p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "100", TargetName: target.Name,
			TargetVm: ownerProto(target), ClassJson: sizedClassJSON})
		assert.Equal(t, classNothingRemainsVM, classOf(t, err))
		assert.Empty(t, srv.VMsNamed(target.Name))
	})
}

// TestAPIErrorKeepsHistoricalMessage: callers that log the error still see
// "<op> failed with status <code>: <body>".
func TestAPIErrorKeepsHistoricalMessage(t *testing.T) {
	err := &pveapi.APIError{Operation: "clone VM", StatusCode: 500, Body: "x"}
	assert.Equal(t, "clone VM failed with status 500: x", err.Error())
}
