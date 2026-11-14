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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The manager never stores, caches or re-sends an operation grant: every
// send, first or re-send, mints a fresh grant over the persisted request
// bytes in the transport client's SendOperation. A refusal of a re-sent grant
// (expired, under a rotated anchor) therefore always concerns a grant the
// manager minted for that send, never one it kept. This guard lists, by
// go/ast, in the non-test files of internal/, every call of
// MintOperationGrant and every non-empty OperationEnvelope composite literal,
// and requires that both appear only in SendOperation; that the controllers
// never name the OperationGrant type (so they cannot hold one); and that
// contracts.OperationSender takes no grant.

// grantSite is one file and enclosing function.
type grantSite struct{ file, function string }

// allowedGrantSites are the only places a grant is minted or an envelope
// built.
var allowedGrantSites = map[grantSite]bool{
	{"transport/grpc/client_operation.go", "SendOperation"}: true,
}

func enclosingFuncName(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if fd, ok := stack[i].(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
	}
	return "<package level>"
}

func TestGrantMintSites_OnlySendOperation(t *testing.T) {
	root := ".."
	var mints, envelopes []grantSite
	controllerNamesGrant := []string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(path) // #nosec G304 -- this repository's own source
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch x := n.(type) {
			case *ast.CallExpr:
				name := ""
				switch fn := x.Fun.(type) {
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				case *ast.Ident:
					name = fn.Name
				}
				if name == "MintOperationGrant" {
					mints = append(mints, grantSite{rel, enclosingFuncName(stack)})
				}
			case *ast.CompositeLit:
				if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "OperationEnvelope" && len(x.Elts) > 0 {
					envelopes = append(envelopes, grantSite{rel, enclosingFuncName(stack)})
				}
			case *ast.SelectorExpr:
				if strings.HasPrefix(rel, "controller/") && x.Sel.Name == "OperationGrant" {
					controllerNamesGrant = append(controllerNamesGrant, rel)
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	for _, set := range [][]grantSite{mints, envelopes} {
		for _, s := range set {
			if !allowedGrantSites[s] {
				t.Errorf("%s: %s mints a grant or builds an operation envelope; only the transport client's SendOperation may", s.file, s.function)
			}
		}
	}
	require.NotEmpty(t, mints, "the guard finds the one mint site")
	require.NotEmpty(t, envelopes, "the guard finds the one envelope site")
	sort.Strings(controllerNamesGrant)
	require.Empty(t, controllerNamesGrant, "a controller names the OperationGrant type, so it could hold a grant")

	// contracts.OperationSender: no method takes a grant.
	src, err := os.ReadFile(filepath.Join(root, "providers", "contracts", "operation.go"))
	require.NoError(t, err)
	f, err := parser.ParseFile(token.NewFileSet(), "operation.go", src, parser.SkipObjectResolution)
	require.NoError(t, err)
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "OperationSender" {
			return true
		}
		found = true
		it, ok := ts.Type.(*ast.InterfaceType)
		require.True(t, ok)
		for _, m := range it.Methods.List {
			ft, ok := m.Type.(*ast.FuncType)
			require.True(t, ok)
			for _, p := range ft.Params.List {
				typ := string(src[p.Type.Pos()-1 : p.Type.End()-1])
				require.NotContains(t, strings.ToLower(typ), "grant", "OperationSender.%s takes a grant", m.Names[0].Name)
				for _, n := range p.Names {
					require.NotContains(t, strings.ToLower(n.Name), "grant", "OperationSender.%s takes a grant", m.Names[0].Name)
				}
			}
		}
		return false
	})
	require.True(t, found, "contracts.OperationSender exists")
}
