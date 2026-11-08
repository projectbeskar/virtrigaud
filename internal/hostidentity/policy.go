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
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"
)

// MaxLifetime is the longest agent certificate lifetime the manager
// authorises.
const MaxLifetime = 30 * 24 * time.Hour

// Certificate usages, spelled as cert-manager's CertificateRequest
// spec.usages spells them.
const (
	// UsageClientAuth is the TLS client authentication usage.
	UsageClientAuth = "client auth"
	// UsageServerAuth is the TLS server authentication usage.
	UsageServerAuth = "server auth"
	// UsageDigitalSignature is the digital signature key usage.
	UsageDigitalSignature = "digital signature"
	// UsageKeyEncipherment is the key encipherment key usage.
	UsageKeyEncipherment = "key encipherment"
)

// Denial reasons. Each is a condition reason (CamelCase, no spaces) and names
// one authorisation rule a request broke.
const (
	// ReasonRequesterNotAuthorised: the requester is not in the enrolment
	// administrator group.
	ReasonRequesterNotAuthorised = "RequesterNotAuthorised"
	// ReasonRequestInvalid: the request is not a single PEM certificate
	// signing request.
	ReasonRequestInvalid = "RequestInvalid"
	// ReasonProofOfPossession: the request's signature does not verify with
	// its own public key.
	ReasonProofOfPossession = "ProofOfPossessionFailed"
	// ReasonKeyUnsupported: the key type or size is not accepted.
	ReasonKeyUnsupported = "KeyUnsupported"
	// ReasonSubjectAltNames: the subject alternative names are not exactly
	// the host URI and the pending enrolment record.
	ReasonSubjectAltNames = "SubjectAltNamesRefused"
	// ReasonSubject: the subject carries more than the host name.
	ReasonSubject = "SubjectRefused"
	// ReasonExtension: the request asks for an extension other than the
	// subject alternative names.
	ReasonExtension = "ExtensionRefused"
	// ReasonCARequested: the request asks for a CA certificate.
	ReasonCARequested = "CARequested"
	// ReasonUsages: the usages are not client and server authentication.
	ReasonUsages = "UsagesRefused"
	// ReasonLifetime: no lifetime, or one longer than MaxLifetime.
	ReasonLifetime = "LifetimeRefused"
)

var (
	// oidSubjectAltName and oidBasicConstraints are the only extensions a
	// request may ask for (the latter only with the CA bit clear).
	oidSubjectAltName   = asn1.ObjectIdentifier{2, 5, 29, 17}
	oidBasicConstraints = asn1.ObjectIdentifier{2, 5, 29, 19}
	// oidCommonName is the only subject attribute a request may carry.
	oidCommonName = asn1.ObjectIdentifier{2, 5, 4, 3}
)

// Denial is the error Authorise returns for a request it refuses: a
// condition reason and a message. The message names the rule, never key
// material or the request's bytes.
type Denial struct {
	// Reason is one of the Reason* constants.
	Reason string
	// Message explains the refusal.
	Message string
}

// Error implements error.
func (d *Denial) Error() string { return d.Reason + ": " + d.Message }

// deny returns a Denial.
func deny(reason, format string, args ...any) *Denial {
	return &Denial{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// AsDenial returns the Denial err wraps, if any.
func AsDenial(err error) (*Denial, bool) {
	var d *Denial
	ok := errors.As(err, &d)
	return d, ok
}

// Request is a certificate request as the issuer API presents it.
type Request struct {
	// CSRPEM is the PEM-encoded PKCS #10 certificate signing request.
	CSRPEM []byte
	// Usages are the requested usages (UsageClientAuth, ...).
	Usages []string
	// Duration is the requested lifetime; zero means none was requested.
	Duration time.Duration
	// IsCA reports whether a CA certificate is requested.
	IsCA bool
	// Username is the authenticated requester, as the API server recorded it.
	Username string
	// Groups are the authenticated requester's groups, as the API server
	// recorded them.
	Groups []string
	// UID is the authenticated requester's UID, as the API server recorded
	// it.
	UID string
	// Extra is the authenticated requester's extra attributes, as the API
	// server recorded them.
	Extra map[string][]string
}

// Policy is what a request for one host must match.
type Policy struct {
	// AdminGroup is the group a requester must belong to.
	AdminGroup string
	// HostName is the Host's name; the only subject a request may carry is
	// a common name equal to it.
	HostName string
	// HostURI is the Host's URI identity.
	HostURI string
	// Record is the pending enrolment record the manager drew for the host.
	Record Record
}

// Authorisation is what the manager authorised for one request: the host,
// the record and the key. An issued certificate is pinned only if it
// matches it (ValidateIssued).
type Authorisation struct {
	// HostName is the Host's name; the only subject an issued certificate
	// may carry is a common name equal to it.
	HostName string
	// HostURI is the Host's URI identity.
	HostURI string
	// Record is the enrolment record.
	Record Record
	// KeyPin is the pin of the request's public key.
	KeyPin string
}

// Authorise checks a request against the policy and returns the
// authorisation, or a *Denial naming the first rule it breaks. The rules: the
// requester is in the administrator group; the request is one PEM CSR whose
// signature verifies (proof of possession) over an accepted key; its subject
// alternative names are exactly the host URI and the pending record; its
// subject is empty or the host name; it asks for no extension but those and
// no CA; usages are client and server authentication (digital signature and
// key encipherment are tolerated); a lifetime is requested and is at most
// MaxLifetime.
func Authorise(p Policy, req Request) (Authorisation, error) {
	if p.AdminGroup == "" || !slices.Contains(req.Groups, p.AdminGroup) {
		return Authorisation{}, deny(ReasonRequesterNotAuthorised,
			"the requester is not in the enrolment administrator group")
	}
	if req.IsCA {
		return Authorisation{}, deny(ReasonCARequested, "a CA certificate is requested")
	}
	if err := checkUsages(req.Usages); err != nil {
		return Authorisation{}, err
	}
	if req.Duration <= 0 || req.Duration > MaxLifetime {
		return Authorisation{}, deny(ReasonLifetime, "the requested lifetime must be set and at most %s", MaxLifetime)
	}
	csr, err := ParseCSR(req.CSRPEM)
	if err != nil {
		return Authorisation{}, deny(ReasonRequestInvalid, "%v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return Authorisation{}, deny(ReasonProofOfPossession, "the request's signature does not verify with its public key")
	}
	if err := checkKey(csr.PublicKey); err != nil {
		return Authorisation{}, err
	}
	if err := checkExtensions(csr); err != nil {
		return Authorisation{}, err
	}
	if err := checkSubject(csr, p.HostName); err != nil {
		return Authorisation{}, err
	}
	if len(csr.DNSNames) > 0 || len(csr.IPAddresses) > 0 || len(csr.EmailAddresses) > 0 {
		return Authorisation{}, deny(ReasonSubjectAltNames, "only URI subject alternative names are allowed")
	}
	rec, err := RecordFromURIs(csr.URIs, p.HostURI)
	if err != nil {
		return Authorisation{}, deny(ReasonSubjectAltNames, "%v", err)
	}
	if !rec.Equal(p.Record) {
		return Authorisation{}, deny(ReasonSubjectAltNames, "the enrolment record does not match the host's pending enrolment")
	}
	pin, err := SPKIPin(csr.PublicKey)
	if err != nil {
		return Authorisation{}, deny(ReasonKeyUnsupported, "%v", err)
	}
	return Authorisation{HostName: p.HostName, HostURI: p.HostURI, Record: rec, KeyPin: pin}, nil
}

// ParseCSR parses exactly one PEM "CERTIFICATE REQUEST" block with nothing
// but whitespace around it.
func ParseCSR(data []byte) (*x509.CertificateRequest, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("the request is not a PEM certificate signing request")
	}
	if len(trimSpace(rest)) != 0 {
		return nil, errors.New("the request carries data after the certificate signing request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, errors.New("the certificate signing request does not parse")
	}
	return csr, nil
}

// trimSpace trims ASCII whitespace.
func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\r' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

// checkUsages requires client and server authentication and tolerates only
// digital signature and key encipherment besides.
func checkUsages(usages []string) error {
	var client, server bool
	for _, u := range usages {
		switch u {
		case UsageClientAuth:
			client = true
		case UsageServerAuth:
			server = true
		case UsageDigitalSignature, UsageKeyEncipherment:
		default:
			return deny(ReasonUsages, "usage %q is not allowed", u)
		}
	}
	if !client || !server {
		return deny(ReasonUsages, "usages must include %q and %q", UsageClientAuth, UsageServerAuth)
	}
	return nil
}

// checkKey accepts ECDSA P-256 and P-384, Ed25519, and RSA of 2048 to 8192
// bits.
func checkKey(pub any) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve == elliptic.P256() || k.Curve == elliptic.P384() {
			return nil
		}
	case ed25519.PublicKey:
		return nil
	case *rsa.PublicKey:
		if n := k.N.BitLen(); n >= 2048 && n <= 8192 {
			return nil
		}
	}
	return deny(ReasonKeyUnsupported, "the key must be ECDSA P-256 or P-384, Ed25519, or RSA of 2048 to 8192 bits")
}

// checkExtensions refuses every requested extension but the subject
// alternative names, and basic constraints with the CA bit clear.
func checkExtensions(csr *x509.CertificateRequest) error {
	for _, ext := range csr.Extensions {
		switch {
		case ext.Id.Equal(oidSubjectAltName):
		case ext.Id.Equal(oidBasicConstraints):
			var bc struct {
				IsCA       bool `asn1:"optional"`
				MaxPathLen int  `asn1:"optional,default:-1"`
			}
			rest, err := asn1.Unmarshal(ext.Value, &bc)
			if err != nil || len(rest) != 0 {
				return deny(ReasonExtension, "the basic constraints extension does not parse")
			}
			if bc.IsCA {
				return deny(ReasonCARequested, "the request asks for the CA bit")
			}
		default:
			return deny(ReasonExtension, "extension %s is not allowed", ext.Id)
		}
	}
	return nil
}

// checkSubject allows an empty subject or a lone common name equal to the
// host name.
func checkSubject(csr *x509.CertificateRequest, hostName string) error {
	if !subjectAllowed(csr.Subject.Names, hostName) {
		return deny(ReasonSubject, "the subject may only be a common name equal to the host name")
	}
	return nil
}

// subjectAllowed reports whether a subject is empty or a lone common name
// equal to the host name.
func subjectAllowed(names []pkix.AttributeTypeAndValue, hostName string) bool {
	for _, atv := range names {
		v, ok := atv.Value.(string)
		if !atv.Type.Equal(oidCommonName) || !ok || v != hostName {
			return false
		}
	}
	return true
}
