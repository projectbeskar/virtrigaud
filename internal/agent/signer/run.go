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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
)

// RunOptions configures Run.
type RunOptions struct {
	// SocketPath is the Unix socket to serve; its directory must pass
	// CheckSocketDir.
	SocketPath string
	// KeyFile is the host key file, owned by OwnerUID and private to it.
	KeyFile string
	// OwnerUID is the signer's own uid: the owner the key file and the socket
	// directory must have.
	OwnerUID uint32
	// AgentUID is the only uid served.
	AgentUID uint32
	// SignsPerSecond, SignBurst and LogInterval are the ServerOptions of the
	// same names.
	SignsPerSecond float64
	// SignBurst is the rate limit's burst size.
	SignBurst int
	// LogInterval is the period of the activity summary.
	LogInterval time.Duration
	// Notifier receives READY when the signer serves (nil sends nothing).
	Notifier systemd.Notifier
	// Log receives structured events.
	Log logr.Logger
}

// Run loads the host key, serves the socket and runs until ctx ends or a
// stop signal arrives on signals. SIGHUP reloads the key file (a failed
// reload keeps the key in use). It returns nil after a graceful stop. It
// refuses to start, serving nothing, when the socket directory or the key
// file is not private as required.
func Run(ctx context.Context, o RunOptions, signals <-chan os.Signal) error {
	if err := CheckSocketDir(filepath.Dir(o.SocketPath), o.OwnerUID); err != nil {
		return fmt.Errorf("run signer: %w", err)
	}
	key, err := LoadKey(o.KeyFile, o.OwnerUID)
	if err != nil {
		return fmt.Errorf("run signer: %w", err)
	}
	srv, err := NewServer(ServerOptions{Key: key, AllowedUID: o.AgentUID, Log: o.Log,
		SignsPerSecond: o.SignsPerSecond, SignBurst: o.SignBurst, LogInterval: o.LogInterval})
	if err != nil {
		return fmt.Errorf("run signer: %w", err)
	}
	lis, err := Listen(o.SocketPath)
	if err != nil {
		return fmt.Errorf("run signer: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, lis) }()
	o.Log.Info("signer serving", "agentUid", o.AgentUID)
	if o.Notifier != nil {
		_ = o.Notifier.Notify(systemd.NotifyReady)
	}
	for {
		select {
		case err := <-served:
			if err != nil {
				return fmt.Errorf("run signer: %w", err)
			}
			return nil
		case sig, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			if sig == syscall.SIGHUP {
				if err := reloadKey(srv, o); err != nil {
					o.Log.Error(err, "key reload refused; the loaded key stays in use")
				} else {
					o.Log.Info("key reloaded")
				}
				continue
			}
			o.Log.Info("stop requested", "signal", sig.String())
			cancel()
			if err := <-served; err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("run signer: %w", err)
			}
			return nil
		}
	}
}

// reloadKey reads the key file again and swaps the key in.
func reloadKey(srv *Server, o RunOptions) error {
	k, err := LoadKey(o.KeyFile, o.OwnerUID)
	if err != nil {
		return err
	}
	return srv.SetKey(k)
}
