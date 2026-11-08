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

package hostidentity

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

const (
	// IDLen is the length in bytes of an enrolment identifier and of an
	// install identifier.
	IDLen = 16

	// URIEnrolmentPrefix prefixes the enrolment identifier URI.
	URIEnrolmentPrefix = "urn:virtrigaud:enrolment:"
	// URIProviderPrefix prefixes the Provider UID URI.
	URIProviderPrefix = "urn:virtrigaud:provider:"
	// URIInstallPrefix prefixes the install identifier URI.
	URIInstallPrefix = "urn:virtrigaud:install:"
	// URIModeIdentity is the mode URI of an identity-only enrolment.
	URIModeIdentity = "urn:virtrigaud:mode:identity"

	// ModeIdentity is the only enrolment mode.
	ModeIdentity = "identity"

	// hostURIScheme and hostURIPathPrefix shape a host URI identity.
	hostURIScheme     = "spiffe"
	hostURIPathPrefix = "/virtrigaud/host/"

	// recordURICount is the number of URIs in a certificate: the host URI and
	// the four record URIs.
	recordURICount = 5
)

var (
	// trustDomainRE is a SPIFFE trust domain: lowercase letters, digits, dots,
	// dashes and underscores.
	trustDomainRE = regexp.MustCompile(`^[a-z0-9._-]{1,255}$`)
	// dnsLabelRE is a Kubernetes namespace or object name segment.
	dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	// providerUIDRE is a Kubernetes UID.
	providerUIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// pinRE is a key pin.
	pinRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ErrRecord is wrapped by every error RecordFromURIs and Record.Validate
// return.
var ErrRecord = errors.New("invalid enrolment record")

// ValidateTrustDomain checks a SPIFFE trust domain.
func ValidateTrustDomain(td string) error {
	if !trustDomainRE.MatchString(td) {
		return fmt.Errorf("trust domain %q is not lowercase letters, digits, dots, dashes and underscores", td)
	}
	return nil
}

// HostURI returns the URI identity of the Host namespace/name under the trust
// domain: spiffe://<td>/virtrigaud/host/<namespace>/<name>.
func HostURI(trustDomain, namespace, name string) (string, error) {
	if err := ValidateTrustDomain(trustDomain); err != nil {
		return "", err
	}
	if !dnsLabelRE.MatchString(namespace) || !dnsLabelRE.MatchString(name) {
		return "", fmt.Errorf("host %s/%s is not a valid namespace and name", namespace, name)
	}
	return hostURIScheme + "://" + trustDomain + hostURIPathPrefix + namespace + "/" + name, nil
}

// ValidateHostURI checks that s has the shape of a host URI identity.
func ValidateHostURI(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != hostURIScheme || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" {
		return errors.New("not a host URI identity")
	}
	rest, ok := strings.CutPrefix(u.Path, hostURIPathPrefix)
	if !ok {
		return errors.New("not a host URI identity")
	}
	ns, name, ok := strings.Cut(rest, "/")
	if !ok {
		return errors.New("not a host URI identity")
	}
	if _, err := HostURI(u.Host, ns, name); err != nil {
		return errors.New("not a host URI identity")
	}
	return nil
}

// Record is the enrolment record carried in an agent certificate.
type Record struct {
	// EnrolmentID is the 16-byte enrolment identifier the manager drew.
	EnrolmentID []byte
	// ProviderUID is the UID of the Provider the host belongs to.
	ProviderUID string
	// InstallID is the 16-byte identifier of the manager installation.
	InstallID []byte
	// Mode is the enrolment mode; only ModeIdentity exists.
	Mode string
}

// Validate checks the record's fields.
func (r Record) Validate() error {
	switch {
	case len(r.EnrolmentID) != IDLen:
		return fmt.Errorf("%w: enrolment id is not %d bytes", ErrRecord, IDLen)
	case len(r.InstallID) != IDLen:
		return fmt.Errorf("%w: install id is not %d bytes", ErrRecord, IDLen)
	case !providerUIDRE.MatchString(r.ProviderUID):
		return fmt.Errorf("%w: provider uid is not a UID", ErrRecord)
	case r.Mode != ModeIdentity:
		return fmt.Errorf("%w: mode is not %q", ErrRecord, ModeIdentity)
	}
	return nil
}

// Equal reports whether two records are the same.
func (r Record) Equal(o Record) bool {
	return bytes.Equal(r.EnrolmentID, o.EnrolmentID) && bytes.Equal(r.InstallID, o.InstallID) &&
		r.ProviderUID == o.ProviderUID && r.Mode == o.Mode
}

// URIs returns the record's four URIs, in a fixed order.
func (r Record) URIs() []string {
	return []string{
		URIEnrolmentPrefix + hex.EncodeToString(r.EnrolmentID),
		URIProviderPrefix + r.ProviderUID,
		URIInstallPrefix + hex.EncodeToString(r.InstallID),
		URIModeIdentity,
	}
}

// RequestURIs returns the exact URI subject alternative names a certificate
// for the record must carry: the host URI, then the record's URIs.
func RequestURIs(hostURI string, r Record) []string {
	return append([]string{hostURI}, r.URIs()...)
}

// NewEnrolmentID draws a 16-byte enrolment identifier from rand.
func NewEnrolmentID(rand io.Reader) ([]byte, error) {
	id := make([]byte, IDLen)
	if _, err := io.ReadFull(rand, id); err != nil {
		return nil, fmt.Errorf("draw enrolment id: %w", err)
	}
	return id, nil
}

// RecordFromURIs derives the enrolment record from the URI subject
// alternative names of a certificate or request. The set must be exactly
// hostURI plus one of each record URI, with no other and no repeated URI.
func RecordFromURIs(uris []*url.URL, hostURI string) (Record, error) {
	if len(uris) != recordURICount {
		return Record{}, fmt.Errorf("%w: %d URIs, want exactly %d", ErrRecord, len(uris), recordURICount)
	}
	var (
		rec                                   Record
		haveHost, haveEnrol, haveProv, haveIn bool
	)
	for _, u := range uris {
		if u == nil {
			return Record{}, fmt.Errorf("%w: empty URI", ErrRecord)
		}
		s := u.String()
		switch {
		case s == hostURI:
			if haveHost {
				return Record{}, fmt.Errorf("%w: repeated host URI", ErrRecord)
			}
			haveHost = true
		case s == URIModeIdentity:
			if rec.Mode != "" {
				return Record{}, fmt.Errorf("%w: repeated mode URI", ErrRecord)
			}
			rec.Mode = ModeIdentity
		case strings.HasPrefix(s, URIEnrolmentPrefix):
			if haveEnrol {
				return Record{}, fmt.Errorf("%w: repeated enrolment URI", ErrRecord)
			}
			haveEnrol = true
			id, err := decodeLowerHexID(strings.TrimPrefix(s, URIEnrolmentPrefix))
			if err != nil {
				return Record{}, fmt.Errorf("%w: enrolment URI: %v", ErrRecord, err)
			}
			rec.EnrolmentID = id
		case strings.HasPrefix(s, URIProviderPrefix):
			if haveProv {
				return Record{}, fmt.Errorf("%w: repeated provider URI", ErrRecord)
			}
			haveProv = true
			rec.ProviderUID = strings.TrimPrefix(s, URIProviderPrefix)
		case strings.HasPrefix(s, URIInstallPrefix):
			if haveIn {
				return Record{}, fmt.Errorf("%w: repeated install URI", ErrRecord)
			}
			haveIn = true
			id, err := decodeLowerHexID(strings.TrimPrefix(s, URIInstallPrefix))
			if err != nil {
				return Record{}, fmt.Errorf("%w: install URI: %v", ErrRecord, err)
			}
			rec.InstallID = id
		default:
			return Record{}, fmt.Errorf("%w: unexpected URI", ErrRecord)
		}
	}
	if !haveHost {
		return Record{}, fmt.Errorf("%w: the host URI is missing", ErrRecord)
	}
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// decodeLowerHexID decodes a 16-byte identifier written as 32 lowercase
// hexadecimal characters (uppercase is refused, so each identifier has one
// spelling).
func decodeLowerHexID(s string) ([]byte, error) {
	if s != strings.ToLower(s) {
		return nil, errors.New("not lowercase hexadecimal")
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != IDLen {
		return nil, fmt.Errorf("not %d hexadecimal characters", 2*IDLen)
	}
	return b, nil
}

// DecodeID decodes a 16-byte identifier written as 32 lowercase hexadecimal
// characters.
func DecodeID(s string) ([]byte, error) {
	return decodeLowerHexID(s)
}

// SPKIPin returns the pin of a public key: the SHA-256 of its DER
// SubjectPublicKeyInfo, as 64 lowercase hexadecimal characters.
func SPKIPin(pub any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// ValidatePin checks the shape of a key pin.
func ValidatePin(pin string) error {
	if !pinRE.MatchString(pin) {
		return errors.New("key pin is not 64 lowercase hexadecimal characters")
	}
	return nil
}
