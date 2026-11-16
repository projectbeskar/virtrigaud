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

package signer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// Certificate renewal, signer side (development builds only).
//
// A renewal needs a new host key. It is made and kept here, never in the
// agent process: the pending key lives beside the host key, in the signer's
// private key directory, as PendingKeyFile(KeyFile), mode 0600. The signer
// signs with it only a certificate request it builds itself from the host's
// URI identities (OpRenewalRequest): never a digest the agent chose, so that
// nothing signed with the key before it is put in use can be taken for a TLS
// handshake or a host-signed answer once it is (a certificate request starts
// with 0x30, the others with 0x20 and 0x76). OpRenewalActivate puts the
// pending key in use, replacing the host key file with one rename, so a
// crash leaves one of the two keys in the file, whole. OpRenewalDiscard
// removes a pending key that will not be used.
//
//	OpRenewalRequest   uri count byte, then each URI as a 2-byte length and its bytes
//	                                                  -> the DER certificate request
//	OpRenewalActivate  32-byte key pin                -> the PKIX DER public key now in use
//	OpRenewalDiscard   32-byte key pin                -> no body
//
// A key pin here is the SHA-256 of the key's DER SubjectPublicKeyInfo.

// Renewal operations.
const (
	// OpRenewalRequest asks for a certificate request for the pending key,
	// creating the key when there is none.
	OpRenewalRequest byte = 3
	// OpRenewalActivate puts the pending key whose pin the body names in use.
	OpRenewalActivate byte = 4
	// OpRenewalDiscard removes the pending key whose pin the body names.
	OpRenewalDiscard byte = 5
)

// Bounds of a renewal request.
const (
	// maxRenewalURIs bounds the URIs of OpRenewalRequest.
	maxRenewalURIs = 8
	// maxRenewalURIBytes bounds one URI.
	maxRenewalURIBytes = 512
	// pinSize is the size of a key pin on the wire.
	pinSize = sha256.Size
)

// Renewal budget: key generation and certificate requests are rare, so a
// small budget of their own keeps a flood of them from costing anything else.
const (
	// DefaultRenewalPerSecond is the sustained rate of renewal operations.
	DefaultRenewalPerSecond = 1.0
	// DefaultRenewalBurst is the bucket size of renewal operations.
	DefaultRenewalBurst = 10
)

// purposeRenewal is the internal budget key of renewal operations (never on
// the wire).
const purposeRenewal byte = 0xff

// pendingKeySuffix names the pending key file beside the host key file.
const pendingKeySuffix = ".pending"

// PendingKeyFile returns the path of the pending renewal key beside the host
// key file keyFile.
func PendingKeyFile(keyFile string) string { return keyFile + pendingKeySuffix }

// ErrNoRenewal is wrapped when the signer serves no renewal (a release build,
// or no key file to keep the pending key beside).
var ErrNoRenewal = errors.New("the host key signer serves no certificate renewal")

// keyPin returns the wire pin of a public key.
func keyPin(pub *ecdsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(der)
	return sum[:], nil
}

// renewalEnabled reports whether the server serves renewal operations.
func (s *Server) renewalEnabled() bool { return s.o.Renewal && s.o.KeyFile != "" }

// dispatchRenewal answers a renewal operation. It holds the renewal lock, so
// two renewal operations never interleave.
func (s *Server) dispatchRenewal(op byte, body []byte) (Status, []byte) {
	if !s.renewalEnabled() {
		s.rejected.Add(1)
		return StatusBadRequest, nil
	}
	if !guard.Off(guard.SignerRateLimit) && !s.limiters[purposeRenewal].allow(s.now()) {
		s.limited.Add(1)
		return StatusRateLimited, nil
	}
	s.renewalMu.Lock()
	defer s.renewalMu.Unlock()
	var (
		out []byte
		err error
	)
	switch op {
	case OpRenewalRequest:
		out, err = s.renewalRequest(body)
	case OpRenewalActivate:
		out, err = s.renewalActivate(body)
	default:
		out, err = nil, s.renewalDiscard(body)
	}
	switch {
	case errors.Is(err, errBadRenewal):
		s.rejected.Add(1)
		s.log.Info("renewal operation refused", "op", op, "reason", err.Error())
		return StatusBadRequest, nil
	case err != nil:
		s.log.Error(err, "renewal operation failed", "op", op)
		return StatusInternal, nil
	}
	return StatusOK, out
}

// errBadRenewal marks a renewal request the signer refuses (its caller's
// fault, not the signer's).
var errBadRenewal = errors.New("bad renewal request")

// parseRenewalURIs decodes the body of OpRenewalRequest.
func parseRenewalURIs(body []byte) ([]string, error) {
	if len(body) < 1 {
		return nil, fmt.Errorf("%w: no URI count", errBadRenewal)
	}
	n := int(body[0])
	if n == 0 || n > maxRenewalURIs {
		return nil, fmt.Errorf("%w: %d URIs", errBadRenewal, n)
	}
	rest := body[1:]
	uris := make([]string, 0, n)
	for range n {
		if len(rest) < 2 {
			return nil, fmt.Errorf("%w: truncated", errBadRenewal)
		}
		l := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if l == 0 || l > maxRenewalURIBytes || len(rest) < l {
			return nil, fmt.Errorf("%w: URI length", errBadRenewal)
		}
		uris = append(uris, string(rest[:l]))
		rest = rest[l:]
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: trailing bytes", errBadRenewal)
	}
	return uris, nil
}

// renewalRequest creates the pending key if there is none and returns a
// certificate request for it carrying exactly uris (a host URI identity and
// an enrolment record, enrol.ParseURIs), built and signed here.
func (s *Server) renewalRequest(body []byte) ([]byte, error) {
	uris, err := parseRenewalURIs(body)
	if err != nil {
		return nil, err
	}
	if _, _, err := enrol.ParseURIs(uris); err != nil && !guard.Off(guard.SignerRenewalURIs) {
		return nil, fmt.Errorf("%w: %v", errBadRenewal, err)
	}
	key, created, err := createKeyAt(PendingKeyFile(s.o.KeyFile), s.o.OwnerUID)
	if err != nil {
		return nil, fmt.Errorf("pending key: %w", err)
	}
	if cur := s.key.Load(); cur != nil && cur.priv.PublicKey.Equal(&key.PublicKey) {
		// Never certify the key in use again as a renewal.
		return nil, errors.New("the pending key is the key in use")
	}
	if created {
		s.log.Info("renewal key created", "file", filepath.Base(PendingKeyFile(s.o.KeyFile)))
	}
	csrPEM, err := enrol.BuildCSR(key, uris)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRenewal, err)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || len(block.Bytes) > hostidentity.MaxRenewalCSRBytes {
		return nil, errors.New("the certificate request is too large")
	}
	return block.Bytes, nil
}

// renewalActivate puts the pending key whose pin the body names in use: the
// pending key file is renamed over the host key file (one rename: a crash
// leaves either key there, whole), the directory is synced, and the key is
// swapped in. A body naming the key already in use succeeds and changes
// nothing, so a repeated activation is harmless.
func (s *Server) renewalActivate(body []byte) ([]byte, error) {
	if len(body) != pinSize {
		return nil, fmt.Errorf("%w: a key pin is %d bytes", errBadRenewal, pinSize)
	}
	if cur := s.key.Load(); cur != nil {
		if pin, err := keyPin(&cur.priv.PublicKey); err == nil && bytes.Equal(pin, body) {
			return cur.der, nil
		}
	}
	path := PendingKeyFile(s.o.KeyFile)
	key, err := LoadKey(path, s.o.OwnerUID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: no pending key", errBadRenewal)
		}
		return nil, fmt.Errorf("pending key: %w", err)
	}
	pin, err := keyPin(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(pin, body) && !guard.Off(guard.SignerActivatePin) {
		return nil, fmt.Errorf("%w: the pending key is not the one named", errBadRenewal)
	}
	if err := os.Rename(path, s.o.KeyFile); err != nil {
		return nil, fmt.Errorf("put the pending key in use: %w", err)
	}
	if err := syncDir(filepath.Dir(s.o.KeyFile)); err != nil {
		return nil, fmt.Errorf("sync the key directory: %w", err)
	}
	if err := s.SetKey(key); err != nil {
		return nil, err
	}
	s.log.Info("renewal key put in use")
	return s.key.Load().der, nil
}

// renewalDiscard removes the pending key when its pin is the one the body
// names; no pending key, or another one, is left as it is and succeeds.
func (s *Server) renewalDiscard(body []byte) error {
	if len(body) != pinSize {
		return fmt.Errorf("%w: a key pin is %d bytes", errBadRenewal, pinSize)
	}
	path := PendingKeyFile(s.o.KeyFile)
	key, err := LoadKey(path, s.o.OwnerUID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("pending key: %w", err)
	}
	pin, err := keyPin(&key.PublicKey)
	if err != nil || !bytes.Equal(pin, body) {
		return nil //nolint:nilerr // not the key named: nothing to discard
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the pending key: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync the key directory: %w", err)
	}
	s.log.Info("renewal key discarded")
	return nil
}

// RenewalRequest asks the signer for a certificate request for its pending
// renewal key (created when there is none) carrying exactly uris. The
// returned request is checked: it parses, its signature verifies, it is not
// for the key in use, and it carries exactly uris.
func (c *Client) RenewalRequest(ctx context.Context, uris []string) ([]byte, error) {
	if len(uris) == 0 || len(uris) > maxRenewalURIs {
		return nil, fmt.Errorf("%w: %d URIs", ErrSigner, len(uris))
	}
	req := []byte{Version, OpRenewalRequest, byte(len(uris))} // #nosec G115 -- 1..maxRenewalURIs (8), checked above
	for _, u := range uris {
		if len(u) == 0 || len(u) > maxRenewalURIBytes {
			return nil, fmt.Errorf("%w: a URI of %d bytes", ErrSigner, len(u))
		}
		req = binary.BigEndian.AppendUint16(req, uint16(len(u))) // #nosec G115 -- bounded above
		req = append(req, u...)
	}
	der, err := c.call(ctx, req)
	if err != nil {
		return nil, err
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || !bytes.Equal(csr.Raw, der) || csr.CheckSignature() != nil {
		return nil, fmt.Errorf("%w: the signer answered with an unusable certificate request", ErrSigner)
	}
	if pub, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || pub.Equal(c.pub) {
		return nil, fmt.Errorf("%w: the certificate request is not for a new ECDSA key", ErrSigner)
	}
	got := make([]string, 0, len(csr.URIs))
	for _, u := range csr.URIs {
		got = append(got, u.String())
	}
	if !sameStrings(got, uris) {
		return nil, fmt.Errorf("%w: the certificate request carries other URIs", ErrSigner)
	}
	return der, nil
}

// ActivateRenewal asks the signer to put in use the pending key whose pin
// (SHA-256 of its DER SubjectPublicKeyInfo) is pin, and returns the public
// key the signer then uses, which must be that key. A Client's own public
// key does not change: the caller connects a new Client for the new key.
func (c *Client) ActivateRenewal(ctx context.Context, pin []byte) (*ecdsa.PublicKey, error) {
	if len(pin) != pinSize {
		return nil, fmt.Errorf("%w: a key pin is %d bytes", ErrSigner, pinSize)
	}
	body, err := c.call(ctx, append([]byte{Version, OpRenewalActivate}, pin...))
	if err != nil {
		return nil, err
	}
	pub, err := parsePublicKey(body)
	if err != nil {
		return nil, err
	}
	got, err := keyPin(pub)
	if err != nil || !bytes.Equal(got, pin) {
		return nil, fmt.Errorf("%w: the signer did not put the named key in use", ErrSigner)
	}
	return pub, nil
}

// DiscardRenewal asks the signer to remove its pending key when its pin is
// pin.
func (c *Client) DiscardRenewal(ctx context.Context, pin []byte) error {
	if len(pin) != pinSize {
		return fmt.Errorf("%w: a key pin is %d bytes", ErrSigner, pinSize)
	}
	_, err := c.call(ctx, append([]byte{Version, OpRenewalDiscard}, pin...))
	return err
}

// sameStrings reports whether a and b are equal, element by element.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
