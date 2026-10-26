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

package grpc

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// countingListener counts the connections it accepts and those closed.
type countingListener struct {
	net.Listener
	accepted atomic.Int32
	closed   atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted.Add(1)
	return &countingConn{Conn: c, l: l}, nil
}

type countingConn struct {
	net.Conn
	l    *countingListener
	once sync.Once
}

func (c *countingConn) Close() error {
	c.once.Do(func() { c.l.closed.Add(1) })
	return c.Conn.Close()
}

// TestReadCapabilities_OneConnectionPerRead pins that a capability read opens
// a connection of its own and closes it: two reads are two connections, so a
// read after a provider rollout is never answered over a connection still held
// by a pod of the superseded ReplicaSet.
func TestReadCapabilities_OneConnectionPerRead(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	lis := &countingListener{Listener: raw}
	linked := atomic.Bool{}
	linked.Store(true)
	gsrv := grpc.NewServer()
	providerv1.RegisterProviderServer(gsrv, &fakeProviderServer{
		GetCapabilitiesFn: func(context.Context, *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
			return &providerv1.GetCapabilitiesResponse{
				SupportsLinkedClones:      linked.Load(),
				SupportsHonestReconfigure: true,
				SupportedExportBackends:   []string{"s3"},
			}, nil
		},
	})
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	caps, err := ReadCapabilities(ctx, raw.Addr().String(), "read-caps-test", nil)
	require.NoError(t, err)
	assert.True(t, caps.SupportsLinkedClones)
	assert.True(t, caps.SupportsHonestReconfigure)
	assert.Equal(t, []string{"s3"}, caps.SupportedExportBackends)

	linked.Store(false)
	caps, err = ReadCapabilities(ctx, raw.Addr().String(), "read-caps-test", nil)
	require.NoError(t, err)
	assert.False(t, caps.SupportsLinkedClones)

	assert.Equal(t, int32(2), lis.accepted.Load(), "each read opens a connection of its own")
	assert.Eventually(t, func() bool { return lis.closed.Load() == 2 }, 5*time.Second, 20*time.Millisecond,
		"each read closes its connection")
}

// TestReadCapabilities_Unreachable pins that a read of an endpoint nothing
// listens on fails (the caller keeps what it recorded) and returns.
func TestReadCapabilities_Unreachable(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := raw.Addr().String()
	require.NoError(t, raw.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = ReadCapabilities(ctx, addr, "read-caps-test", nil)
	require.Error(t, err)
}
