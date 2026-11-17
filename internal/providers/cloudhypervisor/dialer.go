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
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/stats"

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
	// accepted, when set, receives the expiry of every agent certificate a
	// handshake accepts.
	accepted func(notAfter time.Time)
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
	if a.accepted != nil {
		a.accepted(leaf.NotAfter)
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

// Keepalive of a connection to an agent: a ping every keepaliveTime, with
// or without calls in flight, and the connection closed when a ping is not
// answered within keepaliveTimeout, so that an agent that went silent on an
// open connection is noticed. The agent accepts pings this often.
const (
	keepaliveTime    = 20 * time.Second
	keepaliveTimeout = 10 * time.Second
)

// alertCreds wraps the client TLS credentials of an agent connection: the
// connection a handshake returns reports, through onAlert, a fatal TLS alert
// the agent sends instead of its first frame. A TLS 1.3 server verifies the
// client certificate after the client finished its handshake, so that alert
// is how an agent refuses the Provider's certificate; gRPC keeps only the
// error of the connection's last attempt, which a refused redial replaces.
// An alert after the agent sent anything (its first frame shows it accepted
// the connection) is not reported: calls on such a connection may have run.
type alertCreds struct {
	credentials.TransportCredentials
	onAlert func()
}

// ClientHandshake implements credentials.TransportCredentials. A failed
// handshake is never reported as a refusal: a TLS 1.3 server's refusal of
// the client certificate arrives after the client's handshake, on the first
// read, and an alert during the handshake (an agent whose signer is away, a
// handshake over its cap) says nothing about the certificate; gRPC then
// redials the same connection.
func (c *alertCreds) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.TransportCredentials.ClientHandshake(ctx, authority, raw)
	if err != nil {
		return nil, nil, err
	}
	return &alertConn{Conn: conn, onAlert: c.onAlert}, info, nil
}

// Clone implements credentials.TransportCredentials.
func (c *alertCreds) Clone() credentials.TransportCredentials {
	return &alertCreds{TransportCredentials: c.TransportCredentials.Clone(), onAlert: c.onAlert}
}

// alertConn is an agent connection that reports a fatal TLS alert it reads
// before any data.
type alertConn struct {
	net.Conn
	onAlert func()
	// read is set once the agent sent any data (its first frame).
	read atomic.Bool
}

// Read implements net.Conn.
func (c *alertConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.read.Store(true)
	}
	if err != nil && n == 0 && !c.read.Load() && isCertificateAlert(err) {
		c.onAlert()
	}
	return n, err
}

// certificateAlerts are the fatal TLS alerts a TLS server sends when it
// refuses the client certificate (crypto/tls's texts). Any other alert, an
// internal error above all, is not a refusal of the certificate.
var certificateAlerts = []string{
	"bad certificate", "certificate required", "unknown certificate authority", "certificate unknown", "access denied",
}

// isCertificateAlert reports whether err is a fatal TLS alert the peer sent
// to refuse the client certificate.
func isCertificateAlert(err error) bool {
	msg := err.Error()
	i := strings.Index(msg, remoteTLSAlert)
	if i < 0 {
		return false
	}
	rest := msg[i+len(remoteTLSAlert):]
	for _, a := range certificateAlerts {
		if strings.HasPrefix(rest, a) {
			return true
		}
	}
	return false
}

// dialAgent returns a lazy client connection to an agent at endpoint: nothing
// is dialed until the first call. The connection carries the stats handler
// sh (route.go), keepalive pings and, when dial is set, dials through it;
// onAlert, when set, runs when the agent ends a connection with a fatal TLS
// alert (it refused the Provider's certificate).
func dialAgent(endpoint string, trust *agentTrust, client tls.Certificate,
	dial func(ctx context.Context, addr string) (net.Conn, error), sh stats.Handler, onAlert func()) (*grpc.ClientConn, error) {
	addr, err := agentAddress(endpoint)
	if err != nil {
		return nil, err
	}
	creds := credentials.NewTLS(trust.tlsConfig(client))
	if onAlert != nil {
		creds = &alertCreds{TransportCredentials: creds, onAlert: onAlert}
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithStatsHandler(sh),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: keepaliveTime, Timeout: keepaliveTimeout, PermitWithoutStream: true}),
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
