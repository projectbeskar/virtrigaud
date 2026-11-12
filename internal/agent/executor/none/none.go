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

// Package none is the executor of a release build of the per-host agent: it
// implements no operation kind and performs no effect. Plan refuses every
// kind, Run reports every step not performed, and inspection never claims
// evidence, so recovery of an operation recorded by another build ends in an
// unknown outcome, never in a guess.
package none

import (
	"context"
	"errors"
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Name is the executor's configuration name.
const Name = "none"

// errNoExecutor is returned by the inspection methods.
var errNoExecutor = errors.New("this agent build has no executor")

// Executor performs nothing.
type Executor struct{}

var (
	_ executor.Executor = Executor{}
	_ executor.Stopper  = Executor{}
)

// Plan implements executor.Executor: every kind is unsupported.
func (Executor) Plan(kind providerv1.OperationKind) ([]executor.Step, error) {
	return nil, fmt.Errorf("%s: %w", kind, executor.ErrUnsupportedKind)
}

// Run implements executor.Executor: nothing is performed.
func (Executor) Run(context.Context, executor.Operation, executor.Step) error {
	return fmt.Errorf("%w: %v", executor.ErrNotPerformed, errNoExecutor)
}

// Inspect implements executor.Executor: no evidence either way.
func (Executor) Inspect(context.Context, executor.Operation, executor.Step) (executor.Evidence, error) {
	return executor.EvidenceUnknown, errNoExecutor
}

// Goal implements executor.Executor: never observed.
func (Executor) Goal(context.Context, executor.Operation, executor.Step) (bool, error) {
	return false, errNoExecutor
}

// StopAll implements executor.Stopper: nothing runs.
func (Executor) StopAll(context.Context) error { return nil }
