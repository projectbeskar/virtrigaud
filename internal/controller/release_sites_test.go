/*
Copyright 2025.

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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The calls that close an operation attempt or release a VirtualMachine
// finalizer are the decisions a signed host answer will have to back for a
// Provider that requires operation identity. This test lists every such call
// in the non-test files of this package, by file, enclosing function and
// callee, and compares the set with releaseSites below, so that a call site
// added, moved or removed fails here until it has been reviewed against the
// evidence that site will need. It covers the legacy create-marker and
// delete-task paths too, which are reached only for Creates made without
// operation identity. Line numbers in the comments are informative only.

// releaseSiteCallees are the functions whose calls this test enumerates.
var releaseSiteCallees = map[string]bool{
	"closeAttempt":               true,
	"removeFinalizer":            true,
	"releaseLeavingHypervisorVM": true,
}

// releaseSite is one (file, enclosing function, callee) triple.
type releaseSite struct {
	file, function, callee string
}

// releaseSites is the reviewed enumeration: the number of calls of callee
// in function of file.
var releaseSites = map[releaseSite]int{
	// Operation identity: attempts closed on a send answer, a refused first
	// delivery and a task poll; for a Provider that requires signed host
	// answers each closure will need a signed operation answer.
	{"virtualmachine_operation_identity.go", "sendAttempt", "closeAttempt"}:        1, // synchronous success
	{"virtualmachine_operation_identity.go", "sendFailed", "closeAttempt"}:         1, // refused first delivery
	{"virtualmachine_operation_identity.go", "pollAttempt", "closeAttempt"}:        1, // typed or untyped task outcome
	{"virtualmachine_operation_identity_kinds.go", "deleteAbsent", "closeAttempt"}: 1, // will need a signed VM answer (deleted)

	// Operation identity: finalizer releases.
	{"virtualmachine_operation_identity.go", "identityDelete", "removeFinalizer"}:     1, // nothing was created
	{"virtualmachine_operation_identity.go", "releaseAfterDelete", "removeFinalizer"}: 1, // inherits the Delete's evidence

	// Operator overrides (force-delete or orphan-on-delete), audited.
	{"virtualmachine_operation_identity.go", "identityDelete", "releaseLeavingHypervisorVM"}:       2,
	{"virtualmachine_operation_identity.go", "confirmDeleteRelease", "releaseLeavingHypervisorVM"}: 1,
	{"virtualmachine_operation_identity.go", "afterDeleteStep", "releaseLeavingHypervisorVM"}:      1,
	// handleDeletion: under force-delete, or after retainForUnroutableDelete
	// or retainForUnavailableProvider declined to retain (only under force),
	// or on the bound path of a Provider without operation identity.
	{"virtualmachine_controller.go", "handleDeletion", "releaseLeavingHypervisorVM"}: 9,
	{"virtualmachine_controller.go", "orphanOnDelete", "removeFinalizer"}:            1,
	// The body of every releaseLeavingHypervisorVM call.
	{"virtualmachine_controller.go", "releaseLeavingHypervisorVM", "removeFinalizer"}: 1,

	// The final release of handleDeletion: an unbound VM with no attempt
	// that needs its host, or whose Provider is gone with no unconfirmed
	// Delete. Will need every stored outcome it reads to be host-signed.
	{"virtualmachine_controller.go", "handleDeletion", "removeFinalizer"}: 1,

	// Legacy create-marker path: reached only for a Create made without
	// operation identity (identity Creates write no legacy create marker).
	{"virtualmachine_create_marker.go", "resolveCreateMarkerForDelete", "releaseLeavingHypervisorVM"}: 2,
	{"virtualmachine_create_marker.go", "resolveCreateMarkerForDelete", "removeFinalizer"}:            2,
	{"virtualmachine_create_marker.go", "releaseConfirmedNotCreated", "removeFinalizer"}:              1,

	// Legacy delete-task path: reached only after the identity branch of
	// handleDeletion returned; identity VMs never record a delete task.
	{"virtualmachine_delete_task.go", "continueDeleteTask", "releaseLeavingHypervisorVM"}:              1,
	{"virtualmachine_delete_task.go", "ensureDeleteTaskRecorded", "releaseLeavingHypervisorVM"}:        1,
	{"virtualmachine_delete_task.go", "awaitDeleteTask", "releaseLeavingHypervisorVM"}:                 2,
	{"virtualmachine_delete_task.go", "awaitDeleteTask", "removeFinalizer"}:                            1,
	{"virtualmachine_delete_task.go", "verifyDeleteTaskByDescribe", "releaseLeavingHypervisorVM"}:      2,
	{"virtualmachine_delete_task.go", "verifyDeleteTaskByDescribe", "removeFinalizer"}:                 2,
	{"virtualmachine_delete_task.go", "deleteTaskFailed", "releaseLeavingHypervisorVM"}:                1,
	{"virtualmachine_delete_task.go", "retainForDeleteTaskProviderGone", "releaseLeavingHypervisorVM"}: 1,
}

// releaseSiteTotal is the number of calls releaseSites enumerates.
const releaseSiteTotal = 38

// findReleaseSites parses the non-test Go files of dir and counts every
// reference to a releaseSiteCallees function (a call or a method value),
// keyed by file, enclosing function and callee.
func findReleaseSites(t *testing.T, dir string) (map[releaseSite]int, map[releaseSite][]int) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	found := map[releaseSite]int{}
	lines := map[releaseSite][]int{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// A selector's Sel is visited after the selector itself; count
			// the reference once.
			seen := map[*ast.Ident]bool{}
			record := func(id *ast.Ident) {
				if seen[id] || !releaseSiteCallees[id.Name] {
					return
				}
				seen[id] = true
				site := releaseSite{filepath.Base(path), fn.Name.Name, id.Name}
				found[site]++
				lines[site] = append(lines[site], fset.Position(id.Pos()).Line)
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					record(x.Sel)
				case *ast.Ident:
					record(x)
				}
				return true
			})
		}
	}
	return found, lines
}

func TestReleaseSites_EveryCallIsReviewed(t *testing.T) {
	total := 0
	for _, n := range releaseSites {
		total += n
	}
	if total != releaseSiteTotal {
		t.Fatalf("releaseSites enumerates %d calls, want %d", total, releaseSiteTotal)
	}

	found, lines := findReleaseSites(t, ".")
	var problems []string
	for site, n := range found {
		if want := releaseSites[site]; n != want {
			problems = append(problems, fmt.Sprintf("%s: %s calls %s %d times (lines %v), reviewed %d",
				site.file, site.function, site.callee, n, lines[site], want))
		}
	}
	for site, want := range releaseSites {
		if _, ok := found[site]; !ok {
			problems = append(problems, fmt.Sprintf("%s: %s no longer calls %s, reviewed %d",
				site.file, site.function, site.callee, want))
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	if len(problems) > 0 {
		t.Log("a call that closes an operation attempt or releases a VirtualMachine finalizer changed: " +
			"review the evidence it needs for a Provider that requires operation identity, then update releaseSites")
	}
}

// TestReleaseSites_FinderSeesEveryReference pins the finder itself on a
// small source: calls, method values and nested calls are each counted once,
// a declaration is not counted, and other names are ignored.
func TestReleaseSites_FinderSeesEveryReference(t *testing.T) {
	dir := t.TempDir()
	src := `package x

type r struct{}

func (r) removeFinalizer() {}
func (r) closeAttempt() error { return nil }

func a(v r) {
	v.removeFinalizer()
	f := v.removeFinalizer
	f()
	_ = v.closeAttempt()
	go func() { v.removeFinalizer() }()
	releaseLeavingHypervisorVM()
	v.RemoveFinalizer()
}

func releaseLeavingHypervisorVM() {}

func (r) RemoveFinalizer() {}
`
	for name, content := range map[string]string{
		"x.go":      src,
		"x_test.go": "package x\n\nfunc b(v r) { v.removeFinalizer() }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	found, _ := findReleaseSites(t, dir)
	want := map[releaseSite]int{
		{"x.go", "a", "removeFinalizer"}:            3,
		{"x.go", "a", "closeAttempt"}:               1,
		{"x.go", "a", "releaseLeavingHypervisorVM"}: 1,
	}
	if len(found) != len(want) {
		t.Errorf("found %v, want %v", found, want)
	}
	for site, n := range want {
		if found[site] != n {
			t.Errorf("%v: found %d, want %d", site, found[site], n)
		}
	}
}
