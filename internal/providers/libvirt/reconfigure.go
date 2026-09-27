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
	"log"
	"strconv"
	"strings"

	"libvirt.org/go/libvirtxml"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The Reconfigure core: an honest result.
//
// For each of CPU, memory and disk that a request changes, reconfigureOn
// either
//
//   - applies it to the running domain AND its persistent definition (a
//     running domain), or to the persistent definition of an inactive domain,
//     which is what that domain boots with — the change is applied;
//   - applies it to the persistent definition of a running domain only, when
//     the running domain cannot take it live, and reports restart-required —
//     the change takes effect at the next power cycle (off, then on; a reboot
//     from inside the guest keeps the running QEMU and does not apply it);
//   - or fails, having reported what it could not do.
//
// It never reports success for a requested change in none of these states.
// The persistent definition is always changed first: a live change that
// succeeded but was never persisted would silently revert at the next power
// cycle, so there is no order in which a failure leaves the running domain
// ahead of its definition.
//
// What "applied" means for memory. A domain's memory is its <memory> (the
// balloon maximum: what the guest can use without host action) and its
// <currentMemory> (the balloon target). A running guest can ignore a balloon
// target — lowering it with `setmem --live` is a request the guest may never
// honour — and a running domain's <memory> cannot be changed. So:
//
//   - a memory grow within the running domain's <memory> moves the balloon
//     target up (`setmem --live`) and is applied: the guest can use it now;
//   - a memory grow beyond it raises <memory> in the persistent definition and
//     needs a restart;
//   - a memory SHRINK lowers <memory> (and <currentMemory>) in the persistent
//     definition — so the smaller size is enforced at the next boot, not
//     merely requested of the guest — and, on a running domain, always needs a
//     restart. The live balloon is not trusted and not touched.
//
// This is the simpler of the two options the review offered (versus reading
// the balloon back with dommemstat): a balloon reading can change the moment
// after it is taken, since the guest can deflate again up to <memory>, so it
// could never prove a shrink anyway.
//
// A domain that is active but not running — paused, suspended to RAM
// (pmsuspended), shutting down, crashed but kept (on_crash=preserve) — or in a
// state this provider does not know is refused with a retryable error before
// anything is changed: a --config change to it would not be what it runs, and
// a guest suspended to RAM wakes at its old size (review R1).

// reconfigureMode is how a Reconfigure applies a change to a domain in its
// current state.
type reconfigureMode int

const (
	// reconfigureLive: the domain runs. A change is applied to its persistent
	// definition, then to the running domain.
	reconfigureLive reconfigureMode = iota + 1
	// reconfigurePersistent: the domain is inactive. Its persistent definition
	// is what it boots with, so a change applied there is fully applied.
	reconfigurePersistent
)

// libvirt domain states as `virsh domstate` prints them (lowercased).
const (
	domStateRunning = "running"
	// domStateIdle is VIR_DOMAIN_BLOCKED: running, its vCPUs waiting on I/O.
	domStateIdle      = "idle"
	domStateBlocked   = "blocked"
	domStateShutOff   = "shut off"
	domStateShutoff   = "shutoff"
	domStateCrashed   = "crashed"
	domStatePaused    = "paused"
	domStatePMSusp    = "pmsuspended"
	domStateInShut    = "in shutdown"
	domStateNoState   = "no state"
	domStateSuspended = "suspended"
)

// inactiveDomainID is the domain Id `virsh dominfo` prints for a domain that
// is not active (it has no QEMU process).
const inactiveDomainID = "-"

// reconfigureModeOf maps a domain's state (and, for a crashed domain, its
// dominfo Id) to how a Reconfigure may change it, or to the retryable refusal
// of a domain that is active but not running.
func reconfigureModeOf(state string, info map[string]string) (reconfigureMode, error) {
	s := strings.ToLower(strings.TrimSpace(state))
	switch s {
	case domStateRunning, domStateIdle, domStateBlocked:
		return reconfigureLive, nil
	case domStateShutOff, domStateShutoff:
		return reconfigurePersistent, nil
	case domStateCrashed:
		// A crashed domain kept for inspection (on_crash=preserve) is still
		// active; only an inactive one boots from its definition.
		if strings.TrimSpace(info["Id"]) == inactiveDomainID {
			return reconfigurePersistent, nil
		}
	}
	return 0, contracts.NewRetryableError(fmt.Sprintf(
		"the VM is %s; its CPU, memory and disk are changed only while it is running or shut off, "+
			"so nothing was changed and the reconfigure is retried", describeDomainState(s)), nil)
}

// describeDomainState names a domain state for a requester-facing message:
// libvirt's own word for a state it knows, never raw host output otherwise.
func describeDomainState(s string) string {
	switch s {
	case domStatePaused, domStatePMSusp, domStateInShut, domStateCrashed, domStateSuspended:
		return fmt.Sprintf("%q (active, but not running)", s)
	case domStateNoState:
		return "in no known state"
	default:
		return "in an unrecognized state"
	}
}

// providerLogOnly keeps a host-side failure in an error chain without its
// text. errors.As still finds the *VirshError inside, so a host that could not
// be reached stays HOST_UNAVAILABLE on the routed path (isHostTransportFailure);
// but the text — virsh's stderr can name disk paths, and a command line the
// connection URI — goes to the provider log only, never into the
// VirtualMachine's status.
type providerLogOnly struct{ err error }

// Error returns a fixed pointer to the provider log.
func (e *providerLogOnly) Error() string { return "details are in the provider log" }

// Unwrap exposes the host-side failure for classification.
func (e *providerLogOnly) Unwrap() error { return e.err }

// reconfigureFailed logs a failed Reconfigure step of domain name with its full
// cause and returns the retryable, requester-facing error: "could not <what>",
// naming neither a host path nor an SSH endpoint.
func reconfigureFailed(name, what string, cause error) error {
	log.Printf("WARN Reconfigure of domain %s: could not %s: %v", name, what, cause)
	return contracts.NewRetryableError("could not "+what, &providerLogOnly{err: cause})
}

// domainSize is the CPU and memory a domain definition gives it: vcpus (the
// vCPUs online at boot, <vcpu current>), maxVCPUs (<vcpu>, the ceiling),
// memKiB (<currentMemory>, the balloon target at boot) and maxMemKiB
// (<memory>, the balloon maximum).
type domainSize struct {
	vcpus     int64
	maxVCPUs  int64
	memKiB    int64
	maxMemKiB int64
}

// persistentDomainSize reads the persistent definition of the domain handle
// addresses (`virsh dumpxml --inactive`) — what an inactive domain boots with.
func persistentDomainSize(ctx context.Context, vp *VirshProvider, handle string) (domainSize, error) {
	res, err := vp.runVirshCommand(ctx, "dumpxml", handle, "--inactive")
	if err != nil {
		return domainSize{}, err
	}
	d, err := parseDomainLibvirtxml(res.Stdout)
	if err != nil {
		return domainSize{}, err
	}
	return domainSizeOf(d)
}

// domainSizeOf extracts a domainSize from a parsed definition. A definition
// without <vcpu> or <memory> is refused rather than read as zero.
func domainSizeOf(d *libvirtxml.Domain) (domainSize, error) {
	if d.VCPU == nil || d.VCPU.Value == 0 {
		return domainSize{}, fmt.Errorf("the domain definition has no <vcpu>")
	}
	if d.Memory == nil || d.Memory.Value == 0 {
		return domainSize{}, fmt.Errorf("the domain definition has no <memory>")
	}
	maxMem, err := kibOf(d.Memory.Value, d.Memory.Unit)
	if err != nil {
		return domainSize{}, fmt.Errorf("<memory>: %w", err)
	}
	cur := maxMem
	if d.CurrentMemory != nil && d.CurrentMemory.Value > 0 {
		if cur, err = kibOf(d.CurrentMemory.Value, d.CurrentMemory.Unit); err != nil {
			return domainSize{}, fmt.Errorf("<currentMemory>: %w", err)
		}
	}
	vcpus := d.VCPU.Current
	if vcpus == 0 {
		vcpus = d.VCPU.Value
	}
	return domainSize{vcpus: int64(vcpus), maxVCPUs: int64(d.VCPU.Value), memKiB: cur, maxMemKiB: maxMem}, nil
}

// kibOf returns a memory value in KiB. libvirt's formatter always writes KiB
// (see domainMemoryMiB); any other unit means the document did not come from
// libvirt and is refused rather than mis-scaled.
func kibOf(v uint, unit string) (int64, error) {
	switch unit {
	case "", "KiB", "k", "K":
		return int64(v), nil
	}
	return 0, fmt.Errorf("unexpected memory unit %q (want KiB)", unit)
}

// dominfoKiB parses a `virsh dominfo` memory line ("2097152 KiB") of info.
func dominfoKiB(info map[string]string, key string) (int64, error) {
	raw, ok := info[key]
	if !ok {
		return 0, fmt.Errorf("dominfo has no %q", key)
	}
	v := strings.TrimSpace(raw)
	v = strings.TrimSuffix(v, " KiB")
	v = strings.TrimSuffix(v, " kB")
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse dominfo %q: %w", key, err)
	}
	return n, nil
}

// reconfigureOn is the Reconfigure core, run on connection c — p.virshProvider's
// connection in single-host mode, the leased host connection in clustered
// mode — against domain d. Every helper it reaches (the CPU/memory change, the
// offline and online disk resize, the guest agent that grows the in-guest
// filesystem) runs on that same connection. It reports restartRequired when a
// requested change was applied to the persistent definition only; see the file
// comment for the full contract.
func (p *Provider) reconfigureOn(ctx context.Context, c libvirtConn, d domainTarget, desired contracts.CreateRequest) (restartRequired bool, err error) {
	vp, err := virshOf(c)
	if err != nil {
		return false, err
	}
	name := d.name

	state, err := vp.getDomainState(ctx, d.handle)
	if err != nil {
		return false, reconfigureFailed(name, "read the VM's power state", err)
	}
	log.Printf("INFO Domain %s current state: %s", name, strings.TrimSpace(state))
	info, err := vp.getDomainInfo(ctx, d.handle)
	if err != nil {
		return false, reconfigureFailed(name, "read the VM's current size", err)
	}
	mode, err := reconfigureModeOf(state, info)
	if err != nil {
		log.Printf("WARN Reconfigure of domain %s refused: %v", name, err)
		return false, err
	}

	var pending []string
	if desired.Class.CPU > 0 || desired.Class.MemoryMiB > 0 {
		cfg, err := persistentDomainSize(ctx, vp, d.handle)
		if err != nil {
			return false, reconfigureFailed(name, "read the VM's persistent CPU and memory definition", err)
		}
		if desired.Class.CPU > 0 {
			pend, err := applyVCPUs(ctx, vp, d, mode, info, cfg, int64(desired.Class.CPU))
			if err != nil {
				return false, err
			}
			if pend {
				pending = append(pending, fmt.Sprintf("%d vCPUs", desired.Class.CPU))
			}
		}
		if desired.Class.MemoryMiB > 0 {
			pend, err := applyMemory(ctx, vp, d, mode, info, cfg, int64(desired.Class.MemoryMiB)*1024)
			if err != nil {
				return false, err
			}
			if pend {
				pending = append(pending, fmt.Sprintf("%d MiB of memory", desired.Class.MemoryMiB))
			}
		}
	}

	if desired.Class.DiskDefaults != nil && desired.Class.DiskDefaults.SizeGiB > 0 {
		if err := reconfigureDisk(ctx, vp, d, mode, int(desired.Class.DiskDefaults.SizeGiB)); err != nil {
			return false, err
		}
	}

	if len(pending) > 0 {
		log.Printf("INFO Reconfigure of domain %s: %s applied to the persistent definition only; "+
			"it takes effect at the next power cycle (power off, then on)", name, strings.Join(pending, " and "))
		return true, nil
	}
	log.Printf("INFO Reconfigure of domain %s applied", name)
	return false, nil
}

// liveChangeFailed decides what a failed live (--live) change, made after the
// persistent definition already took it, means: a host or connection that
// failed (or a call that was cancelled) is an error — nothing is known about
// the running domain — and anything else is the running domain refusing the
// change, which then takes effect at the next power cycle (restart required).
func liveChangeFailed(ctx context.Context, name, what string, err error) (restartRequired bool, _ error) {
	if ctx.Err() != nil || isHostTransportFailure(err) {
		return false, reconfigureFailed(name, what+" on the running VM", err)
	}
	log.Printf("INFO Reconfigure of domain %s: could not %s on the running VM; it is in the persistent definition "+
		"and takes effect at the next power cycle: %v", name, what, err)
	return true, nil
}

// applyVCPUs makes want the vCPU count of domain d: in its persistent
// definition first (raising the definition's vCPU maximum if want exceeds it),
// then, for a running domain whose count differs, live. A live change the
// running domain refuses — a grow beyond its vCPU maximum, or an unplug of
// vCPUs that are not hotpluggable — leaves the change in the definition and
// reports restartRequired.
func applyVCPUs(ctx context.Context, vp *VirshProvider, d domainTarget, mode reconfigureMode, info map[string]string, cfg domainSize, want int64) (restartRequired bool, err error) {
	n := strconv.FormatInt(want, 10)
	if cfg.vcpus != want {
		if want > cfg.maxVCPUs {
			if _, err := vp.runVirshCommand(ctx, "setvcpus", d.handle, n, "--config", "--maximum"); err != nil {
				return false, reconfigureFailed(d.name, fmt.Sprintf("raise the vCPU maximum of the VM's persistent definition to %d", want), err)
			}
		}
		if _, err := vp.runVirshCommand(ctx, "setvcpus", d.handle, n, "--config"); err != nil {
			return false, reconfigureFailed(d.name, fmt.Sprintf("set %d vCPUs in the VM's persistent definition", want), err)
		}
	}
	if mode != reconfigureLive {
		return false, nil
	}
	live, err := strconv.ParseInt(strings.TrimSpace(info["CPU(s)"]), 10, 64)
	if err != nil {
		return false, reconfigureFailed(d.name, "read the running VM's vCPU count", fmt.Errorf("dominfo CPU(s) %q: %w", info["CPU(s)"], err))
	}
	if live == want {
		return false, nil
	}
	if _, err := vp.runVirshCommand(ctx, "setvcpus", d.handle, n, "--live"); err != nil {
		return liveChangeFailed(ctx, d.name, fmt.Sprintf("set %d vCPUs", want), err)
	}
	return false, nil
}

// applyMemory makes wantKiB the memory of domain d; see the file comment for
// what "applied" means for memory. The persistent definition gets
// <currentMemory> = wantKiB and <memory> = wantKiB when that grows beyond it
// or shrinks below the current allocation (otherwise <memory> — a hot-add
// VM's balloon headroom — is kept). A setmaxmem failure fails the call. A
// running domain then takes the change live only when its running <memory>
// already equals the definition's (a grow within the balloon maximum: the
// balloon target is moved up); otherwise the change needs a restart.
func applyMemory(ctx context.Context, vp *VirshProvider, d domainTarget, mode reconfigureMode, info map[string]string, cfg domainSize, wantKiB int64) (restartRequired bool, err error) {
	arg := fmt.Sprintf("%dK", wantKiB)
	ceiling := cfg.maxMemKiB
	if wantKiB > cfg.maxMemKiB || wantKiB < cfg.memKiB {
		ceiling = wantKiB
	}
	if ceiling != cfg.maxMemKiB {
		// Raised before a grow (libvirt refuses a current allocation above the
		// maximum); lowered before a shrink (libvirt clamps the current
		// allocation to it).
		if _, err := vp.runVirshCommand(ctx, "setmaxmem", d.handle, fmt.Sprintf("%dK", ceiling), "--config"); err != nil {
			return false, reconfigureFailed(d.name, fmt.Sprintf("set the memory maximum of the VM's persistent definition to %d MiB", ceiling/1024), err)
		}
	}
	if cfg.memKiB != wantKiB || ceiling != cfg.maxMemKiB {
		if _, err := vp.runVirshCommand(ctx, "setmem", d.handle, arg, "--config"); err != nil {
			return false, reconfigureFailed(d.name, fmt.Sprintf("set %d MiB of memory in the VM's persistent definition", wantKiB/1024), err)
		}
	}
	if mode != reconfigureLive {
		return false, nil
	}
	liveMax, err := dominfoKiB(info, "Max memory")
	if err != nil {
		return false, reconfigureFailed(d.name, "read the running VM's memory maximum", err)
	}
	if liveMax != ceiling {
		// A shrink (the maximum was lowered) or a grow beyond the running
		// maximum: a running domain's <memory> cannot change.
		log.Printf("INFO Reconfigure of domain %s: memory %d KiB needs the running maximum %d KiB to become %d KiB; "+
			"it is in the persistent definition and takes effect at the next power cycle", d.name, wantKiB, liveMax, ceiling)
		return true, nil
	}
	if used, err := dominfoKiB(info, "Used memory"); err == nil && used == wantKiB {
		return false, nil
	}
	if _, err := vp.runVirshCommand(ctx, "setmem", d.handle, arg, "--live"); err != nil {
		return liveChangeFailed(ctx, d.name, fmt.Sprintf("set %d MiB of memory", wantKiB/1024), err)
	}
	return false, nil
}

// reconfigureDisk grows domain d's primary disk to desiredGiB. Grow-only: a
// disk already that large (or larger — the VMClass disk size is a floor, and
// libvirt/qcow2 cannot shrink it) is left alone. Any failure to grow it fails
// the call: the guest would not see the requested capacity.
//
// Running: growDiskOnline (live blockresize, then the best-effort in-guest
// filesystem grow). Inactive: growDiskOffline.
func reconfigureDisk(ctx context.Context, vp *VirshProvider, d domainTarget, mode reconfigureMode, desiredGiB int) error {
	sp := NewStorageProvider(vp)
	what := fmt.Sprintf("grow the VM's disk to %d GiB", desiredGiB)
	if mode == reconfigureLive {
		log.Printf("INFO Attempting online disk grow for running VM %s to %dGB", d.name, desiredGiB)
		if _, err := growDiskOnline(ctx, vp, d, desiredGiB, sp); err != nil {
			return reconfigureFailed(d.name, what, err)
		}
		return nil
	}
	if err := growDiskOffline(ctx, vp, d, desiredGiB, sp); err != nil {
		return reconfigureFailed(d.name, what, err)
	}
	return nil
}

// growDiskOffline grows the primary disk of the inactive domain d to
// desiredGiB. The disk's current capacity is read from the domain itself
// (domblklist, domblkinfo) so a disk already that large is not touched; a grow
// then resizes the backing volume — single-host by the historical
// "<name>-disk" volume of the default pool, a clustered (owner-checked) target
// by the path of its own primary disk, which fails for a disk with no host
// path (a network disk).
func growDiskOffline(ctx context.Context, vp *VirshProvider, d domainTarget, desiredGiB int, sp *StorageProvider) error {
	res, err := vp.runVirshCommand(ctx, "domblklist", d.handle)
	if err != nil {
		return fmt.Errorf("list block devices of domain %s: %w", d.name, err)
	}
	target, err := parseDomblklistPrimaryTarget(res.Stdout)
	if err != nil {
		return fmt.Errorf("resolve the primary disk of domain %s: %w", d.name, err)
	}
	info, err := vp.runVirshCommand(ctx, "domblkinfo", d.handle, target)
	if err != nil {
		return fmt.Errorf("read block info of domain %s target %s: %w", d.name, target, err)
	}
	currentBytes, err := parseDomblkinfoCapacity(info.Stdout)
	if err != nil {
		return fmt.Errorf("parse the capacity of domain %s target %s: %w", d.name, target, err)
	}
	if !shouldGrowDisk(currentBytes, int64(desiredGiB)*bytesPerGiB) {
		log.Printf("INFO Offline disk grow skipped for domain %s target %s: %d bytes already ≥ %dGB (grow-only)",
			d.name, target, currentBytes, desiredGiB)
		return nil
	}
	log.Printf("INFO Growing the disk of stopped domain %s target %s to %dGB", d.name, target, desiredGiB)
	if d.diskByPath {
		return resizePrimaryDiskOffline(ctx, vp, d, sp, desiredGiB)
	}
	return sp.ResizeVolume(ctx, defaultStoragePool, vmDiskVolumeName(d.name), desiredGiB)
}
