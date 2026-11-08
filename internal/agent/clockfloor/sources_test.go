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

package clockfloor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
)

// TestProcBootID: a boot_id file is read, trimmed and validated.
func TestProcBootID(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		p := filepath.Join(dir, "boot_id")
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
		return p
	}
	id, err := clockfloor.ProcBootID{Path: write("0a1b2c3d-1111-2222-3333-444455556666\n")}.BootID()
	require.NoError(t, err)
	assert.Equal(t, "0a1b2c3d-1111-2222-3333-444455556666", id)

	for name, content := range map[string]string{"empty": "\n", "junk": "not hex!", "long": string(make([]byte, 100))} {
		_, err := clockfloor.ProcBootID{Path: write(content)}.BootID()
		assert.Error(t, err, name)
	}
	_, err = clockfloor.ProcBootID{Path: filepath.Join(dir, "missing")}.BootID()
	assert.Error(t, err)
}
