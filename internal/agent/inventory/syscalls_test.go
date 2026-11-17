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

package inventory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// probeSyscalls classifies every golang.org/x/sys/unix function the
// collector calls against the agent's reference unit's system call filter
// (SystemCallFilter=@system-service ~@privileged, SystemCallErrorNumber=EPERM;
// packaging/systemd/vrtg-agent.service, whose EPERM setting
// cmd/vrtg-agent/units_test.go asserts). A call the filter denies fails with
// EPERM there, so its probe must read a refusal as "not supported" (see
// TestPidfsHandle_FilteredCallIsNotSupported); a call it admits needs nothing.
var probeSyscalls = map[string]string{
	"Close":          "admitted (@basic-io)",
	"Fstat":          "admitted (@file-system)",
	"Fstatfs":        "admitted (@file-system)",
	"Statfs":         "admitted (@file-system)",
	"PidfdOpen":      "admitted (@process)",
	"NameToHandleAt": "admitted (@file-system)",
	"OpenByHandleAt": "denied (@privileged): EPERM reads as not supported",
}

// TestProbeSyscallsAreAccountedFor: every system call the collector makes
// through golang.org/x/sys/unix is classified in probeSyscalls, so that a
// new one cannot reach a host whose unit filter would refuse it without its
// probe being checked for that refusal.
func TestProbeSyscallsAreAccountedFor(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- the package's own sources
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		require.NoError(t, err)
		unixName := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "golang.org/x/sys/unix" {
				unixName = "unix"
				if imp.Name != nil {
					unixName = imp.Name.Name
				}
			}
		}
		if unixName == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// A call, or a function value (assigned to a test seam).
			var exprs []ast.Expr
			switch x := n.(type) {
			case *ast.CallExpr:
				exprs = append(exprs, x.Fun)
			case *ast.ValueSpec:
				exprs = append(exprs, x.Values...)
			}
			for _, e := range exprs {
				sel, ok := e.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == unixName {
					seen[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	var unclassified []string
	for name := range seen {
		if _, ok := probeSyscalls[name]; !ok {
			unclassified = append(unclassified, name)
		}
	}
	sort.Strings(unclassified)
	require.Empty(t, unclassified, "classify these calls against the agent unit's system call filter in probeSyscalls")
	require.True(t, seen["OpenByHandleAt"], "the persistent handle probe is expected to call open_by_handle_at")
}
