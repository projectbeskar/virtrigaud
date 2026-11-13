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
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
)

// CheckLeafProfile checks an agent certificate against the certificate
// profile, independently of who issued it. The manager runs it on an issued
// certificate before pinning its key (ValidateIssued) and the agent runs it
// on the certificate it loads, so both sides refuse the same certificates.
// The profile: not a CA and no certificate or CRL signing key usage; the
// digital signature key usage; server authentication as the only extended key
// usage; a lifetime of at most MaxLifetime; a subject that is empty or the
// common name hostName; and no DNS, IP address or e-mail subject alternative
// name. The chain to an issuer, the URI subject alternative names and the key
// are checked by the caller.
func CheckLeafProfile(leaf *x509.Certificate, hostName string) error {
	switch {
	case leaf.IsCA:
		return errors.New("it is a CA certificate")
	case leaf.KeyUsage&(x509.KeyUsageCertSign|x509.KeyUsageCRLSign) != 0:
		return errors.New("it carries a certificate or CRL signing key usage")
	case leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0:
		return errors.New("it lacks the digital signature key usage")
	case !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth):
		return errors.New("it lacks server authentication")
	case len(leaf.ExtKeyUsage) != 1 || len(leaf.UnknownExtKeyUsage) > 0:
		return errors.New("its extended key usage is not server authentication only")
	case leaf.NotAfter.Sub(leaf.NotBefore) > MaxLifetime:
		return fmt.Errorf("its lifetime exceeds %s", MaxLifetime)
	case !subjectAllowed(leaf.Subject.Names, hostName):
		return errors.New("its subject is not empty or the host name")
	case len(leaf.DNSNames) > 0 || len(leaf.IPAddresses) > 0 || len(leaf.EmailAddresses) > 0:
		return errors.New("it carries non-URI subject alternative names")
	}
	return nil
}
