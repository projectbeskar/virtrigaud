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
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// This file implements the ADR-0008 D4 per-family driver flag,
// VIRTRIGAUD_LIBVIRT_NATIVE, and the ADR-0008 D6 shadow sampling knob. The flag is
// an env var on the provider Deployment (never a Provider.spec field: a transient
// transition flag must not pollute the stable v1beta1 API), reaching the pod
// through the existing generic `spec.runtime.env` passthrough the provider
// controller already appends verbatim. Empty/unset = pure virsh, zero behavior
// change.
//
// # Grammar
//
//	VIRTRIGAUD_LIBVIRT_NATIVE = entry ("," entry)*
//	entry                     = [ mode ":" ] family
//	mode                      = "off" | "shadow" | "native"     (default: shadow)
//	family                    = "describe" | "list"             (the families in PR 4b/4c)
//
// Whitespace-tolerant and case-insensitive. A bare family (no "mode:") defaults to
// SHADOW — the safe transition default: you cannot accidentally flip a read to
// native by naming a family; you must ask for it explicitly with "native:".
// Unknown families and unknown modes are warned-and-ignored (never fatal), so an
// older provider binary tolerates a config naming a family a newer one adds.
//
// # Modes, and the PR 4b downgrade
//
//   - off    — pure virsh; go-libvirt stays dormant for this family.
//   - shadow — run BOTH drivers, return virsh's answer, meter divergence (PR 4b).
//   - native — return go-libvirt's answer. NOT reachable for reads in PR 4b: reads
//     never flip here (that is PR 5, gated on D5). A family configured native is
//     DOWNGRADED to shadow with a loud startup warning, so the grammar PR 5 flips
//     is already the grammar operators write today — PR 5 changes one line
//     (effectiveMode stops downgrading), not the schema.
//
// The nativeMode a config records is the CONFIGURED mode; effectiveMode applies
// the downgrade. The active-driver metric and the startup log both report the
// EFFECTIVE driver, so the audit signal is what is actually happening, not merely
// what was asked for.

// nativeMode is the configured driver mode for one operation family.
type nativeMode int

const (
	// modeOff runs pure virsh for the family (go-libvirt dormant). The zero value,
	// so an unconfigured family is off.
	modeOff nativeMode = iota
	// modeShadow runs both drivers and returns virsh's answer, metering divergence.
	modeShadow
	// modeNative returns go-libvirt's answer. Not reachable for reads in PR 4b
	// (downgraded to modeShadow by effectiveMode); the flip is ADR-0008 PR 5.
	modeNative
)

// String renders a mode as its metrics/log driver token.
func (m nativeMode) String() string {
	switch m {
	case modeShadow:
		return metrics.DriverShadow
	case modeNative:
		return metrics.DriverNative
	default:
		return metrics.DriverVirsh
	}
}

// nativeFamily identifies an operation family the driver flag can gate.
type nativeFamily string

const (
	// familyDescribe is the side-effect-free "Describe first" read family — the
	// first family ADR-0008 wired through the shadow harness (PR 4b).
	familyDescribe nativeFamily = "describe"
	// familyList is the ListVMs read family — the second family shadowed (PR 4c),
	// soaking in parallel with describe. Like describe it never flips to native
	// here: effectiveMode downgrades native to shadow, and the flip stays PR 5,
	// gated on the D5 divergence soak.
	familyList nativeFamily = "list"
)

// knownFamilies is the set of families this binary understands. A config naming a
// family not in here is warned-and-ignored. Grows one entry per ported family.
// effectiveMode, loadNativeConfig (the active-driver metric loop), and the PR 4b
// native->shadow downgrade all iterate this slice, so a new family is picked up
// everywhere by appending it here — no per-family special-casing.
var knownFamilies = []nativeFamily{familyDescribe, familyList}

// nativeConfig is the parsed VIRTRIGAUD_LIBVIRT_NATIVE flag: the CONFIGURED mode
// per family (default modeOff). It is immutable after construction (the env var is
// read once at startup), so it is safe to share without locking.
type nativeConfig struct {
	modes map[nativeFamily]nativeMode
}

// parseNativeConfig parses the flag value into a nativeConfig, returning any
// non-fatal warnings (unknown family, unknown mode, duplicate family) for the
// caller to log. It never errors: a malformed entry is skipped with a warning so a
// typo can never crash the provider or silently change more than the typo'd entry.
func parseNativeConfig(raw string) (nativeConfig, []string) {
	cfg := nativeConfig{modes: make(map[nativeFamily]nativeMode)}
	var warnings []string

	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		mode := modeShadow // bare family defaults to shadow (safe transition default)
		famToken := entry
		if colon := strings.IndexByte(entry, ':'); colon >= 0 {
			modeToken := strings.ToLower(strings.TrimSpace(entry[:colon]))
			famToken = strings.TrimSpace(entry[colon+1:])
			parsed, ok := parseMode(modeToken)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("unknown mode %q in %q (want off|shadow|native); ignoring entry", modeToken, entry))
				continue
			}
			mode = parsed
		}

		fam := nativeFamily(strings.ToLower(famToken))
		if !isKnownFamily(fam) {
			warnings = append(warnings, fmt.Sprintf("unknown family %q (want one of %s); ignoring entry", famToken, familiesList()))
			continue
		}
		if _, dup := cfg.modes[fam]; dup {
			warnings = append(warnings, fmt.Sprintf("family %q set more than once; last wins", fam))
		}
		cfg.modes[fam] = mode
	}

	return cfg, warnings
}

// parseMode maps a lowercased mode token to a nativeMode.
func parseMode(token string) (nativeMode, bool) {
	switch token {
	case "off":
		return modeOff, true
	case "shadow":
		return modeShadow, true
	case "native":
		return modeNative, true
	default:
		return modeOff, false
	}
}

// configuredMode returns the mode the operator asked for (before any PR 4b
// downgrade), defaulting to modeOff for an unconfigured family.
func (c nativeConfig) configuredMode(fam nativeFamily) nativeMode {
	if c.modes == nil {
		return modeOff
	}
	return c.modes[fam]
}

// effectiveMode returns the mode the provider will ACTUALLY run for fam. It
// applies the ADR-0008 PR 4b constraint that reads never flip to native: a family
// configured modeNative runs as modeShadow here. PR 5 removes this downgrade (the
// one-line flip) so the same env grammar then returns go-libvirt's answer.
func (c nativeConfig) effectiveMode(fam nativeFamily) nativeMode {
	m := c.configuredMode(fam)
	if m == modeNative {
		// Reads do not flip in PR 4b — observe only. See ADR-0008 D6 / PR 5.
		return modeShadow
	}
	return m
}

// loadNativeConfig reads VIRTRIGAUD_LIBVIRT_NATIVE, logs the parse warnings and a
// startup summary of the effective driver per family, publishes the active-driver
// metric for every known family, and returns the parsed config. It is the single
// place the ADR-0008 D4 "audit via status/metric + a startup log, not spec" signal
// is emitted.
func loadNativeConfig(logger *slog.Logger) nativeConfig {
	if logger == nil {
		logger = slog.Default()
	}
	raw := os.Getenv("VIRTRIGAUD_LIBVIRT_NATIVE")
	cfg, warnings := parseNativeConfig(raw)
	for _, w := range warnings {
		logger.Warn("VIRTRIGAUD_LIBVIRT_NATIVE: "+w, "value", raw)
	}

	for _, fam := range knownFamilies {
		configured := cfg.configuredMode(fam)
		effective := cfg.effectiveMode(fam)
		metrics.SetLibvirtActiveDriver(string(fam), effective.String())

		switch {
		case configured == modeNative && effective != modeNative:
			// Loud: the operator asked for native but PR 4b runs it as shadow.
			logger.Warn("libvirt driver: native mode requested but reads do not flip in this build; running SHADOW (ADR-0008 PR 4b; the flip is PR 5, gated on the D5 divergence soak)",
				"family", fam, "configured", metrics.DriverNative, "effective", effective.String())
		case effective == modeShadow:
			logger.Info("libvirt driver: shadow-compare enabled — both drivers run, virsh's answer is returned, divergence is metered (ADR-0008 PR 4b)",
				"family", fam, "driver", effective.String())
		default:
			logger.Info("libvirt driver active", "family", fam, "driver", effective.String())
		}
	}
	return cfg
}

// isKnownFamily reports whether fam is a family this binary understands.
func isKnownFamily(fam nativeFamily) bool {
	for _, f := range knownFamilies {
		if f == fam {
			return true
		}
	}
	return false
}

// familiesList renders knownFamilies for a warning message, sorted.
func familiesList() string {
	names := make([]string, 0, len(knownFamilies))
	for _, f := range knownFamilies {
		names = append(names, string(f))
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// sampler decides, for a shadowed read family, whether a given call is shadowed.
// It shadows 1 in every n calls (n<=1 shadows every call). It exists so an
// operator can bound the CPU cost of shadowing a hot read on a large VM population
// (ADR-0008's cost-awareness note) without an unbounded doubling; the default
// (every call) is right for the documented 14-17 VM production population, where
// the doubling is negligible and full coverage maximizes the D5 evidence.
type sampler struct {
	n uint64
	c atomic.Uint64
}

// newSamplerFromEnv builds a sampler from VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE (a
// positive integer "shadow 1 in N"); an absent, zero, or invalid value means
// sample every call. It returns any warning for the caller to log.
func newSamplerFromEnv() (*sampler, string) {
	raw := strings.TrimSpace(os.Getenv("VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE"))
	if raw == "" {
		return &sampler{n: 1}, ""
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || n == 0 {
		return &sampler{n: 1}, fmt.Sprintf("ignoring invalid VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE=%q (want positive integer); shadowing every call", raw)
	}
	return &sampler{n: n}, ""
}

// sample reports whether this call should be shadowed. It is safe for concurrent
// use. A nil sampler or n<=1 shadows every call.
func (s *sampler) sample() bool {
	if s == nil || s.n <= 1 {
		return true
	}
	return s.c.Add(1)%s.n == 0
}
