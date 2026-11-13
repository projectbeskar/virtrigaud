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
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// ErrRequest is wrapped by every error that makes a certificate signing
// request impossible: unusable URIs or key.
var ErrRequest = errors.New("certificate signing request refused")

// ParseURIs parses the URI subject alternative names a request must carry (the
// manager's status.agent.requestURIs): exactly the host URI identity and the
// four enrolment record URIs, each once. It returns the parsed URIs and the
// host URI. Nothing but those five is accepted, so a request this package
// builds is one the manager can authorise.
func ParseURIs(uris []string) ([]*url.URL, string, error) {
	var hostURI string
	parsed := make([]*url.URL, 0, len(uris))
	for _, s := range uris {
		u, err := url.Parse(s)
		if err != nil || u.String() != s {
			return nil, "", fmt.Errorf("%w: %q is not a canonical URI", ErrRequest, s)
		}
		parsed = append(parsed, u)
		if hostidentity.ValidateHostURI(s) == nil {
			if hostURI != "" {
				return nil, "", fmt.Errorf("%w: two host URI identities", ErrRequest)
			}
			hostURI = s
		}
	}
	if hostURI == "" {
		return nil, "", fmt.Errorf("%w: no host URI identity", ErrRequest)
	}
	if _, err := hostidentity.RecordFromURIs(parsed, hostURI); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrRequest, err)
	}
	return parsed, hostURI, nil
}

// HostName returns the Host name a host URI identity names: its last path
// segment.
func HostName(hostURI string) (string, error) {
	if err := hostidentity.ValidateHostURI(hostURI); err != nil {
		return "", fmt.Errorf("%w: %v", ErrRequest, err)
	}
	return hostURI[strings.LastIndex(hostURI, "/")+1:], nil
}

// BuildCSR returns the PEM certificate signing request for the host key: its
// subject is the Host's name as the common name, its subject alternative
// names are exactly uris (see ParseURIs: the manager-published host URI and
// enrolment record), it asks for no extension besides, and it is signed with
// key, which is the proof of possession the manager checks. key must be
// ECDSA P-256, the only key type the agent's identity accepts. The caller is
// the signer's own process; the key never leaves it.
func BuildCSR(key crypto.Signer, uris []string) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("%w: no key", ErrRequest)
	}
	if pub, ok := key.Public().(*ecdsa.PublicKey); !guard.Off(guard.EnrolCSRKeyType) && (!ok || pub.Curve != elliptic.P256()) {
		return nil, fmt.Errorf("%w: the host key is not ECDSA P-256", ErrRequest)
	}
	parsed, hostURI, err := ParseURIs(uris)
	if err != nil && !guard.Off(guard.EnrolCSRURIs) {
		return nil, err
	}
	if guard.Off(guard.EnrolCSRURIs) {
		parsed, hostURI = lenientURIs(uris)
	}
	name, err := HostName(hostURI)
	if err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name},
		URIs:    parsed,
	}, key)
	if err != nil {
		return nil, fmt.Errorf("%w: sign the request: %v", ErrRequest, err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	// Check what was built the way the manager will, before anyone is asked
	// to approve it.
	csr, err := hostidentity.ParseCSR(out)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequest, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: the request's signature does not verify", ErrRequest)
	}
	return out, nil
}

// KeyPin returns the pin of a public key (hostidentity.SPKIPin): the value
// the manager pins and an administrator can compare.
func KeyPin(pub crypto.PublicKey) (string, error) {
	return hostidentity.SPKIPin(pub)
}

// lenientURIs parses uris without any check. It exists only for the mutation
// test build, which switches the URI check off.
func lenientURIs(uris []string) ([]*url.URL, string) {
	var host string
	parsed := make([]*url.URL, 0, len(uris))
	for _, s := range uris {
		if u, err := url.Parse(s); err == nil {
			parsed = append(parsed, u)
			if hostidentity.ValidateHostURI(s) == nil {
				host = s
			}
		}
	}
	return parsed, host
}
