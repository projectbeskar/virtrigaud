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

package clustered

import (
	"fmt"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
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
// is dialed for it. Only a host the registry fronts is ever reached (the
// provider resolves ids through its registry, never through the reference).

const (
	// HostTaskRefPrefix starts every host-encoded task reference; "v1" is its
	// format version.
	HostTaskRefPrefix = "host-task/v1/"

	// HostTaskRefMaxLen bounds a task reference a clustered provider accepts.
	HostTaskRefMaxLen = 1024

	// HostIDMaxLen is the longest Host object name (a DNS-1123 subdomain).
	HostIDMaxLen = hostsecret.HostIDMaxLen
)

// EncodeHostTaskRef returns the host-encoded form of the host-local task
// reference inner, returned by a routed call that ran on host. An empty inner
// means the call completed synchronously and stays empty: there is no task.
func EncodeHostTaskRef(host HostID, inner string) string {
	if inner == "" {
		return ""
	}
	return HostTaskRefPrefix + string(host) + "/" + inner
}

// ParseHostTaskRef splits a host-encoded task reference into its host id and
// host-local reference. It checks syntax only (the host id must be a DNS-1123
// subdomain and the inner reference non-empty); the caller checks the host
// against its registry (Fronts). The error message never echoes more of the
// reference than its host id.
func ParseHostTaskRef(ref string) (HostID, string, error) {
	if len(ref) > HostTaskRefMaxLen {
		return "", "", fmt.Errorf("task reference is longer than %d bytes", HostTaskRefMaxLen)
	}
	rest, ok := strings.CutPrefix(ref, HostTaskRefPrefix)
	if !ok {
		return "", "", fmt.Errorf("task reference is not host-encoded (%s<host>/<task>)", HostTaskRefPrefix)
	}
	host, inner, ok := strings.Cut(rest, "/")
	if !ok || inner == "" {
		return "", "", fmt.Errorf("host-encoded task reference has no task after its host")
	}
	if !hostsecret.ValidHostID(host) {
		return "", "", fmt.Errorf("host-encoded task reference names an invalid host id")
	}
	return HostID(host), inner, nil
}

// NewTaskHostNotFrontedError is the NotFound answer for a well-formed task
// reference whose host is not in the provider's host registry (unknown,
// removed or draining): the reference was not issued by this provider, and
// nothing is dialed for it.
func NewTaskHostNotFrontedError(host HostID) *contracts.ProviderError {
	return contracts.NewNotFoundError(fmt.Sprintf(
		"task host %q is not in this clustered provider's host registry; the task reference was not issued by it", host), nil)
}

// UnknownHostTaskError is the answer for a host-local task reference that
// the host never issued (ADR-0016 A7, UNKNOWN_TO_AGENT): one that reaches
// TaskStatus — well-formed, naming a host the registry fronts — but of which
// the host keeps no record was forged or comes from elsewhere. TypedStatus
// answers it codes.NotFound with the UNKNOWN_TO_AGENT ErrorInfo: never done,
// and never "not performed". The message names the host only, never the
// reference.
type UnknownHostTaskError struct {
	// Host is the host that has no record of the task.
	Host HostID
}

// Error says the host has no record of the task.
func (e *UnknownHostTaskError) Error() string {
	return fmt.Sprintf("host %q has no record of the task: the task reference was not issued by this provider", e.Host)
}
