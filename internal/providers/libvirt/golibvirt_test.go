/*
Copyright 2025.

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

package libvirt

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket"
)

// --- a minimal, hand-rolled fake libvirt RPC server ------------------------
//
// The bundled libvirttest mock is explicitly unusable for this (ADR-0008 Fact
// 5: it stamps its own serial counter instead of echoing the request's, and
// hangs under concurrency — exactly the tests that matter here). This speaks
// just enough of the real wire protocol — the framing socket.go implements,
// plus the two RPCs go-libvirt's open handshake needs (AuthList,
// ConnectOpen) and the one this PR's keepalive probe uses
// (ConnectGetLibVersion) — to drive golibvirtHolder through a REAL
// ConnectToURI handshake over an in-memory net.Pipe(), with no real libvirtd
// anywhere. It is deliberately NOT a general-purpose libvirt server: scripted
// per-test behavior (reply / hang / drop) is exactly what makes it useful for
// watchdog and redial tests, which need a connection that misbehaves on cue —
// something no real daemon does reliably (ADR-0008 Testing strategy, Tier 1).

const (
	// fakeLibvirtProgram/fakeLibvirtProtoVer are libvirt's stable wire values
	// (REMOTE_PROGRAM / REMOTE_PROTOCOL_VERSION), confirmed against go-libvirt
	// v0.0.0-20260814190004-1a83157e1858's internal/constants package. They
	// are not exported by go-libvirt, so the fake server hardcodes them.
	fakeLibvirtProgram  = 0x20008086
	fakeLibvirtProtoVer = 1

	// Procedure numbers go-libvirt's open handshake and this PR's keepalive
	// probe use (REMOTE_PROC_*), confirmed the same way.
	fakeProcConnectOpen          = 1
	fakeProcAuthList             = 66
	fakeProcConnectGetLibVersion = 157
)

// fakeLibvirtAction is how a fakeLibvirtServer responds to one request.
type fakeLibvirtAction int

const (
	// fakeLibvirtReplyOK sends a canned, successful reply for the request's
	// procedure (see fakeLibvirtDefaultPayload).
	fakeLibvirtReplyOK fakeLibvirtAction = iota
	// fakeLibvirtHang reads the request and never replies — a wedged remote,
	// exactly the case exec.CommandContext could kill and go-libvirt cannot
	// (ADR-0008 Fact 5).
	fakeLibvirtHang
	// fakeLibvirtDropConn closes the connection instead of replying.
	fakeLibvirtDropConn
)

// fakeLibvirtServer speaks just enough of libvirt's RPC wire framing over
// conn (one side of a net.Pipe()) to complete a real go-libvirt open
// handshake and answer ConnectGetLibVersion, with per-procedure behavior
// scripted by onProc.
type fakeLibvirtServer struct {
	conn   net.Conn
	onProc func(procedure uint32) fakeLibvirtAction // nil => always fakeLibvirtReplyOK
}

// serve reads and answers requests until conn errors or is closed (test
// teardown, or a scripted fakeLibvirtDropConn). Run it in its own goroutine
// per accepted connection.
func (s *fakeLibvirtServer) serve() {
	for {
		hdr, err := readFakeLibvirtRequest(s.conn)
		if err != nil {
			return
		}

		action := fakeLibvirtReplyOK
		if s.onProc != nil {
			action = s.onProc(hdr.procedure)
		}

		switch action {
		case fakeLibvirtHang:
			continue // never reply to this one; keep the loop alive for any later request on the same conn
		case fakeLibvirtDropConn:
			_ = s.conn.Close()
			return
		default:
			payload := fakeLibvirtDefaultPayload(hdr.procedure)
			if err := writeFakeLibvirtReply(s.conn, hdr.serial, hdr.procedure, payload); err != nil {
				return
			}
		}
	}
}

// fakeLibvirtRequestHeader is the subset of socket.Header a test server needs
// to answer a request: which procedure, and which serial to echo back.
// socket.Header itself is unexported field-for-field-compatible but lives in
// an internal wire-framing package we replicate here rather than import.
type fakeLibvirtRequestHeader struct {
	procedure uint32
	serial    int32
}

// readFakeLibvirtRequest reads one framed request from r: a 4-byte
// big-endian total length, then libvirt's 24-byte header (6 uint32/int32
// fields), then the payload — the exact framing socket.go implements. The
// payload itself is discarded: none of the three procedures this fake
// understands need their request payload decoded to produce a correct reply.
func readFakeLibvirtRequest(r io.Reader) (fakeLibvirtRequestHeader, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return fakeLibvirtRequestHeader{}, err
	}
	total := binary.BigEndian.Uint32(lenBuf[:])

	var hdrBuf [24]byte
	if _, err := io.ReadFull(r, hdrBuf[:]); err != nil {
		return fakeLibvirtRequestHeader{}, err
	}
	procedure := binary.BigEndian.Uint32(hdrBuf[8:12])
	serial := int32(binary.BigEndian.Uint32(hdrBuf[16:20]))

	payloadLen := int(total) - 4 - 24
	if payloadLen > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(payloadLen)); err != nil {
			return fakeLibvirtRequestHeader{}, err
		}
	}
	return fakeLibvirtRequestHeader{procedure: procedure, serial: serial}, nil
}

// writeFakeLibvirtReply writes a StatusOK Reply packet for procedure/serial —
// CRITICALLY echoing the request's own serial (never stamping a fresh one:
// that is precisely the libvirttest bug ADR-0008 Fact 5 calls out), with the
// given XDR-encoded payload.
func writeFakeLibvirtReply(w io.Writer, serial int32, procedure uint32, payload []byte) error {
	total := uint32(4 + 24 + len(payload))
	buf := make([]byte, 4, total)
	binary.BigEndian.PutUint32(buf, total)

	var hdrBuf [24]byte
	binary.BigEndian.PutUint32(hdrBuf[0:4], fakeLibvirtProgram)
	binary.BigEndian.PutUint32(hdrBuf[4:8], fakeLibvirtProtoVer)
	binary.BigEndian.PutUint32(hdrBuf[8:12], procedure)
	binary.BigEndian.PutUint32(hdrBuf[12:16], uint32(socket.Reply))
	binary.BigEndian.PutUint32(hdrBuf[16:20], uint32(serial))
	binary.BigEndian.PutUint32(hdrBuf[20:24], uint32(socket.StatusOK))
	buf = append(buf, hdrBuf[:]...)
	buf = append(buf, payload...)

	_, err := w.Write(buf)
	return err
}

// fakeLibvirtDefaultPayload returns the canned successful-reply payload for
// procedure. AuthList must decode as an empty XDR array (4 zero bytes: a
// zero element count) so go-libvirt's authenticate() short-circuits without
// issuing a second RPC. ConnectOpen's reply payload is discarded by the
// caller (initLibvirtComms only checks the error), so empty is correct.
// ConnectGetLibVersion decodes a bare XDR unsigned hyper (8 bytes,
// big-endian).
func fakeLibvirtDefaultPayload(procedure uint32) []byte {
	switch procedure {
	case fakeProcAuthList:
		return make([]byte, 4)
	case fakeProcConnectGetLibVersion:
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, 9_004_000) // an arbitrary, plausible fake libvirt version
		return buf
	default:
		return nil
	}
}

// --- fake resettableDialer ---------------------------------------------------

// fakeResettableDialer is a resettableDialer whose Dial is fully scripted —
// the "fake socket.Dialer returning a scripted net.Conn" ADR-0008's testing
// strategy calls for (Tier 1). It mirrors sshTunnelDialer's forceClose
// behavior (close whatever was most recently dialed) so watchdog/eviction
// tests can observe the underlying conn actually being severed, and it
// counts resetTransport calls so redial-on-a-stuck-Dial tests can assert on
// that path without needing a real SSH layer at all.
type fakeResettableDialer struct {
	newConn func() (net.Conn, error) // called once per Dial; each call should hand back a FRESH conn wired to its own fakeLibvirtServer

	mu       sync.Mutex
	dials    int
	resets   int
	lastConn net.Conn
	settled  bool
}

var _ resettableDialer = (*fakeResettableDialer)(nil)

func (d *fakeResettableDialer) Dial() (net.Conn, error) {
	d.mu.Lock()
	d.dials++
	d.mu.Unlock()

	conn, err := d.newConn()
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	d.lastConn = conn
	d.mu.Unlock()
	return conn, nil
}

func (d *fakeResettableDialer) beginAttempt(_ context.Context) {
	d.mu.Lock()
	d.settled = false
	d.mu.Unlock()
}

func (d *fakeResettableDialer) settle() {
	d.mu.Lock()
	d.settled = true
	d.mu.Unlock()
}

func (d *fakeResettableDialer) forceClose() {
	d.mu.Lock()
	conn := d.lastConn
	d.lastConn = nil
	d.settled = false
	d.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (d *fakeResettableDialer) evictSettled() bool {
	d.mu.Lock()
	if !d.settled {
		d.mu.Unlock()
		return false
	}
	conn := d.lastConn
	d.lastConn = nil
	d.settled = false
	d.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return true
}

func (d *fakeResettableDialer) resetTransport() {
	d.mu.Lock()
	d.resets++
	d.mu.Unlock()
}

func (d *fakeResettableDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

func (d *fakeResettableDialer) resetCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resets
}

// newFakeDialer builds a fakeResettableDialer whose every Dial() spins up a
// fresh net.Pipe() with a fakeLibvirtServer on the server side, scripted by
// onProc (nil => always reply OK). Each connect attempt — including every
// redial — gets its own independent fake server, exactly as a real redial
// would open a fresh transport.
func newFakeDialer(t *testing.T, onProc func(procedure uint32) fakeLibvirtAction) *fakeResettableDialer {
	t.Helper()
	return &fakeResettableDialer{
		newConn: func() (net.Conn, error) {
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			go (&fakeLibvirtServer{conn: server, onProc: onProc}).serve()
			return client, nil
		},
	}
}

// newTestHolder builds a golibvirtHolder over dialer with all timing
// parameters shrunk to millisecond scale, so watchdog/keepalive/redial tests
// run fast instead of waiting on the production defaults (seconds). It is
// always closed via t.Cleanup.
func newTestHolder(t *testing.T, dialer resettableDialer) *golibvirtHolder {
	t.Helper()
	h := newGolibvirtHolderWithDialer(dialer, golibvirt.ConnectURI("test:///default"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.keepaliveInterval = 20 * time.Millisecond
	h.probeTimeout = 200 * time.Millisecond
	h.connectTimeout = 500 * time.Millisecond
	t.Cleanup(h.close)
	return h
}

// --- tests -------------------------------------------------------------------

// TestGolibvirtHolder_EnsureConnected_LazyConnectsOnce proves the fast path:
// a live client is reused with no further Dial, and the very first call
// actually dials (proving the fake handshake itself is wired correctly).
func TestGolibvirtHolder_EnsureConnected_LazyConnectsOnce(t *testing.T) {
	dialer := newFakeDialer(t, nil)
	h := newTestHolder(t, dialer)

	client, err := h.ensureConnected(t.Context())
	require.NoError(t, err)
	assert.True(t, client.IsConnected())
	assert.Equal(t, 1, dialer.dialCount())

	// A second call on an already-live connection must not redial.
	_, err = h.ensureConnected(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, dialer.dialCount())
}

// TestGolibvirtHolder_WatchdogEvictsHungCall is the core watchdog proof: a
// call whose RPC never gets a reply is bounded by ctx, not left hanging
// forever, and the underlying connection is evicted (force-closed) rather
// than merely returning an error while leaving a wedged connection cached for
// the next caller to trip over.
func TestGolibvirtHolder_WatchdogEvictsHungCall(t *testing.T) {
	dialer := newFakeDialer(t, func(procedure uint32) fakeLibvirtAction {
		if procedure == fakeProcConnectGetLibVersion {
			return fakeLibvirtHang
		}
		return fakeLibvirtReplyOK
	})
	h := newTestHolder(t, dialer)

	// Establish the connection first (fast: AuthList + ConnectOpen both
	// reply normally), so the watchdog below is timing ONLY the hung RPC.
	client, err := h.ensureConnected(t.Context())
	require.NoError(t, err)
	require.True(t, client.IsConnected())

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	callErr := h.call(ctx, func(c *golibvirt.Libvirt) error {
		_, err := c.ConnectGetLibVersion()
		return err
	})
	require.Error(t, callErr)
	assert.ErrorIs(t, callErr, context.DeadlineExceeded)

	// evict's forceClose is synchronous, but go-libvirt's own listen
	// goroutine noticing the severed conn and flipping IsConnected() false is
	// not — poll rather than assert immediately, exactly as any real caller
	// downstream of evict would need to (the package doc's eventual-
	// consistency note).
	require.Eventually(t, func() bool { return !client.IsConnected() }, time.Second, 5*time.Millisecond,
		"watchdog must evict the connection on timeout, not just return an error")
}

// TestGolibvirtHolder_WatchdogDoesNotEvictOnSuccess is the control: a call
// that returns well within its deadline must NOT evict a healthy connection.
func TestGolibvirtHolder_WatchdogDoesNotEvictOnSuccess(t *testing.T) {
	dialer := newFakeDialer(t, nil)
	h := newTestHolder(t, dialer)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	err := h.call(ctx, func(c *golibvirt.Libvirt) error {
		_, err := c.ConnectGetLibVersion()
		return err
	})
	require.NoError(t, err)
	assert.True(t, h.currentClient().IsConnected())
	assert.Equal(t, 1, dialer.dialCount(), "a successful call must not trigger a redial")
}

// TestGolibvirtHolder_KeepaliveHealsDeadConnection proves the keepalive
// goroutine notices a connection that died between ticks (simulated here by
// the test forcibly closing it, standing in for a remote libvirtd
// restart/reboot) and redials automatically — the "redial-on-dead" half of
// ADR-0008 Fact 5 — with no caller ever touching the holder again.
func TestGolibvirtHolder_KeepaliveHealsDeadConnection(t *testing.T) {
	dialer := newFakeDialer(t, nil)
	h := newTestHolder(t, dialer)

	client, err := h.ensureConnected(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, dialer.dialCount())

	// Simulate an unexpected drop (e.g. libvirtd restart) by severing the
	// transport out from under the holder, exactly as evict would.
	dialer.forceClose()
	require.Eventually(t, func() bool { return !client.IsConnected() }, time.Second, 5*time.Millisecond)

	// The keepalive goroutine's next tick should notice the connection is
	// down and redial it, with no caller involved. Poll the END STATE
	// (currentClient healed) rather than an intermediate signal like
	// dialCount: a rising dial count only means an attempt STARTED, not that
	// it has finished and been published yet, so asserting on it alone would
	// be racy against exactly how far that attempt has progressed.
	require.Eventually(t, func() bool {
		c := h.currentClient()
		return c != nil && c.IsConnected()
	}, 2*time.Second, 10*time.Millisecond, "keepalive must redial and leave the holder connected again")
	assert.GreaterOrEqual(t, dialer.dialCount(), 2, "healing must have happened via a fresh dial, not the original dead connection")
}

// TestGolibvirtHolder_KeepaliveProbeFailureEvicts proves a keepalive tick
// that gets a real (non-timeout) RPC error still drops the connection, so a
// technically-open-but-unhealthy connection does not linger until some
// unrelated caller happens to trip over it.
func TestGolibvirtHolder_KeepaliveProbeFailureEvicts(t *testing.T) {
	dialer := newFakeDialer(t, func(procedure uint32) fakeLibvirtAction {
		if procedure == fakeProcConnectGetLibVersion {
			return fakeLibvirtDropConn // the remote answers the probe by hanging up
		}
		return fakeLibvirtReplyOK
	})
	h := newTestHolder(t, dialer)

	client, err := h.ensureConnected(t.Context())
	require.NoError(t, err)
	_ = client

	// The next keepalive tick (20ms) issues ConnectGetLibVersion, which the
	// fake server answers by dropping the connection — a real RPC-level
	// failure once the drop is noticed, not a watchdog timeout.
	require.Eventually(t, func() bool { return dialer.dialCount() >= 2 }, 2*time.Second, 10*time.Millisecond,
		"keepalive must notice the probe failure and redial")
}

// TestGolibvirtHolder_RedialProducesNewClientAfterForcedBreak is the
// deliverable's explicit "redial produces a new client after a forced break"
// case, proven end-to-end via the public entry point (ensureConnected) rather
// than reaching into the keepalive loop: force a break, call ensureConnected
// again, and confirm it heals via a fresh dial rather than returning a dead
// cached client or a bare error.
func TestGolibvirtHolder_RedialProducesNewClientAfterForcedBreak(t *testing.T) {
	dialer := newFakeDialer(t, nil)
	h := newTestHolder(t, dialer)

	first, err := h.ensureConnected(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, dialer.dialCount())

	dialer.forceClose() // simulate a forced break (host reboot, transport drop)
	require.Eventually(t, func() bool { return !first.IsConnected() }, time.Second, 5*time.Millisecond)

	again, err := h.ensureConnected(t.Context())
	require.NoError(t, err)
	assert.True(t, again.IsConnected())
	assert.GreaterOrEqual(t, dialer.dialCount(), 2, "ensureConnected must redial after a forced break")
}

// TestGolibvirtHolder_ConnectTimeoutResetsTransportOnStuckDial proves the
// connect-level watchdog branch: a Dial() that never returns (the SSH
// channel-open itself wedged, not merely a slow RPC after connecting) is
// bounded by ctx from the CALLER's perspective, and — because go-libvirt
// holds its own internal socket mutex for the duration of Dial(), which a
// mere forceClose cannot reach — the transport is eventually reset so a
// LATER connect attempt is not permanently wedged behind the abandoned one.
// "Eventually" is deliberate here: the reset happens only after
// reapAbandonedConnect's own grace window elapses (see connect()'s
// ctx.Done() doc for why it is not synchronous), so this test polls rather
// than asserting immediately after connect returns.
func TestGolibvirtHolder_ConnectTimeoutResetsTransportOnStuckDial(t *testing.T) {
	block := make(chan struct{}) // never closed by production code: Dial hangs until this test releases it

	dialer := &fakeResettableDialer{
		newConn: func() (net.Conn, error) {
			<-block
			return nil, errors.New("unreachable: test ended before Dial returned")
		},
	}
	h := newTestHolder(t, dialer)
	// Registered AFTER newTestHolder's t.Cleanup(h.close), so — t.Cleanup runs
	// LIFO — this releases the abandoned goroutine BEFORE close() waits on it,
	// letting teardown finish immediately instead of riding out close's
	// bounded (but real) timeout fallback.
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := h.connect(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 300*time.Millisecond, "connect must return to its caller as soon as ctx expires, not wait for the reaper")

	require.Eventually(t, func() bool { return dialer.resetCount() == 1 }, 2*time.Second, 10*time.Millisecond,
		"a connect timeout stuck inside Dial() must eventually reset the underlying transport, once the reaper concludes it is truly stuck")
}

// TestGolibvirtHolder_Evict_LeavesInFlightAttemptAlone proves evict's
// settled-only contract directly, not just incidentally via the stress test:
// while a connect attempt's handshake is deliberately held open, evict must
// leave it alone — only a connection that has actually settled is fair game
// (sshTunnelDialer.evictSettled's doc). Interrupting an in-flight handshake
// from the outside is exactly the scenario that reproduced a genuine race
// INSIDE go-libvirt's own ConnectToURI/waitAndDisconnect interaction during
// this package's development; evictSettled is what makes evict unconditionally
// safe to call from anywhere despite that.
func TestGolibvirtHolder_Evict_LeavesInFlightAttemptAlone(t *testing.T) {
	release := make(chan struct{})
	dialer := newFakeDialer(t, func(procedure uint32) fakeLibvirtAction {
		if procedure == fakeProcAuthList {
			<-release // hold the handshake open until the test says go
		}
		return fakeLibvirtReplyOK
	})
	h := newTestHolder(t, dialer)

	connectDone := make(chan struct{})
	go func() {
		_, _ = h.connect(context.Background())
		close(connectDone)
	}()

	require.Eventually(t, func() bool { return dialer.dialCount() >= 1 }, time.Second, 5*time.Millisecond,
		"the attempt must have started dialing")

	// The handshake is deliberately still blocked inside AuthList right now:
	// evict must be a no-op, not a trigger for go-libvirt's internal race.
	h.evict()
	select {
	case <-connectDone:
		t.Fatal("evict must not have completed/aborted the in-flight attempt")
	default:
	}

	close(release)
	select {
	case <-connectDone:
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not complete after release")
	}

	client := h.currentClient()
	require.NotNil(t, client)
	assert.True(t, client.IsConnected(),
		"the held-open handshake must complete successfully once released — evict must not have disturbed it")
}

// TestGolibvirtHolder_Close_StopsKeepaliveAndIsIdempotent proves close both
// tears down a live connection and can be called more than once safely —
// the Registry Evict/Close contract golibvirtHolder must honor.
func TestGolibvirtHolder_Close_StopsKeepaliveAndIsIdempotent(t *testing.T) {
	dialer := newFakeDialer(t, nil)
	h := newGolibvirtHolderWithDialer(dialer, golibvirt.ConnectURI("test:///default"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.keepaliveInterval = 5 * time.Millisecond

	_, err := h.ensureConnected(t.Context())
	require.NoError(t, err)

	h.close()
	h.close() // must not panic or block

	_, err = h.ensureConnected(t.Context())
	assert.ErrorIs(t, err, errGolibvirtHolderClosed)

	// The keepalive goroutine must actually have stopped, not merely be
	// between ticks: dial count should not keep climbing after close.
	n := dialer.dialCount()
	// Asserting the ABSENCE of further activity from a background goroutine
	// has no event to select on; a short fixed wait is the standard idiom.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, n, dialer.dialCount(), "no goroutine should still be dialing after close")
}

// TestGolibvirtHolder_ConcurrentAccess_Race exercises ensureConnected and
// call from many goroutines at once against a connection that a separate
// goroutine repeatedly evicts — the scenario -race exists to catch:
// concurrent callers racing the holder's own redial/evict bookkeeping.
//
// The chaos goroutine calls the same evict() production code uses, with no
// pre-check of its own: evict is unconditionally safe to call from any
// goroutine at any time (evictSettled's whole job, package doc) — it only
// ever closes a fully established, in-use connection, never one whose
// ConnectToURI handshake might still be running, which is what makes this
// test able to hammer it arbitrarily without needing to reason about timing
// itself. Run with -race.
func TestGolibvirtHolder_ConcurrentAccess_Race(t *testing.T) {
	dialer := newFakeDialer(t, nil)
	h := newTestHolder(t, dialer)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Many callers hammering ensureConnected/call concurrently.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				_ = h.call(ctx, func(c *golibvirt.Libvirt) error {
					_, err := c.ConnectGetLibVersion()
					return err
				})
				cancel()
			}
		}()
	}

	// One goroutine repeatedly evicting whatever connection is currently up,
	// out from under the callers above.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			select {
			case <-stop:
				return
			case <-time.After(3 * time.Millisecond):
				h.evict()
			}
		}
	}()

	// Fixed-duration stress window for a concurrency/-race test, not a
	// synchronization wait.
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}
