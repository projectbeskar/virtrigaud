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
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"

	obsmetrics "github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// This file is the ADR-0008 PR 4b shadow-compare harness for read operations. It
// is where go-libvirt finally does real work, safely, under the D5 gate:
//
//   - virsh remains AUTHORITATIVE — its answer is ALWAYS what the caller receives.
//   - go-libvirt runs in SHADOW — observed and metered only, never returned.
//   - Both answers are reduced to a canonical projection and compared field by
//     field; a semantic divergence increments
//     virtrigaud_libvirt_shadow_divergence_total{family,field}.
//   - The shadow path is isolated: a go-libvirt error or a panic is recovered,
//     logged, and metered, NEVER propagated. Shadow cannot affect correctness or
//     availability (it runs on a detached, time-bounded goroutine after virsh has
//     already answered).
//
// Reads do NOT flip to native here. The flip is ADR-0008 PR 5, gated on this
// metric reading 0 semantic divergences across the D5 soak window.

const (
	// shadowDescribeDefaultTimeout bounds one shadow Describe (the go-libvirt
	// DomainGetXMLDesc + DomainGetState under the connection watchdog). The shadow
	// runs on a detached goroutine after virsh answered, so this bounds only the
	// shadow's own lifetime, never the caller. Overridable with
	// VIRTRIGAUD_LIBVIRT_SHADOW_TIMEOUT (a Go duration).
	shadowDescribeDefaultTimeout = 10 * time.Second
)

// describeFieldCmp compares one field between the virsh (authoritative) and
// go-libvirt (shadow) DescribeResponses. Each extractor returns the field's
// CANONICAL string form and whether the driver produced it; canonicalization
// (trim, lowercasing, numeric normalization, unit conversion) is what excludes
// non-semantic differences (whitespace, formatting, field order) so only meaning
// is compared, per the ADR-0008 D5 definition of "semantic divergence".
//
// A field is compared only when BOTH drivers produced it. If either is absent it
// is skipped, not counted as a divergence: PR 4b's native path derives its
// projection from DomainGetXMLDesc (+ DomainGetState) alone, so fields virsh
// enriches from the guest agent (live IPs, guest OS, filesystems, users) or that a
// given libvirt version omits are simply not in scope for this family's native
// implementation — comparing them would meter phantom drift, not real drift.
type describeFieldCmp struct {
	field  string
	virsh  func(contracts.DescribeResponse) (string, bool)
	native func(contracts.DescribeResponse) (string, bool)
}

// describeFields is the canonical comparison projection for the describe family.
// It covers exactly the fields both drivers derive from the same data class — the
// domain definition XML plus the coarse runtime state — and includes the two
// parity cases ADR-0008 / PR #291 call out explicitly: power-state coarsening
// (#291 M2) and the memory-unit guard (D2). Live IPs, console URL, and the
// guest-agent enrichment are intentionally excluded (see describeFieldCmp).
var describeFields = []describeFieldCmp{
	{
		field:  "exists",
		virsh:  func(r contracts.DescribeResponse) (string, bool) { return strconv.FormatBool(r.Exists), true },
		native: func(r contracts.DescribeResponse) (string, bool) { return strconv.FormatBool(r.Exists), true },
	},
	{
		// #291 M2: both drivers coarsen non-running states (paused, blocked,
		// pmsuspended, ...) to "Off"; this asserts they still agree after the port.
		field:  "power_state",
		virsh:  func(r contracts.DescribeResponse) (string, bool) { return canonLower(r.PowerState) },
		native: func(r contracts.DescribeResponse) (string, bool) { return canonLower(r.PowerState) },
	},
	{
		field:  "uuid",
		virsh:  func(r contracts.DescribeResponse) (string, bool) { return canonLower(r.ProviderRaw["UUID"]) },
		native: func(r contracts.DescribeResponse) (string, bool) { return canonLower(r.ProviderRaw["uuid"]) },
	},
	{
		field:  "name",
		virsh:  func(r contracts.DescribeResponse) (string, bool) { return canonTrim(r.ProviderRaw["Name"]) },
		native: func(r contracts.DescribeResponse) (string, bool) { return canonTrim(r.ProviderRaw["name"]) },
	},
	{
		field:  "vcpu",
		virsh:  func(r contracts.DescribeResponse) (string, bool) { return canonInt(r.ProviderRaw["CPU(s)"]) },
		native: func(r contracts.DescribeResponse) (string, bool) { return canonInt(r.ProviderRaw["vcpu"]) },
	},
	{
		// D2 memory-unit guard: virsh dominfo reports "Max memory" in KiB
		// ("4194304 KiB"); the native path reports MiB. Both are normalized to MiB
		// so a semantic mismatch (not a unit/format one) is what counts.
		field:  "memory_mib",
		virsh:  func(r contracts.DescribeResponse) (string, bool) { return canonKiBToMiB(r.ProviderRaw["Max memory"]) },
		native: func(r contracts.DescribeResponse) (string, bool) { return canonInt(r.ProviderRaw["memory_mib"]) },
	},
}

// compareDescribe returns the names of the fields that SEMANTICALLY diverge
// between the authoritative virsh response and the shadow go-libvirt response,
// after canonicalization. An empty result means no semantic divergence. It never
// panics and never reads outside the two responses.
func compareDescribe(virsh, native contracts.DescribeResponse) []string {
	var diverged []string
	for _, f := range describeFields {
		vVal, vOK := f.virsh(virsh)
		nVal, nOK := f.native(native)
		if !vOK || !nOK {
			continue // only compare fields both drivers produced
		}
		if vVal != nVal {
			diverged = append(diverged, f.field)
		}
	}
	return diverged
}

// canonTrim returns s trimmed, present iff non-empty.
func canonTrim(s string) (string, bool) {
	s = strings.TrimSpace(s)
	return s, s != ""
}

// canonLower returns s trimmed and lowercased, present iff non-empty. Used for
// case-insensitive identity/state comparison (UUIDs, "On"/"Off").
func canonLower(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	return s, s != ""
}

// canonInt parses the first whitespace-delimited token of s as a base-10 integer
// and returns its canonical decimal form, present iff it parsed. This normalizes
// "2", " 2 ", and "02" to the same value.
func canonInt(s string) (string, bool) {
	n, ok := firstInt(s)
	if !ok {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

// canonKiBToMiB parses the leading integer of s as KiB (e.g. virsh dominfo's
// "4194304 KiB") and returns it as MiB, present iff it parsed. It applies the same
// integer /1024 the native path's domainMemoryMiB uses, so the two sides normalize
// identically.
func canonKiBToMiB(s string) (string, bool) {
	kib, ok := firstInt(s)
	if !ok {
		return "", false
	}
	return strconv.FormatInt(kib/1024, 10), true
}

// firstInt parses the first whitespace-delimited token of s as a base-10 integer.
func firstInt(s string) (int64, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// mapNativeDomainState coarsens a go-libvirt DomainState to the same "On"/"Off"
// vocabulary the virsh path's mapLibvirtPowerState produces: only DomainRunning is
// "On"; every other state (shutoff, paused, blocked, pmsuspended, shutdown,
// crashed, nostate) folds to "Off". Keeping this coarsening IDENTICAL to virsh's
// is the whole point — it is what makes power_state a 0-divergence field (#291 M2)
// rather than a text->typed drift the shadow harness would flag.
func mapNativeDomainState(state golibvirt.DomainState) string {
	if state == golibvirt.DomainRunning {
		return "On"
	}
	return "Off"
}

// buildNativeDescribe builds a DescribeResponse for id from the go-libvirt client
// alone: DomainGetXMLDesc parsed through libvirtxml (D2) for config identity, plus
// DomainGetState for the coarse power state. It is the pure, host-free core the
// test:// integration test drives directly against a real libvirtd, and that
// Provider.describeNative wraps with the connection watchdog. It populates only
// the ProviderRaw keys the comparator projects (uuid/name/vcpu/memory_mib/
// power_state_mapped) — guest-agent enrichment is out of scope for PR 4b's native
// path (see describeFieldCmp).
func buildNativeDescribe(lv *golibvirt.Libvirt, id string) (contracts.DescribeResponse, error) {
	dom, err := lv.DomainLookupByName(id)
	if err != nil {
		if golibvirt.IsNotFound(err) {
			return contracts.DescribeResponse{
				Exists:      false,
				PowerState:  "Off",
				ProviderRaw: map[string]string{},
			}, nil
		}
		return contracts.DescribeResponse{}, fmt.Errorf("native lookup domain %q: %w", id, err)
	}

	xmlDesc, err := lv.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return contracts.DescribeResponse{}, fmt.Errorf("native DomainGetXMLDesc %q: %w", id, err)
	}
	d, err := parseDomainLibvirtxml(xmlDesc)
	if err != nil {
		return contracts.DescribeResponse{}, fmt.Errorf("native parse domain XML %q: %w", id, err)
	}

	// DomainGetState is side-effect free; a failure degrades power_state to "Off"
	// (the comparator then still compares config identity) rather than failing the
	// whole shadow describe.
	powerState := "Off"
	if stateInt, _, sErr := lv.DomainGetState(dom, 0); sErr == nil {
		powerState = mapNativeDomainState(golibvirt.DomainState(stateInt))
	}

	raw := map[string]string{
		"name":               d.Name,
		"uuid":               d.UUID,
		"power_state_mapped": powerState,
	}
	if d.VCPU != nil {
		raw["vcpu"] = strconv.FormatUint(uint64(d.VCPU.Value), 10)
	}
	if mib, ok, mErr := domainMemoryMiB(d); mErr == nil && ok {
		raw["memory_mib"] = strconv.FormatInt(mib, 10)
	}

	return contracts.DescribeResponse{
		Exists:      true,
		PowerState:  powerState,
		ProviderRaw: raw,
	}, nil
}

// describeNative is the production native Describe: it resolves the host
// connection through the seam and runs buildNativeDescribe under the go-libvirt
// connection watchdog (callLibvirt), so a hung RPC is bounded by ctx and evicts
// the connection instead of blocking forever (ADR-0008 Fact 5). It is the default
// value of Provider.describeNativeFn; unit tests swap that field to script native
// results, errors, and panics without a live libvirtd.
func (p *Provider) describeNative(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	lc, err := p.conn(ctx)
	if err != nil {
		return contracts.DescribeResponse{}, err
	}
	var resp contracts.DescribeResponse
	err = lc.callLibvirt(ctx, func(lv *golibvirt.Libvirt) error {
		var bErr error
		resp, bErr = buildNativeDescribe(lv, id)
		return bErr
	})
	if err != nil {
		return contracts.DescribeResponse{}, err
	}
	return resp, nil
}

// maybeShadowDescribe runs the go-libvirt shadow comparison for a Describe when
// the describe family is in shadow mode and this call is sampled. It returns
// immediately: the comparison runs on a detached, time-bounded goroutine so it
// can never delay or fail the caller, which has already received virsh's answer.
// The goroutine's whole body is panic-recovered and its lifetime is bounded by
// shadowTimeout — that is its cancellation story (it holds no lock, mutates
// nothing, and cannot outlive the timeout); shadowWG lets tests and a future
// graceful shutdown drain any in-flight shadows.
func (p *Provider) maybeShadowDescribe(ctx context.Context, id string, virshResp contracts.DescribeResponse) {
	if p.nativeCfg.effectiveMode(familyDescribe) != modeShadow {
		return
	}
	if !p.shadowSampler.sample() {
		return
	}

	p.shadowWG.Add(1)
	go func() {
		defer p.shadowWG.Done()
		defer func() {
			if r := recover(); r != nil {
				obsmetrics.RecordShadowCompare(string(familyDescribe), obsmetrics.ShadowResultPanic)
				p.shadowLogger().Warn("shadow-compare Describe panicked; recovered, metered, not propagated (caller got virsh's answer)",
					"family", familyDescribe, "id", id, "panic", fmt.Sprintf("%v", r))
			}
		}()

		// Detached from the caller's ctx (which is done once Describe returned) but
		// time-bounded so the shadow cannot run unbounded. ctx values are not needed
		// by the go-libvirt path.
		sctx, cancel := context.WithTimeout(context.Background(), p.shadowTimeout())
		defer cancel()
		p.runShadowDescribe(sctx, id, virshResp)
	}()
}

// runShadowDescribe is the synchronous core of the shadow comparison: obtain the
// native answer, compare it against the authoritative virsh answer, and meter the
// outcome. It NEVER returns anything (the caller already has virsh's answer) and
// isolates go-libvirt errors as metered ShadowResultError. Unit tests call it
// directly (Provider.describeNativeFn scripted) to assert the metering and the
// error isolation without a live libvirtd; maybeShadowDescribe wraps it with the
// goroutine, timeout, and panic recovery.
func (p *Provider) runShadowDescribe(ctx context.Context, id string, virshResp contracts.DescribeResponse) {
	nativeResp, err := p.describeNativeFn(ctx, id)
	if err != nil {
		obsmetrics.RecordShadowCompare(string(familyDescribe), obsmetrics.ShadowResultError)
		p.shadowLogger().Warn("shadow-compare Describe: go-libvirt path failed; metered, not propagated (caller got virsh's answer)",
			"family", familyDescribe, "id", id, "error", err)
		return
	}

	diverged := compareDescribe(virshResp, nativeResp)
	if len(diverged) == 0 {
		obsmetrics.RecordShadowCompare(string(familyDescribe), obsmetrics.ShadowResultEqual)
		return
	}

	obsmetrics.RecordShadowCompare(string(familyDescribe), obsmetrics.ShadowResultDivergent)
	for _, field := range diverged {
		obsmetrics.RecordShadowDivergence(string(familyDescribe), field)
	}
	// Log FIELD NAMES only, never values: a divergence is diagnosed from the metric
	// plus a manual dumpxml, and this keeps VM configuration out of provider logs
	// (compliance posture: never persist resource internals in logs/events).
	p.shadowLogger().Warn("shadow-compare Describe divergence (virsh answer returned; go-libvirt observed only)",
		"family", familyDescribe, "id", id, "fields", strings.Join(diverged, ","))
}

// shadowLogger returns the provider's structured logger, defaulting to
// slog.Default() when unset so the shadow path always logs.
func (p *Provider) shadowLogger() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

// shadowTimeout returns the per-shadow-Describe timeout, defaulting to
// shadowDescribeDefaultTimeout when unset.
func (p *Provider) shadowTimeout() time.Duration {
	if p.shadowTimeoutValue > 0 {
		return p.shadowTimeoutValue
	}
	return shadowDescribeDefaultTimeout
}

// initShadow wires the ADR-0008 PR 4b shadow-compare machinery onto p: it parses
// VIRTRIGAUD_LIBVIRT_NATIVE (logging the effective driver per family and
// publishing the active-driver metric, D4), builds the sampler and per-shadow
// timeout, and installs the real native-describe function. It is called once from
// each constructor. When the flag is unset every family is off and the provider
// behaves exactly as pure virsh — initShadow only reads env and sets fields, it
// never dials go-libvirt (the first shadowed Describe does that, lazily).
func (p *Provider) initShadow(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	p.logger = logger
	p.nativeCfg = loadNativeConfig(logger)

	sampler, warn := newSamplerFromEnv()
	if warn != "" {
		logger.Warn("VIRTRIGAUD_LIBVIRT_SHADOW_SAMPLE: " + warn)
	}
	p.shadowSampler = sampler

	if raw := strings.TrimSpace(os.Getenv("VIRTRIGAUD_LIBVIRT_SHADOW_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			p.shadowTimeoutValue = d
		} else {
			logger.Warn("ignoring invalid VIRTRIGAUD_LIBVIRT_SHADOW_TIMEOUT; using default",
				"value", raw, "default", shadowDescribeDefaultTimeout.String())
		}
	}

	if p.describeNativeFn == nil {
		p.describeNativeFn = p.describeNative
	}
}
