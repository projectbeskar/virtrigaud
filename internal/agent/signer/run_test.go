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
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notes records systemd notifications.
type notes struct {
	mu   sync.Mutex
	sent []string
}

func (n *notes) Notify(s string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, s)
	return nil
}

func (n *notes) got() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...)
}

// TestRun: the signer process's run loop refuses a bad socket directory or
// key file before it serves anything, notifies READY once it serves, reloads
// the key on SIGHUP (a bad reload keeps the old key) and stops on SIGTERM.
func TestRun(t *testing.T) {
	dir := shortTempDir(t)
	require.NoError(t, os.Chmod(dir, 0o750))
	keyDir := shortTempDir(t)
	key1 := newKey(t)
	keyFile := writeKeyFile(t, keyDir, key1, 0o600)
	l := &logs{}
	n := &notes{}
	signals := make(chan os.Signal, 4)
	o := RunOptions{SocketPath: filepath.Join(dir, "signer.sock"), KeyFile: keyFile, OwnerUID: EUID(), AgentUID: EUID(),
		Notifier: n, Log: l.logger()}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), o, signals) }()

	publicKey := func() *ecdsa.PublicKey {
		var c *Client
		require.Eventually(t, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var err error
			c, err = Connect(ctx, ClientOptions{Socket: o.SocketPath, ServerUID: EUID(), Lifetime: context.Background()})
			return err == nil
		}, 5*time.Second, 10*time.Millisecond)
		return c.pub
	}
	assert.True(t, publicKey().Equal(&key1.PublicKey))
	assert.Equal(t, []string{"READY=1"}, n.got())

	// SIGHUP loads a replaced key file.
	key2 := newKey(t)
	writeKeyFile(t, keyDir, key2, 0o600)
	signals <- syscall.SIGHUP
	require.Eventually(t, func() bool { return publicKey().Equal(&key2.PublicKey) }, 5*time.Second, 10*time.Millisecond)

	// A key file that no longer loads keeps the loaded key in use.
	require.NoError(t, os.Chmod(keyFile, 0o644))
	signals <- syscall.SIGHUP
	require.Eventually(t, func() bool { return contains(l.text(), "key reload refused") }, 5*time.Second, 10*time.Millisecond)
	assert.True(t, publicKey().Equal(&key2.PublicKey))
	c, err := Connect(context.Background(), ClientOptions{Socket: o.SocketPath, ServerUID: EUID()})
	require.NoError(t, err)
	d := digestOf("m")
	sig, err := c.Sign(rand.Reader, d, crypto.SHA256)
	require.NoError(t, err)
	assert.True(t, ecdsa.VerifyASN1(&key2.PublicKey, d, sig))

	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the signer did not stop")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestRunRefusals: nothing is served when the directory or the key is wrong.
func TestRunRefusals(t *testing.T) {
	signals := make(chan os.Signal)
	good := func(t *testing.T) RunOptions {
		dir := shortTempDir(t)
		keyFile := writeKeyFile(t, shortTempDir(t), newKey(t), 0o600)
		return RunOptions{SocketPath: filepath.Join(dir, "s.sock"), KeyFile: keyFile, OwnerUID: EUID(), AgentUID: EUID(), Log: (&logs{}).logger()}
	}
	for name, edit := range map[string]func(*RunOptions){
		"socket dir owned by another user": func(o *RunOptions) { o.OwnerUID = EUID() + 1 },
		"missing key":                      func(o *RunOptions) { o.KeyFile += ".none" },
		"socket dir open to others": func(o *RunOptions) {
			require.NoError(t, os.Chmod(filepath.Dir(o.SocketPath), 0o755))
		},
		"negative rate": func(o *RunOptions) { o.SignsPerSecond = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			o := good(t)
			edit(&o)
			require.Error(t, Run(context.Background(), o, signals))
			_, err := os.Lstat(o.SocketPath)
			assert.True(t, os.IsNotExist(err), "no socket was created")
		})
	}
}
