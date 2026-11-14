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
// go/ast, in the non-test files of internal/ and cmd/, every reference to
// MintOperationGrant, OperationEnvelope or GetOperationEnvelope (a call, a
// type, a field, a method value) and every proto Clone, CloneOf or Merge
// whose arguments name an envelope or a grant, and requires that they appear
// only where allowed: the mint and the envelope only in SendOperation; the
// shared verifier's reading of an envelope; the definition of
// MintOperationGrant. It also requires that the controllers never name the
// OperationGrant type (so they cannot hold one) and that
// contracts.OperationSender takes no grant.

// grantSite is one file and enclosing function.
type grantSite struct{ file, function string }

// allowedGrantSites are the only places that may name a grant mint or an
// envelope, and why.
var allowedGrantSites = map[grantSite]string{
	{"internal/transport/grpc/client_operation.go", "SendOperation"}: "mints the grant and builds the envelope of every send",
	{"internal/operation/grant.go", "MintOperationGrant"}:            "the definition",
	{"internal/operation/grantverify/verify.go", "checkSizes"}:       "the shared verifier reads an envelope (agent side)",
	{"internal/operation/grantverify/verify.go", "envelopeOf"}:       "the shared verifier reads an envelope (agent side)",
}

// guardedNames are the identifiers a reference to which is a grant site.
var guardedNames = map[string]bool{"MintOperationGrant": true, "OperationEnvelope": true, "GetOperationEnvelope": true}

// cloneFuncs are the proto calls that copy a message.
var cloneFuncs = map[string]bool{"Clone": true, "CloneOf": true, "Merge": true}

func enclosingFuncName(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if fd, ok := stack[i].(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
	}
	return "<package level>"
}

// isIdent reports whether e is the identifier name.
func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// namesEnvelopeOrGrant reports whether e mentions an envelope or a grant.
func namesEnvelopeOrGrant(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			l := strings.ToLower(id.Name)
			found = found || strings.Contains(l, "envelope") || strings.Contains(l, "grant")
		}
		return !found
	})
	return found
}

func TestGrantMintSites_OnlySendOperation(t *testing.T) {
	var sites []grantSite
	var controllerNamesGrant []string
	base, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	for _, dir := range []string{"internal", "cmd"} {
		root := filepath.Join(base, dir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(base, path)
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
				case *ast.Ident:
					if guardedNames[x.Name] {
						sites = append(sites, grantSite{rel, enclosingFuncName(stack)})
					}
					if strings.HasPrefix(rel, "internal/controller/") && x.Name == "OperationGrant" {
						controllerNamesGrant = append(controllerNamesGrant, rel)
					}
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && cloneFuncs[sel.Sel.Name] && isIdent(sel.X, "proto") {
						for _, arg := range x.Args {
							if namesEnvelopeOrGrant(arg) {
								sites = append(sites, grantSite{rel, enclosingFuncName(stack) + " (clone)"})
							}
						}
					}
				}
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
	seen := map[grantSite]bool{}
	for _, s := range sites {
		if _, ok := allowedGrantSites[s]; !ok {
			t.Errorf("%s: %s names a grant mint or an operation envelope; only the transport client's SendOperation may build one", s.file, s.function)
		}
		seen[s] = true
	}
	for s := range allowedGrantSites {
		require.True(t, seen[s], "%s: %s is allowed but no longer found: review the allowlist", s.file, s.function)
	}
	sort.Strings(controllerNamesGrant)
	require.Empty(t, controllerNamesGrant, "a controller names the OperationGrant type, so it could hold a grant")

	// contracts.OperationSender: no method takes a grant.
	src, err := os.ReadFile(filepath.Join("..", "providers", "contracts", "operation.go"))
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
