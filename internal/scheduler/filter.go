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

package scheduler

import (
	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// Coarse rejection categories, used both as the per-host trace reason and as the
// tally key in NoFeasibleHostError. Each string appears in the error a caller may
// surface, so they are stable constants.
const (
	rejNotReady          = "host not Ready"
	rejCordoned          = "host cordoned (schedulable=false)"
	rejHostNotAllowed    = "host not in hard host allow-list"
	rejHostExcluded      = "host in hard excluded-hosts list"
	rejNodeSelector      = "host labels do not match hard nodeSelector"
	rejStorageVisibility = "host cannot see a required storage pool"
	rejNetworkVisibility = "host does not have a required network"
	rejMissingFeature    = "host missing a required CPU feature"
	rejMachineType       = "host does not support the required machine type"
	rejMinResources      = "host below a required minimum-per-host resource"
	rejInsufficientCPU   = "insufficient CPU capacity"
	rejInsufficientMem   = "insufficient memory capacity"
	rejHostAffinity      = "host not in the strict host-affinity set"
	rejHostAntiAffinity  = "host at the strict host-anti-affinity VM cap"
	rejReqVMAffinity     = "host does not satisfy a required VM-affinity term"
	rejReqVMAntiAffinity = "host runs a strict anti-affine VM"
)

// RejectionExcludedForVM is the rejection category of a host the caller listed
// in Request.ExcludedHosts. It is exported (unlike the other categories) so a
// caller can recognise a no-fit in which every candidate was excluded
// (AllExcluded).
const RejectionExcludedForVM = "host excluded for this VM"

// defaultMaxVMsPerHost is the per-host VM cap applied when HostAntiAffinity is
// enabled but MaxVMsPerHost is unset: "prevent VMs from being placed on the same
// host" read literally is one VM per host.
const defaultMaxVMsPerHost = 1

// filterHost runs the ordered hard-constraint chain against one host. It returns
// ("", "", nil) when the host is feasible, or (reasonCategory, detail, nil) naming
// the first constraint that eliminated it. It returns a non-nil error only for a
// malformed affinity label selector the caller must fix.
//
// Order is cheapest/most-fundamental first (health, then policy host lists, then
// visibility/features, then capacity, then affinity), so the reported reason is
// the most basic thing wrong with the host.
func (ec *evalContext) filterHost(h *v1beta1.Host) (reason, detail string, err error) {
	// 0. Hosts the caller excluded for this VM (Request.ExcludedHosts), checked
	//    first so an excluded host is always reported as excluded — which is
	//    what lets AllExcluded recognise "every candidate is excluded".
	if containsString(ec.req.ExcludedHosts, h.Name) {
		return RejectionExcludedForVM, "", nil
	}

	// 1. Health + cordon (ADR-0007 D4/D8). A drained (cordoned) or NotReady host is
	//    never a placement target; this is also what makes idempotency re-place a
	//    bound VM off a now-cordoned host.
	if h.Status.Health != v1beta1.HostHealthReady {
		return rejNotReady, string(h.Status.Health), nil
	}
	if !h.Spec.Schedulable {
		return rejCordoned, "", nil
	}

	// 2-3. Hard host allow/deny lists (VMPlacementPolicy.Hard.Hosts / ExcludedHosts).
	if ec.hard != nil {
		if len(ec.hard.Hosts) > 0 && !containsString(ec.hard.Hosts, h.Name) {
			return rejHostNotAllowed, "", nil
		}
		if containsString(ec.hard.ExcludedHosts, h.Name) {
			return rejHostExcluded, "", nil
		}
		// 4. Hard node-selector against Host.spec.labels.
		for k, v := range ec.hard.NodeSelector {
			if !hasLabelValue(h.Spec.Labels, k, v) {
				return rejNodeSelector, k, nil
			}
		}
	}

	// 5-6. D6 storage/network visibility: the VM's required pools/networks as a
	//      Host.spec.labels requirement.
	for _, pool := range ec.req.RequiredStoragePools {
		if !hasLabelValue(h.Spec.Labels, LabelStoragePoolPrefix+pool, labelValueTrue) {
			return rejStorageVisibility, pool, nil
		}
	}
	for _, net := range ec.req.RequiredNetworks {
		if !hasLabelValue(h.Spec.Labels, LabelNetworkPrefix+net, labelValueTrue) {
			return rejNetworkVisibility, net, nil
		}
	}

	// 7. Required CPU features (ResourceConstraints.RequiredFeatures) vs
	//    Host.status.cpuFeatures.
	if ec.resources != nil {
		for _, feat := range ec.resources.RequiredFeatures {
			if !containsString(h.Status.CPUFeatures, feat) {
				return rejMissingFeature, feat, nil
			}
		}
	}

	// 8. Required machine type (caller-resolved) vs Host.status.machineTypes.
	if ec.req.RequiredMachineType != "" && !containsString(h.Status.MachineTypes, ec.req.RequiredMachineType) {
		return rejMachineType, ec.req.RequiredMachineType, nil
	}

	// 9. Minimum-per-host resource floors (ResourceConstraints.Min*PerHost),
	//    checked against the host's raw (pre-overcommit) allocatable size.
	if r, d, ok := ec.filterMinResources(h); !ok {
		return r, d, nil
	}

	// 10. Capacity fit (ADR-0007 D4): the request must fit in what is FREE —
	//     the host's capacity after overcommit, minus what bound, pending and
	//     assumed VMs already hold there (ADR-0007 Addendum A, scheduler
	//     accuracy amendment). The arithmetic is recorded by the caller
	//     (capacityShortfall) for the no-fit message.
	freeCPU, freeMem := ec.freeCapacity(h)
	if freeCPU < int64(ec.req.Resources.CPU) {
		return rejInsufficientCPU, "", nil
	}
	if freeMem < ec.req.Resources.MemoryMiB {
		return rejInsufficientMem, "", nil
	}

	// 11-12. Strict host (anti-)affinity.
	if r, d := ec.filterHostAffinity(h); r != "" {
		return r, d, nil
	}

	// 13-14. Required VM (anti-)affinity against the already-placed set.
	return ec.filterVMAffinity(h)
}

// capacityShortfall returns the arithmetic behind a capacity rejection of h
// (reason rejInsufficientCPU or rejInsufficientMem), or nil for any other
// reason.
func (ec *evalContext) capacityShortfall(h *v1beta1.Host, reason string) *CapacityShortfall {
	effCPU, effMem := ec.effectiveCapacity(h)
	c := ec.committedOn(h.Name)
	switch reason {
	case rejInsufficientCPU:
		return &CapacityShortfall{Requested: int64(ec.req.Resources.CPU), Capacity: effCPU, Committed: c.cpu}
	case rejInsufficientMem:
		return &CapacityShortfall{Requested: ec.req.Resources.MemoryMiB, Capacity: effMem, Committed: c.memMiB}
	}
	return nil
}

// filterMinResources enforces the ResourceConstraints.Min*PerHost floors against
// the host's raw allocatable (physical) capacity. It returns ok=false with the
// reason/detail for the first floor the host fails.
func (ec *evalContext) filterMinResources(h *v1beta1.Host) (reason, detail string, ok bool) {
	rc := ec.resources
	if rc == nil {
		return "", "", true
	}
	if rc.MinCPUPerHost != nil && int32Deref(h.Status.AllocatableCPU) < int64(*rc.MinCPUPerHost) {
		return rejMinResources, "cpu", false
	}
	if rc.MinMemoryPerHost != nil {
		minMiB := rc.MinMemoryPerHost.Value() / bytesPerMiB
		if int64Deref(h.Status.AllocatableMemoryMiB) < minMiB {
			return rejMinResources, "memory", false
		}
	}
	if rc.MinDiskSpacePerHost != nil && int64Deref(h.Status.AllocatableStorageBytes) < rc.MinDiskSpacePerHost.Value() {
		return rejMinResources, "disk", false
	}
	return "", "", true
}

// filterHostAffinity enforces STRICT host affinity (host must be in the preferred
// set) and STRICT host anti-affinity (host must be below the per-host VM cap).
// Non-strict scopes are handled as score preferences in score.go, not here.
func (ec *evalContext) filterHostAffinity(h *v1beta1.Host) (reason, detail string) {
	if ec.affinity != nil && ec.affinity.HostAffinity != nil {
		ha := ec.affinity.HostAffinity
		if ha.Enabled && ha.Scope == scopeStrict && !containsString(ha.PreferredHosts, h.Name) {
			return rejHostAffinity, ""
		}
	}
	if ec.antiAffinity != nil && ec.antiAffinity.HostAntiAffinity != nil {
		haa := ec.antiAffinity.HostAntiAffinity
		if haa.Enabled && haa.Scope == scopeStrict {
			vmCap := hostAntiAffinityCap(haa)
			if ec.boundCount(h.Name) >= vmCap {
				return rejHostAntiAffinity, ""
			}
		}
	}
	return "", ""
}

// filterVMAffinity enforces REQUIRED VM affinity (the host must satisfy every
// required-affinity term) and REQUIRED VM anti-affinity (the host must run no VM
// matching any required term). Preferred terms are scored, not filtered.
func (ec *evalContext) filterVMAffinity(h *v1beta1.Host) (reason, detail string, err error) {
	if ec.affinity != nil && ec.affinity.VMAffinity != nil {
		for _, term := range ec.affinity.VMAffinity.RequiredDuringScheduling {
			ok, matchErr := ec.hostRunsMatchingVM(h.Name, term)
			if matchErr != nil {
				return "", "", matchErr
			}
			if !ok {
				return rejReqVMAffinity, term.TopologyKey, nil
			}
		}
	}
	if ec.antiAffinity != nil && ec.antiAffinity.VMAntiAffinity != nil {
		for _, term := range ec.antiAffinity.VMAntiAffinity.RequiredDuringScheduling {
			ok, matchErr := ec.hostRunsMatchingVM(h.Name, term)
			if matchErr != nil {
				return "", "", matchErr
			}
			if ok {
				return rejReqVMAntiAffinity, term.TopologyKey, nil
			}
		}
	}
	return "", "", nil
}

// hostAntiAffinityCap resolves the effective per-host VM cap for a HostAntiAffinity
// rule: the configured MaxVMsPerHost, or defaultMaxVMsPerHost when unset.
func hostAntiAffinityCap(haa *v1beta1.HostAntiAffinityRule) int {
	if haa.MaxVMsPerHost != nil {
		return int(*haa.MaxVMsPerHost)
	}
	return defaultMaxVMsPerHost
}
