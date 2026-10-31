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

package vsphere

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/session/keepalive"
	"github.com/vmware/govmomi/simulator"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// newSimConfig spins up an in-memory vCenter simulator and returns a Config that
// points createVSphereClient at it, plus a cleanup func.
func newSimConfig(t *testing.T) (*Config, func()) {
	t.Helper()
	model := simulator.VPX()
	require.NoError(t, model.Create())
	server := model.Service.NewServer()

	u := server.URL // includes embedded user:pass
	pw, _ := u.User.Password()
	cfg := &Config{
		Endpoint:           (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String(),
		Username:           u.User.Username(),
		Password:           pw,
		InsecureSkipVerify: true,
	}
	cleanup := func() {
		server.Close()
		model.Remove()
	}
	return cfg, cleanup
}

// TestCreateVSphereClient_InstallsKeepAlive verifies the govmomi client logs in
// against vCenter and that a keepalive handler is installed on the round-tripper,
// so the session is kept warm and does not idle out (#190).
func TestCreateVSphereClient_InstallsKeepAlive(t *testing.T) {
	cfg, cleanup := newSimConfig(t)
	defer cleanup()

	client, err := createVSphereClient(cfg)
	require.NoError(t, err)
	require.NotNil(t, client)
	defer func() { _ = client.Logout(context.Background()) }()

	_, ok := client.RoundTripper.(*keepalive.HandlerSOAP)
	assert.True(t, ok, "expected keepalive.HandlerSOAP installed on the vim25 round-tripper (#190)")
}

// TestValidate_ReconnectsAfterSessionLoss verifies Validate probes the live
// session (GetCurrentTime) rather than trusting the cached client.Valid(), and
// reconnects when the session is gone — returning Ok instead of letting the next
// operation hit NotAuthenticated (#190).
func TestValidate_ReconnectsAfterSessionLoss(t *testing.T) {
	cfg, cleanup := newSimConfig(t)
	defer cleanup()

	client, err := createVSphereClient(cfg)
	require.NoError(t, err)

	p := &Provider{client: client, config: cfg, logger: slog.Default()}

	// Healthy session → Ok.
	resp, err := p.Validate(context.Background(), &providerv1.ValidateRequest{})
	require.NoError(t, err)
	assert.True(t, resp.Ok, "validate should be Ok on a live session")

	// Drop the session server-side; Validate must reconnect and still report Ok
	// (the pre-fix code trusted the cached Valid() and would not have).
	require.NoError(t, client.Logout(context.Background()))

	resp, err = p.Validate(context.Background(), &providerv1.ValidateRequest{})
	require.NoError(t, err)
	assert.True(t, resp.Ok, "validate should reconnect after session loss and report Ok")

	// The provider must now hold a usable session (a fresh probe succeeds).
	require.NotNil(t, p.client)
	_, err = p.Validate(context.Background(), &providerv1.ValidateRequest{})
	require.NoError(t, err)
	_ = p.client.Logout(context.Background())
}

// TestKeepAlive_KeepsRetryingAfterFailedRelogin: a re-login that fails (vCenter
// unreachable when the keepalive ticked) must not stop the keepalive for good.
// govmomi stops it when the send function returns an error, and since v0.4.2
// the manager no longer calls Validate (the only other re-login) while the
// Provider's breaker is closed, so a stopped keepalive would leave every RPC
// failing NotAuthenticated after vCenter returns. The keepalive keeps probing
// and logs in once vCenter accepts it again.
func TestKeepAlive_KeepsRetryingAfterFailedRelogin(t *testing.T) {
	var probes, logins atomic.Int64
	loggedIn := make(chan struct{})
	send := vSphereKeepAlive(
		func(context.Context) error {
			probes.Add(1)
			return errors.New("NotAuthenticated")
		},
		func(context.Context) error {
			if logins.Add(1) < 3 {
				return errors.New("dial tcp: connect: connection refused")
			}
			if logins.Load() == 3 {
				close(loggedIn)
			}
			return nil
		},
		slog.Default(),
	)

	h := keepalive.NewHandlerSOAP(nil, 5*time.Millisecond, send)
	h.Start()
	defer h.Stop()

	select {
	case <-loggedIn:
	case <-time.After(5 * time.Second):
		t.Fatalf("keepalive stopped after a failed re-login: %d probes, %d logins", probes.Load(), logins.Load())
	}
	assert.GreaterOrEqual(t, probes.Load(), int64(3))
}

// TestKeepAlive_HealthySessionDoesNotLogIn: a probe that succeeds sends no login.
func TestKeepAlive_HealthySessionDoesNotLogIn(t *testing.T) {
	var logins atomic.Int64
	send := vSphereKeepAlive(
		func(context.Context) error { return nil },
		func(context.Context) error { logins.Add(1); return nil },
		slog.Default(),
	)
	require.NoError(t, send())
	assert.Zero(t, logins.Load())
}
