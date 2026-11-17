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
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/clustered/inventoryfile"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// freezer is a TCP forwarder in front of an agent that can go silent: while
// frozen it keeps every connection open and forwards nothing, in either
// direction, as a host whose network path black-holes after the connection
// was made.
type freezer struct {
	lis    net.Listener
	target string
	frozen atomic.Bool
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  []net.Conn
}

func newFreezer(t *testing.T, target string) *freezer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &freezer{lis: l, target: target}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			f.mu.Lock()
			f.conns = append(f.conns, c, up)
			f.mu.Unlock()
			f.wg.Add(2)
			go f.pipe(up, c)
			go f.pipe(c, up)
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		f.mu.Lock()
		for _, c := range f.conns {
			_ = c.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
	return f
}

// pipe copies src to dst, dropping what it reads while frozen.
func (f *freezer) pipe(dst, src net.Conn) {
	defer f.wg.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !f.frozen.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// TestSilentHostAfterReady: a host that goes silent on an open, READY
// connection is answered host-scoped within the Provider's call deadline,
// whatever the caller's deadline; the expiry counts toward that host's
// breaker only, and the other host is unaffected.
func TestSilentHostAfterReady(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	b := newFakeAgent(t, p, "host-b", fakeAgentOptions{})
	fz := newFreezer(t, b.lis.Addr().String())
	eb := b.entry()
	eb.Endpoint = "grpc://" + fz.lis.Addr().String()
	prov, err := New(Options{ClientCertificate: p.client, Logger: quiet(), CallTimeout: 300 * time.Millisecond,
		Breaker: &resilience.Config{FailureThreshold: 2, ResetTimeout: time.Hour, HalfOpenMaxCalls: 1}},
		hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{a.entry(), eb}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = prov.Close() })
	client := providerv1.NewProviderClient(served(t, prov))
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
	require.NoError(t, err, "the connection is READY")

	fz.frozen.Store(true)
	start := time.Now()
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
	requireHostUnavailable(t, err, CauseAgentTimeout)
	_, err = client.Power(ctx(t), signedPower(t, "host-b"))
	_, ok := errorInfo(err, contracts.OutcomeUnknownRetryReason)
	assert.True(t, ok, "a sent mutation that timed out has an unknown outcome: %v", err)
	assert.Less(t, time.Since(start), 3*time.Second, "both calls answered within the call deadline")
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
	requireHostUnavailable(t, err, CauseBreakerOpen)
	for i := 0; i < 3; i++ {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err, "the other host is unaffected")
	}
	e, _ := prov.pool.get("host-a")
	assert.Equal(t, resilience.StateClosed, e.breaker.GetState())
}

// TestCallDeadlineFollowsCaller: the Provider's deadline is the caller's
// less a margin when that is sooner than its call timeout, so the caller
// still gets the host-scoped answer.
func TestCallDeadlineFollowsCaller(t *testing.T) {
	p := newPKI(t)
	b := newFakeAgent(t, p, "host-b", fakeAgentOptions{})
	fz := newFreezer(t, b.lis.Addr().String())
	eb := b.entry()
	eb.Endpoint = "grpc://" + fz.lis.Addr().String()
	prov, err := New(Options{ClientCertificate: p.client, Logger: quiet()},
		hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{eb}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = prov.Close() })
	client := providerv1.NewProviderClient(served(t, prov))
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
	require.NoError(t, err)
	fz.frozen.Store(true)
	c, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err = client.Describe(c, &providerv1.DescribeRequest{TargetHostId: "host-b"})
	requireHostUnavailable(t, err, CauseAgentTimeout)
}

// TestUnwatchedInventoryStillReloads: when the inventory file cannot be
// watched, the periodic re-read still removes a host and closes its
// connection, and Validate says the file is not watched.
func TestUnwatchedInventoryStillReloads(t *testing.T) {
	orig := newInventoryWatcher
	newInventoryWatcher = func(string, inventoryfile.Reconciler, *slog.Logger) (*inventoryfile.Watcher, error) {
		return nil, os.ErrPermission
	}
	t.Cleanup(func() { newInventoryWatcher = orig })

	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	b := newFakeAgent(t, p, "host-b", fakeAgentOptions{})
	path := filepath.Join(t.TempDir(), "hosts.json")
	write := func(hosts ...hostsecret.Host) {
		data, err := hostsecret.Marshal(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: hosts})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
	write(a.entry(), b.entry())
	prov, err := NewFromFile(path, Options{ClientCertificate: p.client, Logger: quiet(), InventoryReloadInterval: 50 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = prov.Close() })
	client := providerv1.NewProviderClient(served(t, prov))
	v, err := client.Validate(ctx(t), &providerv1.ValidateRequest{})
	require.NoError(t, err)
	assert.Contains(t, v.GetMessage(), "not watched")
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
	require.NoError(t, err)
	require.EqualValues(t, 1, b.lis.open.Load())

	write(a.entry())
	require.Eventually(t, func() bool { return b.lis.open.Load() == 0 }, testTimeout, 10*time.Millisecond,
		"the re-read closes the removed host's connection")
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-b"})
	requireHostUnavailable(t, err, CauseNotFronted)
}

// TestQuickRestartNeedsNewHello: an agent that restarts as an incompatible
// release behind a quick reconnect is never served under the earlier Hello:
// no call reaches it.
func TestQuickRestartNeedsNewHello(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{}, a)))
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	before := len(a.received())
	a.apiVersion.Store(answer.AgentAPIVersion + 1)
	a.lis.dropAll()
	refused := false
	for i := 0; i < 50 && !refused; i++ {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.Error(t, err, "no call is served by the restarted agent")
		info, ok := errorInfo(err, contracts.HostUnavailableReason)
		refused = ok && info.GetMetadata()[MetadataCause] == CauseAgentIncompatible
	}
	assert.True(t, refused, "the new transport's Hello refuses the agent")
	assert.Len(t, a.received(), before, "the restarted agent received no call")
}

// TestClosedEntryRefused: a call that still holds an entry the inventory
// removed is refused without connecting.
func TestClosedEntryRefused(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	prov := newProvider(t, p, Options{}, a)
	e, ok := prov.pool.get("host-a")
	require.True(t, ok)
	require.NoError(t, prov.Reconcile(hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}))
	cs, err := prov.pool.invoke(ctx(t), e, providerv1.Provider_Describe_FullMethodName, &providerv1.DescribeRequest{}, &providerv1.DescribeResponse{}, nil)
	require.ErrorIs(t, err, errEntryClosed)
	requireHostUnavailable(t, mapCallError(ctx(t), "host-a", false, cs, err), CauseNotFronted)
	assert.Zero(t, a.lis.accepted.Load())
}

// TestConnectionReplacedWhenOldOrExpired: a connection older than the
// maximum age, or whose agent certificate has expired, is replaced by a new
// one (a new handshake verifies the agent again) before the next call.
func TestConnectionReplacedWhenOldOrExpired(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	prov := newProvider(t, p, Options{MaxConnectionAge: time.Hour, Now: now}, a)
	client := providerv1.NewProviderClient(served(t, prov))
	call := func() {
		t.Helper()
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.NoError(t, err)
	}
	call()
	call()
	require.EqualValues(t, 1, a.lis.accepted.Load())
	offset.Store(int64(2 * time.Hour))
	call()
	assert.EqualValues(t, 2, a.lis.accepted.Load(), "an old connection is replaced")
	assert.EqualValues(t, 2, a.hellos.Load(), "with a new Hello")

	// Past the agent certificate's expiry (but judged by the handshake at
	// the real clock), the connection is replaced again; the new handshake
	// refuses the expired certificate.
	offset.Store(int64(time.Until(a.leaf.NotAfter) + time.Minute))
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	requireHostUnavailable(t, err, CauseAgentCertificateRefused)
}

// TestHelloWaitIsContextAware: a caller waiting for another caller's Hello
// of the same host gives up when its context ends.
func TestHelloWaitIsContextAware(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	prov := newProvider(t, p, Options{}, a)
	e, _ := prov.pool.get("host-a")
	e.helloSlot <- struct{}{}
	c, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := prov.pool.helloed(c, e)
	assert.Equal(t, codes.DeadlineExceeded, status.Code(err))
	assert.Less(t, time.Since(start), 2*time.Second)
	<-e.helloSlot
}

// TestListHostsHellosInParallel: ListHosts makes the Hellos it needs at the
// same time, within one overall budget.
func TestListHostsHellosInParallel(t *testing.T) {
	orig := listHostsHelloTimeout
	listHostsHelloTimeout = 400 * time.Millisecond
	t.Cleanup(func() { listHostsHelloTimeout = orig })
	p := newPKI(t)
	var agents []*fakeAgent
	for _, h := range []string{"host-a", "host-b", "host-c", "host-d"} {
		a := newFakeAgent(t, p, h, fakeAgentOptions{})
		a.helloBlocks.Store(true)
		agents = append(agents, a)
	}
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{}, agents...)))
	start := time.Now()
	resp, err := client.ListHosts(ctx(t), &providerv1.ListHostsRequest{})
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 1200*time.Millisecond, "four hung Hellos take one budget, not four")
	require.Len(t, resp.GetHosts(), 4)
	for _, h := range resp.GetHosts() {
		assert.Equal(t, providerv1.HostHealth_HOST_HEALTH_NOT_READY, h.GetHealth())
	}
}

// TestIncompleteGrantRefusedWithoutDialing: an envelope without a complete
// grant is refused GRANT_UNAUTHENTICATED, as the host would, without dialing.
func TestIncompleteGrantRefusedWithoutDialing(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{}, a)))
	for _, g := range []*providerv1.OperationGrant{nil, {Signature: []byte("s"), SignerCertChain: [][]byte{{1}}},
		{Body: []byte("b"), SignerCertChain: [][]byte{{1}}}, {Body: []byte("b"), Signature: []byte("s")}} {
		req := signedPower(t, "host-a")
		req.OperationEnvelope.Grant = g
		_, err := client.Power(ctx(t), req)
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
		_, ok := errorInfo(err, contracts.GrantUnauthenticatedReason)
		assert.True(t, ok, "%v", err)
	}
	assert.Zero(t, a.lis.accepted.Load())
}

// TestHelloBoundToConnectionGeneration: once an old connection is replaced
// (by age), the Hello made on the new one never vouches for a call that
// still holds the old one, and the old connection's Hello is gone.
func TestHelloBoundToConnectionGeneration(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	prov := newProvider(t, p, Options{MaxConnectionAge: time.Hour, Now: now}, a)
	client := providerv1.NewProviderClient(served(t, prov))
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	e, _ := prov.pool.get("host-a")
	_, oldGen, err := e.connection()
	require.NoError(t, err)
	require.True(t, e.helloedFor(oldGen))

	offset.Store(int64(2 * time.Hour))
	_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	_, newGen, err := e.connection()
	require.NoError(t, err)
	require.NotEqual(t, oldGen, newGen, "the old connection was replaced")
	assert.True(t, e.helloedFor(newGen), "the new connection has its own Hello")
	assert.False(t, e.helloedFor(oldGen), "a call holding the old connection is not vouched for by the new Hello")
	assert.EqualValues(t, 2, a.hellos.Load())
}

// fixedAddrConn reports the same local address for every connection.
type fixedAddrConn struct{ net.Conn }

func (fixedAddrConn) LocalAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

// TestHelloNotKeyedByAddress: with a dialer whose connections all report one
// local address, an agent that restarts as an incompatible release is still
// never served under the earlier Hello.
func TestHelloNotKeyedByAddress(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		return fixedAddrConn{c}, nil
	}
	client := providerv1.NewProviderClient(served(t, newProvider(t, p, Options{DialContext: dial}, a)))
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	require.NoError(t, err)
	before := len(a.received())
	a.apiVersion.Store(answer.AgentAPIVersion + 1)
	a.lis.dropAll()
	refused := false
	for i := 0; i < 50 && !refused; i++ {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		require.Error(t, err, "no call is served by the restarted agent")
		info, ok := errorInfo(err, contracts.HostUnavailableReason)
		refused = ok && info.GetMetadata()[MetadataCause] == CauseAgentIncompatible
	}
	assert.True(t, refused)
	assert.Len(t, a.received(), before)
	assert.GreaterOrEqual(t, a.hellos.Load(), int32(2), "the new transport got its own Hello")
}

// TestRefusedEntriesLoggedOnce: a refused entry is logged once, not at every
// reload, and a malformed id is never logged verbatim.
func TestRefusedEntriesLoggedOnce(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	p := newPKI(t)
	bad := hostsecret.Host{ID: "bad/../id", Endpoint: "grpc://x:1"}
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion, Hosts: []hostsecret.Host{bad}}
	prov, err := New(Options{ClientCertificate: p.client, Logger: log}, inv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = prov.Close() })
	for i := 0; i < 3; i++ {
		require.NoError(t, prov.Reconcile(inv))
	}
	out := buf.String()
	assert.Equal(t, 1, strings.Count(out, "entry refused"), out)
	assert.NotContains(t, out, "bad/../id")
	assert.Contains(t, out, "invalid-id-")
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestAgentRefusingTheProviderCertificateHasItsOwnCause: an agent that
// refuses the Provider's identity certificate after the TLS 1.3 handshake
// (a URI without the Provider UID, a Provider it does not allow) is reported
// as ProviderCertificateRefused, not as unreachable, for reads and mutating
// calls alike, and receives no call.
func TestAgentRefusingTheProviderCertificateHasItsOwnCause(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{refuseClient: true})
	prov := newProvider(t, p, Options{}, a)
	client := providerv1.NewProviderClient(served(t, prov))
	// The agent's alert usually reaches the Provider; when the agent closes
	// the connection before the Provider's first write lands, the Provider
	// reads a reset instead and reports AgentUnreachable (the safe side:
	// never "refused" without the alert). Every answer is host-scoped, and
	// the alert is seen within a few calls, reads and mutations alike.
	causes := map[string]int{}
	for i := 0; i < 8 && causes[CauseProviderCertificateRefused] < 2; i++ {
		var err error
		if i%2 == 0 {
			_, err = client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		} else {
			_, err = client.Power(ctx(t), signedPower(t, "host-a"))
		}
		info, ok := errorInfo(err, contracts.HostUnavailableReason)
		require.True(t, ok, "host-scoped: %v", err)
		c := info.GetMetadata()[MetadataCause]
		require.Contains(t, []string{CauseProviderCertificateRefused, CauseAgentUnreachable, CauseBreakerOpen}, c, "%v", err)
		causes[c]++
	}
	assert.GreaterOrEqual(t, causes[CauseProviderCertificateRefused], 2, "the refusal has its own cause: %v", causes)
	assert.Empty(t, a.received(), "the agent received no call")
}

// scriptedConn returns its reads in order, then their errors.
type scriptedConn struct {
	net.Conn
	reads []struct {
		data string
		err  error
	}
}

func (c *scriptedConn) Read(b []byte) (int, error) {
	r := c.reads[0]
	c.reads = c.reads[1:]
	return copy(b, r.data), r.err
}

// TestAlertConnReportsOnlyARefusalBeforeAnyData: a fatal TLS alert read
// before the agent sent anything is a refusal of the Provider's certificate
// (nothing ran); one read after the agent's first data is not, because a
// call on that connection may have run.
func TestAlertConnReportsOnlyARefusalBeforeAnyData(t *testing.T) {
	alert := errors.New("remote error: tls: bad certificate")
	var n int
	c := &alertConn{Conn: &scriptedConn{reads: []struct {
		data string
		err  error
	}{{"", alert}}}, onAlert: func() { n++ }}
	_, _ = c.Read(make([]byte, 8))
	assert.Equal(t, 1, n, "an alert before any data is a refusal")

	n = 0
	c = &alertConn{Conn: &scriptedConn{reads: []struct {
		data string
		err  error
	}{{"settings", nil}, {"", errors.New("remote error: tls: bad record MAC")}}}, onAlert: func() { n++ }}
	_, _ = c.Read(make([]byte, 8))
	_, _ = c.Read(make([]byte, 8))
	assert.Zero(t, n, "an alert after the agent's first data is not a refusal")

	n = 0
	c = &alertConn{Conn: &scriptedConn{reads: []struct {
		data string
		err  error
	}{{"", io.EOF}}}, onAlert: func() { n++ }}
	_, _ = c.Read(make([]byte, 8))
	assert.Zero(t, n, "a close is not a refusal")
}

// TestHandshakeAlertIsNotARefusal: an agent that ends a handshake with an
// internal error alert (its signer is away, or it is over its handshake cap)
// has not refused the Provider's certificate. gRPC redials the same
// connection; once that succeeds, a mutating call that was sent and never
// answered has an unknown outcome, never "definitely not performed".
func TestHandshakeAlertIsNotARefusal(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{failHandshakes: 1})
	prov := newProvider(t, p, Options{}, a)
	client := providerv1.NewProviderClient(served(t, prov))
	_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
	if err != nil {
		requireHostUnavailable(t, err, CauseAgentUnreachable)
	}
	require.Eventually(t, func() bool {
		_, err := client.Describe(ctx(t), &providerv1.DescribeRequest{TargetHostId: "host-a"})
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the redial succeeds")
	require.EqualValues(t, 1, a.handshakesFailed.Load(), "the first handshake ended with an alert")
	e, _ := prov.pool.get("host-a")
	_, gen, err := e.connection()
	require.NoError(t, err)
	assert.False(t, e.refusedOn(gen), "a handshake alert does not mark the connection refused")

	// The agent receives the mutating call and drops every connection
	// without answering.
	a.setRespond(func(method string, raw []byte) (proto.Message, error) {
		if strings.HasSuffix(method, "/Power") {
			a.lis.dropAll()
			return nil, errors.New("dropped")
		}
		return defaultRespond(method, raw)
	})
	_, err = client.Power(ctx(t), signedPower(t, "host-a"))
	_, ok := errorInfo(err, contracts.OutcomeUnknownRetryReason)
	assert.True(t, ok, "a sent, unanswered mutation has an unknown outcome: %v", err)
	_, refused := errorInfo(err, contracts.HostUnavailableReason)
	assert.False(t, refused, "never a host-scoped refusal: %v", err)
}

// TestOnlyCertificateAlertsAreRefusals: of the fatal alerts an agent can
// send before its first frame, only the certificate alerts mark a refusal;
// an internal error (the signer away, the handshake cap) never does.
func TestOnlyCertificateAlertsAreRefusals(t *testing.T) {
	for alert, want := range map[string]bool{
		"bad certificate": true, "certificate required": true, "unknown certificate authority": true,
		"certificate unknown": true, "access denied": true,
		"internal error": false, "handshake failure": false, "protocol version not supported": false, "decrypt error": false,
	} {
		err := errors.New("read tcp 127.0.0.1:1->127.0.0.1:2: remote error: tls: " + alert)
		assert.Equal(t, want, isCertificateAlert(err), alert)
		n := 0
		c := &alertConn{Conn: &scriptedConn{reads: []struct {
			data string
			err  error
		}{{"", err}}}, onAlert: func() { n++ }}
		_, _ = c.Read(make([]byte, 8))
		assert.Equal(t, want, n == 1, alert)
	}
	assert.False(t, isCertificateAlert(errors.New("bad certificate")), "only an alert the peer sent")
}

// TestTransportBeganClearsARefusal: a refusal marks one transport; a new
// transport of the same connection starts unrefused.
func TestTransportBeganClearsARefusal(t *testing.T) {
	p := newPKI(t)
	a := newFakeAgent(t, p, "host-a", fakeAgentOptions{})
	prov := newProvider(t, p, Options{}, a)
	e, _ := prov.pool.get("host-a")
	e.certificateRefused(e.gen)
	require.True(t, e.refusedOn(e.gen))
	e.transportBegan(e.gen)
	assert.False(t, e.refusedOn(e.gen))
}
