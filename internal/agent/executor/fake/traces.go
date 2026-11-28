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

package fake

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

var _ executor.TraceScanner = (*Executor)(nil)

// Traces implements executor.TraceScanner over everything the fake owns:
// an intent or effect line, or an effect file, stamped with op's op_id is
// an operation trace; for a Create, a fake VM with op's UID and binding is
// a placement trace. It reads only, and never takes the executor's lock, so
// a scan never stalls a step: the logs are append-only (a torn last line is
// skipped), effect files are created whole, and a VM file is replaced by a
// rename.
func (e *Executor) Traces(ctx context.Context, op executor.Operation) (executor.Trace, error) {
	if err := ctx.Err(); err != nil {
		return executor.Trace{}, fmt.Errorf("fake executor trace scan: %w", err)
	}
	opHex := hex.EncodeToString(op.OpID)
	var t executor.Trace
	for _, name := range []string{IntentsLog, EffectsLog} {
		lines, err := readLog(e.dir, name)
		if err != nil {
			return executor.Trace{}, fmt.Errorf("fake executor trace scan: %w", err)
		}
		for _, l := range lines {
			if l.OpID == opHex {
				t.Op = true
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(e.dir, effectsDir))
	if err != nil {
		return executor.Trace{}, fmt.Errorf("fake executor trace scan: %w", err)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), opHex+".") {
			t.Op = true
		}
	}
	if op.Kind == providerv1.OperationKind_OPERATION_KIND_CREATE {
		path, err := e.vmPath(op.VMUID)
		if err != nil {
			return executor.Trace{}, fmt.Errorf("fake executor trace scan: %w", err)
		}
		vm, exists, err := readVM(path)
		if err != nil {
			return executor.Trace{}, fmt.Errorf("fake executor trace scan: %w", err)
		}
		t.Placement = exists && vm.Binding == hex.EncodeToString(op.BindingID)
	}
	return t, nil
}
