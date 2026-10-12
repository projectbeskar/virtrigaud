/*
Copyright 2026.

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

package main

import (
	"crypto/tls"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/version"
)

// TestVersionString verifies the banner emitted by `--version`.
//
// We test versionString() directly rather than invoking main() with
// os.Args = []string{"manager", "--version"}, because the production
// handler exits the process via os.Exit(0). Subprocess execution
// (os/exec on the built binary) is exercised by `make build` + manual
// run; the unit test pins the *content* of the banner so dashboards or
// release-verification scripts that grep for "virtrigaud-manager"
// continue to work.
//
// Pinned by H1 PR-1 / #114.
func TestVersionString(t *testing.T) {
	s := versionString()

	require.NotEmpty(t, s, "versionString() must never return empty")
	assert.True(t, strings.HasPrefix(s, "virtrigaud-manager "),
		"banner must start with 'virtrigaud-manager ' so release-verification grep patterns keep working; got %q", s)

	tail := strings.TrimPrefix(s, "virtrigaud-manager ")
	assert.NotEmpty(t, tail,
		"banner must include the version.String() payload after the prefix; got bare %q", s)

	// Pin that we delegate to internal/version.String() so the two
	// callers (this banner + virtrigaud_build_info metric label) stay
	// in lockstep. If someone refactors versionString() to embed a
	// hardcoded string, this catches the regression.
	assert.Equal(t, "virtrigaud-manager "+version.String(), s,
		"banner must be 'virtrigaud-manager ' + version.String() verbatim")
}

// TestEnforceTLSMinVersion pins the explicit TLS floor applied to the manager's
// webhook and metrics servers. main() seeds the shared tlsOpts slice with
// enforceTLSMinVersion, and both webhookTLSOpts and metricsServerOptions.TLSOpts
// derive from tlsOpts, so a regression here (floor lowered, or the mutator
// clobbering a sibling field) silently weakens admission-path and metrics TLS
// for the whole manager — exactly the class of drift the banking/regulated
// posture must not allow.
//
// Added by the webhook/metrics TLS-hardening change.
func TestEnforceTLSMinVersion(t *testing.T) {
	t.Run("sets the TLS 1.2 floor on a bare config", func(t *testing.T) {
		cfg := &tls.Config{}
		enforceTLSMinVersion(cfg)
		assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion,
			"floor must be TLS 1.2 (tls.VersionTLS12)")
		assert.Equal(t, uint16(tls.VersionTLS12), uint16(tlsVersionFloor),
			"tlsVersionFloor const must stay pinned at TLS 1.2")
	})

	t.Run("only touches MinVersion, composing with sibling mutators", func(t *testing.T) {
		// Mirror the two sibling mutators main() layers into tlsOpts /
		// webhookTLSOpts / metricsServerOptions.TLSOpts: disableHTTP2 sets
		// NextProtos, and the certwatcher callback sets GetCertificate. Applying
		// them in EITHER order relative to the floor must leave all three fields
		// intact — this is the "never clobbered by a later appended func"
		// invariant the wiring in main() relies on.
		setNextProtos := func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} }
		setGetCert := func(c *tls.Config) {
			c.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }
		}

		// Floor first (as in main(): enforceTLSMinVersion is tlsOpts[0]).
		cfg := &tls.Config{}
		for _, mut := range []func(*tls.Config){enforceTLSMinVersion, setNextProtos, setGetCert} {
			mut(cfg)
		}
		assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion,
			"MinVersion must survive sibling mutators appended after the floor")
		assert.Equal(t, []string{"http/1.1"}, cfg.NextProtos,
			"floor must not clobber NextProtos (http/2 disable)")
		assert.NotNil(t, cfg.GetCertificate,
			"floor must not clobber GetCertificate (certwatcher hot-reload)")

		// Floor last (defensive: even if a future refactor reorders tlsOpts).
		cfg2 := &tls.Config{}
		for _, mut := range []func(*tls.Config){setGetCert, setNextProtos, enforceTLSMinVersion} {
			mut(cfg2)
		}
		assert.Equal(t, uint16(tls.VersionTLS12), cfg2.MinVersion)
		assert.Equal(t, []string{"http/1.1"}, cfg2.NextProtos)
		assert.NotNil(t, cfg2.GetCertificate)
	})
}

// managerSubprocessEnv, when set to "1", makes
// TestMaxDiskSizeFlag_InvalidRefusedAtStartup run main() itself (in a child
// test process) with the --max-disk-size value in managerSubprocessMaxDiskEnv.
const (
	managerSubprocessEnv        = "VIRTRIGAUD_TEST_RUN_MANAGER_MAIN"
	managerSubprocessMaxDiskEnv = "VIRTRIGAUD_TEST_MAX_DISK_SIZE"
)

// TestMaxDiskSizeFlag_InvalidRefusedAtStartup: an invalid --max-disk-size
// stops the manager at once (exit 1, before any cluster connection) rather
// than running without the cap the administrator asked for.
func TestMaxDiskSizeFlag_InvalidRefusedAtStartup(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = []string{"manager", "--max-disk-size=" + os.Getenv(managerSubprocessMaxDiskEnv)}
		main()
		return
	}
	for _, bad := range []string{"lots", "500", "0", "-1Gi", "1023Mi", "none", "1e30", "65Pi", "1.1Gi"} {
		t.Run(bad, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMaxDiskSizeFlag_InvalidRefusedAtStartup$") // #nosec G204 -- re-runs this test binary
			// No usable kubeconfig: should the value be accepted by mistake,
			// main() fails to find a cluster instead of reaching a real one.
			cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1", managerSubprocessMaxDiskEnv+"="+bad,
				"KUBECONFIG="+t.TempDir()+"/none", "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "the manager must exit: %s", out)
			assert.Equal(t, 1, exitErr.ExitCode())
			assert.Contains(t, string(out), "invalid --max-disk-size")
		})
	}
}
