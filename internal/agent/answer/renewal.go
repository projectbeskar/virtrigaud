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

package answer

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// BodyTypeRenewalRequest is the body type of a host's signed renewal request
// (shared with the manager's verifier). It is signed under the same domain
// separator as the answers and carried only in HostAnswer.pending_renewal.
const BodyTypeRenewalRequest = hostidentity.BodyTypeRenewalRequest

// RenewalVersion is the highest renewal request version this agent
// implements.
const RenewalVersion = hostidentity.RenewalVersion

// Renewal is what a renewal request is made from.
type Renewal struct {
	// Requested is the request's renewal_version (at least 1).
	Requested uint32
	// HostID is the host identity.
	HostID string
	// Record is the enrolment record of the current certificate.
	Record hostidentity.Record
	// CertificateDER is the current certificate.
	CertificateDER []byte
	// CSRDER is the certificate request for the new key, signed by the new
	// key.
	CSRDER []byte
}

// RenewalRequest builds the RenewalRequest for r: the lower of the requested
// and the implemented version, the current certificate's enrolment, the
// digest of the current certificate and the certificate request.
func RenewalRequest(r Renewal) (*providerv1.RenewalRequest, error) {
	switch {
	case r.Requested == 0:
		return nil, fmt.Errorf("%w: no renewal request was asked for", ErrNoAnswer)
	case len(r.CSRDER) == 0 || len(r.CSRDER) > hostidentity.MaxRenewalCSRBytes:
		return nil, fmt.Errorf("%w: certificate request of %d bytes", ErrSign, len(r.CSRDER))
	case r.Record.Validate() != nil:
		return nil, fmt.Errorf("%w: the current certificate carries no valid enrolment record", ErrSign)
	}
	sum := sha256.Sum256(r.CertificateDER)
	return &providerv1.RenewalRequest{
		RenewalVersion:           min(r.Requested, RenewalVersion),
		ProviderUid:              r.Record.ProviderUID,
		HostId:                   r.HostID,
		InstallId:                bytes.Clone(r.Record.InstallID),
		EnrolmentId:              bytes.Clone(r.Record.EnrolmentID),
		CurrentCertificateSha256: sum[:],
		CertificateRequest:       bytes.Clone(r.CSRDER),
	}, nil
}

// SignRenewalRequest signs body with signer, the key of the current
// certificate certDER, under the host-signed domain separator with body type
// BodyTypeRenewalRequest: never a raw signature over the certificate request.
// The signature is verified before it is returned.
func SignRenewalRequest(signer crypto.Signer, certDER []byte, body *providerv1.RenewalRequest) (*providerv1.SignedBlob, error) {
	if body == nil {
		return nil, fmt.Errorf("%w: no renewal request", ErrSign)
	}
	return signBody(signer, certDER, BodyTypeRenewalRequest, body, hostidentity.MaxRenewalBlobBytes)
}
