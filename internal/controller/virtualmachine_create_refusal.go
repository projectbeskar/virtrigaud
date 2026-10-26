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

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// A VirtualMachine Create the provider refuses as an invalid specification
// (InvalidSpec, gRPC InvalidArgument) is sent again on a backoff, not on a
// fixed cadence (v0.4.1): a libvirt Create copies the VM's base image before
// the definition is refused, so a refusal re-sent every 30 seconds copied
// gigabytes every 30 seconds for as long as the VM existed.
//
// The re-check waits createRefusalRetryBase after the first refusal, twice as
// long after each consecutive one with the same answer, at most
// createRefusalRetryMax. The answer can change only when its inputs do, so
// the wait ends at once — and the run starts again from the base — when any
// of them changes (createRefusalInputs): the VirtualMachine's spec (its
// generation), or the spec of its Provider, VMClass, VMImage or network
// attachments (their UID or generation). A different refusal message also
// starts the run again. The Provider, VMClass and VMImage are watched for
// such a VM (withCreateRefusalWatches), so editing one retries the create
// within seconds; an edit of the VM itself is a new generation, which the VM
// watch already passes.
//
// The run is kept in memory, per VM (createRefusalBackoff). After a manager
// restart it is recovered from the VM's Ready condition, whose message names
// the wait and the time of the next re-check (createRefusalMessage): a VM
// whose condition is for its current generation waits until that time, and
// its next refusal continues the run. A change to the Provider, VMClass,
// VMImage or networks made while no manager ran is therefore picked up at
// that re-check, at most createRefusalRetryMax later.
const (
	// createRefusalRetryBase is the wait after the first refusal of a run.
	createRefusalRetryBase = vmCreateInvalidSpecRetryInterval
	// createRefusalRetryMax bounds the wait.
	createRefusalRetryMax = 10 * time.Minute
	// createRefusalForget is how long after its wait ended a run is kept: a
	// VM not re-checked for that long (deleted, or created) no longer holds
	// memory.
	createRefusalForget = time.Hour
)

// The Ready / Provisioning message of a refused Create. It is parsed back
// after a manager restart (parseCreateRefusalMessage), so its parts are
// constants: createRefusalMessagePrefix, the wait, createRefusalMessageAt,
// the time of the next re-check (RFC 3339, UTC), createRefusalMessageTail,
// then the provider's sanitized detail.
const (
	createRefusalMessagePrefix = "Provider rejected VM create (non-retryable; re-checking in "
	createRefusalMessageAt     = ", at "
	createRefusalMessageTail   = ", or at once when the VirtualMachine, its VMClass, VMImage or Provider changes): "
)

// createRefusalMessage renders the condition message of a Create refused with
// detail, re-checked after wait, at next.
func createRefusalMessage(wait time.Duration, next time.Time, detail string) string {
	return createRefusalMessagePrefix + wait.String() + createRefusalMessageAt +
		next.UTC().Format(time.RFC3339) + createRefusalMessageTail + detail
}

// parseCreateRefusalMessage recovers the wait, the time of the next re-check
// and the provider's detail from a createRefusalMessage; ok is false for any
// other text.
func parseCreateRefusalMessage(msg string) (wait time.Duration, next time.Time, detail string, ok bool) {
	rest, found := strings.CutPrefix(msg, createRefusalMessagePrefix)
	if !found {
		return 0, time.Time{}, "", false
	}
	waitText, rest, found := strings.Cut(rest, createRefusalMessageAt)
	if !found {
		return 0, time.Time{}, "", false
	}
	nextText, detail, found := strings.Cut(rest, createRefusalMessageTail)
	if !found {
		return 0, time.Time{}, "", false
	}
	wait, err := time.ParseDuration(waitText)
	if err != nil || wait <= 0 {
		return 0, time.Time{}, "", false
	}
	next, err = time.Parse(time.RFC3339, nextText)
	if err != nil {
		return 0, time.Time{}, "", false
	}
	return wait, next, detail, true
}

// createRefusalDelay is the wait after refusals consecutive refusals (at
// least 1): createRefusalRetryBase doubling, at most createRefusalRetryMax.
func createRefusalDelay(refusals int) time.Duration {
	d := createRefusalRetryBase
	for i := 1; i < refusals && d < createRefusalRetryMax; i++ {
		d *= 2
	}
	return min(d, createRefusalRetryMax)
}

// createRefusalsFor is the number of consecutive refusals whose wait is wait
// (the inverse of createRefusalDelay), for a run recovered from a condition.
func createRefusalsFor(wait time.Duration) int {
	n := 1
	for createRefusalDelay(n) < wait && createRefusalDelay(n) < createRefusalRetryMax {
		n++
	}
	return n
}

// createRefusalInputs identifies everything the answer to a VM's Create
// depends on that the manager can see: the VM's generation, and the UID and
// generation of its Provider, VMClass, VMImage and network attachments. Only
// spec changes move a generation, so a Provider's or VMImage's frequent
// status writes do not end a wait.
func createRefusalInputs(
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider *infravirtrigaudiov1beta1.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) string {
	parts := []string{"vm=" + strconv.FormatInt(vm.Generation, 10)}
	add := func(name string, uid types.UID, generation int64) {
		parts = append(parts, name+"="+string(uid)+"/"+strconv.FormatInt(generation, 10))
	}
	if provider != nil {
		add("provider", provider.UID, provider.Generation)
	}
	if vmClass != nil {
		add("class", vmClass.UID, vmClass.Generation)
	}
	if vmImage != nil {
		add("image", vmImage.UID, vmImage.Generation)
	}
	for _, n := range networks {
		if n != nil {
			add("network", n.UID, n.Generation)
		}
	}
	return strings.Join(parts, "|")
}

// createRefusalBackoff remembers, per VirtualMachine UID, the run of
// consecutive definite refusals of its Create and when it may be sent again.
// The zero value is ready to use and it is safe for concurrent use. In-process
// state is correct: only the elected leader runs this reconciler.
type createRefusalBackoff struct {
	mu      sync.Mutex
	entries map[types.UID]*createRefusalEntry
}

// createRefusalEntry is one VM's run of refusals.
type createRefusalEntry struct {
	// inputs are the createRefusalInputs the refusals answered.
	inputs string
	// detail is the provider's (sanitized) answer.
	detail string
	// refusals is the number of consecutive refusals with that answer.
	refusals int
	// until is when the Create may be sent again.
	until time.Time
}

// prune drops runs whose wait ended more than createRefusalForget ago. Call
// it with mu held.
func (b *createRefusalBackoff) prune(now time.Time) {
	for uid, e := range b.entries {
		if now.Sub(e.until) > createRefusalForget {
			delete(b.entries, uid)
		}
	}
}

// refused records, at now, a refusal of the VM uid's Create with detail for
// inputs and returns how long to wait before sending it again: the run goes
// on when inputs and detail are those of the last refusal, and starts again
// otherwise.
func (b *createRefusalBackoff) refused(uid types.UID, inputs, detail string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[types.UID]*createRefusalEntry{}
	}
	b.prune(now)
	e, ok := b.entries[uid]
	if !ok || e.inputs != inputs || e.detail != detail {
		e = &createRefusalEntry{inputs: inputs, detail: detail}
		b.entries[uid] = e
	}
	e.refusals++
	d := createRefusalDelay(e.refusals)
	e.until = now.Add(d)
	return d
}

// wait returns how much of the VM uid's wait is left at now for inputs; 0
// when the Create may be sent. A run recorded for other inputs is dropped:
// something the answer depends on changed, so the Create is sent at once.
func (b *createRefusalBackoff) wait(uid types.UID, inputs string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[uid]
	if !ok {
		return 0
	}
	if e.inputs != inputs {
		delete(b.entries, uid)
		return 0
	}
	if !now.Before(e.until) {
		return 0
	}
	return e.until.Sub(now)
}

// known reports whether a run is recorded for the VM uid.
func (b *createRefusalBackoff) known(uid types.UID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.entries[uid]
	return ok
}

// seed records a run recovered from a VM's condition (after a manager
// restart) unless one is recorded already.
func (b *createRefusalBackoff) seed(uid types.UID, inputs, detail string, refusals int, until, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[types.UID]*createRefusalEntry{}
	}
	b.prune(now)
	if _, ok := b.entries[uid]; ok {
		return
	}
	b.entries[uid] = &createRefusalEntry{inputs: inputs, detail: detail, refusals: refusals, until: until}
}

// forget drops the VM uid's run.
func (b *createRefusalBackoff) forget(uid types.UID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, uid)
}

// recordCreateRefusal records that the provider refused vm's Create as an
// invalid specification with detail, for inputs, and returns the wait before
// it is sent again.
func (r *VirtualMachineReconciler) recordCreateRefusal(vm *infravirtrigaudiov1beta1.VirtualMachine, inputs, detail string) (time.Duration, time.Time) {
	now := r.now()
	wait := r.createRefusals.refused(vm.UID, inputs, detail, now)
	return wait, now.Add(wait)
}

// recoverCreateRefusal seeds vm's run from its Ready condition when no run is
// in memory (after a manager restart): a create refusal recorded for the VM's
// current generation by createRefusalMessage. A time of the next re-check
// further away than createRefusalRetryMax (a clock that moved) is brought
// back to that bound.
func (r *VirtualMachineReconciler) recoverCreateRefusal(vm *infravirtrigaudiov1beta1.VirtualMachine, inputs string, now time.Time) {
	if r.createRefusals.known(vm.UID) {
		return
	}
	ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	if ready == nil || ready.Reason != k8s.ReasonValidationError || ready.ObservedGeneration != vm.Generation {
		return
	}
	wait, next, detail, ok := parseCreateRefusalMessage(ready.Message)
	if !ok || !now.Before(next) {
		return
	}
	if limit := now.Add(createRefusalRetryMax); next.After(limit) {
		next = limit
	}
	r.createRefusals.seed(vm.UID, inputs, detail, createRefusalsFor(wait), next, now)
}

// holdRefusedCreate holds the Create of vm while the wait after its last
// definite refusal runs (see createRefusalBackoff): nothing is sent to the
// provider — no image prepare, placement or Create — and the VM is
// requeued for the rest of the wait. held is false when the Create may be
// sent: no refusal, its wait is over, or something it depends on changed.
func (r *VirtualMachineReconciler) holdRefusedCreate(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	deps vmDependencies,
) (ctrl.Result, bool) {
	now := r.now()
	inputs := createRefusalInputs(vm, deps.provider, deps.class, deps.image, deps.networks)
	r.recoverCreateRefusal(vm, inputs, now)
	wait := r.createRefusals.wait(vm.UID, inputs, now)
	if wait <= 0 {
		return ctrl.Result{}, false
	}
	log.FromContext(ctx).V(1).Info("The provider refused this VM's create; waiting for the backoff before sending it again",
		"retryAfter", wait.String())
	if !equality.Semantic.DeepEqual(persisted, &vm.Status) {
		r.updateStatus(ctx, vm)
	}
	return ctrl.Result{RequeueAfter: wait}, true
}

// createRefusalRefIndex is the cache field index of a VirtualMachine whose
// create is refused as invalid (createRefusalIndexValues), by every Provider,
// VMClass and VMImage it references (consumerRefIndexValue).
const createRefusalRefIndex = "virtrigaud.io/create-refusal-ref"

// createRefused reports whether vm has no provider VM and its Ready or
// Provisioning condition records a refusal of its create as invalid
// (ValidationError): a provider's InvalidSpec answer, or a VMClass value the
// create request could not be built from.
func createRefused(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	if vm.Status.ID != "" {
		return false
	}
	for _, t := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
		if c := meta.FindStatusCondition(vm.Status.Conditions, t); c != nil && c.Reason == k8s.ReasonValidationError {
			return true
		}
	}
	return false
}

// createRefusalIndexValues is the createRefusalRefIndex extractor: a VM whose
// create is refused is indexed by each Provider, VMClass and VMImage it
// references; any other VM is not indexed, so a change to those objects
// re-drives only refused VMs.
func createRefusalIndexValues(obj client.Object) []string {
	vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
	if !ok || !createRefused(vm) {
		return nil
	}
	var out []string
	for _, ref := range vmConsumerRefs(vm) {
		if ref.set {
			out = append(out, consumerRefIndexValue(ref.kind, ref.key))
		}
	}
	return out
}

// vmsForCreateRefusalChange returns a request for each VirtualMachine whose
// refused create references obj, a Provider, VMClass or VMImage whose spec
// changed.
func (r *VirtualMachineReconciler) vmsForCreateRefusalChange(ctx context.Context, obj client.Object) []reconcile.Request {
	kind, _, ok := consumerSelectorOf(obj)
	if !ok {
		return nil
	}
	list := &infravirtrigaudiov1beta1.VirtualMachineList{}
	value := consumerRefIndexValue(kind, client.ObjectKeyFromObject(obj))
	if err := r.List(ctx, list, client.MatchingFields{createRefusalRefIndex: value}); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list VirtualMachines whose create was refused", "index", value)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

// specChanged is the predicate of the create-refusal watches: it passes an
// update that changes the object's generation (its spec). Creates, deletes
// and status-only updates — frequent on a Provider — never pass.
func specChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// withCreateRefusalWatches adds to b the watches that retry a refused create
// as soon as the spec of its Provider, VMClass or VMImage changes, instead of
// at the end of its backoff (holdRefusedCreate drops the wait because the
// inputs changed).
func (r *VirtualMachineReconciler) withCreateRefusalWatches(b *builder.Builder) *builder.Builder {
	onChange := handler.EnqueueRequestsFromMapFunc(r.vmsForCreateRefusalChange)
	return b.
		Watches(&infravirtrigaudiov1beta1.Provider{}, onChange, builder.WithPredicates(specChanged())).
		Watches(&infravirtrigaudiov1beta1.VMClass{}, onChange, builder.WithPredicates(specChanged())).
		Watches(&infravirtrigaudiov1beta1.VMImage{}, onChange, builder.WithPredicates(specChanged()))
}

// indexCreateRefusals registers createRefusalRefIndex in mgr's cache.
func indexCreateRefusals(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &infravirtrigaudiov1beta1.VirtualMachine{},
		createRefusalRefIndex, createRefusalIndexValues); err != nil {
		return fmt.Errorf("index VirtualMachine by %s: %w", createRefusalRefIndex, err)
	}
	return nil
}
