//go:build unix

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

package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHostEnrollRefusesAFIFOWithoutBlocking: a FIFO given as the CSR is
// refused before it is opened (opening a FIFO with no writer blocks).
func TestHostEnrollRefusesAFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "csr.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	done := make(chan error, 1)
	go func() {
		_, err := runVrtg(t, filepath.Join(dir, "no-kubeconfig"), "host", "enroll", "--host", "host-a", "--csr", fifo,
			"--issuer-namespace", testIssuerNS, "--issuer", "ClusterIssuer/host-agent-ca", "--manifest-out", filepath.Join(dir, "x.yaml"))
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a regular file")
	case <-time.After(10 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}
