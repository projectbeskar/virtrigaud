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

	"github.com/projectbeskar/virtrigaud/internal/clustered"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// Host-encoded task references (ADR-0007 Addendum A, A1, slice 3): the
// format, its parser and the refusals are hypervisor-neutral and live in
// internal/clustered (taskref.go). This file routes the clustered libvirt
// TaskStatus to the encoded host. Only a host the registry fronts is ever
// leased (withHostConn resolves ids through the registry, never through the
// reference).

// routedTaskComplete is the CLUSTERED TaskStatus (ADR-0007 Addendum A, slice
// 3): it decodes the host from ref, refuses a reference this provider did not
// issue, and routes the check to that host's leased connection.
//
//   - a reference that is not host-encoded or is malformed: InvalidSpec;
//   - a host id that is not in this provider's host registry (unknown, removed
//     or draining): NotFound — the reference is never parsed into an endpoint
//     and nothing is dialed;
//   - a host that is in the registry but cannot be reached: the retryable,
//     host-scoped Unavailable of withHostConn.
func (p *Provider) routedTaskComplete(ctx context.Context, ref string) (bool, error) {
	host, inner, err := clustered.ParseHostTaskRef(ref)
	if err != nil {
		return false, contracts.NewInvalidSpecError(
			fmt.Sprintf("clustered libvirt provider refuses the task reference: %v", err), nil)
	}
	if !p.fronts(host) {
		return false, clustered.NewTaskHostNotFrontedError(host)
	}
	var done bool
	err = p.withHostConn(ctx, string(host), func(ctx context.Context, c libvirtConn) error {
		var terr error
		done, terr = hostTaskComplete(ctx, c, inner)
		return terr
	})
	return done, err
}

// fronts reports whether host is a routable host of this clustered provider's
// registry (known and not draining).
func (p *Provider) fronts(host hostconn.HostID) bool {
	if p.clusterReg == nil {
		return false
	}
	return clustered.Fronts(p.clusterReg, host)
}

// hostTaskComplete reports whether the host-local task inner, on the host
// behind c, is complete. Every libvirt operation a routed call runs today is
// synchronous — it has finished by the time the call returns, and returns no
// task — so no host-local task reference is ever issued, and one that reaches
// here was not issued by this provider: it is answered
// clustered.UnknownHostTaskError, never done (ADR-0016 A7; the clustered conformance case
// tasks.forged-inner-ref). A host-side asynchronous operation (the ADR-0007
// D5 MigrateVM job) must keep a record of the references it issues and check
// inner against it here.
func hostTaskComplete(_ context.Context, c libvirtConn, _ string) (bool, error) {
	return false, &clustered.UnknownHostTaskError{Host: c.HostID()}
}
