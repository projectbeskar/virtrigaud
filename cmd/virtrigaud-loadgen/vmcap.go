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

package main

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// phaseSkipped is the Phase of an operation the run did not perform because
// the number of VMs it has alive reached vmCount.
const phaseSkipped = "Skipped: vmCount reached"

// aliveLocked returns the number of VMs the run has alive, which is what
// vmCount caps: the VMs it created and has not deleted (a clone's target
// joins them once its clone has ended, see settleClones), the VMs whose delete
// was sent but is not confirmed complete, the targets of the clones that have
// not ended, and the slots reserved for a create or clone whose API call is in
// flight. lg.mu must be held.
func (lg *LoadGenerator) aliveLocked() int {
	pending := 0
	for _, c := range lg.clones {
		if !c.resolved {
			pending++
		}
	}
	return len(lg.vms) + len(lg.deleting) + pending + lg.reserved
}

// reserveSlot reserves room for one more VM (the new VM of a create, the
// target of a clone) and reports whether there was any. A reservation not
// followed by commitCreate or commitClone must be given back with
// releaseSlot. When the cap is reached it first checks whether the VMs whose
// delete the run sent are gone by now, and counts only those still present.
func (lg *LoadGenerator) reserveSlot(ctx context.Context) bool {
	if lg.tryReserve() {
		return true
	}
	if lg.dueToForget() {
		lg.forgetDeleted(ctx)
	}
	return lg.tryReserve()
}

// dueToForget reports whether the VMs being deleted should be read again, and
// records that they are: the reads are limited to one round per pollInterval,
// however many workers meet the cap.
func (lg *LoadGenerator) dueToForget() bool {
	interval := lg.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if !lg.lastForget.IsZero() && time.Since(lg.lastForget) < interval {
		return false
	}
	lg.lastForget = time.Now()
	return true
}

// tryReserve reserves a slot when the run has fewer than vmCount VMs alive.
func (lg *LoadGenerator) tryReserve() bool {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if lg.aliveLocked() >= lg.config.VMCount {
		return false
	}
	lg.reserved++
	return true
}

// releaseSlot gives back a slot reserved with reserveSlot.
func (lg *LoadGenerator) releaseSlot() {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.reserved--
}

// commitCreate records vm, the VM the reserved slot was for, as created.
func (lg *LoadGenerator) commitCreate(vm *runVM) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.vms = append(lg.vms, vm)
	lg.reserved--
}

// commitClone records clone, the VMClone the reserved slot was for, and the
// target VM it asks for. A non-nil target is that VM, tracked as one of the
// run's VMs at once (a dry run).
func (lg *LoadGenerator) commitClone(clone runObject, target *runVM) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.clones = append(lg.clones, clone)
	lg.cloneTargets = append(lg.cloneTargets, clone.target)
	if target != nil {
		lg.vms = append(lg.vms, target)
	}
	lg.reserved--
}

// settleClones reads the VMClones the run requested that are not known to have
// ended, and the target VMs of those that have. A Ready clone whose target
// exists and carries the run's label makes that VM one of the run's VMs, so
// every operation, delete included, can act on it and free its slot. A Failed
// or vanished clone, or a Ready one whose target is being deleted, does not
// free the slot while its target VM is still there: the operator removes the
// target of a failed clone later, and keeps a bound one. The target is then
// tracked as a VM being deleted, which counts until it is gone and which
// --cleanup deletes. A clone still in flight, or one (or a target) that cannot
// be read, stays as it is. Until a clone has ended, its source is not deleted
// by the run.
func (lg *LoadGenerator) settleClones(ctx context.Context) {
	if lg.opts.dryRun {
		return
	}
	lg.mu.Lock()
	var pending []runObject
	for _, c := range lg.clones {
		if !c.resolved {
			pending = append(pending, c)
		}
	}
	lg.mu.Unlock()

	for _, c := range pending {
		cur := &infrav1beta1.VMClone{}
		err := lg.client.Get(ctx, client.ObjectKey{Namespace: lg.namespace, Name: c.name}, cur)
		var ready bool
		switch {
		case apierrors.IsNotFound(err) || (err == nil && cur.UID != c.uid):
		case err != nil:
			continue
		case cur.Status.Phase == infrav1beta1.ClonePhaseFailed:
		case cur.Status.Phase == infrav1beta1.ClonePhaseReady:
			ready = true
		default:
			continue
		}

		item, found, readErr := lg.ownedCloneTarget(ctx, c.target)
		switch {
		case readErr != nil:
			continue
		case !found:
			lg.resolveClone(c.name, nil, nil)
		case ready && item.obj.GetDeletionTimestamp() == nil:
			target := &runVM{name: c.target, uid: item.uid, provider: c.provider, power: infrav1beta1.PowerStateOff}
			if vm, ok := item.obj.(*infrav1beta1.VirtualMachine); ok && vm.Spec.PowerState != "" {
				target.power = vm.Spec.PowerState
			}
			lg.resolveClone(c.name, target, nil)
		default:
			lg.resolveClone(c.name, nil, &runObject{name: c.target, uid: item.uid})
		}
	}
}

// resolveClone marks the VMClone name as ended. A non-nil target is tracked as
// one of the run's VMs; a non-nil held is a target VM that still exists but is
// not usable, tracked as a VM being deleted.
func (lg *LoadGenerator) resolveClone(name string, target *runVM, held *runObject) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	for i := range lg.clones {
		if lg.clones[i].name == name && !lg.clones[i].resolved {
			lg.clones[i].resolved = true
			if target != nil {
				lg.vms = append(lg.vms, target)
			}
			if held != nil {
				lg.deleting = append(lg.deleting, *held)
			}
			return
		}
	}
}

// dueToSettle reports whether the pending VMClones should be read again, and
// records that they are: delete operations read them at most once per
// pollInterval, outside the latency they record.
func (lg *LoadGenerator) dueToSettle() bool {
	interval := lg.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if !lg.lastSettle.IsZero() && time.Since(lg.lastSettle) < interval {
		return false
	}
	lg.lastSettle = time.Now()
	return true
}

// forgetDeleted drops from lg.deleting the VMs that are gone (or replaced by
// another object under their name), so they stop counting as alive. A VM it
// cannot read keeps counting.
func (lg *LoadGenerator) forgetDeleted(ctx context.Context) {
	if lg.opts.dryRun {
		return
	}
	lg.mu.Lock()
	pending := append([]runObject{}, lg.deleting...)
	lg.mu.Unlock()
	if len(pending) == 0 {
		return
	}

	gone := make(map[string]bool, len(pending))
	for _, d := range pending {
		cur := &infrav1beta1.VirtualMachine{}
		err := lg.client.Get(ctx, client.ObjectKey{Namespace: lg.namespace, Name: d.name}, cur)
		if apierrors.IsNotFound(err) || (err == nil && cur.UID != d.uid) {
			gone[d.name] = true
		}
	}
	if len(gone) == 0 {
		return
	}

	lg.mu.Lock()
	defer lg.mu.Unlock()
	kept := lg.deleting[:0]
	for _, d := range lg.deleting {
		if !gone[d.name] {
			kept = append(kept, d)
		}
	}
	lg.deleting = kept
}

// skippedResult returns the result of an operation of the given kind that was
// not performed because the run has vmCount VMs alive. It is neither a success
// nor a failure.
func skippedResult(operation, provider string) Result {
	return Result{
		Operation: operation,
		Provider:  provider,
		StartTime: time.Now(),
		Skipped:   true,
		Phase:     phaseSkipped,
	}
}
