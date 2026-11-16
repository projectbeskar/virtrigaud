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

package enrol_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
)

// TestRemoveStrayTemps: the temporary files of the key material directory's
// atomically written files are removed; the files themselves, other names,
// malformed suffixes and anything that is not a regular file stay.
func TestRemoveStrayTemps(t *testing.T) {
	dir := t.TempDir()
	strays := []string{
		".start-epoch.9aa53d29ce613652", ".host.crt.0123456789abcdef", ".renewal.csr.aaaaaaaaaaaaaaaa",
		".renewal.since.0000000000000000", ".renewal.crt.ffffffffffffffff",
	}
	keep := []string{"host.crt", "start-epoch", "renewal.csr", ".start-epoch.short", ".start-epoch.9AA53D29CE613652",
		".other.0123456789abcdef", ".host.crt.0123456789abcdef0"}
	for _, n := range append(append([]string(nil), strays...), keep...) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".renewal.crt.1111111111111111"), 0o700))
	keep = append(keep, ".renewal.crt.1111111111111111")

	got := enrol.RemoveStrayTemps(dir)
	sort.Strings(got)
	want := append([]string(nil), strays...)
	sort.Strings(want)
	assert.Equal(t, want, got)
	for _, n := range strays {
		_, err := os.Lstat(filepath.Join(dir, n))
		assert.True(t, os.IsNotExist(err), n)
	}
	for _, n := range keep {
		_, err := os.Lstat(filepath.Join(dir, n))
		assert.NoError(t, err, n)
	}
	assert.Empty(t, enrol.RemoveStrayTemps(filepath.Join(dir, "missing")))
}
