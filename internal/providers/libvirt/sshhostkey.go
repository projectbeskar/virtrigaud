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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// SSH host-key verification policy for the libvirt provider (ADR-0004, #149).
//
// VirtRigaud's libvirt provider is a virsh-CLI-over-SSH provider: it shells out
// to `ssh`/`scp` (directly via sshpass for the password path, or implicitly via
// libvirt's own qemu+ssh:// transport for the key-based path). Historically every
// one of those code paths disabled SSH host-key verification — `no_verify=1` on
// the URI, `StrictHostKeyChecking=accept-new` + an ephemeral `/tmp/known_hosts`
// on the argv paths — leaving the manager→hypervisor channel open to MITM.
//
// This file is the SINGLE source of truth for the host-key policy. Every SSH/scp
// call site MUST route its host-key options through this helper so the on/off
// decision and the known_hosts location live in exactly one place (per the
// project rule: global constants for repeated literals, no duplicated ssh option
// strings). Five independently-maintained host-key policies is how #149 happened
// in the first place.
const (
	// EnvInsecureSkipHostKeyVerification is the explicit, audit-flagged escape
	// hatch. When set to "true" (case-insensitive, whitespace-trimmed) the
	// provider connects with host-key verification DISABLED and emits a loud
	// WARN log on every connection. It is settable per-Provider via the
	// existing spec.runtime.env field, mirroring ADR-0003's
	// VIRTRIGAUD_PROVIDER_INSECURE. Any other value (unset, "false", "1",
	// "yes") keeps verification ON. We deliberately require the literal word
	// "true" so the operator's intent is unambiguous and greppable in audit
	// logs.
	EnvInsecureSkipHostKeyVerification = "LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION"

	// KnownHostsFile is the in-pod path at which the verifying host-key
	// material is read. It lives inside the existing credentials Secret mount
	// (CredentialsPath, /etc/virtrigaud/credentials), so an operator who adds a
	// `known_hosts` key to the Provider's credentialSecretRef Secret gets it
	// projected here read-only with ZERO controller/mount changes (ADR-0004,
	// "trust material co-located in the provider's Secret").
	KnownHostsFile = CredentialsPath + "/known_hosts"

	// insecureKnownHostsFile is the ephemeral, never-persisted known_hosts file
	// used only on the insecure (escape-hatch) path. It is an emptyDir-backed
	// path that is wiped on pod restart; that re-trust-on-every-restart
	// behaviour is exactly why it MUST NOT be used on the verifying path.
	insecureKnownHostsFile = "/tmp/known_hosts"

	// strictYes / strictAcceptNew are the two StrictHostKeyChecking values the
	// helper selects between.
	strictYes       = "yes"
	strictAcceptNew = "accept-new"

	// EnvDisableSSHMultiplexing is the escape hatch that turns OFF SSH
	// connection multiplexing (ControlMaster). Multiplexing is ON by default
	// (#194) so a burst of `virsh`/`scp` invocations reuses a single SSH
	// connection instead of opening a fresh handshake per command — the churn
	// that can trip the libvirt host's sshd MaxStartups / fail2ban (the same
	// symptom #191 retries client-side). Set to the literal "true"
	// (case-insensitive, trimmed) to revert to one connection per command;
	// any other value keeps multiplexing on. Settable per-Provider via
	// spec.runtime.env, mirroring the other libvirt escape hatches.
	EnvDisableSSHMultiplexing = "LIBVIRT_SSH_DISABLE_MULTIPLEXING"

	// sshControlPath is the ssh ControlMaster socket path. `%C` is a short,
	// fixed-length hash of (local host, remote host, port, user), keeping the
	// path well under the AF_UNIX sun_path limit and unique per destination. It
	// lives under /tmp — writable in the provider container and wiped on pod
	// restart, so no stale control sockets survive a restart.
	sshControlPath = "/tmp/virtrigaud-ssh-%C"

	// sshControlPersist keeps the background master connection open this long
	// after the last multiplexed session, so subsequent commands (and the next
	// reconcile burst) reuse it instead of re-handshaking.
	sshControlPersist = "60s"
)

// sshMultiplexingEnabled reports whether SSH ControlMaster connection sharing is
// active. It is ON by default and disabled only by the explicit, greppable
// escape hatch EnvDisableSSHMultiplexing=true (#194).
func sshMultiplexingEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(EnvDisableSSHMultiplexing)), "true")
}

// sshMultiplexOptions returns the `ssh`/`scp` `-o key=value` flag pairs that
// enable ControlMaster connection sharing, laid out as alternating
// ["-o","k=v",...] so they spread directly into an argv builder. It returns nil
// when multiplexing is disabled (EnvDisableSSHMultiplexing=true) so callers fall
// back to a fresh connection per command. Reusing one connection across many
// virsh/scp invocations collapses the SSH handshake churn that can trip the
// host's MaxStartups/fail2ban (#191/#194). Independent of the host-key policy.
func sshMultiplexOptions() []string {
	if !sshMultiplexingEnabled() {
		return nil
	}
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + sshControlPath,
		"-o", "ControlPersist=" + sshControlPersist,
	}
}

// hostKeyPolicy captures the resolved SSH host-key verification posture for a
// single provider process. It is computed once (resolveHostKeyPolicy) and then
// consulted by every SSH/scp call site so the decision is taken in exactly one
// place.
type hostKeyPolicy struct {
	// insecure is true when EnvInsecureSkipHostKeyVerification=true; host-key
	// verification is disabled and a WARN is logged on every connection.
	insecure bool
}

// resolveHostKeyPolicy reads the escape-hatch env var once and returns the
// effective host-key policy. Verification is ON unless the operator has
// explicitly opted out with EnvInsecureSkipHostKeyVerification=true.
//
// The "true"-only check mirrors ADR-0003's isInsecureOptedIn: we match the exact
// word "true" (case-insensitive, trimmed) rather than any truthy value, so the
// operator's opt-out is deliberate and auditable.
func resolveHostKeyPolicy() hostKeyPolicy {
	return hostKeyPolicy{
		insecure: strings.EqualFold(strings.TrimSpace(os.Getenv(EnvInsecureSkipHostKeyVerification)), "true"),
	}
}

// strictHostKeyChecking returns the StrictHostKeyChecking value for the policy:
// "yes" when verifying, "accept-new" (legacy TOFU) on the insecure path.
func (p hostKeyPolicy) strictHostKeyChecking() string {
	if p.insecure {
		return strictAcceptNew
	}
	return strictYes
}

// knownHostsFile returns the UserKnownHostsFile path for the policy: the
// credentials-mounted, operator-supplied file when verifying; the ephemeral
// /tmp file on the insecure path.
func (p hostKeyPolicy) knownHostsFile() string {
	if p.insecure {
		return insecureKnownHostsFile
	}
	return KnownHostsFile
}

// sshHostKeyOptions returns the `ssh`/`scp` `-o key=value` flag pairs that
// enforce the policy. The slice is laid out as alternating ["-o", "k=v", ...]
// so it can be spread directly into an argv builder (sshpass/ssh/scp). Both the
// password (sshpass) virsh path and the scp disk-copy path consume this.
func (p hostKeyPolicy) sshHostKeyOptions() []string {
	return []string{
		"-o", "StrictHostKeyChecking=" + p.strictHostKeyChecking(),
		"-o", "UserKnownHostsFile=" + p.knownHostsFile(),
	}
}

// sshConfigStanza returns the body written into the provider's ~/.ssh/config so
// any ssh invocation that reads the config (notably libvirt's own qemu+ssh://
// transport) honours the same host-key policy as the explicit-argv paths.
func (p hostKeyPolicy) sshConfigStanza() string {
	stanza := fmt.Sprintf(`Host *
    StrictHostKeyChecking %s
    PasswordAuthentication yes
    PubkeyAuthentication yes
    UserKnownHostsFile %s
    LogLevel ERROR
`, p.strictHostKeyChecking(), p.knownHostsFile())
	// Mirror the explicit-argv ControlMaster options into the config so that
	// libvirt's own qemu+ssh:// transport also shares one connection (#194).
	if sshMultiplexingEnabled() {
		stanza += fmt.Sprintf(`    ControlMaster auto
    ControlPath %s
    ControlPersist %s
`, sshControlPath, sshControlPersist)
	}
	return stanza
}

// applyURIHostKeyOptions sets the host-key-related query parameters on the
// qemu+ssh:// URI used by libvirt's key-based transport. On the insecure path it
// restores the legacy `no_verify=1`; on the verifying path it omits no_verify
// entirely (so libvirt's ssh transport falls through to the verifying ~/.ssh/config
// + known_hosts written by createSSHConfig). The caller owns no_tty.
func (p hostKeyPolicy) applyURIHostKeyOptions(query interface {
	Set(key, value string)
	Del(key string)
}) {
	if p.insecure {
		query.Set("no_verify", "1")
	} else {
		query.Del("no_verify")
	}
}

// verifyKnownHostsPresent is the loud, actionable hard-fail gate. When
// verification is ON it requires that a non-empty known_hosts file is present at
// KnownHostsFile; otherwise it returns an error that names (a) the host, (b) the
// expected file path, (c) the ssh-keyscan recipe to populate it, and (d) the
// escape-hatch env var as the explicit opt-out. On the insecure path it is a
// no-op (the WARN log carries the audit signal instead).
//
// We deliberately reject trust-on-first-use: TOFU accepts whatever key the host
// presents on the first connection, which is exactly the MITM window #149 is
// about. The hard fail forces the operator to seed an out-of-band trust anchor.
//
// Interaction with I1 (ADR-0004): post-#149, an operator hitting the I1 libvirt
// connectivity failure with a stale/missing known_hosts will see a clean
// "Host key verification failed" (or this pre-flight error) instead of the old
// silent no_verify=1 success. That is the security control working as designed,
// not a regression — the two failure modes are distinguishable by error string
// (kex_exchange_identification = connectivity/host-side; host-key = trust
// material).
func (p hostKeyPolicy) verifyKnownHostsPresent(host string) error {
	if p.insecure {
		return nil
	}

	// A present, non-empty file is the only acceptable state. A missing file,
	// or a file that exists but is empty (no trust material), both fall through
	// to the actionable hard-fail error below.
	if info, err := os.Stat(filepath.Clean(KnownHostsFile)); err == nil && info.Size() > 0 {
		return nil
	}

	return fmt.Errorf(
		"libvirt SSH host-key verification is on (default) but no usable known_hosts "+
			"was found at %s for host %q. Seed it from a trusted bastion with "+
			"`ssh-keyscan -H %s >> known_hosts` and add it as the `known_hosts` key in the "+
			"credentials Secret referenced by the Provider's credentialSecretRef, OR set "+
			"%s=true to connect without verification (audit-flagged, NOT recommended for production)",
		KnownHostsFile, host, host, EnvInsecureSkipHostKeyVerification,
	)
}

// logVerificationMode emits exactly one startup/connect audit line for the
// policy. Banking auditors grep for this line, the same way they grep
// ADR-0003's TLSConfigured. On the insecure path it is a loud WARN that names
// the MITM exposure, that it is audit-flagged, and that it is intended only for
// lab/migration; on the verifying path it is an INFO naming the known_hosts
// path. Structured fields carry the host and provider for correlation.
func (p hostKeyPolicy) logVerificationMode(logger *slog.Logger, host string) {
	if logger == nil {
		logger = slog.Default()
	}
	if p.insecure {
		logger.Warn("LIBVIRT SSH HOST-KEY VERIFICATION DISABLED: "+
			"connecting without verifying the hypervisor's SSH host key. "+
			"The manager→hypervisor SSH/scp channel is exposed to MITM (credential leak + injected virsh/disk-image tampering). "+
			"This is audit-flagged per ADR-0004 and intended ONLY for lab/migration. "+
			"Unset "+EnvInsecureSkipHostKeyVerification+" and seed known_hosts to re-enable verification.",
			"provider", "libvirt",
			"host", host,
			"env_var", EnvInsecureSkipHostKeyVerification,
		)
		return
	}
	logger.Info("libvirt SSH host-key verification: enabled",
		"provider", "libvirt",
		"host", host,
		"known_hosts", KnownHostsFile,
	)
}
