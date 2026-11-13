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

package daemon

import (
	"context"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/runner"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// operations is the server's operation path over the operation store, the
// executor and the runner pool.
type operations struct {
	store *opstore.Store
	exec  executor.Executor
	pool  *runner.Pool
}

var _ server.Operations = (*operations)(nil)

// Admit implements server.Operations.
func (o *operations) Admit(ctx context.Context, r opstore.Request) (opstore.Decision, error) {
	return o.store.Admit(ctx, r)
}

// AnswerOp implements server.Operations.
func (o *operations) AnswerOp(ctx context.Context, ref opstore.OpRef, continuitySeq uint64) (opstore.OpAnswerView, error) {
	return o.store.AnswerOp(ctx, ref, continuitySeq)
}

// AnswerVM implements server.Operations.
func (o *operations) AnswerVM(ctx context.Context, vmUID string, binding []byte, continuitySeq uint64) (opstore.VMAnswerView, error) {
	return o.store.AnswerVM(ctx, vmUID, binding, continuitySeq)
}

// Supports implements server.Operations: the executor plans kind.
func (o *operations) Supports(kind providerv1.OperationKind) bool {
	_, err := o.exec.Plan(kind)
	return err == nil
}

// Start implements server.Operations.
func (o *operations) Start(ref opstore.OpRef, payload []byte) { o.pool.Start(ref, payload) }

// Describe implements server.Operations.
func (o *operations) Describe(ctx context.Context, vmUID string) (executor.VMState, bool, error) {
	d, ok := o.exec.(executor.Describer)
	if !ok {
		return executor.VMState{}, false, nil
	}
	st, err := d.Describe(ctx, vmUID)
	return st, err == nil, err
}
