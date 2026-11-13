//go:build virtrigaud_dev

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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck/hostchecktest"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// enrollEnv is one host's enrolment, as two users would run it, in one
// process: the key directory is private to the test user (the signer's user),
// the state directory's pki directory is the agent's.
type enrollEnv struct {
	pki      *trusttest.PKI
	keyDir   string
	keyFile  string
	stateDir string
	trustDir string
	sockDir  string
	socket   string
	uris     []string
	rec      hostidentity.Record
	probe    *hostchecktest.Fake
	deps     enrollDeps
	workDir  string
}

func newEnrollEnv(t *testing.T) *enrollEnv {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("enrolment refuses to run as root")
	}
	e := &enrollEnv{pki: trusttest.New(t), workDir: t.TempDir()}
	e.keyDir = filepath.Join(e.workDir, "signer")
	require.NoError(t, os.Mkdir(e.keyDir, 0o700))
	e.keyFile = filepath.Join(e.keyDir, "host.key")
	e.stateDir = filepath.Join(e.workDir, "state")
	require.NoError(t, os.Mkdir(e.stateDir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(e.stateDir, config.PKIDirName), 0o700))
	e.trustDir = filepath.Join(e.workDir, "trust")
	e.pki.WriteTrust(t, e.trustDir)
	sock, err := os.MkdirTemp("", "vsg")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	require.NoError(t, os.Chmod(sock, 0o750))
	e.sockDir, e.socket = sock, filepath.Join(sock, "s.sock")
	e.rec = hostidentity.Record{
		EnrolmentID: bytes16(0x30), ProviderUID: trusttest.ProviderUID, InstallID: bytes16(0x50), Mode: hostidentity.ModeIdentity,
	}
	e.uris = hostidentity.RequestURIs(e.pki.HostURI, e.rec)
	hc := hostcheck.Config{StateDir: e.stateDir, PKIDir: filepath.Join(e.stateDir, config.PKIDirName), TrustDir: e.trustDir,
		UID: signer.EUID(), SignerKeyFile: e.keyFile, SignerSocketDir: e.sockDir, SignerUID: signer.EUID()}
	e.probe = hostchecktest.Healthy(hc, e.pki.HostURI)
	e.deps = enrollDeps{probe: e.probe, trustOwner: signer.EUID(), trustDir: e.trustDir, now: time.Now, allowSignerSameUser: true}
	return e
}

func bytes16(b byte) []byte {
	out := make([]byte, hostidentity.IDLen)
	for i := range out {
		out[i] = b + byte(i)
	}
	return out
}

func (e *enrollEnv) csrArgs(out string, extra ...string) []string {
	args := []string{"csr", "--key-file=" + e.keyFile, "--out=" + out,
		"--signer-user=" + strconv.Itoa(os.Getuid()), "--agent-user=" + strconv.Itoa(os.Getuid()+1)}
	for _, u := range e.uris {
		args = append(args, "--uri="+u)
	}
	return append(args, extra...)
}

func (e *enrollEnv) installArgs(csr, cert string, extra ...string) []string {
	args := []string{"install", "--state-dir=" + e.stateDir, "--signer-socket=" + e.socket,
		"--signer-user=" + strconv.Itoa(os.Getuid()), "--signer-key-file=" + e.keyFile,
		"--csr=" + csr, "--certificate=" + cert}
	for _, u := range e.uris {
		args = append(args, "--uri="+u)
	}
	return append(args, extra...)
}

func (e *enrollEnv) run(args []string) (int, string) {
	var stdout, stderr bytes.Buffer
	code := enrollWith(args, logr.Discard(), &stdout, &stderr, e.deps)
	return code, stdout.String() + stderr.String()
}

// serveSigner starts an in-process signer for the key the csr step made.
func (e *enrollEnv) serveSigner(t *testing.T) {
	t.Helper()
	key, err := signer.LoadKey(e.keyFile, signer.EUID())
	require.NoError(t, err)
	srv, err := signer.NewServer(signer.ServerOptions{Key: key, AllowedUID: signer.EUID(), Log: logr.Discard()})
	require.NoError(t, err)
	lis, err := signer.Listen(e.socket)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, lis) }()
	t.Cleanup(func() { cancel(); <-done })
}

// issue is the administrator's side: the manager authorises the request, the
// sub-CA signs it.
func (e *enrollEnv) issue(t *testing.T, csrFile string, o hostidentitytest.IssueOptions) string {
	t.Helper()
	csr, err := os.ReadFile(csrFile)
	require.NoError(t, err)
	_, err = hostidentity.Authorise(hostidentity.Policy{AdminGroup: "admins", HostName: trusttest.HostName, HostURI: e.pki.HostURI, Record: e.rec},
		hostidentity.Request{CSRPEM: csr, Usages: []string{hostidentity.UsageServerAuth}, Duration: hostidentity.MaxLifetime, Groups: []string{"admins"}})
	require.NoError(t, err, "the manager authorises the request the agent wrote")
	out := filepath.Join(e.workDir, "issued-"+strconv.Itoa(len(o.URIs))+".pem")
	require.NoError(t, os.WriteFile(out, e.pki.HostCA.Issue(t, csr, o), 0o600))
	return out
}

func TestEnrollCSRCreatesTheKeyOnceAndWritesTheRequest(t *testing.T) {
	e := newEnrollEnv(t)
	out := filepath.Join(e.workDir, "host.csr")
	code, msg := e.run(e.csrArgs(out))
	require.Equal(t, 0, code, msg)

	fi, err := os.Lstat(e.keyFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	key, err := signer.LoadKey(e.keyFile, signer.EUID())
	require.NoError(t, err)
	csrBytes, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.NotContains(t, string(csrBytes), "PRIVATE KEY")
	csr, err := hostidentity.ParseCSR(csrBytes)
	require.NoError(t, err)
	require.NoError(t, csr.CheckSignature())
	wantPin, err := enrol.KeyPin(key.Public())
	require.NoError(t, err)
	gotPin, err := enrol.KeyPin(csr.PublicKey)
	require.NoError(t, err)
	assert.Equal(t, wantPin, gotPin, "the request is signed with the host key")

	// A second run with a new file keeps the key; an existing file is never
	// overwritten.
	out2 := filepath.Join(e.workDir, "host2.csr")
	code, msg = e.run(e.csrArgs(out2))
	require.Equal(t, 0, code, msg)
	key2, err := signer.LoadKey(e.keyFile, signer.EUID())
	require.NoError(t, err)
	assert.True(t, key2.Equal(key), "the key is never replaced")
	code, _ = e.run(e.csrArgs(out2))
	assert.Equal(t, 1, code, "an existing output file is refused")
	// Standard output carries the request for "-".
	code, msg = e.run(e.csrArgs("-"))
	require.Equal(t, 0, code)
	assert.Contains(t, msg, "BEGIN CERTIFICATE REQUEST")
}

func TestEnrollCSRRefusals(t *testing.T) {
	e := newEnrollEnv(t)
	out := filepath.Join(e.workDir, "host.csr")
	uid := strconv.Itoa(os.Getuid())
	tests := []struct {
		name string
		args []string
		code int
	}{
		{"no output", []string{"csr", "--key-file=" + e.keyFile}, 2},
		{"relative key file", []string{"csr", "--key-file=host.key", "--out=" + out}, 2},
		{"no uris", []string{"csr", "--key-file=" + e.keyFile, "--out=" + out, "--signer-user=" + uid, "--agent-user=" + strconv.Itoa(os.Getuid()+1)}, 2},
		{"uris without the record", append(e.csrArgs(out)[:5:5], "--uri="+e.pki.HostURI), 2},
		{"the agent is the signer's user", e.csrArgs(out, "--agent-user="+uid), 1},
		{"not the signer's user", e.csrArgs(out, "--signer-user="+strconv.Itoa(os.Getuid()+1)), 1},
		{"unknown flag", []string{"csr", "--bogus"}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := e.run(tc.args)
			assert.Equal(t, tc.code, code, msg)
			_, err := os.Lstat(e.keyFile)
			assert.True(t, os.IsNotExist(err), "a refused step makes no key")
			_, err = os.Lstat(out)
			assert.True(t, os.IsNotExist(err), "a refused step writes no request")
		})
	}
}

func TestEnrollCSRReadsURIsFromAFile(t *testing.T) {
	e := newEnrollEnv(t)
	list := filepath.Join(e.workDir, "uris")
	require.NoError(t, os.WriteFile(list, []byte("# the manager's record\n"+strings.Join(e.uris, "\n")+"\n\n"), 0o600))
	out := filepath.Join(e.workDir, "host.csr")
	code, msg := e.run([]string{"csr", "--key-file=" + e.keyFile, "--out=" + out, "--uris-file=" + list,
		"--signer-user=" + strconv.Itoa(os.Getuid()), "--agent-user=" + strconv.Itoa(os.Getuid()+1)})
	require.Equal(t, 0, code, msg)
}

// TestEnrollOfflineFlow is the whole offline flow: the signer's user makes
// the key and the request, an administrator has the request approved and
// signed with files only, the agent's user installs the certificate against
// the running signer, and the agent derives the same enrolment record from
// the installed file.
func TestEnrollOfflineFlow(t *testing.T) {
	e := newEnrollEnv(t)
	csr := filepath.Join(e.workDir, "host.csr")
	code, msg := e.run(e.csrArgs(csr))
	require.Equal(t, 0, code, msg)
	e.serveSigner(t)
	cert := e.issue(t, csr, hostidentitytest.IssueOptions{})

	code, msg = e.run(e.installArgs(csr, cert))
	require.Equal(t, 0, code, msg)
	assert.Contains(t, msg, e.pki.HostURI)

	pkiDir := filepath.Join(e.stateDir, config.PKIDirName)
	fi, err := os.Lstat(filepath.Join(pkiDir, trust.FileHostCert))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	_, err = os.Lstat(filepath.Join(pkiDir, trust.FileHostKey))
	assert.True(t, os.IsNotExist(err), "no key in the agent's directory")

	tc, err := trust.LoadConfig(e.trustDir, trust.TrustFileReader(signer.EUID()))
	require.NoError(t, err)
	sg, err := signer.Connect(context.Background(), signer.ClientOptions{Socket: e.socket, ServerUID: signer.EUID()})
	require.NoError(t, err)
	id, err := trust.LoadIdentity(pkiDir, tc, time.Now(), trust.KeyFileReader(signer.EUID()), sg)
	require.NoError(t, err)
	assert.True(t, id.Record.Equal(e.rec), "the record is derived from the certificate")

	code, _ = e.run(e.installArgs(csr, cert))
	assert.Equal(t, 1, code, "an installed certificate is not replaced without --replace")
}

func TestEnrollInstallRefusals(t *testing.T) {
	e := newEnrollEnv(t)
	csr := filepath.Join(e.workDir, "host.csr")
	code, msg := e.run(e.csrArgs(csr))
	require.Equal(t, 0, code, msg)
	cert := e.issue(t, csr, hostidentitytest.IssueOptions{})
	crt := filepath.Join(e.stateDir, config.PKIDirName, trust.FileHostCert)

	// The signer is not running yet.
	code, msg = e.run(e.installArgs(csr, cert))
	assert.Equal(t, 1, code, msg)
	_, err := os.Lstat(crt)
	assert.True(t, os.IsNotExist(err))

	e.serveSigner(t)
	// The agent's user could read the key: refused before anything else.
	e.probe.Edit(func(f *hostchecktest.Fake) {
		delete(f.DeniedPaths, e.keyFile)
		f.ReadablePaths[e.keyFile] = true
	})
	code, msg = e.run(e.installArgs(csr, cert))
	assert.Equal(t, 1, code, msg)
	_, err = os.Lstat(crt)
	assert.True(t, os.IsNotExist(err), "a host where the agent can read the key installs nothing")
	e.probe.Edit(func(f *hostchecktest.Fake) {
		delete(f.ReadablePaths, e.keyFile)
		f.DeniedPaths[e.keyFile] = true
	})

	// Other refusals.
	other := hostidentitytest.NewCA(t)
	csrBytes, err := os.ReadFile(csr)
	require.NoError(t, err)
	foreign := filepath.Join(e.workDir, "foreign.pem")
	require.NoError(t, os.WriteFile(foreign, other.Issue(t, csrBytes, hostidentitytest.IssueOptions{}), 0o600))
	for name, args := range map[string][]string{
		"a certificate of another CA": e.installArgs(csr, foreign),
		"no certificate flag":         {"install", "--state-dir=" + e.stateDir, "--csr=" + csr},
		"no current request URIs":     {"install", "--state-dir=" + e.stateDir, "--csr=" + csr, "--certificate=" + cert},
		"a stale request":             append(e.installArgs(csr, cert)[:7:7], "--uri="+e.pki.HostURI),
		"a missing file":              e.installArgs(csr, filepath.Join(e.workDir, "none.pem")),
		"a root signer user":          append(e.installArgs(csr, cert), "--signer-user=0"),
	} {
		t.Run(name, func(t *testing.T) {
			code, msg := e.run(args)
			assert.NotZero(t, code, msg)
			_, err := os.Lstat(crt)
			assert.True(t, os.IsNotExist(err))
		})
	}
	code, msg = e.run(e.installArgs(csr, cert))
	require.Equal(t, 0, code, msg)
}

func TestEnrollUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	assert.Equal(t, 2, enrollWith(nil, logr.Discard(), &stdout, &stderr, enrollDeps{}))
	assert.Contains(t, stderr.String(), "usage: vrtg-agent enroll")
	assert.Equal(t, 2, enrollWith([]string{"nope"}, logr.Discard(), &stdout, &stderr, enrollDeps{}))
	assert.Equal(t, 0, enrollWith([]string{"help"}, logr.Discard(), &stdout, &stderr, enrollDeps{}))
}
