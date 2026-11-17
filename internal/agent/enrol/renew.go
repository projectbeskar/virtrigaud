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
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// Certificate renewal, agent side (development builds only).
//
// At two thirds of its certificate's lifetime the agent asks the signer for
// a certificate request for a new key (the signer makes and keeps the key)
// and keeps the request as FileRenewalCSR. The request is offered to the
// manager, co-signed by the current key, in the signed answer of GetHostInfo.
// The Provider then delivers the issued certificate with
// InstallRenewedCertificate: first to stage it (CheckRenewed, then
// FileRenewalCert), and, once the manager has pinned its key, to put it in
// use (the signer puts the new key in use, then Promote makes the staged
// certificate the host certificate).
//
// Every file is written to a private temporary file, synced and renamed, so
// a crash leaves the old or the new file. A crash between the signer's switch
// to the new key and Promote leaves a host certificate for the old key and a
// signer that holds the new one; RecoverRenewal, run before the identity is
// loaded at start and on reload, promotes the staged certificate when it
// certifies the signer's key, so the host is always left with one usable
// pair: the current certificate and key, or the renewed ones.

// The renewal files of the key material directory.
const (
	// FileRenewalCSR holds the PEM certificate request of the pending
	// renewal: for the signer's pending key, carrying exactly the current
	// certificate's URIs.
	FileRenewalCSR = "renewal.csr"
	// FileRenewalCert holds the staged renewed certificate (PEM), not yet in
	// use.
	FileRenewalCert = "renewal.crt"
	// FileRenewalSince holds when the pending renewal was requested (RFC 3339,
	// the agent's clock), for the bound after which an unanswered request is
	// drawn again (PendingExpired).
	FileRenewalSince = "renewal.since"
)

// ErrRenewed is wrapped by every refusal of a renewed certificate. Nothing is
// written when it is returned.
var ErrRenewed = errors.New("renewed certificate refused")

// maxRenewalFileBytes bounds a renewal file.
const maxRenewalFileBytes = 16 << 10

// RenewalAt returns when a certificate is due for renewal: two thirds of the
// way through its lifetime.
func RenewalAt(leaf *x509.Certificate) time.Time {
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	return leaf.NotBefore.Add(life / 3 * 2)
}

// RenewalDue reports whether leaf is due for renewal at now.
func RenewalDue(leaf *x509.Certificate, now time.Time) bool {
	return !now.Before(RenewalAt(leaf))
}

// Pending is the agent's pending renewal, as its files hold it.
type Pending struct {
	// CSR is the pending certificate request; nil when no renewal is
	// pending.
	CSR *x509.CertificateRequest
	// KeyPin is the pin of the pending key (the request's).
	KeyPin string
	// Staged is the staged renewed certificate; nil when none is staged.
	Staged *x509.Certificate
	// Since is when the pending request was made; zero when unknown.
	Since time.Time
}

// PendingExpired reports whether a pending renewal of the current
// certificate cur that was never staged has waited past half of the lifetime
// cur had left when it was requested, at now: the manager has not answered
// it (its request was denied, failed or lost), and a new key is drawn so that
// a fresh request reaches it before cur expires. A staged renewal never
// expires: the manager may have pinned its key already.
func (p Pending) PendingExpired(cur *x509.Certificate, now time.Time) bool {
	if p.CSR == nil || p.Staged != nil || p.Since.IsZero() || !cur.NotAfter.After(p.Since) {
		return false
	}
	return now.After(p.Since.Add(cur.NotAfter.Sub(p.Since) / 2))
}

// readRenewalFile reads a renewal file with read; found is false when it
// does not exist.
func readRenewalFile(path string, read func(string) ([]byte, error)) (b []byte, found bool, err error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	b, err = read(path)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > maxRenewalFileBytes {
		return nil, false, fmt.Errorf("%s is larger than %d bytes", path, maxRenewalFileBytes)
	}
	return b, true, nil
}

// LoadPending reads the renewal files of pkiDir with read (a strict file
// reader, trust.KeyFileReader of the agent's user). A file that is present
// but unreadable or malformed is an error; the caller discards it.
func LoadPending(pkiDir string, read func(string) ([]byte, error)) (Pending, error) {
	var p Pending
	b, found, err := readRenewalFile(filepath.Join(pkiDir, FileRenewalCSR), read)
	if err != nil {
		return Pending{}, err
	}
	if found {
		csr, err := hostidentity.ParseCSR(b)
		if err != nil {
			return Pending{}, fmt.Errorf("%s: %w", FileRenewalCSR, err)
		}
		if err := csr.CheckSignature(); err != nil {
			return Pending{}, fmt.Errorf("%s: its signature does not verify", FileRenewalCSR)
		}
		if p.KeyPin, err = hostidentity.SPKIPin(csr.PublicKey); err != nil {
			return Pending{}, fmt.Errorf("%s: %w", FileRenewalCSR, err)
		}
		p.CSR = csr
		if b, ok, err := readRenewalFile(filepath.Join(pkiDir, FileRenewalSince), read); err == nil && ok {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil {
				p.Since = t
			}
		}
	}
	b, found, err = readRenewalFile(filepath.Join(pkiDir, FileRenewalCert), read)
	if err != nil {
		return Pending{}, err
	}
	if found {
		certs, err := hostidentity.ParseCertificates(b)
		if err != nil || len(certs) != 1 {
			return Pending{}, fmt.Errorf("%s does not hold exactly one certificate", FileRenewalCert)
		}
		p.Staged = certs[0]
	}
	return p, nil
}

// SavePendingCSR writes der, the certificate request for the signer's
// pending key, as FileRenewalCSR in pkiDir, after checking that it carries
// exactly the URIs of the current certificate cur and that its signature
// verifies. A staged certificate for another request is removed first.
func SavePendingCSR(pkiDir string, der []byte, cur *x509.Certificate, since time.Time) error {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || !bytes.Equal(csr.Raw, der) || csr.CheckSignature() != nil {
		return fmt.Errorf("%w: the renewal request does not parse or verify", ErrRequest)
	}
	if !sameURIs(csr.URIs, cur.URIs) {
		return fmt.Errorf("%w: the renewal request does not carry the current certificate's URIs", ErrRequest)
	}
	if err := removeFile(filepath.Join(pkiDir, FileRenewalCert)); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(pkiDir, FileRenewalSince), []byte(since.UTC().Format(time.RFC3339)+"\n")); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(pkiDir, FileRenewalCSR), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// MatchesCurrent reports whether the pending renewal belongs to the current
// certificate cur: its request carries exactly cur's URIs and is for another
// key than cur's.
func (p Pending) MatchesCurrent(cur *x509.Certificate) bool {
	if p.CSR == nil {
		return false
	}
	pub, ok := p.CSR.PublicKey.(*ecdsa.PublicKey)
	return ok && sameURIs(p.CSR.URIs, cur.URIs) && !pub.Equal(cur.PublicKey)
}

// CheckRenewed checks a renewed certificate (DER) delivered for the pending
// renewal p of the current identity cur, against the trust configuration tc
// at now: one DER certificate of at most hostidentity.MaxAnswerCertBytes;
// it chains directly to a pinned host identity issuer for server
// authentication and is valid at now; it meets the certificate profile
// (hostidentity.CheckLeafProfile) and lasts at least hostidentity.MinLifetime;
// it carries exactly the current
// certificate's URI names (same host, same enrolment record, same epoch);
// and it certifies the pending key, which is not the current key.
func CheckRenewed(der []byte, cur *trust.Identity, p Pending, tc *trust.Config, now time.Time) (*x509.Certificate, error) {
	if p.CSR == nil {
		return nil, fmt.Errorf("%w: no renewal is pending", ErrRenewed)
	}
	if len(der) == 0 || len(der) > hostidentity.MaxAnswerCertBytes {
		return nil, fmt.Errorf("%w: the certificate is empty or larger than %d bytes", ErrRenewed, hostidentity.MaxAnswerCertBytes)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("%w: it is not one DER certificate", ErrRenewed)
	}
	if !guard.Off(guard.RenewChain) {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: tc.HostCAPool, CurrentTime: now,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return nil, fmt.Errorf("%w: it does not chain to the pinned host identity issuer at this time", ErrRenewed)
		}
	}
	// The profile bounds the lifetime too (at most hostidentity.MaxLifetime),
	// so the agent never installs a long-lived certificate whatever the
	// manager approved.
	if err := hostidentity.CheckLeafProfile(leaf, cur.HostName); err != nil && !guard.Off(guard.RenewProfile) {
		return nil, fmt.Errorf("%w: %v", ErrRenewed, err)
	}
	// A renewed certificate too short to finish its own renewal is refused
	// here, before it is staged (an installed one is not: see
	// hostidentity.CheckLeafMinLifetime).
	if err := hostidentity.CheckLeafMinLifetime(leaf); err != nil && !guard.Off(guard.RenewMinLifetime) {
		return nil, fmt.Errorf("%w: %w", ErrRenewed, err)
	}
	if !guard.Off(guard.RenewSameIdentity) {
		rec, err := hostidentity.RecordFromURIs(leaf.URIs, cur.HostURI)
		if err != nil || !rec.Equal(cur.Record) || !sameURIs(leaf.URIs, cur.Leaf.URIs) {
			return nil, fmt.Errorf("%w: it does not carry exactly the current certificate's identities", ErrRenewed)
		}
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: its key is not ECDSA P-256", ErrRenewed)
	}
	if !guard.Off(guard.RenewPendingKey) && !pub.Equal(p.CSR.PublicKey) {
		return nil, fmt.Errorf("%w: it does not certify the key of the pending renewal", ErrRenewed)
	}
	if pub.Equal(cur.Leaf.PublicKey) {
		return nil, fmt.Errorf("%w: it certifies the current key", ErrRenewed)
	}
	return leaf, nil
}

// Stage writes der as the staged renewed certificate FileRenewalCert in
// pkiDir.
func Stage(pkiDir string, der []byte) error {
	return writeAtomic(filepath.Join(pkiDir, FileRenewalCert), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// Promote makes the staged renewed certificate the host certificate: it
// writes it as trust.FileHostCert (written whole and renamed, so a crash
// leaves the old or the new certificate), then removes the renewal files. It
// is the step after the signer put the new key in use.
func Promote(pkiDir string, read func(string) ([]byte, error)) error {
	b, found, err := readRenewalFile(filepath.Join(pkiDir, FileRenewalCert), read)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: no staged certificate", ErrRenewed)
	}
	if err := writeAtomic(filepath.Join(pkiDir, trust.FileHostCert), b); err != nil {
		return err
	}
	return DiscardPending(pkiDir)
}

// DiscardPending removes the renewal files of pkiDir.
func DiscardPending(pkiDir string) error {
	return errors.Join(removeFile(filepath.Join(pkiDir, FileRenewalCert)), removeFile(filepath.Join(pkiDir, FileRenewalCSR)),
		removeFile(filepath.Join(pkiDir, FileRenewalSince)))
}

// RecoverRenewal finishes a renewal that a crash cut short between the
// signer putting the new key in use and Promote: when the host certificate
// does not certify the signer's key signerPub, and the staged certificate
// does, is valid at now under tc and names the same host and enrolment as
// the host certificate, the staged certificate is promoted. It reports
// whether it promoted. Anything else is left for the identity loader to
// judge.
func RecoverRenewal(pkiDir string, read func(string) ([]byte, error), tc *trust.Config, signerPub *ecdsa.PublicKey, now time.Time) (bool, error) {
	hostPEM, err := read(filepath.Join(pkiDir, trust.FileHostCert))
	if err != nil {
		return false, nil //nolint:nilerr // no host certificate: the loader reports it
	}
	certs, err := hostidentity.ParseCertificates(hostPEM)
	if err != nil || len(certs) != 1 {
		return false, nil //nolint:nilerr // the loader reports it
	}
	host := certs[0]
	if pub, ok := host.PublicKey.(*ecdsa.PublicKey); ok && pub.Equal(signerPub) {
		return false, nil
	}
	p, err := LoadPending(pkiDir, read)
	if err != nil || p.Staged == nil {
		return false, nil //nolint:nilerr // nothing usable to recover with
	}
	staged := p.Staged
	if pub, ok := staged.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(signerPub) {
		return false, nil
	}
	if _, err := staged.Verify(x509.VerifyOptions{Roots: tc.HostCAPool, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return false, nil //nolint:nilerr // not a certificate the host may use
	}
	if !sameURIs(staged.URIs, host.URIs) {
		return false, nil
	}
	if err := Promote(pkiDir, read); err != nil {
		return false, fmt.Errorf("finish the interrupted renewal: %w", err)
	}
	return true, nil
}

// removeFile removes path; a missing file is not an error. The directory is
// synced, so the removal survives a crash.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove %s: %w", path, err)
	}
	d, err := os.Open(filepath.Dir(path)) // #nosec G304 -- the agent's key material directory
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Dir(path), err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", filepath.Dir(path), err)
	}
	return nil
}

// sameURIs reports whether two URI lists hold the same URIs, as sets.
func sameURIs(a, b []*url.URL) bool {
	if len(a) != len(b) {
		return false
	}
	as := make([]string, 0, len(a))
	for _, u := range a {
		as = append(as, u.String())
	}
	bs := make([]string, 0, len(b))
	for _, u := range b {
		bs = append(bs, u.String())
	}
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(as, bs) && len(slices.Compact(slices.Clone(as))) == len(as)
}
