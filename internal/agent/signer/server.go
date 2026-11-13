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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
)

// DefaultLogInterval is the period of the signer's activity summary.
const DefaultLogInterval = time.Minute

// ServerOptions configures a Server.
type ServerOptions struct {
	// Key is the host key. Required; ECDSA P-256.
	Key *ecdsa.PrivateKey
	// AllowedUID is the only uid served: the agent's.
	AllowedUID uint32
	// Log receives structured events. Digests, signatures and key material
	// are never logged; only counts are.
	Log logr.Logger
	// SignsPerSecond and SignBurst set the rate limit (DefaultSignsPerSecond
	// and DefaultSignBurst when zero).
	SignsPerSecond float64
	// SignBurst is the rate limit's burst size.
	SignBurst int
	// MaxConns bounds the connections served at once (DefaultMaxConns when
	// zero); a connection above it is closed at once.
	MaxConns int
	// IOTimeout bounds the work on one connection (DefaultIOTimeout when
	// zero).
	IOTimeout time.Duration
	// LogInterval is the period of the activity summary
	// (DefaultLogInterval when zero).
	LogInterval time.Duration
}

// keyPair is the key and its encoded public half.
type keyPair struct {
	priv *ecdsa.PrivateKey
	der  []byte
}

// Server is the signer: it serves the host key over a Unix socket to one
// uid, as a digest oracle (see the package documentation).
type Server struct {
	o       ServerOptions
	log     logr.Logger
	key     atomic.Pointer[keyPair]
	limiter *bucket
	now     func() time.Time
	// peerUID reads a connection's peer uid (PeerUID; tests replace it).
	peerUID func(net.Conn) (uint32, error)

	signed, limited, rejected, denied, overloaded atomic.Uint64
}

// NewServer returns a Server for o.
func NewServer(o ServerOptions) (*Server, error) {
	if o.SignsPerSecond == 0 {
		o.SignsPerSecond = DefaultSignsPerSecond
	}
	if o.SignBurst == 0 {
		o.SignBurst = DefaultSignBurst
	}
	if o.SignsPerSecond < 0 || o.SignBurst < 1 {
		return nil, errors.New("the signing rate limit must be positive")
	}
	if o.MaxConns == 0 {
		o.MaxConns = DefaultMaxConns
	}
	if o.IOTimeout == 0 {
		o.IOTimeout = DefaultIOTimeout
	}
	if o.LogInterval == 0 {
		o.LogInterval = DefaultLogInterval
	}
	s := &Server{o: o, log: o.Log, now: time.Now, peerUID: PeerUID}
	if err := s.SetKey(o.Key); err != nil {
		return nil, err
	}
	s.limiter = newBucket(o.SignsPerSecond, o.SignBurst, s.now())
	return s, nil
}

// SetKey replaces the host key (a reload); calls in flight finish with the
// old one. The key must be ECDSA P-256.
func (s *Server) SetKey(k *ecdsa.PrivateKey) error {
	if k == nil || k.Curve != elliptic.P256() {
		return errors.New("the host key must be an ECDSA P-256 private key")
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return fmt.Errorf("encode the public key: %w", err)
	}
	s.key.Store(&keyPair{priv: k, der: der})
	return nil
}

// Serve serves connections from lis until ctx ends or lis fails. It closes
// lis and returns after every connection is done; a stop returns nil. Every
// goroutine it starts ends with ctx or with its connection's deadline, and at
// most MaxConns run at once.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = lis.Close() })
	defer stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.summaries(ctx)
	}()
	sem := make(chan struct{}, s.o.MaxConns)
	var err error
	for {
		conn, aerr := lis.Accept()
		if aerr != nil {
			if ctx.Err() == nil {
				err = fmt.Errorf("accept: %w", aerr)
			}
			break
		}
		select {
		case sem <- struct{}{}:
		default:
			s.overloaded.Add(1)
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.handle(conn)
		}()
	}
	cancel()
	_ = lis.Close()
	wg.Wait()
	s.logTotals("signer stopped")
	return err
}

// handle serves one connection: one request, one response.
func (s *Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(s.now().Add(s.o.IOTimeout)); err != nil {
		return
	}
	if !guard.Off(guard.SignerPeerCheck) {
		uid, err := s.peerUID(conn)
		if err == nil {
			err = CheckPeer(uid, s.o.AllowedUID)
		}
		if err != nil {
			s.denied.Add(1)
			s.log.V(1).Info("connection refused", "reason", "peer")
			return
		}
	}
	req, err := readFrame(conn)
	if err != nil {
		s.rejected.Add(1)
		return
	}
	status, body := s.dispatch(req)
	if err := writeFrame(conn, append([]byte{Version, byte(status)}, body...)); err != nil {
		s.log.V(1).Info("response not delivered")
	}
}

// dispatch answers one request. It refuses anything it does not know.
func (s *Server) dispatch(req []byte) (Status, []byte) {
	if len(req) < 2 || req[0] != Version {
		s.rejected.Add(1)
		return StatusBadRequest, nil
	}
	kp := s.key.Load()
	switch req[1] {
	case OpPublicKey:
		if len(req) != 2 {
			s.rejected.Add(1)
			return StatusBadRequest, nil
		}
		return StatusOK, kp.der
	case OpSign:
		if len(req) < 3 {
			s.rejected.Add(1)
			return StatusBadRequest, nil
		}
		h, ok := signHashes[req[2]]
		digest := req[3:]
		if !guard.Off(guard.SignerDigestCheck) && (!ok || len(digest) != h.size) {
			s.rejected.Add(1)
			return StatusBadRequest, nil
		}
		if !guard.Off(guard.SignerRateLimit) && !s.limiter.allow(s.now()) {
			s.limited.Add(1)
			return StatusRateLimited, nil
		}
		sig, err := ecdsa.SignASN1(rand.Reader, kp.priv, digest)
		if err != nil {
			s.log.Error(errors.New("signing failed"), "sign")
			return StatusInternal, nil
		}
		s.signed.Add(1)
		return StatusOK, sig
	}
	s.rejected.Add(1)
	return StatusBadRequest, nil
}

// summaries logs the counters every LogInterval while they change, and
// nothing else about what was signed.
func (s *Server) summaries(ctx context.Context) {
	t := time.NewTicker(s.o.LogInterval)
	defer t.Stop()
	var last [5]uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := [5]uint64{s.signed.Load(), s.limited.Load(), s.rejected.Load(), s.denied.Load(), s.overloaded.Load()}
			if cur == last {
				continue
			}
			s.log.Info("signer activity", "windowSeconds", int(s.o.LogInterval.Seconds()),
				"signed", cur[0]-last[0], "rateLimited", cur[1]-last[1], "rejected", cur[2]-last[2],
				"peerRefused", cur[3]-last[3], "overloaded", cur[4]-last[4], "signedTotal", cur[0])
			last = cur
		}
	}
}

// logTotals logs the counters since start.
func (s *Server) logTotals(msg string) {
	s.log.Info(msg, "signedTotal", s.signed.Load(), "rateLimitedTotal", s.limited.Load(),
		"rejectedTotal", s.rejected.Load(), "peerRefusedTotal", s.denied.Load(), "overloadedTotal", s.overloaded.Load())
}
