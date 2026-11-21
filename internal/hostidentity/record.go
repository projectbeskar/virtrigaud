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
	"strconv"
	"strings"
)

const (
	// IDLen is the length in bytes of an enrolment identifier and of an
	// install identifier.
	IDLen = 16

	// RecordPathPrefix is the path prefix of every record URI:
	// spiffe://<trust domain>/virtrigaud/record/<kind>/<value>. The record
	// URIs carry the host URI's trust domain as their host, so a sub-CA
	// whose URI name constraint permits only the trust domain can issue
	// them (a URI without a host fails every URI name constraint, RFC 5280
	// section 4.2.1.10).
	RecordPathPrefix = "/virtrigaud/record/"

	// RecordKindEnrolment names the enrolment identifier URI.
	RecordKindEnrolment = "enrolment"
	// RecordKindProvider names the Provider UID URI.
	RecordKindProvider = "provider"
	// RecordKindInstall names the install identifier URI.
	RecordKindInstall = "install"
	// RecordKindMode names the mode URI.
	RecordKindMode = "mode"
	// RecordKindEpoch names the start epoch URI: the decimal start epoch of
	// the enrolment, from 2. A record without it starts at epoch 1, so a
	// first enrolment's names are unchanged.
	RecordKindEpoch = "epoch"

	// AnnotationStartEpochFloor is the Host annotation that raises the start
	// epoch of the first record the manager draws for a Host that holds no
	// enrolment state (a re-created Host), so that a host whose store has
	// already started a later epoch can enrol it again. The manager ignores
	// it once the Host has an enrolment or an approved record, and above
	// 65536.
	AnnotationStartEpochFloor = "enrolment.virtrigaud.io/start-epoch-floor"

	// FirstStartEpoch is the start epoch of a record without an epoch URI.
	FirstStartEpoch = 1
	// MaxStartEpoch bounds the start epoch a record may carry.
	MaxStartEpoch = 1 << 32

	// ModeIdentity is the only enrolment mode.
	ModeIdentity = "identity"

	// hostURIScheme and hostURIPathPrefix shape a host URI identity; the
	// record URIs share the scheme.
	hostURIScheme     = "spiffe"
	hostURIPathPrefix = "/virtrigaud/host/"

	// recordURICount is the number of URIs in a certificate: the host URI and
	// the four record URIs, plus the epoch URI for a start epoch above 1.
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
	// StartEpoch is the epoch the host starts the enrolment at; zero or
	// FirstStartEpoch is a first-epoch record, which carries no epoch URI.
	// The manager chooses it and the CA signs it, so the host enrols at
	// exactly this epoch and grants for the enrolment are minted at it.
	StartEpoch uint64
}

// Epoch returns the record's start epoch: StartEpoch, or FirstStartEpoch
// when it is unset.
func (r Record) Epoch() uint64 {
	if r.StartEpoch < FirstStartEpoch {
		return FirstStartEpoch
	}
	return r.StartEpoch
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
	case r.StartEpoch > MaxStartEpoch:
		return fmt.Errorf("%w: start epoch above %d", ErrRecord, uint64(MaxStartEpoch))
	}
	return nil
}

// Equal reports whether two records are the same.
func (r Record) Equal(o Record) bool {
	return bytes.Equal(r.EnrolmentID, o.EnrolmentID) && bytes.Equal(r.InstallID, o.InstallID) &&
		r.ProviderUID == o.ProviderUID && r.Mode == o.Mode && r.Epoch() == o.Epoch()
}

// RecordURI returns the record URI of kind with value under the trust
// domain: spiffe://<trust domain>/virtrigaud/record/<kind>/<value>.
func RecordURI(trustDomain, kind, value string) string {
	return hostURIScheme + "://" + trustDomain + RecordPathPrefix + kind + "/" + value
}

// EnrolmentURI returns the enrolment identifier URI under the trust domain
// for an identifier written as 32 lowercase hexadecimal characters.
func EnrolmentURI(trustDomain, enrolmentIDHex string) string {
	return RecordURI(trustDomain, RecordKindEnrolment, enrolmentIDHex)
}

// InstallURI returns the install identifier URI under the trust domain.
func InstallURI(trustDomain string, installID []byte) string {
	return RecordURI(trustDomain, RecordKindInstall, hex.EncodeToString(installID))
}

// EpochURI returns the start epoch URI under the trust domain.
func EpochURI(trustDomain string, epoch uint64) string {
	return RecordURI(trustDomain, RecordKindEpoch, strconv.FormatUint(epoch, 10))
}

// TrustDomainOf returns the trust domain of a host URI identity.
func TrustDomainOf(hostURI string) (string, error) {
	if err := ValidateHostURI(hostURI); err != nil {
		return "", err
	}
	u, err := url.Parse(hostURI)
	if err != nil {
		return "", errors.New("not a host URI identity")
	}
	return u.Host, nil
}

// URIs returns the record's URIs under the trust domain, in a fixed order:
// four, and the epoch URI last for a start epoch above FirstStartEpoch.
func (r Record) URIs(trustDomain string) []string {
	out := []string{
		EnrolmentURI(trustDomain, hex.EncodeToString(r.EnrolmentID)),
		RecordURI(trustDomain, RecordKindProvider, r.ProviderUID),
		InstallURI(trustDomain, r.InstallID),
		RecordURI(trustDomain, RecordKindMode, ModeIdentity),
	}
	if r.Epoch() > FirstStartEpoch {
		out = append(out, EpochURI(trustDomain, r.Epoch()))
	}
	return out
}

// RequestURIs returns the exact URI subject alternative names a certificate
// for the record must carry: the host URI, then the record's URIs under the
// host URI's trust domain. For a hostURI that is not a host URI identity
// the record URIs have an empty trust domain, which RecordFromURIs refuses.
func RequestURIs(hostURI string, r Record) []string {
	td, _ := TrustDomainOf(hostURI)
	return append([]string{hostURI}, r.URIs(td)...)
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
// hostURI plus one of each record URI under hostURI's trust domain, and at
// most one epoch URI, with no other and no repeated URI. The epoch URI
// names an epoch from 2, in decimal without leading zeros (one spelling per
// epoch).
func RecordFromURIs(uris []*url.URL, hostURI string) (Record, error) {
	td, err := TrustDomainOf(hostURI)
	if err != nil {
		return Record{}, fmt.Errorf("%w: the expected host URI: %v", ErrRecord, err)
	}
	if len(uris) != recordURICount && len(uris) != recordURICount+1 {
		return Record{}, fmt.Errorf("%w: %d URIs, want exactly %d, or %d with a start epoch", ErrRecord, len(uris), recordURICount,
			recordURICount+1)
	}
	recordPrefix := hostURIScheme + "://" + td + RecordPathPrefix
	var (
		rec                                   Record
		haveHost, haveEnrol, haveProv, haveIn bool
	)
	for _, u := range uris {
		if u == nil {
			return Record{}, fmt.Errorf("%w: empty URI", ErrRecord)
		}
		s := u.String()
		if s == hostURI {
			if haveHost {
				return Record{}, fmt.Errorf("%w: repeated host URI", ErrRecord)
			}
			haveHost = true
			continue
		}
		rest, ok := strings.CutPrefix(s, recordPrefix)
		if !ok {
			return Record{}, fmt.Errorf("%w: unexpected URI", ErrRecord)
		}
		kind, value, ok := strings.Cut(rest, "/")
		if !ok || strings.Contains(value, "/") {
			return Record{}, fmt.Errorf("%w: unexpected URI", ErrRecord)
		}
		switch kind {
		case RecordKindMode:
			if rec.Mode != "" {
				return Record{}, fmt.Errorf("%w: repeated mode URI", ErrRecord)
			}
			if value != ModeIdentity {
				return Record{}, fmt.Errorf("%w: mode is not %q", ErrRecord, ModeIdentity)
			}
			rec.Mode = ModeIdentity
		case RecordKindEnrolment:
			if haveEnrol {
				return Record{}, fmt.Errorf("%w: repeated enrolment URI", ErrRecord)
			}
			haveEnrol = true
			id, err := decodeLowerHexID(value)
			if err != nil {
				return Record{}, fmt.Errorf("%w: enrolment URI: %v", ErrRecord, err)
			}
			rec.EnrolmentID = id
		case RecordKindProvider:
			if haveProv {
				return Record{}, fmt.Errorf("%w: repeated provider URI", ErrRecord)
			}
			haveProv = true
			rec.ProviderUID = value
		case RecordKindEpoch:
			if rec.StartEpoch != 0 {
				return Record{}, fmt.Errorf("%w: repeated epoch URI", ErrRecord)
			}
			e, err := parseEpoch(value)
			if err != nil {
				return Record{}, fmt.Errorf("%w: epoch URI: %v", ErrRecord, err)
			}
			rec.StartEpoch = e
		case RecordKindInstall:
			if haveIn {
				return Record{}, fmt.Errorf("%w: repeated install URI", ErrRecord)
			}
			haveIn = true
			id, err := decodeLowerHexID(value)
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

// parseEpoch parses the start epoch of an epoch URI: decimal, no sign, no
// leading zero, from 2 to MaxStartEpoch (epoch 1 is written by leaving the
// URI out).
func parseEpoch(s string) (uint64, error) {
	if s == "" || s[0] == '0' || strings.TrimLeft(s, "0123456789") != "" {
		return 0, errors.New("not a decimal epoch without leading zeros")
	}
	e, err := strconv.ParseUint(s, 10, 64)
	if err != nil || e <= FirstStartEpoch || e > MaxStartEpoch {
		return 0, fmt.Errorf("not an epoch from %d to %d", FirstStartEpoch+1, uint64(MaxStartEpoch))
	}
	return e, nil
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
