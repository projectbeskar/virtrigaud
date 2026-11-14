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
// go/ast, in the non-test files of internal/ and cmd/, every place that
// constructs a grant or an envelope, and requires that it is SendOperation:
//
//   - a reference to MintOperationGrant (a call or a method value; its
//     definition aside);
//   - a composite literal of OperationEnvelope or OperationGrant with
//     content, or new() of one;
//   - setting an envelope on a request: an OperationEnvelope key in a
//     composite literal, or an assignment to an OperationEnvelope field;
//   - a proto Clone, CloneOf or Merge whose arguments name an envelope or a
//     grant.
//
// Reading an envelope (a parameter or result type, GetOperationEnvelope, the
// verifiers' checks) constructs nothing and is not a site. It also requires
// that the controllers never name the OperationGrant type (so they cannot
// hold one) and that contracts.OperationSender takes no grant.

// grantSite is one file and enclosing function.
type grantSite struct{ file, function string }

// allowedGrantSites are the only places that may construct a grant or an
// envelope, and why.
var allowedGrantSites = map[grantSite]string{
	{"internal/transport/grpc/client_operation.go", "SendOperation"}: "mints the grant and builds the envelope of every send",
	{"internal/operation/grant.go", "MintOperationGrant"}:            "the mint itself builds the grant it returns",
}

// grantTypes are the message types whose construction is a site.
var grantTypes = map[string]bool{"OperationEnvelope": true, "OperationGrant": true}

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

// typeName is the last name of a type expression (pkg.T or T), or "".
func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
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

// grantConstructionSites lists the grant and envelope construction sites of
// one source file (see above), and whether it names the OperationGrant type.
func grantConstructionSites(rel string, src []byte) (sites []grantSite, namesGrant bool, err error) {
	f, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, false, err
	}
	var stack []ast.Node
	add := func(suffix string) { sites = append(sites, grantSite{rel, enclosingFuncName(stack) + suffix}) }
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch x := n.(type) {
		case *ast.Ident:
			if x.Name == "OperationGrant" {
				namesGrant = true
			}
			if x.Name == "MintOperationGrant" {
				if fd, ok := stack[len(stack)-2].(*ast.FuncDecl); !ok || fd.Name != x {
					add("")
				}
			}
		case *ast.CompositeLit:
			t := x.Type
			if u, ok := t.(*ast.UnaryExpr); ok {
				t = u.X
			}
			if grantTypes[typeName(t)] && len(x.Elts) > 0 {
				add("")
			}
		case *ast.KeyValueExpr:
			if isIdent(x.Key, "OperationEnvelope") {
				add("")
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "OperationEnvelope" {
					add("")
				}
			}
		case *ast.CallExpr:
			if isIdent(x.Fun, "new") && len(x.Args) == 1 && grantTypes[typeName(x.Args[0])] {
				add("")
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && cloneFuncs[sel.Sel.Name] && isIdent(sel.X, "proto") {
				for _, arg := range x.Args {
					if namesEnvelopeOrGrant(arg) {
						add(" (clone)")
					}
				}
			}
		}
		return true
	})
	return sites, namesGrant, nil
}

// TestGrantMintSites_GuardCatchesConstruction: the guard flags each kind of
// construction outside SendOperation and not a verifier that only reads.
func TestGrantMintSites_GuardCatchesConstruction(t *testing.T) {
	src := []byte(`package p

func Mint(ctx context.Context) { _, _ = operation.MintOperationGrant(ctx, s, r) }
func Ref() { f := operation.MintOperationGrant; _ = f }
func Literal() { _ = &providerv1.OperationEnvelope{Request: b} }
func GrantLiteral() { _ = providerv1.OperationGrant{Body: b} }
func New() { _ = new(providerv1.OperationEnvelope) }
func Attach() { _ = &providerv1.CreateRequest{OperationEnvelope: env} }
func Assign(r *providerv1.PowerRequest) { r.OperationEnvelope = env }
func Clone(r *providerv1.PowerRequest) { _ = proto.Clone(r.GetOperationEnvelope()) }
func Reader(outer proto.Message) (*providerv1.OperationEnvelope, error) {
	env := outer.(*providerv1.PowerRequest).GetOperationEnvelope()
	_ = (&providerv1.OperationEnvelope{}).ProtoReflect().Descriptor().FullName()
	return env, nil
}
`)
	sites, _, err := grantConstructionSites("internal/x/x.go", src)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, s := range sites {
		got[s.function] = true
	}
	for _, fn := range []string{"Mint", "Ref", "Literal", "GrantLiteral", "New", "Attach", "Assign", "Clone (clone)"} {
		require.True(t, got[fn], "the guard misses construction in %s", fn)
	}
	require.False(t, got["Reader"], "reading an envelope is not construction")
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
			found, namesGrant, err := grantConstructionSites(rel, src)
			if err != nil {
				return err
			}
			sites = append(sites, found...)
			if namesGrant && strings.HasPrefix(rel, "internal/controller/") {
				controllerNamesGrant = append(controllerNamesGrant, rel)
			}
			return nil
		})
		require.NoError(t, err)
	}
	seen := map[grantSite]bool{}
	for _, s := range sites {
		if _, ok := allowedGrantSites[s]; !ok {
			t.Errorf("%s: %s constructs a grant or an operation envelope; only the transport client's SendOperation may", s.file, s.function)
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
