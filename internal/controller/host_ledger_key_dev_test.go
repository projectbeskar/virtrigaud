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

	_, err := LoadHostLedgerKeys(dir, false)
	require.Error(t, err, "the keys need per-host agent enrolment")

	k, err := LoadHostLedgerKeys(dir, true)
	require.NoError(t, err)
	assert.Equal(t, hostLedgerKeyID(cur), k.CurrentKeyID())
	assert.Empty(t, k.PreviousKeyID())

	write(HostLedgerKeyPreviousFile, prev)
	k, err = LoadHostLedgerKeys(dir, true)
	require.NoError(t, err)
	assert.Equal(t, hostLedgerKeyID(prev), k.PreviousKeyID())

	require.NoError(t, os.Remove(filepath.Join(dir, HostLedgerKeyIdentityFile)))
	_, err = LoadHostLedgerKeys(dir, true)
	require.ErrorIs(t, err, errHostLedgerKeys, "the identity key is required")

	write(HostLedgerKeyIdentityFile, cur)
	_, err = LoadHostLedgerKeys(dir, true)
	require.ErrorIs(t, err, errHostLedgerKeys, "the identity key must differ from the tag keys")
}
