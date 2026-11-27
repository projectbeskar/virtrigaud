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

package controller

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A development build loads the host ledger keys from a directory (the
// mounted Secret), with or without a previous key, only with enrolment.
func TestLoadHostLedgerKeys_DevelopmentBuild(t *testing.T) {
	dir := t.TempDir()
	cur, prev, ident := randomLedgerKey(t), randomLedgerKey(t), randomLedgerKey(t)
	write := func(name string, key []byte) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(hex.EncodeToString(key)+"\n"), 0o600))
	}
	write(HostLedgerKeyCurrentFile, cur)
	write(HostLedgerKeyIdentityFile, ident)

	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki"}
	const ref = "virtrigaud-system/host-ledger-key"
	_, err := LoadHostLedgerKeys(dir, ref, nil)
	require.Error(t, err, "the keys need per-host agent enrolment")
	_, err = LoadHostLedgerKeys(dir, "", cfg)
	require.ErrorIs(t, err, errHostLedgerKeys, "the key Secret is named with the directory")
	_, err = LoadHostLedgerKeys("", ref, cfg)
	require.ErrorIs(t, err, errHostLedgerKeys)
	_, err = LoadHostLedgerKeys(dir, "host-agent-pki/host-ledger-key", cfg)
	require.ErrorIs(t, err, errHostLedgerKeys, "never in the enrolment namespace")
	_, err = LoadHostLedgerKeys(dir, "not a reference", cfg)
	require.ErrorIs(t, err, errHostLedgerKeys)

	k, err := LoadHostLedgerKeys(dir, ref, cfg)
	require.NoError(t, err)
	assert.Equal(t, hostLedgerKeyID(cur), k.CurrentKeyID())
	assert.Empty(t, k.PreviousKeyID())
	require.NotNil(t, cfg.LedgerKeySecret, "the Secret access check reviews the key Secret")
	assert.Equal(t, ref, cfg.LedgerKeySecret.String())

	write(HostLedgerKeyPreviousFile, prev)
	k, err = LoadHostLedgerKeys(dir, ref, cfg)
	require.NoError(t, err)
	assert.Equal(t, hostLedgerKeyID(prev), k.PreviousKeyID())

	require.NoError(t, os.Remove(filepath.Join(dir, HostLedgerKeyIdentityFile)))
	_, err = LoadHostLedgerKeys(dir, ref, cfg)
	require.ErrorIs(t, err, errHostLedgerKeys, "the identity key is required")

	write(HostLedgerKeyIdentityFile, cur)
	_, err = LoadHostLedgerKeys(dir, ref, cfg)
	require.ErrorIs(t, err, errHostLedgerKeys, "the identity key must differ from the tag keys")
}
