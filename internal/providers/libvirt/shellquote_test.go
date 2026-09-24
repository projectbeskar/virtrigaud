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

package libvirt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/knownhosts"
)

// injectionPayloads are argument values that, joined unquoted into a remote
// shell command line (the pre-fix behaviour), would run a command, split into
// several words, or be glob/tilde/variable-expanded. Every one must reach the
// remote program as exactly one, byte-identical argument. {{MARK}} is replaced
// with a per-test marker path whose creation proves a command ran.
var injectionPayloads = []string{
	"plain",
	"with space",
	"semi;touch {{MARK}}",
	"subst $(touch {{MARK}})",
	"backtick `touch {{MARK}}`",
	"pipe | touch {{MARK}}",
	"amp & touch {{MARK}}",
	"and && touch {{MARK}}",
	"redirect > {{MARK}}",
	"newline\ntouch {{MARK}}",
	"single'quote; touch {{MARK}}",
	`double"quote; touch {{MARK}}`,
	`backslash\; touch {{MARK}}`,
	"$HOME ${PATH} $0",
	"glob * ? [a-z]",
	"~root",
	"=ls",
	"-n",
	"",
	"unicode é ✓",
}

// withMarker substitutes the marker path into a payload.
func withMarker(p, mark string) string { return strings.ReplaceAll(p, "{{MARK}}", mark) }

// TestShellQuoteArg pins which values pass through verbatim (keeping logged
// command lines readable) and which are quoted.
func TestShellQuoteArg(t *testing.T) {
	verbatim := []string{"virsh", "-c", "qemu:///system", "list", "--all", "/var/lib/libvirt/images/a.qcow2",
		"libvirt-qemu:kvm", "%s", "10G", "a=b", "user@host", "a,b", "+x"}
	for _, s := range verbatim {
		assert.Equal(t, s, shellQuoteArg(s), "%q is shell-inert and must pass through verbatim", s)
	}
	quoted := map[string]string{
		"":            "''",
		"a b":         "'a b'",
		"a;b":         "'a;b'",
		"$(id)":       "'$(id)'",
		"it's":        `'it'\''s'`,
		"=ls":         "'=ls'",
		"~":           "'~'",
		"*":           "'*'",
		`{"a":1}`:     `'{"a":1}'`,
		"line\nbreak": "'line\nbreak'",
	}
	for in, want := range quoted {
		assert.Equal(t, want, shellQuoteArg(in), "shellQuoteArg(%q)", in)
	}
}

// TestShellJoin_RoundTripsThroughRealShell is the core property: whatever an
// argument contains, a POSIX shell parsing shellJoin's output hands the program
// exactly the original argv — and no embedded command ever runs.
func TestShellJoin_RoundTripsThroughRealShell(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "pwned")
	for _, raw := range injectionPayloads {
		arg := withMarker(raw, mark)
		line, err := shellJoin([]string{"printf", "[%s]", arg})
		require.NoError(t, err)

		out, err := exec.Command("/bin/sh", "-c", line).Output() //nolint:gosec // test: proving the quoted line is inert
		require.NoError(t, err, "command line %q", line)
		assert.Equal(t, "["+arg+"]", string(out), "argument %q must round-trip byte-for-byte", arg)
	}
	_, statErr := os.Stat(mark)
	assert.True(t, os.IsNotExist(statErr), "an injected command ran (marker file exists)")
}

// TestShellJoin_RejectsNUL proves a NUL byte (unrepresentable in a C-string
// command line; the remote shell would silently truncate at it) is refused.
func TestShellJoin_RejectsNUL(t *testing.T) {
	_, err := shellJoin([]string{"rm", "-f", "/tmp/a\x00b"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errNULInShellArg))
}

// TestRemoteCommandLine pins the exact command line runVirshCommand sends over
// SSH for both shapes (virsh control command / "!" host command), including
// the -c pinning and its refusal to forward an attacker-shaped URI path.
func TestRemoteCommandLine(t *testing.T) {
	const sys = "qemu+ssh://virt@host-a/system"
	cases := []struct {
		name    string
		uri     string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "plain virsh", uri: sys, args: []string{"list", "--all"},
			want: "virsh -c qemu:///system list --all"},
		{name: "session instance", uri: "qemu+ssh://virt@host-a/session", args: []string{"list"},
			want: "virsh -c qemu:///session list"},
		{name: "hostile domain name is one quoted word", uri: sys, args: []string{"dominfo", "vm;touch /tmp/x"},
			want: "virsh -c qemu:///system dominfo 'vm;touch /tmp/x'"},
		{name: "snapshot description with spaces and quotes", uri: sys,
			args: []string{"snapshot-create-as", "vm", "s1", "--description", `it's "nightly" $(date)`, "--atomic"},
			want: `virsh -c qemu:///system snapshot-create-as vm s1 --description 'it'\''s "nightly" $(date)' --atomic`},
		{name: "hostile endpoint path is not forwarded as -c", uri: "qemu+ssh://virt@host-a/system;id",
			args: []string{"list"}, want: "virsh list"},
		{name: "percent-encoded endpoint path is not forwarded", uri: "qemu+ssh://virt@host-a/system%20-c%20id",
			args: []string{"list"}, want: "virsh list"},
		{name: "host command", uri: sys, args: []string{"!", "rm", "-f", "/tmp/a b"},
			want: "rm -f '/tmp/a b'"},
		{name: "host command with url", uri: sys, args: []string{"!", "curl", "-fsSL", "http://x/i.img?a=1&b=$(id)", "-o", "/tmp/i"},
			want: "curl -fsSL 'http://x/i.img?a=1&b=$(id)' -o /tmp/i"},
		{name: "remote virsh via ! keeps -c quoted", uri: sys,
			args: []string{"!", "virsh", "-c", "qemu:///system", "define", "/tmp/my vm-domain.xml"},
			want: "virsh -c qemu:///system define '/tmp/my vm-domain.xml'"},
		{name: "bare ! is an error", uri: sys, args: []string{"!"}, wantErr: true},
		{name: "NUL is an error", uri: sys, args: []string{"dominfo", "a\x00b"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := remoteCommandLine(tc.uri, tc.args)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// --- end-to-end through the real SSH client + loopback sshd fixture ---------

// installFakeVirsh puts an executable `virsh` on PATH (for the loopback sshd
// fixture, which shells out locally) that prints each argument it receives on
// its own NUL-terminated record, so a test can assert the exact argv virsh saw.
func installFakeVirsh(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "virsh"), []byte(script), 0o700)) //nolint:gosec // test fixture must be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// splitNUL splits the fake virsh's NUL-terminated argv records.
func splitNUL(s string) []string {
	s = strings.TrimSuffix(s, "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// newInjectionTestProvider starts the loopback sshd fixture and returns a
// VirshProvider dialing it (uri path /system).
func newInjectionTestProvider(t *testing.T) *VirshProvider {
	t.Helper()
	hostKey := generateTestHostKey(t)
	addr := startTestSSHServer(t, hostKey, testSSHServerOpts{password: "s3cret"})
	useTempKnownHosts(t, knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n")
	v := testVirshProvider("virtrigaud", addr, &Credentials{Password: "s3cret"})
	t.Cleanup(func() { _ = v.Cleanup() })
	return v
}

// TestRunVirshCommand_SSH_ArgvIsInjectionSafe drives the REAL transport
// (runVirshCommand -> runOverSSH -> remote /bin/sh -c) with every injection
// payload, for all three call shapes — a virsh control command, the "!" host
// command (RunHost), and runRemoteVirshCommand — and asserts the remote program
// received exactly the intended argv and no injected command ran.
func TestRunVirshCommand_SSH_ArgvIsInjectionSafe(t *testing.T) {
	installFakeVirsh(t)
	v := newInjectionTestProvider(t)
	mark := filepath.Join(t.TempDir(), "pwned")
	ctx := context.Background()

	for _, raw := range injectionPayloads {
		arg := withMarker(raw, mark)

		// Virsh control command (e.g. a snapshot description / domain name).
		res, err := v.runVirshCommand(ctx, "snapshot-create-as", "vm", "--description", arg)
		require.NoError(t, err, "payload %q", arg)
		assert.Equal(t, []string{"-c", "qemu:///system", "snapshot-create-as", "vm", "--description", arg},
			splitNUL(res.Stdout), "virsh must receive the payload as one verbatim argument")

		// runRemoteVirshCommand (define / pool-define of a host-local file).
		res, err = v.runRemoteVirshCommand(ctx, "define", arg)
		require.NoError(t, err, "payload %q", arg)
		assert.Equal(t, []string{"-c", "qemu:///system", "define", arg}, splitNUL(res.Stdout))

		// "!" host command (RunHost shape).
		res, err = v.runVirshCommand(ctx, "!", "printf", "[%s]", arg)
		require.NoError(t, err, "payload %q", arg)
		assert.Equal(t, "["+arg+"]", res.Stdout)
	}

	_, statErr := os.Stat(mark)
	assert.True(t, os.IsNotExist(statErr), "an injected command ran on the 'host' (marker file exists)")
}

// TestRunVirshCommand_SSH_HostileEndpointPathNotExecuted reproduces the C1
// primitive end to end: a connection URI whose path carries shell syntax
// (what a malicious Host.spec.endpoint used to reach) must neither run the
// embedded command nor be forwarded as the -c URI.
func TestRunVirshCommand_SSH_HostileEndpointPathNotExecuted(t *testing.T) {
	installFakeVirsh(t)
	v := newInjectionTestProvider(t)
	mark := filepath.Join(t.TempDir(), "pwned")

	u := v.uri // qemu+ssh://virtrigaud@127.0.0.1:port/system
	for _, suffix := range []string{";touch " + mark, "$(touch " + mark + ")", "`touch " + mark + "`"} {
		v.uri = u + suffix
		res, err := v.runVirshCommand(context.Background(), "list", "--all")
		require.NoError(t, err)
		assert.Equal(t, []string{"list", "--all"}, splitNUL(res.Stdout),
			"a hostile URI path must not be forwarded to the remote virsh as -c")
	}
	v.uri = u

	_, statErr := os.Stat(mark)
	assert.True(t, os.IsNotExist(statErr), "the endpoint path was executed by the remote shell")
}

// TestWriteRemoteFile_SSH_ContentAndPathAreInert proves the heredoc
// replacement: content that would have terminated the old heredoc (a line
// equal to a delimiter) or carried shell syntax lands byte-for-byte in the
// file, a path with spaces/quotes is honoured, nothing is executed, and the
// new file is private to the SSH user.
func TestWriteRemoteFile_SSH_ContentAndPathAreInert(t *testing.T) {
	v := newInjectionTestProvider(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "pwned")
	path := filepath.Join(dir, "it's a \"domain\" $(x).xml")
	content := "<domain>\nEOF\nEOF_DOMAIN_1\n$(touch " + mark + ")\n`touch " + mark + "`\n</domain>\n"

	require.NoError(t, v.writeRemoteFile(context.Background(), path, []byte(content)))

	got, err := os.ReadFile(path) //nolint:gosec // test reads the file it just asked the fixture to write
	require.NoError(t, err)
	assert.Equal(t, content, string(got), "content must round-trip byte-for-byte")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a newly written file must be private (umask 077)")

	_, statErr := os.Stat(mark)
	assert.True(t, os.IsNotExist(statErr), "file content was executed")
}

// TestWriteRemoteFile_SSH_ContentNotInCommandLine proves the content (which
// may carry cloud-init secrets) is streamed on stdin and never becomes part of
// the logged / error-embedded command line.
func TestWriteRemoteFile_SSH_ContentNotInCommandLine(t *testing.T) {
	line, err := shellJoin(writePrivateFileArgv("/tmp/virtrigaud-cloudinit/vm/user-data"))
	require.NoError(t, err)
	assert.Equal(t, `sh -c 'umask 077 && cat > "$1"' sh /tmp/virtrigaud-cloudinit/vm/user-data`, line)
}

// TestGuestAgentCommand_SSH_ArgvIsInjectionSafe proves the guest-agent path no
// longer builds a bash script: the domain name and the guest-exec command
// reach virsh as single verbatim arguments, and the JSON payload decodes to
// exactly the requested guest-exec call.
func TestGuestAgentCommand_SSH_ArgvIsInjectionSafe(t *testing.T) {
	installFakeVirsh(t)
	v := newInjectionTestProvider(t)
	mark := filepath.Join(t.TempDir(), "pwned")
	g := NewGuestAgentProvider(v)

	domain := "vm$(touch " + mark + ")"
	guestCmd := `echo "hi"; growpart /dev/vda 1 && touch ` + mark
	res, err := g.agentCommand(context.Background(), domain, guestAgentRequest{
		Execute: qgaExec,
		Arguments: &guestAgentArguments{
			Path:          guestExecShell,
			Arg:           []string{guestExecShellCommand, guestCmd},
			CaptureOutput: true,
		},
	})
	require.NoError(t, err)

	argv := splitNUL(res.Stdout)
	require.Len(t, argv, 7, "virsh -c <uri> qemu-agent-command --timeout <s> <domain> <json>: got %q", argv)
	assert.Equal(t, []string{"-c", "qemu:///system", "qemu-agent-command",
		"--timeout", guestAgentCommandTimeoutSeconds, domain}, argv[:6])

	var req struct {
		Execute   string `json:"execute"`
		Arguments struct {
			Path          string   `json:"path"`
			Arg           []string `json:"arg"`
			CaptureOutput bool     `json:"capture-output"`
		} `json:"arguments"`
	}
	require.NoError(t, json.Unmarshal([]byte(argv[6]), &req))
	assert.Equal(t, qgaExec, req.Execute)
	assert.Equal(t, guestExecShell, req.Arguments.Path)
	assert.Equal(t, []string{"-c", guestCmd}, req.Arguments.Arg)
	assert.True(t, req.Arguments.CaptureOutput)

	_, statErr := os.Stat(mark)
	assert.True(t, os.IsNotExist(statErr), "an injected command ran on the 'host'")

	// A plain request serializes to exactly the historical QMP shape.
	b, err := json.Marshal(guestAgentRequest{Execute: qgaGuestPing})
	require.NoError(t, err)
	assert.Equal(t, `{"execute":"guest-ping"}`, string(b))
	b, err = json.Marshal(guestAgentRequest{Execute: qgaExecStatus, Arguments: &guestAgentArguments{PID: 42}})
	require.NoError(t, err)
	assert.Equal(t, `{"execute":"guest-exec-status","arguments":{"pid":42}}`, string(b))
}
