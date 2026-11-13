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

package enrol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInstallPathNeverReadsTheKey: the agent's side of enrolment must not be
// able to read key bytes. No non-test file of this package may name a key
// loader, the key file name or the key creator, and no option of Install
// names a key file. (The key's creation and the request's signature are in
// the signer's own user's process, which calls this package with a
// crypto.Signer.)
func TestInstallPathNeverReadsTheKey(t *testing.T) {
	forbiddenIdents := map[string]bool{"LoadKey": true, "ParseHostKey": true, "CreateKey": true, "FileHostKey": true, "DefaultSignerKeyFile": true}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		src, err := os.ReadFile(name) // #nosec G304 -- this package's own files
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				assert.False(t, forbiddenIdents[v.Name], "%s names %s", name, v.Name)
			case *ast.BasicLit:
				assert.NotContains(t, v.Value, "host.key", "%s names the key file", name)
			}
			return true
		})
	}
	require.Greater(t, checked, 2)

	// And Install's options have no key path field.
	for _, field := range []string{"KeyFile", "KeyPath", "Key", "PrivateKey"} {
		_, found := reflect.TypeOf(InstallOptions{}).FieldByName(field)
		assert.False(t, found, "InstallOptions has a %s field", field)
	}
}

// TestRestorePutsTheOldCertificateBack covers the loader-refusal path of
// install: the previous file is restored, or the new one removed.
func TestRestorePutsTheOldCertificateBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.crt")
	require.NoError(t, os.WriteFile(path, []byte("new"), 0o600))
	require.NoError(t, restore(path, []byte("old"), true))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
	require.NoError(t, restore(path, nil, false))
	_, err = os.Lstat(path)
	assert.True(t, os.IsNotExist(err), "with no previous file the refused one is removed")
	require.NoError(t, restore(path, nil, false), "removing nothing is fine")
	require.NoError(t, os.WriteFile(path, []byte("new"), 0o600))
	require.NoError(t, restore(path, nil, true))
	_, err = os.Lstat(path)
	assert.True(t, os.IsNotExist(err), "a previous file that could not be read is not restored, and the refused one is not left")
}
