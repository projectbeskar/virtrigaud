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
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
)

// These tests pin the ADR-0007 Slice 5 lab follow-up: a clustered clone whose
// host answered a read with an error BEFORE the copy started — the owner
// check, the source's state, its definition, the pool — is retried (Unavailable
// + VM_OPERATION_FAILED), not failed (Unknown), while a real refusal stays
// terminal and a failure after the copy started stays a failure.

func TestClustered_Clone_TransientReadBeforeTheCopyIsRetryable(t *testing.T) {
	for _, what := range []string{"domstate", "dumpxml", "pool-info"} {
		t.Run(what, func(t *testing.T) {
			fx := sourceWeb(t, nil)
			fx.script("host-b", "fail-"+what, "")
			_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
			assertRoutedOp(t, err, codes.Unavailable, `clone VM on host "host-b": a read on the host failed before anything was copied`)
			st, _ := status.FromError(err)
			assert.NotContains(t, st.Message(), "scripted failure", "the host's output stays in the provider log")
			assertNoCopy(t, fx.calls())
		})
	}
}

func TestClustered_Clone_RefusalsBeforeTheCopyStayTerminal(t *testing.T) {
	t.Run("the source is not the requester's", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		req := routedCloneReq()
		req.SourceOwner = teamBOwner
		_, err := NewServer(fx.p).Clone(context.Background(), req)
		assert.Equal(t, codes.NotFound, status.Code(err), "got %v", err)
	})
	t.Run("the target name is taken", func(t *testing.T) {
		fx := sourceWeb(t, map[string]string{cloneTargetDomain: scdDomainXML(cloneTargetDomain, scdDomainOpts{owner: ownerTeamB})})
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		assert.Equal(t, codes.AlreadyExists, status.Code(err), "got %v", err)
	})
	t.Run("the source is running", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		fx.script("host-b", "state", "running\n")
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		assert.Equal(t, codes.FailedPrecondition, status.Code(err), "got %v", err)
	})
	t.Run("the copy itself failed", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		failQemuImgConvert(t, fx)
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		assertRoutedOp(t, err, codes.Unknown, `failed to clone VM on host "host-b"`)
	})
}

func TestCloneBeforeCopyFailure(t *testing.T) {
	const host = "host-b"
	answered := &VirshError{Command: "virsh domstate x", ExitCode: 1, Stderr: "error: cannot acquire state change lock"}
	transient := map[string]error{
		"a retryable provider error":      contracts.NewRetryableError("failed to dump source domain XML", answered),
		"an unavailable provider error":   contracts.NewUnavailableError("busy", nil),
		"a wrapped untyped host error":    fmt.Errorf("get pool %q info: %w", "default", answered),
		"a host error inside hostOpError": &hostOpError{host: host, err: answered},
	}
	for name, err := range transient {
		t.Run(name, func(t *testing.T) {
			got := cloneBeforeCopyFailure(host, err)
			var roe *routedOpError
			require.ErrorAs(t, got, &roe)
			assert.Equal(t, codes.Unavailable, roe.code)
			assert.ErrorIs(t, got, err, "the cause is kept for the log")
		})
	}
	unchanged := map[string]error{
		"nil":               nil,
		"invalid spec":      contracts.NewInvalidSpecError("multi-disk", nil),
		"not found":         contracts.NewNotFoundError("not owned", nil),
		"conflict":          contracts.NewConflictError("name taken", nil),
		"host unavailable":  contracts.NewHostUnavailableError("connect", nil),
		"host unreachable":  &hostOpError{host: host, err: &VirshError{ExitCode: -1}},
		"source running":    &sourceRunningError{state: "running"},
		"domain busy":       &domainBusyError{op: guardOpClone, domain: cloneTargetDomain},
		"previous":          &previousIncarnationError{op: guardOpClone, domain: cloneTargetDomain},
		"guard incomplete":  &clusterGuardIncompleteError{op: guardOpClone, domain: cloneTargetDomain},
		"disk check failed": &diskCheckFailedError{op: guardOpClone, domain: cloneTargetDomain},
		"budget":            &routedOpError{code: codes.Unknown, wire: "stopped"},
	}
	for name, err := range unchanged {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, err, cloneBeforeCopyFailure(host, err), "a refusal or a categorized answer is kept as it is")
		})
	}
	assert.True(t, errors.Is(cloneBeforeCopyFailure(host, context.Canceled), context.Canceled))
}

// TestClustered_CloneTransientReadOverGRPC_IsRetryableAndNeverCounts: over
// the real gRPC hop, the manager sees a retryable answer (the VMClone waits)
// and its breaker stays closed.
func TestClustered_CloneTransientReadOverGRPC_IsRetryableAndNeverCounts(t *testing.T) {
	fx := sourceWeb(t, nil)
	fx.script("host-b", "fail-domstate", "")
	addr, stop := serveProviderOverTCP(t, NewServer(fx.p))
	defer stop()
	cli, cb := managerClient(t, addr)

	for i := 0; i < 4; i++ {
		_, err := cli.Clone(context.Background(), managerCloneReq())
		require.Error(t, err)
		assert.True(t, contracts.IsRetryable(err), "the VMClone controller retries it: %v", err)
		assert.False(t, contracts.IsVMSourceRunning(err))
		assert.False(t, contracts.IsHostUnavailable(err))
	}
	assert.Equal(t, resilience.StateClosed, cb.GetState())
	assert.Zero(t, cb.GetFailures())
}
