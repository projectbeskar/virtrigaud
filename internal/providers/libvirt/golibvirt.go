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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket"
)

// This file is the ADR-0008 PR 4a go-libvirt connection foundation: the
// pure-Go libvirt RPC client (github.com/digitalocean/go-libvirt, D1) dialed
// as a transport OVER the persistent in-process SSH client ADR-0008 PR 3
// already manages (sshclient.go) — no new listener, no new PKI, the same
// trust boundary virsh already uses (D3). It is deliberately connection
// plumbing only: NO operation is routed through it yet (see
// hostconn.Conn.Libvirt's doc and VirshProvider.Libvirt below), so it stays
// fully dormant — no dial, no background goroutine — until ADR-0008 PR 4b
// starts routing shadow-compare reads through it.
//
// # The gaps this file exists to paper over (ADR-0008 Fact 5)
//
// go-libvirt's ~488 generated RPC methods give none of what an unattended,
// long-lived client needs to stay healthy:
//
//   - No context.Context / per-call deadline on any RPC. A blocked call blocks
//     forever; the only way to unblock it is to sever the connection it is
//     blocked on. See golibvirtHolder.call and .evict.
//   - No keepalive. libvirt's own RPC keepalive is not implemented
//     client-side. See golibvirtHolder.keepaliveLoop.
//   - No auto-reconnect. See keepaliveLoop (the periodic redial-on-dead
//     tick) and connect (the lazy redial any caller gets for free the next
//     time it asks for a client).
//
// golibvirtHolder is the mutex-guarded answer to all three. Read its doc
// before its methods.
//
// # Lifecycle, precisely
//
//  1. Dormant: VirshProvider.golibvirt is nil. This is the state of every
//     production process today — nothing calls Libvirt(ctx).
//  2. First Libvirt(ctx) call: a *golibvirtHolder is constructed
//     (newGolibvirtHolder) and ensureConnected calls connect, which dials the
//     SSH tunnel (sshTunnelDialer, over sshclient.go's persistent *ssh.Client)
//     and performs the go-libvirt open/auth handshake, both bounded by ctx.
//     On success it starts the keepalive/redial goroutine — once, ever, for
//     this holder, see startKeepaliveLocked — and returns the client.
//  3. Steady state: the keepalive goroutine, every keepaliveInterval, first
//     ensures the connection is up (a no-op if it already is) and then probes
//     ConnectGetLibVersion through the same watchdog any other call uses. A
//     connection found dead is redialed in the same tick before probing.
//     Ordinary callers go through ensureConnected, which reuses the live
//     client without doing any I/O.
//  4. A caller's ctx expires mid-call: call's watchdog fires, evicts the
//     connection, and returns a deadline error. This is CONNECTION-scoped
//     cancellation, not call-scoped: concurrent sibling calls sharing the
//     connection fail too, and the connection is left dead for the next
//     ensureConnected/keepalive tick to redial. This blast radius is a
//     documented, accepted tradeoff (go-libvirt gives us no finer-grained way
//     to interrupt one specific call), not a bug.
//  5. Close (VirshProvider.Cleanup, via virshConn.Close): stops the keepalive
//     goroutine and force-closes the transport. Idempotent.
//
// # Why connect/close/evict never touch h.client concurrently with each other
//
// go-libvirt's RPC-call surface (request/requestStream and the generated
// methods built on it) IS safe for many goroutines at once — atomic serial
// number, mutex-guarded callback map, serialized socket writes. Its
// CONNECTION-state surface is not: ConnectToURI/Connect/Disconnect write
// *Libvirt's own `disconnected` field with no lock at all, and
// Disconnected/IsConnected read it the same way. Calling ANY of those
// concurrently with each other on the same client — e.g. one goroutine still
// inside an abandoned (ctx-timed-out) ConnectToURI while another reads
// IsConnected — is a genuine data race, caught empirically by `go test
// -race` during this PR's own development, not a hypothetical. Because a
// timed-out ConnectToURI cannot be cancelled (go-libvirt gives us nothing to
// cancel it with) and keeps running in the background, the fix is not "hold
// h.mu a little longer" — it is: every path that would touch h.client's
// connection-state methods goes through connect, and connect tracks whether
// an attempt (including an abandoned one) is still outstanding via
// h.connecting/h.connectDone, and WAITS for it to actually finish before
// anyone touches h.client again. See connect's body.
const (
	// libvirtSocketPath is libvirtd's default Unix socket, forwarded over the
	// per-host SSH client (ADR-0008 D3) instead of being exposed on a
	// TCP/TLS listener.
	libvirtSocketPath = "/var/run/libvirt/libvirt-sock"

	// The following are the PRODUCTION defaults for golibvirtHolder's timing
	// parameters (see the struct's fields). They are struct fields, not bare
	// consts used directly, so golibvirt_test.go can shrink them and exercise
	// the watchdog/keepalive/redial lifecycle in milliseconds instead of
	// real seconds — newGolibvirtHolderWithDialer sets these defaults; tests
	// override the fields right after construction, before triggering any
	// connect.

	// golibvirtDefaultKeepaliveInterval is how often the keepalive prober
	// issues its cheap health-check RPC (ConnectGetLibVersion) against an
	// idle, otherwise-healthy connection. go-libvirt implements none of
	// libvirt's own RPC keepalive (ADR-0008 Fact 5) — this loop is the whole
	// of it.
	golibvirtDefaultKeepaliveInterval = 30 * time.Second

	// golibvirtDefaultProbeTimeout bounds a single keepalive RPC. Exceeding
	// it is treated as a dead connection and evicted, same as any other
	// watchdog timeout.
	golibvirtDefaultProbeTimeout = 10 * time.Second

	// golibvirtDefaultConnectTimeout bounds one connect/redial attempt: the
	// SSH tunnel dial plus the libvirt open/auth handshake. It is also the
	// bound keepaliveLoop uses when redialing on a dead connection; the
	// ticker interval itself (keepaliveInterval) is the backoff between
	// attempts, so a persistently unreachable host is retried once per tick,
	// never busy-looped.
	golibvirtDefaultConnectTimeout = 15 * time.Second
)

// sshTunnelDialer implements go-libvirt's one-method socket.Dialer interface
// over one host's persistent *ssh.Client (ADR-0008 PR 3, sshclient.go). Dial
// opens a new channel to the remote libvirt socket on every call, multiplexed
// over the existing SSH connection — it does NOT perform a new SSH handshake,
// unlike go-libvirt's own bundled SSH dialer (socket/dialers/gossh.go), which
// re-handshakes on every reconnect (ADR-0008 D3's explicit reason for not
// using it).
//
// Deliberately NOT dialers.NewAlreadyConnected: that dialer is one-shot (it
// hands back the same net.Conn forever and cannot redial), which would break
// golibvirtHolder's redial-on-dead lifecycle entirely.
type sshTunnelDialer struct {
	vp         *VirshProvider
	socketPath string

	mu      sync.Mutex
	ctx     context.Context // set by beginAttempt before each connect attempt
	gen     uint64          // bumped by beginAttempt/forceClose; see Dial's staleness check
	conn    net.Conn        // the most recently dialed raw conn, for forceClose/evictSettled
	settled bool            // true once the CURRENT generation's ConnectToURI has succeeded; see settle's doc
}

// Compile-time proof sshTunnelDialer satisfies go-libvirt's dialer interface.
var _ socket.Dialer = (*sshTunnelDialer)(nil)

// beginAttempt records ctx as the one in effect for the connect attempt about
// to start, and invalidates any Dial() call still in flight from a PREVIOUS
// attempt (see Dial's staleness check). golibvirtHolder.connect must call
// this before launching that attempt's watched goroutine.
func (d *sshTunnelDialer) beginAttempt(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.gen++
	d.settled = false
	d.mu.Unlock()
}

// Dial opens a new "unix" channel to d.socketPath over the persistent SSH
// client (VirshProvider.dialTunnel, sshclient.go), which itself redials the
// SSH transport once if the cached client turns out to be dead. If a newer
// attempt (or an eviction) has superseded this one while the dial was still
// in flight — the stale-generation case — the returned conn is closed
// immediately instead of being handed to go-libvirt, so a slow, since-
// abandoned dial can never resurrect a connection the holder has already
// moved on from.
func (d *sshTunnelDialer) Dial() (net.Conn, error) {
	d.mu.Lock()
	ctx, gen := d.ctx, d.gen
	d.mu.Unlock()
	if ctx == nil {
		return nil, errors.New("golibvirt: Dial called before beginAttempt")
	}

	conn, err := d.vp.dialTunnel(ctx, "unix", d.socketPath)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	stale := gen != d.gen
	if !stale {
		d.conn = conn
	}
	d.mu.Unlock()

	if stale {
		_ = conn.Close()
		return nil, errors.New("golibvirt: dial superseded by a newer connection attempt")
	}
	return conn, nil
}

// settle marks the CURRENT generation's dial as a fully established
// connection — golibvirtHolder.connect calls this immediately after its
// ConnectToURI succeeds. It exists so evictSettled can tell a live,
// in-use connection apart from one whose handshake is still in flight: see
// evictSettled's doc for why that distinction is load-bearing, not
// pedantic.
func (d *sshTunnelDialer) settle() {
	d.mu.Lock()
	d.settled = true
	d.mu.Unlock()
}

// forceClose closes the most recently dialed raw connection directly,
// bypassing go-libvirt's polite Disconnect (see golibvirtHolder.evict's doc
// for why that distinction matters), and invalidates any Dial() currently in
// flight so it cannot stash a conn for an attempt the holder has already
// abandoned. A no-op if nothing has been dialed yet.
//
// forceClose is UNCONDITIONAL: it closes whatever the current generation is,
// settled or not. That is only safe when the caller IS the connect attempt
// that owns the current (necessarily unsettled, still in-flight) generation
// — golibvirtHolder.connect's own ctx.Done() branch is the one legitimate
// caller. Every other caller (evict, close) MUST use evictSettled instead:
// forceClose called on someone ELSE's in-flight, unsettled attempt can
// close the raw conn out from under a handshake that is about to succeed,
// which triggers a genuine, separately-confirmed race INSIDE go-libvirt's
// own ConnectToURI/waitAndDisconnect interaction (see golibvirtHolder's
// package doc and close's doc) — not a race in this package, but this
// package IS what decides whether that window can ever be reached, and
// evictSettled is how it refuses to.
func (d *sshTunnelDialer) forceClose() {
	d.mu.Lock()
	conn := d.conn
	d.conn = nil
	d.settled = false
	d.gen++
	d.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// evictSettled closes the most recently dialed connection ONLY if it has
// been settle()d — i.e. only a fully established, currently-in-use
// connection, never one whose ConnectToURI handshake might still be running.
// It reports whether it actually closed anything. This is the safe primitive
// every EXTERNAL caller (evict; anything that is not the connect attempt's
// own watchdog) must use: an in-flight, unsettled attempt is left strictly
// alone, because interrupting it from the outside is exactly the scenario
// that can trigger go-libvirt's internal race (see forceClose's doc). The
// in-flight attempt is not stuck by being left alone — it will settle or
// fail on its own, same as any other connect.
func (d *sshTunnelDialer) evictSettled() bool {
	d.mu.Lock()
	if !d.settled {
		d.mu.Unlock()
		return false
	}
	conn := d.conn
	d.conn = nil
	d.settled = false
	d.gen++
	d.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return true
}

// resetTransport tears down the persistent SSH client itself (sshclient.go),
// which sshTunnelDialer.Dial multiplexes channels over. It exists for the one
// case forceClose cannot handle: a connect attempt abandoned while still
// stuck INSIDE Dial() itself, before any conn was produced for forceClose to
// close — see golibvirtHolder.connect's ctx.Done() branch for why resetting
// the shared SSH transport is the only remaining way to unstick that.
func (d *sshTunnelDialer) resetTransport() {
	d.vp.resetSSHClient()
}

// resettableDialer is the subset of sshTunnelDialer's behavior golibvirtHolder
// depends on beyond socket.Dialer.Dial itself: per-attempt ctx threading
// (beginAttempt), marking a successful handshake as safe to evict (settle),
// unconditional eviction for a connect attempt's own watchdog (forceClose),
// conditional eviction for every other caller (evictSettled), and tearing
// down the underlying transport when a dial is stuck before producing
// anything forceClose can close (resetTransport). sshTunnelDialer is the
// production implementation, over the host's persistent SSH client; tests
// inject a fake implementation driving a scripted net.Conn directly, with no
// SSH layer at all — see golibvirt_test.go. Small and structural on purpose:
// this is exactly what the ADR's "fake socket.Dialer" testing strategy needs
// to be able to substitute.
type resettableDialer interface {
	socket.Dialer
	beginAttempt(ctx context.Context)
	settle()
	forceClose()
	evictSettled() bool
	resetTransport()
}

// Compile-time proof sshTunnelDialer satisfies the holder's full dependency,
// not just socket.Dialer.
var _ resettableDialer = (*sshTunnelDialer)(nil)

// golibvirtHolder is the mutex-guarded connection-lifecycle wrapper around
// one host's go-libvirt client: lazy connect, a ctx-deadline watchdog on
// every call, a keepalive prober, redial-on-dead, and clean close/evict. See
// the package doc above for the full lifecycle narrative.
//
// One *golibvirt.Libvirt is documented as concurrency-safe for many
// goroutines (atomic serial number, mutex-guarded callback map, serialized
// writes) — but deciding WHICH client is current, and tearing the old one
// down on redial or eviction, is this holder's job and needs its own mutex,
// separate from go-libvirt's internal one.
type golibvirtHolder struct {
	dialer     resettableDialer
	connectURI golibvirt.ConnectURI
	logger     *slog.Logger

	// rootCtx/rootCancel bound the holder's OWN background goroutine (the
	// keepalive/redial loop), which must keep running across many callers'
	// requests rather than being tied to any single caller's ctx — the
	// holder owns this context itself, per the "no go func() without a
	// cancellation story" rule. Cancelled by close.
	rootCtx    context.Context
	rootCancel context.CancelFunc

	// Timing parameters, defaulted from the golibvirtDefault* consts by
	// newGolibvirtHolderWithDialer. Fields (not bare consts) purely so tests
	// can shrink them; production code never changes them after construction.
	keepaliveInterval time.Duration
	probeTimeout      time.Duration
	connectTimeout    time.Duration

	mu sync.Mutex
	// client is the CURRENT, published go-libvirt client, or nil before the
	// first successful connect. A fresh *golibvirt.Libvirt is constructed for
	// EVERY connect attempt (never reused in place across a redial — see
	// connect's doc for why) and is only ever assigned here AFTER its one and
	// only ConnectToURI call has fully returned, so by the time any other
	// goroutine can observe this field, that object's connection-state fields
	// will never be written again.
	client *golibvirt.Libvirt

	// connecting/connectDone coordinate concurrent callers so at most one
	// dial attempt is in flight at a time — a resource-efficiency measure
	// (each attempt opens a real SSH-tunneled channel and go-libvirt client
	// that would otherwise leak if left to run unobserved), NOT what makes
	// concurrent access safe: the fresh-client-per-attempt design is what
	// does that, by construction. connecting is true from the moment an
	// attempt's goroutine is spawned until it returns from ConnectToURI
	// (even an abandoned, ctx-timed-out one); connectDone is closed at that
	// same moment.
	connecting  bool
	connectDone chan struct{}

	keepaliveOn bool
	closed      bool
}

// newGolibvirtHolder constructs the production connection-lifecycle holder
// for vp, dialing over vp's persistent SSH client (sshTunnelDialer). It does
// not dial: the returned holder is inert (no goroutine, no socket) until
// ensureConnected — reached via VirshProvider.Libvirt — is called for the
// first time.
func newGolibvirtHolder(vp *VirshProvider) *golibvirtHolder {
	// remoteVirshConnectURI (virsh.go) derives the driver+path a REMOTE virsh
	// must target so it does not silently split reads/writes across
	// qemu:///system and qemu:///session (see its doc); the same derivation
	// is correct here for exactly the same reason — go-libvirt must open the
	// same libvirtd path virsh does, not always assume the system instance.
	connectURI := remoteVirshConnectURI(vp.uri)
	if connectURI == "" {
		connectURI = string(golibvirt.QEMUSystem)
	}

	dialer := &sshTunnelDialer{vp: vp, socketPath: libvirtSocketPath}
	return newGolibvirtHolderWithDialer(dialer, golibvirt.ConnectURI(connectURI), vp.logger)
}

// newGolibvirtHolderWithDialer builds a holder around an arbitrary
// resettableDialer. It is the seam newGolibvirtHolder uses for the production
// SSH-tunnel dialer and golibvirt_test.go uses to inject a fake one, so the
// watchdog/keepalive/redial lifecycle below is exercised identically in both
// — only what Dial() connects to differs.
func newGolibvirtHolderWithDialer(dialer resettableDialer, connectURI golibvirt.ConnectURI, logger *slog.Logger) *golibvirtHolder {
	if logger == nil {
		logger = slog.Default()
	}
	rootCtx, rootCancel := context.WithCancel(context.Background()) // owns its own background-goroutine lifetime; see the holder's doc

	return &golibvirtHolder{
		dialer:     dialer,
		connectURI: connectURI,
		logger:     logger,
		// client starts nil: a fresh *golibvirt.Libvirt is constructed by
		// connect for every attempt (see its doc), never here.
		rootCtx:           rootCtx,
		rootCancel:        rootCancel,
		keepaliveInterval: golibvirtDefaultKeepaliveInterval,
		probeTimeout:      golibvirtDefaultProbeTimeout,
		connectTimeout:    golibvirtDefaultConnectTimeout,
	}
}

// ensureConnected returns the holder's go-libvirt client, connecting lazily
// on first use and redialing if the cached client is no longer connected. It
// is a thin, ctx-precheck wrapper over connect — kept as a separate name
// because "ensure I have a connected client" reads better than "connect" at
// call sites (VirshProvider.Libvirt, call) that do not care whether a dial
// actually happened.
func (h *golibvirtHolder) ensureConnected(ctx context.Context) (*golibvirt.Libvirt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return h.connect(ctx)
}

// connect returns the holder's live go-libvirt client, dialing if needed,
// bounding any dial it performs by ctx.
//
// It constructs a BRAND NEW *golibvirt.Libvirt for every attempt rather than
// redialing the existing one in place. This is not stylistic: go-libvirt's
// connection-state surface (ConnectToURI/Disconnect/IsConnected/Disconnected)
// mutates and reads *Libvirt's own `disconnected` field with NO
// synchronization at all — confirmed empirically with `go test -race` while
// building this file, both directly (a redial's ConnectToURI reassigning the
// field while another goroutine reads IsConnected) and indirectly (the
// background goroutine ConnectToURI spawns internally, waitAndDisconnect,
// closes that same field whenever the connection later dies, fully
// unsynchronized with anything). See the package doc's "why connect/close/
// evict never touch h.client concurrently" section.
//
// A fresh object per attempt sidesteps this by construction instead of by
// synchronization: EVERY *golibvirt.Libvirt this holder creates gets AT MOST
// one ConnectToURI call, ever, so its connection-state fields are written
// exactly once and read freely thereafter — no second writer can ever appear,
// with or without a lock. h.client is only ever assigned (under h.mu) AFTER
// that one call has fully returned, so by the time any other goroutine can
// observe h.client, its fields will never be written again. An attempt
// abandoned on ctx timeout is simply never published to h.client and never
// touched again by this holder; its background goroutines may keep running,
// harmlessly, since nothing will ever call another method on that object.
//
// connecting/connectDone additionally ensure at most one attempt is in
// flight at a time — a resource-efficiency measure (each attempt opens a
// real SSH-tunneled channel) layered on top of the safety property above,
// not a substitute for it.
func (h *golibvirtHolder) connect(ctx context.Context) (*golibvirt.Libvirt, error) {
	for {
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return nil, errGolibvirtHolderClosed
		}
		if h.client != nil && h.client.IsConnected() {
			client := h.client
			h.mu.Unlock()
			return client, nil
		}
		if h.connecting {
			// Another goroutine is already dialing; wait for it rather than
			// starting a second, wasted attempt in parallel. Re-evaluate
			// from the top once it finishes: it may have succeeded (we are
			// simply connected now), failed (try again), or a third
			// goroutine may have started yet another attempt meanwhile.
			done := h.connectDone
			h.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, fmt.Errorf("connect go-libvirt client: %w", ctx.Err())
			}
		}

		h.connecting = true
		done := make(chan struct{})
		h.connectDone = done
		h.mu.Unlock()

		h.dialer.beginAttempt(ctx)
		newClient := golibvirt.NewWithDialer(h.dialer)

		connectErr := make(chan error, 1)
		go func() {
			err := newClient.ConnectToURI(h.connectURI)
			connectErr <- err
			h.mu.Lock()
			h.connecting = false
			h.mu.Unlock()
			close(done)
		}()

		select {
		case err := <-connectErr:
			if err != nil {
				return nil, fmt.Errorf("connect go-libvirt client: %w", err)
			}
			// settle BEFORE publishing: once other goroutines can observe
			// h.client == newClient (via currentClient/evict), evictSettled
			// must already report this generation as safe to close.
			h.dialer.settle()
			h.mu.Lock()
			h.client = newClient
			h.startKeepaliveLocked()
			h.mu.Unlock()
			h.logger.Info("go-libvirt connection established", "uri", string(h.connectURI))
			return newClient, nil

		case <-ctx.Done():
			// The caller's ctx is up, but the spawned goroutine above keeps
			// running regardless (go-libvirt gives it no way to be
			// cancelled). Do NOT force-close its conn HERE, synchronously:
			// "ctx expired" does not mean "ConnectToURI is blocked" — it may
			// simply be finishing up a handshake that has already received
			// its last reply and is microseconds from succeeding, purely
			// because of scheduling, not because anything is stuck. Closing
			// the conn out from under THAT specific window can race
			// go-libvirt's own internal ConnectToURI/waitAndDisconnect
			// interaction (package doc) — confirmed empirically under
			// concurrent load with `go test -race`, not a theoretical corner
			// case, and NOT preventable by only ever touching "our own"
			// attempt (it happens even though this IS our own attempt).
			//
			// So: hand cleanup to a detached reaper that gives the attempt a
			// further connectTimeout to finish NATURALLY — succeed or fail
			// on its own, either way harmless, since it was never published
			// — before concluding it is truly stuck (not merely slow) and
			// only then forcing the transport closed. The caller is not
			// blocked by any of this — its ctx error is returned below
			// regardless of how long the reaper ends up waiting. Other
			// callers are not blocked forever either: h.connecting stays
			// true (so nobody starts a redundant, wasted second attempt)
			// until this exact goroutine flips it back, whether that
			// happens because ConnectToURI finished on its own or because
			// the reaper's forceClose/resetTransport eventually unblocked
			// it — the accepted cost is that a genuinely stuck connect can
			// now take up to 2×connectTimeout to resolve for waiters,
			// traded for closing a real data race.
			go h.reapAbandonedConnect(connectErr)
			return nil, fmt.Errorf("connect go-libvirt client: %w", ctx.Err())
		}
	}
}

// reapAbandonedConnect waits, with its OWN bound (connectTimeout, a second
// helping on top of whatever the original caller's ctx already allowed), for
// an abandoned connect attempt to finish on its own before concluding it is
// genuinely stuck (not just running slower than the caller's ctx budget) and
// only THEN forcing the transport closed — see connect's ctx.Done() branch
// for why forcing it closed immediately is not safe. connectErr is the same
// buffered, capacity-1 channel connect's own select already gave up reading
// from (select only ever consumes one of its cases), so there is no risk of
// double-receiving the attempt's result.
func (h *golibvirtHolder) reapAbandonedConnect(connectErr <-chan error) {
	select {
	case <-connectErr:
		// Finished on its own, success or failure, within the grace window.
		// A success is simply discarded here (never published to h.client)
		// — wasteful (one extra connection briefly existed for nothing) but
		// harmless, and no rarer than any other abandoned-goroutine outcome
		// this file already documents.
	case <-time.After(h.connectTimeout):
		h.dialer.forceClose()
		h.dialer.resetTransport()
	}
}

// call runs fn against the holder's connected client under a watchdog: fn
// executes in its own goroutine while call selects on ctx.Done(). If ctx
// expires or is cancelled first, call evicts the connection — the only way to
// unblock a wire client stuck waiting on a hung read/write, since go-libvirt
// has no per-RPC deadline of its own (ADR-0008 Fact 5) — and returns a
// wrapped ctx.Err(). Otherwise it returns fn's own result.
//
// Cancellation here is CONNECTION-scoped, not call-scoped: a timeout evicts
// the WHOLE connection, so any concurrent sibling call sharing it fails too.
// This is a documented, accepted blast radius (see the package doc above),
// not a bug — go-libvirt gives us no finer-grained way to interrupt one
// specific call.
func (h *golibvirtHolder) call(ctx context.Context, fn func(*golibvirt.Libvirt) error) error {
	client, err := h.ensureConnected(ctx)
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() { done <- fn(client) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		h.evict()
		return fmt.Errorf("go-libvirt call watchdog: %w", ctx.Err())
	}
}

// evict force-closes the connection's raw transport (via the dialer's
// evictSettled, never forceClose and never by calling client.Disconnect —
// see the notes below) so the next ensureConnected/connect redials from
// scratch. It deliberately does NOT itself read
// h.client.IsConnected()/Disconnected(): only connect is allowed to touch
// that surface (package doc), and evict does not need to — closing the
// transport is sufficient to make it fail, and connect will observe that
// indirectly (ConnectToURI/IsConnected on a severed transport) the next
// time anyone calls it.
//
// evictSettled, not the unconditional forceClose: evict can be called at any
// time by call's watchdog or the keepalive loop, potentially while an
// UNRELATED connect attempt is concurrently in flight (e.g. another goroutine
// is already redialing because the connection evict is ABOUT to close was
// already noticed as dead by someone else). forceClose-ing unconditionally
// in that window can sever a DIFFERENT, in-progress handshake instead of the
// established connection evict actually means to drop — which, if that
// handshake was about to succeed, can trigger a genuine race INSIDE
// go-libvirt's own ConnectToURI/waitAndDisconnect interaction (confirmed
// empirically with `go test -race`; see the package doc and
// sshTunnelDialer.forceClose's doc). evictSettled refuses to touch anything
// that is not yet a fully established, in-use connection, so evict can never
// be the trigger for that window — a concurrent in-flight attempt is simply
// left alone to settle or fail on its own.
//
// Closing the raw conn directly, rather than calling client.Disconnect, is
// also deliberate: Disconnect's first act is a ProcConnectClose RPC that
// WAITS FOR A REPLY before closing the socket — on the exact wedged
// connection eviction exists to escape, that wait can itself hang, defeating
// the entire point of the watchdog. Closing the raw conn instead fails the
// socket's blocked Read immediately, which drives go-libvirt's own cleanup
// (deregisterAll) and completes every in-flight call with an error.
//
// Closing the transport is synchronous, but go-libvirt's own listen goroutine
// noticing the severed conn and flipping IsConnected() false afterward is
// NOT: there is a brief, self-correcting window where a caller that checks
// IsConnected() immediately after evict returns can still observe true. This
// is harmless — the next real operation against the connection fails and
// converges the state — and deliberate: waiting HERE for that window to
// close would mean reading Disconnected(), which only connect is allowed to
// touch (see above).
func (h *golibvirtHolder) evict() {
	if h.dialer.evictSettled() {
		h.logger.Warn("evicted go-libvirt connection")
		return
	}
	// Nothing settled to evict — either already dead/never connected, or a
	// DIFFERENT attempt is currently mid-handshake and is deliberately left
	// alone (see doc above). Either way there is nothing unsafe left to do
	// here.
	h.logger.Warn("go-libvirt evict: no settled connection to evict (already down, or a redial is currently in flight)")
}

// currentClient safely reads the current published client (nil before the
// first successful connect). It exists so callers outside this file — and
// tests — never need to reach into h.client directly, which would bypass the
// h.mu synchronization connect's invariant depends on (package doc).
func (h *golibvirtHolder) currentClient() *golibvirt.Libvirt {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.client
}

// startKeepaliveLocked starts the keepalive/redial goroutine if it is not
// already running. Callers must hold h.mu. It runs at most once per holder
// for the holder's whole life: once started it never exits until close,
// self-healing through every subsequent reconnect rather than being
// restarted per-connection. It is only ever started from a SUCCESSFUL
// connect (never eagerly at construction), which is what keeps the whole
// go-libvirt subsystem dormant — no goroutine, no dial — until something
// actually calls Libvirt(ctx) for the first time.
func (h *golibvirtHolder) startKeepaliveLocked() {
	if h.keepaliveOn {
		return
	}
	h.keepaliveOn = true
	go h.keepaliveLoop()
}

// keepaliveLoop is the answer to both "no keepalive" and "no auto-reconnect"
// (ADR-0008 Fact 5). Every keepaliveInterval it (1) calls connect, which is a
// cheap no-op if the connection is already up and a full redial if it is
// not — this is the redial-on-dead half, driven by IsConnected exactly as the
// deliverable asks, just reached through connect rather than a second,
// separately-synchronized read of it (see the package doc's "why
// connect/close/evict never touch h.client concurrently" section) — and then
// (2), once connected, probes ConnectGetLibVersion through the same watchdog
// every other call uses. A failed redial is simply retried next tick: the
// ticker interval itself is the backoff, so a persistently unreachable host
// is retried steadily, never busy-looped. It runs until h.rootCtx is
// cancelled (holder close).
func (h *golibvirtHolder) keepaliveLoop() {
	ticker := time.NewTicker(h.keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.rootCtx.Done():
			return

		case <-ticker.C:
			connectCtx, cancel := context.WithTimeout(h.rootCtx, h.connectTimeout)
			_, err := h.connect(connectCtx)
			cancel()
			if err != nil {
				if errors.Is(err, errGolibvirtHolderClosed) {
					return
				}
				h.logger.Warn("go-libvirt keepalive: redial failed, will retry next tick", "error", err)
				continue
			}

			probeCtx, cancel := context.WithTimeout(h.rootCtx, h.probeTimeout)
			err = h.call(probeCtx, func(c *golibvirt.Libvirt) error {
				_, err := c.ConnectGetLibVersion()
				return err
			})
			cancel()
			if err != nil {
				h.logger.Warn("go-libvirt keepalive probe failed", "error", err)
				// A ctx-deadline failure already evicted inside call(). A
				// non-deadline RPC error means the transport is still up but
				// libvirtd itself returned an error, which still warrants
				// dropping the connection so the next tick redials rather
				// than repeating the same failure against a
				// technically-connected-but-unhealthy client.
				h.evict()
			}
		}
	}
}

// errGolibvirtHolderClosed is returned (wrapped) by connect/ensureConnected
// once close has run, so keepaliveLoop can tell "the holder was deliberately
// torn down" apart from "this attempt failed, try again" and exit instead of
// spinning forever against a holder nobody will ever use again.
var errGolibvirtHolderClosed = errors.New("golibvirt: holder is closed")

// close stops the keepalive goroutine and force-closes the connection. It is
// safe to call more than once (Registry Evict/Close semantics) and safe to
// call on a holder that never connected.
//
// If a connect attempt is CURRENTLY in flight, close waits — bounded by
// connectTimeout — for it to finish before forceClose-ing anything. This is
// not the same concern connect's fresh-client-per-attempt design solves
// (that is about two DIFFERENT *golibvirt.Libvirt objects never sharing
// state): it is about not yanking the transport out from under a handshake
// that is on the verge of completing on THIS SAME object. go-libvirt's
// ConnectToURI spawns its background watcher (waitAndDisconnect) BEFORE the
// `disconnected` field it later writes on success is itself synchronized
// against that watcher's own read of the same field — so a transport that
// dies at EXACTLY the moment a handshake succeeds can make ConnectToURI's
// success-path write race the watcher's read, independent of anything this
// package does to serialize its OWN callers (confirmed empirically with `go
// test -race`; it reproduces even with connect's own synchronization
// correct, because both racing goroutines are go-libvirt's own). Waiting out
// an in-flight attempt before forceClose avoids being the trigger for that
// window — the same two-stage "wait first, force second" shape
// reapAbandonedConnect uses for connect's own ctx.Done() branch, for the
// same reason. It reduces the risk to the same "would require the handshake
// to still be unfinished after a full extra connectTimeout" level the reaper
// accepts, rather than eliminating it outright — go-libvirt gives this
// package no primitive that would let it do better than that. This code path
// only runs once, at process teardown, so that residual is accepted here
// too.
func (h *golibvirtHolder) close() {
	h.mu.Lock()
	h.closed = true
	connecting := h.connecting
	done := h.connectDone
	h.mu.Unlock()

	h.rootCancel()

	if connecting {
		select {
		case <-done:
		case <-time.After(h.connectTimeout):
			h.logger.Warn("go-libvirt close: an in-flight connect attempt did not finish in time; forcing transport closed anyway")
		}
	}

	h.mu.Lock()
	client := h.client
	h.mu.Unlock()

	h.dialer.forceClose()
	h.dialer.resetTransport()

	if client != nil {
		_ = client.Disconnect() // best-effort; safe on an already-disconnected client (Disconnect tolerates syscall.EINVAL)
	}
}

// Libvirt returns the provider's go-libvirt client (ADR-0008 PR 4a),
// connecting lazily over the persistent SSH client on first use. golibvirt is
// constructed here, on the FIRST call — not in NewVirshProvider — so a
// process that never calls Libvirt never dials, never starts the keepalive
// goroutine, and never links go-libvirt into its runtime behavior at all; see
// the package doc above for the full lifecycle. virshConn.Libvirt (conn.go)
// is the hostconn.Conn seam method that reaches here; VirshProvider.Cleanup
// closes the holder this creates.
func (v *VirshProvider) Libvirt(ctx context.Context) (*golibvirt.Libvirt, error) {
	v.golibvirtMu.Lock()
	if v.golibvirt == nil {
		v.golibvirt = newGolibvirtHolder(v)
	}
	holder := v.golibvirt
	v.golibvirtMu.Unlock()

	return holder.ensureConnected(ctx)
}

// callLibvirt runs fn against this host's go-libvirt client under the holder's
// ctx-deadline watchdog (golibvirtHolder.call). This is the ctx-bounded invocation
// the hostconn.Conn.Libvirt doc directs callers to use instead of driving the raw
// client Libvirt(ctx) returns: go-libvirt has no per-call deadline of its own
// (ADR-0008 Fact 5), so ctx bounds a call here only because call runs fn in a
// goroutine and evicts the connection if ctx fires first. Cancellation is
// therefore CONNECTION-scoped (a timeout drops the whole connection, failing any
// concurrent sibling) — the documented, accepted blast radius, see call's doc.
//
// It connects lazily on first use exactly like Libvirt(ctx); a process that never
// calls either keeps go-libvirt fully dormant. ADR-0008 PR 4b's shadow-compare
// Describe is the first caller.
func (v *VirshProvider) callLibvirt(ctx context.Context, fn func(*golibvirt.Libvirt) error) error {
	v.golibvirtMu.Lock()
	if v.golibvirt == nil {
		v.golibvirt = newGolibvirtHolder(v)
	}
	holder := v.golibvirt
	v.golibvirtMu.Unlock()

	return holder.call(ctx, fn)
}
