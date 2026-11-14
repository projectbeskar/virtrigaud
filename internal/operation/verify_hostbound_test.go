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

package operation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestVerifyHostBound_LeavesProviderAndEnrolmentToTheCaller: a grant naming
// another Provider or enrolment passes VerifyHostBound (the caller answers a
// known op_id first and compares both itself) and is refused by Verify; a
// grant for another host or installation is refused by both; every other rule
// is unchanged.
func TestVerifyHostBound_LeavesProviderAndEnrolmentToTheCaller(t *testing.T) {
	pki := validPKI(t)
	v := testVerifier(t, pki)
	kind := providerv1.OperationKind_OPERATION_KIND_POWER_ON
	for name, tc := range map[string]struct {
		edit       func(c *call)
		hostBound  bool // VerifyHostBound accepts
		wantReason string
	}{
		"valid":             {edit: func(*call) {}, hostBound: true},
		"another Provider":  {edit: func(c *call) { c.body.ProviderUid = "other" }, hostBound: true},
		"another enrolment": {edit: func(c *call) { c.body.EnrolmentId = bytesOf(0xE2, IDLen) }, hostBound: true},
		"another host":      {edit: func(c *call) { c.body.HostId = "host-b" }, wantReason: contracts.GrantMismatchReason},
		"another install":   {edit: func(c *call) { c.body.InstallId = bytesOf(0x33, IDLen) }, wantReason: contracts.GrantMismatchReason},
		"wrong RPC":         {edit: func(c *call) { c.method = providerv1.Provider_Delete_FullMethodName }, wantReason: contracts.GrantMismatchReason},
		"digest":            {edit: func(c *call) { c.keepDigest = true; c.body.IntentDigest = bytesOf(1, DigestLen) }, wantReason: contracts.GrantMismatchReason},
		"forged":            {edit: func(c *call) { c.sigMutate = func(b []byte) []byte { b[len(b)-1] ^= 1; return b } }, wantReason: contracts.GrantUnauthenticatedReason},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCall(t, pki, kind)
			tc.edit(c)
			outer := c.outer(t, pki)
			got, err := v.VerifyHostBound(c.method, outer, nil)
			if tc.hostBound {
				require.NoError(t, err)
				assert.Equal(t, c.request, got.RequestBytes)
				_, full := v.Verify(c.method, outer)
				if name == "valid" {
					assert.NoError(t, full)
				} else {
					r, ok := AsRefusal(full)
					require.True(t, ok, "Verify refuses what VerifyHostBound leaves to the caller")
					assert.Equal(t, contracts.GrantMismatchReason, r.Reason)
					assert.Equal(t, RuleBinding, r.Rule)
				}
				return
			}
			r, ok := AsRefusal(err)
			require.True(t, ok, "refused: %v", err)
			assert.Equal(t, tc.wantReason, r.Reason)
		})
	}
}
