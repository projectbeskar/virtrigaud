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

// Package fake is a development-only executor for the per-host agent: its
// "VMs" are small files under a directory, so that the agent's operation
// path can be exercised end to end without a hypervisor. It is compiled only
// with the virtrigaud_dev build tag; a release build has no executor that
// performs effects.
//
// It implements the seven kinds a manager marks with operation identity:
// Create (a once step that defines the VM, then an idempotent start),
// Delete (an irreversible destroy), the four power kinds and Reconfigure
// (idempotent). A step that finds its own effect already present, recognised
// by the VM's binding, is done; an effect that belongs to another binding
// fails the step without claiming that nothing happened.
package fake

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Name is the executor's configuration name.
const Name = "fake"

// Step names.
const (
	stepDefine      = "define"
	stepStart       = "start"
	stepDestroy     = "destroy"
	stepPower       = "power"
	stepReconfigure = "reconfigure"
)

// Power states of a fake VM.
const (
	// PowerOn is a running fake VM.
	PowerOn = "on"
	// PowerOff is a stopped fake VM.
	PowerOff = "off"
)

const (
	vmsDir     = "vms"
	effectsDir = "effects"
	vmSuffix   = ".json"
)

// vmUIDRE is the shape of a VM UID: also a safe file name.
var vmUIDRE = regexp.MustCompile(`^[0-9a-z-]{1,128}$`)

// VM is the content of a fake VM file.
type VM struct {
	// Binding is the hexadecimal placement identifier.
	Binding string `json:"binding"`
	// Power is PowerOn or PowerOff.
	Power string `json:"power"`
	// Generation counts reconfigurations.
	Generation uint64 `json:"generation"`
}

// Executor is the fake executor. It is safe for concurrent use.
type Executor struct {
	dir string
	mu  sync.Mutex
}

var (
	_ executor.Executor = (*Executor)(nil)
	_ executor.Stopper  = (*Executor)(nil)
)

// New returns a fake executor keeping its VMs under dir, created 0700.
func New(dir string) (*Executor, error) {
	for _, d := range []string{dir, filepath.Join(dir, vmsDir), filepath.Join(dir, effectsDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("fake executor: create %s: %w", d, err)
		}
	}
	return &Executor{dir: dir}, nil
}

// Plan implements executor.Executor.
func (e *Executor) Plan(kind providerv1.OperationKind) ([]executor.Step, error) {
	switch kind {
	case providerv1.OperationKind_OPERATION_KIND_CREATE:
		return []executor.Step{{Name: stepDefine, Once: true}, {Name: stepStart}}, nil
	case providerv1.OperationKind_OPERATION_KIND_DELETE:
		return []executor.Step{{Name: stepDestroy, Irreversible: true}}, nil
	case providerv1.OperationKind_OPERATION_KIND_POWER_ON, providerv1.OperationKind_OPERATION_KIND_POWER_OFF,
		providerv1.OperationKind_OPERATION_KIND_REBOOT, providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL:
		return []executor.Step{{Name: stepPower}}, nil
	case providerv1.OperationKind_OPERATION_KIND_RECONFIGURE:
		return []executor.Step{{Name: stepReconfigure}}, nil
	}
	return nil, fmt.Errorf("%s: %w", kind, executor.ErrUnsupportedKind)
}

// Run implements executor.Executor.
func (e *Executor) Run(ctx context.Context, op executor.Operation, step executor.Step) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", executor.ErrNotPerformed, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	path, err := e.vmPath(op.VMUID)
	if err != nil {
		return fmt.Errorf("%w: %v", executor.ErrNotPerformed, err)
	}
	binding := hex.EncodeToString(op.BindingID)
	vm, exists, err := readVM(path)
	if err != nil {
		return err
	}
	if exists && vm.Binding != binding && step.Name != stepDefine {
		return fmt.Errorf("fake VM %s belongs to another placement", op.VMUID)
	}
	switch step.Name {
	case stepDefine:
		if exists {
			if vm.Binding == binding {
				return nil // this operation's own effect
			}
			return fmt.Errorf("fake VM %s exists for another placement", op.VMUID)
		}
		if err := writeFile(e.effectPath(op, step), nil); err != nil {
			return err
		}
		return writeVM(path, VM{Binding: binding, Power: PowerOff})
	case stepStart:
		if !exists {
			return fmt.Errorf("fake VM %s is not defined", op.VMUID)
		}
		vm.Power = PowerOn
		return writeVM(path, vm)
	case stepDestroy:
		if !exists {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove fake VM %s: %w", op.VMUID, err)
		}
		return syncDir(filepath.Dir(path))
	case stepPower:
		if !exists {
			return fmt.Errorf("%w: fake VM %s does not exist", executor.ErrNotPerformed, op.VMUID)
		}
		switch op.Kind {
		case providerv1.OperationKind_OPERATION_KIND_POWER_ON, providerv1.OperationKind_OPERATION_KIND_REBOOT:
			vm.Power = PowerOn
		default:
			vm.Power = PowerOff
		}
		return writeVM(path, vm)
	case stepReconfigure:
		if !exists {
			return fmt.Errorf("%w: fake VM %s does not exist", executor.ErrNotPerformed, op.VMUID)
		}
		if ok, err := fileExists(e.effectPath(op, step)); err != nil || ok {
			return err
		}
		vm.Generation++
		if err := writeVM(path, vm); err != nil {
			return err
		}
		return writeFile(e.effectPath(op, step), nil)
	}
	return fmt.Errorf("%w: unknown step %q", executor.ErrNotPerformed, step.Name)
}

// Inspect implements executor.Executor: the step's effect exists when its
// effect file, stamped with the operation's op_id, exists, or when the VM
// exists with this operation's binding.
func (e *Executor) Inspect(_ context.Context, op executor.Operation, step executor.Step) (executor.Evidence, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ok, err := fileExists(e.effectPath(op, step)); err != nil {
		return executor.EvidenceUnknown, err
	} else if ok {
		return executor.EvidencePresent, nil
	}
	path, err := e.vmPath(op.VMUID)
	if err != nil {
		return executor.EvidenceUnknown, err
	}
	vm, exists, err := readVM(path)
	if err != nil {
		return executor.EvidenceUnknown, err
	}
	if step.Name == stepDefine && exists && vm.Binding == hex.EncodeToString(op.BindingID) {
		return executor.EvidencePresent, nil
	}
	return executor.EvidenceAbsent, nil
}

// Goal implements executor.Executor: a destroy's goal is the VM's absence.
func (e *Executor) Goal(_ context.Context, op executor.Operation, step executor.Step) (bool, error) {
	if step.Name != stepDestroy {
		return false, fmt.Errorf("step %q has no goal check", step.Name)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	path, err := e.vmPath(op.VMUID)
	if err != nil {
		return false, err
	}
	_, exists, err := readVM(path)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

// StopAll implements executor.Stopper: every fake VM is powered off.
func (e *Executor) StopAll(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	names, err := os.ReadDir(filepath.Join(e.dir, vmsDir))
	if err != nil {
		return fmt.Errorf("list fake VMs: %w", err)
	}
	var errs []error
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(n.Name(), vmSuffix) {
			continue
		}
		path := filepath.Join(e.dir, vmsDir, n.Name())
		vm, exists, err := readVM(path)
		if err != nil || !exists {
			errs = append(errs, err)
			continue
		}
		vm.Power = PowerOff
		errs = append(errs, writeVM(path, vm))
	}
	return errors.Join(errs...)
}

// VMs returns the fake VMs by UID.
func (e *Executor) VMs() (map[string]VM, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(e.dir, vmsDir))
	if err != nil {
		return nil, err
	}
	out := map[string]VM{}
	names := make([]string, 0, len(entries))
	for _, n := range entries {
		names = append(names, n.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		uid, ok := strings.CutSuffix(n, vmSuffix)
		if !ok {
			continue
		}
		vm, exists, err := readVM(filepath.Join(e.dir, vmsDir, n))
		if err != nil {
			return nil, err
		}
		if exists {
			out[uid] = vm
		}
	}
	return out, nil
}

func (e *Executor) vmPath(uid string) (string, error) {
	if !vmUIDRE.MatchString(uid) {
		return "", fmt.Errorf("VM UID %q is not a UID", uid)
	}
	return filepath.Join(e.dir, vmsDir, uid+vmSuffix), nil
}

func (e *Executor) effectPath(op executor.Operation, step executor.Step) string {
	return filepath.Join(e.dir, effectsDir, hex.EncodeToString(op.OpID)+"."+step.Name)
}

func readVM(path string) (VM, bool, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- a path the executor built from a validated UID
	if errors.Is(err, fs.ErrNotExist) {
		return VM{}, false, nil
	}
	if err != nil {
		return VM{}, false, fmt.Errorf("read fake VM: %w", err)
	}
	var vm VM
	if err := json.Unmarshal(b, &vm); err != nil {
		return VM{}, false, fmt.Errorf("decode fake VM %s: %w", filepath.Base(path), err)
	}
	return vm, true, nil
}

func writeVM(path string, vm VM) error {
	b, err := json.Marshal(vm)
	if err != nil {
		return fmt.Errorf("encode fake VM: %w", err)
	}
	return writeFile(path, b)
}

// writeFile replaces path durably: write a temporary file, fsync it, rename
// it, fsync the directory.
func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- a path the executor built
	if err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(path), err)
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- a directory the executor owns
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}
