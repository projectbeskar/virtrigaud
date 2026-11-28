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
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every mutationGuard constant declared in mutation_guard.go is listed by
// mutationGuards exactly once, and an ordinary build switches none off.
func TestMutationGuardsListed(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "mutation_guard.go", nil, 0)
	require.NoError(t, err)
	var declared []mutationGuard
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if id, isID := vs.Type.(*ast.Ident); !ok || !isID || id.Name != "mutationGuard" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				require.True(t, ok)
				s, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				declared = append(declared, mutationGuard(s))
			}
		}
	}
	require.NotEmpty(t, declared)
	listed := mutationGuards()
	assert.ElementsMatch(t, declared, listed)
	for _, g := range listed {
		assert.Equal(t, 1, len(slices.DeleteFunc(slices.Clone(listed), func(x mutationGuard) bool { return x != g })), "%s once", g)
		if !testingMutationBuild {
			assert.False(t, mutationOff(g), "%s is off in an ordinary build", g)
		}
	}
}
