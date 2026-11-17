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
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
)

// labT adapts the test PKI helpers, which take a testing.TB, to a command: a
// failure ends the process. The embedded testing.TB is nil; the helpers call
// only the methods defined here.
type labT struct{ testing.TB }

// Helper implements testing.TB.
func (labT) Helper() {}

// Fatal implements testing.TB: it reports and ends the process.
func (labT) Fatal(args ...any) {
	_, _ = fmt.Fprintln(os.Stderr, args...)
	os.Exit(1)
}

// Fatalf implements testing.TB: it reports and ends the process.
func (labT) Fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// account is a local user the agent or the signer runs as.
type account struct {
	Name string
	UID  uint32
	GID  uint32
}

// lookupAccount resolves a user name to its uid and primary gid.
func lookupAccount(name string) (account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return account{}, fmt.Errorf("look up user %s: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return account{}, fmt.Errorf("parse uid of %s: %w", name, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return account{}, fmt.Errorf("parse gid of %s: %w", name, err)
	}
	return account{Name: name, UID: uint32(uid), GID: uint32(gid)}, nil
}

// credential is the process credential of a.
func (a account) credential() *syscall.Credential {
	return &syscall.Credential{Uid: a.UID, Gid: a.GID}
}

// labDirs is the directory layout of one lab host.
type labDirs struct {
	// State is the agent's state directory (on the disk under test).
	State string
	// Work holds the driver's logs, results and the effects directory.
	Work string
	// Trust is the root-owned trust configuration directory.
	Trust string
	// SignerKeyDir holds the signer's private key file.
	SignerKeyDir string
	// SocketDir holds the signer's socket.
	SocketDir string
	// DevEnrolment is the development enrolment file.
	DevEnrolment string
}

func (d labDirs) effects() string      { return filepath.Join(d.Work, "effects") }
func (d labDirs) signerKey() string    { return filepath.Join(d.SignerKeyDir, "host.key") }
func (d labDirs) signerSocket() string { return filepath.Join(d.SocketDir, "signer.sock") }
func (d labDirs) fakeExecutor() string { return filepath.Join(d.State, config.FakeExecutorDirName) }
func (d labDirs) agentLog(n int) string {
	return filepath.Join(d.Work, "logs", fmt.Sprintf("agent-%05d.log", n))
}

// lab is a host prepared for the agent: its users, its PKI and its files.
type lab struct {
	Dirs   labDirs
	Agent  account
	Signer account
	PKI    *trusttest.PKI
}

// prepareLab wipes the lab directories and builds a fresh host: a PKI, the
// trust configuration, the provisioning marker, the host certificate, the
// signer's key and socket directory, and the fake executor's directory (a
// link from the state directory to a directory outside it, so that nothing
// that happens to the state erases the effect log). It must run as root.
func prepareLab(d labDirs, agent, signer account) (*lab, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("the lab is prepared as root, inside a disposable host")
	}
	for _, p := range []string{d.State, d.Work, d.Trust, d.SignerKeyDir, d.SocketDir} {
		if err := os.RemoveAll(p); err != nil {
			return nil, fmt.Errorf("remove %s: %w", p, err)
		}
	}
	_ = os.Remove(d.DevEnrolment)
	pki := trusttest.New(labT{})
	mk := func(path string, mode os.FileMode, uid, gid uint32) error {
		if err := os.MkdirAll(path, mode); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
		return os.Chown(path, int(uid), int(gid))
	}
	if err := mk(d.State, 0o700, agent.UID, agent.GID); err != nil {
		return nil, err
	}
	for _, sub := range []string{"logs", "effects"} {
		if err := mk(filepath.Join(d.Work, sub), 0o755, 0, 0); err != nil {
			return nil, err
		}
	}
	if err := mk(d.effects(), 0o700, agent.UID, agent.GID); err != nil {
		return nil, err
	}
	// The trust configuration: root-owned, not writable by the agent.
	pki.WriteTrust(labT{}, d.Trust)
	if err := os.Chmod(d.Trust, 0o755); err != nil {
		return nil, fmt.Errorf("chmod trust directory: %w", err)
	}
	// The agent's key material directory: only the host certificate.
	pkiDir := filepath.Join(d.State, config.PKIDirName)
	pki.WritePKI(labT{}, pkiDir)
	for _, p := range []string{pkiDir, filepath.Join(pkiDir, "host.crt")} {
		if err := os.Chown(p, int(agent.UID), int(agent.GID)); err != nil {
			return nil, fmt.Errorf("chown %s: %w", p, err)
		}
	}
	marker := filepath.Join(d.State, hostcheck.HostMarkerFile)
	if err := os.WriteFile(marker, []byte(pki.HostURI+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write provisioning marker: %w", err)
	}
	if err := os.Chown(marker, int(agent.UID), int(agent.GID)); err != nil {
		return nil, fmt.Errorf("chown provisioning marker: %w", err)
	}
	// The signer's key and socket directory.
	if err := mk(d.SignerKeyDir, 0o700, signer.UID, signer.GID); err != nil {
		return nil, err
	}
	keyPath := pki.WriteKey(labT{}, d.SignerKeyDir)
	if err := os.Chown(keyPath, int(signer.UID), int(signer.GID)); err != nil {
		return nil, fmt.Errorf("chown signer key: %w", err)
	}
	if err := mk(d.SocketDir, 0o750|os.ModeSetgid, signer.UID, agent.GID); err != nil {
		return nil, err
	}
	// The fake executor's directory lives outside the state directory.
	if err := os.Symlink(d.effects(), d.fakeExecutor()); err != nil {
		return nil, fmt.Errorf("link the fake executor directory: %w", err)
	}
	if err := os.Lchown(d.fakeExecutor(), int(agent.UID), int(agent.GID)); err != nil {
		return nil, fmt.Errorf("chown the fake executor link: %w", err)
	}
	if err := os.WriteFile(d.DevEnrolment, []byte(`{"startEpoch": 2}`+"\n"), 0o644); err != nil { // #nosec G306 -- root-owned configuration the agent only reads
		return nil, fmt.Errorf("write development enrolment file: %w", err)
	}
	return &lab{Dirs: d, Agent: agent, Signer: signer, PKI: pki}, nil
}

// startSigner starts the signer process and waits for its socket.
func (l *lab) startSigner(bin string) (*exec.Cmd, error) {
	logf, err := os.OpenFile(filepath.Join(l.Dirs.Work, "logs", "signer.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) // #nosec G304 -- a path under the lab directory
	if err != nil {
		return nil, fmt.Errorf("open signer log: %w", err)
	}
	// A socket left by a signer that was killed would end the wait at once.
	_ = os.Remove(l.Dirs.signerSocket())
	cmd := exec.Command(bin, "signer", "--socket", l.Dirs.signerSocket(), "--key-file", l.Dirs.signerKey(), "--agent-user", l.Agent.Name) // #nosec G204 -- the lab's own binary
	cmd.Stderr, cmd.Stdout = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: l.Signer.credential()}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start signer: %w", err)
	}
	for i := 0; i < 200; i++ {
		if fi, err := os.Lstat(l.Dirs.signerSocket()); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return cmd, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return nil, errors.New("the signer's socket did not appear")
}

// tlsConfig is the client side of the agent's mutual TLS: a Provider
// certificate of the Provider identity issuer, and the agent's certificate
// verified against the host identity issuer (it carries a URI identity, no
// host name).
func (l *lab) tlsConfig() *tls.Config {
	cert := l.PKI.ProviderCert(labT{}, nil, trusttest.ProviderURI(trusttest.ProviderUID))
	roots := l.PKI.HostCAPool()
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // #nosec G402 -- verified below against the host identity issuer
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return fmt.Errorf("parse server certificate: %w", err)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
				return fmt.Errorf("verify server certificate: %w", err)
			}
			return nil
		},
	}
}
