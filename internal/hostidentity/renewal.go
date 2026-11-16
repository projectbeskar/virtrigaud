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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Certificate renewal. A per-host agent renews its identity certificate for a
// new key it generates on the host: it builds a PKCS #10 request for the new
// key (self-signed by it, the proof of possession of the new key) and wraps
// it in a RenewalRequest that it signs, under the host-signed domain
// separator, with the key of its current certificate (the proof that the
// holder of the pinned key asks for the renewal). The request travels in the
// signed answer of a GetHostInfo that asked for it. The manager verifies it
// here, then (in package controller) asks the host-agent issuer for the
// certificate under its own identity.

// BodyTypeRenewalRequest is the body type of a host's signed renewal
// request: the protobuf full name of RenewalRequest. It is carried only in
// HostAnswer.pending_renewal.
const BodyTypeRenewalRequest = "provider.v1.RenewalRequest"

// RenewalVersion is the version of RenewalRequest this module implements: a
// manager sends it as GetHostInfoRequest.renewal_version, and an agent uses
// the lower of the request's and its own.
const RenewalVersion uint32 = 1

// Size bounds of a renewal request.
const (
	// MaxRenewalCSRBytes bounds RenewalRequest.certificate_request (DER).
	MaxRenewalCSRBytes = 4096
	// MaxRenewalBodyBytes bounds the encoded RenewalRequest.
	MaxRenewalBodyBytes = 8 << 10
	// MaxRenewalBlobBytes bounds the encoded SignedBlob that carries it.
	MaxRenewalBlobBytes = 18 << 10
)

// ErrRenewal is wrapped by every refusal of a renewal request.
var ErrRenewal = errors.New("renewal request refused")

// refuseRenewal wraps ErrRenewal with what failed.
func refuseRenewal(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRenewal, fmt.Sprintf(format, args...))
}

// RenewalExpectation is what a verifier expects of a renewal request: all of
// it from the manager's own state for the host.
type RenewalExpectation struct {
	// Anchors are the host-agent subordinate CA certificates.
	Anchors *x509.CertPool
	// HostURI is the host's URI identity.
	HostURI string
	// HostName is the Host's name.
	HostName string
	// HostID is the host identity the request must name.
	HostID string
	// Pins are the host's pinned agent keys: the request must be signed by
	// one of them.
	Pins []string
	// ProviderUID is the Provider's metadata.uid.
	ProviderUID string
	// InstallID is the manager installation's 16-byte identifier.
	InstallID []byte
	// EnrolmentID is the host's current 16-byte enrolment identifier.
	EnrolmentID []byte
	// Version is the renewal_version the request was asked for with (at
	// least 1).
	Version uint32
	// Now is the manager's clock: the signer certificate (the host's current
	// certificate) must be valid at it.
	Now time.Time
}

// VerifiedRenewal is a renewal request that passed VerifyRenewal.
type VerifiedRenewal struct {
	// Blob is the deterministic encoding of the SignedBlob, as the manager
	// stores it to verify again later.
	Blob []byte
	// CSR is the DER certificate signing request for the new key.
	CSR []byte
	// KeyPin is the pin of the new key.
	KeyPin string
	// SignerPin is the pin of the current key that signed the request.
	SignerPin string
	// SignerCertificate is the host's current certificate (the signer's).
	SignerCertificate *x509.Certificate
	// Record is the enrolment record the request names (the current
	// certificate's and the new request's).
	Record Record
}

// VerifyRenewal verifies a renewal request against exp, in this order, each
// failure refusing it: sizes; no unknown field in the SignedBlob; the body
// type is BodyTypeRenewalRequest; the signer certificate has the agent
// certificate profile, carries exactly the host URI and one enrolment record,
// chains to exp.Anchors for server authentication at exp.Now (the current
// certificate is valid) and its key is one of exp.Pins; the signature over
// AnswerSignatureInput verifies (low-S, exact DER); the body decodes with no
// unknown field, canonically encoded; renewal_version is the lower of
// exp.Version and RenewalVersion; it names exp's Provider, host,
// installation and enrolment, and the certificate's; it names the digest of
// the signer certificate; and its certificate request passes
// CheckRenewalCSR: a new ECDSA P-256 key, self-signed, carrying exactly the
// signer certificate's URI identities (same host, same enrolment record, same
// epoch). Nothing in the body is decoded before the signature verifies.
func VerifyRenewal(blob *providerv1.SignedBlob, exp RenewalExpectation) (VerifiedRenewal, error) {
	if blob == nil {
		return VerifiedRenewal{}, refuseRenewal("no renewal request")
	}
	switch {
	case blob.GetBodyType() != BodyTypeRenewalRequest:
		return VerifiedRenewal{}, refuseRenewal("body type %q is not %q", blob.GetBodyType(), BodyTypeRenewalRequest)
	case len(blob.GetBody()) == 0 || len(blob.GetBody()) > MaxRenewalBodyBytes:
		return VerifiedRenewal{}, refuseRenewal("body size")
	case len(blob.GetSignature()) == 0 || len(blob.GetSignature()) > MaxAnswerSignatureBytes:
		return VerifiedRenewal{}, refuseRenewal("signature size")
	case len(blob.GetSignerCertificate()) == 0 || len(blob.GetSignerCertificate()) > MaxAnswerCertBytes:
		return VerifiedRenewal{}, refuseRenewal("signer certificate size")
	case proto.Size(blob) > MaxRenewalBlobBytes:
		return VerifiedRenewal{}, refuseRenewal("signed request size")
	case len(blob.ProtoReflect().GetUnknown()) != 0:
		return VerifiedRenewal{}, refuseRenewal("the signed request carries unknown fields")
	case exp.Version == 0:
		return VerifiedRenewal{}, refuseRenewal("no renewal request was asked for")
	}
	leaf, err := x509.ParseCertificate(blob.GetSignerCertificate())
	if err != nil {
		return VerifiedRenewal{}, refuseRenewal("the signer certificate is not one DER certificate")
	}
	pub, rec, signerPin, err := verifySigner(blob.GetSignerCertificate(), AnswerExpectation{
		Anchors: exp.Anchors, HostURI: exp.HostURI, HostName: exp.HostName, Pins: exp.Pins, Now: exp.Now,
	})
	if err != nil {
		return VerifiedRenewal{}, refuseRenewal("%v", err)
	}
	if err := verifyAnswerSignature(pub, blob); err != nil {
		return VerifiedRenewal{}, refuseRenewal("%v", err)
	}
	body := &providerv1.RenewalRequest{}
	if err := proto.Unmarshal(blob.GetBody(), body); err != nil {
		return VerifiedRenewal{}, refuseRenewal("the body does not decode as %s", BodyTypeRenewalRequest)
	}
	if err := checkMessage(body.ProtoReflect()); err != nil {
		return VerifiedRenewal{}, refuseRenewal("%v", err)
	}
	if canon, err := (proto.MarshalOptions{Deterministic: true}).Marshal(body); err != nil || !bytes.Equal(canon, blob.GetBody()) {
		return VerifiedRenewal{}, refuseRenewal("the body is not canonically encoded")
	}
	certSum := sha256.Sum256(blob.GetSignerCertificate())
	switch {
	case body.GetRenewalVersion() != min(exp.Version, RenewalVersion):
		return VerifiedRenewal{}, refuseRenewal("renewal version %d, want %d", body.GetRenewalVersion(), min(exp.Version, RenewalVersion))
	case body.GetProviderUid() != exp.ProviderUID || rec.ProviderUID != exp.ProviderUID:
		return VerifiedRenewal{}, refuseRenewal("the request names another Provider")
	case body.GetHostId() != exp.HostID:
		return VerifiedRenewal{}, refuseRenewal("the request names another host")
	case !bytes.Equal(body.GetInstallId(), exp.InstallID) || !bytes.Equal(rec.InstallID, exp.InstallID):
		return VerifiedRenewal{}, refuseRenewal("the request names another installation")
	case !bytes.Equal(body.GetEnrolmentId(), exp.EnrolmentID) || !bytes.Equal(rec.EnrolmentID, exp.EnrolmentID):
		return VerifiedRenewal{}, refuseRenewal("the request names another enrolment")
	case !bytes.Equal(body.GetCurrentCertificateSha256(), certSum[:]):
		return VerifiedRenewal{}, refuseRenewal("the request does not name the certificate that signed it")
	}
	csr, keyPin, err := CheckRenewalCSR(body.GetCertificateRequest(), exp.HostName, exp.HostURI, rec)
	if err != nil {
		return VerifiedRenewal{}, err
	}
	if keyPin == signerPin || slices.Contains(exp.Pins, keyPin) {
		return VerifiedRenewal{}, refuseRenewal("the request's key is already pinned for the host")
	}
	enc, err := proto.MarshalOptions{Deterministic: true}.Marshal(blob)
	if err != nil {
		return VerifiedRenewal{}, refuseRenewal("encode the signed request")
	}
	return VerifiedRenewal{Blob: enc, CSR: csr.Raw, KeyPin: keyPin, SignerPin: signerPin, SignerCertificate: leaf, Record: rec}, nil
}

// CheckRenewalCSR checks the certificate request of a renewal: one DER
// PKCS #10 request of at most MaxRenewalCSRBytes with nothing after it, whose
// signature verifies with its own key (proof of possession), for an ECDSA
// P-256 key, asking for no extension but the subject alternative names (and
// basic constraints without the CA bit), with an empty subject or the common
// name hostName, no DNS, address or e-mail name, and URI names that are
// exactly hostURI and record (same enrolment, same epoch). It returns the
// parsed request and its key pin.
func CheckRenewalCSR(der []byte, hostName, hostURI string, record Record) (*x509.CertificateRequest, string, error) {
	if len(der) == 0 || len(der) > MaxRenewalCSRBytes {
		return nil, "", refuseRenewal("the certificate request is empty or larger than %d bytes", MaxRenewalCSRBytes)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, "", refuseRenewal("the certificate request does not parse as one DER request")
	}
	if !bytes.Equal(csr.Raw, der) {
		return nil, "", refuseRenewal("the certificate request carries data after it")
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", refuseRenewal("the certificate request's signature does not verify with its key")
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, "", refuseRenewal("the new key is not ECDSA P-256")
	}
	if err := checkExtensions(csr); err != nil {
		return nil, "", refuseRenewal("%v", err)
	}
	if !subjectAllowed(csr.Subject.Names, hostName) {
		return nil, "", refuseRenewal("the subject is not empty or the host name")
	}
	if len(csr.DNSNames) > 0 || len(csr.IPAddresses) > 0 || len(csr.EmailAddresses) > 0 {
		return nil, "", refuseRenewal("only URI subject alternative names are allowed")
	}
	rec, err := RecordFromURIs(csr.URIs, hostURI)
	if err != nil {
		return nil, "", refuseRenewal("the URI names are not the host's: %v", err)
	}
	if !rec.Equal(record) {
		return nil, "", refuseRenewal("the URI names change the enrolment record")
	}
	pin, err := SPKIPin(pub)
	if err != nil {
		return nil, "", refuseRenewal("%v", err)
	}
	return csr, pin, nil
}

// RenewalRequestSpec is what the issuer request of a renewal asks for, as the
// issuer API presents it.
type RenewalRequestSpec struct {
	// CSRPEM is the PEM certificate signing request the issuer request carries.
	CSRPEM []byte
	// Usages are the requested usages.
	Usages []string
	// Duration is the requested lifetime.
	Duration time.Duration
	// IsCA reports whether a CA certificate is requested.
	IsCA bool
}

// CheckRenewalRequestSpec checks what an issuer request of a renewal asks
// for against the verified renewal: its certificate request is exactly the
// verified one, usages are those of an agent certificate (server
// authentication and digital signature, key encipherment tolerated), it asks
// for no CA, and its lifetime is set and at most MaxLifetime. It returns a
// *Denial naming the first rule broken.
func CheckRenewalRequestSpec(s RenewalRequestSpec, verified VerifiedRenewal) error {
	if s.IsCA {
		return deny(ReasonCARequested, "a CA certificate is requested")
	}
	if err := checkUsages(s.Usages); err != nil {
		return err
	}
	if s.Duration <= 0 || s.Duration > MaxLifetime {
		return deny(ReasonLifetime, "the requested lifetime must be set and at most %s", MaxLifetime)
	}
	csr, err := ParseCSR(s.CSRPEM)
	if err != nil {
		return deny(ReasonRequestInvalid, "%v", err)
	}
	if !bytes.Equal(csr.Raw, verified.CSR) {
		return deny(ReasonRequestInvalid, "the certificate request is not the one the host's signed renewal request carries")
	}
	return nil
}

// RenewalLifetime returns the lifetime a renewal asks for: the current
// certificate's, at most MaxLifetime and at least one hour, in whole hours.
func RenewalLifetime(current *x509.Certificate) time.Duration {
	d := current.NotAfter.Sub(current.NotBefore).Truncate(time.Hour)
	return min(max(d, time.Hour), MaxLifetime)
}

// CertificateSHA256 returns the hex SHA-256 digest of a DER certificate.
func CertificateSHA256(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}
