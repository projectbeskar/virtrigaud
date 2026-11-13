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
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
)

const enrollUsage = `usage: vrtg-agent enroll <step> [flags]

steps:
  csr      run as the signer's user: create the host key if there is none and
           write the certificate signing request (give it the Host's
           status.agent.requestURIs)
  install  run as the agent's user: install the certificate that was issued
           for the request, after checking it
`

// signerConnectTimeout bounds how long enroll install waits for the signer.
const signerConnectTimeout = 10 * time.Second

// maxEnrolFileBytes bounds the request and certificate files.
const maxEnrolFileBytes = 64 << 10

// enrollDeps are the parts of host enrolment a test replaces. The zero value
// is the real host (see realEnrollDeps).
type enrollDeps struct {
	// probe looks at the host (hostcheck.System in production).
	probe hostcheck.Probe
	// trustOwner is the owner the trust configuration's files must have
	// (root in production).
	trustOwner uint32
	// trustDir replaces the configured trust directory when set (tests).
	trustDir string
	// now is the clock.
	now func() time.Time
	// allowSignerSameUser lets the signer run as the agent's user (tests).
	allowSignerSameUser bool
}

// realEnrollDeps returns the production dependencies.
func realEnrollDeps() enrollDeps {
	return enrollDeps{probe: hostcheck.System{}, now: time.Now}
}

// enrollCommand runs a host enrolment step: csr (as the signer's user) or
// install (as the agent's user). See package enrol for why the work is split
// between two users. Exit status 0 on success, 1 when a step is refused or
// fails, 2 for bad usage.
func enrollCommand(args []string, log logr.Logger, stdout, stderr io.Writer) int {
	return enrollWith(args, log, stdout, stderr, realEnrollDeps())
}

// enrollWith is enrollCommand with its dependencies.
func enrollWith(args []string, log logr.Logger, stdout, stderr io.Writer, deps enrollDeps) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, enrollUsage)
		return 2
	}
	switch args[0] {
	case "csr":
		return enrollCSR(args[1:], log, stdout, stderr)
	case "install":
		return enrollInstall(args[1:], log, stdout, stderr, deps)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, enrollUsage)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "unknown enroll step %q\n%s", args[0], enrollUsage)
	return 2
}

// uriList is a repeatable flag.
type uriList []string

// String implements flag.Value.
func (u *uriList) String() string { return strings.Join(*u, ",") }

// Set implements flag.Value.
func (u *uriList) Set(v string) error {
	*u = append(*u, v)
	return nil
}

// readLines reads a file of one URI per line with read (blank lines and lines
// that start with '#' are ignored).
func readLines(path string, read func(string) ([]byte, error)) ([]string, error) {
	b, err := read(path)
	if err != nil {
		return nil, err
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

// enrollCSR creates the host key when there is none and writes the
// certificate signing request. It must run as the signer's own user, the only
// user that owns the key file; it refuses to run as root or as the agent's
// user.
func enrollCSR(args []string, log logr.Logger, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("enroll csr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyFile := fs.String("key-file", config.DefaultSignerKeyFile, "the host private key file: created when absent (mode 0600, in a directory private to the signer's user), never replaced")
	signerUser := fs.String("signer-user", config.DefaultSignerUser, "the signer's user (name or uid); this command must run as it")
	agentUser := fs.String("agent-user", defaultSignerAgentUser, "the agent's user (name or uid); never the signer's user")
	out := fs.String("out", "", "where to write the request (a new file; - for standard output)")
	urisFile := fs.String("uris-file", "", "a file with the URIs, one per line (the Host's status.agent.requestURIs)")
	var uris uriList
	fs.Var(&uris, "uri", "a URI the request must carry (repeatable): the Host's status.agent.requestURIs")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" || !filepath.IsAbs(*keyFile) || filepath.Clean(*keyFile) != *keyFile {
		log.Error(errors.New("--out is required and --key-file must be a clean absolute path"), "invalid configuration")
		return 2
	}
	if *urisFile != "" {
		more, err := readLines(*urisFile, trust.HandoffFileReader(signer.EUID()))
		if err != nil {
			log.Error(err, "read --uris-file")
			return 2
		}
		uris = append(uris, more...)
	}
	own := signer.EUID()
	if own == 0 {
		log.Error(errors.New("running as root"), "refused; run this step as the signer's user: the key must belong to it")
		return 1
	}
	signerUID, err := signer.ResolveUID(*signerUser)
	if err != nil {
		log.Error(err, "signer user")
		return 1
	}
	agentUID, err := signer.ResolveUID(*agentUser)
	if err != nil {
		log.Error(err, "agent user")
		return 1
	}
	if own != signerUID || agentUID == own {
		log.Error(errors.New("wrong user"), "refused; run this step as the signer's user, which must differ from the agent's user")
		return 1
	}
	// Validate the URIs before a key is created for them.
	if _, _, err := enrol.ParseURIs(uris); err != nil {
		log.Error(err, "the request URIs")
		return 2
	}
	key, created, err := signer.CreateKey(*keyFile, own)
	if err != nil {
		log.Error(err, "host key")
		return 1
	}
	csr, err := enrol.BuildCSR(key, uris)
	if err != nil {
		log.Error(err, "build the request")
		return 1
	}
	if err := writeCSR(*out, csr, stdout); err != nil {
		log.Error(err, "write the request")
		return 1
	}
	pin, err := enrol.KeyPin(key.Public())
	if err != nil {
		log.Error(err, "key pin")
		return 1
	}
	log.Info("request written", "createdKey", created, "keyPin", pin)
	return 0
}

// writeCSR writes the request to a new file (mode 0644: it is public), or to
// stdout for "-".
func writeCSR(path string, csr []byte, stdout io.Writer) error {
	if path == "-" {
		_, err := stdout.Write(csr)
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // #nosec G302,G304 -- the request is public; O_EXCL never replaces a file or follows a link
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(csr); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// enrollInstall checks and installs the issued certificate. It runs as the
// agent's user, never root, and asks the running signer for its public key:
// the key file is not read, and the step refuses to run when the agent's user
// could read it.
func enrollInstall(args []string, log logr.Logger, stdout, stderr io.Writer, deps enrollDeps) int {
	fs := flag.NewFlagSet("enroll install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := config.Register(fs)
	csrFile := fs.String("csr", "", "the request file that was submitted (written by enroll csr)")
	certFile := fs.String("certificate", "", "the issued certificate, PEM, host certificate first")
	replace := fs.Bool("replace", false, "replace an installed certificate (a re-enrolment: the certificate must name another enrolment)")
	urisFile := fs.String("uris-file", "", "a file with the Host's current request URIs, one per line (status.agent.requestURIs)")
	var uris uriList
	fs.Var(&uris, "uri", "a current request URI of the Host (repeatable): its status.agent.requestURIs; the request file must carry exactly these")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *urisFile != "" {
		more, err := readLines(*urisFile, trust.HandoffFileReader(deps.probe.EUID()))
		if err != nil {
			log.Error(err, "read --uris-file")
			return 2
		}
		uris = append(uris, more...)
	}
	if err := cfg.Validate(); err != nil {
		log.Error(err, "invalid configuration")
		return 2
	}
	if *csrFile == "" || *certFile == "" || len(uris) == 0 {
		log.Error(errors.New("--csr, --certificate and the Host's current request URIs (--uri or --uris-file) are required"), "invalid configuration")
		return 2
	}
	res, err := installCertificate(cfg, *csrFile, *certFile, uris, *replace, deps)
	if err != nil {
		log.Error(err, "the certificate was not installed")
		return 1
	}
	log.Info("host certificate installed", "enrolment", hex.EncodeToString(res.Record.EnrolmentID), "keyPin", res.KeyPin,
		"notAfter", res.NotAfter.UTC().Format(time.RFC3339), "replaced", res.Replaced)
	_, _ = fmt.Fprintf(stdout, "installed the host certificate of %s (enrolment %s, valid until %s)\n",
		res.HostURI, hex.EncodeToString(res.Record.EnrolmentID), res.NotAfter.UTC().Format(time.RFC3339))
	_, _ = fmt.Fprintln(stdout, "start vrtg-agent, or reload a running one (SIGHUP): the agent derives its enrolment from this certificate")
	return 0
}

// installCertificate runs the host checks, loads what enrol.Install needs
// and installs.
func installCertificate(cfg *config.Config, csrFile, certFile string, uris []string, replace bool, deps enrollDeps) (enrol.Result, error) {
	probe := deps.probe
	uid := probe.EUID()
	if err := hostcheck.CheckNotRoot(probe); err != nil {
		return enrol.Result{}, err
	}
	signerUID, err := signer.ResolveUID(cfg.SignerUser)
	if err != nil {
		return enrol.Result{}, fmt.Errorf("signer user: %w", err)
	}
	if signerUID == 0 || (signerUID == uid && !deps.allowSignerSameUser) {
		return enrol.Result{}, fmt.Errorf("the signer's user (uid %d) must be neither root nor the user running this step", signerUID)
	}
	trustDir := deps.trustDir
	if trustDir == "" {
		if trustDir, err = cfg.TrustDir(); err != nil {
			return enrol.Result{}, err
		}
	}
	if err := hostcheck.CheckPKI(probe, cfg.PKIDir(), uid); err != nil {
		return enrol.Result{}, err
	}
	// The agent's user must be refused the host key: the key is the signer's.
	if err := hostcheck.CheckHostKeyUnreadable(probe, cfg.PKIDir(), cfg.SignerKeyFile); err != nil {
		return enrol.Result{}, err
	}
	if err := hostcheck.CheckTrustDir(probe, trustDir); err != nil {
		return enrol.Result{}, err
	}
	csr, err := readEnrolFile(csrFile, uid)
	if err != nil {
		return enrol.Result{}, fmt.Errorf("read --csr: %w", err)
	}
	cert, err := readEnrolFile(certFile, uid)
	if err != nil {
		return enrol.Result{}, fmt.Errorf("read --certificate: %w", err)
	}
	tc, err := trust.LoadConfig(trustDir, trust.TrustFileReader(deps.trustOwner))
	if err != nil {
		return enrol.Result{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), signerConnectTimeout)
	defer cancel()
	sg, err := signer.Connect(ctx, signer.ClientOptions{Socket: cfg.SignerSocket, ServerUID: signerUID, Lifetime: context.Background()})
	if err != nil {
		return enrol.Result{}, fmt.Errorf("reach the signer at %s (start it after enroll csr has made the key): %w", cfg.SignerSocket, err)
	}
	return enrol.Install(enrol.InstallOptions{
		PKIDir: cfg.PKIDir(), UID: uid, CSR: csr, RequestURIs: uris, Certificate: cert, Trust: tc, Signer: sg, Now: deps.now(), Replace: replace,
	})
}

// readEnrolFile reads a file handed over by an administrator (see
// trust.HandoffFileReader: a regular file owned by root or uid, not writable
// by group or others, not reached through a symbolic link) of at most
// maxEnrolFileBytes.
func readEnrolFile(path string, uid uint32) ([]byte, error) {
	b, err := trust.HandoffFileReader(uid)(path)
	if err != nil {
		return nil, err
	}
	if len(b) > maxEnrolFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxEnrolFileBytes)
	}
	return b, nil
}
