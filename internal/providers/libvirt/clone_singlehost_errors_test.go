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

package libvirt

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the single-host Clone's wire classification: a transient
// failure before anything was copied reaches the manager as Unavailable (it
// holds the clone and retries), never as the terminal codes.Unknown it used to
// be; a refusal keeps its own status; anything else stays terminal.

// sshDown is a virsh command that never completed on the host (the SSH
// session failed): isHostTransportFailure.
func sshDown() error {
	return &VirshError{Command: "domstate", ExitCode: -1, Stderr: "ssh: handshake failed"}
}

func TestSingleHostCloneRPCError(t *testing.T) {
	for name, tc := range map[string]struct {
		err          error
		code         codes.Code
		vmOpFailed   bool
		wantInWire   string
		notInWire    string
		keepsWrapped bool
	}{
		"host unreachable": {err: fmt.Errorf("list: %w", sshDown()), code: codes.Unavailable, vmOpFailed: true,
			wantInWire: "the libvirt host could not be reached", notInWire: "handshake"},
		"retryable before the copy": {err: contracts.NewRetryableError("failed to list domains", errors.New("virsh: raw output")),
			code: codes.Unavailable, vmOpFailed: true, wantInWire: "failed to list domains", notInWire: "raw output"},
		"pool read failed": {err: contracts.NewRetryableError(`could not read the clone's storage pool "default"`, errors.New("x")),
			code: codes.Unavailable, vmOpFailed: true, wantInWire: "storage pool"},
		"source running keeps its status": {err: &sourceRunningError{state: "running"}, code: codes.FailedPrecondition},
		"invalid spec stays terminal":     {err: contracts.NewInvalidSpecError("target VM exists", nil), code: codes.Unknown, keepsWrapped: true},
		"timeout stays terminal":          {err: contracts.NewTimeoutError("copy timed out", nil), code: codes.Unknown, keepsWrapped: true},
		"plain error stays terminal":      {err: errors.New("define target domain: boom"), code: codes.Unknown, keepsWrapped: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := singleHostCloneRPCError(tc.err)
			assert.Equal(t, tc.code, status.Code(got), "%v", got)
			st := status.Convert(got)
			if tc.wantInWire != "" {
				assert.Contains(t, st.Message(), tc.wantInWire)
			}
			if tc.notInWire != "" {
				assert.NotContains(t, st.Message(), tc.notInWire)
			}
			if tc.keepsWrapped {
				assert.Contains(t, got.Error(), "failed to clone VM: ")
			}
			hasVMOpInfo := false
			for _, d := range st.Details() {
				if containsReason(d, contracts.VMOperationFailedReason) {
					hasVMOpInfo = true
				}
			}
			assert.Equal(t, tc.vmOpFailed, hasVMOpInfo, "VM_OPERATION_FAILED keeps a per-VM failure out of the breaker")
		})
	}
}

// containsReason reports whether the status detail d is an ErrorInfo with reason.
func containsReason(d any, reason string) bool {
	type reasoner interface{ GetReason() string }
	r, ok := d.(reasoner)
	return ok && r.GetReason() == reason
}

func TestCloneSourceStateReadError(t *testing.T) {
	assert.True(t, contracts.IsRetryable(cloneSourceStateReadError("team-a.web", sshDown())),
		"a host that could not be reached says nothing about the source: retry")
	notFound := cloneSourceStateReadError("team-a.web", &VirshError{Command: "domstate", ExitCode: 1,
		Stderr: "error: failed to get domain 'team-a.web'"})
	assert.True(t, contracts.IsNotFound(notFound), "a host that answered keeps the historical not-found")
}
