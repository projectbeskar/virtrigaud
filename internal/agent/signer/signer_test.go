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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortTempDir returns a short-path temporary directory: a Unix socket path
// is limited to about 100 bytes, which t.TempDir can exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vsg")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return k
}

// logs collects log lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 1})
}

func (l *logs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b bytes.Buffer
	for _, s := range l.lines {
		b.WriteString(s + "\n")
	}
	return b.String()
}

// running is a Server on a real socket.
type running struct {
	srv    *Server
	socket string
	cancel context.CancelFunc
	done   chan error
	logs   *logs
}

// serve starts a Server with o on socket (a new one when empty).
func serve(t *testing.T, socket string, o ServerOptions, mod func(*Server)) *running {
	t.Helper()
	if socket == "" {
		socket = filepath.Join(shortTempDir(t), "s.sock")
	}
	r := &running{socket: socket, done: make(chan error, 1), logs: &logs{}}
	if o.Key == nil {
		o.Key = newKey(t)
	}
	o.AllowedUID = EUID()
	o.Log = r.logs.logger()
	srv, err := NewServer(o)
	require.NoError(t, err)
	if mod != nil {
		mod(srv)
	}
	lis, err := Listen(socket)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	r.srv, r.cancel = srv, cancel
	go func() { r.done <- srv.Serve(ctx, lis) }()
	t.Cleanup(func() { r.stop(t) })
	return r
}

// stop stops the server and waits for it; it is safe to call twice.
func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		require.NoError(t, err)
		r.done <- nil
	case <-time.After(10 * time.Second):
		t.Fatal("the signer did not stop")
	}
}

func (r *running) client(t *testing.T) *Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := Connect(ctx, ClientOptions{Socket: r.socket, ServerUID: EUID()})
	require.NoError(t, err)
	return c
}

func digestOf(s string) []byte {
	d := sha256.Sum256([]byte(s))
	return d[:]
}

// roundTrip sends raw payload as one frame (or raw bytes when frame is
// false) and returns the response payload, or the read error.
func roundTrip(t *testing.T, socket string, raw []byte) ([]byte, error) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Write(raw)
	require.NoError(t, err)
	return readFrame(conn)
}

func frame(payload ...byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(payload))), payload...)
}

func TestSignOverSocket(t *testing.T) {
	key := newKey(t)
	r := serve(t, "", ServerOptions{Key: key}, nil)
	c := r.client(t)
	pub, ok := c.Public().(*ecdsa.PublicKey)
	require.True(t, ok)
	assert.True(t, pub.Equal(&key.PublicKey), "Public is the signer's key")

	d := digestOf("a message")
	sig, err := c.Sign(rand.Reader, d, crypto.SHA256)
	require.NoError(t, err)
	assert.True(t, ecdsa.VerifyASN1(&key.PublicKey, d, sig))
	assert.EqualValues(t, 1, r.srv.signed.Load())
}

// TestProtocolRefusals: the signer refuses everything but a public key
// request and a SHA-256 sign request of exactly 32 bytes.
func TestProtocolRefusals(t *testing.T) {
	d := digestOf("x")
	tests := []struct {
		name string
		req  []byte
		want Status
	}{
		{"wrong hash id", frame(append([]byte{Version, OpSign, 2, PurposeAnswer}, d...)...), StatusBadRequest},
		{"hash id zero", frame(append([]byte{Version, OpSign, 0, PurposeAnswer}, d...)...), StatusBadRequest},
		{"digest of 31 bytes", frame(append([]byte{Version, OpSign, HashSHA256, PurposeAnswer}, d[:31]...)...), StatusBadRequest},
		{"digest of 33 bytes", frame(append([]byte{Version, OpSign, HashSHA256, PurposeAnswer}, append(d, 0)...)...), StatusBadRequest},
		{"digest of 48 bytes", frame(append([]byte{Version, OpSign, HashSHA256, PurposeAnswer}, make([]byte, 48)...)...), StatusBadRequest},
		{"no digest", frame(Version, OpSign, HashSHA256, PurposeAnswer), StatusBadRequest},
		{"no purpose", frame(Version, OpSign, HashSHA256), StatusBadRequest},
		{"unknown purpose", frame(append([]byte{Version, OpSign, HashSHA256, 9}, d...)...), StatusBadRequest},
		{"purpose zero", frame(append([]byte{Version, OpSign, HashSHA256, 0}, d...)...), StatusBadRequest},
		{"old request shape, digest directly after the hash", frame(append([]byte{Version, OpSign, HashSHA256}, d...)...), StatusBadRequest},
		{"no hash id", frame(Version, OpSign), StatusBadRequest},
		{"unknown operation", frame(Version, 9), StatusBadRequest},
		{"unknown version", frame(2, OpPublicKey), StatusBadRequest},
		{"empty request", frame(), StatusBadRequest},
		{"public key with a body", frame(Version, OpPublicKey, 0), StatusBadRequest},
	}
	r := serve(t, "", ServerOptions{}, nil)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := roundTrip(t, r.socket, tc.req)
			require.NoError(t, err)
			require.Len(t, resp, 2)
			assert.Equal(t, tc.want, Status(resp[1]))
		})
	}
	assert.EqualValues(t, 0, r.srv.signed.Load(), "nothing was signed")
}

// TestOversizedFrame: a frame longer than MaxFrameBytes closes the connection
// without a response and without the payload being read.
func TestOversizedFrame(t *testing.T) {
	r := serve(t, "", ServerOptions{}, nil)
	req := binary.BigEndian.AppendUint16(nil, MaxFrameBytes+1)
	req = append(req, make([]byte, MaxFrameBytes+1)...)
	_, err := roundTrip(t, r.socket, req)
	require.Error(t, err)
	assert.NotErrorIs(t, err, os.ErrDeadlineExceeded)
	// A frame of exactly the maximum is read (and refused as a bad request).
	resp, err := roundTrip(t, r.socket, append(binary.BigEndian.AppendUint16(nil, MaxFrameBytes), make([]byte, MaxFrameBytes)...))
	require.NoError(t, err)
	assert.Equal(t, StatusBadRequest, Status(resp[1]))
	// The server still serves.
	r.client(t)
}

// TestTruncatedFrameTimesOut: a peer that announces a payload and sends less
// is dropped at the I/O deadline, and holds nothing afterwards.
func TestTruncatedFrameTimesOut(t *testing.T) {
	r := serve(t, "", ServerOptions{IOTimeout: 100 * time.Millisecond}, nil)
	conn, err := net.Dial("unix", r.socket)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte{0, 10, 1})
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.ReadAll(conn)
	require.NoError(t, err, "the server closed the connection")
	assert.EqualValues(t, 1, r.srv.rejected.Load())
}

// TestClientRefusesLocally: a digest of the wrong size and a hash other than
// SHA-256 never reach the signer.
func TestClientRefusesLocally(t *testing.T) {
	r := serve(t, "", ServerOptions{}, nil)
	c := r.client(t)
	for name, tc := range map[string]struct {
		digest []byte
		opts   crypto.SignerOpts
	}{
		"31 bytes":        {digestOf("x")[:31], crypto.SHA256},
		"empty":           {nil, crypto.SHA256},
		"SHA-384 hash":    {make([]byte, 48), crypto.SHA384},
		"SHA-512 hash":    {make([]byte, 64), crypto.SHA512},
		"32 bytes, other": {digestOf("x"), crypto.SHA1},
		"no hash":         {digestOf("x"), crypto.Hash(0)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.Sign(rand.Reader, tc.digest, tc.opts)
			require.ErrorIs(t, err, ErrSigner)
		})
	}
	assert.EqualValues(t, 0, r.srv.signed.Load())
}

func TestCheckPeer(t *testing.T) {
	require.NoError(t, CheckPeer(1000, 1000))
	require.ErrorIs(t, CheckPeer(1000, 1001), ErrPeer)
	require.ErrorIs(t, CheckPeer(0, 1000), ErrPeer)
}

// TestPeerUIDIsTheKernelsView: PeerUID returns this process's uid for a
// connection from this process, and refuses a connection that is not a Unix
// socket.
func TestPeerUIDIsTheKernelsView(t *testing.T) {
	dir := shortTempDir(t)
	lis, err := net.Listen("unix", filepath.Join(dir, "p.sock"))
	require.NoError(t, err)
	defer func() { _ = lis.Close() }()
	got := make(chan uint32, 1)
	go func() {
		c, err := lis.Accept()
		if err != nil {
			got <- 1<<32 - 1
			return
		}
		defer func() { _ = c.Close() }()
		uid, _ := PeerUID(c)
		got <- uid
	}()
	c, err := net.Dial("unix", filepath.Join(dir, "p.sock"))
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	assert.Equal(t, EUID(), <-got)
	uid, err := PeerUID(c)
	require.NoError(t, err)
	assert.Equal(t, EUID(), uid)

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = tcp.Close() }()
	tc, err := net.Dial("tcp", tcp.Addr().String())
	require.NoError(t, err)
	defer func() { _ = tc.Close() }()
	_, err = PeerUID(tc)
	require.Error(t, err)
}

// TestServerRefusesOtherUIDs: a peer whose uid is not the allowed one gets no
// response and no signature, and is counted.
func TestServerRefusesOtherUIDs(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "s.sock")
	srv, err := NewServer(ServerOptions{Key: newKey(t), AllowedUID: EUID() + 1, Log: logr.Discard()})
	require.NoError(t, err)
	lis, err := Listen(sock)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, lis) }()
	defer func() { cancel(); <-done }()

	cctx, ccancel := context.WithCancel(context.Background())
	defer ccancel()
	_, err = Connect(cctx, ClientOptions{Socket: sock, ServerUID: EUID()})
	require.ErrorIs(t, err, ErrSigner)
	assert.EqualValues(t, 1, srv.denied.Load())
	assert.EqualValues(t, 0, srv.signed.Load())
}

// TestClientRefusesOtherSigners: a signer that is not the expected uid is not
// asked to sign anything, and its key is not trusted.
func TestClientRefusesOtherSigners(t *testing.T) {
	r := serve(t, "", ServerOptions{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Connect(ctx, ClientOptions{Socket: r.socket, ServerUID: EUID() + 1})
	require.ErrorIs(t, err, ErrSigner)
	require.ErrorIs(t, err, ErrPeer)
	assert.EqualValues(t, 0, r.srv.signed.Load())

	// A peer that goes wrong after the connect is refused on the next call.
	c := r.client(t)
	c.o.ServerUID = EUID() + 1
	_, err = c.Sign(rand.Reader, digestOf("x"), crypto.SHA256)
	require.ErrorIs(t, err, ErrPeer)
}

// TestSignerRestart: when the signer goes away every call fails (no unsigned
// result), and when it comes back the same Client signs again without being
// rebuilt.
func TestSignerRestart(t *testing.T) {
	key := newKey(t)
	r := serve(t, "", ServerOptions{Key: key}, nil)
	c := r.client(t)
	d := digestOf("m")
	_, err := c.Sign(rand.Reader, d, crypto.SHA256)
	require.NoError(t, err)

	r.stop(t)
	sig, err := c.Sign(rand.Reader, d, crypto.SHA256)
	require.ErrorIs(t, err, ErrSigner, "signer down: fail closed")
	assert.Nil(t, sig)

	serve(t, r.socket, ServerOptions{Key: key}, nil)
	sig, err = c.Sign(rand.Reader, d, crypto.SHA256)
	require.NoError(t, err)
	assert.True(t, ecdsa.VerifyASN1(&key.PublicKey, d, sig))
}

func shortSock(t *testing.T) string { return filepath.Join(shortTempDir(t), "s.sock") }

// TestRekeyedSignerFailsVerification: a signer that is replaced by one with
// another key cannot make this Client accept its signatures.
func TestRekeyedSignerFailsVerification(t *testing.T) {
	r := serve(t, "", ServerOptions{}, nil)
	c := r.client(t)
	r.stop(t)
	serve(t, r.socket, ServerOptions{Key: newKey(t)}, nil)
	_, err := c.Sign(rand.Reader, digestOf("m"), crypto.SHA256)
	require.ErrorIs(t, err, ErrSigner)
}

// TestRateLimit: the signer refuses signatures above its burst and signs
// again as the bucket refills; refusals are counted.
func TestRateLimit(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r := serve(t, "", ServerOptions{SignsPerSecond: 1, SignBurst: 2}, func(s *Server) {
		s.now = clock
		s.limiters = map[byte]*bucket{PurposeAnswer: newBucket(1, 2, clock()), PurposeHandshake: newBucket(1, 2, clock())}
	})
	c := r.client(t)
	d := digestOf("m")
	for i := 0; i < 2; i++ {
		_, err := c.Sign(rand.Reader, d, crypto.SHA256)
		require.NoError(t, err, "within the burst")
	}
	_, err := c.Sign(rand.Reader, d, crypto.SHA256)
	require.ErrorIs(t, err, ErrSigner)
	assert.Contains(t, err.Error(), StatusRateLimited.String())
	assert.EqualValues(t, 1, r.srv.limited.Load())
	assert.EqualValues(t, 2, r.srv.signed.Load())

	mu.Lock()
	now = now.Add(1500 * time.Millisecond)
	mu.Unlock()
	_, err = c.Sign(rand.Reader, d, crypto.SHA256)
	require.NoError(t, err, "one token refilled")
	// A public key request is not limited.
	_, err = Connect(context.Background(), ClientOptions{Socket: r.socket, ServerUID: EUID()})
	require.NoError(t, err)
}

func TestBucket(t *testing.T) {
	now := time.Now()
	b := newBucket(10, 3, now)
	for i := 0; i < 3; i++ {
		assert.True(t, b.allow(now))
	}
	assert.False(t, b.allow(now))
	assert.False(t, b.allow(now.Add(50*time.Millisecond)), "half a token")
	assert.True(t, b.allow(now.Add(150*time.Millisecond)))
	// The bucket never holds more than its burst.
	later := now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		assert.True(t, b.allow(later))
	}
	assert.False(t, b.allow(later))
}

// TestConnectionBound: above MaxConns a connection is closed at once, and the
// signer serves again when one frees up.
func TestConnectionBound(t *testing.T) {
	r := serve(t, "", ServerOptions{MaxConns: 1, IOTimeout: 3 * time.Second}, nil)
	idle, err := net.Dial("unix", r.socket)
	require.NoError(t, err)
	// Wait until the server has taken the idle connection.
	require.Eventually(t, func() bool {
		c, err := net.Dial("unix", r.socket)
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, rerr := readFrame(c)
		return rerr != nil && r.srv.overloaded.Load() > 0
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, idle.Close())
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := Connect(ctx, ClientOptions{Socket: r.socket, ServerUID: EUID()})
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
}

// TestLogsCarryCountsOnly: the signer's log has the signature count and no
// digest, signature or key material.
func TestLogsCarryCountsOnly(t *testing.T) {
	key := newKey(t)
	r := serve(t, "", ServerOptions{Key: key, LogInterval: 20 * time.Millisecond}, nil)
	c := r.client(t)
	d := digestOf("secret-ish")
	sig, err := c.Sign(rand.Reader, d, crypto.SHA256)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return bytes.Contains([]byte(r.logs.text()), []byte("signer activity")) }, 5*time.Second, 10*time.Millisecond)
	r.stop(t)
	text := r.logs.text()
	assert.Contains(t, text, `"signed"=1`)
	assert.Contains(t, text, "signedTotal")
	raw, err := key.Bytes()
	require.NoError(t, err)
	for _, secret := range [][]byte{d, sig, raw} {
		assert.NotContains(t, text, string(secret))
		assert.NotContains(t, text, hexOf(secret))
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// TestServeStopsAndClosesConnections: a stop returns even with an idle
// connection open, because the connection's deadline ends it.
func TestServeStops(t *testing.T) {
	r := serve(t, "", ServerOptions{IOTimeout: 200 * time.Millisecond}, nil)
	idle, err := net.Dial("unix", r.socket)
	require.NoError(t, err)
	defer func() { _ = idle.Close() }()
	r.stop(t)
	_, err = net.Dial("unix", r.socket)
	require.Error(t, err, "the socket no longer accepts")
}

func TestNewServerRefusals(t *testing.T) {
	_, err := NewServer(ServerOptions{})
	require.Error(t, err, "no key")
	k384, gerr := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, gerr)
	_, err = NewServer(ServerOptions{Key: k384})
	require.Error(t, err, "not P-256")
	_, err = NewServer(ServerOptions{Key: newKey(t), SignsPerSecond: -1})
	require.Error(t, err)
}

// FuzzDispatch: no request makes the signer panic or sign anything but a
// well-formed request.
func FuzzDispatch(f *testing.F) {
	f.Add([]byte{Version, OpSign, HashSHA256, PurposeAnswer})
	f.Add(append([]byte{Version, OpSign, HashSHA256, PurposeHandshake}, make([]byte, 32)...))
	f.Add([]byte{Version, OpPublicKey})
	f.Add([]byte{})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(f, err)
	s, err := NewServer(ServerOptions{Key: key, SignsPerSecond: 1e9, SignBurst: 1 << 20, Log: logr.Discard()})
	require.NoError(f, err)
	f.Fuzz(func(t *testing.T, req []byte) {
		status, body := s.dispatch(req)
		if status != StatusOK {
			return
		}
		switch {
		case len(req) == 2 && req[0] == Version && req[1] == OpPublicKey:
		case len(req) == 4+DigestSize && req[0] == Version && req[1] == OpSign && req[2] == HashSHA256 && (req[3] == PurposeAnswer || req[3] == PurposeHandshake):
			if !ecdsa.VerifyASN1(&key.PublicKey, req[4:], body) {
				t.Fatal("a signature that does not verify")
			}
		default:
			t.Fatalf("request %x was answered OK", req)
		}
	})
}

// TestConnectWithRetry: a signer that starts late is reached; one that never
// starts ends with the context's deadline and the last error.
func TestConnectWithRetry(t *testing.T) {
	sock := shortSock(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := ConnectWithRetry(ctx, ClientOptions{Socket: sock, ServerUID: EUID()}, 20*time.Millisecond)
	require.ErrorIs(t, err, ErrSigner)

	errc := make(chan error, 1)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	go func() {
		_, err := ConnectWithRetry(ctx2, ClientOptions{Socket: sock, ServerUID: EUID()}, 20*time.Millisecond)
		errc <- err
	}()
	// The signer starts while the client is retrying (or just before its first
	// attempt: either way it is reached).
	serve(t, sock, ServerOptions{}, nil)
	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the client did not reach the late signer")
	}
}

// TestClientContextEnds: when the Client's lifetime ends every call fails.
func TestClientContextEnds(t *testing.T) {
	r := serve(t, "", ServerOptions{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	c, err := Connect(ctx, ClientOptions{Socket: r.socket, ServerUID: EUID()})
	require.NoError(t, err)
	cancel()
	_, err = c.Sign(rand.Reader, digestOf("x"), crypto.SHA256)
	require.ErrorIs(t, err, ErrSigner)
	assert.True(t, errors.Is(err, ErrSigner))
}

// TestPurposesHaveSeparateBudgets: a flood of handshake signatures that
// exhausts the handshake budget leaves the answer budget untouched.
func TestPurposesHaveSeparateBudgets(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r := serve(t, "", ServerOptions{SignsPerSecond: 1, SignBurst: 2, HandshakeSignsPerSecond: 1, HandshakeSignBurst: 2}, func(s *Server) {
		s.now = clock
		s.limiters = map[byte]*bucket{PurposeAnswer: newBucket(1, 2, clock()), PurposeHandshake: newBucket(1, 2, clock())}
	})
	c := r.client(t)
	answers := c.AnswerSigner()
	d := digestOf("m")
	for i := 0; i < 2; i++ {
		_, err := c.Sign(rand.Reader, d, crypto.SHA256)
		require.NoError(t, err)
	}
	_, err := c.Sign(rand.Reader, d, crypto.SHA256)
	require.ErrorIs(t, err, ErrSigner, "the handshake budget is used")
	for i := 0; i < 2; i++ {
		sig, err := answers.Sign(rand.Reader, d, crypto.SHA256)
		require.NoError(t, err, "the answer budget is separate")
		assert.True(t, ecdsa.VerifyASN1(answers.Public().(*ecdsa.PublicKey), d, sig))
	}
	_, err = answers.Sign(rand.Reader, d, crypto.SHA256)
	require.ErrorIs(t, err, ErrSigner)
	assert.EqualValues(t, 2, r.srv.signedBy[PurposeHandshake].Load())
	assert.EqualValues(t, 2, r.srv.signedBy[PurposeAnswer].Load())
	assert.EqualValues(t, 1, r.srv.limitedBy[PurposeHandshake].Load())
	assert.EqualValues(t, 1, r.srv.limitedBy[PurposeAnswer].Load())
}
