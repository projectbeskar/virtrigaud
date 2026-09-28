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
	"regexp"
	"slices"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// Host-encoded task references (ADR-0007 Addendum A, A1, slice 3).
//
// TaskStatus is not per-VM: the operator polls a task by its reference alone,
// with no target_host_id. So on a CLUSTERED provider every task reference a
// routed call returns carries the host it ran on:
//
//	host-task/v1/<host id>/<host-local reference>
//
// The format is unambiguous: a host id is a Host object name (a DNS-1123
// subdomain: lowercase letters, digits, '-' and '.'), so it can never contain
// the '/' that ends it, and everything after that '/' is the host-local
// reference, verbatim (it may itself contain '/'). A single-host provider never
// encodes anything, so its task references are byte-identical to before, and
// its TaskStatus is unchanged.
//
// A clustered provider accepts only a reference it could have issued: one that
// is not host-encoded, is malformed, or names a host that is not in its own
// host registry is refused — it is never parsed into an endpoint, and nothing
// is dialed for it. Only a host the registry fronts is ever leased (withHostConn
// resolves ids through the registry, never through the reference).

const (
	// hostTaskRefPrefix starts every host-encoded task reference; "v1" is its
	// format version.
	hostTaskRefPrefix = "host-task/v1/"

	// hostTaskRefMaxLen bounds a task reference a clustered provider accepts.
	hostTaskRefMaxLen = 1024

	// hostIDMaxLen is the longest Host object name (a DNS-1123 subdomain).
	hostIDMaxLen = 253
)

// hostIDRE matches a Host object name: a DNS-1123 subdomain.
var hostIDRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// encodeHostTaskRef returns the host-encoded form of the host-local task
// reference inner, returned by a routed call that ran on host. An empty inner
// means the call completed synchronously and stays empty: there is no task.
func encodeHostTaskRef(host hostconn.HostID, inner string) string {
	if inner == "" {
		return ""
	}
	return hostTaskRefPrefix + string(host) + "/" + inner
}

// parseHostTaskRef splits a host-encoded task reference into its host id and
// host-local reference. It checks syntax only (the host id must be a DNS-1123
// subdomain and the inner reference non-empty); the caller checks the host
// against its registry. The error message never echoes more of the reference
// than its host id.
func parseHostTaskRef(ref string) (hostconn.HostID, string, error) {
	if len(ref) > hostTaskRefMaxLen {
		return "", "", fmt.Errorf("task reference is longer than %d bytes", hostTaskRefMaxLen)
	}
	rest, ok := strings.CutPrefix(ref, hostTaskRefPrefix)
	if !ok {
		return "", "", fmt.Errorf("task reference is not host-encoded (%s<host>/<task>)", hostTaskRefPrefix)
	}
	host, inner, ok := strings.Cut(rest, "/")
	if !ok || inner == "" {
		return "", "", fmt.Errorf("host-encoded task reference has no task after its host")
	}
	if len(host) > hostIDMaxLen || !hostIDRE.MatchString(host) {
		return "", "", fmt.Errorf("host-encoded task reference names an invalid host id")
	}
	return hostconn.HostID(host), inner, nil
}

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
	host, inner, err := parseHostTaskRef(ref)
	if err != nil {
		return false, contracts.NewInvalidSpecError(
			fmt.Sprintf("clustered libvirt provider refuses the task reference: %v", err), nil)
	}
	if !p.fronts(host) {
		return false, contracts.NewNotFoundError(fmt.Sprintf(
			"task host %q is not in this clustered provider's host registry; the task reference was not issued by it", host), nil)
	}
	var done bool
	err = p.withHostConn(ctx, string(host), func(c libvirtConn) error {
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
	return slices.Contains(p.clusterReg.Hosts(), host)
}

// hostTaskComplete reports whether the host-local task inner, on the host
// behind c, is complete. Every libvirt operation a routed call runs today is
// synchronous — it has finished by the time the call returns, and returns no
// task — so a task reference that reaches here is complete, exactly as the
// single-host IsTaskComplete answers. A host-side asynchronous operation (the
// ADR-0007 D5 MigrateVM job) checks its job on c here.
func hostTaskComplete(_ context.Context, _ libvirtConn, _ string) (bool, error) {
	return true, nil
}
