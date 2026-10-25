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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/cliout"
)

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
// and its VMs. Every delete carries the object's UID as a precondition, so
// only the object this run created (or, for a clone target, the one this run
// labelled) is deleted. It returns the objects still present.
func (lg *LoadGenerator) cleanup(ctx context.Context) []string {
	lg.mu.Lock()
	var snaps, rest []cleanupItem
	for _, s := range lg.snapshots {
		snaps = append(snaps, cleanupItem{obj: &infrav1beta1.VMSnapshot{}, uid: s.uid})
		snaps[len(snaps)-1].obj.SetName(s.name)
	}
	for _, c := range lg.clones {
		rest = append(rest, cleanupItem{obj: &infrav1beta1.VMClone{}, uid: c.uid})
		rest[len(rest)-1].obj.SetName(c.name)
	}
	targets := append([]string{}, lg.cloneTargets...)
	for _, vm := range lg.vms {
		rest = append(rest, cleanupItem{obj: &infrav1beta1.VirtualMachine{}, uid: vm.uid})
		rest[len(rest)-1].obj.SetName(vm.name)
	}
	lg.mu.Unlock()

	for _, name := range targets {
		if item, ok := lg.ownedCloneTarget(ctx, name); ok {
			rest = append(rest, item)
		}
	}

	left := lg.deleteAndWait(ctx, snaps)
	left = append(left, lg.deleteAndWait(ctx, rest)...)
	sort.Strings(left)
	return left
}

// ownedCloneTarget returns the clone target VM named name when it carries
// this run's label; a VM under that name without it is not this run's.
func (lg *LoadGenerator) ownedCloneTarget(ctx context.Context, name string) (cleanupItem, bool) {
	vm := &infrav1beta1.VirtualMachine{}
	if err := lg.client.Get(ctx, client.ObjectKey{Namespace: lg.namespace, Name: name}, vm); err != nil {
		return cleanupItem{}, false
	}
	if vm.Labels[loadgenRunLabelKey] != lg.runID {
		return cleanupItem{}, false
	}
	return cleanupItem{obj: vm, uid: vm.UID}, true
}

// deleteAndWait deletes items and waits until each is gone (or replaced by
// another object under its name). It returns the items still present when
// ctx ends.
func (lg *LoadGenerator) deleteAndWait(ctx context.Context, items []cleanupItem) []string {
	var pending []cleanupItem
	for _, item := range items {
		item.obj.SetNamespace(lg.namespace)
		uid := item.uid
		err := lg.client.Delete(ctx, item.obj, client.Preconditions{UID: &uid})
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
