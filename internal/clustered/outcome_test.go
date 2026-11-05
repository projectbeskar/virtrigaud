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

package clustered

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// reasonsOf returns the ErrorInfo reasons of err's status, and the metadata of
// the last ErrorInfo.
func reasonsOf(t *testing.T, err error) ([]string, map[string]string) {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "a gRPC status")
	var reasons []string
	var meta map[string]string
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reasons = append(reasons, info.Reason)
			meta = info.Metadata
		}
	}
	return reasons, meta
}

// staticSet is a HostSet over a fixed host list.
type staticSet []HostID

func (s staticSet) Hosts() []HostID { return s }

func TestFronts(t *testing.T) {
	set := staticSet{"host-a", "host-b"}
	assert.True(t, Fronts(set, "host-a"))
	assert.False(t, Fronts(set, "host-c"))
	assert.False(t, Fronts(staticSet{}, "host-a"))
}

func TestTaskRefAnswers(t *testing.T) {
	pe := NewTaskHostNotFrontedError("host-z")
	assert.True(t, contracts.IsNotFound(pe))
	assert.Contains(t, pe.Error(), `"host-z"`)

	err := TypedStatus(&UnknownHostTaskError{Host: "host-a"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	reasons, _ := reasonsOf(t, err)
	assert.Equal(t, []string{contracts.UnknownToAgentReason}, reasons)
}

// TestHostDownStatus_Matrix pins the answer for every kind of RPC at every
// stage of a call whose host stopped answering.
func TestHostDownStatus_Matrix(t *testing.T) {
	hostUnavailable := []string{contracts.HostUnavailableReason}
	lost := []string{contracts.VMOperationFailedReason, contracts.HostConnectionLostReason}
	retry := []string{contracts.OutcomeUnknownRetryReason, contracts.VMOperationFailedReason, contracts.HostConnectionLostReason}
	for _, tc := range []struct {
		name       string
		kind       OpKind
		stage      HostDownStage
		failClosed bool
		code       codes.Code
		reasons    []string
	}{
		{"write, nothing ran", OpWrite, HostDownNothingRan, false, codes.Unavailable, hostUnavailable},
		{"write, reads answered", OpWrite, HostDownReadsOnly, false, codes.Unavailable, lost},
		{"write, write sent", OpWrite, HostDownWriteSent, false, codes.Unknown, lost},
		{"retry, reads answered", OpWriteRetry, HostDownReadsOnly, false, codes.Unavailable, lost},
		{"retry, write sent", OpWriteRetry, HostDownWriteSent, false, codes.Unavailable, retry},
		{"read, write sent", OpRead, HostDownWriteSent, false, codes.Unavailable, hostUnavailable},
		{"fail closed", OpWrite, HostDownWriteSent, true, codes.Unavailable, hostUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := HostDownStatus(tc.kind, "do it", "host-b", tc.stage, tc.failClosed)
			assert.Equal(t, tc.code, status.Code(err))
			reasons, meta := reasonsOf(t, err)
			assert.Equal(t, tc.reasons, reasons)
			assert.Contains(t, status.Convert(err).Message(), `"host-b"`)
			if tc.reasons[0] == contracts.HostUnavailableReason {
				assert.Equal(t, "host-b", meta[contracts.HostUnavailableHostKey])
			}
		})
	}
}

func TestHostOpFailedStatus(t *testing.T) {
	err := HostOpFailedStatus("do it", "host-b", "")
	assert.Equal(t, codes.Unknown, status.Code(err))
	assert.Equal(t, `failed to do it on host "host-b"`, status.Convert(err).Message())
	err = HostOpFailedStatus("do it", "host-b", "nothing was changed")
	assert.Equal(t, `failed to do it on host "host-b": nothing was changed`, status.Convert(err).Message())
	reasons, _ := reasonsOf(t, err)
	assert.Equal(t, []string{contracts.VMOperationFailedReason}, reasons)
}

func TestTypedStatus(t *testing.T) {
	rh := &HostUnavailableError{Host: "host-b",
		PE: contracts.NewHostUnavailableError(`connect to host "host-b"`, errors.New("dial 10.0.0.5:22"))}
	err := TypedStatus(rh)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.NotContains(t, status.Convert(err).Message(), "10.0.0.5")
	_, meta := reasonsOf(t, err)
	assert.Equal(t, "host-b", meta[contracts.HostUnavailableHostKey])
	assert.True(t, contracts.IsHostUnavailable(rh), "transparent to the contracts classifiers")

	for pe, code := range map[*contracts.ProviderError]codes.Code{
		contracts.NewInvalidSpecError("x", nil):     codes.InvalidArgument,
		contracts.NewHostUnavailableError("x", nil): codes.Unavailable,
		contracts.NewNotFoundError("x", nil):        codes.NotFound,
		contracts.NewConflictError("x", nil):        codes.AlreadyExists,
		contracts.NewUnavailableError("x", nil):     codes.OK,
		contracts.NewRetryableError("x", nil):       codes.OK,
		contracts.NewNotSupportedError("x"):         codes.OK,
	} {
		got := TypedStatus(pe)
		if code == codes.OK {
			assert.NoError(t, got, "%s is not categorized", pe.Type)
			continue
		}
		assert.Equal(t, code, status.Code(got), "%s", pe.Type)
	}
	assert.NoError(t, TypedStatus(errors.New("plain")))
}

func TestProviderFailureStatus(t *testing.T) {
	err := ProviderFailureStatus("do it", contracts.NewUnavailableError("registry not initialized", nil))
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, "registry not initialized", status.Convert(err).Message())
	err = ProviderFailureStatus("do it", errors.New("ssh: 10.0.0.5"))
	assert.Equal(t, codes.Unknown, status.Code(err))
	assert.Equal(t, "failed to do it", status.Convert(err).Message())
}

func TestOpError(t *testing.T) {
	cause := errors.New("cause")
	e := &OpError{Code: codes.Unavailable, Wire: "in progress", Cause: cause}
	assert.Equal(t, "in progress: cause", e.Error())
	assert.ErrorIs(t, e, cause)
	assert.Equal(t, "in progress", (&OpError{Wire: "in progress"}).Error())
}
