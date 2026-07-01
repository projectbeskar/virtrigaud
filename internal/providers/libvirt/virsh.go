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
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	sshPrivateKeyDir  = "/tmp/virtrigaud-libvirt"
	sshPrivateKeyPath = sshPrivateKeyDir + "/ssh-privatekey"

	// defaultMaxConcurrentVirsh bounds how many virsh/ssh subprocesses this
	// provider forks at once. Each virsh-over-ssh call is a real process fork
	// on the provider pod and the remote host; an unbounded burst (e.g. the
	// post-adoption Validate storm with 10-way reconcile concurrency) exhausts
	// the host fork limit and yields "cannot fork child process". Override with
	// VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_VIRSH (see #288). This is a fixed cap; the
	// strategic fix (#255/#256) is a persistent libvirt connection, after which
	// it can be raised or removed.
	defaultMaxConcurrentVirsh = 4
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
}

func (e *VirshError) Error() string {
	return fmt.Sprintf("virsh command '%s' failed (exit code %d): stderr=%s, stdout=%s",
		e.Command, e.ExitCode, e.Stderr, e.Stdout)
}

// NewVirshProvider creates a new virsh-based provider
func NewVirshProvider(config *ProviderConfig) *VirshProvider {
	return &VirshProvider{
		config:  config,
		execSem: make(chan struct{}, maxConcurrentVirshFromEnv()),
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

// setupConnection prepares the libvirt URI and environment for virsh commands.
//
// As of #149 / ADR-0004 it also resolves the SSH host-key verification policy
// (default ON; opt-out via LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION=true),
// emits the one-line verification-mode audit log, points the SSH transport at
// the credentials-mounted known_hosts, and hard-fails (no TOFU) when
// verification is on but no usable known_hosts material is present.
func (v *VirshProvider) setupConnection() error {
	// Resolve the host-key verification policy once for this provider process.
	// Every SSH/scp call site consumes v.hostKey so the decision is taken here
	// and nowhere else.
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

	// Add SSH options for container environments. Host-key handling is delegated
	// to the centralized policy: insecure path restores no_verify=1, the
	// verifying path omits it and relies on the verifying ~/.ssh/config +
	// known_hosts written below.
	if isSSHURI {
		query := parsedURI.Query()
		v.hostKey.applyURIHostKeyOptions(query)
		query.Set("no_tty", "1") // Non-interactive mode
		if strings.TrimSpace(v.credentials.SSHPrivateKey) != "" {
			keyPath, err := v.writeSSHPrivateKey()
			if err != nil {
				return fmt.Errorf("failed to write SSH private key for libvirt transport: %w", err)
			}
			query.Set("keyfile", keyPath)
			query.Set("sshauth", "privkey")
			log.Printf("INFO Configured key-based SSH authentication for libvirt transport keyfile=%s", keyPath)
		}
		parsedURI.RawQuery = query.Encode()

		// Emit the one-line host-key verification-mode audit log (WARN on the
		// escape hatch, INFO when verifying) and hard-fail if verification is on
		// but no usable known_hosts is present (no TOFU).
		v.hostKey.logVerificationMode(v.logger, parsedURI.Host)
		if err := v.hostKey.verifyKnownHostsPresent(parsedURI.Host); err != nil {
			return fmt.Errorf("libvirt SSH host-key verification pre-flight failed: %w", err)
		}

		log.Printf("INFO Added SSH options for container environment")
	}

	v.uri = parsedURI.String()

	// Set up environment variables for virsh
	v.env = os.Environ()
	v.env = append(v.env, fmt.Sprintf("LIBVIRT_DEFAULT_URI=%s", v.uri))

	if isSSHURI {
		if err := v.createSSHConfig(); err != nil {
			if strings.TrimSpace(v.credentials.SSHPrivateKey) != "" {
				return fmt.Errorf("failed to create SSH config for key-based libvirt transport: %w", err)
			}
			log.Printf("WARN Failed to create SSH config: %v", err)
		}
	}

	// Set SSH authentication via environment variables for non-interactive use
	if v.credentials.Password != "" {
		// Use sshpass for non-interactive password authentication
		v.env = append(v.env, fmt.Sprintf("SSHPASS=%s", v.credentials.Password))

		// Set SSH options for non-interactive authentication
		v.env = append(v.env, "SSH_ASKPASS_REQUIRE=never")

		log.Printf("INFO Configured non-interactive SSH authentication via sshpass")
	}

	log.Printf("INFO Configured virsh environment with URI: %s", v.uri)
	return nil
}

func (v *VirshProvider) writeSSHPrivateKey() (string, error) {
	if err := os.MkdirAll(sshPrivateKeyDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create SSH private key directory: %w", err)
	}

	key := strings.TrimSpace(v.credentials.SSHPrivateKey) + "\n"
	if err := os.WriteFile(sshPrivateKeyPath, []byte(key), 0600); err != nil {
		return "", fmt.Errorf("failed to write SSH private key: %w", err)
	}
	if err := os.Chmod(sshPrivateKeyPath, 0600); err != nil {
		return "", fmt.Errorf("failed to chmod SSH private key: %w", err)
	}
	return sshPrivateKeyPath, nil
}

// sshKeyAuthOptions returns the ssh/scp flags that force key-based authentication
// using keyFile. Shared by the direct-ssh ("!"), scp, and stdin-stream call sites
// so the key is offered identically everywhere: the key lives at a non-default
// path with no IdentityFile in ~/.ssh/config, so -i is mandatory, and
// IdentitiesOnly + the explicit auth toggles stop ssh from wandering onto an agent
// key or a password prompt.
func sshKeyAuthOptions(keyFile string) []string {
	return []string{
		"-i", keyFile,
		"-o", "IdentitiesOnly=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "PubkeyAuthentication=yes",
	}
}

// resolveSSHKeyFile picks the private-key path for a direct ssh/scp invocation:
// the keyfile= pinned on the libvirt URI by setupConnection, falling back to the
// well-known path writeSSHPrivateKey persists to.
func resolveSSHKeyFile(parsedURI *url.URL) string {
	if kf := parsedURI.Query().Get("keyfile"); kf != "" {
		return kf
	}
	return sshPrivateKeyPath
}

// remoteVirshConnectURI derives the libvirt connection URI to hand to a `virsh`
// process running ON the remote hypervisor host. The transport URI
// qemu+ssh://user@host/system means "manage qemu:///system on host", so a
// remote-side virsh must target that exact driver+path via -c. Without it the
// command falls back to the ssh user's default (qemu:///system for root,
// qemu:///session for non-root), which silently splits reads and writes across
// two libvirtd instances. Returns "" when no driver/path can be derived, in which
// case the caller omits -c and keeps the legacy (remote-default) behaviour.
func remoteVirshConnectURI(rawURI string) string {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return ""
	}
	driver := parsed.Scheme
	if i := strings.IndexByte(driver, '+'); i >= 0 {
		driver = driver[:i] // qemu+ssh -> qemu
	}
	path := strings.Trim(parsed.Path, "/") // /system -> system
	if driver == "" || path == "" {
		return ""
	}
	return fmt.Sprintf("%s:///%s", driver, path)
}

// runRemoteVirshCommand runs `virsh <args>` ON the remote hypervisor host over SSH
// (used for subcommands that must read a host-local file the provider wrote there,
// e.g. define / pool-define). It pins -c to the connection URI's driver+path so the
// command targets the same libvirtd as the rest of the provider whether the ssh
// user is root (qemu:///system) or non-root (qemu:///session).
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
func transientSSHConnectError(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, m := range []string{
		"kex_exchange_identification",      // host closed conn during key exchange (MaxStartups/fail2ban)
		"connection closed by remote host", // host dropped the connection pre-auth
		"connection reset by peer",
		"connection refused",
		"connection timed out",
		"no route to host",
		"ssh: connect to host",                 // generic ssh connect failure
		"temporary failure in name resolution", // transient DNS
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
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
func retryOnTransientSSH(ctx context.Context, attempt func() (*VirshResult, error)) (*VirshResult, error) {
	var result *VirshResult
	var err error
	backoff := sshConnectBaseBackoff

	for n := 1; n <= sshConnectMaxAttempts; n++ {
		result, err = attempt()
		if err == nil {
			return result, nil
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

// runVirshCommandOnce executes a virsh command once with proper environment and
// error handling. Special case: if first arg is "!", execute the remaining args
// as a direct command (not virsh).
func (v *VirshProvider) runVirshCommandOnce(ctx context.Context, args ...string) (*VirshResult, error) {
	// Bound concurrent subprocess forks so a reconcile burst cannot exhaust the
	// host fork limit (cannot fork child process). Held only across the fork.
	release, err := v.acquireExecSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	start := time.Now()

	var cmd *exec.Cmd
	var command string

	// Special handling for direct commands (prefixed with "!")
	if len(args) > 0 && args[0] == "!" {
		// Execute direct command (not through virsh)
		directArgs := args[1:] // Remove the "!" prefix
		if len(directArgs) == 0 {
			return nil, fmt.Errorf("no command specified after '!' prefix")
		}

		if strings.Contains(v.uri, "ssh://") {
			parsedURI, _ := url.Parse(v.uri)
			host := parsedURI.Host
			user := parsedURI.User.Username()

			if v.credentials.Password != "" {
				// For remote execution with password authentication, use SSH via sshpass.
				sshArgs := []string{
					"-e", // Read password from SSHPASS environment variable
					"ssh",
					"-o", "PasswordAuthentication=yes",
					"-o", "PubkeyAuthentication=no",
					"-o", "LogLevel=ERROR",
				}
				// Host-key options come from the centralized policy (#149/ADR-0004);
				// ControlMaster multiplexing reuses one connection (#194).
				sshArgs = append(sshArgs, v.hostKey.sshHostKeyOptions()...)
				sshArgs = append(sshArgs, sshMultiplexOptions()...)
				sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", user, host))
				sshArgs = append(sshArgs, directArgs...)

				cmd = exec.CommandContext(ctx, "sshpass", sshArgs...)
				command = fmt.Sprintf("sshpass -e ssh %s@%s %s", user, host, strings.Join(directArgs, " "))
			} else if strings.TrimSpace(v.credentials.SSHPrivateKey) != "" {
				keyFile := resolveSSHKeyFile(parsedURI)
				sshArgs := sshKeyAuthOptions(keyFile)
				sshArgs = append(sshArgs, "-o", "LogLevel=ERROR")
				sshArgs = append(sshArgs, v.hostKey.sshHostKeyOptions()...)
				sshArgs = append(sshArgs, sshMultiplexOptions()...)
				sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", user, host))
				sshArgs = append(sshArgs, directArgs...)

				cmd = exec.CommandContext(ctx, "ssh", sshArgs...)
				command = fmt.Sprintf("ssh -i %s %s@%s %s", keyFile, user, host, strings.Join(directArgs, " "))
			} else {
				// Local execution
				cmd = exec.CommandContext(ctx, directArgs[0], directArgs[1:]...)
				command = strings.Join(directArgs, " ")
			}
		} else {
			// Local execution
			cmd = exec.CommandContext(ctx, directArgs[0], directArgs[1:]...)
			command = strings.Join(directArgs, " ")
		}
		cmd.Env = v.env
	} else {
		// Standard virsh command execution
		if v.credentials.Password != "" && strings.Contains(v.uri, "ssh://") {
			// Build command: SSHPASS=password sshpass -e ssh -o [options] user@host virsh [args]
			// This directly uses SSH with options rather than relying on config files

			// Extract host and user from URI for direct SSH call
			parsedURI, _ := url.Parse(v.uri)
			host := parsedURI.Host
			user := parsedURI.User.Username()

			// Build SSH command with all necessary options
			sshArgs := []string{
				"-e", // Read password from SSHPASS environment variable
				"ssh",
				"-o", "PasswordAuthentication=yes",
				"-o", "PubkeyAuthentication=no",
				"-o", "LogLevel=ERROR",
			}
			// Host-key options come from the centralized policy (#149/ADR-0004);
			// ControlMaster multiplexing reuses one connection (#194).
			sshArgs = append(sshArgs, v.hostKey.sshHostKeyOptions()...)
			sshArgs = append(sshArgs, sshMultiplexOptions()...)
			sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", user, host), "virsh")
			// Pin the remote virsh to the connection URI's libvirtd (root → system,
			// non-root → session) instead of letting it pick the ssh user's default.
			remoteVirshArgs := args
			if uri := remoteVirshConnectURI(v.uri); uri != "" {
				remoteVirshArgs = append([]string{"-c", uri}, args...)
			}
			sshArgs = append(sshArgs, remoteVirshArgs...)

			cmd = exec.CommandContext(ctx, "sshpass", sshArgs...)
			command = fmt.Sprintf("sshpass -e ssh %s@%s virsh %s", user, host, strings.Join(remoteVirshArgs, " "))
			cmd.Env = v.env
		} else {
			// Standard virsh command for local or key-based connections
			cmd = exec.CommandContext(ctx, "virsh", args...)
			command = "virsh " + strings.Join(args, " ")
			cmd.Env = v.env
		}
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	log.Printf("DEBUG Executing: %s", command)

	// Run the command
	err = cmd.Run()
	duration := time.Since(start)

	result := &VirshResult{
		Command:  command,
		ExitCode: cmd.ProcessState.ExitCode(),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}

	if err != nil {
		log.Printf("ERROR Command failed: %s (exit code: %d, duration: %v)",
			command, result.ExitCode, duration)
		log.Printf("ERROR Stderr: %s", result.Stderr)
		return result, &VirshError{
			Command:  command,
			ExitCode: result.ExitCode,
			Stderr:   result.Stderr,
			Stdout:   result.Stdout,
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

// createSSHConfig writes an SSH configuration file for non-interactive
// authentication that honours the centralized host-key verification policy
// (#149/ADR-0004). It is consumed by libvirt's own qemu+ssh:// transport (the
// key-based path), so the config must enforce the same StrictHostKeyChecking +
// UserKnownHostsFile that the explicit-argv password/scp paths use.
func (v *VirshProvider) createSSHConfig() error {
	// SSH config content honouring the centralized host-key policy
	// (#149/ADR-0004). Verifying by default (StrictHostKeyChecking yes +
	// credentials-mounted known_hosts); legacy accept-new + /tmp/known_hosts
	// only on the explicit escape-hatch path.
	sshConfig := v.hostKey.sshConfigStanza()

	var lastErr error
	for _, candidate := range []struct {
		dir  string
		home string
	}{
		{dir: "/home/app/.ssh"},
		{dir: "/tmp/.ssh", home: "/tmp"},
	} {
		configPath := candidate.dir + "/config"
		if err := os.MkdirAll(candidate.dir, 0700); err != nil {
			lastErr = err
			log.Printf("DEBUG Failed to create SSH directory %s: %v", candidate.dir, err)
			continue
		}
		if err := os.WriteFile(configPath, []byte(sshConfig), 0600); err != nil {
			lastErr = err
			log.Printf("DEBUG Failed to write SSH config at %s: %v", configPath, err)
			continue
		}
		if candidate.home != "" {
			v.env = append(v.env, "HOME="+candidate.home)
			log.Printf("INFO Using %s as HOME for SSH config", candidate.home)
		}
		log.Printf("INFO Created SSH config at %s honouring host-key policy", configPath)
		return nil
	}

	return fmt.Errorf("failed to create writable SSH config: %w", lastErr)
}

// Cleanup performs any necessary cleanup operations
func (v *VirshProvider) Cleanup() error {
	log.Printf("INFO Cleaning up virsh provider")

	// No persistent connections to close with virsh approach
	// All commands are stateless

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
