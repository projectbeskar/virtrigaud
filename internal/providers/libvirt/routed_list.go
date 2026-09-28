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
	stderrors "errors"
	"fmt"
	"log"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// A clustered ListVMs runs on every routable host of the registry (ADR-0007
// Addendum A, A3). It is the one call that does not go to a single host.
//
// Fan-out design:
//
//   - Concurrency is bounded (clusteredListHostConcurrency hosts at a time,
//     an errgroup limit), so a large cluster cannot open one SSH session per
//     host at once.
//   - Every host gets its own deadline (clusteredListHostTimeout), derived
//     from the call's budget, so one dead or hung host cannot starve the
//     others.
//   - The call's budget is the caller's deadline less
//     clusteredListResponseMargin, so the answer — with the hosts that did not
//     finish in time reported unreachable — still reaches the caller before
//     the caller gives up. A host not yet started when the budget is spent is
//     not dialed and is reported unreachable.
//   - Cancellation: every per-host goroutine runs under a context derived from
//     the caller's, and the call waits for all of them (errgroup.Wait) before
//     it returns, so none outlives the RPC. The only work that can outlive it
//     is the detached, time-bounded ADR-0008 list shadow, which holds its own
//     reference on the host lease (maybeShadowList).
//   - A host that could not be listed — unknown or draining in the registry,
//     unreachable, past its deadline, or whose `virsh list` failed — goes to
//     VMList.UnreachableHostIDs, never silently dropped. Such failures never
//     fail the call, so they never reach the manager's per-Provider circuit
//     breaker (a host-scoped failure must not fast-fail every other host). The
//     cause is logged per host, classified as the routed calls classify it
//     (HOST_UNAVAILABLE for a host that could not be reached).
//   - Every VMInfo is tagged with its host id: on a clustered provider a VM is
//     identified by (host id, VM id), never by name alone.
//
// Draining hosts are not listed: the registry no longer routes to them (they
// are being removed from the inventory), and the manager's own Host objects
// say which hosts the Provider fronts.
const (
	// clusteredListHostConcurrency bounds how many hosts one clustered
	// ListVMs lists at once.
	clusteredListHostConcurrency = 8
	// clusteredListHostTimeout is each host's deadline in a clustered ListVMs:
	// long enough for one `virsh list` plus a `virsh dumpxml` per domain of a
	// busy host over its persistent SSH connection, short enough that a hung
	// host frees its slot well inside the manager's 2-minute ListVMs deadline.
	clusteredListHostTimeout = 60 * time.Second
	// clusteredListResponseMargin is kept back from the caller's deadline so a
	// clustered ListVMs answers — reporting the hosts it could not finish as
	// unreachable — before the caller's deadline expires.
	clusteredListResponseMargin = 5 * time.Second
	// clusteredListMaxDomainsPerHost bounds how many domains one host's
	// listing reads (one `virsh dumpxml` each). A host with more is reported
	// unreachable (unknown), with a log line, rather than read partially or
	// past its deadline. Follow-up: the per-domain reads are sequential; a
	// host near this bound needs a batched read (ADR-0008's native list) to
	// fit its per-host deadline.
	clusteredListMaxDomainsPerHost = 2000
)

// hostListResult is one host's outcome in a clustered ListVMs.
type hostListResult struct {
	vms []contracts.VMInfo
	err error
}

// listVMsClustered is the clustered ListVMs (see the fan-out design above).
// It fails as a whole only when the provider itself cannot answer (its
// registry is not initialized: a provider-level Unavailable) or the caller's
// context ended; every host-scoped failure is reported in
// VMList.UnreachableHostIDs instead.
func (p *Provider) listVMsClustered(ctx context.Context) (contracts.VMList, error) {
	if p.clusterReg == nil {
		return contracts.VMList{}, contracts.NewUnavailableError("clustered libvirt provider registry not initialized", nil)
	}
	hosts := p.clusterReg.Hosts()
	log.Printf("INFO Listing virtual machines across %d hosts", len(hosts))

	budget, cancel := listBudget(ctx)
	defer cancel()

	results := make([]hostListResult, len(hosts))
	var g errgroup.Group
	g.SetLimit(p.effectiveListHostConcurrency())
	// Start from a different host on every call, so a budget spent on slow
	// hosts does not always leave the same tail unlisted. Results stay in
	// host order.
	start := 0
	if n := len(hosts); n > 0 {
		start = int(p.listRotation.Add(1) % uint64(n)) // #nosec G115 -- n > 0, the remainder fits an int
	}
	for k := range hosts {
		i := (start + k) % len(hosts)
		id := hosts[i]
		if err := budget.Err(); err != nil {
			// The budget is spent: this host is not dialed at all.
			results[i] = hostListResult{err: err}
			continue
		}
		g.Go(func() error {
			results[i] = p.listOneHost(budget, id)
			return nil // a host's failure is its own result, never the group's
		})
	}
	_ = g.Wait() // every goroutine returns nil; Wait only joins them

	// The caller gave up: nothing it could use is left to return.
	if err := ctx.Err(); err != nil {
		return contracts.VMList{}, err
	}

	var out contracts.VMList
	for i, id := range hosts {
		r := results[i]
		if r.err != nil {
			log.Printf("WARN ListVMs: host %s could not be listed (%s); reporting it unreachable, its VMs unknown: %v",
				id, listFailureClass(r.err), r.err)
			out.UnreachableHostIDs = append(out.UnreachableHostIDs, string(id))
			continue
		}
		out.VMs = append(out.VMs, r.vms...)
	}
	return out, nil
}

// listBudget returns the context a clustered ListVMs fans out under: the
// caller's, with its deadline brought forward by clusteredListResponseMargin
// (or by half of what is left, when less than twice the margin is left). A
// caller without a deadline gets no budget beyond the per-host deadlines.
func listBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	margin := clusteredListResponseMargin
	if left := time.Until(deadline); left < 2*margin {
		margin = left / 2
	}
	return context.WithDeadline(ctx, deadline.Add(-margin))
}

// listOneHost lists host id's VMs on its own lease with its own deadline.
func (p *Provider) listOneHost(ctx context.Context, id hostconn.HostID) hostListResult {
	hctx, cancel := context.WithTimeout(ctx, p.effectiveListHostTimeout())
	defer cancel()
	if err := hctx.Err(); err != nil {
		return hostListResult{err: err}
	}
	var vms []contracts.VMInfo
	err := p.withHostConn(hctx, string(id), func(c libvirtConn) error {
		list := p.listHostVMsFn
		if list == nil {
			list = p.listHostVMs
		}
		var lerr error
		vms, lerr = list(hctx, c)
		return lerr
	})
	if err != nil {
		return hostListResult{err: err}
	}
	// The per-host deadline passing during the listing leaves a partial or
	// failed answer; it is never reported as the host's complete list.
	if err := hctx.Err(); err != nil {
		return hostListResult{err: err}
	}
	return hostListResult{vms: vms}
}

// listHostVMs is the default per-host listing of a clustered ListVMs, on the
// host's leased connection c: the same core as single-host (listVMsOn), every
// VMInfo tagged with the host, then the host's ADR-0008 list shadow on the
// same lease (compared on host id and VM id).
//
// Unlike single-host, a domain whose definition could not be read because the
// host itself stopped answering (isHostTransportFailure) fails the host's
// listing: the host is then reported unreachable (unknown), never as a host
// that has fewer VMs. A domain whose definition the host did return but that
// cannot be used (unparseable, an unexpected memory unit) is skipped, as on
// single-host.
func (p *Provider) listHostVMs(ctx context.Context, c libvirtConn) ([]contracts.VMInfo, error) {
	vp, err := virshOf(c)
	if err != nil {
		return nil, err
	}
	var transportErr error
	vms, err := p.listVMsOn(ctx, vp, listOptions{
		onReadFailure: func(_ string, rerr error) {
			if transportErr == nil && isHostTransportFailure(rerr) {
				transportErr = rerr
			}
		},
		stampReport: true,
		maxDomains:  clusteredListMaxDomainsPerHost,
	})
	if err != nil {
		return nil, err
	}
	if transportErr != nil {
		return nil, fmt.Errorf("host stopped answering while its domains were read: %w", transportErr)
	}
	tagHost(vms, c.HostID())
	p.maybeShadowList(ctx, c, vms)
	return vms, nil
}

// tagHost sets every VMInfo's host id to host.
func tagHost(vms []contracts.VMInfo, host hostconn.HostID) {
	for i := range vms {
		vms[i].HostID = string(host)
	}
}

// listFailureClass names, for the provider log, why a host could not be
// listed, with the classification the routed calls use.
func listFailureClass(err error) string {
	switch {
	case contracts.IsHostUnavailable(err) || isHostTransportFailure(err):
		return "host unavailable"
	case stderrors.Is(err, context.DeadlineExceeded):
		return "no answer within the per-host deadline"
	default:
		return "listing failed"
	}
}

// effectiveListHostTimeout is p.listHostTimeout, or clusteredListHostTimeout.
func (p *Provider) effectiveListHostTimeout() time.Duration {
	if p.listHostTimeout > 0 {
		return p.listHostTimeout
	}
	return clusteredListHostTimeout
}

// effectiveListHostConcurrency is p.listHostConcurrency, or
// clusteredListHostConcurrency.
func (p *Provider) effectiveListHostConcurrency() int {
	if p.listHostConcurrency > 0 {
		return p.listHostConcurrency
	}
	return clusteredListHostConcurrency
}
