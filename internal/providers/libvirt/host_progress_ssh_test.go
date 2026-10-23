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
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/knownhosts"
)

// TestHostProgress_OverSSH (R25 L2) drives the real SSH client: a write whose
// dial failed ran nothing, so the call stays "nothing ran" (HOST_UNAVAILABLE);
// one whose session was opened counts as sent, whatever its result.
func TestHostProgress_OverSSH(t *testing.T) {
	t.Run("the dial fails: a write ran nothing", func(t *testing.T) {
		// A port nothing listens on.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		require.NoError(t, ln.Close())
		useTempKnownHosts(t, "")

		v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
		t.Cleanup(func() { _ = v.Cleanup() })
		ctx, p := withHostProgress(t.Context())
		_, err = v.runVirshCommand(ctx, "destroy", "web")
		require.Error(t, err)
		assert.True(t, isHostTransportFailure(err), "a dial failure is a lost connection: %v", err)
		assert.Equal(t, hostDownNothingRan, p.stage(), "nothing could have run: HOST_UNAVAILABLE")
	})

	t.Run("a session is opened: a write was sent", func(t *testing.T) {
		hostKey := generateTestHostKey(t)
		addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
		useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")
		v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
		t.Cleanup(func() { _ = v.Cleanup() })

		ctx, p := withHostProgress(t.Context())
		_, err := v.runVirshCommand(ctx, "!", "test", "-e", "/")
		require.NoError(t, err)
		assert.Equal(t, hostDownReadsOnly, p.stage(), "a read over an open session is not a write")

		_, err = v.runVirshCommand(ctx, "!", "true")
		require.NoError(t, err)
		assert.Equal(t, hostDownWriteSent, p.stage())
	})
}
