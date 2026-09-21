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
	"os/exec"
	"strings"

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
	// copyDiskToRemote scp's a local disk file into the host image pool and
	// returns the remote path.
	copyDiskToRemote(ctx context.Context, localPath, volumeName string) (string, error)
	// storageProvider returns the storage helper bound to this host.
	storageProvider() *StorageProvider
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
// export/import data plane, ADR-0006). It reuses the existing, tested
// runSSHStdout helper behind an io.Pipe so the transfer is not buffered in the
// pod and holds a streamSem (not execSem) slot for its duration. The caller MUST
// Close the returned reader.
//
// ADR-0008 PR 3 migrates the export/import call sites onto this method as part of
// moving to in-process SSH; for now it is the seam's first-class streaming
// primitive (targeting the SSH host path, as export/import always do).
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

// Close releases the connection. Virsh-over-subprocess holds no persistent
// socket today (Cleanup is a no-op); ADR-0008 PR 4 closes the ssh.Client /
// go-libvirt handle here.
func (c *virshConn) Close() error { return c.virsh.Cleanup() }

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
// libvirt host over scp, returning the remote path.
//
// This method was moved off the gRPC Server (ADR-0008 PR 2) so the server layer
// no longer names *VirshProvider; the body is unchanged. It is reached through
// the libvirtConn seam (virshConn.copyDiskToRemote). ADR-0008 PR 3 moves this
// disk-stream site onto in-process SSH along with the other scp/ssh sites.
func (v *VirshProvider) copyDiskToRemote(ctx context.Context, localPath, volumeName string) (string, error) {
	// IMPORTANT: Copy directly to libvirt pool directory for efficient in-place usage
	// This allows CreateVolumeFromImageFile to detect and use the disk without copying
	remoteDir := "/var/lib/libvirt/images"
	remotePath := fmt.Sprintf("%s/%s.qcow2", remoteDir, volumeName)

	// Extract SSH target (user@host) from URI
	parsedURI, err := url.Parse(v.uri)
	if err != nil {
		return "", fmt.Errorf("failed to parse libvirt URI: %w", err)
	}

	user := parsedURI.User.Username()
	host := parsedURI.Host
	sshTarget := fmt.Sprintf("%s@%s", user, host)

	log.Printf("INFO Ensuring libvirt pool directory exists on %s", sshTarget)

	// Ensure pool directory exists (usually already exists, but safe to check)
	_, err = v.runVirshCommand(ctx, "!", "sudo", "mkdir", "-p", remoteDir)
	if err != nil {
		log.Printf("WARN Failed to ensure pool directory exists (may already exist): %v", err)
	}

	// Copy disk file using scp (run locally from the pod, not through SSH)
	log.Printf("INFO Copying disk file (%s) to remote host via scp...", localPath)

	// Host-key options come from the same centralized policy as the virsh
	// paths (#149/ADR-0004) so the disk-image transfer is verified against the
	// same trust material. Re-emit the verification-mode audit line for the scp
	// connection and hard-fail before transfer if verification is on but no
	// usable known_hosts is present (no TOFU).
	v.hostKey.logVerificationMode(v.logger, host)
	if err := v.hostKey.verifyKnownHostsPresent(host); err != nil {
		return "", fmt.Errorf("libvirt scp host-key verification pre-flight failed: %w", err)
	}
	hostKeyOpts := v.hostKey.sshHostKeyOptions()
	// Share the SSH connection with the virsh path via ControlMaster (#194).
	hostKeyOpts = append(hostKeyOpts, sshMultiplexOptions()...)

	// Bound concurrent long-lived disk-stream forks separately from execSem's
	// short control-call budget (see VirshProvider.streamSem) — scp can hold
	// this subprocess for minutes copying a multi-GB disk, and must not
	// starve, or be starved by, short virsh control calls. Scoped to just the
	// scp fork itself (not the mkdir above, which already has its own
	// execSem-guarded call via runVirshCommand) so the two budgets stay
	// independent.
	release, err := v.acquireStreamSlot(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	// Run scp LOCALLY on the pod to copy to remote host
	var cmd *exec.Cmd
	if v.credentials.Password != "" {
		// Use sshpass with scp for password authentication
		scpArgs := append([]string{"-e", "scp"}, hostKeyOpts...)
		scpArgs = append(scpArgs, localPath, fmt.Sprintf("%s:%s", sshTarget, remotePath))
		cmd = exec.CommandContext(ctx, "sshpass", scpArgs...)
		// Set password via environment variable for sshpass
		cmd.Env = append(os.Environ(), fmt.Sprintf("SSHPASS=%s", v.credentials.Password))
	} else if strings.TrimSpace(v.credentials.SSHPrivateKey) != "" {
		scpArgs := sshKeyAuthOptions(resolveSSHKeyFile(parsedURI))
		scpArgs = append(scpArgs, hostKeyOpts...)
		scpArgs = append(scpArgs, localPath, fmt.Sprintf("%s:%s", sshTarget, remotePath))
		cmd = exec.CommandContext(ctx, "scp", scpArgs...)
	} else {
		scpArgs := append([]string{}, hostKeyOpts...)
		scpArgs = append(scpArgs, localPath, fmt.Sprintf("%s:%s", sshTarget, remotePath))
		cmd = exec.CommandContext(ctx, "scp", scpArgs...)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("scp failed: %w, output: %s", err, string(output))
	}

	log.Printf("INFO Successfully copied disk file to remote host: %s", remotePath)
	return remotePath, nil
}
