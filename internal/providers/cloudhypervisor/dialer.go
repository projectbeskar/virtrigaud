//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// ErrAgentRefused is wrapped by every refusal of an agent's certificate. The
// error names the rule, never a certificate value.
var ErrAgentRefused = errors.New("agent certificate refused")

// agentTrust is what an agent of one host must prove in its TLS handshake.
type agentTrust struct {
	hostID      string
	roots       *x509.CertPool
	expectedSAN string
	pins        []string
	providerUID string
	now         func() time.Time
}

// newAgentTrust builds the trust of host from its inventory entry, which
// hostsecret.ValidateAgentHost accepted.
func newAgentTrust(h hostsecret.Host, providerUID string, now func() time.Time) (*agentTrust, error) {
	roots, err := hostidentity.RootPool(h.Credentials.AgentTLS.CABundle)
	if err != nil {
		return nil, fmt.Errorf("host %q: agent CA bundle: %w", h.ID, err)
	}
	return &agentTrust{hostID: h.ID, roots: roots, expectedSAN: h.Credentials.AgentTLS.ExpectedSAN,
		pins: slices.Clone(h.Credentials.AgentTLS.KeyPins), providerUID: providerUID, now: now}, nil
}

// verify checks the certificates an agent presented: the leaf chains to the
// host's CA bundle for server authentication at the current time; it has the
// host certificate profile; its URI subject alternative names are exactly the
// host's expected URI and one enrolment record (of this Provider, when the
// Provider knows its UID); and its key is one of the host's pins.
func (a *agentTrust) verify(chain []*x509.Certificate) error {
	if len(chain) == 0 {
		return fmt.Errorf("%w: no certificate", ErrAgentRefused)
	}
	leaf := chain[0]
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: a.roots, Intermediates: inter, CurrentTime: a.now(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("%w: it does not chain to the host's agent CA", ErrAgentRefused)
	}
	if err := hostidentity.CheckLeafProfile(leaf, a.hostID); err != nil {
		return fmt.Errorf("%w: %w", ErrAgentRefused, err)
	}
	rec, err := hostidentity.RecordFromURIs(leaf.URIs, a.expectedSAN)
	if err != nil {
		return fmt.Errorf("%w: it does not name this host and one enrolment", ErrAgentRefused)
	}
	if a.providerUID != "" && rec.ProviderUID != a.providerUID {
		return fmt.Errorf("%w: it is enrolled with another Provider", ErrAgentRefused)
	}
	pin, err := hostidentity.SPKIPin(leaf.PublicKey)
	if err != nil || !slices.Contains(a.pins, pin) {
		return fmt.Errorf("%w: its key is not pinned for this host", ErrAgentRefused)
	}
	return nil
}

// tlsConfig is the client TLS of the connection to host's agent: TLS 1.3,
// the Provider's identity certificate, and the agent's certificate checked by
// verify. The agent's certificate names no DNS name, so the standard
// name verification is replaced, not skipped: VerifyConnection runs on every
// handshake, including resumed ones.
func (a *agentTrust) tlsConfig(client tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{client},
		// #nosec G402 -- the chain, profile, URI and pin are verified in VerifyConnection.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return a.verify(cs.PeerCertificates)
		},
	}
}

// agentAddress is the dial address of a grpc://host:port endpoint, which
// hostsecret.ValidateAgentHost accepted.
func agentAddress(endpoint string) (string, error) {
	addr, ok := strings.CutPrefix(endpoint, hostsecret.AgentEndpointScheme)
	if !ok {
		return "", fmt.Errorf("endpoint is not %shost:port", hostsecret.AgentEndpointScheme)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", fmt.Errorf("endpoint is not %shost:port", hostsecret.AgentEndpointScheme)
	}
	return addr, nil
}

// dialAgent returns a lazy client connection to an agent at endpoint: nothing
// is dialed until the first call. The connection carries the call-state
// stats handler (route.go) and, when dial is set, dials through it.
func dialAgent(endpoint string, trust *agentTrust, client tls.Certificate,
	dial func(ctx context.Context, addr string) (net.Conn, error)) (*grpc.ClientConn, error) {
	addr, err := agentAddress(endpoint)
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(trust.tlsConfig(client))),
		grpc.WithStatsHandler(callStatsHandler{}),
	}
	if dial != nil {
		opts = append(opts, grpc.WithContextDialer(dial))
	}
	// passthrough: the address is dialed as given, never resolved into a
	// list the balancer could spread a host's calls over.
	cc, err := grpc.NewClient("passthrough:///"+addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("client connection: %w", err)
	}
	return cc, nil
}
