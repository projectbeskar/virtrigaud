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
// vmCount caps: the VMs it created and has not deleted (including a delete
// that was sent but is not confirmed complete), the target VMs of the clones it
// requested, and the slots reserved for a create or clone whose API call is in
// flight. A clone target counts from the moment its VMClone is accepted until
// the run ends, even when that clone later fails or its target is deleted by
// someone else: the run never learns that, and the cap errs on the side of
// fewer VMs. lg.mu must be held.
func (lg *LoadGenerator) aliveLocked() int {
	return len(lg.vms) + len(lg.deleting) + len(lg.cloneTargets) + lg.reserved
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
	lg.forgetDeleted(ctx)
	return lg.tryReserve()
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
// target VM it asks for.
func (lg *LoadGenerator) commitClone(clone runObject) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.clones = append(lg.clones, clone)
	lg.cloneTargets = append(lg.cloneTargets, clone.target)
	lg.reserved--
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
func (lg *LoadGenerator) skippedResult(operation, provider string) Result {
	return Result{
		Operation: operation,
		Provider:  provider,
		StartTime: time.Now(),
		Skipped:   true,
		Phase:     phaseSkipped,
	}
}
