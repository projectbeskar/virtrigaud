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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

const (
	// phaseDryRun is the Phase of every operation of a dry run.
	phaseDryRun = "DryRun"

	// Operation names: the keys of the config's operations weights and the
	// Operation column of the results.
	opCreate      = "create"
	opDelete      = "delete"
	opPower       = "power"
	opReconfigure = "reconfigure"
	opSnapshot    = "snapshot"
	opClone       = "clone"
	opDescribe    = "describe"

	// conditionReconfiguring and reasonRestartRequired are the VM condition
	// and reason the operator reports for a resize applied to the next boot
	// only.
	conditionReconfiguring = "Reconfiguring"
	reasonRestartRequired  = "RestartRequired"

	// defaultObserveTimeout is how long --observe waits for one operation's
	// effect by default.
	defaultObserveTimeout = 5 * time.Minute
	// defaultCleanupTimeout is how long --cleanup may take by default.
	defaultCleanupTimeout = 10 * time.Minute
	// defaultPollInterval is how often --observe and --cleanup read an
	// object.
	defaultPollInterval = time.Second

	// defaultReconfigureCPUMin and defaultReconfigureCPUMax are the vCPU
	// counts reconfigure alternates between when vmTemplate.resources.cpuRange
	// is not set.
	defaultReconfigureCPUMin = 1
	defaultReconfigureCPUMax = 2
)

// errNoRunVM is the error of an operation that needs a VM this run created
// when there is none it can use (none created yet, or all busy).
var errNoRunVM = errors.New("no VM created by this run is available")

// runVM is a VirtualMachine this run created.
type runVM struct {
	name     string
	uid      types.UID
	provider string
	// busy is set while an operation acts on the VM, so two operations never
	// act on the same VM at once and delete never takes a VM in use.
	busy bool
	// power is the spec.powerState this run last set.
	power infrav1beta1.PowerState
	// cpu and memoryMiB are the spec.resources this run last set (0: not
	// set by this run).
	cpu       int32
	memoryMiB int64
}

// runObject is a VMSnapshot, VMClone or VirtualMachine this run created.
type runObject struct {
	name string
	uid  types.UID
	// source and target are, for a VMClone, the names of its source VM and
	// of the target VM it asks for.
	source, target string
}

// idleVMs returns the indexes of the VMs no operation is using. lg.mu must
// be held.
func (lg *LoadGenerator) idleVMs() []int {
	var idle []int
	for i, vm := range lg.vms {
		if !vm.busy {
			idle = append(idle, i)
		}
	}
	return idle
}

// acquireVM marks a random idle VM this run created as busy and returns it.
// It reports false when there is none.
func (lg *LoadGenerator) acquireVM() (*runVM, bool) {
	return lg.acquireVMPreferring(nil)
}

// acquireVMPreferring is acquireVM, choosing among the idle VMs for which
// prefer reports true when there are any.
func (lg *LoadGenerator) acquireVMPreferring(prefer func(*runVM) bool) (*runVM, bool) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	idle := lg.idleVMs()
	if len(idle) == 0 {
		return nil, false
	}
	if prefer != nil {
		var preferred []int
		for _, i := range idle {
			if prefer(lg.vms[i]) {
				preferred = append(preferred, i)
			}
		}
		if len(preferred) > 0 {
			idle = preferred
		}
	}
	vm := lg.vms[idle[rand.Intn(len(idle))]] // #nosec G404 -- picks which created VM to act on, not a secret
	vm.busy = true
	return vm, true
}

// releaseVM marks vm as no longer in use.
func (lg *LoadGenerator) releaseVM(vm *runVM) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	vm.busy = false
}

// nextSeq returns the next sequence number for an object name.
func (lg *LoadGenerator) nextSeq() int {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.vmCounter++
	return lg.vmCounter
}

// ignoreNotFound returns nil for a NotFound error (the object may not be
// visible yet) and err otherwise.
func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// observe, with --observe, polls done until it reports true, fails, or
// --observe-timeout ends, and records in result the time from result's
// StartTime until then (or why it was not seen). An error from done stops
// the wait at once. A transient read error is retried.
func (lg *LoadGenerator) observe(result *Result, done func(ctx context.Context) (bool, error)) {
	if !lg.opts.observe || lg.opts.dryRun {
		return
	}
	parent := lg.observeCtx
	if parent == nil {
		parent = context.Background()
	}
	interval := lg.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	timeout := lg.opts.observeTimeout
	if timeout <= 0 {
		timeout = defaultObserveTimeout
	}
	var lastErr error
	err := wait.PollUntilContextTimeout(parent, interval, timeout, true, func(ctx context.Context) (bool, error) {
		ok, err := done(ctx)
		if err != nil && (apierrors.IsNotFound(err) || isTransient(err)) {
			lastErr = err
			return false, nil
		}
		return ok, err
	})
	switch {
	case err == nil:
		result.Observed = time.Since(result.StartTime)
	case parent.Err() != nil:
		result.ObserveError = "interrupted before the status reflected the request"
	case wait.Interrupted(err):
		result.ObserveError = fmt.Sprintf("status did not reflect the request within %v", timeout)
		if lastErr != nil {
			result.ObserveError += fmt.Sprintf(" (last read error: %v)", lastErr)
		}
	default:
		result.ObserveError = err.Error()
	}
}

// isTransient reports whether err is an API error worth retrying while
// observing.
func isTransient(err error) bool {
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err)
}

// mergePatch returns a JSON merge patch that sets fields and carries uid as
// metadata.uid, which the apiserver checks: the patch is refused unless it
// reaches the object with that UID (not another created later under the
// same name).
func mergePatch(uid types.UID, fields map[string]interface{}) (client.Patch, error) {
	body := map[string]interface{}{"metadata": map[string]interface{}{"uid": string(uid)}}
	for k, v := range fields {
		body[k] = v
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode patch: %w", err)
	}
	return client.RawPatch(types.MergePatchType, data), nil
}

// vmObject returns a VirtualMachine object to patch or read vm through.
func (lg *LoadGenerator) vmObject(vm *runVM) *infrav1beta1.VirtualMachine {
	return &infrav1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: vm.name, Namespace: lg.namespace}}
}

// getVM reads the current state of vm.
func (lg *LoadGenerator) getVM(ctx context.Context, vm *runVM) (*infrav1beta1.VirtualMachine, error) {
	cur := &infrav1beta1.VirtualMachine{}
	if err := lg.client.Get(ctx, client.ObjectKey{Namespace: lg.namespace, Name: vm.name}, cur); err != nil {
		return nil, err
	}
	if cur.UID != vm.uid {
		return nil, fmt.Errorf("VM %s was replaced by another object", vm.name)
	}
	return cur, nil
}

// patchVM applies fields to vm (a merge patch with its UID as a
// precondition) and records the result's Duration and outcome. It reports
// whether the apiserver accepted the patch, and the VM's metadata.generation
// after it.
func (lg *LoadGenerator) patchVM(ctx context.Context, vm *runVM, fields map[string]interface{}, result *Result) (bool, int64) {
	patch, err := mergePatch(vm.uid, fields)
	if err != nil {
		result.Error = err.Error()
		return false, 0
	}
	obj := lg.vmObject(vm)
	result.StartTime = time.Now()
	err = lg.client.Patch(ctx, obj, patch)
	result.Duration = time.Since(result.StartTime)
	result.Success = err == nil
	if err != nil {
		result.Error = err.Error()
	}
	return result.Success, obj.Generation
}

// performPower toggles spec.powerState (On <-> Off) of a VM this run
// created. --observe waits until status.powerState matches.
func (lg *LoadGenerator) performPower(ctx context.Context, provider string, _ int) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	result := Result{Operation: opPower, Provider: provider, StartTime: time.Now()}
	vm, ok := lg.acquireVM()
	if !ok {
		result.Error = errNoRunVM.Error()
		lg.results <- result
		return
	}
	defer lg.releaseVM(vm)
	result.VMName, result.Provider = vm.name, vm.provider

	desired, observed := infrav1beta1.PowerStateOff, infrav1beta1.ObservedPowerStateOff
	if vm.power == infrav1beta1.PowerStateOff {
		desired, observed = infrav1beta1.PowerStateOn, infrav1beta1.ObservedPowerStateOn
	}
	result.Phase = string(desired)

	if lg.opts.dryRun {
		result.Success = true
		result.Phase = phaseDryRun
		vm.power = desired
		lg.results <- result
		return
	}

	if ok, _ := lg.patchVM(ctx, vm, map[string]interface{}{"spec": map[string]interface{}{"powerState": desired}}, &result); ok {
		vm.power = desired
		lg.observe(&result, func(ctx context.Context) (bool, error) {
			cur, err := lg.getVM(ctx, vm)
			if err != nil {
				return false, err
			}
			return cur.Status.PowerState == observed, nil
		})
	}
	lg.results <- result
}

// pickInRange returns a value in [r[0], r[1]] (or [defMin, defMax] when r is
// empty) that differs from current when the range allows it.
func pickInRange(r []int, defMin, defMax int, current int64) int64 {
	lo, hi := defMin, defMax
	if len(r) == 2 {
		lo, hi = r[0], r[1]
	}
	if hi <= lo {
		return int64(lo)
	}
	for attempt := 0; attempt < 8; attempt++ {
		v := int64(lo + rand.Intn(hi-lo+1)) // #nosec G404 -- picks a test size, not a secret
		if v != current {
			return v
		}
	}
	if current == int64(lo) {
		return int64(hi)
	}
	return int64(lo)
}

// performReconfigure sets spec.resources.cpu (from vmTemplate.resources
// .cpuRange, default 1..2) and, when vmTemplate.resources.memoryRange is
// set, spec.resources.memoryMiB of a VM this run created, each to a value
// different from the one this run last set: the change the VirtualMachine
// controller sends to the provider as a Reconfigure. --observe waits until
// status.currentResources records it, or until the operator reports the
// change as applied to the next boot only (Reconfiguring=True, reason
// RestartRequired, for this generation): a running VM's shrink, for one,
// never shows in status.currentResources, which records the larger size.
func (lg *LoadGenerator) performReconfigure(ctx context.Context, provider string, _ int) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	result := Result{Operation: opReconfigure, Provider: provider, StartTime: time.Now()}
	vm, ok := lg.acquireVM()
	if !ok {
		result.Error = errNoRunVM.Error()
		lg.results <- result
		return
	}
	defer lg.releaseVM(vm)
	result.VMName, result.Provider = vm.name, vm.provider

	res := lg.config.VMTemplate.Resources
	cpu := int32(pickInRange(res.CPURange, defaultReconfigureCPUMin, defaultReconfigureCPUMax, int64(vm.cpu))) // #nosec G115 -- validated range within [1, 128]
	resources := map[string]interface{}{"cpu": cpu}
	var memoryMiB int64
	if len(res.MemoryRange) == 2 {
		memoryMiB = pickInRange(res.MemoryRange, 0, 0, vm.memoryMiB)
		resources["memoryMiB"] = memoryMiB
	}
	result.Phase = fmt.Sprintf("cpu=%d", cpu)
	if memoryMiB > 0 {
		result.Phase += fmt.Sprintf(",memoryMiB=%d", memoryMiB)
	}

	if lg.opts.dryRun {
		result.Success = true
		result.Phase = phaseDryRun
		vm.cpu, vm.memoryMiB = cpu, memoryMiB
		lg.results <- result
		return
	}

	if ok, generation := lg.patchVM(ctx, vm, map[string]interface{}{"spec": map[string]interface{}{"resources": resources}}, &result); ok {
		vm.cpu = cpu
		if memoryMiB > 0 {
			vm.memoryMiB = memoryMiB
		}
		lg.observe(&result, func(ctx context.Context) (bool, error) {
			cur, err := lg.getVM(ctx, vm)
			if err != nil {
				return false, err
			}
			if c := meta.FindStatusCondition(cur.Status.Conditions, conditionReconfiguring); c != nil &&
				c.Status == metav1.ConditionTrue && c.Reason == reasonRestartRequired && c.ObservedGeneration >= generation {
				return true, nil
			}
			cr := cur.Status.CurrentResources
			if cr == nil || cr.CPU == nil || *cr.CPU != cpu {
				return false, nil
			}
			return memoryMiB == 0 || (cr.MemoryMiB != nil && *cr.MemoryMiB == memoryMiB), nil
		})
	}
	lg.results <- result
}

// performSnapshot creates a VMSnapshot of a VM this run created, with
// retentionPolicy.deleteOnVMDelete, so the snapshot is deleted with its VM.
// --observe waits until the snapshot's status.phase is Ready.
func (lg *LoadGenerator) performSnapshot(ctx context.Context, provider string, _ int) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	result := Result{Operation: opSnapshot, Provider: provider, StartTime: time.Now()}
	vm, ok := lg.acquireVM()
	if !ok {
		result.Error = errNoRunVM.Error()
		lg.results <- result
		return
	}
	defer lg.releaseVM(vm)
	result.VMName, result.Provider = vm.name, vm.provider

	snap := &infrav1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("loadgen-snap-%s-%d", lg.runID, lg.nextSeq()),
			Namespace: lg.namespace,
			Labels:    lg.vmLabels(),
		},
		Spec: infrav1beta1.VMSnapshotSpec{
			VMRef:           infrav1beta1.LocalObjectReference{Name: vm.name},
			RetentionPolicy: &infrav1beta1.SnapshotRetentionPolicy{DeleteOnVMDelete: true},
		},
	}
	result.Phase = snap.Name

	if lg.opts.dryRun {
		result.Success = true
		result.Phase = phaseDryRun
		lg.results <- result
		return
	}

	result.StartTime = time.Now()
	err := lg.client.Create(ctx, snap)
	result.Duration = time.Since(result.StartTime)
	result.Success = err == nil
	if err != nil {
		result.Error = err.Error()
		lg.results <- result
		return
	}
	lg.mu.Lock()
	lg.snapshots = append(lg.snapshots, runObject{name: snap.Name, uid: snap.UID})
	lg.mu.Unlock()

	lg.observe(&result, func(ctx context.Context) (bool, error) {
		cur := &infrav1beta1.VMSnapshot{}
		if err := lg.client.Get(ctx, client.ObjectKeyFromObject(snap), cur); err != nil {
			return false, err
		}
		if cur.Status.Phase == infrav1beta1.SnapshotPhaseFailed {
			return false, fmt.Errorf("snapshot failed: %s", cur.Status.Message)
		}
		return cur.Status.Phase == infrav1beta1.SnapshotPhaseReady, nil
	})
	lg.results <- result
}

// performClone creates a VMClone (full clone, target powered off) of a VM
// this run created, preferring one this run last powered off (some providers
// clone only a powered-off source and hold the clone until it is). The
// target VM it asks for carries the run's labels. --observe waits until the
// clone's status.phase is Ready.
func (lg *LoadGenerator) performClone(ctx context.Context, provider string, _ int) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	result := Result{Operation: opClone, Provider: provider, StartTime: time.Now()}
	vm, ok := lg.acquireVMPreferring(func(v *runVM) bool { return v.power == infrav1beta1.PowerStateOff })
	if !ok {
		result.Error = errNoRunVM.Error()
		lg.results <- result
		return
	}
	defer lg.releaseVM(vm)
	result.VMName, result.Provider = vm.name, vm.provider

	seq := lg.nextSeq()
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("loadgen-clone-%s-%d", lg.runID, seq),
			Namespace: lg.namespace,
			Labels:    lg.vmLabels(),
		},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: vm.name}},
			Target: infrav1beta1.VMCloneTarget{
				Name:   fmt.Sprintf("loadgen-vm-%s-clone-%d", lg.runID, seq),
				Labels: lg.vmLabels(),
			},
		},
	}
	result.Phase = clone.Spec.Target.Name

	if lg.opts.dryRun {
		result.Success = true
		result.Phase = phaseDryRun
		lg.results <- result
		return
	}

	result.StartTime = time.Now()
	err := lg.client.Create(ctx, clone)
	result.Duration = time.Since(result.StartTime)
	result.Success = err == nil
	if err != nil {
		result.Error = err.Error()
		lg.results <- result
		return
	}
	lg.mu.Lock()
	lg.clones = append(lg.clones, runObject{name: clone.Name, uid: clone.UID, source: vm.name, target: clone.Spec.Target.Name})
	lg.cloneTargets = append(lg.cloneTargets, clone.Spec.Target.Name)
	lg.mu.Unlock()

	lg.observe(&result, func(ctx context.Context) (bool, error) {
		cur := &infrav1beta1.VMClone{}
		if err := lg.client.Get(ctx, client.ObjectKeyFromObject(clone), cur); err != nil {
			return false, err
		}
		if cur.Status.Phase == infrav1beta1.ClonePhaseFailed {
			return false, fmt.Errorf("clone failed: %s", cur.Status.Message)
		}
		return cur.Status.Phase == infrav1beta1.ClonePhaseReady, nil
	})
	lg.results <- result
}
