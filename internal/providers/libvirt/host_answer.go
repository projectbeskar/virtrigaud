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

package libvirt

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
)

// Waits on a clustered host that end before the call's deadline (V3c).
//
// Before a command of a routed call runs on its host, the call waits for the
// host three times: for one of the host's command slots (execSem), for the SSH
// dial and handshake (a new connection), and for an SSH session on the
// connection. A host that stops answering makes each of these waits last:
// the session open on a connection whose host is gone waits for the TCP stack
// to give up (minutes), and the calls stuck there hold the host's command
// slots, so every later call waits for a slot until its own deadline. Such a
// call used to end with the call's context error — which is not a host failure
// (isHostTransportFailure) — and was answered codes.Unknown + VM_OPERATION_FAILED
// ("failed to describe VM on host ..."), at the very moment the manager's own
// deadline ran out, while a call that found the dead connection at once was
// answered HOST_UNAVAILABLE.
//
// On a clustered provider's host connection (VirshProvider.boundHostWaits)
// each of those waits now ends hostAnswerMargin before the call's deadline,
// and a wait that ends there is a host that did not answer
// (hostNotAnsweringError): a transport failure that ran nothing, answered by
// how far the call got (hostOpRPCErrorFor, R25) — HOST_UNAVAILABLE for a read
// or a call that ran nothing — early enough to reach the manager. A call whose
// own context ends first keeps the context error, as before. The single-host
// provider is unchanged.

// hostAnswerMargin is how long before a call's deadline its waits for a
// clustered host end (at most a quarter of the time the call has left), so the
// answer reaches the manager before the manager gives up on the call.
const hostAnswerMargin = 5 * time.Second

// What a call was waiting for when its host did not answer
// (hostNotAnsweringError.wait). Provider log only.
const (
	hostWaitExecSlot = "a command slot of the host"
	hostWaitDial     = "the SSH dial and handshake"
	hostWaitSession  = "an SSH session"
)

// hostNotAnsweringError reports that a wait on a clustered host (wait) did not
// end before hostAnswerDeadline: the host did not answer in the call's time.
// Nothing was run by the command that waited. isHostTransportFailure counts it
// as a host failure; it never wraps the call's context error.
type hostNotAnsweringError struct {
	// wait names what the call waited for (hostWait*).
	wait string
}

// Error describes the wait that ran out.
func (e *hostNotAnsweringError) Error() string {
	return fmt.Sprintf("the host did not answer within the call's time (waiting for %s)", e.wait)
}

// isHostNotAnswering reports whether err's chain holds a hostNotAnsweringError.
func isHostNotAnswering(err error) bool {
	var hna *hostNotAnsweringError
	return stderrors.As(err, &hna)
}

// hostAnswerDeadline returns when a wait on the host must end for a call whose
// context is ctx: hostAnswerMargin before ctx's deadline, or a quarter of the
// time left when that is shorter. ok is false when ctx has no deadline (the
// wait is then bounded by ctx alone).
func hostAnswerDeadline(ctx context.Context) (deadline time.Time, ok bool) {
	d, ok := ctx.Deadline()
	if !ok {
		return time.Time{}, false
	}
	margin := hostAnswerMargin
	if left := time.Until(d); left <= 0 {
		margin = 0
	} else if left/4 < margin {
		margin = left / 4
	}
	return d.Add(-margin), true
}

// hostWaitContext returns the context a wait on v's host runs under: ctx
// bounded to hostAnswerDeadline on a clustered host connection
// (boundHostWaits), ctx itself otherwise. The cancel func must be called.
func (v *VirshProvider) hostWaitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if !v.boundHostWaits {
		return ctx, func() {}
	}
	d, ok := hostAnswerDeadline(ctx)
	if !ok {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, d)
}

// hostWaitError is the error of a wait on v's host for wait that ended because
// its context (wctx, from hostWaitContext) is done: the call's own context
// error when the call ended, or a hostNotAnsweringError when only the wait's
// bound ran out.
func (v *VirshProvider) hostWaitError(ctx, wctx context.Context, wait string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.boundHostWaits && wctx.Err() != nil {
		return &hostNotAnsweringError{wait: wait}
	}
	return wctx.Err()
}

// openSession opens an SSH session on client. On a clustered host connection
// (boundHostWaits) the open is bounded by hostWaitContext: a host that does
// not confirm the session in time is a hostNotAnsweringError. The goroutine
// that runs the open outlives such a wait only until NewSession returns — when
// the host answers, or the connection fails — and closes a session it got
// then, so nothing is leaked past the connection's own life. A single-host
// connection opens the session as before (no bound).
func (v *VirshProvider) openSession(ctx context.Context, client *ssh.Client) (*ssh.Session, error) {
	if !v.boundHostWaits {
		return client.NewSession()
	}
	wctx, cancel := v.hostWaitContext(ctx)
	defer cancel()
	type opened struct {
		sess *ssh.Session
		err  error
	}
	ch := make(chan opened, 1)
	go func() {
		sess, err := client.NewSession()
		ch <- opened{sess: sess, err: err}
	}()
	select {
	case o := <-ch:
		return o.sess, o.err
	case <-wctx.Done():
		go func() {
			if o := <-ch; o.sess != nil {
				_ = o.sess.Close()
			}
		}()
		return nil, v.hostWaitError(ctx, wctx, hostWaitSession)
	}
}

// handshakeDeadline is the deadline set on a new connection's socket for the
// SSH handshake on a clustered host connection: sshHandshakeTimeout from now,
// or the wait's bound (dctx's deadline) when that is earlier. ok is false on a
// single-host connection, whose handshake is not bounded.
func (v *VirshProvider) handshakeDeadline(dctx context.Context) (deadline time.Time, ok bool) {
	if !v.boundHostWaits {
		return time.Time{}, false
	}
	deadline = time.Now().Add(sshHandshakeTimeout)
	if d, has := dctx.Deadline(); has && d.Before(deadline) {
		deadline = d
	}
	return deadline, true
}

// dialWaitError is the error of a failed dial or handshake on v's host:
// a hostNotAnsweringError when, on a clustered host connection, the wait's
// bound (until) had passed while the call itself was still running, so a
// retry would not be in time; err otherwise.
func (v *VirshProvider) dialWaitError(ctx context.Context, until time.Time, bounded bool, err error) error {
	if v.boundHostWaits && bounded && ctx.Err() == nil && !time.Now().Before(until) {
		return fmt.Errorf("%w: %w", &hostNotAnsweringError{wait: hostWaitDial}, err)
	}
	return err
}
