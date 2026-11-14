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

package grantverify_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// hostBoundCallers are the only non-test packages allowed to call
// Verifier.VerifyHostBound: the per-host agent's server, which answers a
// known op_id from its durable store before it compares the Provider and
// enrolment. Every other caller must use Verify.
var hostBoundCallers = []string{"internal/agent/server/"}

// TestVerifyHostBoundCallers walks every non-test Go file of the module and
// fails on a use (a call or a method value) of VerifyHostBound outside
// hostBoundCallers.
func TestVerifyHostBoundCallers(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "testdata" ||
				d.Name() == "node_modules" || rel == "fieldTesting" || rel == "proto" || rel == "sdk"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// A call or a method value: any use of the selector.
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "VerifyHostBound" {
				return true
			}
			found++
			allowed := false
			for _, p := range hostBoundCallers {
				allowed = allowed || strings.HasPrefix(rel, p)
			}
			if !allowed {
				t.Errorf("%s uses VerifyHostBound; only %v may", fset.Position(sel.Pos()), hostBoundCallers)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("no call of VerifyHostBound found: the walk is broken")
	}
}
