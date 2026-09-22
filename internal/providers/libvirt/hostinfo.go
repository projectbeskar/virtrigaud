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
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"libvirt.org/go/libvirtxml"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// This file is the ADR-0007 P1 libvirt host-inventory data plane: it turns the
// per-host `virsh` output that ListHosts/GetHostInfo (hosts.go) need into a
// typed contracts.HostInfo. Each HostInfo field is fed by exactly one virsh
// command:
//
//	allocatable_cpu / allocatable_mem_mib  <- virsh nodeinfo        (text)
//	cpu_model / cpu_features               <- virsh capabilities    (XML, libvirtxml.Caps)
//	machine_types / emulator_version       <- virsh domcapabilities (XML, libvirtxml.DomainCaps)
//	allocatable_storage                    <- virsh pool-list/pool-info (text, best-effort)
//	health                                 <- nodeinfo reachability (the core query)
//	id / address / labels                  <- the registry's parsed inventory (no query)
//
// "Allocatable" means the host's RAW schedulable capacity — total logical CPUs
// and total memory. This PR does NOT apply HostPool overcommit and does NOT
// subtract already-bound VMs; the operator-side scheduler does that later
// (ADR-0007). allocatable_cpu/mem are therefore host TOTALS.
//
// XML is parsed through the typed libvirt.org/go/libvirtxml types, never ad-hoc
// regex (ADR-0008 D2's typed-XML discipline), so a namespace/attribute quirk in
// real `virsh` output cannot silently mis-parse.

const (
	// kibPerMiB converts the KiB unit virsh nodeinfo reports memory in to MiB.
	kibPerMiB = 1024

	// nodeInfoKeyCPUs is the virsh nodeinfo label for the host's logical CPU
	// count. It is matched EXACTLY: several nodeinfo labels share the "CPU"
	// prefix ("CPU model", "CPU(s)", "CPU frequency", "CPU socket(s)").
	nodeInfoKeyCPUs = "CPU(s)"
	// nodeInfoKeyMemory is the virsh nodeinfo label for total memory (KiB).
	nodeInfoKeyMemory = "Memory size"
	// nodeInfoMemUnit is the only memory unit libvirt emits in nodeinfo.
	nodeInfoMemUnit = "KiB"

	// poolStateActive is the virsh pool-list State value for an active pool.
	poolStateActive = "active"
	// poolListHeaderName is the first column header of `virsh pool-list`, skipped
	// when scanning rows.
	poolListHeaderName = "Name"
	// poolInfoKeyAvailable is the virsh pool-info label for a pool's free bytes.
	poolInfoKeyAvailable = "Available"
)

// collectOneHost gathers one host's live inventory behind a briefly-held
// connection lease and returns it as a contracts.HostInfo. It is the per-host
// unit ListHosts iterates and GetHostInfo calls once.
//
// The lease is ALWAYS released: ConnFor is paired with a deferred Close on
// every exit path (success, query error, panic), so a concurrent host-remove is
// never blocked by a leaked lease and graceful-drain stays non-severing
// (ADR-0007 D3). A host whose lease cannot be obtained (unknown, draining,
// closed registry, or a failed lazy dial) is reported HOST_HEALTH_NOT_READY
// with the metadata the registry still knows (id/address/labels) — a down host
// never aborts a whole ListHosts.
func (p *Provider) collectOneHost(ctx context.Context, id hostconn.HostID) contracts.HostInfo {
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}

	// Non-secret metadata (endpoint + labels) is available without dialing.
	address, labels, _ := p.clusterReg.HostMeta(id)

	lease, err := p.clusterReg.ConnFor(ctx, id)
	if err != nil {
		logger.Warn("libvirt host inventory: connection unavailable; reporting host NotReady",
			"host", string(id), "error", err.Error())
		return contracts.HostInfo{
			ID:      string(id),
			Address: address,
			Labels:  labels,
			Health:  contracts.HostHealthNotReady,
		}
	}
	// Release the lease on every exit path. Close releases the per-borrow lease;
	// it does NOT close the shared underlying connection (hostconn lease API),
	// so a drain that removed this host mid-collection completes cleanly the
	// moment we return.
	defer func() { _ = lease.Close() }()

	return collectHostInfo(ctx, lease, id, address, labels, logger)
}

// collectHostInfo gathers one already-leased host's inventory into a
// contracts.HostInfo. It is split from collectOneHost (which owns the lease) so
// it can be unit-tested against a fake hostconn.Conn returning canned virsh
// fixtures — no registry, no lease, no live libvirtd.
//
// Health gating: `virsh nodeinfo` is the CORE query. If it fails (connection
// down) or does not parse, the host is HOST_HEALTH_NOT_READY and the richer
// queries are skipped. If it succeeds the host is HOST_HEALTH_READY; the
// remaining queries (capabilities, domcapabilities, storage pools) are
// BEST-EFFORT — a failure there leaves the corresponding field empty/zero and
// is logged, never flipping health or aborting.
func collectHostInfo(ctx context.Context, c hostconn.Conn, id hostconn.HostID, address string, labels map[string]string, logger *slog.Logger) contracts.HostInfo {
	info := contracts.HostInfo{
		ID:      string(id),
		Address: address,
		Labels:  labels,
	}

	// --- Core query: nodeinfo (cpu/mem + health gate). ---
	niRes, err := c.Virsh(ctx, "nodeinfo")
	if err != nil {
		logger.Warn("libvirt host inventory: nodeinfo failed; reporting host NotReady",
			"host", string(id), "error", err.Error())
		info.Health = contracts.HostHealthNotReady
		return info
	}
	cpu, memMiB, err := parseNodeInfoCPUMem(niRes.Stdout)
	if err != nil {
		logger.Warn("libvirt host inventory: nodeinfo parse failed; reporting host NotReady",
			"host", string(id), "error", err.Error())
		info.Health = contracts.HostHealthNotReady
		return info
	}
	info.AllocatableCPU = cpu
	info.AllocatableMemMiB = memMiB
	info.Health = contracts.HostHealthReady

	// --- Best-effort: capabilities (host CPU model + feature flags). ---
	if capsRes, cerr := c.Virsh(ctx, "capabilities"); cerr != nil {
		logger.Debug("libvirt host inventory: capabilities unavailable (best-effort)",
			"host", string(id), "error", cerr.Error())
	} else if model, features, perr := parseHostCPUFromCaps(capsRes.Stdout); perr != nil {
		logger.Debug("libvirt host inventory: capabilities parse failed (best-effort)",
			"host", string(id), "error", perr.Error())
	} else {
		info.CPUModel = model
		info.CPUFeatures = features
	}

	// --- Best-effort: domcapabilities (default machine type + emulator). ---
	if dcRes, derr := c.Virsh(ctx, "domcapabilities"); derr != nil {
		logger.Debug("libvirt host inventory: domcapabilities unavailable (best-effort)",
			"host", string(id), "error", derr.Error())
	} else if machines, emulator, perr := parseDomCapsMachineEmulator(dcRes.Stdout); perr != nil {
		logger.Debug("libvirt host inventory: domcapabilities parse failed (best-effort)",
			"host", string(id), "error", perr.Error())
	} else {
		info.MachineTypes = machines
		info.EmulatorVersion = emulator
	}

	// --- Best-effort: storage (sum of active pools' available bytes). ---
	info.AllocatableStorage = collectStorageBytes(ctx, c, id, logger)

	return info
}

// parseNodeInfoCPUMem parses `virsh nodeinfo` text for the host's logical CPU
// count and total memory. nodeinfo is "Label:<spaces>Value" lines; this reads
// "CPU(s)" (logical CPUs -> allocatable_cpu) and "Memory size" (KiB, libvirt's
// fixed unit -> converted to MiB for allocatable_mem_mib).
//
// Labels are matched EXACTLY on the token before ':' because several share the
// "CPU" prefix. A missing/blank CPU(s) or Memory size line is an error:
// nodeinfo is the health-gate query, so an unparseable one means the host is
// not reporting core facts and must be marked NotReady by the caller.
func parseNodeInfoCPUMem(stdout string) (cpu int32, memMiB int64, err error) {
	var haveCPU, haveMem bool
	for _, line := range strings.Split(stdout, "\n") {
		key, value, ok := splitColon(line)
		if !ok {
			continue
		}
		switch key {
		case nodeInfoKeyCPUs:
			n, perr := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
			if perr != nil {
				return 0, 0, fmt.Errorf("parse nodeinfo %q value %q: %w", nodeInfoKeyCPUs, value, perr)
			}
			cpu = int32(n)
			haveCPU = true
		case nodeInfoKeyMemory:
			kib, perr := parseNodeInfoMemoryKiB(value)
			if perr != nil {
				return 0, 0, perr
			}
			memMiB = kib / kibPerMiB
			haveMem = true
		}
	}
	if !haveCPU {
		return 0, 0, fmt.Errorf("nodeinfo missing %q line", nodeInfoKeyCPUs)
	}
	if !haveMem {
		return 0, 0, fmt.Errorf("nodeinfo missing %q line", nodeInfoKeyMemory)
	}
	return cpu, memMiB, nil
}

// parseNodeInfoMemoryKiB parses a nodeinfo "Memory size" value ("<n> KiB") into
// KiB. libvirt always emits nodeinfo memory in KiB; a present-but-different unit
// is rejected rather than silently mis-scaled (the same guard domainMemoryMiB
// applies to a domain's <memory>). A bare number with no unit is accepted as
// KiB.
func parseNodeInfoMemoryKiB(value string) (int64, error) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0, fmt.Errorf("nodeinfo %q has no value", nodeInfoKeyMemory)
	}
	kib, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse nodeinfo %q value %q: %w", nodeInfoKeyMemory, value, err)
	}
	if len(fields) > 1 && fields[1] != nodeInfoMemUnit {
		return 0, fmt.Errorf("unexpected nodeinfo %q unit %q (want %s)", nodeInfoKeyMemory, fields[1], nodeInfoMemUnit)
	}
	return kib, nil
}

// parseHostCPUFromCaps parses `virsh capabilities` XML for the host's baseline
// CPU model and feature flags via the typed libvirtxml.Caps (never regex). The
// model is <host><cpu><model> (e.g. "Skylake-Client-IBRS"); the features are the
// <feature name='...'/> flags under <host><cpu>. Either may be empty on a host
// libvirt cannot map to a named model — that is not an error.
func parseHostCPUFromCaps(capsXML string) (model string, features []string, err error) {
	caps := &libvirtxml.Caps{}
	if uerr := caps.Unmarshal(capsXML); uerr != nil {
		return "", nil, fmt.Errorf("parse virsh capabilities XML: %w", uerr)
	}
	if caps.Host.CPU == nil {
		return "", nil, nil
	}
	model = caps.Host.CPU.Model
	if len(caps.Host.CPU.FeatureFlags) > 0 {
		features = make([]string, 0, len(caps.Host.CPU.FeatureFlags))
		for _, f := range caps.Host.CPU.FeatureFlags {
			if f.Name != "" {
				features = append(features, f.Name)
			}
		}
	}
	return model, features, nil
}

// parseDomCapsMachineEmulator parses `virsh domcapabilities` XML for the host's
// default machine type and emulator via the typed libvirtxml.DomainCaps.
//
// machineTypes is the single <machine> domcapabilities reports for the queried
// (default) emulator/arch/virttype — the host's DEFAULT machine type, returned
// as a one-element slice (nil if absent). domcapabilities does NOT enumerate
// every supported machine type; a full list would come from `virsh
// capabilities` <guest> arches and is a documented follow-up.
//
// emulator is <path> — the emulator BINARY PATH (e.g.
// "/usr/bin/qemu-system-x86_64"). domcapabilities carries no numeric QEMU
// version, so this surfaces the emulator identity (path) into
// HostInfo.emulator_version — which is exactly what the ADR-0007 migration
// pre-flight's "same emulator" check compares. A true QEMU version string (via
// `virsh version`) is a documented follow-up.
func parseDomCapsMachineEmulator(domCapsXML string) (machineTypes []string, emulator string, err error) {
	dc := &libvirtxml.DomainCaps{}
	if uerr := dc.Unmarshal(domCapsXML); uerr != nil {
		return nil, "", fmt.Errorf("parse virsh domcapabilities XML: %w", uerr)
	}
	if dc.Machine != "" {
		machineTypes = []string{dc.Machine}
	}
	return machineTypes, dc.Path, nil
}

// collectStorageBytes best-effort sums the available bytes of every ACTIVE
// storage pool the host reports (`virsh pool-list --all`, then `virsh pool-info
// --bytes` per active pool). It never fails the caller: any error (pools
// unavailable, --bytes unsupported on an old virsh, an odd pool-info) yields 0
// for that pool and is logged at debug. The host-inventory file carries no pool
// configuration yet, so this is a whole-host best-effort until per-pool
// scheduling constraints land (ADR-0007).
func collectStorageBytes(ctx context.Context, c hostconn.Conn, id hostconn.HostID, logger *slog.Logger) int64 {
	listRes, err := c.Virsh(ctx, "pool-list", "--all")
	if err != nil {
		logger.Debug("libvirt host inventory: pool-list unavailable (best-effort)",
			"host", string(id), "error", err.Error())
		return 0
	}
	var total int64
	for _, pool := range parseActivePoolNames(listRes.Stdout) {
		infoRes, ierr := c.Virsh(ctx, "pool-info", "--bytes", pool)
		if ierr != nil {
			logger.Debug("libvirt host inventory: pool-info failed (best-effort)",
				"host", string(id), "pool", pool, "error", ierr.Error())
			continue
		}
		avail, ok := parsePoolAvailableBytes(infoRes.Stdout)
		if !ok {
			logger.Debug("libvirt host inventory: pool-info missing Available (best-effort)",
				"host", string(id), "pool", pool)
			continue
		}
		total += avail
	}
	return total
}

// parseActivePoolNames parses `virsh pool-list --all` tabular output for the
// names of pools whose State is "active". The output is a header row
// ("Name  State  Autostart"), a dashed separator, then one row per pool; this
// skips the header and separator and returns the first column of every row whose
// second column is "active".
func parseActivePoolNames(stdout string) []string {
	var names []string
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == poolListHeaderName || strings.HasPrefix(fields[0], "---") {
			continue
		}
		if fields[1] == poolStateActive {
			names = append(names, fields[0])
		}
	}
	return names
}

// parsePoolAvailableBytes parses `virsh pool-info --bytes` output for the pool's
// Available bytes. With --bytes the value is a raw integer (optionally followed
// by a "bytes" token); this reads the first integer after the "Available:"
// label. ok is false when there is no parseable Available line.
func parsePoolAvailableBytes(stdout string) (int64, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		key, value, ok := splitColon(line)
		if !ok || key != poolInfoKeyAvailable {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return 0, false
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// splitColon splits a "Label: value" line at the FIRST colon, returning the
// trimmed label and trimmed value. ok is false for a line with no colon.
// Splitting on the first colon keeps values that themselves contain ':' intact.
func splitColon(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}
