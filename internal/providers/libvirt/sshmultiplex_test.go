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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSSHMultiplexingEnabled_EnvParsing verifies multiplexing is ON by default
// and disabled ONLY by the literal word "true" (case-insensitive, trimmed),
// mirroring the other libvirt escape-hatch env semantics (#194).
func TestSSHMultiplexingEnabled_EnvParsing(t *testing.T) {
	cases := []struct {
		name     string
		envSet   bool
		envValue string
		want     bool
	}{
		{"unset (default on)", false, "", true},
		{"empty", true, "", true},
		{"true disables", true, "true", false},
		{"TRUE disables", true, "TRUE", false},
		{"  true  trimmed disables", true, "  true  ", false},
		{"false keeps on", true, "false", true},
		{"1 keeps on", true, "1", true},
		{"yes keeps on", true, "yes", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSet {
				t.Setenv(EnvDisableSSHMultiplexing, tc.envValue)
			} else {
				t.Setenv(EnvDisableSSHMultiplexing, "")
			}
			assert.Equal(t, tc.want, sshMultiplexingEnabled())
		})
	}
}

// TestSSHMultiplexOptions_Enabled verifies the ControlMaster option triplet is
// emitted (as alternating -o k=v pairs) when multiplexing is on (#194).
func TestSSHMultiplexOptions_Enabled(t *testing.T) {
	t.Setenv(EnvDisableSSHMultiplexing, "")
	opts := sshMultiplexOptions()
	joined := strings.Join(opts, " ")
	assert.Contains(t, joined, "ControlMaster=auto")
	assert.Contains(t, joined, "ControlPath=/tmp/virtrigaud-ssh-%C")
	assert.Contains(t, joined, "ControlPersist=60s")
	// Laid out as alternating -o pairs so it spreads into an argv builder.
	assert.Equal(t, 6, len(opts), "expected three -o k=v pairs")
	for i := 0; i < len(opts); i += 2 {
		assert.Equal(t, "-o", opts[i])
	}
}

// TestSSHMultiplexOptions_Disabled verifies the escape hatch yields no options,
// so callers fall back to one SSH connection per command (#194).
func TestSSHMultiplexOptions_Disabled(t *testing.T) {
	t.Setenv(EnvDisableSSHMultiplexing, "true")
	assert.Empty(t, sshMultiplexOptions())
}

// TestSSHConfigStanza_Multiplexing verifies the ControlMaster lines are present
// in the ssh config stanza (used by libvirt's qemu+ssh:// transport) when
// multiplexing is on, and absent when disabled (#194).
func TestSSHConfigStanza_Multiplexing(t *testing.T) {
	p := hostKeyPolicy{}

	t.Setenv(EnvDisableSSHMultiplexing, "")
	on := p.sshConfigStanza()
	assert.Contains(t, on, "ControlMaster auto")
	assert.Contains(t, on, "ControlPath /tmp/virtrigaud-ssh-%C")
	assert.Contains(t, on, "ControlPersist 60s")

	t.Setenv(EnvDisableSSHMultiplexing, "true")
	off := p.sshConfigStanza()
	assert.NotContains(t, off, "ControlMaster")
	// Host-key directives remain regardless of multiplexing.
	assert.Contains(t, off, "StrictHostKeyChecking")
}
