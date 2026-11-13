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

// Package trust loads what a per-host agent trusts and what it is: its trust
// configuration (the issuer of host identity certificates, the issuer of
// Provider identity certificates, the operation grant anchors and the Provider
// identities it serves) and its own identity (the host key and the host
// certificate, with the enrolment record the certificate carries).
//
// The trust configuration lives in a directory of the host's signed,
// read-only system image (DefaultDir); the start-up checks refuse a writable
// one before it is loaded. The grant anchors and the host identity issuer
// must be separate hierarchies (CheckSeparation): an issuer of host
// certificates must never be able to issue a certificate that verifies as a
// grant signer. The Provider identity issuer is kept apart from both, so that
// no issuer of one role can mint a credential of another.
//
// The Provider identity certificate the agent pins is a development-only
// stand-in for the Provider identity path that a later change defines.
//
// It imports no Kubernetes package.
package trust

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/safefile"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// DefaultDir is where the trust configuration is read from: a directory of
// the host's signed, read-only system image, never a writable location such
// as /etc.
const DefaultDir = "/usr/share/virtrigaud/trust"

// The files of a trust configuration directory.
const (
	// FileHostCA holds the PEM certificates of the issuer of host identity
	// certificates.
	FileHostCA = "host-ca.pem"
	// FileProviderCA holds the PEM certificates of the issuer of Provider
	// identity certificates.
	FileProviderCA = "provider-ca.pem"
	// FileGrantAnchors holds the PEM operation grant anchors.
	FileGrantAnchors = "grant-anchors.pem"
	// FileProviderSANs lists the Provider identities the agent serves, one
	// URI per line: spiffe://<trust domain>/virtrigaud/provider/<namespace>/<name>.
	// Blank lines and lines starting with '#' are ignored.
	FileProviderSANs = "provider-sans"
)

// maxTrustFileBytes bounds every file the package reads.
const maxTrustFileBytes = 1 << 20

// providerPathPrefix is the path prefix of a Provider URI identity.
const providerPathPrefix = "/virtrigaud/provider/"

// hostPathPrefix is the path prefix of a host URI identity.
const hostPathPrefix = "/virtrigaud/host/"

// spiffeScheme is the scheme of URI identities.
const spiffeScheme = "spiffe"

var (
	// dnsLabelRE is a Kubernetes namespace or object name.
	dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	// uidRE is a Kubernetes UID.
	uidRE = regexp.MustCompile(`^[0-9a-z-]{1,128}$`)
)

// ErrConfig is wrapped by every trust configuration error.
var ErrConfig = errors.New("trust configuration refused")

// ProviderIdentity names a Provider: its trust domain, namespace and name.
type ProviderIdentity struct {
	// TrustDomain is the trust domain of the URI identity.
	TrustDomain string
	// Namespace is the Provider's namespace.
	Namespace string
	// Name is the Provider's name.
	Name string
}

// String returns the identity's URI prefix (without the UID).
func (p ProviderIdentity) String() string {
	return spiffeScheme + "://" + p.TrustDomain + providerPathPrefix + p.Namespace + "/" + p.Name
}

// Config is a loaded trust configuration.
type Config struct {
	// HostCAs are the issuers of host identity certificates.
	HostCAs []*x509.Certificate
	// HostCAPool holds HostCAs.
	HostCAPool *x509.CertPool
	// ProviderCAs are the issuers of Provider identity certificates.
	ProviderCAs []*x509.Certificate
	// ProviderCAPool holds ProviderCAs.
	ProviderCAPool *x509.CertPool
	// GrantAnchors are the operation grant anchors.
	GrantAnchors []*x509.Certificate
	// Providers are the Provider identities the agent serves.
	Providers []ProviderIdentity
}

// ReadFile reads a file of the trust configuration or the key material: it
// opens it without following a symbolic link and reads it only if the open
// file is a regular file of at most 1 MiB. It checks no owner or mode; see
// TrustFileReader and KeyFileReader.
func ReadFile(path string) ([]byte, error) {
	return safefile.Read(path, safefile.Policy{Max: maxTrustFileBytes})
}

// TrustFileReader returns a reader for the trust configuration: each file,
// checked on its open descriptor, must be a regular file owned by owner (root
// on a host) that group and others cannot write, of at most 1 MiB.
func TrustFileReader(owner uint32) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		return safefile.Read(path, safefile.Policy{Owner: &owner, DenyMode: 0o022, Max: maxTrustFileBytes})
	}
}

// KeyFileReader returns a reader for the agent's key material directory (the
// host certificate; the private key is not there): each file, checked on its
// open descriptor, must be a regular file owned by the agent's user uid with
// no permission for group or others, of at most 1 MiB.
func KeyFileReader(uid uint32) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		return safefile.Read(path, safefile.Policy{Owner: &uid, DenyMode: 0o077, Max: maxTrustFileBytes})
	}
}

// HandoffFileReader returns a reader for the files an administrator hands to
// an enrolment step (the request, the issued certificate, the list of request
// URIs): each file, checked on its open descriptor, must be a regular file
// owned by root or by caller (the user running the step) that group and
// others cannot write, of at most 1 MiB. A file another user could have
// replaced, or planted in a shared directory, is refused.
func HandoffFileReader(caller uint32) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		return safefile.Read(path, safefile.Policy{Owners: []uint32{0, caller}, DenyMode: 0o022, Max: maxTrustFileBytes})
	}
}

// LoadConfig reads and validates the trust configuration in dir with read
// (ReadFile when nil). Every file is required and none may be empty; the
// grant anchors, the host identity issuers and the Provider identity issuers
// must be three separate hierarchies (CheckSeparated).
func LoadConfig(dir string, read func(string) ([]byte, error)) (*Config, error) {
	if read == nil {
		read = ReadFile
	}
	certs := func(name string, wantCA bool) ([]*x509.Certificate, *x509.CertPool, error) {
		b, err := read(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: read %s: %v", ErrConfig, name, err)
		}
		cs, err := hostidentity.ParseCertificates(b)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %v", ErrConfig, name, err)
		}
		if len(cs) == 0 {
			return nil, nil, fmt.Errorf("%w: %s holds no certificate", ErrConfig, name)
		}
		pool := x509.NewCertPool()
		for _, c := range cs {
			if wantCA && (!c.IsCA || !c.BasicConstraintsValid) {
				return nil, nil, fmt.Errorf("%w: %s holds a certificate that is not a CA", ErrConfig, name)
			}
			pool.AddCert(c)
		}
		return cs, pool, nil
	}
	cfg := &Config{}
	var err error
	if cfg.HostCAs, cfg.HostCAPool, err = certs(FileHostCA, true); err != nil {
		return nil, err
	}
	if cfg.ProviderCAs, cfg.ProviderCAPool, err = certs(FileProviderCA, true); err != nil {
		return nil, err
	}
	if cfg.GrantAnchors, _, err = certs(FileGrantAnchors, false); err != nil {
		return nil, err
	}
	b, err := read(filepath.Join(dir, FileProviderSANs))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrConfig, FileProviderSANs, err)
	}
	if cfg.Providers, err = ParseProviderList(b); err != nil {
		return nil, err
	}
	if err := CheckSeparation(cfg.GrantAnchors, cfg.HostCAs); err != nil {
		return nil, err
	}
	if err := CheckSeparated("Provider identity issuers", cfg.ProviderCAs, "host identity issuers", cfg.HostCAs); err != nil {
		return nil, err
	}
	if err := CheckSeparated("Provider identity issuers", cfg.ProviderCAs, "grant anchors", cfg.GrantAnchors); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ParseProviderList parses the Provider identity allow-list (FileProviderSANs).
// It must name at least one identity; a repeated identity is refused.
func ParseProviderList(b []byte) ([]ProviderIdentity, error) {
	var out []ProviderIdentity
	seen := map[ProviderIdentity]bool{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			return nil, fmt.Errorf("%w: %s line %d: not a URI", ErrConfig, FileProviderSANs, i+1)
		}
		id, rest, err := parseProviderURI(u)
		if err != nil || rest != "" {
			return nil, fmt.Errorf("%w: %s line %d: not a Provider identity without a UID", ErrConfig, FileProviderSANs, i+1)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: %s line %d: repeated identity", ErrConfig, FileProviderSANs, i+1)
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s names no Provider", ErrConfig, FileProviderSANs)
	}
	return out, nil
}

// parseProviderURI splits spiffe://<td>/virtrigaud/provider/<ns>/<name>[/<uid>]
// into the identity and the UID ("" when absent).
func parseProviderURI(u *url.URL) (ProviderIdentity, string, error) {
	errNot := errors.New("not a Provider URI identity")
	if u == nil || u.Scheme != spiffeScheme || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" ||
		u.RawPath != "" {
		return ProviderIdentity{}, "", errNot
	}
	if err := hostidentity.ValidateTrustDomain(u.Host); err != nil {
		return ProviderIdentity{}, "", errNot
	}
	rest, ok := strings.CutPrefix(u.Path, providerPathPrefix)
	if !ok {
		return ProviderIdentity{}, "", errNot
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return ProviderIdentity{}, "", errNot
	}
	if !dnsLabelRE.MatchString(parts[0]) || !dnsLabelRE.MatchString(parts[1]) {
		return ProviderIdentity{}, "", errNot
	}
	id := ProviderIdentity{TrustDomain: u.Host, Namespace: parts[0], Name: parts[1]}
	if len(parts) == 2 {
		return id, "", nil
	}
	if !uidRE.MatchString(parts[2]) {
		return ProviderIdentity{}, "", errNot
	}
	return id, parts[2], nil
}

// ErrProviderRefused is wrapped by AuthorizeProvider's errors.
var ErrProviderRefused = errors.New("provider identity refused")

// AuthorizeProvider checks the URI identity of a verified Provider client
// certificate: it must carry exactly one URI subject alternative name, a
// Provider identity with a UID whose trust domain, namespace and name are in
// the allow-list. It returns the identity and the UID.
func (c *Config) AuthorizeProvider(leaf *x509.Certificate) (ProviderIdentity, string, error) {
	if leaf == nil || len(leaf.URIs) != 1 {
		return ProviderIdentity{}, "", fmt.Errorf("%w: the certificate must carry exactly one URI identity", ErrProviderRefused)
	}
	id, uid, err := parseProviderURI(leaf.URIs[0])
	if err != nil || uid == "" {
		return ProviderIdentity{}, "", fmt.Errorf("%w: the URI identity is not a Provider identity with a UID", ErrProviderRefused)
	}
	for _, p := range c.Providers {
		if p == id {
			return id, uid, nil
		}
	}
	return ProviderIdentity{}, "", fmt.Errorf("%w: %s is not an allowed Provider", ErrProviderRefused, id)
}

// CheckSeparation refuses a trust configuration in which the grant anchors and
// the issuers of host identity certificates are not separate hierarchies
// (CheckSeparated). A holder of the host identity issuer's key must never be
// able to issue a certificate that verifies as a grant signer, nor the
// reverse.
func CheckSeparation(grantAnchors, hostCAs []*x509.Certificate) error {
	return CheckSeparated("grant anchors", grantAnchors, "host identity issuers", hostCAs)
}

// CheckSeparated refuses two sets of trust anchors, named aName and bName,
// that share a certificate, a key or an issuer, or in which a certificate of
// one set signed, or chains to, a certificate of the other.
func CheckSeparated(aName string, a []*x509.Certificate, bName string, b []*x509.Certificate) error {
	all := x509.NewCertPool()
	aPool := x509.NewCertPool()
	bPool := x509.NewCertPool()
	for _, c := range a {
		all.AddCert(c)
		aPool.AddCert(c)
	}
	for _, c := range b {
		all.AddCert(c)
		bPool.AddCert(c)
	}
	violation := func(what string) error {
		return fmt.Errorf("%w: the %s and the %s are not separate hierarchies: %s", ErrConfig, aName, bName, what)
	}
	for _, g := range a {
		for _, h := range b {
			switch {
			case g.Equal(h):
				return violation("a certificate is in both")
			case bytes.Equal(g.RawSubjectPublicKeyInfo, h.RawSubjectPublicKeyInfo):
				return violation("a key is in both")
			case h.CheckSignatureFrom(g) == nil, g.CheckSignatureFrom(h) == nil:
				return violation("a certificate of one signed a certificate of the other")
			case len(g.AuthorityKeyId) > 0 && bytes.Equal(g.AuthorityKeyId, h.AuthorityKeyId):
				return violation("they share an issuer")
			case len(g.SubjectKeyId) > 0 && bytes.Equal(g.SubjectKeyId, h.AuthorityKeyId),
				len(h.SubjectKeyId) > 0 && bytes.Equal(h.SubjectKeyId, g.AuthorityKeyId):
				return violation("a certificate of one names the other as its issuer")
			}
		}
	}
	chains := func(c *x509.Certificate, roots *x509.CertPool) bool {
		_, err := c.Verify(x509.VerifyOptions{Roots: roots, Intermediates: all,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}, CurrentTime: c.NotBefore.Add(time.Second)})
		return err == nil
	}
	for _, c := range b {
		if chains(c, aPool) {
			return violation("one of the " + bName + " chains to one of the " + aName)
		}
	}
	for _, c := range a {
		if chains(c, bPool) {
			return violation("one of the " + aName + " chains to one of the " + bName)
		}
	}
	return nil
}
