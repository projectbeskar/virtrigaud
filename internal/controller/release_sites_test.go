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
//
// Besides the three helpers it counts, fail-closed, every direct
// RemoveFinalizer call (the k8s helper or controllerutil) whose finalizer
// argument is not one of the other kinds' finalizer constants, so a release
// of the VirtualMachine finalizer that bypasses removeFinalizer is seen too,
// and every assignment to a Finalizers field. References in package-level
// declarations (a function literal or a method expression in a var) are
// counted under the name of the declared variable.

// directRemoveFinalizer is the callee name recorded for a direct
// RemoveFinalizer call that may remove the VirtualMachine finalizer.
const directRemoveFinalizer = "RemoveFinalizer"

// finalizersAssignment is the callee name recorded for an assignment to a
// Finalizers field.
const finalizersAssignment = "Finalizers="

// otherKindFinalizers are the finalizer constants of other kinds. A direct
// RemoveFinalizer call whose last argument names one of them is not counted;
// any other argument (VirtualMachineFinalizer, a literal, a variable) is.
var otherKindFinalizers = map[string]bool{
	"VMSnapshotFinalizer":    true,
	"VMCloneFinalizer":       true,
	"VMMigrationFinalizer":   true,
	"ProviderInUseFinalizer": true,
	"HostInUseFinalizer":     true,
}

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
	// Operation identity: attempts are closed only on a verified, READY,
	// bound signed answer the manager asked for: the operation's state
	// (closeSigned), or a deleted placement naming the Delete's own op_id
	// (deleteByVMAnswer). No send answer, refusal or task status closes one.
	{"virtualmachine_operation_answers.go", "closeSigned", "closeAttempt"}:      1, // signed operation state
	{"virtualmachine_operation_answers.go", "deleteByVMAnswer", "closeAttempt"}: 1, // signed DELETED of the placement

	// Operation identity: finalizer releases.
	{"virtualmachine_operation_identity.go", "identityDelete", "removeFinalizer"}:     1, // nothing created: no Create, or its not-performed outcome signed again
	{"virtualmachine_operation_identity.go", "releaseAfterDelete", "removeFinalizer"}: 1, // a signed COMPLETED Delete or DELETED placement

	// Operator overrides (force-delete or orphan-on-delete), audited.
	{"virtualmachine_operation_identity.go", "identityDelete", "releaseLeavingHypervisorVM"}:       3,
	{"virtualmachine_operation_identity.go", "confirmDeleteRelease", "releaseLeavingHypervisorVM"}: 1,
	{"virtualmachine_operation_identity.go", "afterDeleteStep", "releaseLeavingHypervisorVM"}:      1,
	// handleDeletion: under force-delete, or after retainForUnroutableDelete
	// or retainForUnavailableProvider declined to retain (only under force),
	// or on the bound path of a Provider without operation identity.
	{"virtualmachine_controller.go", "handleDeletion", "releaseLeavingHypervisorVM"}: 9,
	{"virtualmachine_controller.go", "orphanOnDelete", "removeFinalizer"}:            1,
	// The body of every releaseLeavingHypervisorVM call.
	{"virtualmachine_controller.go", "releaseLeavingHypervisorVM", "removeFinalizer"}: 1,
	// The only direct removal of the VirtualMachine finalizer: the body of
	// every removeFinalizer call.
	{"virtualmachine_controller.go", "removeFinalizer", directRemoveFinalizer}: 1,

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

// lastArgName returns the identifier or selector name of the last argument
// of call, or "" when it is neither.
func lastArgName(call *ast.CallExpr) string {
	if len(call.Args) == 0 {
		return ""
	}
	switch a := call.Args[len(call.Args)-1].(type) {
	case *ast.Ident:
		return a.Name
	case *ast.SelectorExpr:
		return a.Sel.Name
	}
	return ""
}

// calleeName returns the name of the function call calls, or "".
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// findReleaseSites parses the non-test Go files of dir and counts every
// reference to a releaseSiteCallees function (a call, a method value or a
// method expression), every direct RemoveFinalizer call that may remove the
// VirtualMachine finalizer and every assignment to a Finalizers field, keyed
// by file, enclosing function (or package-level variable) and callee.
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
		base := filepath.Base(path)
		scan := func(root ast.Node, function string) {
			add := func(callee string, pos token.Pos) {
				site := releaseSite{base, function, callee}
				found[site]++
				lines[site] = append(lines[site], fset.Position(pos).Line)
			}
			// A selector's Sel is visited after the selector itself; count
			// the reference once.
			seen := map[*ast.Ident]bool{}
			record := func(id *ast.Ident) {
				if seen[id] || !releaseSiteCallees[id.Name] {
					return
				}
				seen[id] = true
				add(id.Name, id.Pos())
			}
			ast.Inspect(root, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if calleeName(x) == directRemoveFinalizer && !otherKindFinalizers[lastArgName(x)] {
						add(directRemoveFinalizer, x.Pos())
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Finalizers" {
							add(finalizersAssignment, sel.Pos())
						}
					}
				case *ast.SelectorExpr:
					record(x.Sel)
				case *ast.Ident:
					record(x)
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					scan(d.Body, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					names := make([]string, 0, len(vs.Names))
					for _, n := range vs.Names {
						names = append(names, n.Name)
					}
					for _, v := range vs.Values {
						scan(v, "var "+strings.Join(names, ","))
					}
				}
			}
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

var hook = func(v r) { v.removeFinalizer() }

var expr = r.closeAttempt

func direct(ctx, c, vm, snap, fin any) {
	_ = k8s.RemoveFinalizer(ctx, c, vm, v1beta1.VirtualMachineFinalizer)
	controllerutil.RemoveFinalizer(snap, v1beta1.VMSnapshotFinalizer)
	controllerutil.RemoveFinalizer(vm, fin)
	controllerutil.RemoveFinalizer(vm, "virtualmachine.infra.virtrigaud.io/finalizer")
	vm.Finalizers = nil
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
		// v.RemoveFinalizer() has no finalizer argument: counted, fail-closed.
		{"x.go", "a", directRemoveFinalizer}:      1,
		{"x.go", "var hook", "removeFinalizer"}:   1,
		{"x.go", "var expr", "closeAttempt"}:      1,
		{"x.go", "direct", directRemoveFinalizer}: 3,
		{"x.go", "direct", finalizersAssignment}:  1,
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
