//go:build !virtrigaud_dev

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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/devgate"
)

// A release build refuses the host ledger key directory, and loads no keys
// without one.
func TestLoadHostLedgerKeys_ReleaseBuild(t *testing.T) {
	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki"}
	_, err := LoadHostLedgerKeys(t.TempDir(), "virtrigaud-system/host-ledger-key", cfg)
	require.ErrorIs(t, err, devgate.ErrNotCompiled)
	_, err = LoadHostLedgerKeys("", "virtrigaud-system/host-ledger-key", cfg)
	require.ErrorIs(t, err, devgate.ErrNotCompiled)
	k, err := LoadHostLedgerKeys("", "", cfg)
	require.NoError(t, err)
	assert.Nil(t, k)
}
