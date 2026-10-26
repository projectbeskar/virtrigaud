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
	nHeld := len(lg.deleting)
	nPendingTargets := 0
	for _, c := range lg.clones {
		if !c.resolved {
			nPendingTargets++
		}
	}
	lg.mu.Unlock()

	if lg.opts.dryRun {
		return nil
	}
	if !lg.opts.cleanup {
		if nVMs+nSnaps+nClones > 0 {
			cliout.Printf(out, "Left in place: %d VMs, %d VMSnapshots, %d VMClones%s. Remove them with:\n", nVMs, nSnaps, nClones,
				pendingTargetsNote(nPendingTargets, nHeld))
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

// pendingTargetsNote renders, for the "Left in place" line, the VMs that are
// not among the counted ones: the target VMs of the clones that had not ended
// (pending) and the VMs being deleted or kept from a failed clone (held, which
// may be gone by now).
func pendingTargetsNote(pending, held int) string {
	var parts []string
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("the target VMs of %d clones that had not ended", pending))
	}
	if held > 0 {
		parts = append(parts, fmt.Sprintf("up to %d VMs being deleted or kept from failed clones", held))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (not counted in the VMs: " + strings.Join(parts, " and ") + ")"
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
// Objects that carry the run's label but were never recorded (a create whose
// answer was lost after the apiserver stored the object) are cleaned up the
// same way.
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

	// Objects carrying the run's label that the run did not record: a create
	// whose answer was lost (a timeout, a dropped connection) after the
	// apiserver stored the object. They are cleaned up like the others.
	var left []string
	extraSnaps, extraClones, extraVMs, listErrs := lg.untrackedObjects(ctx, snapshots, clones, targets, vms)
	left = append(left, listErrs...)
	snapshots = append(snapshots, extraSnaps...)
	clones = append(clones, extraClones...)
	vms = append(vms, extraVMs...)
	// The target of an unrecorded clone is looked for too: it may appear
	// while the clone is waited for.
	for _, c := range extraClones {
		targets = append(targets, c.target)
	}

	// A clone target the run took over (see settleClones) is among its VMs.
	tracked := make(map[string]bool, len(vms))
	for _, vm := range vms {
		tracked[vm.name] = true
	}

	keepVM := map[string]bool{}
	keepAsTarget := map[string]bool{}
	var settled []runObject
	for _, c := range clones {
		if lg.cloneSettled(ctx, c) {
			settled = append(settled, c)
			continue
		}
		left = append(left, "VMClone/"+c.name+" (clone in flight)")
		keepVM[c.source], keepVM[c.target] = true, true
		keepAsTarget[c.target] = true
	}

	var snaps, rest []cleanupItem
	for _, s := range snapshots {
		snaps = append(snaps, newCleanupItem(&infrav1beta1.VMSnapshot{}, s))
	}
	for _, c := range settled {
		rest = append(rest, newCleanupItem(&infrav1beta1.VMClone{}, c))
	}
	for _, name := range targets {
		if tracked[name] {
			continue
		}
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
			role := "source"
			if keepAsTarget[vm.name] {
				role = "target"
			}
			left = append(left, "VirtualMachine/"+vm.name+" ("+role+" of a clone in flight)")
			continue
		}
		rest = append(rest, newCleanupItem(&infrav1beta1.VirtualMachine{}, vm))
	}

	left = append(left, lg.deleteAndWait(ctx, snaps)...)
	left = append(left, lg.deleteAndWait(ctx, rest)...)
	sort.Strings(left)
	return left
}

// untrackedObjects lists the VMSnapshots, VMClones and VirtualMachines that
// carry this run's label and are not among the ones the run recorded
// (snapshots, clones, clone target names and vms), with their UIDs. A list
// that fails is returned as an entry for the "still present" report: the
// cleanup cannot say it left nothing behind.
func (lg *LoadGenerator) untrackedObjects(ctx context.Context, snapshots, clones []runObject, targets []string, vms []runObject) (
	snaps, cls, vmList []runObject, failures []string) {
	known := func(objs []runObject, extra ...string) map[string]bool {
		m := make(map[string]bool, len(objs)+len(extra))
		for _, o := range objs {
			m[o.name] = true
		}
		for _, n := range extra {
			m[n] = true
		}
		return m
	}
	knownSnaps, knownClones, knownVMs := known(snapshots), known(clones), known(vms, targets...)
	opts := []client.ListOption{client.InNamespace(lg.namespace), client.MatchingLabels{loadgenRunLabelKey: lg.runID}}

	snapList := &infrav1beta1.VMSnapshotList{}
	if err := lg.client.List(ctx, snapList, opts...); err != nil {
		failures = append(failures, fmt.Sprintf("VMSnapshots (could not be listed: %v)", err))
	}
	for _, o := range snapList.Items {
		if !knownSnaps[o.Name] {
			snaps = append(snaps, runObject{name: o.Name, uid: o.UID})
		}
	}
	cloneList := &infrav1beta1.VMCloneList{}
	if err := lg.client.List(ctx, cloneList, opts...); err != nil {
		failures = append(failures, fmt.Sprintf("VMClones (could not be listed: %v)", err))
	}
	for _, o := range cloneList.Items {
		if !knownClones[o.Name] {
			c := runObject{name: o.Name, uid: o.UID, target: o.Spec.Target.Name}
			if o.Spec.Source.VMRef != nil {
				c.source = o.Spec.Source.VMRef.Name
			}
			cls = append(cls, c)
		}
	}
	vmObjs := &infrav1beta1.VirtualMachineList{}
	if err := lg.client.List(ctx, vmObjs, opts...); err != nil {
		failures = append(failures, fmt.Sprintf("VirtualMachines (could not be listed: %v)", err))
	}
	for _, o := range vmObjs.Items {
		if !knownVMs[o.Name] {
			vmList = append(vmList, runObject{name: o.Name, uid: o.UID})
		}
	}
	return snaps, cls, vmList, failures
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
