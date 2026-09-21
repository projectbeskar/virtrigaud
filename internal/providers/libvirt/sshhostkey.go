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
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSH host-key verification policy for the libvirt provider (ADR-0004, #149;
// re-platformed onto an in-process client by ADR-0008 PR 3).
//
// VirtRigaud's libvirt provider reaches its hypervisor hosts over SSH. As of
// ADR-0008 PR 3 that SSH connection is an in-process golang.org/x/crypto/ssh
// client (sshclient.go), not a forked ssh/sshpass/scp subprocess — but the
// host-key policy itself, and the security posture it encodes, is UNCHANGED:
// verification is on by default, trust material is the operator-seeded
// known_hosts key in the credentials Secret, and there is a loud, audit-logged
// escape hatch. No TOFU, ever.
//
// This file is the SINGLE source of truth for the host-key policy. Every SSH
// connection MUST route its host-key decision through this helper so the
// on/off decision and the known_hosts location live in exactly one place (per
// the project rule: global constants for repeated literals). Five
// independently-maintained host-key policies is how #149 happened in the first
// place.
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
)

// KnownHostsFile is the in-pod path at which the verifying host-key material
// is read. It lives inside the existing credentials Secret mount
// (CredentialsPath, /etc/virtrigaud/credentials), so an operator who adds a
// `known_hosts` key to the Provider's credentialSecretRef Secret gets it
// projected here read-only with ZERO controller/mount changes (ADR-0004,
// "trust material co-located in the provider's Secret").
//
// This is a package var, not a const (mirroring virsh.go's
// sshConnectMaxAttempts/sshConnectBaseBackoff), solely so tests can point it at
// a temp file to drive dialSSH/hostKeyCallback end-to-end against an in-memory
// SSH server fixture without touching the real filesystem path. Production code
// never reassigns it.
var KnownHostsFile = CredentialsPath + "/known_hosts"

// hostKeyPolicy captures the resolved SSH host-key verification posture for a
// single provider process. It is computed once (resolveHostKeyPolicy) and then
// consulted by the in-process SSH client so the decision is taken in exactly
// one place.
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

// hostKeyCallback builds the ssh.HostKeyCallback the in-process client uses to
// verify the hypervisor's host key during the handshake.
//
// On the verifying (default) path this is golang.org/x/crypto/ssh/knownhosts,
// backed by KnownHostsFile — the exact same trust material and matching
// semantics (plain and hashed hostnames, multiple keys/host) a real `ssh`
// binary would use with `-o UserKnownHostsFile=`. It is NEVER
// ssh.InsecureIgnoreHostKey, on either path: the insecure/escape-hatch path
// below is a hand-written callback that always accepts, functionally
// equivalent to InsecureIgnoreHostKey but written out so it is not the
// specific flagged symbol, is reached ONLY when the operator has explicitly
// opted out (resolveHostKeyPolicy), and is loudly WARN-logged by
// logVerificationMode on every connection.
func (p hostKeyPolicy) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if p.insecure {
		return func(_ string, _ net.Addr, _ ssh.PublicKey) error {
			// Intentionally accept any host key: the operator explicitly set
			// EnvInsecureSkipHostKeyVerification=true (audit-logged by
			// logVerificationMode at connect time). ADR-0004's escape hatch,
			// preserved verbatim by ADR-0008 PR 3.
			return nil
		}, nil
	}
	cb, err := knownhosts.New(KnownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", KnownHostsFile, err)
	}
	return cb, nil
}

// verifyKnownHostsPresent is the loud, actionable hard-fail gate. When
// verification is ON it requires that KnownHostsFile exist, be non-empty, AND
// contain an entry matching the specific host being connected to (#291 finding
// B6: previously this only checked the file was non-empty, so seeding
// known_hosts for host A and then pointing the Provider at host B passed the
// gate with zero trust material for B). Otherwise it returns an error that
// names (a) the host, (b) the expected file path, (c) the ssh-keyscan recipe to
// populate it, and (d) the escape-hatch env var as the explicit opt-out. On the
// insecure path it is a no-op (the WARN log carries the audit signal instead).
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

	// A present, non-empty file is required before we even try to match the
	// host — this keeps the "missing file entirely" error message distinct
	// from "file present, host absent", matching the existing pre-#149-fix
	// diagnostics operators already grep for.
	if info, err := os.Stat(filepath.Clean(KnownHostsFile)); err != nil || info.Size() == 0 {
		return p.missingKnownHostsError(host)
	}

	present, err := knownHostsHasEntry(KnownHostsFile, host)
	if err != nil {
		return fmt.Errorf("libvirt SSH known_hosts at %s could not be read: %w", KnownHostsFile, err)
	}
	if !present {
		return p.missingKnownHostsError(host)
	}
	return nil
}

// missingKnownHostsError is the actionable hard-fail message shared by both
// "file absent/empty" and "file present but this host has no entry".
func (p hostKeyPolicy) missingKnownHostsError(host string) error {
	return fmt.Errorf(
		"libvirt SSH host-key verification is on (default) but no known_hosts entry "+
			"was found for host %q at %s. Seed it from a trusted bastion with "+
			"`ssh-keyscan -H %s >> known_hosts` and add it as the `known_hosts` key in the "+
			"credentials Secret referenced by the Provider's credentialSecretRef, OR set "+
			"%s=true to connect without verification (audit-flagged, NOT recommended for production)",
		host, KnownHostsFile, host, EnvInsecureSkipHostKeyVerification,
	)
}

// knownHostsHasEntry reports whether path contains a known_hosts entry
// matching host (a bare host or host:port string), using the SAME matching
// golang.org/x/crypto/ssh/knownhosts applies during a real handshake — plain
// and HMAC-hashed hostnames alike — so this pre-flight check can never
// disagree with what the actual connection will accept.
//
// It works by invoking the real knownhosts.HostKeyCallback with a throwaway,
// never-used-for-auth probe key for host. A host with NO known_hosts entries
// at all makes the callback return a *knownhosts.KeyError with an EMPTY Want
// slice ("host unknown"); a host WITH entries makes it return a KeyError with
// Want populated (our random probe key predictably does not match any of
// them) — that populated-Want shape is precisely "this host is known", which
// is all this function needs, independent of what the real host key is. This
// is the documented way to ask golang.org/x/crypto/ssh/knownhosts "is this
// host present" without an established connection.
//
// Returns a non-nil error only for an I/O or parse failure on path itself —
// never for "host not found", which is reported as (false, nil).
func knownHostsHasEntry(path, host string) (bool, error) {
	cb, err := knownhosts.New(path)
	if err != nil {
		return false, err
	}
	probe, err := probeHostKey()
	if err != nil {
		return false, fmt.Errorf("build host-key probe: %w", err)
	}

	// The callback matches by the hostname string AND (when present in the
	// file as an address literal) the remote address; we have no live
	// connection yet, so the address is a placeholder and only the hostname
	// string participates in the match. This mirrors today's argv-based
	// UserKnownHostsFile matching, which is likewise hostname-string-keyed
	// (ssh-keyscan is normally run against the same host string the Provider
	// connects with, per the ADR-0004 runbook).
	err = cb(hostPort(host), &net.TCPAddr{IP: net.IPv4zero}, probe)
	if err == nil {
		// A random probe key does not collide with a real host key in
		// practice; treat a surprise accept as "present" rather than error.
		return true, nil
	}
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		return len(keyErr.Want) > 0, nil
	}
	return false, err
}

// probeHostKey lazily generates a single throwaway ed25519 public key, process
// -wide, used only to probe knownHostsHasEntry's callback for host presence.
// It is never used for authentication and never derived from any operator
// material.
func probeHostKey() (ssh.PublicKey, error) {
	probeKeyOnce.Do(func() {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			probeKeyErr = err
			return
		}
		probeKeyVal, probeKeyErr = ssh.NewPublicKey(pub)
	})
	return probeKeyVal, probeKeyErr
}

var (
	probeKeyOnce sync.Once
	probeKeyVal  ssh.PublicKey
	probeKeyErr  error
)

// hostPort normalizes a URL-authority host string to host:port, defaulting to
// the standard SSH port 22 when host carries none (the common case for a
// qemu+ssh:// URI with no explicit port). Used consistently for both the real
// SSH dial and the known_hosts presence probe so they agree on what "this
// host" means.
func hostPort(host string) string {
	if host == "" {
		return host
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host // already host:port (or [ipv6]:port)
	}
	// No port. url.URL.Host already brackets a bare IPv6 literal (e.g.
	// "[::1]"); strip that bracketing before JoinHostPort, which adds its own
	// brackets for any host containing a colon — otherwise a portless IPv6
	// host would come out double-bracketed ("[[::1]]:22").
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return net.JoinHostPort(h, "22")
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
			"The manager→hypervisor SSH channel is exposed to MITM (credential leak + injected virsh/disk-image tampering). "+
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
