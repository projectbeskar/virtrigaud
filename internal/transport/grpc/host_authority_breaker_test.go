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

package grpc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/clustered"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestHostAuthority_ProviderAnswersNotCountedTowardBreaker pins the
// per-Provider breaker's accounting of the answers a clustered Provider gives
// to a host authority call (provider.v1.HostAuthority): every failure it
// reports for one host (unreachable, unknown or open-breaker host, an
// unknown outcome, an agent too old to serve the service) is kept out of the
// breaker, so that one bad host cannot fast-fail every other host of the
// Provider. A bare answered Unavailable still counts: that is why the Provider
// never relays one for a host authority call.
func TestHostAuthority_ProviderAnswersNotCountedTowardBreaker(t *testing.T) {
	withInfo := func(code codes.Code, reason string, md map[string]string) error {
		st, err := status.New(code, "host \"host-a\"").WithDetails(&errdetails.ErrorInfo{Reason: reason,
			Domain: contracts.ErrorInfoDomain, Metadata: md})
		require.NoError(t, err)
		return st.Err()
	}
	methods := []string{
		providerv1.HostAuthority_Challenge_FullMethodName,
		providerv1.HostAuthority_Attest_FullMethodName,
		providerv1.HostAuthority_LedgerMark_FullMethodName,
		providerv1.HostAuthority_RetireIfUnknown_FullMethodName,
	}
	exempt := map[string]error{
		"host unavailable": withInfo(codes.Unavailable, contracts.HostUnavailableReason,
			map[string]string{contracts.HostUnavailableCauseKey: "AgentUnreachable", contracts.HostUnavailableHostKey: "host-a"}),
		"outcome unknown": clustered.OutcomeUnknownRetryStatus("host \"host-a\" stopped answering"),
		"agent too old": withInfo(codes.Unimplemented, contracts.HostAuthorityUnsupportedReason,
			map[string]string{contracts.HostUnavailableHostKey: "host-a"}),
		"not served": status.Error(codes.Unimplemented, "unknown service provider.v1.HostAuthority"),
	}
	for _, m := range methods {
		for name, err := range exempt {
			assert.False(t, countsTowardBreaker(m, err, true), "%s %s: answered", m, name)
		}
		assert.True(t, countsTowardBreaker(m, status.Error(codes.Unavailable, "down"), true),
			"%s: a bare answered Unavailable counts", m)
	}
}
