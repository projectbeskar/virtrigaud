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
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// --- test-only in-memory SSH server fixture -------------------------------
//
// This exercises the REAL client code (dialSSH/withSession/runOverSSH) end to
// end against a real (loopback) SSH server, per the ADR-0008 PR 3 ask to cover
// the host-key callback, auth-method selection, and the persistent-client
// lifecycle without a real hypervisor. The server's "exec" handler shells out
// locally (os/exec is fine here — this is test infrastructure standing in for
// a remote sshd, not the provider's production transport, which is exactly
// what this PR removes exec.Command from).

// testSSHServerOpts configures the fixture's single allowed credential. Set
// exactly one of password/authorizedKey to match the auth method under test.
type testSSHServerOpts struct {
	password      string
	authorizedKey ssh.PublicKey
}

// startTestSSHServer starts a loopback SSH server with hostKey as its host
// key and returns its "host:port" address. It is torn down via t.Cleanup.
func startTestSSHServer(t *testing.T, hostKey ssh.Signer, opts testSSHServerOpts) string {
	t.Helper()

	config := &ssh.ServerConfig{}
	switch {
	case opts.password != "":
		config.PasswordCallback = func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == opts.password {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("wrong password")
		}
	case opts.authorizedKey != nil:
		config.PublicKeyCallback = func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(opts.authorizedKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unauthorized key")
		}
	default:
		config.NoClientAuth = true
	}
	config.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			nConn, err := ln.Accept()
			if err != nil {
				return // listener closed (test cleanup)
			}
			go serveTestSSHConn(nConn, config)
		}
	}()
	return ln.Addr().String()
}

// serveTestSSHConn completes the handshake and answers "session"/"exec"
// requests by running the command through the local shell, mirroring what a
// real sshd does for `ssh host cmd` — enough to exercise runOverSSH's stdout/
// stderr/exit-code plumbing without a real hypervisor.
func serveTestSSHConn(nConn net.Conn, config *ssh.ServerConfig) {
	sConn, chans, reqs, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		return // handshake/auth/host-key rejection — nothing more to do
	}
	defer func() { _ = sConn.Close() }()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newCh.Accept()
		if err != nil {
			continue
		}
		go serveTestSSHSession(channel, requests)
	}
}

func serveTestSSHSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()
	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		_ = ssh.Unmarshal(req.Payload, &payload)
		if req.WantReply {
			_ = req.Reply(true, nil)
		}

		cmd := exec.Command("/bin/sh", "-c", payload.Command) //nolint:gosec // test fixture standing in for a remote sshd
		cmd.Stdin = channel
		cmd.Stdout = channel
		cmd.Stderr = channel.Stderr()
		runErr := cmd.Run()

		code := 0
		if runErr != nil {
			code = 1
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			}
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
		return
	}
}

// generateTestHostKey returns a fresh ed25519 SSH host-key signer.
func generateTestHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_ = pub
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return signer
}

// generateTestClientKeyPEM returns a fresh ed25519 client keypair: its PEM
// (for Credentials.SSHPrivateKey, exactly as the mounted Secret provides it)
// and its ssh.PublicKey (for the server's authorizedKey).
func generateTestClientKeyPEM(t *testing.T) (pemStr string, pub ssh.PublicKey) {
	t.Helper()
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(privKey, "")
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(pubKey)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(block)), sshPub
}

// useTempKnownHosts points the package-level KnownHostsFile var at a fresh
// temp file (optionally pre-seeded) for the duration of the test, restoring
// the original value on cleanup. KnownHostsFile is a var (not const)
// specifically for this.
func useTempKnownHosts(t *testing.T, seed string) string {
	t.Helper()
	orig := KnownHostsFile
	path := filepath.Join(t.TempDir(), "known_hosts")
	if seed != "" {
		require.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
	}
	KnownHostsFile = path
	t.Cleanup(func() { KnownHostsFile = orig })
	return path
}

// testVirshProvider builds a *VirshProvider ready to dial addr (an
// "127.0.0.1:port" loopback test server address) as user, with the given
// credentials and a verifying (non-insecure) host-key policy.
func testVirshProvider(user, addr string, creds *Credentials) *VirshProvider {
	return &VirshProvider{
		uri:         fmt.Sprintf("qemu+ssh://%s@%s/system", user, addr),
		credentials: creds,
		hostKey:     hostKeyPolicy{insecure: false},
	}
}

// --- auth-method selection --------------------------------------------------

// TestSSHAuthMethods covers the precedence and rejection rules sshAuthMethods
// enforces: password wins when both are configured (preserving the former
// argv-transport's exact precedence — never a new policy decision), key-only
// and password-only both work standalone, and neither configured is a clear
// error rather than an empty, silently-failing Auth list.
func TestSSHAuthMethods(t *testing.T) {
	pemStr, _ := generateTestClientKeyPEM(t)

	t.Run("password only", func(t *testing.T) {
		methods, err := sshAuthMethods(&Credentials{Password: "secret"})
		require.NoError(t, err)
		assert.Len(t, methods, 1)
	})

	t.Run("key only", func(t *testing.T) {
		methods, err := sshAuthMethods(&Credentials{SSHPrivateKey: pemStr})
		require.NoError(t, err)
		assert.Len(t, methods, 1)
	})

	t.Run("password wins when both configured", func(t *testing.T) {
		// Give the key an unparsable value: if the password branch is NOT
		// selected first, ssh.ParsePrivateKey would fail and this would
		// error — it must not.
		methods, err := sshAuthMethods(&Credentials{Password: "secret", SSHPrivateKey: "not a real key"})
		require.NoError(t, err)
		assert.Len(t, methods, 1)
	})

	t.Run("neither configured is an error", func(t *testing.T) {
		_, err := sshAuthMethods(&Credentials{})
		assert.Error(t, err)
	})

	t.Run("nil credentials is an error, not a panic", func(t *testing.T) {
		assert.NotPanics(t, func() {
			_, err := sshAuthMethods(nil)
			assert.Error(t, err)
		})
	})

	t.Run("unparsable key is a clear error", func(t *testing.T) {
		_, err := sshAuthMethods(&Credentials{SSHPrivateKey: "not a real key"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse SSH private key")
	})
}

// --- host-key verification: accept pinned / reject unknown or mismatched ---

// TestDialSSH_PasswordAuth_AcceptsPinnedHostKey is the full happy path: the
// server's real host key is seeded into known_hosts, and password auth is
// configured — dialSSH must succeed and hand back a working client (proven by
// running a command over it).
func TestDialSSH_PasswordAuth_AcceptsPinnedHostKey(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	client, err := v.dialSSH(t.Context())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	require.NoError(t, err)
	defer func() { _ = sess.Close() }()
	out, err := sess.CombinedOutput("echo hello")
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(out))
}

// TestDialSSH_KeyAuth_AcceptsPinnedHostKey mirrors the password test with
// public-key authentication, proving both auth methods are fully functional
// in-process (ADR-0008 PR 3 keeps both; D8's password-auth removal is a
// separate, not-yet-made decision).
func TestDialSSH_KeyAuth_AcceptsPinnedHostKey(t *testing.T) {
	hostKey := generateTestHostKey(t)
	clientKeyPEM, clientPub := generateTestClientKeyPEM(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{authorizedKey: clientPub})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{SSHPrivateKey: clientKeyPEM})
	client, err := v.dialSSH(t.Context())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	sess, err := client.NewSession()
	require.NoError(t, err)
	defer func() { _ = sess.Close() }()
	out, err := sess.CombinedOutput("echo hello")
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(out))
}

// TestDialSSH_RejectsWhenKnownHostsHasNoEntryForHost proves the pre-flight
// gate (verifyKnownHostsPresent, called from dialSSH) refuses to dial at all
// when known_hosts has entries for OTHER hosts but none for this one — the
// #291 B6 case, exercised here at the dialSSH integration level rather than
// just the unit level (sshhostkey_test.go).
func TestDialSSH_RejectsWhenKnownHostsHasNoEntryForHost(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	// Seed known_hosts for an unrelated host only.
	useTempKnownHosts(t, knownhosts.Line([]string{"totally-different-host:22"}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	_, err := v.dialSSH(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-key verification")
}

// TestDialSSH_RejectsMismatchedHostKey proves that a known_hosts entry
// PRESENT for this exact host, but carrying a DIFFERENT key than the one the
// server actually presents, still fails the connection — at the ssh-protocol
// handshake layer this time (verifyKnownHostsPresent's presence check passes;
// ssh.ClientConfig.HostKeyCallback, built from the same knownhosts file, is
// what rejects the mismatch). This is the actual MITM scenario #149/ADR-0004
// exists to catch.
func TestDialSSH_RejectsMismatchedHostKey(t *testing.T) {
	realHostKey := generateTestHostKey(t)
	wrongHostKey := generateTestHostKey(t) // a different key, same host entry
	addr := startTestSSHServer(t, realHostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, wrongHostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	_, err := v.dialSSH(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "handshake")
}

// TestDialSSH_InsecurePolicy_AcceptsAnyHostKey proves the escape hatch still
// connects when known_hosts is empty/absent — functionally equivalent to
// ssh.InsecureIgnoreHostKey, but never calling that symbol (hostKeyCallback's
// doc explains why) — and is reached only via the explicit hostKeyPolicy{
// insecure: true}, never the default.
func TestDialSSH_InsecurePolicy_AcceptsAnyHostKey(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	// No known_hosts seeded at all.
	useTempKnownHosts(t, "")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	v.hostKey = hostKeyPolicy{insecure: true}
	client, err := v.dialSSH(t.Context())
	require.NoError(t, err)
	_ = client.Close()
}

// --- persistent-client lifecycle: reuse + redial-on-broken -----------------

// TestRunOverSSH_ReusesPersistentClient proves the whole point of ADR-0008
// PR 3: two calls through runOverSSH on the same *VirshProvider dial exactly
// once and reuse the same *ssh.Client for the second command (no fork/dial
// per call).
func TestRunOverSSH_ReusesPersistentClient(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	t.Cleanup(func() { _ = v.Cleanup() })

	res1, err := v.runOverSSH(t.Context(), "echo one")
	require.NoError(t, err)
	assert.Equal(t, "one\n", res1.Stdout)
	assert.Equal(t, 0, res1.ExitCode)

	client1 := v.sshClient
	require.NotNil(t, client1, "first call must have dialed a persistent client")

	res2, err := v.runOverSSH(t.Context(), "echo two")
	require.NoError(t, err)
	assert.Equal(t, "two\n", res2.Stdout)

	assert.Same(t, client1, v.sshClient, "second call must reuse the SAME *ssh.Client, not redial")
}

// TestRunOverSSH_NonZeroExitAndStderr proves VirshResult/VirshError carry the
// exit code and stderr exactly as the former exec.Cmd-based transport did —
// downstream parsers (and transientSSHConnectError's stderr inspection)
// depend on this shape being unchanged.
func TestRunOverSSH_NonZeroExitAndStderr(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	t.Cleanup(func() { _ = v.Cleanup() })

	res, err := v.runOverSSH(t.Context(), "echo boom >&2; exit 3")
	require.Error(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 3, res.ExitCode)
	assert.Equal(t, "boom\n", res.Stderr)

	var virshErr *VirshError
	require.ErrorAs(t, err, &virshErr)
	assert.Equal(t, 3, virshErr.ExitCode)
}

// TestRunOverSSH_RedialsAfterBrokenConnection proves the "basic redial if the
// client is dead" requirement: after the cached *ssh.Client is forcibly
// closed out from under the provider (simulating a dropped connection —
// libvirtd restart, host reboot, idle timeout), the NEXT call must not wedge
// the provider forever; it redials once and succeeds.
func TestRunOverSSH_RedialsAfterBrokenConnection(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	t.Cleanup(func() { _ = v.Cleanup() })

	_, err := v.runOverSSH(t.Context(), "echo one")
	require.NoError(t, err)
	staleClient := v.sshClient
	require.NotNil(t, staleClient)

	// Simulate a dropped connection: close the transport out from under the
	// provider without going through resetSSHClient (which is exactly what a
	// network blip / remote restart looks like from the client's side).
	require.NoError(t, staleClient.Close())

	res, err := v.runOverSSH(t.Context(), "echo two")
	require.NoError(t, err, "a broken cached connection must self-heal via one redial, not fail the call")
	assert.Equal(t, "two\n", res.Stdout)
	assert.NotSame(t, staleClient, v.sshClient, "withSession must have redialed a fresh *ssh.Client")
}

// --- remote virsh -c targeting (unaffected by the transport change) --------

// TestRemoteVirshConnectURI pins the driver+path a remote-side `virsh -c` must
// target, so a remote virsh hits the SAME libvirtd as the rest of the provider
// regardless of whether the ssh user is root (default system) or non-root
// (default session). Query params (keyfile/no_tty/…) and the ssh transport/host
// must all be stripped. ADR-0008 PR 3 makes this MORE load-bearing than before
// (Fact/D3): -c is now applied on every ssh:// virsh call, real or "!", since
// there is no more LIBVIRT_DEFAULT_URI env var to imply it.
func TestRemoteVirshConnectURI(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"root system", "qemu+ssh://root@host/system", "qemu:///system"},
		{"non-root session", "qemu+ssh://user@host/session", "qemu:///session"},
		{"non-root system in libvirt group", "qemu+ssh://libvirtuser@host/system", "qemu:///system"},
		{"strips query", "qemu+ssh://host/system?keyfile=%2Fk&no_tty=1&sshauth=privkey", "qemu:///system"},
		{"local uri", "qemu:///system", "qemu:///system"},
		{"other driver", "test+ssh://host/default", "test:///default"},
		{"no path -> empty (legacy fallback)", "qemu+ssh://host", ""},
		{"empty path -> empty", "qemu+ssh://host/", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, remoteVirshConnectURI(tc.in))
		})
	}
}

// --- streaming primitives (Conn.Stream / Conn.StreamIn) --------------------

// TestRunSSHStdout_StreamsRemoteOutput proves the export-direction primitive
// (virshConn.Stream's backing function) delivers the remote command's stdout
// byte-for-byte, exercising the real io.Pipe + goroutine plumbing rather than
// just the request/response shape runOverSSH's tests cover.
func TestRunSSHStdout_StreamsRemoteOutput(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	t.Cleanup(func() { _ = v.Cleanup() })

	var out bytes.Buffer
	err := runSSHStdout(context.Background(), v, &out, "printf 'line1\\nline2\\n'")
	require.NoError(t, err)
	assert.Equal(t, "line1\nline2\n", out.String())
}

// TestRunSSHStdout_SurfacesStderrOnFailure proves a failing remote command's
// stderr is folded into the returned error (so the real cause is visible, not
// masked by an io.Pipe "closed pipe" error).
func TestRunSSHStdout_SurfacesStderrOnFailure(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	t.Cleanup(func() { _ = v.Cleanup() })

	var out bytes.Buffer
	err := runSSHStdout(context.Background(), v, &out, "echo nope >&2; exit 1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

// TestRunSSHStdout_CancelInterruptsHungCommand proves ctx cancellation
// interrupts a remote command that would otherwise block forever, mirroring
// the property exec.CommandContext gave the former subprocess transport for
// free — a hung host-side qemu-img/cat must not wedge the caller forever.
func TestRunSSHStdout_CancelInterruptsHungCommand(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	t.Cleanup(func() { _ = v.Cleanup() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- runSSHStdout(ctx, v, &out, "sleep 30")
	}()

	time.Sleep(50 * time.Millisecond) // let the remote command actually start
	cancel()

	select {
	case err := <-done:
		assert.Error(t, err, "a cancelled stream must return an error, not silently succeed")
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not interrupt the hung remote command within 10s")
	}
}

// TestRunSSHStdin_StreamsToRemote proves the import-direction primitive
// (virshConn.StreamIn's backing function) delivers r's bytes to the remote
// command's stdin byte-for-byte. The test server's "remote" `cat > file`
// writes to a real local temp file (the fixture shells out locally), which
// this test reads back to verify.
func TestRunSSHStdin_StreamsToRemote(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	t.Cleanup(func() { _ = v.Cleanup() })

	dst := filepath.Join(t.TempDir(), "staged.bin")
	const payload = "the quick brown fox jumps over the lazy dog\n"

	err := runSSHStdin(context.Background(), v, strings.NewReader(payload), fmt.Sprintf("cat > %s", shellQuote(dst)))
	require.NoError(t, err)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, payload, string(got))
}

// TestVirshConn_StreamIn_WiresThroughToRunSSHStdin proves the libvirtConn seam
// method (virshConn.StreamIn, what nfs.go/s3import.go/s3export.go now call
// instead of type-asserting *Provider) actually reaches the host, not just the
// free function directly.
func TestVirshConn_StreamIn_WiresThroughToRunSSHStdin(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	conn := newVirshConn("host-a", v)
	t.Cleanup(func() { _ = conn.Close() })

	dst := filepath.Join(t.TempDir(), "staged.bin")
	err := conn.StreamIn(context.Background(), strings.NewReader("hello via seam\n"), "cat", ">", shellQuote(dst))
	require.NoError(t, err)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "hello via seam\n", string(got))
}

// TestVirshConn_Stream_WiresThroughToRunSSHStdout is Stream's counterpart to
// the StreamIn test above.
func TestVirshConn_Stream_WiresThroughToRunSSHStdout(t *testing.T) {
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")

	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	conn := newVirshConn("host-a", v)
	t.Cleanup(func() { _ = conn.Close() })

	rc, err := conn.Stream(context.Background(), "echo", "via-seam")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "via-seam\n", string(got))
}
