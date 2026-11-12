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

func op(kind providerv1.OperationKind, id byte, binding byte) executor.Operation {
	return executor.Operation{OpID: []byte{id, 1, 2, 3}, VMUID: "vm-1", BindingID: []byte{binding}, Kind: kind, Attempt: 1}
}

func runAll(t *testing.T, e *fake.Executor, o executor.Operation) error {
	t.Helper()
	steps, err := e.Plan(o.Kind)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if err := e.Run(context.Background(), o, s); err != nil {
			return err
		}
	}
	return nil
}

func TestFakeLifecycle(t *testing.T) {
	ctx := context.Background()
	e, err := fake.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	create := op(providerv1.OperationKind_OPERATION_KIND_CREATE, 1, 7)
	if err := runAll(t, e, create); err != nil {
		t.Fatal(err)
	}
	vms, _ := e.VMs()
	if vms["vm-1"].Power != fake.PowerOn {
		t.Fatalf("after create: %+v", vms)
	}
	// Re-running its own Create finds its own effect.
	if err := runAll(t, e, create); err != nil {
		t.Fatalf("re-run own create: %v", err)
	}
	define := executor.Step{Name: "define", Once: true}
	if ev, err := e.Inspect(ctx, create, define); err != nil || ev != executor.EvidencePresent {
		t.Fatalf("inspect own define: %v %v", ev, err)
	}
	// A Create for another placement fails, without claiming not performed.
	other := op(providerv1.OperationKind_OPERATION_KIND_CREATE, 2, 8)
	if err := runAll(t, e, other); err == nil || errors.Is(err, executor.ErrNotPerformed) {
		t.Fatalf("foreign create: %v", err)
	}
	if ev, _ := e.Inspect(ctx, other, define); ev != executor.EvidenceAbsent {
		t.Fatalf("inspect foreign define: %v", ev)
	}
	if err := runAll(t, e, op(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, 3, 7)); err != nil {
		t.Fatal(err)
	}
	if vms, _ := e.VMs(); vms["vm-1"].Power != fake.PowerOff {
		t.Fatalf("after power off: %+v", vms)
	}
	if err := runAll(t, e, op(providerv1.OperationKind_OPERATION_KIND_REBOOT, 4, 7)); err != nil {
		t.Fatal(err)
	}
	rc := op(providerv1.OperationKind_OPERATION_KIND_RECONFIGURE, 5, 7)
	for range 2 {
		if err := runAll(t, e, rc); err != nil {
			t.Fatal(err)
		}
	}
	if vms, _ := e.VMs(); vms["vm-1"].Generation != 1 {
		t.Fatalf("reconfigure applied twice: %+v", vms)
	}
	if err := e.StopAll(ctx); err != nil {
		t.Fatal(err)
	}
	if vms, _ := e.VMs(); vms["vm-1"].Power != fake.PowerOff {
		t.Fatalf("after StopAll: %+v", vms)
	}
	del := op(providerv1.OperationKind_OPERATION_KIND_DELETE, 6, 7)
	destroy := executor.Step{Name: "destroy", Irreversible: true}
	if ok, _ := e.Goal(ctx, del, destroy); ok {
		t.Fatal("goal before delete")
	}
	if err := runAll(t, e, del); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.Goal(ctx, del, destroy); !ok || err != nil {
		t.Fatalf("goal after delete: %v %v", ok, err)
	}
	if err := e.Run(ctx, op(providerv1.OperationKind_OPERATION_KIND_POWER_ON, 7, 7), executor.Step{Name: "power"}); !errors.Is(err, executor.ErrNotPerformed) {
		t.Fatalf("power on a missing VM: %v", err)
	}
}

func TestFakeRefusesOtherKindsAndBadUIDs(t *testing.T) {
	e, err := fake.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []providerv1.OperationKind{providerv1.OperationKind_OPERATION_KIND_CLONE,
		providerv1.OperationKind_OPERATION_KIND_SNAPSHOT_CREATE, providerv1.OperationKind_OPERATION_KIND_UNSPECIFIED} {
		if _, err := e.Plan(k); !errors.Is(err, executor.ErrUnsupportedKind) {
			t.Errorf("Plan(%v) = %v", k, err)
		}
	}
	o := op(providerv1.OperationKind_OPERATION_KIND_CREATE, 1, 1)
	o.VMUID = "../escape"
	if err := e.Run(context.Background(), o, executor.Step{Name: "define", Once: true}); !errors.Is(err, executor.ErrNotPerformed) {
		t.Fatalf("path escape: %v", err)
	}
}
