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

package guard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// TestAllListsEveryGuard: All lists every Name constant declared in guard.go,
// exactly once. hack/agent-mutation.sh reads the constants from the source;
// a guard missing from All is refused by TestMutationGuardIsKnown, a failure
// the script must not mistake for a killed mutant.
func TestAllListsEveryGuard(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "guard.go", nil, 0)
	if err != nil {
		t.Fatalf("parse guard.go: %v", err)
	}
	var declared []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Name" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("a Name constant is not a string literal: %v", v)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				declared = append(declared, s)
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no Name constants found in guard.go")
	}
	listed := map[string]int{}
	for _, n := range All() {
		listed[string(n)]++
	}
	for name, count := range listed {
		if count != 1 {
			t.Errorf("All lists %q %d times", name, count)
		}
		if !slices.Contains(declared, name) {
			t.Errorf("All lists %q, which guard.go does not declare", name)
		}
	}
	for _, name := range declared {
		if listed[name] == 0 {
			t.Errorf("guard %q is declared but missing from All", name)
		}
	}
}
