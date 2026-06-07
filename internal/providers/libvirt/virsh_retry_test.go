/*
Copyright 2025.

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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransientSSHConnectError(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"kex_exchange_identification", "kex_exchange_identification: Connection closed by remote host", true},
		{"connection refused", "ssh: connect to host 10.0.0.1 port 22: Connection refused", true},
		{"connection reset", "client_loop: send disconnect: Connection reset by peer", true},
		{"connection timed out", "ssh: connect to host h port 22: Connection timed out", true},
		{"no route to host", "ssh: connect to host h port 22: No route to host", true},
		{"dns transient", "ssh: Could not resolve hostname h: Temporary failure in name resolution", true},
		{"case-insensitive", "KEX_EXCHANGE_IDENTIFICATION: Connection Closed By Remote Host", true},
		// Real virsh errors must NOT be treated as transient.
		{"domain not found", "error: failed to get domain 'vm1'\nerror: Domain not found", false},
		{"permission denied", "Permission denied, please try again.", false},
		{"empty", "", false},
		{"generic virsh error", "error: invalid argument: could not find capabilities", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, transientSSHConnectError(tc.stderr))
		})
	}
}

// withFastBackoff shrinks the retry backoff for the duration of a test.
func withFastBackoff(t *testing.T) {
	t.Helper()
	orig := sshConnectBaseBackoff
	sshConnectBaseBackoff = time.Millisecond
	t.Cleanup(func() { sshConnectBaseBackoff = orig })
}

func TestRetryOnTransientSSH_RetriesThenSucceeds(t *testing.T) {
	withFastBackoff(t)
	calls := 0
	res, err := retryOnTransientSSH(context.Background(), func() (*VirshResult, error) {
		calls++
		if calls < 3 {
			return &VirshResult{Stderr: "kex_exchange_identification: Connection closed by remote host"},
				&VirshError{Stderr: "kex_exchange_identification: Connection closed by remote host"}
		}
		return &VirshResult{Stdout: "ok"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", res.Stdout)
	assert.Equal(t, 3, calls, "should retry transient failures until success")
}

func TestRetryOnTransientSSH_RealErrorNoRetry(t *testing.T) {
	withFastBackoff(t)
	calls := 0
	_, err := retryOnTransientSSH(context.Background(), func() (*VirshResult, error) {
		calls++
		return &VirshResult{Stderr: "error: Domain not found"},
			&VirshError{Stderr: "error: Domain not found"}
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls, "real virsh errors must not be retried")
}

func TestRetryOnTransientSSH_ExhaustsAttempts(t *testing.T) {
	withFastBackoff(t)
	calls := 0
	_, err := retryOnTransientSSH(context.Background(), func() (*VirshResult, error) {
		calls++
		return &VirshResult{Stderr: "kex_exchange_identification: Connection closed by remote host"},
			&VirshError{Stderr: "kex_exchange_identification: Connection closed by remote host"}
	})
	require.Error(t, err)
	assert.Equal(t, sshConnectMaxAttempts, calls, "should give up after the bounded number of attempts")
}

func TestRetryOnTransientSSH_ContextCancelStops(t *testing.T) {
	withFastBackoff(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	calls := 0
	_, err := retryOnTransientSSH(ctx, func() (*VirshResult, error) {
		calls++
		return &VirshResult{Stderr: "kex_exchange_identification: Connection closed by remote host"},
			&VirshError{Stderr: "kex_exchange_identification: Connection closed by remote host"}
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls, "a cancelled context must stop retries after the first attempt")
}
