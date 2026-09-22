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

package libvirt

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
)

// providerBackend is the surface the gRPC Server (server.go) needs from the
// libvirt provider implementation. Holding this interface — instead of
// type-asserting the concrete *Provider, as server.go did at six RPCs
// (ADR-0008 Fact 3) — is what lets an alternative transport (ADR-0008 PR 3/PR 4)
// be swapped underneath the provider without touching the gRPC layer.
//
// It is satisfied by *Provider. server_test.go provides fakes so the de-welded
// RPCs can be exercised without a live libvirt host.
type providerBackend interface {
	contracts.Provider

	// Clone creates a VM clone (RPC Clone). It is defined on *Provider
	// (clone.go), not on contracts.Provider, so the Server reaches it here rather
	// than via a concrete type assertion.
	Clone(ctx context.Context, req contracts.CloneRequest) (contracts.CloneResponse, error)

	// imagePrepare imports/prepares a VM image into a storage pool (RPC
	// ImagePrepare). It is defined on *Provider (image.go).
	imagePrepare(ctx context.Context, imageJSON, targetName, storageHint string) (preparedID, preparedPath string, err error)

	// conn returns the connection for the provider's host. Today there is exactly
	// one host (from PROVIDER_ENDPOINT); ADR-0007 P1 changes only the provider's
	// constructor to project N. The Server obtains its per-host handle here
	// instead of reaching into an unexported *Provider field.
	conn(ctx context.Context) (libvirtConn, error)

	// clustered reports whether the provider is running in CLUSTERED topology
	// (ADR-0007 D3) — fronting N host-keyed connections from a mounted inventory
	// — versus single-host mode. The Server uses it to gate the host-inventory
	// surface: GetCapabilities advertises supports_clustering only when true, and
	// ListHosts/GetHostInfo are real only then (single-host stays Unimplemented,
	// D9). It is the *Provider's clusterReg != nil discriminator, exposed through
	// the seam so the Server never type-asserts the concrete type.
	clustered() bool
}

// libvirtConn is the libvirt-specific view of one host's connection that the
// gRPC Server needs for the snapshot, import and disk RPCs. It embeds the
// transport-neutral hostconn.Conn (HostID/Virsh/RunHost/Stream/Close) and adds
// the virsh query helpers and import/scp operations those RPCs use.
//
// It is the richer, package-local companion to hostconn.Conn: the generic seam
// stays clean for ADR-0007 P1 / ADR-0008 PR 3–4, while this interface carries
// the extras the current gRPC RPCs happen to need. *virshConn satisfies both.
type libvirtConn interface {
	hostconn.Conn

	// getDomainState returns a domain's virsh power state (domstate).
	getDomainState(ctx context.Context, domain string) (string, error)
	// snapshotExists reports whether a named snapshot exists for a domain.
	snapshotExists(ctx context.Context, domain, snapshot string) (bool, error)
	// uri is the libvirt connection URI (the import RPC branches on ssh://).
	uri() string
	// copyDiskToRemote copies a local disk file into the host image pool over
	// the in-process SSH client (ADR-0008 PR 3: `cat > remotePath`, replacing
	// scp) and returns the remote path.
	copyDiskToRemote(ctx context.Context, localPath, volumeName string) (string, error)
	// storageProvider returns the storage helper bound to this host.
	storageProvider() *StorageProvider
	// callLibvirt runs fn against this host's go-libvirt client under the
	// connection-lifecycle watchdog (ADR-0008 PR 4a's golibvirtHolder.call): the
	// ctx-bounded invocation the ADR-0008 PR 4b shadow-compare reads use instead of
	// driving the raw client Libvirt(ctx) returns. It is a libvirtConn extra (not
	// part of the transport-neutral hostconn.Conn seam) because bounding a
	// per-call deadline is a go-libvirt-lifecycle concern the generic seam does not
	// model — Libvirt(ctx) on the seam only bounds the connect, not a subsequent RPC.
	callLibvirt(ctx context.Context, fn func(*golibvirt.Libvirt) error) error
	// StreamIn runs a host command with r wired to its stdin and blocks until
	// the command completes — the input-direction counterpart to Stream (which
	// streams the host's stdout back to the caller). It is a libvirtConn extra
	// (not part of the transport-neutral hostconn.Conn seam) because it exists
	// specifically for the ADR-0006 S3 import path's host-side stage step
	// (`cat > stagePath`), which the generic seam has no need to model.
	StreamIn(ctx context.Context, r io.Reader, argv ...string) error
}

// virshConn is the per-host hostconn.Conn implementation backed by a
// VirshProvider running virsh over the current SSH-subprocess transport. It is
// the single host today; ADR-0007 P1 builds N of these from projected Host CRs,
// and ADR-0008 PR 4 gives it a Libvirt() *libvirt.Libvirt accessor so control
// exec (Virsh) can route through go-libvirt while shell exec (RunHost/Stream)
// stays on SSH.
type virshConn struct {
	id    hostconn.HostID
	virsh *VirshProvider

	// cleanup is an optional teardown hook run by Close AFTER the VirshProvider
	// is cleaned up. It is nil for the single-host path (newVirshConn); a
	// clustered host (newClusteredVirshConn) sets it to remove that host's
	// materialised known_hosts temp file, so a drained/removed host leaves no
	// file behind. Keeping it nil for single-host means Close is byte-for-byte
	// unchanged there.
	cleanup func()
}

// Compile-time proof that *virshConn satisfies both the transport-neutral seam
// and the richer libvirt view the Server holds.
var (
	_ hostconn.Conn = (*virshConn)(nil)
	_ libvirtConn   = (*virshConn)(nil)
)

// newVirshConn wraps a VirshProvider as the per-host connection for id.
func newVirshConn(id hostconn.HostID, vp *VirshProvider) *virshConn {
	return &virshConn{id: id, virsh: vp}
}

// newClusteredVirshConn wraps a per-host VirshProvider (ADR-0007 D3) for id,
// with a cleanup hook Close runs after teardown — used to remove the host's
// materialised known_hosts temp file when the connection is drained/closed.
func newClusteredVirshConn(id hostconn.HostID, vp *VirshProvider, cleanup func()) *virshConn {
	return &virshConn{id: id, virsh: vp, cleanup: cleanup}
}

// HostID returns the id of the host this connection targets.
func (c *virshConn) HostID() hostconn.HostID { return c.id }

// Virsh runs a virsh control command against the host (control-plane exec).
// ADR-0008 PR 4 will route this through go-libvirt; today it is virsh over the
// SSH-subprocess transport, unchanged.
func (c *virshConn) Virsh(ctx context.Context, args ...string) (*hostconn.Result, error) {
	r, err := c.virsh.runVirshCommand(ctx, args...)
	return toResult(r), err
}

// RunHost runs a command directly on the host (shell exec: qemu-img, scp, sudo,
// bash, genisoimage, ...). These are permanent SSH tenants — a libvirt RPC
// client structurally cannot replace them (ADR-0008 Fact 1). It maps onto the
// existing "!"-escape of runVirshCommand, so behaviour is unchanged.
func (c *virshConn) RunHost(ctx context.Context, argv ...string) (*hostconn.Result, error) {
	r, err := c.virsh.runVirshCommand(ctx, append([]string{"!"}, argv...)...)
	return toResult(r), err
}

// Stream runs a host command and returns its stdout as a stream (the
// export/import data plane, ADR-0006). It reuses the runSSHStdout helper
// behind an io.Pipe so the transfer is not buffered in the pod and holds a
// streamSem (not execSem) slot for its duration. The caller MUST Close the
// returned reader.
//
// ADR-0008 PR 3: runSSHStdout now rides the persistent in-process SSH client
// (sshclient.go) instead of forking ssh/sshpass per call; this method's
// signature, contract, and callers are unchanged — exportDiskToS3 (ADR-0006's
// S3 export path) is the primary consumer.
func (c *virshConn) Stream(ctx context.Context, argv ...string) (io.ReadCloser, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("hostconn: Stream requires a command")
	}
	pr, pw := io.Pipe()
	go func() {
		// runSSHStdout streams the remote stdout into pw and returns when the
		// command exits; propagate its error (a nil error closes the reader with
		// io.EOF).
		err := runSSHStdout(ctx, c.virsh, pw, strings.Join(argv, " "))
		_ = pw.CloseWithError(err)
	}()
	return pr, nil
}

// Libvirt returns the pure-Go go-libvirt client for this host, connecting
// lazily over the same persistent SSH client Virsh/RunHost/Stream use
// (ADR-0008 PR 4a). See VirshProvider.Libvirt (golibvirt.go) for the
// connection-lifecycle details — the watchdog, keepalive prober, and
// redial-on-dead that make an unattended go-libvirt connection safe. No
// production code path calls this yet: it exists so ADR-0008 PR 4b's
// shadow-compare reads have a proven connection to build on.
func (c *virshConn) Libvirt(ctx context.Context) (*golibvirt.Libvirt, error) {
	return c.virsh.Libvirt(ctx)
}

// callLibvirt runs fn against this host's go-libvirt client under the
// connection-lifecycle watchdog (VirshProvider.callLibvirt -> golibvirtHolder.call).
// See the libvirtConn.callLibvirt doc: this is the ctx-bounded invocation the
// ADR-0008 PR 4b shadow-compare Describe uses.
func (c *virshConn) callLibvirt(ctx context.Context, fn func(*golibvirt.Libvirt) error) error {
	return c.virsh.callLibvirt(ctx, fn)
}

// StreamIn runs a host command with r wired to its stdin and blocks until the
// command completes (ADR-0008 PR 3, libvirtConn extra): the input-direction
// counterpart to Stream, used by importDiskFromS3's host-side stage step
// (`cat > stagePath`) and by copyDiskToRemote (`cat > remotePath`, the scp
// replacement). It reuses the runSSHStdin helper, which holds a streamSem
// (not execSem) slot for its duration, matching Stream/RunHost.
func (c *virshConn) StreamIn(ctx context.Context, r io.Reader, argv ...string) error {
	if len(argv) == 0 {
		return fmt.Errorf("hostconn: StreamIn requires a command")
	}
	return runSSHStdin(ctx, c.virsh, r, strings.Join(argv, " "))
}

// Close releases the connection: ADR-0008 PR 3 closes the persistent
// in-process SSH client, if one was dialed (sshclient.go); PR 4 adds the
// go-libvirt handle alongside it. It then runs the optional cleanup hook (a
// clustered host's known_hosts temp-file removal); single-host has none, so its
// Close is unchanged.
func (c *virshConn) Close() error {
	err := c.virsh.Cleanup()
	if c.cleanup != nil {
		c.cleanup()
	}
	return err
}

// getDomainState delegates to the wrapped VirshProvider (domstate).
func (c *virshConn) getDomainState(ctx context.Context, domain string) (string, error) {
	return c.virsh.getDomainState(ctx, domain)
}

// snapshotExists delegates to the wrapped VirshProvider (snapshot-list).
func (c *virshConn) snapshotExists(ctx context.Context, domain, snapshot string) (bool, error) {
	return c.virsh.snapshotExists(ctx, domain, snapshot)
}

// uri returns the libvirt connection URI.
func (c *virshConn) uri() string { return c.virsh.uri }

// copyDiskToRemote delegates to the wrapped VirshProvider.
func (c *virshConn) copyDiskToRemote(ctx context.Context, localPath, volumeName string) (string, error) {
	return c.virsh.copyDiskToRemote(ctx, localPath, volumeName)
}

// storageProvider returns a StorageProvider bound to this host's VirshProvider.
func (c *virshConn) storageProvider() *StorageProvider {
	return NewStorageProvider(c.virsh)
}

// toResult converts a virsh *VirshResult into the transport-neutral
// *hostconn.Result the Conn seam returns. Callers return the original error
// unchanged, preserving *VirshError classification.
func toResult(r *VirshResult) *hostconn.Result {
	if r == nil {
		return nil
	}
	return &hostconn.Result{
		Command:  r.Command,
		ExitCode: r.ExitCode,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Duration: r.Duration,
	}
}

// hostIDFromEndpoint derives the HostID for the single host from
// PROVIDER_ENDPOINT. Today one host exists; ADR-0007 P1 replaces this with the
// Host CR name. It uses the URI authority host when parseable, else the raw
// endpoint, else "localhost" for an empty/local endpoint.
func hostIDFromEndpoint(endpoint string) hostconn.HostID {
	if endpoint == "" {
		return hostconn.HostID("localhost")
	}
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return hostconn.HostID(u.Host)
	}
	return hostconn.HostID(endpoint)
}

// copyDiskToRemote copies a disk file from local pod storage to the remote
// libvirt host, returning the remote path.
//
// This method was moved off the gRPC Server (ADR-0008 PR 2); it is reached
// through the libvirtConn seam (virshConn.copyDiskToRemote). ADR-0008 PR 3
// replaces the former scp/sshpass subprocess with `cat > remotePath` over the
// persistent in-process SSH client (runSSHStdin, sshclient.go) — the same
// primitive importDiskFromS3's host-side stage step uses. Host-key
// verification, auth-method selection, and the streamSem budget all now live
// in runSSHStdin's single implementation instead of being re-derived here.
func (v *VirshProvider) copyDiskToRemote(ctx context.Context, localPath, volumeName string) (string, error) {
	// IMPORTANT: Copy directly to libvirt pool directory for efficient in-place usage
	// This allows CreateVolumeFromImageFile to detect and use the disk without copying
	remoteDir := "/var/lib/libvirt/images"
	remotePath := fmt.Sprintf("%s/%s.qcow2", remoteDir, volumeName)

	log.Printf("INFO Ensuring libvirt pool directory exists on remote host")

	// Ensure pool directory exists (usually already exists, but safe to check)
	if _, err := v.runVirshCommand(ctx, "!", "sudo", "mkdir", "-p", remoteDir); err != nil {
		log.Printf("WARN Failed to ensure pool directory exists (may already exist): %v", err)
	}

	log.Printf("INFO Copying disk file (%s) to remote host over SSH...", localPath)

	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open local disk file %s for remote copy: %w", localPath, err)
	}
	defer func() { _ = f.Close() }()

	// remotePath is caller-derived (volumeName) and now lands inside a shell
	// command line (`cat > path`) rather than scp's non-shell destination
	// argument, so it must be shell-quoted here — a necessary adaptation to
	// the transport shape change, not a behavior change to the copy itself.
	remoteCmd := fmt.Sprintf("cat > %s", shellQuote(remotePath))
	if err := runSSHStdin(ctx, v, f, remoteCmd); err != nil {
		return "", fmt.Errorf("disk copy to remote host failed: %w", err)
	}

	log.Printf("INFO Successfully copied disk file to remote host: %s", remotePath)
	return remotePath, nil
}
