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
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/cliout"
)

// conditionCloneRequested is the VMClone condition the operator records
// before it sends a Clone to the provider (and removes once the outcome is
// recorded): while it is present, a provider clone may be in flight.
const conditionCloneRequested = "CloneRequested"

// cleanupDeleteTimeout bounds the delete calls --cleanup still sends once
// its timeout is used up.
const cleanupDeleteTimeout = 30 * time.Second

// errCleanupIncomplete is returned when --cleanup could not delete
// everything the run created in time.
var errCleanupIncomplete = errors.New("cleanup incomplete")

// cleanupItem is one object the cleanup deletes.
type cleanupItem struct {
	obj client.Object
	uid types.UID
}

// String renders the item as Kind/name.
func (c cleanupItem) String() string {
	kind := fmt.Sprintf("%T", c.obj)
	kind = kind[strings.LastIndex(kind, ".")+1:]
	return kind + "/" + c.obj.GetName()
}

// finish runs --cleanup, or reports what the run left in place.
func (lg *LoadGenerator) finish(out io.Writer) error {
	lg.mu.Lock()
	nVMs, nSnaps, nClones := len(lg.vms), len(lg.snapshots), len(lg.clones)
	lg.mu.Unlock()

	if lg.opts.dryRun {
		return nil
	}
	if !lg.opts.cleanup {
		if nVMs+nSnaps+nClones > 0 {
			cliout.Printf(out, "Left in place: %d VMs, %d VMSnapshots, %d VMClones (and their target VMs). Remove them with:\n", nVMs, nSnaps, nClones)
			cliout.Printf(out, "  kubectl delete vmsnapshots,vmclones,virtualmachines -n %s -l %s=%s\n", lg.namespace, loadgenRunLabelKey, lg.runID)
		}
		return nil
	}

	parent := lg.observeCtx
	if parent == nil {
		parent = context.Background()
	}
	timeout := lg.opts.cleanupTimeout
	if timeout <= 0 {
		timeout = defaultCleanupTimeout
	}
	// Cleanup also runs after an interrupt, bounded by its own timeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()

	cliout.Printf(out, "Cleaning up (up to %v)...\n", timeout)
	left := lg.cleanup(ctx)
	if len(left) > 0 {
		cliout.Printf(out, "Cleanup incomplete; still present: %s\n", strings.Join(left, ", "))
		return fmt.Errorf("%w: %d objects still present", errCleanupIncomplete, len(left))
	}
	cliout.Printf(out, "Cleanup complete\n")
	return nil
}

// cleanup deletes what the run created and waits until it is gone: first its
// VMSnapshots (so each is deleted through its provider while its VM still
// exists), then its VMClones, the clone targets that carry this run's label,
// its VMs, and the VMs whose delete it issued during the run. Every delete
// carries the object's UID as a precondition, so only the object this run
// created (or, for a clone target, the one this run labelled) is deleted.
//
// A VMClone whose provider clone may still be in flight is waited for first:
// deleting it, its target or its source then could leave the cloned VM on the
// hypervisor with nothing recording it. A clone that does not settle in time
// is left in place with its source and target, and reported.
//
// It returns the objects still present (or left in place).
func (lg *LoadGenerator) cleanup(ctx context.Context) []string {
	lg.mu.Lock()
	snapshots := append([]runObject{}, lg.snapshots...)
	clones := append([]runObject{}, lg.clones...)
	targets := append([]string{}, lg.cloneTargets...)
	vms := make([]runObject, 0, len(lg.vms)+len(lg.deleting))
	for _, vm := range lg.vms {
		vms = append(vms, runObject{name: vm.name, uid: vm.uid})
	}
	vms = append(vms, lg.deleting...)
	lg.mu.Unlock()

	var left []string
	keepVM := map[string]bool{}
	var settled []runObject
	for _, c := range clones {
		if lg.cloneSettled(ctx, c) {
			settled = append(settled, c)
			continue
		}
		left = append(left, "VMClone/"+c.name+" (clone in flight)")
		keepVM[c.source], keepVM[c.target] = true, true
	}

	var snaps, rest []cleanupItem
	for _, s := range snapshots {
		snaps = append(snaps, newCleanupItem(&infrav1beta1.VMSnapshot{}, s))
	}
	for _, c := range settled {
		rest = append(rest, newCleanupItem(&infrav1beta1.VMClone{}, c))
	}
	for _, name := range targets {
		if keepVM[name] {
			left = append(left, "VirtualMachine/"+name+" (target of a clone in flight)")
			continue
		}
		item, found, err := lg.ownedCloneTarget(ctx, name)
		switch {
		case err != nil:
			left = append(left, fmt.Sprintf("VirtualMachine/%s (could not be read: %v)", name, err))
		case found:
			rest = append(rest, item)
		}
	}
	for _, vm := range vms {
		if keepVM[vm.name] {
			left = append(left, "VirtualMachine/"+vm.name+" (source of a clone in flight)")
			continue
		}
		rest = append(rest, newCleanupItem(&infrav1beta1.VirtualMachine{}, vm))
	}

	left = append(left, lg.deleteAndWait(ctx, snaps)...)
	left = append(left, lg.deleteAndWait(ctx, rest)...)
	sort.Strings(left)
	return left
}

// newCleanupItem returns a cleanup item for o, read and deleted through obj.
func newCleanupItem(obj client.Object, o runObject) cleanupItem {
	obj.SetName(o.name)
	return cleanupItem{obj: obj, uid: o.uid}
}

// cloneSettled waits, within ctx, until the VMClone c has no provider clone
// in flight — it is Ready or Failed, it has not requested a clone yet, or it
// is gone — and reports whether that happened.
func (lg *LoadGenerator) cloneSettled(ctx context.Context, c runObject) bool {
	interval := lg.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		cur := &infrav1beta1.VMClone{}
		err := lg.client.Get(ctx, client.ObjectKey{Namespace: lg.namespace, Name: c.name}, cur)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, nil
		}
		if cur.UID != c.uid {
			return true, nil
		}
		switch cur.Status.Phase {
		case infrav1beta1.ClonePhaseReady, infrav1beta1.ClonePhaseFailed:
			return true, nil
		case "", infrav1beta1.ClonePhasePending:
			return meta.FindStatusCondition(cur.Status.Conditions, conditionCloneRequested) == nil, nil
		default:
			return false, nil
		}
	})
	return err == nil
}

// ownedCloneTarget reads the clone target VM named name. It reports found
// when the VM exists and carries this run's label (a VM under that name
// without it is not this run's), and an error when it cannot be read.
func (lg *LoadGenerator) ownedCloneTarget(ctx context.Context, name string) (cleanupItem, bool, error) {
	vm := &infrav1beta1.VirtualMachine{}
	if err := lg.client.Get(ctx, client.ObjectKey{Namespace: lg.namespace, Name: name}, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return cleanupItem{}, false, nil
		}
		return cleanupItem{}, false, err
	}
	if vm.Labels[loadgenRunLabelKey] != lg.runID {
		return cleanupItem{}, false, nil
	}
	return cleanupItem{obj: vm, uid: vm.UID}, true, nil
}

// deleteAndWait deletes items and waits until each is gone (or replaced by
// another object under its name). It returns the items still present when
// ctx ends (after ctx has ended it still sends the deletes, and reports every
// item it sent one for).
func (lg *LoadGenerator) deleteAndWait(ctx context.Context, items []cleanupItem) []string {
	// Once the cleanup timeout is used up, the deletes are still sent, each
	// with a short timeout of its own, but not waited for.
	callCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), cleanupDeleteTimeout)
		defer cancel()
	}
	var pending []cleanupItem
	for _, item := range items {
		item.obj.SetNamespace(lg.namespace)
		uid := item.uid
		err := lg.client.Delete(callCtx, item.obj, client.Preconditions{UID: &uid})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			pending = append(pending, item) // retried below by waiting; reported if still present
			continue
		}
		if err == nil {
			pending = append(pending, item)
		}
	}

	interval := lg.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	_ = wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		var still []cleanupItem
		for _, item := range pending {
			cur, ok := item.obj.DeepCopyObject().(client.Object)
			if !ok {
				continue
			}
			err := lg.client.Get(ctx, client.ObjectKeyFromObject(item.obj), cur)
			if apierrors.IsNotFound(err) || (err == nil && cur.GetUID() != item.uid) {
				continue
			}
			if err == nil && cur.GetDeletionTimestamp() == nil {
				// The delete call failed earlier; try again.
				uid := item.uid
				_ = lg.client.Delete(ctx, cur, client.Preconditions{UID: &uid})
			}
			still = append(still, item)
		}
		pending = still
		return len(pending) == 0, nil
	})

	left := make([]string, 0, len(pending))
	for _, item := range pending {
		left = append(left, item.String())
	}
	return left
}
