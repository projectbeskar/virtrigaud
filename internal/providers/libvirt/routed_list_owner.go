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

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// The owner-filtered ListVMs of a CLUSTERED provider (ADR-0007 A6.2, R4).
//
// Before a clustered VirtualMachine is scheduled for the first time, the
// manager asks every host whether it already holds a domain for that
// VirtualMachine's namespace and name (A6 decision 2: at most one per
// <namespace>.<name> per clustered Provider). The listing it answers is the
// full listing's fan-out (listAcrossHosts: bounded concurrency, a per-host
// deadline inside the caller's, hosts that could not be checked reported
// unreachable, never failing the call), but each host is asked about ONE
// VirtualMachine only, so it costs a `virsh list --all` and at most two
// definition reads per host (plus the persistent definition of a running
// domain), whatever the number of domains there:
//
//   - the domain the provider would name for that VirtualMachine,
//     "<namespace>.<name>" (domainNameFor), is returned whatever its owner
//     stamp — none, another VirtualMachine's, this one's, or one that cannot
//     be read (reported with owner_stamp_state, never skipped: the manager
//     holds on a candidate it cannot classify);
//   - the legacy bare name "<name>" (legacyNameOf) is returned only when its
//     one owner stamp records this namespace and name: a bare name is not
//     namespaced, so another namespace's pre-namespacing VM of the same name
//     is not this VirtualMachine's and is left out.
//
// A domain named otherwise is not looked at (the residual the docs name: only
// an administrator renames domains, and adoption never re-stamps a stamped
// domain until A6.4).
//
// A candidate whose definition cannot be read because the host stopped
// answering fails that host (unreachable: unknown, not empty). The answer is
// marked OwnerFilterApplied. It reads; it never changes anything, and it never
// chooses a host.

// ownerCandidates are the domain names an owner-filtered listing looks at on
// each host.
type ownerCandidates struct {
	filter contracts.OwnerFilter
	// domainName is the namespaced name the provider would give the
	// VirtualMachine ("<namespace>.<name>", shortened when long).
	domainName string
	// legacy is the bare pre-namespacing name, or "" when the VirtualMachine
	// could not have had one.
	legacy string
}

// ownerCandidatesFor validates owner and derives its candidate domain names.
// An incomplete filter, or a namespace or name Kubernetes would not admit
// (they reach virsh arguments), is refused as InvalidSpec.
func ownerCandidatesFor(owner contracts.OwnerFilter) (ownerCandidates, error) {
	if !owner.Complete() {
		return ownerCandidates{}, contracts.NewInvalidSpecError(
			"an owner filter needs both a namespace and a name", nil)
	}
	id := contracts.ObjectIdentity{Namespace: owner.Namespace, Name: owner.Name}
	domainName, err := domainNameFor(id, "")
	if err != nil {
		return ownerCandidates{}, err
	}
	legacy, _ := legacyNameOf(id, owner.Name, domainName)
	return ownerCandidates{filter: owner, domainName: domainName, legacy: legacy}, nil
}

// ListVMsForOwner implements contracts.OwnerFilteredLister (ADR-0007 A6.2,
// R4). On a clustered provider it lists, across every routable host, the
// candidates of the VirtualMachine owner names (see the file comment) and
// marks the answer OwnerFilterApplied. A single-host provider does not honour
// the filter — it never schedules, and its listing is pinned byte for byte —
// so it answers its ordinary, unmarked listing.
func (p *Provider) ListVMsForOwner(ctx context.Context, owner contracts.OwnerFilter) (contracts.VMList, error) {
	if !p.clustered() {
		return p.ListVMs(ctx)
	}
	return p.listVMsClusteredForOwner(ctx, owner)
}

// listVMsClusteredForOwner is the clustered owner-filtered listing.
func (p *Provider) listVMsClusteredForOwner(ctx context.Context, owner contracts.OwnerFilter) (contracts.VMList, error) {
	cand, err := ownerCandidatesFor(owner)
	if err != nil {
		return contracts.VMList{}, err
	}
	list, err := p.listAcrossHosts(ctx, "ListVMs (owner filter)", func(budget context.Context, id hostconn.HostID) hostListResult {
		return p.listOneHostForOwner(budget, id, cand)
	})
	if err != nil {
		return contracts.VMList{}, err
	}
	list.OwnerFilterApplied = true
	return list, nil
}

// listOneHostForOwner lists host id's candidates of cand on its own lease with
// its own deadline (onHostWithin).
func (p *Provider) listOneHostForOwner(ctx context.Context, id hostconn.HostID, cand ownerCandidates) hostListResult {
	var vms []contracts.VMInfo
	err := p.onHostWithin(ctx, id, func(hctx context.Context, c libvirtConn) error {
		var lerr error
		vms, lerr = p.listHostVMsForOwner(hctx, c, cand)
		return lerr
	})
	if err != nil {
		return hostListResult{err: err}
	}
	return hostListResult{vms: vms}
}

// listHostVMsForOwner is one host's owner-filtered listing, on its leased
// connection c: `virsh list --all`, then the candidates' definitions only.
// There is no list shadow (ADR-0008): the shadow compares full listings.
func (p *Provider) listHostVMsForOwner(ctx context.Context, c libvirtConn, cand ownerCandidates) ([]contracts.VMInfo, error) {
	vp, err := virshOf(c)
	if err != nil {
		return nil, err
	}
	domains, err := vp.listDomains(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list domains: %w", err)
	}
	var out []contracts.VMInfo
	for _, d := range domains {
		primary := d.Name == cand.domainName
		legacy := cand.legacy != "" && d.Name == cand.legacy
		if !primary && !legacy {
			continue
		}
		info, err := p.ownerCandidateInfo(ctx, vp, d)
		if err != nil {
			return nil, err
		}
		if legacy && !cand.filter.Matches(info) {
			continue
		}
		out = append(out, info)
	}
	tagHost(out, c.HostID())
	return out, nil
}

// ownerCandidateInfo reads one candidate domain d and reports it as a listing
// does: its owner stamps (clusteredStampReport: both definitions of a running
// domain), uuid, state and size. A definition the host returned but that
// cannot be read or parsed is reported with owner_stamp_state "unreadable"
// rather than skipped, so the caller can hold on it; a read that failed
// because the host stopped answering is returned as an error (the host is
// then unknown).
func (p *Provider) ownerCandidateInfo(ctx context.Context, vp *VirshProvider, d VirshDomain) (contracts.VMInfo, error) {
	info := contracts.VMInfo{
		ID:         d.Name,
		Name:       d.Name,
		PowerState: string(p.mapLibvirtPowerState(d.State)),
		ProviderRaw: map[string]string{
			"domain_name": d.Name,
			"domain_id":   d.ID,
			"state":       d.State,
		},
	}
	info.ProviderRaw["power_state"] = info.PowerState
	unreadable := func(why string, err error) (contracts.VMInfo, error) {
		if isHostTransportFailure(err) {
			return contracts.VMInfo{}, fmt.Errorf("host stopped answering while domain %s was read: %w", d.Name, err)
		}
		log.Printf("WARN ListVMs (owner filter): domain %s %s: %v; reporting its owner stamp unreadable", d.Name, why, err)
		info.ProviderRaw[contracts.VMInfoOwnerStampStateKey] = contracts.OwnerStampUnreadable
		return info, nil
	}

	raw, err := vp.runVirshCommand(ctx, "dumpxml", "--domain", d.Name)
	if err != nil {
		return unreadable("could not be read", err)
	}
	dx, err := parseDomainXML(raw.Stdout)
	if err != nil {
		return unreadable("could not be parsed", err)
	}
	if dx.UUID != "" {
		info.ProviderRaw[contracts.VMInfoUUIDKey] = dx.UUID
	}
	info.CPU = dx.VCPU
	if mem, merr := dx.MemoryMiB(); merr == nil {
		info.MemoryMiB = mem
	}
	info.Disks = dx.Disks(d.Name)

	var transportErr error
	sr := clusteredStampReport(ctx, vp, d, raw.Stdout, dx.UUID, func(_ string, rerr error) {
		if transportErr == nil && isHostTransportFailure(rerr) {
			transportErr = rerr
		}
	})
	if transportErr != nil {
		return contracts.VMInfo{}, fmt.Errorf("host stopped answering while domain %s was read: %w", d.Name, transportErr)
	}
	if sr.uids != "" {
		info.ProviderRaw[contracts.VMInfoOwnerUIDKey] = sr.uids
	}
	if sr.state != "" {
		info.ProviderRaw[contracts.VMInfoOwnerStampStateKey] = sr.state
	}
	info.OwnerNamespace, info.OwnerName = sr.owner.Namespace, sr.owner.Name
	return info, nil
}
