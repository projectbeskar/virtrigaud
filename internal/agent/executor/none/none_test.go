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

package none_test

import (
	"context"
	"errors"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/none"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestNoneRefusesEverything: every kind is unsupported, nothing runs and no
// inspection claims evidence.
func TestNoneRefusesEverything(t *testing.T) {
	var e none.Executor
	ctx := context.Background()
	for v := range providerv1.OperationKind_name {
		kind := providerv1.OperationKind(v)
		if _, err := e.Plan(kind); !errors.Is(err, executor.ErrUnsupportedKind) {
			t.Errorf("Plan(%v) = %v", kind, err)
		}
		op := executor.Operation{Kind: kind}
		if err := e.Run(ctx, op, executor.Step{Name: "x"}); !errors.Is(err, executor.ErrNotPerformed) {
			t.Errorf("Run(%v) = %v", kind, err)
		}
		if ev, err := e.Inspect(ctx, op, executor.Step{Name: "x", Once: true}); ev != executor.EvidenceUnknown || err == nil {
			t.Errorf("Inspect(%v) = %v, %v", kind, ev, err)
		}
		if ok, err := e.Goal(ctx, op, executor.Step{Name: "x", Irreversible: true}); ok || err == nil {
			t.Errorf("Goal(%v) = %v, %v", kind, ok, err)
		}
	}
	if err := e.StopAll(ctx); err != nil {
		t.Fatal(err)
	}
}
