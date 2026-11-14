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

package agenttarget

import (
	"io"
	"net"
	"sync"
	"time"
)

// proxyDialTimeout bounds the proxy's connection to an agent.
const proxyDialTimeout = 5 * time.Second

// proxy is a loopback TCP forwarder in front of one agent: the Provider
// dials the proxy, so that the Target can make the host unreachable (every
// connection cut and new ones refused) or a black hole (connections accepted,
// nothing forwarded), and the agent can restart on another port.
type proxy struct {
	lis      net.Listener
	upstream func() string

	mu        sync.Mutex
	down      bool
	blackhole bool
	closed    bool
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
}

// newProxy listens on a loopback port and forwards to the address upstream
// returns at each connection.
func newProxy(upstream func() string) (*proxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &proxy{lis: l, upstream: upstream, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

// Addr is the address the Provider dials.
func (p *proxy) Addr() string { return p.lis.Addr().String() }

// serve accepts until the listener is closed.
func (p *proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.lis.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			_ = c.Close()
			continue
		}
		p.wg.Add(1)
		go p.handle(c)
	}
}

// track records c, unless the proxy is down or closed.
func (p *proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down || p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

// untrack forgets and closes c.
func (p *proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	_ = c.Close()
}

// handle forwards c to the agent, or holds it open without forwarding while
// the proxy is a black hole, until either side closes or the proxy cuts it.
func (p *proxy) handle(c net.Conn) {
	defer p.wg.Done()
	defer p.untrack(c)
	p.mu.Lock()
	hole := p.blackhole
	p.mu.Unlock()
	if hole {
		_, _ = io.Copy(io.Discard, c)
		return
	}
	up, err := net.DialTimeout("tcp", p.upstream(), proxyDialTimeout)
	if err != nil {
		return
	}
	if !p.track(up) {
		_ = up.Close()
		return
	}
	defer p.untrack(up)
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	<-done
	// One direction ended: close both, which ends the other copy.
	_ = c.Close()
	_ = up.Close()
	<-done
}

// cutAll closes every open connection.
func (p *proxy) cutAll() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// setDown makes the host unreachable (every connection cut, new ones
// refused) or reachable again.
func (p *proxy) setDown(down bool) {
	p.mu.Lock()
	p.down = down
	p.mu.Unlock()
	if down {
		p.cutAll()
	}
}

// setBlackhole makes the proxy accept connections and forward nothing, or
// forward again; the open connections are cut either way.
func (p *proxy) setBlackhole(on bool) {
	p.mu.Lock()
	p.blackhole = on
	p.mu.Unlock()
	p.cutAll()
}

// Close stops the proxy and waits for its goroutines.
func (p *proxy) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	_ = p.lis.Close()
	p.cutAll()
	p.wg.Wait()
}
