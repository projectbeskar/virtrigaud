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

package enrol

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// ErrInstall is wrapped by every error that makes Install refuse a
// certificate. Nothing is written when it is returned.
var ErrInstall = errors.New("host certificate refused")

// ErrExists is wrapped by the refusal to replace an installed certificate
// without InstallOptions.Replace (or with the same enrolment).
var ErrExists = errors.New("a host certificate is already installed")

// InstallOptions is what Install needs.
type InstallOptions struct {
	// PKIDir is the agent's key material directory; it holds host.crt and is
	// owned by UID.
	PKIDir string
	// UID is the agent's user id: the owner of PKIDir's files.
	UID uint32
	// CSR is the PEM certificate signing request that was submitted (the file
	// vrtg-agent enroll csr wrote). It names the host URI and the enrolment
	// record the certificate must carry, and the key it must certify.
	CSR []byte
	// Certificate is the issued certificate, PEM, the host certificate first.
	// Anything after the first certificate is ignored: the host certificate
	// must chain to the pinned issuers directly, as the agent loads it.
	Certificate []byte
	// Trust is the trust configuration: its host identity issuers are the
	// pinned sub-CA the certificate must chain to.
	Trust *trust.Config
	// Signer is the host key's signer, the client of the running signer
	// process. Only its public half is used: it names the key the
	// certificate must certify, and the agent never holds the private half.
	Signer crypto.Signer
	// Now is the time the certificate must be valid at.
	Now time.Time
	// Replace allows replacing an installed certificate (a re-enrolment: the
	// new certificate must name another enrolment).
	Replace bool
}

// Result describes an installed certificate.
type Result struct {
	// HostURI is the host's URI identity.
	HostURI string
	// Record is the enrolment record derived from the certificate's URI
	// subject alternative names.
	Record hostidentity.Record
	// KeyPin is the pin of the certified key.
	KeyPin string
	// NotAfter is when the certificate expires.
	NotAfter time.Time
	// Replaced reports that an earlier certificate was replaced.
	Replaced bool
}

// Install checks an issued certificate against everything the agent knows
// and, only when all of it holds, installs it as host.crt in PKIDir (mode
// 0600, written to a temporary file, synced and renamed, so a crash leaves
// the old or the new file, never a partial one). The checks:
//
//   - the request file is one PEM request whose signature verifies, and its
//     URI subject alternative names are exactly the host URI identity and an
//     enrolment record (ParseURIs);
//   - the signer's key is ECDSA P-256 and is the request's key;
//   - the certificate chains directly to a pinned host identity issuer for
//     server authentication at Now, is not a CA, has server authentication as
//     its only usage, lives at most hostidentity.MaxLifetime, has an empty
//     subject or the Host's name, carries no DNS, address or e-mail name,
//     carries exactly the request's URIs (so the enrolment record the manager
//     drew), and certifies the signer's key (hostidentity.ValidateIssued);
//   - the daemon's own loader (trust.LoadIdentity) accepts the installed
//     file; if it does not, the previous file is put back.
//
// The enrolment record is not stored anywhere else: the daemon derives it
// from the installed certificate at start and on reload.
func Install(o InstallOptions) (Result, error) {
	if o.Trust == nil || o.Trust.HostCAPool == nil || o.Signer == nil || o.PKIDir == "" {
		return Result{}, fmt.Errorf("%w: incomplete options", ErrInstall)
	}
	csr, err := hostidentity.ParseCSR(o.CSR)
	if err != nil {
		return Result{}, fmt.Errorf("%w: the request file: %v", ErrInstall, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return Result{}, fmt.Errorf("%w: the request file's signature does not verify", ErrInstall)
	}
	uris := make([]string, 0, len(csr.URIs))
	for _, u := range csr.URIs {
		uris = append(uris, u.String())
	}
	parsed, hostURI, err := ParseURIs(uris)
	if err != nil {
		return Result{}, fmt.Errorf("%w: the request file: %v", ErrInstall, err)
	}
	rec, err := hostidentity.RecordFromURIs(parsed, hostURI)
	if err != nil {
		return Result{}, fmt.Errorf("%w: the request file: %v", ErrInstall, err)
	}
	name, err := HostName(hostURI)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInstall, err)
	}
	pub, ok := o.Signer.Public().(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return Result{}, fmt.Errorf("%w: the signer's key is not ECDSA P-256", ErrInstall)
	}
	signerPin, err := hostidentity.SPKIPin(pub)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInstall, err)
	}
	csrPin, err := hostidentity.SPKIPin(csr.PublicKey)
	if err != nil {
		return Result{}, fmt.Errorf("%w: the request file: %v", ErrInstall, err)
	}
	if csrPin != signerPin {
		return Result{}, fmt.Errorf("%w: the request file was made with a key other than the signer's", ErrInstall)
	}
	certs, err := hostidentity.ParseCertificates(o.Certificate)
	if err != nil {
		return Result{}, fmt.Errorf("%w: the certificate file: %v", ErrInstall, err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certs[0].Raw})
	issued, err := hostidentity.ValidateIssued(hostidentity.Authorisation{
		HostName: name, HostURI: hostURI, Record: rec, KeyPin: signerPin,
	}, leafPEM, o.Trust.HostCAPool, o.Now)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInstall, err)
	}
	return install(o, leafPEM, issued, hostURI)
}

// install writes leafPEM as host.crt and confirms it with the daemon's
// loader, restoring the previous file when the loader refuses it.
func install(o InstallOptions, leafPEM []byte, issued hostidentity.Issued, hostURI string) (Result, error) {
	path := filepath.Join(o.PKIDir, trust.FileHostCert)
	old, hadOld, err := readInstalled(path, o.UID)
	if err != nil {
		return Result{}, err
	}
	if hadOld {
		if !o.Replace {
			return Result{}, fmt.Errorf("%w: %s exists; to replace it with a new enrolment, run again with --replace", ErrExists, path)
		}
		if prev, ok := installedRecord(old, hostURI); ok && prev.Equal(issued.Record) {
			return Result{}, fmt.Errorf("%w: it names the enrolment that is already installed", ErrExists)
		}
	}
	if err := writeAtomic(path, leafPEM); err != nil {
		return Result{}, err
	}
	id, err := trust.LoadIdentity(o.PKIDir, o.Trust, o.Now, trust.KeyFileReader(o.UID), o.Signer)
	if err != nil {
		rerr := restore(path, old, hadOld)
		return Result{}, errors.Join(fmt.Errorf("%w: the agent would not load it: %v", ErrInstall, err), rerr)
	}
	return Result{HostURI: id.HostURI, Record: id.Record, KeyPin: issued.KeyPin, NotAfter: id.Leaf.NotAfter, Replaced: hadOld}, nil
}

// readInstalled reads the installed certificate file, if there is one. A
// file the agent cannot read is an error, not "none": it must not be
// overwritten blindly.
func readInstalled(path string, uid uint32) ([]byte, bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%w: stat %s: %v", ErrInstall, path, err)
	}
	b, err := trust.KeyFileReader(uid)(path)
	if err != nil {
		// Present but not a file the agent accepts: replacing it is the
		// administrator's decision (--replace), the content is not needed.
		return nil, true, nil
	}
	return b, true, nil
}

// installedRecord returns the enrolment record of an installed host.crt, if it
// is one certificate that carries a record for hostURI.
func installedRecord(b []byte, hostURI string) (hostidentity.Record, bool) {
	certs, err := hostidentity.ParseCertificates(b)
	if err != nil || len(certs) != 1 {
		return hostidentity.Record{}, false
	}
	rec, err := hostidentity.RecordFromURIs(certs[0].URIs, hostURI)
	return rec, err == nil
}

// restore puts the previous host certificate back, or removes the new one.
func restore(path string, old []byte, hadOld bool) error {
	if hadOld && len(old) > 0 {
		return writeAtomic(path, old)
	}
	if hadOld {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the refused certificate: %w", err)
	}
	return nil
}

// writeAtomic writes data to path through a private temporary file in the
// same directory: mode 0600, synced, renamed over path, directory synced.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("draw a temporary file name: %w", err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+hex.EncodeToString(nonce[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- inside the agent's private key material directory, name drawn at random, O_EXCL
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("install %s: %w", path, err)
	}
	d, err := os.Open(dir) // #nosec G304 -- the agent's key material directory
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
