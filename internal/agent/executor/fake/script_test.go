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
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestScript_FaultsHoldAndEffects: scripted faults apply once each, in
// order, to the steps they match; a held step does nothing until its
// deadline; every effect is logged once; CreateStopped leaves a new VM off.
func TestScript_FaultsHoldAndEffects(t *testing.T) {
	dir := t.TempDir()
	e, err := fake.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := fake.WriteScript(dir, fake.Script{CreateStopped: true, Faults: []fake.ScriptedFault{
		{Step: "power", Fault: fake.FaultNotPerformed},
		{VMUID: "vm-1", Step: "power", Fault: fake.FaultProbeError},
	}}); err != nil {
		t.Fatal(err)
	}
	create := op(providerv1.OperationKind_OPERATION_KIND_CREATE, 1, 7)
	if err := runAll(t, e, create); err != nil {
		t.Fatal(err)
	}
	vms, _ := e.VMs()
	if vms["vm-1"].Power != fake.PowerOff {
		t.Fatalf("CreateStopped: %+v", vms)
	}
	power := executor.Step{Name: "power"}
	on := op(providerv1.OperationKind_OPERATION_KIND_POWER_ON, 2, 7)
	if err := e.Run(ctx, on, power); !errors.Is(err, executor.ErrNotPerformed) {
		t.Fatalf("first fault: %v", err)
	}
	if err := e.Run(ctx, on, power); err == nil || errors.Is(err, executor.ErrNotPerformed) {
		t.Fatalf("second fault leaves the effect in doubt: %v", err)
	}
	if _, err := e.Inspect(ctx, on, power); err == nil {
		t.Fatal("the inspection of a probe-error step must fail")
	}
	if err := e.Run(ctx, on, power); err != nil {
		t.Fatalf("faults are used up: %v", err)
	}
	if st, err := e.Describe(ctx, "vm-1"); err != nil || !st.Exists || !st.Running {
		t.Fatalf("describe: %+v %v", st, err)
	}

	if err := fake.SetHold(dir, true); err != nil {
		t.Fatal(err)
	}
	hctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	off := op(providerv1.OperationKind_OPERATION_KIND_POWER_OFF, 3, 7)
	if err := e.Run(hctx, off, power); !errors.Is(err, executor.ErrNotPerformed) {
		t.Fatalf("a held step that reaches its deadline did nothing: %v", err)
	}
	if err := fake.SetHold(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := fake.WriteScript(dir, fake.Script{Faults: []fake.ScriptedFault{{Fault: fake.FaultStall}}}); err != nil {
		t.Fatal(err)
	}
	sctx, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	if err := e.Run(sctx, off, power); err == nil || errors.Is(err, executor.ErrNotPerformed) {
		t.Fatalf("a stalled step ends at its deadline, its effect in doubt: %v", err)
	}
	effects, err := fake.Effects(dir)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, ef := range effects {
		steps = append(steps, ef.Step)
	}
	if len(steps) != 2 || steps[0] != "define" || steps[1] != "power" {
		t.Fatalf("effects %v, want define then one power", steps)
	}
}
