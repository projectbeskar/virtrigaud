//go:build libvirt_native

package libvirt

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
)

// These are opt-in LIVE integration tests for the native transport. They skip
// unless the relevant env vars are set, and run with `-tags libvirt_native`
// (which pulls in the CGO libvirt binding). Example:
//
//	LIBVIRT_NATIVE_URI='qemu+ssh://root@192.168.254.12/system?no_tty=1' \
//	  go test -tags libvirt_native -run TestNative -v ./internal/providers/libvirt/

// TestNativeSmoke drives the happy path against a live host: connect, probe
// liveness, list domains, and run the cheap DescribeBasic against one real
// domain plus a missing one. Read-only, no mutation.
func TestNativeSmoke(t *testing.T) {
	uri := os.Getenv("LIBVIRT_NATIVE_URI")
	if uri == "" {
		t.Skip("set LIBVIRT_NATIVE_URI to run the live native smoke test")
	}

	conn, err := dialNative(uri, resolveHostKeyPolicy())
	if err != nil {
		t.Fatalf("dialNative: %v", err)
	}
	defer conn.Close()

	ctx := context.Background()

	// 1. Cheap liveness probe (the Validate-storm fix): no fork, no ssh per call.
	if err := conn.Validate(ctx); err != nil {
		t.Fatalf("Validate (IsAlive): %v", err)
	}
	t.Log("Validate OK — connection alive")

	// 2. Enumerate real domains via the persistent connection.
	doms, err := conn.c.ListAllDomains(0)
	if err != nil {
		t.Fatalf("ListAllDomains: %v", err)
	}
	t.Logf("ListAllDomains OK — %d domains", len(doms))
	if len(doms) == 0 {
		return
	}
	name, err := doms[0].GetName()
	for i := range doms {
		_ = doms[i].Free()
	}
	if err != nil {
		t.Fatalf("GetName: %v", err)
	}

	// 3. Cheap existence+power probe on a real domain.
	exists, state, err := conn.DescribeBasic(ctx, name)
	if err != nil {
		t.Fatalf("DescribeBasic(%q): %v", name, err)
	}
	t.Logf("DescribeBasic(%q) OK — exists=%v state=%s", name, exists, state)

	// 3b. Full controller-facing Describe (all DescribeResponse fields).
	resp, err := conn.Describe(ctx, name)
	if err != nil {
		t.Fatalf("Describe(%q): %v", name, err)
	}
	if !resp.Exists {
		t.Fatalf("Describe(%q) reported Exists=false for a listed domain", name)
	}
	t.Logf("Describe(%q) OK — power=%s ips=%v console=%q raw[vcpus]=%s",
		name, resp.PowerState, resp.IPs, resp.ConsoleURL, resp.ProviderRaw["vcpus"])

	// 3c. Rich Describe: host-side stats always, guest-agent best-effort.
	rich, err := conn.DescribeRich(ctx, name)
	if err != nil {
		t.Fatalf("DescribeRich(%q): %v", name, err)
	}
	t.Logf("DescribeRich(%q) OK — %d raw keys; agent=%s os=%q rss=%s blk=%s",
		name, len(rich.ProviderRaw), rich.ProviderRaw["guest_agent"],
		rich.ProviderRaw["guest_os"], rich.ProviderRaw["mem_rss_kib"],
		rich.ProviderRaw["blk_vda_rd_bytes"])

	// 4. Negative path: a name that cannot exist must come back exists=false,
	//    nil error (ERR_NO_DOMAIN swallowed), not a transport error.
	exists, _, err = conn.DescribeBasic(ctx, "virtrigaud-nonexistent-"+name)
	if err != nil {
		t.Fatalf("DescribeBasic(missing) should not error: %v", err)
	}
	if exists {
		t.Fatalf("DescribeBasic(missing) reported exists=true")
	}
	t.Log("DescribeBasic(missing) OK — exists=false, no error")
}

// TestNativeTrustStrict proves, against a live host, that the native transport
// preserves the ADR-0004 trust contract — fail closed, no TOFU, one opt-out:
//
//  1. missing/empty known_hosts (verifying) -> pre-flight gate HARD-FAILS, no connect
//  2. WRONG host key                         -> connection REJECTED, file NOT rewritten
//     (no accept-new / no TOFU write-back)
//  3. explicit insecure escape hatch         -> the ONE opt-out lets it connect
//
// The wrong-key case drives qemu+libssh2:// (in-process, known_hosts enforced via
// URI params) rather than qemu+ssh://. Why: on OpenSSH 10.3 the ssh CLI resolves
// ~/.ssh/config from the passwd home and ignores $HOME, so a test cannot reliably
// force the provider's StrictHostKeyChecking config onto libvirt's ssh transport.
// libssh2 verification is config-independent and deterministic — host-key checking
// happens during KEX, before user auth, so it is unaffected by which key authn
// uses. (qemu+ssh enforcement itself is sound; verified out-of-band:
// `ssh -F <provider config>` with a wrong key returns "Host key verification failed".)
//
// Requires (set by the run wrapper):
//
//	LIBVIRT_NATIVE_URI   qemu+ssh://root@HOST/system?no_tty=1
//	LIBVIRT_KH_WRONG     a valid-format but WRONG host-key line for HOST
func TestNativeTrustStrict(t *testing.T) {
	uri := os.Getenv("LIBVIRT_NATIVE_URI")
	wrongKH := os.Getenv("LIBVIRT_KH_WRONG")
	if uri == "" || wrongKH == "" {
		t.Skip("set LIBVIRT_NATIVE_URI + LIBVIRT_KH_WRONG to run the strict trust test")
	}
	ctx := context.Background()

	writeKH := func(t *testing.T, content string) {
		t.Helper()
		if err := os.WriteFile(KnownHostsFile, []byte(content), 0600); err != nil {
			t.Fatalf("write known_hosts (need write access to %s): %v", KnownHostsFile, err)
		}
	}

	t.Run("missing trust material hard-fails (no TOFU)", func(t *testing.T) {
		writeKH(t, "")
		_, err := dialNative(uri, resolveHostKeyPolicy())
		if err == nil {
			t.Fatal("expected hard-fail with empty known_hosts, got nil (silent accept!)")
		}
		if !strings.Contains(err.Error(), "known_hosts") {
			t.Fatalf("error should name known_hosts, got: %v", err)
		}
		t.Logf("OK: empty known_hosts -> hard fail: %v", err)
	})

	t.Run("wrong host key rejected, no accept-new write-back", func(t *testing.T) {
		writeKH(t, wrongKH+"\n")
		_, err := dialNative(libssh2URI(t, uri), resolveHostKeyPolicy())
		if err == nil {
			t.Fatal("expected host-key rejection with wrong key, got nil (accepted unknown host!)")
		}
		if !strings.Contains(err.Error(), "HOST KEY VERIFICATION FAILED") {
			t.Fatalf("expected an explicit host-key rejection, got: %v", err)
		}
		// no-TOFU proof: a failed verify must NOT have written the real key back.
		after, readErr := os.ReadFile(KnownHostsFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.TrimSpace(string(after)) != strings.TrimSpace(wrongKH) {
			t.Fatalf("known_hosts mutated after a failed connect (accept-new/TOFU leak):\n%s", after)
		}
		t.Logf("OK: wrong host key -> rejected, file unchanged (no TOFU)")
	})

	t.Run("explicit insecure escape hatch connects", func(t *testing.T) {
		t.Setenv(EnvInsecureSkipHostKeyVerification, "true")
		writeKH(t, "") // no trust material at all
		insecureURI := uri
		if strings.Contains(insecureURI, "?") {
			insecureURI += "&no_verify=1"
		} else {
			insecureURI += "?no_verify=1"
		}
		conn, err := dialNative(insecureURI, resolveHostKeyPolicy())
		if err != nil {
			t.Fatalf("insecure opt-out should connect with no known_hosts, got: %v", err)
		}
		defer conn.Close()
		if err := conn.Validate(ctx); err != nil {
			t.Fatalf("Validate (insecure): %v", err)
		}
		t.Log("OK: explicit insecure opt-out -> connected (the single, audit-flagged bypass)")
	})
}

// libssh2URI rewrites a qemu+ssh:// URI to qemu+libssh2:// with the host-key
// policy carried in URI params (config-independent enforcement).
func libssh2URI(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme = "qemu+libssh2"
	q := u.Query()
	q.Set("known_hosts", KnownHostsFile)
	q.Set("known_hosts_verify", "normal")
	u.RawQuery = q.Encode()
	return u.String()
}
