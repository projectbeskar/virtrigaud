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

package proxmox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// SSH disk DATA-plane transport for the Proxmox provider (ADR-0006 Slice 3).
//
// The Proxmox provider talks to PVE over a REST API token for the CONTROL plane
// (Create/Delete/GetDiskInfo). That API has no streaming disk-transfer endpoint
// suitable for cross-hypervisor migration, so the migration DATA plane runs over
// SSH to the PVE node — the SAME host as the API endpoint
// (PROVIDER_ENDPOINT=https://pve.lab.k8:8006 → ssh host pve.lab.k8). `root@pam`
// maps to the node's Linux root, so an SSH user/password (or private key) reaches
// a shell that can run qemu-img / qm / pvesm against the node's storage.
//
// This file is ported from the libvirt provider's SSH transport (virsh.go,
// sshhostkey.go) so the two providers share an identical host-key policy,
// key-materialization, sshpass/-i and ControlMaster posture. The libvirt provider
// remains the single source of truth for the policy SEMANTICS; this is a
// Proxmox-namespaced copy (its own env-var spellings, key dir, control-socket
// path) so an operator configures each provider independently.

const (
	// EnvProxmoxInsecureSkipHostKeyVerification is the explicit, audit-flagged
	// escape hatch that disables SSH host-key verification on the Proxmox DATA
	// plane (#149 semantics). When set to the literal "true" (case-insensitive,
	// whitespace-trimmed) the provider connects without verifying the PVE node's
	// SSH host key and emits a loud WARN on every connection. Any other value
	// (unset, "false", "1", "yes") keeps verification ON. Settable per-Provider
	// via spec.runtime.env.
	EnvProxmoxInsecureSkipHostKeyVerification = "PROXMOX_INSECURE_SKIP_HOST_KEY_VERIFICATION"

	// EnvProxmoxDisableSSHMultiplexing turns OFF SSH ControlMaster connection
	// sharing on the Proxmox DATA plane (#194 semantics). Multiplexing is ON by
	// default so a burst of qemu-img/qm/pvesm invocations reuses one SSH
	// connection. Set to the literal "true" (case-insensitive, trimmed) to revert
	// to one connection per command.
	EnvProxmoxDisableSSHMultiplexing = "PROXMOX_SSH_DISABLE_MULTIPLEXING"

	// proxmoxCredentialsPath is the in-pod directory at which the Provider's
	// credentialSecretRef Secret is projected read-only. The control-plane token
	// files (token_id/token_secret) already live here (server.go); the DATA-plane
	// SSH credential files (ssh_user/ssh_password/ssh_privatekey/known_hosts) are
	// read from the same mount with ZERO controller/mount changes.
	proxmoxCredentialsPath = "/etc/virtrigaud/credentials"

	// proxmoxKnownHostsFile is the in-pod path at which the verifying host-key
	// material is read. It lives inside the existing credentials mount, so an
	// operator who adds a `known_hosts` key to the Provider's Secret gets it
	// projected here with no mount changes (ADR-0004).
	proxmoxKnownHostsFile = proxmoxCredentialsPath + "/known_hosts"

	// proxmoxInsecureKnownHostsFile is the ephemeral, never-persisted known_hosts
	// file used only on the insecure (escape-hatch) path. It is wiped on pod
	// restart; that re-trust-on-every-restart behaviour is exactly why it MUST
	// NOT be used on the verifying path.
	proxmoxInsecureKnownHostsFile = "/tmp/known_hosts"

	// proxmoxSSHPrivateKeyDir is the directory the provider materializes the SSH
	// private key into (0700). It is Proxmox-specific so it never collides with
	// the libvirt key dir. NOTE: the provider controller backs the libvirt key
	// dir with a memory-backed (emptyDir medium=Memory) volume (#250); for
	// production hardening the controller should mount the SAME kind of volume at
	// this path for proxmox so the private key never touches the pod's writable
	// container layer. Until then the key lands on the container's ephemeral
	// overlay, which is acceptable for the lab but flagged for follow-up.
	proxmoxSSHPrivateKeyDir = "/tmp/virtrigaud-proxmox"

	// proxmoxSSHPrivateKeyPath is the 0600 file the materialized key is written
	// to inside proxmoxSSHPrivateKeyDir.
	proxmoxSSHPrivateKeyPath = proxmoxSSHPrivateKeyDir + "/ssh-privatekey"

	// proxmoxSSHControlPath is the ssh ControlMaster socket path. `%C` is a
	// short, fixed-length hash of (local host, remote host, port, user), keeping
	// the path well under the AF_UNIX sun_path limit and unique per destination.
	// It lives under /tmp — writable in the provider container and wiped on pod
	// restart, so no stale control sockets survive a restart.
	proxmoxSSHControlPath = "/tmp/virtrigaud-proxmox-ssh-%C"

	// proxmoxSSHControlPersist keeps the background master connection open this
	// long after the last multiplexed session, so subsequent commands reuse it
	// instead of re-handshaking.
	proxmoxSSHControlPersist = "60s"

	// proxmoxStrictYes / proxmoxStrictAcceptNew are the two
	// StrictHostKeyChecking values the policy selects between.
	proxmoxStrictYes       = "yes"
	proxmoxStrictAcceptNew = "accept-new"

	// Credential file names read from proxmoxCredentialsPath for the DATA plane.
	credFileSSHUser       = "ssh_user"
	credFileSSHPassword   = "ssh_password"
	credFileSSHPrivateKey = "ssh_privatekey"
)

// sshCredentials holds the resolved SSH DATA-plane credentials for the Proxmox
// node. The control-plane API token is held separately by the pveapi.Client; this
// struct carries ONLY the material needed to open a shell on the PVE node.
type sshCredentials struct {
	// User is the SSH login (e.g. "root"). Defaults to "root" when unset, since
	// root@pam maps to the node's Linux root.
	User string
	// Password is the SSH password (sshpass path). Mutually preferred over the
	// private key: when both are set, the password path wins to mirror the
	// libvirt provider's precedence.
	Password string
	// PrivateKey is the PEM SSH private key (the `ssh -i` path). Materialized to
	// proxmoxSSHPrivateKeyPath when used.
	PrivateKey string
}

// sshTransport is the resolved SSH DATA-plane transport for one provider process.
// It is built once (newSSHTransport) and consumed by the s3export/s3import paths
// and the Create import-disk attachment. The host comes from the SAME
// PROVIDER_ENDPOINT the control-plane API uses.
type sshTransport struct {
	// host is the PVE node hostname (and optional :port stripped) derived from
	// the API endpoint.
	host string
	// creds carries the SSH login material.
	creds sshCredentials
	// hostKey is the resolved host-key verification policy.
	hostKey proxmoxHostKeyPolicy
	// keyFile is the on-disk path the private key was materialized to, or "" when
	// password auth is used. Populated lazily by ensureKeyFile.
	keyFile string
	// logger carries the structured host-key audit log.
	logger *slog.Logger
}

// proxmoxHostKeyPolicy captures the resolved SSH host-key verification posture.
// It mirrors the libvirt hostKeyPolicy (ADR-0004, #149) so the on/off decision
// and the known_hosts location live in exactly one place per provider.
type proxmoxHostKeyPolicy struct {
	// insecure is true when EnvProxmoxInsecureSkipHostKeyVerification=true;
	// host-key verification is disabled and a WARN is logged on every connection.
	insecure bool
}

// resolveProxmoxHostKeyPolicy reads the escape-hatch env var once and returns the
// effective host-key policy. Verification is ON unless the operator has
// explicitly opted out with EnvProxmoxInsecureSkipHostKeyVerification=true. The
// "true"-only check mirrors ADR-0003's isInsecureOptedIn.
func resolveProxmoxHostKeyPolicy() proxmoxHostKeyPolicy {
	return proxmoxHostKeyPolicy{
		insecure: strings.EqualFold(strings.TrimSpace(os.Getenv(EnvProxmoxInsecureSkipHostKeyVerification)), "true"),
	}
}

// strictHostKeyChecking returns the StrictHostKeyChecking value for the policy:
// "yes" when verifying, "accept-new" (legacy TOFU) on the insecure path.
func (p proxmoxHostKeyPolicy) strictHostKeyChecking() string {
	if p.insecure {
		return proxmoxStrictAcceptNew
	}
	return proxmoxStrictYes
}

// knownHostsFile returns the UserKnownHostsFile path for the policy: the
// credentials-mounted, operator-supplied file when verifying; the ephemeral /tmp
// file on the insecure path.
func (p proxmoxHostKeyPolicy) knownHostsFile() string {
	if p.insecure {
		return proxmoxInsecureKnownHostsFile
	}
	return proxmoxKnownHostsFile
}

// sshHostKeyOptions returns the `ssh` `-o key=value` flag pairs that enforce the
// policy, laid out as alternating ["-o", "k=v", ...] so they spread directly into
// an argv builder. Both the sshpass (password) and `ssh -i` (key) paths consume
// this.
func (p proxmoxHostKeyPolicy) sshHostKeyOptions() []string {
	return []string{
		"-o", "StrictHostKeyChecking=" + p.strictHostKeyChecking(),
		"-o", "UserKnownHostsFile=" + p.knownHostsFile(),
	}
}

// verifyKnownHostsPresent is the loud, actionable hard-fail gate. When
// verification is ON it requires a non-empty known_hosts file at
// proxmoxKnownHostsFile; otherwise it returns an error naming (a) the host, (b)
// the expected file path, (c) the ssh-keyscan recipe, and (d) the escape-hatch
// env var. On the insecure path it is a no-op (the WARN log carries the audit
// signal). It deliberately rejects trust-on-first-use (the MITM window #149 is
// about).
func (p proxmoxHostKeyPolicy) verifyKnownHostsPresent(host string) error {
	if p.insecure {
		return nil
	}
	if info, err := os.Stat(proxmoxKnownHostsFile); err == nil && info.Size() > 0 {
		return nil
	}
	return fmt.Errorf(
		"proxmox SSH host-key verification is on (default) but no usable known_hosts "+
			"was found at %s for host %q. Seed it from a trusted bastion with "+
			"`ssh-keyscan -H %s >> known_hosts` and add it as the `known_hosts` key in the "+
			"credentials Secret referenced by the Provider's credentialSecretRef, OR set "+
			"%s=true to connect without verification (audit-flagged, NOT recommended for production)",
		proxmoxKnownHostsFile, host, host, EnvProxmoxInsecureSkipHostKeyVerification,
	)
}

// logVerificationMode emits exactly one connect audit line for the policy.
// Auditors grep for this line. On the insecure path it is a loud WARN naming the
// MITM exposure; on the verifying path it is an INFO naming the known_hosts path.
func (p proxmoxHostKeyPolicy) logVerificationMode(logger *slog.Logger, host string) {
	if logger == nil {
		logger = slog.Default()
	}
	if p.insecure {
		logger.Warn("PROXMOX SSH HOST-KEY VERIFICATION DISABLED: "+
			"connecting without verifying the PVE node's SSH host key. "+
			"The manager→hypervisor SSH disk-transfer channel is exposed to MITM (credential leak + injected disk-image tampering). "+
			"This is audit-flagged per ADR-0004 and intended ONLY for lab/migration. "+
			"Unset "+EnvProxmoxInsecureSkipHostKeyVerification+" and seed known_hosts to re-enable verification.",
			"provider", "proxmox",
			"host", host,
			"env_var", EnvProxmoxInsecureSkipHostKeyVerification,
		)
		return
	}
	logger.Info("proxmox SSH host-key verification: enabled",
		"provider", "proxmox",
		"host", host,
		"known_hosts", proxmoxKnownHostsFile,
	)
}

// proxmoxSSHMultiplexingEnabled reports whether SSH ControlMaster connection
// sharing is active. It is ON by default and disabled only by the explicit,
// greppable escape hatch EnvProxmoxDisableSSHMultiplexing=true (#194).
func proxmoxSSHMultiplexingEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(EnvProxmoxDisableSSHMultiplexing)), "true")
}

// proxmoxSSHMultiplexOptions returns the `ssh` `-o key=value` flag pairs that
// enable ControlMaster connection sharing, laid out as alternating
// ["-o","k=v",...]. It returns nil when multiplexing is disabled so callers fall
// back to a fresh connection per command.
func proxmoxSSHMultiplexOptions() []string {
	if !proxmoxSSHMultiplexingEnabled() {
		return nil
	}
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + proxmoxSSHControlPath,
		"-o", "ControlPersist=" + proxmoxSSHControlPersist,
	}
}

// proxmoxSSHKeyAuthOptions returns the ssh flags that force key-based
// authentication using keyFile. The key lives at a non-default path with no
// IdentityFile in ~/.ssh/config, so -i is mandatory, and IdentitiesOnly + the
// explicit auth toggles stop ssh from wandering onto an agent key or a password
// prompt. It mirrors the libvirt sshKeyAuthOptions exactly.
func proxmoxSSHKeyAuthOptions(keyFile string) []string {
	return []string{
		"-i", keyFile,
		"-o", "IdentitiesOnly=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "PubkeyAuthentication=yes",
	}
}

// sshHostFromEndpoint derives the SSH host from the PVE API endpoint
// (PROVIDER_ENDPOINT=https://pve.lab.k8:8006 → pve.lab.k8). The API port (8006)
// is stripped: SSH uses port 22. An empty or unparseable endpoint yields "".
func sshHostFromEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	// Tolerate a bare host[:port] with no scheme by giving url.Parse a scheme.
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	if h := u.Hostname(); h != "" {
		return h
	}
	return ""
}

// loadSSHCredentials resolves the SSH DATA-plane credentials from the mounted
// credentials files first, then PROVIDER_*/PVE_* env fallbacks, mirroring the
// control-plane token loading in server.go. The User defaults to "root" (root@pam
// maps to the node's Linux root). NOTHING is logged here; callers log only paths.
func loadSSHCredentials() sshCredentials {
	user := readCredentialFile(proxmoxCredentialsPath + "/" + credFileSSHUser)
	password := readCredentialFile(proxmoxCredentialsPath + "/" + credFileSSHPassword)
	privateKey := readCredentialFile(proxmoxCredentialsPath + "/" + credFileSSHPrivateKey)

	if user == "" {
		user = firstNonEmptyEnv("PROVIDER_SSH_USER", "PVE_SSH_USER")
	}
	if password == "" {
		password = firstNonEmptyEnv("PROVIDER_SSH_PASSWORD", "PVE_SSH_PASSWORD")
	}
	if privateKey == "" {
		privateKey = firstNonEmptyEnv("PROVIDER_SSH_PRIVATE_KEY", "PVE_SSH_PRIVATE_KEY")
	}

	if user == "" {
		user = "root"
	}
	return sshCredentials{User: user, Password: password, PrivateKey: privateKey}
}

// firstNonEmptyEnv returns the value of the first set, non-empty environment
// variable in order, or "" when none is set.
func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// newSSHTransport builds the SSH DATA-plane transport from the API endpoint
// (host) and the mounted/env SSH credentials. It resolves the host-key policy
// once. It returns an error only when the host cannot be derived; credential
// absence is deferred to connect time so Validate (control-plane only) still
// succeeds on a token-only deployment.
func newSSHTransport(endpoint string, logger *slog.Logger) (*sshTransport, error) {
	host := sshHostFromEndpoint(endpoint)
	if host == "" {
		return nil, fmt.Errorf("cannot derive SSH host from PVE endpoint %q (set PROVIDER_ENDPOINT)", endpoint)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &sshTransport{
		host:    host,
		creds:   loadSSHCredentials(),
		hostKey: resolveProxmoxHostKeyPolicy(),
		logger:  logger,
	}, nil
}

// writeProxmoxSSHPrivateKey persists the PEM private key (trimmed, single
// trailing newline) at proxmoxSSHPrivateKeyPath with 0600 perms inside a 0700
// dir (#249), and returns the path. It logs only the PATH, never the key bytes.
func writeProxmoxSSHPrivateKey(pem string) (string, error) {
	if err := os.MkdirAll(proxmoxSSHPrivateKeyDir, 0o700); err != nil {
		return "", fmt.Errorf("create SSH private key directory %s: %w", proxmoxSSHPrivateKeyDir, err)
	}
	key := strings.TrimSpace(pem) + "\n"
	if err := os.WriteFile(proxmoxSSHPrivateKeyPath, []byte(key), 0o600); err != nil {
		return "", fmt.Errorf("write SSH private key to %s: %w", proxmoxSSHPrivateKeyPath, err)
	}
	if err := os.Chmod(proxmoxSSHPrivateKeyPath, 0o600); err != nil {
		return "", fmt.Errorf("chmod SSH private key %s: %w", proxmoxSSHPrivateKeyPath, err)
	}
	return proxmoxSSHPrivateKeyPath, nil
}

// ensureKeyFile materializes the SSH private key to disk on first use (when key
// auth is in play and no password is set) and caches the path. It is idempotent.
func (t *sshTransport) ensureKeyFile() (string, error) {
	if t.keyFile != "" {
		return t.keyFile, nil
	}
	path, err := writeProxmoxSSHPrivateKey(t.creds.PrivateKey)
	if err != nil {
		return "", err
	}
	t.keyFile = path
	return path, nil
}

// usePassword reports whether the password (sshpass) path should be taken. The
// password path wins when a password is set, mirroring the libvirt precedence.
func (t *sshTransport) usePassword() bool {
	return t.creds.Password != ""
}

// useKey reports whether the key (`ssh -i`) path should be taken: no password but
// a non-empty private key.
func (t *sshTransport) useKey() bool {
	return t.creds.Password == "" && strings.TrimSpace(t.creds.PrivateKey) != ""
}

// buildSSHCommand assembles the *exec.Cmd for running remoteCmd on the PVE node.
// It selects the sshpass (password) branch or the `ssh -i` (key) branch, applies
// the host-key policy options and ControlMaster multiplexing, and returns the
// command WITHOUT wiring stdio (the caller sets Stdin/Stdout/Stderr). It is the
// single place the argv is built, so the export/import/attach call sites cannot
// drift. It logs only the host and the remote command — never credentials.
func (t *sshTransport) buildSSHCommand(ctx context.Context, remoteCmd string) (*exec.Cmd, error) {
	// Host-key pre-flight: emit the audit line and hard-fail if verification is
	// on but no usable known_hosts is present (no TOFU).
	t.hostKey.logVerificationMode(t.logger, t.host)
	if err := t.hostKey.verifyKnownHostsPresent(t.host); err != nil {
		return nil, fmt.Errorf("proxmox SSH host-key verification pre-flight failed: %w", err)
	}

	target := fmt.Sprintf("%s@%s", t.creds.User, t.host)

	if t.usePassword() {
		// sshpass -e reads the password from the SSHPASS env var so it never
		// appears in the process argv (visible in `ps`).
		sshArgs := []string{
			"-e",
			"ssh",
			"-o", "PasswordAuthentication=yes",
			"-o", "PubkeyAuthentication=no",
			"-o", "LogLevel=ERROR",
		}
		sshArgs = append(sshArgs, t.hostKey.sshHostKeyOptions()...)
		sshArgs = append(sshArgs, proxmoxSSHMultiplexOptions()...)
		sshArgs = append(sshArgs, target, remoteCmd)
		cmd := exec.CommandContext(ctx, "sshpass", sshArgs...)
		cmd.Env = append(os.Environ(), fmt.Sprintf("SSHPASS=%s", t.creds.Password))
		t.logger.Debug("Built proxmox SSH command (password auth)", "host", t.host, "user", t.creds.User, "remote_cmd", remoteCmd)
		return cmd, nil
	}

	if t.useKey() {
		keyFile, err := t.ensureKeyFile()
		if err != nil {
			return nil, fmt.Errorf("materialize SSH private key: %w", err)
		}
		sshArgs := proxmoxSSHKeyAuthOptions(keyFile)
		sshArgs = append(sshArgs, "-o", "LogLevel=ERROR")
		sshArgs = append(sshArgs, t.hostKey.sshHostKeyOptions()...)
		sshArgs = append(sshArgs, proxmoxSSHMultiplexOptions()...)
		sshArgs = append(sshArgs, target, remoteCmd)
		cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
		t.logger.Debug("Built proxmox SSH command (key auth)", "host", t.host, "user", t.creds.User, "key_file", keyFile, "remote_cmd", remoteCmd)
		return cmd, nil
	}

	return nil, fmt.Errorf(
		"no SSH credentials for the migration data plane: provide %q (or %q) in the Provider's credentials Secret, or set PROVIDER_SSH_PASSWORD / PROVIDER_SSH_PRIVATE_KEY",
		credFileSSHPassword, credFileSSHPrivateKey)
}

// runSSH runs remoteCmd on the PVE node and returns its captured stdout and
// stderr. Used for short control commands (e.g. `pvesm path`, `qm importdisk`).
// On a non-zero exit it returns an error that includes the trimmed stderr so the
// real cause is surfaced.
func (t *sshTransport) runSSH(ctx context.Context, remoteCmd string) (stdout, stderr string, err error) {
	cmd, err := t.buildSSHCommand(ctx, remoteCmd)
	if err != nil {
		return "", "", err
	}
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.Stdin = nil
	runErr := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()
	if runErr != nil {
		return stdout, stderr, fmt.Errorf("ssh %s: %w (stderr: %s)", t.host, runErr, strings.TrimSpace(stderr))
	}
	return stdout, stderr, nil
}

// runSSHStdout runs remoteCmd on the PVE node, streaming the command's stdout into
// w (a pipe), and returns when the command exits. It wires the remote process's
// stdout to w so a multi-GB disk streams OUT of the node without being buffered in
// the pod (the export path: `cat <hostTmp>` → S3). It is the symmetric sibling of
// runSSHStdin.
func (t *sshTransport) runSSHStdout(ctx context.Context, w io.Writer, remoteCmd string) error {
	cmd, err := t.buildSSHCommand(ctx, remoteCmd)
	if err != nil {
		return err
	}
	cmd.Stdout = w
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runSSHStdin runs remoteCmd on the PVE node, streaming stdin from r (a pipe), and
// returns when the command exits. It wires r to the remote process's stdin so a
// multi-GB disk streams IN to the node without being buffered in the pod (the
// import staging path: S3 → `cat > <hostTmp>`).
func (t *sshTransport) runSSHStdin(ctx context.Context, r io.Reader, remoteCmd string) error {
	cmd, err := t.buildSSHCommand(ctx, remoteCmd)
	if err != nil {
		return err
	}
	cmd.Stdin = r
	cmd.Stdout = io.Discard
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// proxmoxShellQuote single-quotes a path for safe interpolation into a remote
// shell command, escaping embedded single quotes. It mirrors the libvirt
// shellQuote so paths with spaces or metacharacters cannot break the command.
func proxmoxShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
