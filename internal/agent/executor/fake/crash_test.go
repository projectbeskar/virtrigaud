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
	"context"
	"errors"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

var errCrash = errors.New("crash")

// crashed returns an executor over dir that stops each step at point.
func crashed(t *testing.T, dir, point string) *fake.Executor {
	t.Helper()
	e, err := fake.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	fake.SetCrashHook(e, func(p string) error {
		if p == point {
			return errCrash
		}
		return nil
	})
	return e
}

// TestDefineCrashLeavesTheVMBeforeItsMarker: a define stopped between its
// VM file and its effect marker leaves the VM, which Inspect reports as the
// define's effect; the next run of the step finds it and is done, and the
// following start runs. A marker without its VM never exists.
func TestDefineCrashLeavesTheVMBeforeItsMarker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	e := crashed(t, dir, fake.PointDefineVMWritten)
	create := op(providerv1.OperationKind_OPERATION_KIND_CREATE, 1, 7)
	steps, err := e.Plan(create.Kind)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx, create, steps[0]); !errors.Is(err, errCrash) {
		t.Fatalf("define: %v", err)
	}
	vms, _ := e.VMs()
	if _, ok := vms["vm-1"]; !ok {
		t.Fatal("the VM file is not there after the crash")
	}
	// After the restart (no hook), recovery inspects the step.
	again, err := fake.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := again.Inspect(ctx, create, steps[0]); err != nil || ev != executor.EvidencePresent {
		t.Fatalf("Inspect = %v, %v; want present", ev, err)
	}
	if err := again.Run(ctx, create, steps[1]); err != nil {
		t.Fatalf("start after the recovered define: %v", err)
	}
	effects, _ := fake.Effects(dir)
	intents, _ := fake.Intents(dir)
	if len(intents) != 2 || len(effects) != 1 || effects[0].Step != "start" {
		t.Fatalf("intents %v, effects %v: the define has an intent line and no effect line", intents, effects)
	}
}

// TestIntentIsLoggedBeforeTheEffect: a step stopped right after its intent
// line performed nothing; every effect that is performed has an intent line
// first, so no effect is ever missing from both logs.
func TestIntentIsLoggedBeforeTheEffect(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []providerv1.OperationKind{
		providerv1.OperationKind_OPERATION_KIND_CREATE, providerv1.OperationKind_OPERATION_KIND_DELETE,
		providerv1.OperationKind_OPERATION_KIND_POWER_OFF, providerv1.OperationKind_OPERATION_KIND_RECONFIGURE,
	} {
		dir := t.TempDir()
		e, err := fake.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		if kind != providerv1.OperationKind_OPERATION_KIND_CREATE {
			if err := runAll(t, e, op(providerv1.OperationKind_OPERATION_KIND_CREATE, 9, 7)); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := e.VMs()
		effectsBefore, _ := fake.Effects(dir)
		intentsBefore, _ := fake.Intents(dir)
		fake.SetCrashHook(e, func(p string) error {
			if p == fake.PointIntentLogged {
				return errCrash
			}
			return nil
		})
		o := op(kind, 1, 7)
		steps, _ := e.Plan(kind)
		if err := e.Run(ctx, o, steps[0]); !errors.Is(err, errCrash) {
			t.Fatalf("%v: %v", kind, err)
		}
		after, _ := e.VMs()
		effects, _ := fake.Effects(dir)
		intents, _ := fake.Intents(dir)
		if len(after) != len(before) || after["vm-1"] != before["vm-1"] {
			t.Fatalf("%v: the VM changed before the effect: %v -> %v", kind, before, after)
		}
		if len(effects) != len(effectsBefore) || len(intents) != len(intentsBefore)+1 {
			t.Fatalf("%v: effects %d -> %d, intents %d -> %d", kind, len(effectsBefore), len(effects), len(intentsBefore), len(intents))
		}
	}
}
