//go:build virtrigaud_dev

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

package fake_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The trace scan finds what a Create left (its intent and effect lines,
// its effect file, its placement) and nothing for an operation that never
// ran.
func TestTraces(t *testing.T) {
	e, err := fake.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	op := executor.Operation{OpID: bytes.Repeat([]byte{1}, 16), VMUID: "vm-1", BindingID: bytes.Repeat([]byte{2}, 16),
		Kind: providerv1.OperationKind_OPERATION_KIND_CREATE, Attempt: 1}
	if tr, err := e.Traces(ctx, op); err != nil || tr.Found() {
		t.Fatalf("before the Create: %+v %v", tr, err)
	}
	plan, err := e.Plan(op.Kind)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx, op, plan[0]); err != nil {
		t.Fatal(err)
	}
	tr, err := e.Traces(ctx, op)
	if err != nil || !tr.Op || !tr.Placement {
		t.Fatalf("after the Create's define: %+v %v", tr, err)
	}
	other := op
	other.OpID = bytes.Repeat([]byte{3}, 16)
	other.Kind = providerv1.OperationKind_OPERATION_KIND_POWER_ON
	if tr, err := e.Traces(ctx, other); err != nil || tr.Found() {
		t.Fatalf("another operation: %+v %v", tr, err)
	}
	otherPlacement := op
	otherPlacement.OpID, otherPlacement.BindingID = other.OpID, bytes.Repeat([]byte{4}, 16)
	if tr, err := e.Traces(ctx, otherPlacement); err != nil || tr.Found() {
		t.Fatalf("a Create of another placement: %+v %v", tr, err)
	}
}
