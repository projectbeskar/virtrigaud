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

package server_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
)

// TestSilentConnectionsHoldNoSlot: a source that opens connections and sends
// nothing does not use up the handshake slots: another source still
// completes a handshake, and the silent connections are closed after the
// hello timeout.
func TestSilentConnectionsHoldNoSlot(t *testing.T) {
	e := newEnvWith(t, server.Throttle{MaxHandshakes: 2, PerSourceHandshakes: 1, HelloTimeout: 300 * time.Millisecond, PerSourceBurst: 20, PerSourceRate: 100})
	var silent []net.Conn
	for i := 0; i < 8; i++ {
		c, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		silent = append(silent, c)
	}
	defer func() {
		for _, c := range silent {
			_ = c.Close()
		}
	}()
	// A second source (another loopback address) completes a handshake and a
	// signed answer while the silent connections are open.
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}}
	cc, err := grpc.NewClient(e.addr, grpc.WithTransportCredentials(credentials.NewTLS(e.clientTLS(e.provider(), tls.VersionTLS13))),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	resp, err := agentv1.NewAgentServiceClient(cc).Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(1), CallerApiVersion: 1})
	if err != nil || resp.GetHostAnswer() == nil {
		t.Fatalf("Hello from a second source = %v, %v", resp, err)
	}
	// The silent connections are closed by the server at the hello timeout.
	for _, c := range silent {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("a silent connection was not closed by the server: %v", err)
		}
	}
}

// stallConn lets the first write through (the ClientHello) and blocks the
// next ones until it is closed: a client that stalls after its hello.
type stallConn struct {
	net.Conn
	mu      sync.Mutex
	writes  int
	release chan struct{}
}

func (s *stallConn) Write(b []byte) (int, error) {
	s.mu.Lock()
	s.writes++
	n := s.writes
	s.mu.Unlock()
	if n > 1 {
		<-s.release
		return 0, net.ErrClosed
	}
	return s.Conn.Write(b)
}

func (s *stallConn) Close() error {
	s.mu.Lock()
	select {
	case <-s.release:
	default:
		close(s.release)
	}
	s.mu.Unlock()
	return s.Conn.Close()
}

// TestStalledHandshakeReleasesItsSlot: a client that sends its ClientHello
// and then stalls is cut off after HandshakeStall, not after the whole
// connection timeout, and gives its slot back, so that with every slot
// taken by stalled clients another source still gets through.
func TestStalledHandshakeReleasesItsSlot(t *testing.T) {
	e := newEnvWith(t, server.Throttle{MaxHandshakes: 1, PerSourceHandshakes: 1, HandshakeStall: 300 * time.Millisecond,
		PerSourceBurst: 20, PerSourceRate: 100})
	raw, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	sc := &stallConn{Conn: raw, release: make(chan struct{})}
	defer func() { _ = sc.Close() }()
	go func() {
		c := tls.Client(sc, &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, // #nosec G402 -- a client that stalls on purpose
			Certificates: []tls.Certificate{*e.provider()}})
		_ = c.Handshake()
	}()
	// The server closes the stalled connection within the stall bound (far
	// below the 10 s connection timeout it would otherwise wait for).
	start := time.Now()
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, err := raw.Read(make([]byte, 4096)); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal("the server did not cut off a stalled handshake")
			}
			break
		}
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("the stalled handshake held its slot for %v", el)
	}
	// The only slot is free again: another source completes a handshake.
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}}
	cc, err := grpc.NewClient(e.addr, grpc.WithTransportCredentials(credentials.NewTLS(e.clientTLS(e.provider(), tls.VersionTLS13))),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	if resp, err := agentv1.NewAgentServiceClient(cc).Hello(ctx(t), &agentv1.HelloRequest{AnswerNonce: nonce(1), CallerApiVersion: 1}); err != nil || resp.GetHostAnswer() == nil {
		t.Fatalf("Hello from another source = %v, %v", resp, err)
	}
}
