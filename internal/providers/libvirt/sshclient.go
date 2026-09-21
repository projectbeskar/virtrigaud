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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// This file is the ADR-0008 PR 3 in-process SSH transport: it replaces the
// former exec.Command("ssh"/"sshpass"/"scp", ...) subprocess fan-out with a
// single, lazily-dialed, reused golang.org/x/crypto/ssh.Client per
// VirshProvider (== per host). Every virsh command AND every host shell
// command (the "!" escape) that targets an ssh:// endpoint funnels through
// here; virsh's own text output and the package's parsers are completely
// unchanged — only how the bytes get to and from the remote host changes.
//
// Lifecycle (deliberately minimal — see the package doc on VirshProvider.
// sshClient for what is explicitly OUT of scope here and deferred to
// ADR-0008 PR 4): connect lazily on first use, reuse the *ssh.Client across
// every subsequent Virsh/RunHost/Stream/StreamIn call, and redial once if a
// cached client turns out to be dead (a new session fails to open). There is
// no background keepalive, no proactive liveness probe, and no per-call
// watchdog — PR 4 owns that. The fork-per-call model being replaced
// "self-heals" for free (every call is a brand new process); a naive
// persistent client without even this basic redial would be a regression, so
// redial-on-broken is the one piece of lifecycle PR 3 must carry.

// sshHandshakeTimeout bounds the SSH handshake (post-TCP-connect) portion of a
// dial. ssh.ClientConfig has no context-based cancellation of its own; the TCP
// connect phase is still fully ctx-aware via net.Dialer.DialContext.
const sshHandshakeTimeout = 30 * time.Second

// sshAuthMethods selects the SSH authentication method for creds, preserving
// the exact precedence the former argv-based transport used: password wins
// when both a password and a private key are configured (this was the
// existing behavior at every ssh:// call site — never a deliberate policy
// choice being introduced here, just carried forward unchanged). Both
// methods remain fully supported in-process; neither is deprecated by this
// change (ADR-0008 D8, the password-auth removal, is a separate, not-yet-made
// decision).
func sshAuthMethods(creds *Credentials) ([]ssh.AuthMethod, error) {
	switch {
	case creds != nil && creds.Password != "":
		return []ssh.AuthMethod{ssh.Password(creds.Password)}, nil
	case creds != nil && strings.TrimSpace(creds.SSHPrivateKey) != "":
		signer, err := ssh.ParsePrivateKey([]byte(creds.SSHPrivateKey))
		if err != nil {
			return nil, fmt.Errorf("parse SSH private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	default:
		return nil, errors.New("no SSH authentication method configured (need a password or an SSH private key)")
	}
}

// dialSSH performs one full dial: TCP connect (ctx-aware) + SSH handshake
// (auth + host-key verification), returning a fresh *ssh.Client. It re-runs
// the ADR-0004 host-key pre-flight (log + hard-fail-if-missing) on every dial,
// exactly as the former transport re-verified on every subprocess spawn — so a
// known_hosts rotation or a revoked opt-out is honored on the very next
// reconnect, not just at pod startup.
func (v *VirshProvider) dialSSH(ctx context.Context) (*ssh.Client, error) {
	parsedURI, err := url.Parse(v.uri)
	if err != nil {
		return nil, fmt.Errorf("parse libvirt URI: %w", err)
	}
	host := parsedURI.Host
	user := parsedURI.User.Username()

	v.hostKey.logVerificationMode(v.logger, host)
	if err := v.hostKey.verifyKnownHostsPresent(host); err != nil {
		return nil, fmt.Errorf("ssh host-key verification pre-flight failed: %w", err)
	}
	hostKeyCB, err := v.hostKey.hostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("build ssh host-key callback: %w", err)
	}
	auth, err := sshAuthMethods(v.credentials)
	if err != nil {
		return nil, fmt.Errorf("build ssh auth method: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hostKeyCB,
		Timeout:         sshHandshakeTimeout,
	}

	addr := hostPort(host)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial ssh host %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake with %s failed: %w", addr, err)
	}
	log.Printf("INFO Established SSH connection to libvirt host %s", addr)
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// ensureSSHClient returns the provider's persistent SSH client, dialing it on
// first use. Subsequent calls return the SAME client (no reconnect) — reuse is
// the entire point of this PR, replacing a fork+handshake per virsh/shell
// command with one connection multiplexing many sessions.
func (v *VirshProvider) ensureSSHClient(ctx context.Context) (*ssh.Client, error) {
	v.sshMu.Lock()
	defer v.sshMu.Unlock()
	if v.sshClient != nil {
		return v.sshClient, nil
	}
	client, err := v.dialSSH(ctx)
	if err != nil {
		return nil, err
	}
	v.sshClient = client
	return client, nil
}

// resetSSHClient discards and closes the cached client, if any, so the next
// ensureSSHClient call redials. This is the "basic redial if the client is
// dead" mechanism: it is invoked by withSession when opening a new session on
// the cached client fails, which is the observable symptom of a broken
// connection (remote libvirtd restart, host reboot, idle-timeout drop, etc.).
func (v *VirshProvider) resetSSHClient() {
	v.sshMu.Lock()
	old := v.sshClient
	v.sshClient = nil
	v.sshMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// withSession runs fn against a session opened on the provider's persistent
// SSH client, redialing exactly once if the cached client is dead (see
// resetSSHClient). The session is always closed before withSession returns.
func (v *VirshProvider) withSession(ctx context.Context, fn func(*ssh.Session) error) error {
	client, err := v.ensureSSHClient(ctx)
	if err != nil {
		return err
	}
	sess, err := client.NewSession()
	if err != nil {
		// The cached client is likely dead (e.g. the remote end dropped the
		// TCP connection); discard it and try exactly once more against a
		// fresh dial. A second failure here is a real, current connectivity
		// problem, not a stale-handle artifact, so it is returned as-is.
		v.resetSSHClient()
		client, err = v.ensureSSHClient(ctx)
		if err != nil {
			return fmt.Errorf("reconnect ssh client: %w", err)
		}
		sess, err = client.NewSession()
		if err != nil {
			return fmt.Errorf("open ssh session: %w", err)
		}
	}
	defer func() { _ = sess.Close() }()
	return fn(sess)
}

// dialTunnel opens a new channel multiplexed over the provider's persistent
// SSH client — a raw net.Conn tunneled to network/addr on the remote host,
// e.g. ("unix", libvirtSocketPath) for ADR-0008 PR 4a's go-libvirt
// socket.Dialer (golibvirt.go). It mirrors withSession's redial-once-on-a
// dead-cached-client behavior: opening a channel has no exec/session
// semantics to fail the way NewSession does, so staleness is detected the
// same way — the operation on the cached client fails — and exactly one
// fresh dial is tried before the failure is returned as-is.
func (v *VirshProvider) dialTunnel(ctx context.Context, network, addr string) (net.Conn, error) {
	client, err := v.ensureSSHClient(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := client.Dial(network, addr)
	if err != nil {
		v.resetSSHClient()
		client, err = v.ensureSSHClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("reconnect ssh client: %w", err)
		}
		conn, err = client.Dial(network, addr)
		if err != nil {
			return nil, fmt.Errorf("open ssh-tunneled %s socket %s: %w", network, addr, err)
		}
	}
	return conn, nil
}

// runOverSSH runs remoteCmd ON the host over the persistent SSH client: both
// real virsh commands (prefixed with "virsh" by the caller) and host shell
// commands (the "!" escape) funnel here once the connection is ssh://. It
// replaces the former fork-per-call ssh/sshpass subprocess; virsh's text
// output and every downstream parser are unaffected — this function returns
// the exact same *VirshResult/*VirshError shape runLocal (and, before this PR,
// exec.Cmd) produced.
func (v *VirshProvider) runOverSSH(ctx context.Context, remoteCmd string) (*VirshResult, error) {
	release, err := v.acquireExecSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	start := time.Now()
	var stdout, stderr bytes.Buffer
	log.Printf("DEBUG Executing over ssh: %s", remoteCmd)

	runErr := v.withSession(ctx, func(sess *ssh.Session) error {
		sess.Stdout = &stdout
		sess.Stderr = &stderr
		errCh := make(chan error, 1)
		go func() { errCh <- sess.Run(remoteCmd) }()
		select {
		case <-ctx.Done():
			// Closing the session is the SSH-session equivalent of
			// exec.CommandContext killing the local ssh subprocess on
			// cancellation: it interrupts a hung remote command instead of
			// leaking the goroutine/session until the remote side eventually
			// notices.
			_ = sess.Close()
			<-errCh
			return ctx.Err()
		case sessErr := <-errCh:
			return sessErr
		}
	})
	duration := time.Since(start)

	exitCode := 0
	if runErr != nil {
		exitCode = sshExitCode(runErr)
		if stderr.Len() == 0 {
			// The failure happened before any remote I/O (dial, handshake, or
			// session-open never reached the remote command), so the usual
			// place transientSSHConnectError looks — the remote command's own
			// stderr — is empty. Surface the connect-stage error text there
			// instead, so a transient connection failure (#191) is still
			// classified and retried under the new transport.
			stderr.WriteString(runErr.Error())
		}
	}

	result := &VirshResult{
		Command:  remoteCmd,
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}
	if runErr != nil {
		log.Printf("ERROR Command failed: %s (exit code: %d, duration: %v)", remoteCmd, exitCode, duration)
		log.Printf("ERROR Stderr: %s", result.Stderr)
		return result, &VirshError{Command: remoteCmd, ExitCode: exitCode, Stderr: result.Stderr, Stdout: result.Stdout, Cause: runErr}
	}
	log.Printf("DEBUG Command successful: %s (duration: %v)", remoteCmd, duration)
	return result, nil
}

// sshExitCode extracts a process exit code from an ssh.Session error, mirroring
// exec.Cmd's ProcessState.ExitCode() shape so downstream error classification
// (transientSSHConnectError, VirshError consumers) is unaffected by the
// transport change. Returns -1 for anything that is not a clean remote exit —
// a transport-level failure that never reached the remote shell.
func sshExitCode(err error) int {
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus()
	}
	return -1
}

// runSSHStdout runs remoteCmd on the libvirt host over the persistent SSH
// client, streaming the command's stdout into w, and returns when the command
// exits. It is the input-neutral sibling of runSSHStdin: instead of wiring a
// reader to the remote process's stdin, it wires the remote process's stdout
// to w, so a multi-GB disk streams OUT of the host without being buffered in
// the pod. Used by virshConn.Stream (the hostconn.Conn seam method) and by
// exportDiskToS3's ADR-0006 S3 export path.
func runSSHStdout(ctx context.Context, vp *VirshProvider, w io.Writer, remoteCmd string) error {
	release, err := vp.acquireStreamSlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	var stderr strings.Builder
	runErr := vp.withSession(ctx, func(sess *ssh.Session) error {
		stdout, pipeErr := sess.StdoutPipe()
		if pipeErr != nil {
			return fmt.Errorf("open ssh stdout pipe: %w", pipeErr)
		}
		sess.Stderr = &stderr

		if err := sess.Start(remoteCmd); err != nil {
			return fmt.Errorf("start remote command: %w", err)
		}

		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = sess.Close() // unblocks the Copy/Wait below on cancellation
			case <-done:
			}
		}()

		_, copyErr := io.Copy(w, stdout)
		_ = sess.Close() // always: harmless if already finished, unblocks Wait() if Copy aborted early
		waitErr := sess.Wait()
		close(done)

		if copyErr != nil {
			return copyErr
		}
		return waitErr
	})

	if runErr != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return fmt.Errorf("%w (stderr: %s)", runErr, s)
		}
		return runErr
	}
	return nil
}

// runSSHStdin runs remoteCmd on the libvirt host over the persistent SSH
// client, streaming stdin from r, and returns when the command exits. Unlike
// runOverSSH it does NOT buffer the input in memory — it wires r to the remote
// process's stdin so a multi-GB disk streams through. Used by
// virshConn.StreamIn (the libvirtConn extension) for the ADR-0006 S3 import
// path's host-side stage step, and by copyDiskToRemote (disk create/clone's
// scp replacement: `cat > remotePath`).
func runSSHStdin(ctx context.Context, vp *VirshProvider, r io.Reader, remoteCmd string) error {
	release, err := vp.acquireStreamSlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	var stderr strings.Builder
	log.Printf("DEBUG Executing SSH stdin stream: %s", remoteCmd)

	runErr := vp.withSession(ctx, func(sess *ssh.Session) error {
		sess.Stdin = r
		sess.Stderr = &stderr
		sess.Stdout = io.Discard

		errCh := make(chan error, 1)
		go func() { errCh <- sess.Run(remoteCmd) }()
		select {
		case <-ctx.Done():
			_ = sess.Close()
			<-errCh
			return ctx.Err()
		case sessErr := <-errCh:
			return sessErr
		}
	})

	if runErr != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return fmt.Errorf("%w (stderr: %s)", runErr, s)
		}
		return runErr
	}
	return nil
}
