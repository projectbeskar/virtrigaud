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
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// putVMAtNewID returns a fault hook that puts vm at the VMID the clone request
// names (newid), as a VM that took the id since /cluster/nextid.
func putVMAtNewID(t *testing.T, srv *pvefake.Server, vm pvefake.VM) func(r *http.Request) {
	return func(r *http.Request) {
		id, err := strconv.Atoi(r.FormValue("newid"))
		require.NoError(t, err)
		vm.VMID = id
		srv.PutVM(vm)
	}
}

// TestSubmitErrors_AlreadyExists (L3): "already exists" from the clone call
// means nothing was created only when the VM at that VMID is certainly
// another's.
func TestSubmitErrors_AlreadyExists(t *testing.T) {
	const body = "unable to create VM: config file already exists"
	cases := []struct {
		name string
		vm   pvefake.VM
		want errorClass
	}{
		{"another VM took the VMID", pvefake.VM{Name: "other", Node: "pve", Status: "running", Description: "notes"}, classNothingRemainsVM},
		{"another VM took the VMID on another node", pvefake.VM{Name: "other", Node: "pve2", Status: "running"}, classNothingRemainsVM},
		{"the VM carries this request's stamp", pvefake.VM{Name: testOwnerName, Node: "pve", Status: "stopped",
			Description: testStamp(testOwnerUID, ownerStampPhaseCreating).line()}, classOutcomeUnknown},
		{"a clone still being made", pvefake.VM{Node: "pve", Status: "stopped", Lock: "clone"}, classOutcomeUnknown},
		// L-d: the stamp is read wherever the VM lives.
		{"this request's stamp, on another node", pvefake.VM{Name: testOwnerName, Node: "pve2", Status: "stopped",
			Description: testStamp(testOwnerUID, ownerStampPhaseCreating).line()}, classOutcomeUnknown},
		{"a clone still being made, on another node", pvefake.VM{Node: "pve2", Status: "stopped", Lock: "clone"}, classOutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusInternalServerError,
				Body: body, Times: 1, Then: putVMAtNewID(t, srv, tc.vm)})
			_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
			assert.Equal(t, tc.want, classOf(t, err), "error: %v", err)
		})
	}
}

// TestSubmitErrors_OnlyPVEAnswersProveNothingWasDone (L3): a 4xx that is not
// PVE's JSON (a proxy's page), or a 4xx other than 400/401/403, leaves the
// outcome unknown.
func TestSubmitErrors_OnlyPVEAnswersProveNothingWasDone(t *testing.T) {
	cases := []struct {
		name  string
		fault pvefake.Fault
		want  errorClass
	}{
		{"400 from PVE", pvefake.Fault{Status: http.StatusBadRequest, Body: "name: invalid format"}, classInvalidSpec},
		{"400 from a proxy", pvefake.Fault{Status: http.StatusBadRequest, Body: "<html>Bad Request</html>", Raw: true}, classOutcomeUnknown},
		{"403 from PVE", pvefake.Fault{Status: http.StatusForbidden, Body: "Permission check failed"}, classPermissionDenied},
		{"403 from a proxy", pvefake.Fault{Status: http.StatusForbidden, Body: "denied", Raw: true}, classOutcomeUnknown},
		{"409 from PVE", pvefake.Fault{Status: http.StatusConflict, Body: "busy"}, classOutcomeUnknown},
		{"429 from a proxy", pvefake.Fault{Status: http.StatusTooManyRequests, Body: "slow down", Raw: true}, classOutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			f := tc.fault
			f.Method, f.PathContains, f.Times = http.MethodPost, "/clone", 1
			srv.InjectFault(f)
			_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
			assert.Equal(t, tc.want, classOf(t, err), "error: %v", err)
		})
	}
}

func TestPVEJSONBody(t *testing.T) {
	assert.True(t, isPVEJSONBody(`{"data":null,"errors":{"name":"invalid format"}}`))
	assert.True(t, isPVEJSONBody(`{"data":null,"message":"x"}`))
	assert.False(t, isPVEJSONBody(`<html>502</html>`))
	assert.False(t, isPVEJSONBody(`{"error":"x"}`))
	assert.Equal(t, "boom; a: x; b: y", pveBodyErrorText(`{"data":null,"message":"boom","errors":{"b":"y","a":"x"}}`))
	assert.Empty(t, pveBodyErrorText(`{"data":null}`))
	assert.Equal(t, "plain", pveBodyErrorText("plain"))
}
