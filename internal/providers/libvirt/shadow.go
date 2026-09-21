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

// membershipField is the divergence field name (scoped by family="list" on the
// metric) recorded when the authoritative virsh list contains a domain the
// go-libvirt list does not — native failed to see a VM virsh saw. See compareList
// for the deliberately one-directional semantics.
const membershipField = "membership"

// This file is the ADR-0008 shadow-compare harness for read operations. It is
// where go-libvirt finally does real work, safely, under the D5 gate:
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
// PR 4b landed the harness and the first read family, describe (Describe). PR 4c
// adds the second read family, list (ListVMs), on the SAME generic wrapper
// (runDetachedShadow), so it soaks in parallel with describe. Reads do NOT flip to
// native here. The flip is ADR-0008 PR 5, gated on this metric reading 0 semantic
// divergences across the D5 soak window.

const (
	// shadowDescribeDefaultTimeout bounds one shadow read's detached goroutine — a
	// Describe (DomainGetXMLDesc + DomainGetState) or a List (ConnectListAllDomains
	// + per-domain DomainGetXMLDesc/State) under the connection watchdog. The shadow
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

// vmListFieldCmp projects one canonical config-identity field out of a
// contracts.VMInfo for the list-family comparator. Unlike describeFieldCmp — where
// virsh and go-libvirt key ProviderRaw DIFFERENTLY, so each field needs a separate
// virsh/native extractor — the list comparator sees both sides as contracts.VMInfo
// (virsh's ListVMs and buildNativeList populate the SAME typed shape), so ONE
// extractor serves both. As in describe, the field is compared only when BOTH sides
// produce it (extractor returns ok); the canonicalization excludes non-semantic
// differences (whitespace, case, leading zeros) so only meaning is compared.
type vmListFieldCmp struct {
	field   string
	project func(contracts.VMInfo) (string, bool)
}

// vmListFields is the canonical comparison projection for the list family. It is
// the SAME config-identity class the describe projection uses — power_state
// (coarsened to "On"/"Off"), uuid, vcpu, memory_mib — minus the name, which is the
// join key (see compareList). Deliberately EXCLUDED: IPs (need the guest agent),
// Disks (carry qemu-img-derived fields), and Networks — comparing them would meter
// phantom drift, not real config drift, exactly as they are excluded from describe.
var vmListFields = []vmListFieldCmp{
	{
		// Both drivers coarsen non-running states to "Off" (virsh via
		// mapLibvirtPowerState, native via mapNativeDomainState), so this is a
		// 0-divergence field when they agree (#291 M2), compared case-insensitively.
		field:   "power_state",
		project: func(v contracts.VMInfo) (string, bool) { return canonLower(v.PowerState) },
	},
	{
		field:   "uuid",
		project: func(v contracts.VMInfo) (string, bool) { return canonLower(v.ProviderRaw["uuid"]) },
	},
	{
		field:   "vcpu",
		project: func(v contracts.VMInfo) (string, bool) { return canonPositiveInt(int64(v.CPU)) },
	},
	{
		// Both sides report MiB already (virsh via domainXML.MemoryMiB, native via
		// domainMemoryMiB — the SAME KiB/1024 with the SAME non-KiB guard), so unlike
		// describe's KiB->MiB case this is a direct integer compare.
		field:   "memory_mib",
		project: func(v contracts.VMInfo) (string, bool) { return canonPositiveInt(v.MemoryMiB) },
	},
}

// compareList returns the names of the fields that SEMANTICALLY diverge between the
// authoritative virsh VM list and the shadow go-libvirt list, after
// canonicalization. Each field name is returned at most once even if it diverges on
// several VMs, so the result is a set (ElementsMatch-friendly and one divergence
// increment per field-kind per run). An empty result means no semantic divergence.
// It never panics and never reads outside the two slices.
//
// # Membership is deliberately ONE-DIRECTIONAL
//
// The two lists are keyed by domain name and joined. For a domain in BOTH, the
// per-field projection above is compared. For a domain in only ONE list, the
// direction matters:
//
//   - virsh has it, native does NOT -> membershipField divergence. This is real
//     drift: native's enumeration (or its per-domain DomainGetXMLDesc/parse, which
//     buildNativeList skips on failure) missed a VM the authoritative driver
//     reported — exactly the D5-relevant signal that native is not yet ready.
//   - native has it, virsh does NOT -> NOT a divergence (log-only at most). virsh's
//     ListVMs deliberately `continue`s a domain whose dumpxml/parse fails (#285), so
//     a native-extra domain is an EXPECTED, benign consequence of virsh being
//     stricter, not native being wrong; counting it would meter phantom drift. We
//     therefore iterate the virsh (authoritative) side only and never flag extras.
func compareList(virsh, native []contracts.VMInfo) []string {
	nativeByName := make(map[string]contracts.VMInfo, len(native))
	for _, v := range native {
		nativeByName[strings.TrimSpace(v.Name)] = v
	}

	seen := make(map[string]bool)
	var diverged []string
	add := func(field string) {
		if !seen[field] {
			seen[field] = true
			diverged = append(diverged, field)
		}
	}

	for _, vv := range virsh {
		nv, ok := nativeByName[strings.TrimSpace(vv.Name)]
		if !ok {
			add(membershipField) // native missed a VM virsh saw (see doc above)
			continue
		}
		for _, f := range vmListFields {
			vVal, vOK := f.project(vv)
			nVal, nOK := f.project(nv)
			if !vOK || !nOK {
				continue // only compare fields both drivers produced
			}
			if vVal != nVal {
				add(f.field)
			}
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

// canonPositiveInt returns n's canonical decimal form, present iff n > 0. It is
// the typed-integer analogue of canonInt for the list family's VMInfo.CPU /
// VMInfo.MemoryMiB fields, which arrive already parsed as ints rather than as
// virsh-formatted strings. A non-positive value is treated as ABSENT, not zero:
// both the virsh and native list projections leave a missing/unparsed vcpu or
// memory as 0, and comparing a synthesized 0 against a real value would meter
// phantom drift — so a 0 is skipped exactly like a field only one driver produced,
// preserving compareDescribe's "compare only when both produced" rule.
func canonPositiveInt(n int64) (string, bool) {
	if n <= 0 {
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

// buildNativeList builds the shadow VMInfo list for the list family from the
// go-libvirt client alone: ConnectListAllDomains (all domains, active+inactive —
// the `virsh list --all` flag pair) enumerates, then per domain DomainGetXMLDesc is
// parsed through libvirtxml (D2) for config identity and DomainGetState gives the
// coarse power state. It projects the SAME config-identity fields as
// buildNativeDescribe (name/uuid/vcpu/memory_mib/power_state) into the SAME typed
// contracts.VMInfo shape virsh's ListVMs produces, so compareList's single-extractor
// projection can compare the two sides directly. It is the pure, host-free core the
// test:// integration test drives directly, and that Provider.listNative wraps with
// the connection watchdog.
//
// It errors ONLY on a total-enumeration failure (ConnectListAllDomains). A
// PER-DOMAIN DomainGetXMLDesc/parse failure SKIPS that one domain rather than
// failing the whole shadow list: one unparseable domain must not mask the rest, and
// if virsh did see that domain the omission correctly surfaces as a compareList
// membership divergence (native missing a VM virsh saw) rather than a blanket
// ShadowResultError — the same posture virsh's own ListVMs takes when it `continue`s
// a domain whose dumpxml fails (#285). It deliberately does NOT populate IPs, Disks,
// or Networks: those are excluded from the comparison projection (see vmListFields).
func buildNativeList(lv *golibvirt.Libvirt) ([]contracts.VMInfo, error) {
	// need_results in libvirt's RPC is a boolean-ish "return the domain objects, not
	// just the count" flag (validated to [0, max], used only as `? &doms : NULL`), so
	// any non-zero value returns ALL matching domains — this mirrors go-libvirt's own
	// Domains() helper, which passes 1. Active|Inactive is exactly `virsh list --all`.
	domains, _, err := lv.ConnectListAllDomains(1, golibvirt.ConnectListDomainsActive|golibvirt.ConnectListDomainsInactive)
	if err != nil {
		return nil, fmt.Errorf("native ConnectListAllDomains: %w", err)
	}

	vms := make([]contracts.VMInfo, 0, len(domains))
	for _, dom := range domains {
		xmlDesc, err := lv.DomainGetXMLDesc(dom, 0)
		if err != nil {
			continue // per-domain failure: skip; surfaces as membership drift if virsh saw it
		}
		d, err := parseDomainLibvirtxml(xmlDesc)
		if err != nil {
			continue // same rationale as the DomainGetXMLDesc skip above
		}

		// DomainGetState is side-effect free; a failure degrades power_state to "Off"
		// (identical to buildNativeDescribe) rather than dropping the whole domain.
		powerState := "Off"
		if stateInt, _, sErr := lv.DomainGetState(dom, 0); sErr == nil {
			powerState = mapNativeDomainState(golibvirt.DomainState(stateInt))
		}

		info := contracts.VMInfo{
			ID:          d.Name, // virsh's ListVMs uses the domain name as the ID too
			Name:        d.Name,
			PowerState:  powerState,
			ProviderRaw: map[string]string{},
		}
		if d.UUID != "" {
			info.ProviderRaw["uuid"] = d.UUID
		}
		if d.VCPU != nil {
			info.CPU = int32(d.VCPU.Value)
		}
		if mib, ok, mErr := domainMemoryMiB(d); mErr == nil && ok {
			info.MemoryMiB = mib
		}
		vms = append(vms, info)
	}
	return vms, nil
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

// listNative is the production native ListVMs: it resolves the host connection
// through the seam and runs buildNativeList under the go-libvirt connection watchdog
// (callLibvirt), so a hung RPC is bounded by ctx and evicts the connection instead
// of blocking forever (ADR-0008 Fact 5). It mirrors describeNative and is the
// default value of Provider.listNativeFn; unit tests swap that field to script
// native results, errors, and panics without a live libvirtd.
func (p *Provider) listNative(ctx context.Context) ([]contracts.VMInfo, error) {
	lc, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	var vms []contracts.VMInfo
	err = lc.callLibvirt(ctx, func(lv *golibvirt.Libvirt) error {
		var bErr error
		vms, bErr = buildNativeList(lv)
		return bErr
	})
	if err != nil {
		return nil, err
	}
	return vms, nil
}

// runDetachedShadow launches fn as a shadow comparison for family on a detached,
// time-bounded, panic-recovered goroutine, and is the shared isolation wrapper both
// read families dispatch through. It is what makes the shadow path unable to affect
// the caller, which has ALREADY received virsh's answer by the time this is reached:
//
//   - Detached: fn gets a fresh context.Background()-derived ctx, NOT the caller's
//     (which is done once the RPC returned). ctx values are not needed by the
//     go-libvirt path.
//   - Time-bounded: that ctx is bounded by shadowTimeout, so the shadow cannot run
//     unbounded — its whole cancellation story (it holds no lock, mutates nothing,
//     and cannot outlive the timeout).
//   - Panic-recovered: the goroutine's whole body is wrapped, so a panic anywhere in
//     fn is recovered, metered as ShadowResultPanic, warn-logged, and NEVER
//     propagated (the caller is entirely unaffected).
//
// shadowWG lets tests and a future graceful shutdown drain any in-flight shadows.
func (p *Provider) runDetachedShadow(family nativeFamily, fn func(ctx context.Context)) {
	p.shadowWG.Add(1)
	go func() {
		defer p.shadowWG.Done()
		defer func() {
			if r := recover(); r != nil {
				obsmetrics.RecordShadowCompare(string(family), obsmetrics.ShadowResultPanic)
				p.shadowLogger().Warn("shadow-compare panicked; recovered, metered, not propagated (caller got virsh's answer)",
					"family", family, "panic", fmt.Sprintf("%v", r))
			}
		}()

		sctx, cancel := context.WithTimeout(context.Background(), p.shadowTimeout())
		defer cancel()
		fn(sctx)
	}()
}

// maybeShadowDescribe runs the go-libvirt shadow comparison for a Describe when the
// describe family is in shadow mode and this call is sampled. It returns
// immediately: runDetachedShadow runs the comparison on a detached, time-bounded,
// panic-recovered goroutine so it can never delay or fail the caller, which has
// already received virsh's answer. The ctx argument is intentionally not forwarded —
// see runDetachedShadow.
func (p *Provider) maybeShadowDescribe(ctx context.Context, id string, virshResp contracts.DescribeResponse) {
	if p.nativeCfg.effectiveMode(familyDescribe) != modeShadow {
		return
	}
	if !p.shadowSampler.sample() {
		return
	}
	p.runDetachedShadow(familyDescribe, func(sctx context.Context) {
		p.runShadowDescribe(sctx, id, virshResp)
	})
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

// maybeShadowList runs the go-libvirt shadow comparison for a ListVMs when the list
// family is in shadow mode and this call is sampled. It mirrors maybeShadowDescribe
// exactly — same effectiveMode/sampler gate, same detached/time-bounded/panic-
// recovered dispatch via runDetachedShadow — so list soaks in parallel with describe
// under identical isolation. The ctx argument is intentionally not forwarded (see
// runDetachedShadow). virshVMs is the authoritative answer the caller already
// received; it is never mutated here.
func (p *Provider) maybeShadowList(ctx context.Context, virshVMs []contracts.VMInfo) {
	if p.nativeCfg.effectiveMode(familyList) != modeShadow {
		return
	}
	if !p.shadowSampler.sample() {
		return
	}
	p.runDetachedShadow(familyList, func(sctx context.Context) {
		p.runShadowList(sctx, virshVMs)
	})
}

// runShadowList is the synchronous core of the list shadow comparison: obtain the
// native list, compare it against the authoritative virsh list, and meter the
// outcome. It NEVER returns anything (the caller already has virsh's answer) and
// isolates go-libvirt errors as a metered ShadowResultError. Unit tests call it
// directly (Provider.listNativeFn scripted) to assert the metering and the error
// isolation without a live libvirtd; maybeShadowList wraps it with the goroutine,
// timeout, and panic recovery.
func (p *Provider) runShadowList(ctx context.Context, virshVMs []contracts.VMInfo) {
	nativeVMs, err := p.listNativeFn(ctx)
	if err != nil {
		obsmetrics.RecordShadowCompare(string(familyList), obsmetrics.ShadowResultError)
		p.shadowLogger().Warn("shadow-compare ListVMs: go-libvirt path failed; metered, not propagated (caller got virsh's answer)",
			"family", familyList, "error", err)
		return
	}

	diverged := compareList(virshVMs, nativeVMs)
	if len(diverged) == 0 {
		obsmetrics.RecordShadowCompare(string(familyList), obsmetrics.ShadowResultEqual)
		return
	}

	obsmetrics.RecordShadowCompare(string(familyList), obsmetrics.ShadowResultDivergent)
	for _, field := range diverged {
		obsmetrics.RecordShadowDivergence(string(familyList), field)
	}
	// Log FIELD NAMES only, never values (and never domain names): a divergence is
	// diagnosed from the metric plus a manual `virsh list --all`/dumpxml, keeping VM
	// identity and configuration out of provider logs (compliance posture: never
	// persist resource internals in logs/events).
	p.shadowLogger().Warn("shadow-compare ListVMs divergence (virsh answer returned; go-libvirt observed only)",
		"family", familyList, "fields", strings.Join(diverged, ","))
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

// initShadow wires the ADR-0008 shadow-compare machinery onto p: it parses
// VIRTRIGAUD_LIBVIRT_NATIVE (logging the effective driver per family and
// publishing the active-driver metric, D4), builds the sampler and per-shadow
// timeout, and installs the real native read functions (describe and list). It is
// called once from
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
	if p.listNativeFn == nil {
		p.listNativeFn = p.listNative
	}
}
