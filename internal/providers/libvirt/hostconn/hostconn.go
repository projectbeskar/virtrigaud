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

// Package hostconn defines the per-host connection seam for the libvirt
// provider: a HostID, a Conn that abstracts execution against one hypervisor
// host, and a Registry that owns the lifecycle of those connections.
//
// # Why this package exists (ADR-0008 PR 2 / ADR-0007 P1)
//
// Historically the libvirt provider was welded to a single host: one
// PROVIDER_ENDPOINT built one VirshProvider, and the gRPC layer reached the
// concrete type to run commands. That makes it impossible to (a) hold N
// host-keyed connections (ADR-0007's clustered provider) or (b) swap the
// transport underneath (ADR-0008's in-process SSH, then go-libvirt) without
// rewriting the whole package.
//
// The seam breaks that weld. Everything that needs to talk to a host obtains a
// Conn from the Registry and talks to the interface, never to a driver. Today
// the Registry holds exactly one Conn (built from PROVIDER_ENDPOINT); ADR-0007
// P1 changes only the Registry's constructor to project N hosts from a mounted
// Secret. The interfaces here deliberately do not assume a single host.
//
// # Control-plane exec vs shell exec are kept separable
//
// Conn splits Virsh (control-plane exec) from RunHost/Stream (shell exec) even
// though both run through virsh-over-SSH today. This is deliberate: ADR-0008
// PR 4 adds a Libvirt() *libvirt.Libvirt accessor and routes control operations
// through the pure-Go go-libvirt client, while the shell operations
// (qemu-img, scp, sudo, bash, genisoimage, ...) stay on SSH permanently — a
// libvirt RPC client structurally cannot run them (ADR-0008 Fact 1). Keeping the
// two conceptually distinct now means PR 4 changes an implementation, not the
// seam.
package hostconn

import (
	"context"
	"io"
	"time"
)

// HostID identifies one hypervisor host. Today exactly one exists, derived from
// PROVIDER_ENDPOINT; under ADR-0007 it equals the Host CR name.
type HostID string

// Result is the outcome of one command executed against a host. It is the
// transport-neutral counterpart of the libvirt provider's internal virsh result
// type: the seam returns this so the hostconn package carries no dependency on
// any particular driver's result type.
type Result struct {
	// Command is a human-readable rendering of the executed command (for logs).
	Command string
	// ExitCode is the process exit code (0 on success).
	ExitCode int
	// Stdout is the command's standard output.
	Stdout string
	// Stderr is the command's standard error.
	Stderr string
	// Duration is how long the command took.
	Duration time.Duration
}

// Conn is one live connection to one host. Every method rides virsh over the
// current SSH-subprocess transport today; the method set is split so ADR-0008
// PR 4 can add a Libvirt() *libvirt.Libvirt accessor and route control-plane
// exec through go-libvirt while shell exec stays on SSH.
//
// A Conn must not be cached across calls by higher layers: obtain it from the
// Registry at the point of use. ADR-0008 PR 4's watchdog can close and evict the
// underlying handle at any moment, so a cached Conn is a use-after-evict waiting
// to happen.
type Conn interface {
	// HostID returns the id of the host this connection targets.
	HostID() HostID

	// Virsh runs a virsh control command against the host (control-plane exec).
	// ADR-0008 PR 4 routes this through go-libvirt; today it is virsh over SSH.
	Virsh(ctx context.Context, args ...string) (*Result, error)

	// RunHost runs a command directly on the host (shell exec): the permanent
	// SSH tenants — qemu-img, scp, sudo, bash, genisoimage, and friends — that a
	// libvirt RPC client structurally cannot replace (ADR-0008 Fact 1).
	RunHost(ctx context.Context, argv ...string) (*Result, error)

	// Stream runs a host command and returns its stdout as a stream: the
	// export/import data plane (ADR-0006), so a multi-GB transfer is not buffered
	// in the pod. The caller must Close the returned reader.
	Stream(ctx context.Context, argv ...string) (io.ReadCloser, error)

	// Close releases the connection's resources. It is invoked by the Registry on
	// Evict/Close; ADR-0008 PR 4 closes the ssh.Client / go-libvirt handle here.
	Close() error
}

// Registry owns the lifecycle of all Conns. It holds exactly one host today
// (built from PROVIDER_ENDPOINT); ADR-0007 P1 changes only the constructor to
// project N hosts from a mounted Secret — the interface never assumes one host.
type Registry interface {
	// ConnFor returns the connection for id, or an error if no such host is
	// configured. Callers resolve the Conn here at the point of use rather than
	// caching it.
	ConnFor(ctx context.Context, id HostID) (Conn, error)

	// Hosts returns the ids of every host the Registry serves, sorted.
	Hosts() []HostID

	// Evict removes and closes the connection for id, if present. A subsequent
	// ConnFor(id) fails until the host is re-registered.
	Evict(id HostID)

	// Close closes every connection the Registry holds and empties it.
	Close() error
}
