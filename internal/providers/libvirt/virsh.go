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
	"log"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	// defaultMaxConcurrentVirsh bounds how many virsh/ssh subprocesses this
	// provider forks at once. Each virsh-over-ssh call is a real process fork
	// on the provider pod and the remote host; an unbounded burst (e.g. the
	// post-adoption Validate storm with 10-way reconcile concurrency) exhausts
	// the host fork limit and yields "cannot fork child process". Override with
	// VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_VIRSH (see #288). This is a fixed cap; the
	// strategic fix (#255/#256) is a persistent libvirt connection, after which
	// it can be raised or removed.
	defaultMaxConcurrentVirsh = 4

	// defaultMaxConcurrentStream bounds how many long-lived disk-stream
	// subprocesses this provider forks at once: the S3 import/export SSH
	// stdin/stdout relays (s3import.go/s3export.go) and the scp disk copy
	// (server.go copyDiskToRemote). These hold their fork slot for the
	// duration of a multi-minute transfer (a 40GB qemu-img/scp), not a quick
	// control call, so they get their OWN small budget instead of sharing
	// defaultMaxConcurrentVirsh — a single large export holding one of only 4
	// execSem slots for minutes would starve inventory/control virsh calls
	// queued behind it. Override with VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_STREAM.
	// These 7 call sites forked subprocesses without ANY bound until this
	// budget was added — a live latent instance of the #288 fork-exhaustion
	// class (see ADR-0008 PR 1, docs/adr/0008-libvirt-pure-go-driver-and-ssh-transport.md).
	defaultMaxConcurrentStream = 2
)

// VirshProvider implements a virsh command-line based libvirt provider
type VirshProvider struct {
	config      *ProviderConfig
	credentials *Credentials
	uri         string
	env         []string

	// hostKey is the resolved SSH host-key verification policy (ADR-0004,
	// #149). It is computed once in setupConnection and consumed by every
	// SSH/scp call site so the on/off decision lives in exactly one place.
	hostKey hostKeyPolicy

	// logger is used for the structured host-key verification-mode audit log
	// (WARN on the escape hatch, INFO when verifying). Defaults to
	// slog.Default() when nil.
	logger *slog.Logger

	// execSem bounds concurrent virsh/ssh subprocess forks (see
	// defaultMaxConcurrentVirsh). nil means unbounded (zero-value provider, e.g.
	// in tests). Set by NewVirshProvider; shared across all goroutines using
	// this provider instance.
	execSem chan struct{}

	// streamSem bounds concurrent long-lived disk-stream subprocess forks —
	// S3 import/export and the scp disk copy — SEPARATELY from execSem (see
	// defaultMaxConcurrentStream). A multi-minute transfer must not compete
	// with short control/inventory virsh calls for the same slot pool in
	// either direction: sharing execSem would let one big export starve
	// control calls behind it, and would let a control-call burst delay a
	// transfer indefinitely. nil means unbounded (zero-value provider, e.g. in
	// tests). Set by NewVirshProvider; shared across all goroutines using this
	// provider instance.
	streamSem chan struct{}

	// sshMu guards sshClient. ADR-0008 PR 3: the persistent, lazily-dialed
	// in-process SSH connection to this host (sshclient.go), reused across
	// every Virsh/RunHost/Stream/StreamIn call instead of forking a fresh
	// ssh/sshpass subprocess per call — this is the "strategic fix" #255/#256
	// named. Redial-on-broken is handled per-call (withSession); the full
	// keepalive/liveness-probe/watchdog lifecycle is ADR-0008 PR 4.
	sshMu     sync.Mutex
	sshClient *ssh.Client

	// golibvirtMu guards the lazy construction of golibvirt below (ADR-0008 PR
	// 4a). golibvirt stays nil — meaning go-libvirt is fully dormant: no dial,
	// no background goroutines — until the first Libvirt(ctx) call. Nothing in
	// production calls that yet: PR 4a lands the pure-Go go-libvirt connection
	// plumbing (golibvirt.go) only; PR 4b is what starts routing
	// shadow-compare reads through it.
	golibvirtMu sync.Mutex
	golibvirt   *golibvirtHolder

	// unroutable, when non-nil, makes this provider an always-failing handle
	// (ADR-0007 Addendum A, A1): every entry point that would run a command,
	// dial SSH, write a host file or open go-libvirt returns it instead of
	// touching any host. A clustered provider holds exactly one such handle as
	// its p.virshProvider, so a per-VM call that was not routed to a Host can
	// never fall through to an unintended (empty-URI or local) connection. It is
	// nil for every real host connection, where the check is a no-op.
	unroutable error

	// unroutableHits counts refused calls on an unroutable handle. A non-zero
	// value is a routing bug; tests assert it stays zero.
	unroutableHits atomic.Int64
}

// errUnroutableCall is the error an unroutable VirshProvider returns for every
// call (see VirshProvider.unroutable).
var errUnroutableCall = errors.New("clustered libvirt provider: call was not routed to a host " +
	"(a clustered provider has no single-host connection; per-VM calls must name their host)")

// newUnroutableVirshProvider returns the always-failing VirshProvider a
// clustered provider holds in place of a single-host connection (ADR-0007
// Addendum A, A1).
func newUnroutableVirshProvider() *VirshProvider {
	vp := NewVirshProvider(&ProviderConfig{Spec: ProviderSpec{}})
	vp.unroutable = errUnroutableCall
	return vp
}

// refuseIfUnroutable returns the unroutable error (and counts the attempt) when
// v is the always-failing clustered placeholder, and nil for a real connection.
func (v *VirshProvider) refuseIfUnroutable() error {
	if v.unroutable == nil {
		return nil
	}
	v.unroutableHits.Add(1)
	log.Printf("ERROR %v", v.unroutable)
	return v.unroutable
}

// VirshDomain represents a VM domain from virsh list output
type VirshDomain struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// VirshError represents an error from virsh command execution
type VirshError struct {
	Command  string
	ExitCode int
	Stderr   string
	Stdout   string
	// Cause is the original error that produced this VirshError, when
	// available (e.g. the underlying *knownhosts.KeyError from a failed SSH
	// handshake). Wired through Unwrap so callers can errors.As/errors.Is past
	// this struct's stringified Command/Stderr/Stdout summary to classify the
	// ACTUAL failure structurally instead of by substring-matching text — see
	// classifyNonTransientSSH. May be nil (e.g. a plain non-zero remote exit
	// has no more specific cause than the VirshError itself).
	Cause error
}

func (e *VirshError) Error() string {
	return fmt.Sprintf("virsh command '%s' failed (exit code %d): stderr=%s, stdout=%s",
		e.Command, e.ExitCode, e.Stderr, e.Stdout)
}

// Unwrap returns the original error behind this VirshError, if any, so
// errors.As/errors.Is can classify the underlying failure (e.g.
// *knownhosts.KeyError, a host-key mismatch) rather than just this struct's
// string summary.
func (e *VirshError) Unwrap() error { return e.Cause }

// NewVirshProvider creates a new virsh-based provider
func NewVirshProvider(config *ProviderConfig) *VirshProvider {
	return &VirshProvider{
		config:    config,
		execSem:   make(chan struct{}, maxConcurrentVirshFromEnv()),
		streamSem: make(chan struct{}, maxConcurrentStreamFromEnv()),
	}
}

// maxConcurrentVirshFromEnv returns the concurrent-virsh fork cap, honoring
// VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_VIRSH (positive int) and falling back to
// defaultMaxConcurrentVirsh otherwise.
func maxConcurrentVirshFromEnv() int {
	if s := os.Getenv("VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_VIRSH"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
		log.Printf("WARN Ignoring invalid VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_VIRSH=%q, using default %d", s, defaultMaxConcurrentVirsh)
	}
	return defaultMaxConcurrentVirsh
}

// maxConcurrentStreamFromEnv returns the concurrent disk-stream fork cap
// (S3 import/export SSH relays + scp disk copy), honoring
// VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_STREAM (positive int) and falling back to
// defaultMaxConcurrentStream otherwise. Deliberately independent of
// maxConcurrentVirshFromEnv — see the streamSem field doc for why the two
// budgets must not be merged.
func maxConcurrentStreamFromEnv() int {
	if s := os.Getenv("VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_STREAM"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
		log.Printf("WARN Ignoring invalid VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_STREAM=%q, using default %d", s, defaultMaxConcurrentStream)
	}
	return defaultMaxConcurrentStream
}

// acquireExecSlot blocks until a virsh/ssh fork slot is free or ctx is done,
// returning a release func to call when the subprocess finishes. A nil execSem
// (zero-value provider) is treated as unbounded.
func (v *VirshProvider) acquireExecSlot(ctx context.Context) (release func(), err error) {
	if v.execSem == nil {
		return func() {}, nil
	}
	select {
	case v.execSem <- struct{}{}:
		return func() { <-v.execSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// acquireStreamSlot blocks until a disk-stream fork slot is free or ctx is
// done, returning a release func to call when the subprocess finishes. A nil
// streamSem (zero-value provider) is treated as unbounded. This is the
// streaming counterpart to acquireExecSlot: callers doing a long-lived S3
// import/export relay or an scp disk copy acquire here instead, so they never
// contend with execSem's short control-call budget (see the streamSem field
// doc).
func (v *VirshProvider) acquireStreamSlot(ctx context.Context) (release func(), err error) {
	if v.streamSem == nil {
		return func() {}, nil
	}
	select {
	case v.streamSem <- struct{}{}:
		return func() { <-v.streamSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Initialize sets up the virsh provider with credentials and connection
func (v *VirshProvider) Initialize(ctx context.Context) error {
	log.Printf("INFO Initializing virsh-based libvirt provider")

	// Load credentials from environment variables (secure approach)
	if err := v.loadCredentialsFromEnv(); err != nil {
		return fmt.Errorf("failed to load credentials: %w", err)
	}

	// Build libvirt URI and environment
	if err := v.setupConnection(); err != nil {
		return fmt.Errorf("failed to setup connection: %w", err)
	}

	// Test the connection
	if err := v.testConnection(ctx); err != nil {
		return fmt.Errorf("failed to connect to libvirt: %w", err)
	}

	log.Printf("INFO Successfully initialized virsh provider with endpoint: %s", v.uri)
	return nil
}

// loadCredentialsFromEnv loads credentials from environment variables for security
func (v *VirshProvider) loadCredentialsFromEnv() error {
	log.Printf("INFO Loading credentials from environment variables (secure method)")

	v.credentials = &Credentials{}

	// Load username from environment
	if username := os.Getenv("LIBVIRT_USERNAME"); username != "" {
		v.credentials.Username = username
		log.Printf("INFO Successfully loaded username from env username_length=%d", len(v.credentials.Username))
	}

	// Load password from environment
	if password := os.Getenv("LIBVIRT_PASSWORD"); password != "" {
		v.credentials.Password = password
		log.Printf("INFO Successfully loaded password from env password_length=%d", len(v.credentials.Password))
	}

	// Load SSH private key from environment
	if sshKey := os.Getenv("LIBVIRT_SSH_PRIVATE_KEY"); sshKey != "" {
		v.credentials.SSHPrivateKey = sshKey
		log.Printf("INFO Successfully loaded SSH private key from env ssh_key_length=%d", len(v.credentials.SSHPrivateKey))
	}

	// Fallback: Load from mounted files if environment variables not set
	if v.credentials.Username == "" {
		if usernameData, err := os.ReadFile("/etc/virtrigaud/credentials/username"); err == nil {
			v.credentials.Username = strings.TrimSpace(string(usernameData))
			log.Printf("INFO Fallback: loaded username from file username_length=%d", len(v.credentials.Username))
		}
	}

	if v.credentials.Password == "" {
		if passwordData, err := os.ReadFile("/etc/virtrigaud/credentials/password"); err == nil {
			v.credentials.Password = strings.TrimSpace(string(passwordData))
			log.Printf("INFO Fallback: loaded password from file password_length=%d", len(v.credentials.Password))
		}
	}

	if v.credentials.SSHPrivateKey == "" {
		if sshKeyData, err := os.ReadFile("/etc/virtrigaud/credentials/ssh-privatekey"); err == nil {
			v.credentials.SSHPrivateKey = strings.TrimSpace(string(sshKeyData))
			log.Printf("INFO Fallback: loaded SSH private key from file ssh_key_length=%d", len(v.credentials.SSHPrivateKey))
		}
	}

	if v.credentials.Username == "" && v.credentials.Password == "" && v.credentials.SSHPrivateKey == "" {
		return fmt.Errorf("no valid credentials found in environment variables or mounted files")
	}

	return nil
}

// setupConnection parses the libvirt endpoint URI and resolves the SSH
// host-key verification policy.
//
// As of ADR-0008 PR 3 virsh and host-shell commands run over a persistent
// in-process SSH client (sshclient.go), dialed lazily on first use — so
// setupConnection no longer shells out to anything, writes an SSH private key
// to disk, or generates a ~/.ssh/config: key material lives in memory only
// (v.credentials.SSHPrivateKey), which is what unblocks a read-only root
// filesystem for the provider pod. Its remaining job is unchanged in spirit
// from #149 / ADR-0004: resolve the host-key policy (default ON; opt-out via
// LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION=true), emit the one-line
// verification-mode audit log, and hard-fail (no TOFU) at startup when
// verification is on but no usable known_hosts material is present — so a
// misconfigured Provider fails loudly before serving any RPC, not on the
// first reconcile.
func (v *VirshProvider) setupConnection() error {
	// Resolve the host-key verification policy once for this provider process.
	// The in-process SSH client (sshclient.go) consumes v.hostKey on every
	// dial, so the decision is taken here and nowhere else.
	v.hostKey = resolveHostKeyPolicy()

	// Get base URI from config
	uri := v.config.Spec.Endpoint
	if uri == "" {
		uri = "qemu:///system" // Default local connection
	}

	// Parse and enhance URI for authentication
	parsedURI, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("failed to parse URI: %w", err)
	}

	// Add username to SSH URIs
	if strings.Contains(parsedURI.Scheme, "ssh") && v.credentials.Username != "" {
		if parsedURI.User == nil {
			parsedURI.User = url.User(v.credentials.Username)
			log.Printf("INFO Added username to libvirt URI: %s", v.credentials.Username)
		}
	}

	isSSHURI := strings.Contains(parsedURI.Scheme, "ssh")
	v.uri = parsedURI.String()
	v.env = os.Environ()

	if isSSHURI {
		// Emit the one-line host-key verification-mode audit log (WARN on the
		// escape hatch, INFO when verifying) and hard-fail if verification is on
		// but no usable known_hosts is present (no TOFU). The in-process client
		// re-verifies on every dial (dialSSH); this pre-flight just makes a
		// misconfigured Provider fail at startup instead of on first use.
		v.hostKey.logVerificationMode(v.logger, parsedURI.Host)
		if err := v.hostKey.verifyKnownHostsPresent(parsedURI.Host); err != nil {
			return fmt.Errorf("libvirt SSH host-key verification pre-flight failed: %w", err)
		}
		log.Printf("INFO Configured in-process SSH transport for libvirt host=%s", parsedURI.Host)
	}

	log.Printf("INFO Configured virsh environment with URI: %s", v.uri)
	return nil
}

// remoteVirshConnectURI derives the libvirt connection URI to hand to a `virsh`
// process running ON the remote hypervisor host. The transport URI
// qemu+ssh://user@host/system means "manage qemu:///system on host", so a
// remote-side virsh must target that exact driver+path via -c. Without it the
// command falls back to the ssh user's default (qemu:///system for root,
// qemu:///session for non-root), which silently splits reads and writes across
// two libvirtd instances.
//
// SECURITY: the derived URI is sent to the remote host's shell, and its path
// comes from a Provider endpoint or a Host endpoint — i.e. user input. Only the
// two libvirt instance paths are accepted ("system" / "session", surrounding
// slashes ignored) and the driver must be a plain lowercase identifier; any
// other path (e.g. "/system;id", "/system%20-c%20id", "/system/../x") yields ""
// — the caller then omits -c and keeps the legacy (remote-default) behaviour
// rather than forwarding an attacker-shaped URI. The command line is ALSO
// shell-quoted (shellJoin), so this is defense in depth, not the only barrier.
// Returns "" when no driver/path can be derived.
func remoteVirshConnectURI(rawURI string) string {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return ""
	}
	driver := parsed.Scheme
	if i := strings.IndexByte(driver, '+'); i >= 0 {
		driver = driver[:i] // qemu+ssh -> qemu
	}
	if !libvirtDriverRE.MatchString(driver) {
		return ""
	}
	switch path := strings.Trim(parsed.Path, "/"); path { // /system -> system
	case libvirtInstanceSystem, libvirtInstanceSession:
		return fmt.Sprintf("%s:///%s", driver, path)
	default:
		return ""
	}
}

// libvirt connection-URI instance paths remoteVirshConnectURI accepts.
const (
	libvirtInstanceSystem  = "system"
	libvirtInstanceSession = "session"
)

// libvirtDriverRE matches a libvirt driver name (qemu, lxc, xen, ch, test, ...):
// a lowercase identifier, never anything a shell could interpret.
var libvirtDriverRE = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// isSSHTransport reports whether this provider reaches its libvirt host over
// SSH (qemu+ssh://...), as opposed to a genuinely local connection.
func (v *VirshProvider) isSSHTransport() bool {
	return strings.Contains(v.uri, "ssh://")
}

// remoteCommandLine builds the exact command line sent to the remote host's
// shell for one runVirshCommand call over SSH. It is a pure function (no I/O)
// so the quoting contract is unit-testable:
//
//   - args[0] == "!": the rest is a host command argv (the host-shell escape
//     behind hostconn.Conn.RunHost), flattened with shellJoin.
//   - otherwise: a virsh control command, `virsh [-c <driver:///system|session>]
//     <args...>`, flattened with shellJoin.
//
// EVERY element is quoted, so a value can never be interpreted by the remote
// shell (see shellquote.go). A caller that needs shell syntax must pass an
// explicit {"sh", "-c", <fixed script>, "sh", <values>...} argv.
func remoteCommandLine(connURI string, args []string) (string, error) {
	if len(args) > 0 && args[0] == "!" {
		direct := args[1:]
		if len(direct) == 0 {
			return "", fmt.Errorf("no command specified after '!' prefix")
		}
		return shellJoin(direct)
	}
	argv := make([]string, 0, len(args)+3)
	argv = append(argv, "virsh")
	if uri := remoteVirshConnectURI(connURI); uri != "" {
		argv = append(argv, "-c", uri)
	}
	argv = append(argv, args...)
	return shellJoin(argv)
}

// runRemoteVirshCommand runs `virsh <args>` ON the remote hypervisor host over SSH
// (used for subcommands that must read a host-local file the provider wrote there,
// e.g. define / pool-define). It pins -c to the connection URI's driver+path so the
// command targets the same libvirtd as the rest of the provider whether the ssh
// user is root (qemu:///system) or non-root (qemu:///session). Every argument is
// shell-quoted on the way to the host (remoteCommandLine).
func (v *VirshProvider) runRemoteVirshCommand(ctx context.Context, args ...string) (*VirshResult, error) {
	cmd := []string{"!", "virsh"}
	if uri := remoteVirshConnectURI(v.uri); uri != "" {
		cmd = append(cmd, "-c", uri)
	}
	cmd = append(cmd, args...)
	return v.runVirshCommand(ctx, cmd...)
}

// testConnection verifies that virsh can connect to the libvirt hypervisor
func (v *VirshProvider) testConnection(ctx context.Context) error {
	log.Printf("INFO Testing virsh connection to libvirt")

	// Run basic virsh command to test connectivity
	result, err := v.runVirshCommand(ctx, "version")
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	log.Printf("INFO Connection successful! Libvirt version: %s", strings.TrimSpace(result.Stdout))

	// Test domain listing to verify full functionality. Keep this lightweight:
	// startup readiness should not run one domstate command per domain on busy
	// libvirt hosts.
	listResult, err := v.runVirshCommand(ctx, "list", "--all", "--name")
	if err != nil {
		return fmt.Errorf("connection established but domain listing failed: %w", err)
	}

	domainCount := 0
	for _, line := range strings.Split(listResult.Stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			domainCount++
		}
	}

	log.Printf("INFO Successfully listed %d domains", domainCount)
	return nil
}

// VirshResult represents the result of a virsh command execution
type VirshResult struct {
	Command  string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// sshConnectMaxAttempts and sshConnectBaseBackoff bound the retry of a
// virsh-over-SSH command when the SSH *connection* (not the virsh command)
// fails transiently — e.g. the host throttles/refuses connections under
// MaxStartups/fail2ban, surfacing as `kex_exchange_identification` (#191).
// They are package vars (not consts) only so tests can shrink the backoff.
var (
	sshConnectMaxAttempts = 3
	sshConnectBaseBackoff = 1 * time.Second
)

// transientSSHConnectError reports whether stderr indicates a transient SSH
// *connection* failure — the host refused or closed the connection before the
// remote command ran. These are safe to retry because the virsh command never
// executed, so a retry cannot duplicate a side effect. Real virsh errors (e.g.
// "domain not found") never match and are returned immediately (#191).
//
// ADR-0008 PR 3: a connect-stage failure now surfaces as a Go error from
// net.Dialer/golang.org/x/crypto/ssh (runOverSSH copies its .Error() text into
// the Stderr this function inspects — see the comment there) rather than the
// OpenSSH CLI's own diagnostic text, so this list carries BOTH the original
// OpenSSH-CLI wording (kept: harmless if never matched again, and still
// exercised by local/test-only stderr) and the Go net/ssh equivalents added
// below, so the exact same class of failure (host slams the door during
// connect/handshake, e.g. MaxStartups/fail2ban) is still classified as
// transient and retried under the new transport.
//
// Security review of ADR-0008 PR 3 (#306): a host-key MISMATCH and an
// authentication failure are BOTH wrapped by golang.org/x/crypto/ssh as
// "ssh: handshake failed: ...", which would otherwise satisfy the generic
// "handshake failed" pattern below and get retried 3x — silently masking a
// real trust failure (or a MITM) as "transient connection blip", and
// hammering a wrong password/key against the host. classifyNonTransientSSH is
// the PRIMARY, structural gate against this in retryOnTransientSSH's main
// loop (checked before this function is ever reached for those two cases);
// the "knownhosts"/"unable to authenticate" exclusion here is defense in
// depth so the text matcher alone can never misclassify them either, even if
// that structural check were bypassed or this function were called directly.
func transientSSHConnectError(stderr string) bool {
	s := strings.ToLower(stderr)
	if strings.Contains(s, "knownhosts") || strings.Contains(s, "unable to authenticate") {
		return false
	}
	for _, m := range []string{
		"kex_exchange_identification",      // host closed conn during key exchange (MaxStartups/fail2ban)
		"connection closed by remote host", // host dropped the connection pre-auth
		"connection reset by peer",
		"connection refused",
		"connection timed out",
		"no route to host",
		"ssh: connect to host",                 // generic ssh connect failure
		"temporary failure in name resolution", // transient DNS (OpenSSH CLI wording)
		"i/o timeout",                          // Go net.Dialer timeout wording
		"no such host",                         // Go DNS resolver wording
		"handshake failed",                     // Go ssh: handshake dropped mid-negotiation (excludes knownhosts/auth above)
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// classifyNonTransientSSH reports whether err represents a definitive,
// non-retryable SSH connection failure, and names which kind for a distinct
// log line (security review of #306). Two cases are detected here so
// retryOnTransientSSH's main loop never retries — and never merely logs as a
// generic "transient connection failure" — either of them:
//
//   - host-key MISMATCH: the host IS present in known_hosts but presented a
//     DIFFERENT key than the pinned one — precisely the MITM / unannounced
//     key-rotation signal ADR-0004's whole design exists to catch. Detected
//     structurally via errors.As against *knownhosts.KeyError with a
//     populated Want (the "unknown host, no entry at all" case is a distinct,
//     separately-and-already-non-retryable failure: it never reaches here
//     because verifyKnownHostsPresent's pre-flight rejects it before any dial
//     is attempted).
//   - authentication failure: the configured password/key was rejected by the
//     host. golang.org/x/crypto/ssh does not export a distinct error type for
//     this on the client side, so it is detected via the stable
//     "unable to authenticate" substring its client auth code has used for
//     years (empirically confirmed against this exact ssh package version).
//
// Both must surface immediately — retrying with the SAME wrong host key or
// the SAME wrong credentials cannot succeed, and for host-key mismatch
// specifically, silently retrying would bury the one signal ADR-0004 exists
// to make loud behind three rounds of "transient, retrying" WARN logs.
func classifyNonTransientSSH(err error) (nonTransient bool, reason string) {
	if err == nil {
		return false, ""
	}
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		return true, "host-key mismatch"
	}
	if strings.Contains(err.Error(), "unable to authenticate") {
		return true, "authentication failure"
	}
	return false, ""
}

// runVirshCommand executes a virsh command, transparently retrying ONLY
// transient SSH-connection failures (see transientSSHConnectError) with bounded
// exponential backoff. Real virsh errors and the success path return on the
// first attempt, so behavior is unchanged except under host-side SSH throttling
// (#191). Retries stop early if the context is cancelled.
//
// Special case: if the first arg is "!", the remaining args run as a direct
// command (not through virsh).
func (v *VirshProvider) runVirshCommand(ctx context.Context, args ...string) (*VirshResult, error) {
	return retryOnTransientSSH(ctx, func() (*VirshResult, error) {
		return v.runVirshCommandOnce(ctx, args...)
	})
}

// retryOnTransientSSH invokes attempt up to sshConnectMaxAttempts times, retrying
// (with exponential backoff, honoring ctx) ONLY when the attempt's result stderr
// indicates a transient SSH connection failure. Any other error — including a
// real virsh command error — and the success path return immediately (#191).
//
// A host-key mismatch or an authentication failure (classifyNonTransientSSH)
// is checked FIRST, before the generic transient-text classification, and
// returns on the very first attempt with its own distinct log line — never
// retried, never folded into the generic "transient connection failure" WARN
// (security review of #306).
func retryOnTransientSSH(ctx context.Context, attempt func() (*VirshResult, error)) (*VirshResult, error) {
	var result *VirshResult
	var err error
	backoff := sshConnectBaseBackoff

	for n := 1; n <= sshConnectMaxAttempts; n++ {
		result, err = attempt()
		if err == nil {
			return result, nil
		}

		if nonTransient, reason := classifyNonTransientSSH(err); nonTransient {
			if reason == "host-key mismatch" {
				log.Printf("ERROR SSH host key presented by the remote host does NOT match known_hosts "+
					"(possible MITM or an un-announced host-key rotation) — this is a trust failure, not a "+
					"connectivity blip, and will NOT be retried: %v", err)
			} else {
				log.Printf("ERROR SSH %s — will NOT be retried with the same credentials: %v", reason, err)
			}
			return result, err
		}

		stderr := ""
		if result != nil {
			stderr = result.Stderr
		}
		if n == sshConnectMaxAttempts || !transientSSHConnectError(stderr) {
			return result, err
		}

		log.Printf("WARN Transient SSH connection failure (attempt %d/%d), retrying in %v: %s",
			n, sshConnectMaxAttempts, backoff, strings.TrimSpace(stderr))
		select {
		case <-ctx.Done():
			return result, err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return result, err
}

// runVirshCommandOnce executes a virsh command once. Special case: if the
// first arg is "!", execute the remaining args as a direct host-shell command
// (not virsh).
//
// ADR-0008 PR 3: for an ssh:// connection, BOTH shapes now unify onto "run the
// command on the host over our own persistent SSH client" (runOverSSH) —
// previously a real virsh command (no "!") went through the LOCAL virsh binary
// with LIBVIRT_DEFAULT_URI set, tunneling only the libvirt RPC wire protocol
// over a forked ssh, while the "!" shell-escape already ran its command
// directly on the host over an explicit ssh/sshpass fork. Unifying removes the
// local virsh dependency for every ssh:// Provider (see the Dockerfile
// libvirt-clients note) and, for a real virsh command, requires pinning `-c
// <driver:///path>` explicitly (remoteVirshConnectURI) since there is no more
// local LIBVIRT_DEFAULT_URI env var to imply it — virsh's own text output and
// every downstream parser are unaffected; only how the bytes reach virsh
// changes. A genuinely local (non-ssh) connection — e.g. qemu:///system, no
// host to dial — is unaffected and still execs the local virsh/command
// binary, now via -c instead of the (removed) LIBVIRT_DEFAULT_URI env var.
//
// SECURITY: over SSH, every argument is shell-quoted (remoteCommandLine /
// shellJoin), so both shapes have exactly the argv semantics of the local
// exec.Command path — no caller-supplied value (domain name, snapshot
// description, image URL, disk path, endpoint-derived URI) is ever
// interpreted by the remote shell.
func (v *VirshProvider) runVirshCommandOnce(ctx context.Context, args ...string) (*VirshResult, error) {
	if err := v.refuseIfUnroutable(); err != nil {
		return nil, err
	}
	direct := len(args) > 0 && args[0] == "!"
	if direct && len(args) == 1 {
		return nil, fmt.Errorf("no command specified after '!' prefix")
	}

	if v.isSSHTransport() {
		// Pin the remote virsh to the connection URI's libvirtd (root ->
		// system, non-root -> session) instead of letting it pick the ssh
		// user's default — see remoteVirshConnectURI's doc for why this
		// matters now that there is no LIBVIRT_DEFAULT_URI env var on the
		// remote side to imply it.
		remoteCmd, err := remoteCommandLine(v.uri, args)
		if err != nil {
			return nil, fmt.Errorf("build remote command: %w", err)
		}
		return v.runOverSSH(ctx, remoteCmd)
	}
	if direct {
		return v.runLocal(ctx, args[1:])
	}
	return v.runLocal(ctx, append([]string{"virsh", "-c", v.uri}, args...))
}

// runLocal executes argv as a local subprocess. It is reached only for a
// genuinely local (non-ssh://) libvirt connection — there is no host to dial
// — so it is unaffected by ADR-0008 PR 3's SSH transport change. The result
// and error shape matches runOverSSH exactly.
func (v *VirshProvider) runLocal(ctx context.Context, argv []string) (*VirshResult, error) {
	// Bound concurrent subprocess forks so a reconcile burst cannot exhaust the
	// host fork limit (cannot fork child process). Held only across the fork.
	release, err := v.acquireExecSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	start := time.Now()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = v.env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	command := strings.Join(argv, " ")
	log.Printf("DEBUG Executing: %s", command)

	runErr := cmd.Run()
	duration := time.Since(start)

	result := &VirshResult{
		Command:  command,
		ExitCode: cmd.ProcessState.ExitCode(),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if runErr != nil {
		log.Printf("ERROR Command failed: %s (exit code: %d, duration: %v)",
			command, result.ExitCode, duration)
		log.Printf("ERROR Stderr: %s", result.Stderr)
		return result, &VirshError{
			Command:  command,
			ExitCode: result.ExitCode,
			Stderr:   result.Stderr,
			Stdout:   result.Stdout,
			Cause:    runErr,
		}
	}

	log.Printf("DEBUG Command successful: %s (duration: %v)", command, duration)
	return result, nil
}

// listDomains lists all domains (VMs) using virsh
func (v *VirshProvider) listDomains(ctx context.Context) ([]VirshDomain, error) {
	// One `virsh list --all` yields name AND state for every domain, replacing the
	// old per-domain `virsh domstate` N+1 (one SSH round-trip per VM).
	result, err := v.runVirshCommand(ctx, "list", "--all")
	if err != nil {
		return nil, fmt.Errorf("failed to list domains: %w", err)
	}
	return parseDomainListTable(result.Stdout), nil
}

// parseDomainListTable parses `virsh list --all` table output (columns: Id,
// Name, State). Both Name ("Windows Server 2019") and State ("shut off") can
// contain spaces, so rows are sliced by the header's column byte-offsets, not
// by whitespace splitting — Fields() would corrupt a spaced name. virsh sizes
// each column to its widest value, so the State header offset always sits at or
// past the longest name; the Name column safely absorbs interior spaces.
func parseDomainListTable(stdout string) []VirshDomain {
	lines := strings.Split(stdout, "\n")

	// The dashed line separates the header from the rows; the header is above it.
	sep := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "---") {
			sep = i
			break
		}
	}
	if sep < 1 {
		// No recognizable table. An empty host still prints header + separator,
		// so reaching here means the format changed — warn instead of silently
		// returning zero domains (which would look like "adopt nothing").
		if strings.TrimSpace(stdout) != "" {
			log.Printf("WARN virsh list output has no header separator; parsed 0 domains")
		}
		return nil
	}
	header := lines[sep-1]
	nameAt := strings.Index(header, "Name")
	stateAt := strings.Index(header, "State")
	if nameAt < 0 || stateAt <= nameAt {
		log.Printf("WARN virsh list header missing Name/State columns; parsed 0 domains")
		return nil
	}

	// NOTE: byte offsets, not rune offsets. Fine for ASCII names (incl. spaces);
	// a CJK domain name could misalign — sanitizeVMName makes that rare.
	var domains []VirshDomain
	for _, line := range lines[sep+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name := strings.TrimSpace(colSlice(line, nameAt, stateAt))
		state := strings.TrimSpace(colSlice(line, stateAt, len(line)))
		if name == "" {
			continue // malformed / short row
		}
		domains = append(domains, VirshDomain{
			ID:    fmt.Sprintf("%d", len(domains)),
			Name:  name,
			State: state,
		})
	}
	return domains
}

// colSlice returns line[from:to], clamped to the line length for short rows.
func colSlice(line string, from, to int) string {
	if from > len(line) {
		return ""
	}
	if to > len(line) {
		to = len(line)
	}
	return line[from:to]
}

// startDomain starts a defined domain
func (v *VirshProvider) startDomain(ctx context.Context, domainName string) error {
	log.Printf("INFO Starting domain: %s", domainName)

	_, err := v.runVirshCommand(ctx, "start", domainName)
	if err != nil {
		return fmt.Errorf("failed to start domain %s: %w", domainName, err)
	}

	log.Printf("INFO Successfully started domain: %s", domainName)
	return nil
}

// stopDomain forcefully stops a running domain
func (v *VirshProvider) stopDomain(ctx context.Context, domainName string) error {
	log.Printf("INFO Force stopping domain: %s", domainName)

	_, err := v.runVirshCommand(ctx, "destroy", domainName)
	if err != nil {
		return fmt.Errorf("failed to stop domain %s: %w", domainName, err)
	}

	log.Printf("INFO Successfully force stopped domain: %s", domainName)
	return nil
}

// shutdownDomain gracefully shuts down a running domain
func (v *VirshProvider) shutdownDomain(ctx context.Context, domainName string) error {
	log.Printf("INFO Gracefully shutting down domain: %s", domainName)

	_, err := v.runVirshCommand(ctx, "shutdown", domainName)
	if err != nil {
		return fmt.Errorf("failed to shutdown domain %s: %w", domainName, err)
	}

	log.Printf("INFO Successfully initiated graceful shutdown for domain: %s", domainName)
	return nil
}

// destroyDomain forcefully stops a domain
func (v *VirshProvider) destroyDomain(ctx context.Context, domainName string) error {
	log.Printf("INFO Force stopping domain: %s", domainName)

	_, err := v.runVirshCommand(ctx, "destroy", domainName)
	if err != nil {
		return fmt.Errorf("failed to destroy domain %s: %w", domainName, err)
	}

	log.Printf("INFO Successfully destroyed domain: %s", domainName)
	return nil
}

// undefineDomain removes a domain definition
func (v *VirshProvider) undefineDomain(ctx context.Context, domainName string) error {
	log.Printf("INFO Undefining domain: %s", domainName)

	_, err := v.runVirshCommand(ctx, "undefine", domainName)
	if err != nil {
		return fmt.Errorf("failed to undefine domain %s: %w", domainName, err)
	}

	log.Printf("INFO Successfully undefined domain: %s", domainName)
	return nil
}

// getDomainInfo gets comprehensive information about a domain (enhanced monitoring)
func (v *VirshProvider) getDomainInfo(ctx context.Context, domainName string) (map[string]string, error) {
	result, err := v.runVirshCommand(ctx, "dominfo", domainName)
	if err != nil {
		return nil, fmt.Errorf("failed to get domain info for %s: %w", domainName, err)
	}

	info := make(map[string]string)
	lines := strings.Split(result.Stdout, "\n")

	for _, line := range lines {
		if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			info[key] = value
		}
	}

	// Enhance with comprehensive monitoring data (like vSphere provider)
	if err := v.enrichDomainInfo(ctx, domainName, info); err != nil {
		log.Printf("WARN Failed to get enhanced monitoring data for %s: %v", domainName, err)
		// Continue with basic info if enhanced monitoring fails
	}

	return info, nil
}

// Cleanup releases the provider's resources. ADR-0008 PR 3 made this close
// the persistent in-process SSH client (sshclient.go), if one was dialed;
// ADR-0008 PR 4a adds closing the go-libvirt connection lifecycle holder
// (golibvirt.go), if Libvirt(ctx) was ever called — in normal operation it
// never is, so this is a no-op in production today. The go-libvirt holder is
// closed BEFORE the SSH client since it tunnels over it: closing it first
// lets go-libvirt send its polite ProcConnectClose while the tunnel is still
// up. Called by virshConn.Close(), which the hostconn.Registry invokes on
// Evict/Close.
func (v *VirshProvider) Cleanup() error {
	log.Printf("INFO Cleaning up virsh provider")

	v.golibvirtMu.Lock()
	holder := v.golibvirt
	v.golibvirt = nil
	v.golibvirtMu.Unlock()
	if holder != nil {
		holder.close()
	}

	v.resetSSHClient()
	return nil
}

// enrichDomainInfo adds comprehensive monitoring data similar to vSphere provider
func (v *VirshProvider) enrichDomainInfo(ctx context.Context, domainName string, info map[string]string) error {
	// Get memory statistics
	if memStats, err := v.getDomainMemoryStats(ctx, domainName); err == nil {
		for k, v := range memStats {
			info[k] = v
		}
	}

	// Get CPU statistics
	if cpuStats, err := v.getDomainCPUStats(ctx, domainName); err == nil {
		for k, v := range cpuStats {
			info[k] = v
		}
	}

	// Get network interfaces and IP addresses
	if netInfo, err := v.getDomainNetworkInfo(ctx, domainName); err == nil {
		for k, v := range netInfo {
			info[k] = v
		}
	}

	// Get block device statistics
	if blockStats, err := v.getDomainBlockStats(ctx, domainName); err == nil {
		for k, v := range blockStats {
			info[k] = v
		}
	}

	// Get guest agent information (if available)
	if guestInfo, err := v.getDomainGuestInfo(ctx, domainName); err == nil {
		for k, v := range guestInfo {
			info[k] = v
		}
	}

	return nil
}

// getDomainMemoryStats retrieves memory usage statistics
func (v *VirshProvider) getDomainMemoryStats(ctx context.Context, domainName string) (map[string]string, error) {
	result, err := v.runVirshCommand(ctx, "dommemstat", domainName)
	if err != nil {
		return nil, err
	}

	stats := make(map[string]string)
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			key := fmt.Sprintf("memory_%s", parts[0])
			stats[key] = parts[1]
		}
	}
	return stats, nil
}

// getDomainCPUStats retrieves CPU usage statistics
func (v *VirshProvider) getDomainCPUStats(ctx context.Context, domainName string) (map[string]string, error) {
	result, err := v.runVirshCommand(ctx, "cpu-stats", domainName)
	if err != nil {
		return nil, err
	}

	stats := make(map[string]string)
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for _, line := range lines {
		if strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				key := fmt.Sprintf("cpu_%s", strings.TrimSpace(parts[0]))
				stats[key] = strings.TrimSpace(parts[1])
			}
		}
	}
	return stats, nil
}

// getDomainNetworkInfo retrieves network interface information and IP addresses
func (v *VirshProvider) getDomainNetworkInfo(ctx context.Context, domainName string) (map[string]string, error) {
	info := make(map[string]string)

	// Get domain interface list
	result, err := v.runVirshCommand(ctx, "domiflist", domainName)
	if err != nil {
		return nil, err
	}

	interfaces := []string{}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for i, line := range lines {
		if i == 0 || strings.HasPrefix(line, "-") {
			continue // Skip header lines
		}
		parts := strings.Fields(line)
		if len(parts) >= 1 {
			interfaces = append(interfaces, parts[0])
		}
	}

	info["network_interfaces"] = strings.Join(interfaces, ",")

	// Try to get IP addresses via guest agent (if available)
	if ipInfo, err := v.getDomainIPAddresses(ctx, domainName); err == nil {
		info["guest_ip_addresses"] = ipInfo
	}

	return info, nil
}

// getDomainIPAddresses attempts to get IP addresses via multiple sources
func (v *VirshProvider) getDomainIPAddresses(ctx context.Context, domainName string) (string, error) {
	ips := []string{}

	// Try multiple sources in order of preference:
	// 1. Guest agent (most reliable, requires qemu-guest-agent installed)
	// 2. DHCP lease (default, may be empty if network is bridged)
	// 3. ARP table (fallback, may not work in all network configurations)

	sources := []string{"agent", "lease", "arp"}

	for _, source := range sources {
		var result *VirshResult
		var err error

		if source == "lease" {
			// Default source, no need to specify
			result, err = v.runVirshCommand(ctx, "domifaddr", domainName)
		} else {
			result, err = v.runVirshCommand(ctx, "domifaddr", domainName, "--source", source)
		}

		if err != nil {
			log.Printf("DEBUG Failed to get IPs from source '%s' for %s: %v", source, domainName, err)
			continue
		}

		// Parse the output
		lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
		for i, line := range lines {
			if i == 0 || strings.HasPrefix(line, "-") || strings.TrimSpace(line) == "" {
				continue // Skip header lines and empty lines
			}
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				// Format: Name MAC address Protocol Address
				interfaceName := parts[0]
				ip := parts[3]

				// Skip loopback interface and invalid entries
				if interfaceName == "lo" || interfaceName == "-" || ip == "N/A" || ip == "-" {
					continue
				}

				// Remove CIDR notation if present (must be done before IP filtering)
				if strings.Contains(ip, "/") {
					ip = strings.Split(ip, "/")[0]
				}

				// Filter out unwanted IPs:
				// - Loopback addresses (127.0.0.1, ::1)
				// - IPv6 link-local addresses (fe80::)
				if ip == "127.0.0.1" || ip == "::1" || strings.HasPrefix(ip, "fe80:") {
					continue
				}

				// Only include IPs from interfaces starting with 'e' (eth*, ens*, enp*, etc.)
				// This excludes docker, virbr, and other virtual interfaces
				if strings.HasPrefix(interfaceName, "e") {
					ips = append(ips, ip)
				}
			}
		}

		// If we found IPs from this source, stop trying other sources
		if len(ips) > 0 {
			log.Printf("DEBUG Successfully retrieved %d IP(s) from source '%s' for %s", len(ips), source, domainName)
			break
		}
	}

	if len(ips) == 0 {
		log.Printf("DEBUG No IP addresses found for domain %s from any source", domainName)
		return "", nil
	}

	return strings.Join(ips, ","), nil
}

// getDomainBlockStats retrieves storage device statistics
func (v *VirshProvider) getDomainBlockStats(ctx context.Context, domainName string) (map[string]string, error) {
	info := make(map[string]string)

	// Get block device list
	result, err := v.runVirshCommand(ctx, "domblklist", domainName)
	if err != nil {
		return nil, err
	}

	devices := []string{}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for i, line := range lines {
		if i == 0 || strings.HasPrefix(line, "-") {
			continue // Skip header lines
		}
		parts := strings.Fields(line)
		if len(parts) >= 1 {
			devices = append(devices, parts[0])
		}
	}

	info["block_devices"] = strings.Join(devices, ",")

	// Get stats for first device (if any)
	if len(devices) > 0 {
		if blockStats, err := v.getBlockDeviceStats(ctx, domainName, devices[0]); err == nil {
			for k, v := range blockStats {
				info[k] = v
			}
		}
	}

	return info, nil
}

// getBlockDeviceStats retrieves statistics for a specific block device
func (v *VirshProvider) getBlockDeviceStats(ctx context.Context, domainName, device string) (map[string]string, error) {
	result, err := v.runVirshCommand(ctx, "domblkstat", domainName, device)
	if err != nil {
		return nil, err
	}

	stats := make(map[string]string)
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			key := fmt.Sprintf("block_%s", parts[0])
			stats[key] = parts[1]
		}
	}
	return stats, nil
}

// getDomainGuestInfo retrieves guest agent information
func (v *VirshProvider) getDomainGuestInfo(ctx context.Context, domainName string) (map[string]string, error) {
	info := make(map[string]string)

	// Try to get guest OS information
	result, err := v.runVirshCommand(ctx, "guestinfo", domainName, "--os")
	if err == nil {
		lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
		for _, line := range lines {
			if strings.Contains(line, ":") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					key := fmt.Sprintf("guest_%s", strings.TrimSpace(parts[0]))
					info[key] = strings.TrimSpace(parts[1])
				}
			}
		}
	}

	// Try to get guest hostname
	if result, err := v.runVirshCommand(ctx, "guestinfo", domainName, "--hostname"); err == nil {
		if strings.TrimSpace(result.Stdout) != "" {
			info["guest_hostname"] = strings.TrimSpace(result.Stdout)
		}
	}

	return info, nil
}

// getDomainState returns the current state of a domain
func (v *VirshProvider) getDomainState(ctx context.Context, domainName string) (string, error) {
	result, err := v.runVirshCommand(ctx, "domstate", domainName)
	if err != nil {
		return "", fmt.Errorf("failed to get domain state: %w", err)
	}

	state := strings.TrimSpace(result.Stdout)
	log.Printf("DEBUG Domain %s state: %s", domainName, state)
	return state, nil
}

// snapshotExists checks if a snapshot exists for a domain
func (v *VirshProvider) snapshotExists(ctx context.Context, domainName, snapshotName string) (bool, error) {
	// List all snapshots for the domain
	result, err := v.runVirshCommand(ctx, "snapshot-list", domainName, "--name")
	if err != nil {
		// If domain has no snapshots, snapshot-list may fail
		if strings.Contains(err.Error(), "no domain snapshot") {
			return false, nil
		}
		return false, fmt.Errorf("failed to list snapshots: %w", err)
	}

	// Check if snapshot name is in the list
	snapshots := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for _, snap := range snapshots {
		if strings.TrimSpace(snap) == snapshotName {
			return true, nil
		}
	}

	return false, nil
}

// getSnapshotInfo returns information about a specific snapshot
//
//nolint:unused // Keeping for future snapshot management features
func (v *VirshProvider) getSnapshotInfo(ctx context.Context, domainName, snapshotName string) (map[string]string, error) {
	result, err := v.runVirshCommand(ctx, "snapshot-info", domainName, snapshotName)
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshot info: %w", err)
	}

	info := make(map[string]string)
	lines := strings.Split(result.Stdout, "\n")
	for _, line := range lines {
		if strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				info[key] = value
			}
		}
	}

	return info, nil
}

// listSnapshots returns all snapshots for a domain
//
//nolint:unused // Keeping for future snapshot listing/querying features
func (v *VirshProvider) listSnapshots(ctx context.Context, domainName string) ([]string, error) {
	result, err := v.runVirshCommand(ctx, "snapshot-list", domainName, "--name")
	if err != nil {
		// If domain has no snapshots, return empty list
		if strings.Contains(err.Error(), "no domain snapshot") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to list snapshots: %w", err)
	}

	snapshots := []string{}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	for _, line := range lines {
		snap := strings.TrimSpace(line)
		if snap != "" {
			snapshots = append(snapshots, snap)
		}
	}

	return snapshots, nil
}
