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
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"libvirt.org/go/libvirtxml"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// This file is the ADR-0007 P1 libvirt host-inventory data plane: it turns the
// per-host `virsh` output that ListHosts/GetHostInfo (hosts.go) need into a
// typed contracts.HostInfo. Each HostInfo field is fed by one query:
//
//	allocatable_cpu / allocatable_mem_mib  <- virsh nodeinfo        (text)
//	cpu_model / cpu_features               <- virsh capabilities    (XML, libvirtxml.Caps)
//	machine_types / emulator_version       <- virsh domcapabilities (XML, libvirtxml.DomainCaps)
//	allocatable_storage                    <- virsh pool-list + pool-dumpxml, then statvfs
//	                                          of the default pool's directory (stat -f)
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

	// statfsAvailFormat is the `stat -f -c` format both free-space
	// measurements use (the inventory's collectStorageBytes and the import-time
	// hostFreeBytes): %a is statvfs f_bavail (blocks available to a non-root
	// user) and %S is f_frsize (the fundamental block size %a counts in).
	statfsAvailFormat = "%a %S"

	// hostStorageProbeTimeout bounds the whole allocatable-storage measurement
	// of one host (pool-list, pool-dumpxml and the statvfs), so a hung storage
	// mount (statfs on a dead hard-mounted NFS export blocks) cannot stall an
	// inventory sync; on expiry the host reports 0 (fail closed).
	hostStorageProbeTimeout = 15 * time.Second
)

// filesystemPoolTypes are the libvirt storage pool types whose target is a
// directory on a mounted filesystem (a plain directory, a local block-device
// filesystem, a network filesystem): the only kinds whose free space statvfs
// of the target measures.
var filesystemPoolTypes = []string{"dir", "fs", "netfs"}

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

// collectStorageBytes reports the bytes a non-root writer can still use in the
// host's `default` storage pool — the pool VM disks, clones and imported disks
// land in (defaultStoragePool) — measured FRESH on every inventory sync (R31).
//
// Steps, all under one hostStorageProbeTimeout budget:
//
//  1. `virsh pool-list --all`: the `default` pool must be ACTIVE. Other pools
//     are not counted: space there cannot hold a disk VirtRigaud lands in
//     `default`, and summing them admitted hosts whose `default` pool is full
//     (#416 M3).
//  2. `virsh pool-dumpxml default` (typed libvirtxml.StoragePool): the pool's
//     type and target directory.
//  3. For a filesystem-backed pool (filesystemPoolTypes — the only kinds whose
//     target is a directory VirtRigaud can write disk files into): `stat -f -c
//     "%a %S" -- <dir>` on the host, i.e. statvfs f_bavail * f_frsize — the
//     same measurement, in the same unit (bytes), as the import-time
//     free-space check (hostFreeBytes).
//
// libvirt's own pool-info Available is deliberately NOT used: libvirt caches it
// until `virsh pool-refresh` (two hosts mounting one NFS export as their
// `default` pool reported 220.8 GiB and 558.5 GiB for a filesystem with ~501
// GiB free), and even right after a refresh it is f_bfree, which counts the
// blocks reserved for root that the provider's non-root writes cannot use. A
// pool-refresh per sync is not done either: it rescans every volume of the
// pool (slow on a large NFS pool, on every heartbeat of every host) and
// contends with the refresh routed import runs after publishing a disk.
//
// It never fails the caller, and it fails CLOSED: any error (pools unavailable,
// no active `default` pool, a pool that is not filesystem-backed, an
// unreadable definition, a stat that fails, times out or prints something
// unexpected) yields 0, which the scheduler's disk-space filter (ADR-0007
// Addendum B, B7.4) and a VMPlacementPolicy's minDiskSpacePerHost read as "no
// room" — never a stale or made-up large value.
func collectStorageBytes(ctx context.Context, c hostconn.Conn, id hostconn.HostID, logger *slog.Logger) int64 {
	ctx, cancel := context.WithTimeout(ctx, hostStorageProbeTimeout)
	defer cancel()

	listRes, err := c.Virsh(ctx, "pool-list", "--all")
	if err != nil {
		logger.Debug("libvirt host inventory: pool-list unavailable (best-effort)",
			"host", string(id), "error", err.Error())
		return 0
	}
	if !slices.Contains(parseActivePoolNames(listRes.Stdout), defaultStoragePool) {
		logger.Debug("libvirt host inventory: no active default storage pool (best-effort)", "host", string(id))
		return 0
	}
	defRes, err := c.Virsh(ctx, "pool-dumpxml", defaultStoragePool)
	if err != nil {
		logger.Warn("libvirt host inventory: cannot read the default storage pool; reporting no allocatable storage",
			"host", string(id), "pool", defaultStoragePool, "error", err.Error())
		return 0
	}
	poolType, dir, err := parseFilesystemPoolDir(defRes.Stdout)
	if err != nil {
		logger.Warn("libvirt host inventory: cannot measure the default storage pool; reporting no allocatable storage",
			"host", string(id), "pool", defaultStoragePool, "type", poolType, "error", err.Error())
		return 0
	}
	statRes, err := c.RunHost(ctx, "stat", "-f", "-c", statfsAvailFormat, "--", dir)
	if err != nil {
		logger.Warn("libvirt host inventory: free-space measurement of the default storage pool failed; reporting no allocatable storage",
			"host", string(id), "pool", defaultStoragePool, "dir", dir, "error", err.Error())
		return 0
	}
	avail, err := parseStatfsAvailBytes(statRes.Stdout)
	if err != nil {
		logger.Warn("libvirt host inventory: unexpected free-space measurement of the default storage pool; reporting no allocatable storage",
			"host", string(id), "pool", defaultStoragePool, "dir", dir, "error", err.Error())
		return 0
	}
	return avail
}

// parseFilesystemPoolDir parses a `virsh pool-dumpxml` document (typed
// libvirtxml.StoragePool) and returns the pool's type and target directory
// when the pool is filesystem-backed (filesystemPoolTypes). poolType is
// returned whenever the document parses, for the caller's log. It is an error
// when the document does not parse, the pool is of another type (a logical,
// iSCSI, RBD, ZFS, ... pool's target is not a directory VirtRigaud writes disk
// files into, and statvfs of it would measure an unrelated filesystem), or the
// target path is missing, relative or not lexically clean.
func parseFilesystemPoolDir(poolXML string) (poolType, dir string, err error) {
	def := &libvirtxml.StoragePool{}
	if uerr := def.Unmarshal(poolXML); uerr != nil {
		return "", "", fmt.Errorf("parse pool-dumpxml: %w", uerr)
	}
	poolType = def.Type
	if !slices.Contains(filesystemPoolTypes, poolType) {
		return poolType, "", fmt.Errorf("pool type %q is not filesystem-backed (want one of %v)", poolType, filesystemPoolTypes)
	}
	if def.Target == nil {
		return poolType, "", fmt.Errorf("pool of type %q has no target", poolType)
	}
	dir = strings.TrimSpace(def.Target.Path)
	if trimmed := strings.TrimRight(dir, "/"); trimmed != "" {
		dir = trimmed
	}
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return poolType, "", fmt.Errorf("pool target path %q is not a usable absolute directory", def.Target.Path)
	}
	return poolType, dir, nil
}

// parseStatfsAvailBytes parses the output of `stat -f -c "%a %S"`
// (statfsAvailFormat): the filesystem's blocks available to a non-root user
// (statvfs f_bavail) and its fundamental block size (f_frsize), and returns
// their product in bytes. It is strict — exactly two non-negative decimal
// integers, a positive block size, and no int64 overflow — so an odd or
// truncated output is an error, never a misread (and possibly huge) number.
func parseStatfsAvailBytes(stdout string) (int64, error) {
	fields := strings.Fields(stdout)
	if len(fields) != 2 {
		return 0, fmt.Errorf("statfs output %q: want 2 fields, got %d", stdout, len(fields))
	}
	avail, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || avail < 0 {
		return 0, fmt.Errorf("statfs available blocks %q is not a non-negative integer", fields[0])
	}
	frsize, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || frsize <= 0 {
		return 0, fmt.Errorf("statfs block size %q is not a positive integer", fields[1])
	}
	if avail > math.MaxInt64/frsize {
		return 0, fmt.Errorf("statfs free space %d x %d overflows int64", avail, frsize)
	}
	return avail * frsize, nil
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
