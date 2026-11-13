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
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
)

// helperEnv makes the test binary act as vrtg-agent.
const helperEnv = "VRTG_AGENT_TEST_AS_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// agentCmd returns a command that runs vrtg-agent with args.
func agentCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...) // #nosec G204 -- this test binary
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	return cmd
}

func writeKey(t *testing.T, dir string) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "host.key")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSignerCommandRefusals: bad flags exit 2, a start that is refused exits
// 1, and nothing is served in either case.
func TestSignerCommandRefusals(t *testing.T) {
	dir, err := os.MkdirTemp("", "vsg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	key := writeKey(t, t.TempDir())
	sock := filepath.Join(dir, "s.sock")
	other := strconv.Itoa(os.Getuid() + 1)
	own := strconv.Itoa(os.Getuid())
	for name, tc := range map[string]struct {
		args []string
		code int
	}{
		"relative socket":          {[]string{"signer", "--socket=s.sock"}, 2},
		"relative key file":        {[]string{"signer", "--key-file=host.key"}, 2},
		"no such flag":             {[]string{"signer", "--bogus"}, 2},
		"zero rate":                {[]string{"signer", "--max-signs-per-second=0"}, 2},
		"agent is the signer user": {[]string{"signer", "--socket=" + sock, "--key-file=" + key, "--agent-user=" + own}, 1},
		"unknown agent user":       {[]string{"signer", "--socket=" + sock, "--key-file=" + key, "--agent-user=no-such-user-virtrigaud"}, 1},
		"missing key":              {[]string{"signer", "--socket=" + sock, "--key-file=" + key + ".none", "--agent-user=" + other}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := agentCmd(tc.args...).CombinedOutput()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if os.Geteuid() == 0 && tc.code == 1 {
				return // root is refused first, with the same status
			}
			if code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", code, tc.code, out)
			}
			if _, err := os.Lstat(sock); err == nil {
				t.Fatal("a refused start left a socket")
			}
		})
	}
}

// TestSignerCommandServesAndStops: the subcommand serves its socket, refuses
// a peer that is not the agent's user, and exits 0 on SIGTERM.
func TestSignerCommandServesAndStops(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the signer refuses to run as root")
	}
	dir, err := os.MkdirTemp("", "vsg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	var out bytes.Buffer
	cmd := agentCmd("signer", "--socket="+sock, "--key-file="+writeKey(t, t.TempDir()),
		"--agent-user="+strconv.Itoa(os.Getuid()+1))
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// The signer is up once its socket answers; this test's user is not the
	// agent's user, so the signer closes the connection without a response.
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, cerr := signer.Connect(ctx, signer.ClientOptions{Socket: sock, ServerUID: signer.EUID()})
		cancel()
		if _, serr := os.Lstat(sock); serr == nil && cerr != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the signer did not serve\n%s", out.String())
		}
		select {
		case err := <-exited:
			t.Fatalf("the signer exited early: %v\n%s", err, out.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v\n%s", err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the signer did not stop\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("signer stopped")) {
		t.Fatalf("no stop line in the log:\n%s", out.String())
	}
}
