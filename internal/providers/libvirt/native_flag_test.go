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
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParseNativeConfig covers the ADR-0008 D4 flag grammar: [mode:]family entries,
// the shadow default for a bare family, the case/whitespace tolerance, and the
// warn-and-ignore handling of unknown modes/families and duplicates.
func TestParseNativeConfig(t *testing.T) {
	tests := []struct {
		name           string
		raw            string
		wantConfigured map[nativeFamily]nativeMode
		wantWarnings   int
	}{
		{
			name:           "empty is all-off (pure virsh, zero behavior change)",
			raw:            "",
			wantConfigured: map[nativeFamily]nativeMode{},
		},
		{
			name:           "explicit shadow:describe",
			raw:            "shadow:describe",
			wantConfigured: map[nativeFamily]nativeMode{familyDescribe: modeShadow},
		},
		{
			name:           "bare family defaults to shadow",
			raw:            "describe",
			wantConfigured: map[nativeFamily]nativeMode{familyDescribe: modeShadow},
		},
		{
			name:           "native:describe is accepted by the grammar (configured native)",
			raw:            "native:describe",
			wantConfigured: map[nativeFamily]nativeMode{familyDescribe: modeNative},
		},
		{
			name:           "off:describe",
			raw:            "off:describe",
			wantConfigured: map[nativeFamily]nativeMode{familyDescribe: modeOff},
		},
		{
			name:           "case-insensitive and whitespace-tolerant",
			raw:            "  SHADOW : Describe  ",
			wantConfigured: map[nativeFamily]nativeMode{familyDescribe: modeShadow},
		},
		{
			name:           "unknown family is warned and ignored",
			raw:            "shadow:lifecycle",
			wantConfigured: map[nativeFamily]nativeMode{},
			wantWarnings:   1,
		},
		{
			name:           "unknown mode is warned and ignored",
			raw:            "bogus:describe",
			wantConfigured: map[nativeFamily]nativeMode{},
			wantWarnings:   1,
		},
		{
			name:           "duplicate family: last wins, with a warning",
			raw:            "off:describe,shadow:describe",
			wantConfigured: map[nativeFamily]nativeMode{familyDescribe: modeShadow},
			wantWarnings:   1,
		},
		{
			name:           "mixed valid + unknown: valid kept, unknown warned",
			raw:            "shadow:describe,shadow:snapshot",
			wantConfigured: map[nativeFamily]nativeMode{familyDescribe: modeShadow},
			wantWarnings:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, warnings := parseNativeConfig(tt.raw)
			assert.Len(t, warnings, tt.wantWarnings, "warning count")
			for fam, want := range tt.wantConfigured {
				assert.Equal(t, want, cfg.configuredMode(fam), "configured mode for %q", fam)
			}
			// Families not in the expected map must be off.
			for _, fam := range knownFamilies {
				if _, expected := tt.wantConfigured[fam]; !expected {
					assert.Equal(t, modeOff, cfg.configuredMode(fam), "unexpected mode for %q", fam)
				}
			}
		})
	}
}

// TestEffectiveModeDowngradesNativeToShadow is the load-bearing PR 4b invariant:
// a family configured native runs as SHADOW here (reads do not flip until PR 5),
// while off and shadow pass through unchanged. PR 5 will remove only this
// downgrade — the grammar is already what it flips.
func TestEffectiveModeDowngradesNativeToShadow(t *testing.T) {
	native, _ := parseNativeConfig("native:describe")
	assert.Equal(t, modeNative, native.configuredMode(familyDescribe), "configured stays native for the audit signal")
	assert.Equal(t, modeShadow, native.effectiveMode(familyDescribe), "effective downgrades native->shadow in PR 4b")

	shadow, _ := parseNativeConfig("shadow:describe")
	assert.Equal(t, modeShadow, shadow.effectiveMode(familyDescribe))

	off, _ := parseNativeConfig("off:describe")
	assert.Equal(t, modeOff, off.effectiveMode(familyDescribe))

	unset, _ := parseNativeConfig("")
	assert.Equal(t, modeOff, unset.effectiveMode(familyDescribe), "unset family is off")
}

// TestNativeModeString verifies the mode->driver-token mapping used by the
// active-driver metric and startup log.
func TestNativeModeString(t *testing.T) {
	assert.Equal(t, "virsh", modeOff.String())
	assert.Equal(t, "shadow", modeShadow.String())
	assert.Equal(t, "native", modeNative.String())
}

// TestSampler verifies the sampling knob: default (n<=1) shadows every call; n>1
// shadows exactly 1 in n.
func TestSampler(t *testing.T) {
	// Default: every call.
	every := &sampler{n: 1}
	for i := 0; i < 5; i++ {
		assert.True(t, every.sample(), "n=1 shadows every call")
	}

	// nil is safe and shadows every call.
	var nilS *sampler
	assert.True(t, nilS.sample())

	// 1 in 3.
	third := &sampler{n: 3}
	got := 0
	for i := 0; i < 30; i++ {
		if third.sample() {
			got++
		}
	}
	assert.Equal(t, 10, got, "n=3 shadows exactly 1 in 3 over 30 calls")
}

// TestNewSamplerFromEnv verifies env parsing and its warn-on-invalid behavior.
func TestNewSamplerFromEnv(t *testing.T) {
	t.Setenv("VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE", "4")
	s, warn := newSamplerFromEnv()
	assert.Empty(t, warn)
	assert.Equal(t, uint64(4), s.n)

	t.Setenv("VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE", "not-a-number")
	s, warn = newSamplerFromEnv()
	assert.NotEmpty(t, warn, "invalid value warns")
	assert.Equal(t, uint64(1), s.n, "invalid value falls back to every call")

	t.Setenv("VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE", "0")
	s, warn = newSamplerFromEnv()
	assert.NotEmpty(t, warn, "zero warns")
	assert.Equal(t, uint64(1), s.n)
}
