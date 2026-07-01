package libvirt

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// controlTransport is the libvirt control-plane surface that can be served
// either by the legacy virsh-over-ssh path or the native libvirt-go SDK. Only
// the methods migrated so far are listed; the rest stay on virsh until moved.
type controlTransport interface {
	Validate(ctx context.Context) error
	Describe(ctx context.Context, id string) (contracts.DescribeResponse, error)
	Close()
}

// nativeDialer is registered (only) by the build-tagged native implementation's
// init(). It stays nil in a build without `-tags libvirt_native`, so requesting
// the native transport in such a binary is an explicit, loud error rather than
// a silent no-op.
var nativeDialer func(uri string, hostKey hostKeyPolicy) (controlTransport, error)

// nativeTransportEnabled reports whether the operator selected the native
// control transport via LIBVIRT_CONTROL_TRANSPORT=native (default: virsh).
func nativeTransportEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("LIBVIRT_CONTROL_TRANSPORT")), "native")
}

// buildNativeTransport dials the native transport reusing the already-built
// virsh connection URI and the one shared host-key policy. It converts the
// qemu+ssh URI to qemu+libssh2 so host-key verification is enforced in-process
// via URI params (known_hosts / known_hosts_verify) rather than depending on
// the system ssh client picking up the provider's ~/.ssh/config — which is
// environment-fragile (modern OpenSSH resolves the config from the passwd home,
// not $HOME). This keeps ADR-0004 trust semantics: same known_hosts file, same
// hard-fail gate (already run by dialNative), same single insecure opt-out.
func buildNativeTransport(v *VirshProvider) (controlTransport, error) {
	if nativeDialer == nil {
		return nil, fmt.Errorf("LIBVIRT_CONTROL_TRANSPORT=native requested but this binary was not built with -tags libvirt_native")
	}
	nativeURI, err := toLibssh2URI(v.uri)
	if err != nil {
		return nil, err
	}
	return nativeDialer(nativeURI, v.hostKey)
}

// toLibssh2URI rewrites an ssh-based libvirt URI to qemu+libssh2 with the
// host-key policy carried in URI params. The verifying path points libssh2 at
// the credentials-mounted known_hosts with known_hosts_verify=normal; the
// explicit insecure opt-out maps to known_hosts_verify=ignore. Auth params
// already on the URI (keyfile/sshauth/no_tty) are preserved.
func toLibssh2URI(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse libvirt URI %q: %w", raw, err)
	}
	if !strings.Contains(u.Scheme, "ssh") {
		return "", fmt.Errorf("native transport needs an ssh-based endpoint, got scheme %q", u.Scheme)
	}
	u.Scheme = "qemu+libssh2"
	q := u.Query()
	q.Set("known_hosts", KnownHostsFile)
	if resolveHostKeyPolicy().insecure {
		q.Set("known_hosts_verify", "ignore")
	} else {
		q.Set("known_hosts_verify", "normal")
	}
	q.Del("no_verify") // libssh2 uses known_hosts_verify, not the ssh transport's no_verify
	u.RawQuery = q.Encode()
	return u.String(), nil
}
