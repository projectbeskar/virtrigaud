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
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
)

// The Provider in-use finalizer (ADR-0010). A Provider being deleted is held
// while any VirtualMachine (in any namespace), VMSnapshot, VMClone or
// VMMigration still needs it; its runtime is kept up meanwhile so they can
// still be deleted through it. It is released — runtime torn down, finalizer
// removed — once nothing needs it, or at once on force-delete.

// providerInUseRecheckInterval is the backstop re-check of a held Provider.
// The watches on the objects that hold it re-drive it promptly; this only
// covers an event that was missed.
const providerInUseRecheckInterval = 2 * time.Minute

// providerInUseListedNamespaces caps how many namespaces the DeleteBlocked
// message names (the counts are always given in full), so the message stays
// far below the condition-message limit.
const providerInUseListedNamespaces = 10

// providerInUseIndex is the cache field index of VMSnapshot, VMClone and
// VMMigration objects by what can make them hold a Provider: the
// VirtualMachine they act on (providerInUseVMValue) and, for a migration, its
// target Provider (providerInUseProviderValue).
const providerInUseIndex = "virtrigaud.io/provider-in-use"

// The finalizers of the VMSnapshot and VMMigration controllers, read here to
// tell whether such an object still has a delete to run. They must match the
// literals those controllers add and remove.
const (
	vmSnapshotFinalizerName  = "snapshot.infra.virtrigaud.io/finalizer"
	vmMigrationFinalizerName = "vmmigration.infra.virtrigaud.io/finalizer"
)

// DeleteBlocked condition reasons on a Provider being deleted (ADR-0010).
const (
	// reasonProviderInUse: objects still need the Provider; its runtime is
	// kept running so they can be deleted through it.
	reasonProviderInUse = "ProviderInUse"
	// reasonProviderInUseRuntimeNotKept: objects still need the Provider, but
	// its runtime cannot be kept (its namespace is being deleted, or it was
	// deleted with foreground cascading deletion); the objects left are held,
	// not orphaned, until an administrator acts.
	reasonProviderInUseRuntimeNotKept = "ProviderInUseRuntimeNotKept"
)

// Event reasons recorded on a Provider by the in-use finalizer.
const (
	// eventReasonProviderDeleteBlocked is the Warning recorded when a hold
	// begins or its reason changes.
	eventReasonProviderDeleteBlocked = "ProviderDeleteBlocked"
	// eventReasonProviderForceDeleted is the Warning recorded when
	// force-delete releases a Provider that objects still need.
	eventReasonProviderForceDeleted = "ProviderForceDeleted"
)

// The DeleteBlocked messages. Each is formatted with the usage summary (the
// object counts) and the namespace summary (counts per namespace), so it only
// changes when the usage does. The annotation names are filled in from their
// constants.
const (
	providerInUseKeptFormat = "The Provider is being deleted but is still in use by %s, in %s. " +
		"Its runtime is kept running so they can still be deleted through it, and the Provider is released once none remain. " +
		"Objects in other namespaces are not deleted with this Provider or its namespace: delete them, or detach " +
		"VirtualMachines with %s=true. To release the Provider at once, annotate it %s=true; the hypervisor VMs left are then unmanaged."
	providerInUseNamespaceDeletedFormat = "The Provider's namespace is being deleted, and Kubernetes deletes the provider runtime with it, " +
		"so the %s still using it, in %s, can be deleted through it only while its pod is terminating. " +
		"Each one left is held, not orphaned. To finish, annotate the Provider %s=true, or annotate each remaining " +
		"object %s=true; the hypervisor VMs left are then unmanaged."
	providerInUseForegroundFormat = "The Provider was deleted with foreground cascading deletion, which deletes its runtime first, " +
		"so the %s still using it, in %s, may no longer be deletable through it. Each one left is held, not orphaned. " +
		"To finish, annotate the Provider %s=true, or annotate each remaining object %s=true; the hypervisor VMs left are " +
		"then unmanaged. Delete Providers with the default (background) cascade."
	providerForceDeletedFormat = "%s=true: releasing the Provider although %s still use it, in %s. Its runtime is removed. " +
		"VirtualMachines in the Provider's namespace are released on their next delete without a provider call; those in " +
		"other namespaces stay held until annotated %s=true or %s=true. Their hypervisor VMs are left unmanaged."
)

// providerUsage counts the objects that still need a Provider being deleted.
type providerUsage struct {
	vms        int
	snapshots  int
	clones     int
	migrations int
	// namespaces counts the objects per namespace.
	namespaces map[string]int
}

// add counts one object of the kind counter points at, in namespace.
func (u *providerUsage) add(namespace string, counter *int) {
	*counter++
	if u.namespaces == nil {
		u.namespaces = map[string]int{}
	}
	u.namespaces[namespace]++
}

// total is the number of objects that still need the Provider.
func (u providerUsage) total() int {
	return u.vms + u.snapshots + u.clones + u.migrations
}

// objectsSummary renders the object counts, e.g. "2 VirtualMachine(s) and 1
// VMSnapshot(s)". Kinds with no object are left out.
func (u providerUsage) objectsSummary() string {
	var parts []string
	for _, kind := range []struct {
		n    int
		name string
	}{
		{u.vms, "VirtualMachine(s)"},
		{u.snapshots, "VMSnapshot(s)"},
		{u.clones, "VMClone(s)"},
		{u.migrations, "VMMigration(s)"},
	} {
		if kind.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", kind.n, kind.name))
		}
	}
	switch len(parts) {
	case 0:
		return "no objects"
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// namespacesSummary renders the per-namespace counts in name order, e.g.
// "namespace(s) team-a (2), team-b (1)", naming at most
// providerInUseListedNamespaces namespaces. It names no object: the Provider
// belongs to its administrator, but the objects may belong to tenants.
func (u providerUsage) namespacesSummary() string {
	names := make([]string, 0, len(u.namespaces))
	for ns := range u.namespaces {
		names = append(names, ns)
	}
	sort.Strings(names)
	listed := names
	if len(listed) > providerInUseListedNamespaces {
		listed = listed[:providerInUseListedNamespaces]
	}
	parts := make([]string, 0, len(listed))
	for _, ns := range listed {
		parts = append(parts, fmt.Sprintf("%s (%d)", ns, u.namespaces[ns]))
	}
	more := ""
	if n := len(names) - len(listed); n > 0 {
		more = fmt.Sprintf(" and %d more namespace(s)", n)
	}
	return "namespace(s) " + strings.Join(parts, ", ") + more
}

// providerInUseMessage is the DeleteBlocked message for reason (and, for
// reasonProviderInUseRuntimeNotKept, foreground: whether the cause is a
// foreground cascading deletion rather than the namespace's deletion).
func providerInUseMessage(reason string, foreground bool, u providerUsage) string {
	switch {
	case reason == reasonProviderInUse:
		return fmt.Sprintf(providerInUseKeptFormat, u.objectsSummary(), u.namespacesSummary(),
			infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation)
	case foreground:
		return fmt.Sprintf(providerInUseForegroundFormat, u.objectsSummary(), u.namespacesSummary(),
			forceDeleteAnnotation, forceDeleteAnnotation)
	default:
		return fmt.Sprintf(providerInUseNamespaceDeletedFormat, u.objectsSummary(), u.namespacesSummary(),
			forceDeleteAnnotation, forceDeleteAnnotation)
	}
}

// providerForceDeletedMessage is the message of the ProviderForceDeleted
// Warning event.
func providerForceDeletedMessage(u providerUsage) string {
	return fmt.Sprintf(providerForceDeletedFormat, forceDeleteAnnotation, u.objectsSummary(), u.namespacesSummary(),
		infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation)
}

// hasProviderForceDeleteAnnotation reports whether the Provider carries the
// force-delete escape hatch (forceDeleteAnnotation set to "true"): a Provider
// being deleted is then released at once, whatever still uses it.
func hasProviderForceDeleteAnnotation(provider *infravirtrigaudiov1beta1.Provider) bool {
	return provider.Annotations[forceDeleteAnnotation] == "true"
}

// vmHoldsProvider reports whether a VirtualMachine of a Provider still needs
// it: it is not being deleted, or its VirtualMachine finalizer — which deletes
// the hypervisor VM through the Provider — is still there. A VM being deleted
// that only another controller's finalizer keeps does not hold it.
func vmHoldsProvider(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return vm.DeletionTimestamp.IsZero() ||
		controllerutil.ContainsFinalizer(vm, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
}

// snapshotHoldsProvider reports whether a VMSnapshot still has a delete to
// run (its finalizer is there). Its delete calls SnapshotDelete through its
// VM's Provider while that VM exists.
func snapshotHoldsProvider(snapshot *infravirtrigaudiov1beta1.VMSnapshot) bool {
	return controllerutil.ContainsFinalizer(snapshot, vmSnapshotFinalizerName)
}

// cloneHoldsProvider reports whether a VMClone is still cloning through its
// source VM's Provider: it carries its finalizer, is not being deleted (its
// delete makes no provider call) and has not finished.
func cloneHoldsProvider(clone *infravirtrigaudiov1beta1.VMClone) bool {
	if !controllerutil.ContainsFinalizer(clone, vmCloneFinalizer) || !clone.DeletionTimestamp.IsZero() {
		return false
	}
	return clone.Status.Phase != infravirtrigaudiov1beta1.ClonePhaseReady &&
		clone.Status.Phase != infravirtrigaudiov1beta1.ClonePhaseFailed
}

// migrationHoldsProvider reports whether a VMMigration still needs its
// Providers: it carries its finalizer and is in flight, or is being deleted
// (its cleanup can delete the source snapshot through the source Provider).
func migrationHoldsProvider(migration *infravirtrigaudiov1beta1.VMMigration) bool {
	if !controllerutil.ContainsFinalizer(migration, vmMigrationFinalizerName) {
		return false
	}
	if !migration.DeletionTimestamp.IsZero() {
		return true
	}
	return migration.Status.Phase != infravirtrigaudiov1beta1.MigrationPhaseReady &&
		migration.Status.Phase != infravirtrigaudiov1beta1.MigrationPhaseFailed
}

// providerInUseVMValue is the providerInUseIndex value of an object that acts
// on the VirtualMachine key.
func providerInUseVMValue(key types.NamespacedName) string {
	return "vm:" + key.String()
}

// providerInUseProviderValue is the providerInUseIndex value of an object
// that names the Provider key directly.
func providerInUseProviderValue(key types.NamespacedName) string {
	return "provider:" + key.String()
}

// inUseRefs returns, for a VMSnapshot, VMClone or VMMigration, the
// VirtualMachine it acts on (none when it names none) and the Providers it
// names directly. Any other object returns nothing.
func inUseRefs(obj client.Object) (vms []types.NamespacedName, providers []types.NamespacedName) {
	switch o := obj.(type) {
	case *infravirtrigaudiov1beta1.VMSnapshot:
		if o.Spec.VMRef.Name != "" {
			vms = append(vms, types.NamespacedName{Namespace: o.Namespace, Name: o.Spec.VMRef.Name})
		}
	case *infravirtrigaudiov1beta1.VMClone:
		if o.Spec.Source.VMRef != nil && o.Spec.Source.VMRef.Name != "" {
			vms = append(vms, types.NamespacedName{Namespace: o.Namespace, Name: o.Spec.Source.VMRef.Name})
		}
	case *infravirtrigaudiov1beta1.VMMigration:
		if o.Spec.Source.VMRef.Name != "" {
			vms = append(vms, types.NamespacedName{Namespace: o.Namespace, Name: o.Spec.Source.VMRef.Name})
		}
		if o.Spec.Target.ProviderRef.Name != "" {
			providers = append(providers, migrationTargetProviderKey(o))
		}
	}
	return vms, providers
}

// providerInUseIndexValues is the providerInUseIndex extractor for VMSnapshot,
// VMClone and VMMigration (see inUseRefs).
func providerInUseIndexValues(obj client.Object) []string {
	vms, providers := inUseRefs(obj)
	out := make([]string, 0, len(vms)+len(providers))
	for _, key := range vms {
		out = append(out, providerInUseVMValue(key))
	}
	for _, key := range providers {
		out = append(out, providerInUseProviderValue(key))
	}
	return out
}

// providerInUseIndexRegistered records the field indexers (one per manager
// cache) providerInUseIndex is registered in.
var providerInUseIndexRegistered sync.Map

// indexProviderInUse registers providerInUseIndex on VMSnapshot, VMClone and
// VMMigration in mgr's cache once.
func indexProviderInUse(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if _, done := providerInUseIndexRegistered.LoadOrStore(indexer, struct{}{}); done {
		return nil
	}
	for _, obj := range []client.Object{
		&infravirtrigaudiov1beta1.VMSnapshot{},
		&infravirtrigaudiov1beta1.VMClone{},
		&infravirtrigaudiov1beta1.VMMigration{},
	} {
		if err := indexer.IndexField(context.Background(), obj, providerInUseIndex, providerInUseIndexValues); err != nil {
			providerInUseIndexRegistered.Delete(indexer)
			return fmt.Errorf("index %T by %s: %w", obj, providerInUseIndex, err)
		}
	}
	return nil
}

// providerUsageOf counts what still needs the Provider key (ADR-0010 D2),
// through the cache field indexes only: the Provider's VirtualMachines (by
// placement Provider, in every namespace), the VMSnapshots, VMClones and
// VMMigrations that act on one of them, and the VMMigrations that target the
// Provider.
func providerUsageOf(ctx context.Context, reader client.Reader, key types.NamespacedName) (providerUsage, error) {
	var u providerUsage
	vms, err := listProviderVMs(ctx, reader, key)
	if err != nil {
		return u, err
	}
	countedMigrations := map[types.NamespacedName]bool{}
	countMigrations := func(value string) error {
		var migrations infravirtrigaudiov1beta1.VMMigrationList
		if err := reader.List(ctx, &migrations, client.MatchingFields{providerInUseIndex: value}, client.UnsafeDisableDeepCopy); err != nil {
			return fmt.Errorf("list VMMigrations by %s: %w", value, err)
		}
		for i := range migrations.Items {
			m := &migrations.Items[i]
			mk := client.ObjectKeyFromObject(m)
			if migrationHoldsProvider(m) && !countedMigrations[mk] {
				countedMigrations[mk] = true
				u.add(m.Namespace, &u.migrations)
			}
		}
		return nil
	}

	for i := range vms {
		vm := &vms[i]
		if vmHoldsProvider(vm) {
			u.add(vm.Namespace, &u.vms)
		}
		value := providerInUseVMValue(client.ObjectKeyFromObject(vm))

		var snapshots infravirtrigaudiov1beta1.VMSnapshotList
		if err := reader.List(ctx, &snapshots, client.MatchingFields{providerInUseIndex: value}, client.UnsafeDisableDeepCopy); err != nil {
			return u, fmt.Errorf("list VMSnapshots by %s: %w", value, err)
		}
		for j := range snapshots.Items {
			if snapshotHoldsProvider(&snapshots.Items[j]) {
				u.add(snapshots.Items[j].Namespace, &u.snapshots)
			}
		}

		var clones infravirtrigaudiov1beta1.VMCloneList
		if err := reader.List(ctx, &clones, client.MatchingFields{providerInUseIndex: value}, client.UnsafeDisableDeepCopy); err != nil {
			return u, fmt.Errorf("list VMClones by %s: %w", value, err)
		}
		for j := range clones.Items {
			if cloneHoldsProvider(&clones.Items[j]) {
				u.add(clones.Items[j].Namespace, &u.clones)
			}
		}

		if err := countMigrations(value); err != nil {
			return u, err
		}
	}
	if err := countMigrations(providerInUseProviderValue(key)); err != nil {
		return u, err
	}
	return u, nil
}

// handleDeletion handles a Provider being deleted (ADR-0010). While anything
// still needs it, it is held with its runtime kept up (holdInUseProvider).
// Once nothing does, or when force-delete is set, its runtime is torn down
// and the in-use finalizer removed. A Provider without the in-use finalizer
// (deleted before the upgrade, kept by another finalizer) only has its runtime
// torn down, as before.
func (r *ProviderReconciler) handleDeletion(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := client.ObjectKeyFromObject(provider)

	if !k8s.HasFinalizer(provider, infravirtrigaudiov1beta1.ProviderInUseFinalizer) {
		if err := r.cleanupRemoteRuntime(ctx, provider); err != nil {
			metrics.RecordError(errReasonCleanupFailed, metrics.ComponentManager)
			return ctrl.Result{}, fmt.Errorf("clean up the runtime of Provider %s: %w", key, err)
		}
		return ctrl.Result{}, nil
	}

	usage, err := providerUsageOf(ctx, r.Client, key)
	if err != nil {
		metrics.RecordError(errReasonProviderInUse, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("check what still uses Provider %s: %w", key, err)
	}
	forced := hasProviderForceDeleteAnnotation(provider)
	if usage.total() > 0 && !forced {
		return r.holdInUseProvider(ctx, provider, usage)
	}

	if err := r.cleanupRemoteRuntime(ctx, provider); err != nil {
		metrics.RecordError(errReasonCleanupFailed, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("clean up the runtime of Provider %s: %w", key, err)
	}
	if err := k8s.RemoveFinalizer(ctx, r.Client, provider, infravirtrigaudiov1beta1.ProviderInUseFinalizer); err != nil {
		metrics.RecordError(errReasonProviderFinalizer, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("remove in-use finalizer from Provider %s: %w", key, err)
	}
	if usage.total() > 0 {
		msg := providerForceDeletedMessage(usage)
		logger.Info("force-delete: released the Provider although objects still use it; their hypervisor VMs are left unmanaged",
			"provider", key.String(), "annotation", forceDeleteAnnotation,
			"virtualMachines", usage.vms, "snapshots", usage.snapshots, "clones", usage.clones,
			"migrations", usage.migrations, "namespaces", len(usage.namespaces))
		r.recordProviderEvent(provider, corev1.EventTypeWarning, eventReasonProviderForceDeleted, msg)
		return ctrl.Result{}, nil
	}
	logger.Info("Provider released: nothing uses it any more", "provider", key.String())
	return ctrl.Result{}, nil
}

// holdInUseProvider keeps a Provider being deleted while usage still needs it
// (ADR-0010 D3). Its runtime is reconciled as for a live Provider, so its
// phase stays Running and the objects can still be deleted through it —
// unless the runtime cannot be kept (providerRuntimeKeepable), in which case
// nothing is written to it and status.runtime is left as it is. It records
// DeleteBlocked=True and, when the hold begins or its reason changes, a
// Warning event. A hold that has not changed writes nothing.
func (r *ProviderReconciler) holdInUseProvider(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, usage providerUsage) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	base := provider.DeepCopy()

	keepable, foreground, err := r.providerRuntimeKeepable(ctx, provider)
	if err != nil {
		metrics.RecordError(errReasonProviderInUse, metrics.ComponentManager)
		return ctrl.Result{}, err
	}

	reason := reasonProviderInUseRuntimeNotKept
	result := ctrl.Result{}
	var runtimeErr error
	if keepable {
		reason = reasonProviderInUse
		result, runtimeErr = r.reconcileRuntimeStatus(ctx, provider)
	}

	msg := providerInUseMessage(reason, foreground, usage)
	prev := meta.FindStatusCondition(base.Status.Conditions, k8s.ConditionDeleteBlocked)
	meta.SetStatusCondition(&provider.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionDeleteBlocked,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: provider.Generation,
	})
	provider.Status.ObservedGeneration = provider.Generation

	if !equality.Semantic.DeepEqual(base.Status, provider.Status) {
		if err := r.writeProviderStatus(ctx, provider); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status of held Provider %s/%s: %w", provider.Namespace, provider.Name, err)
		}
	}
	if prev == nil || prev.Status != metav1.ConditionTrue || prev.Reason != reason {
		logger.Info("Provider deletion blocked: still in use", "provider", provider.Name, "reason", reason,
			"virtualMachines", usage.vms, "snapshots", usage.snapshots, "clones", usage.clones,
			"migrations", usage.migrations, "namespaces", len(usage.namespaces))
		r.recordProviderEvent(provider, corev1.EventTypeWarning, eventReasonProviderDeleteBlocked, msg)
	}

	if runtimeErr != nil {
		return ctrl.Result{}, runtimeErr
	}
	requeue := providerInUseRecheckInterval
	if result.RequeueAfter > 0 && result.RequeueAfter < requeue {
		requeue = result.RequeueAfter
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// providerRuntimeKeepable reports whether the runtime of a held Provider can
// be kept (ADR-0010 D6). It cannot when the Provider was deleted with
// foreground cascading deletion (foreground=true: the garbage collector
// deletes the runtime first, and re-creating it would fight the collector), or
// when its namespace is being deleted (the namespace controller deletes the
// runtime's pods directly, and nothing can be created there any more).
func (r *ProviderReconciler) providerRuntimeKeepable(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (keepable, foreground bool, err error) {
	if controllerutil.ContainsFinalizer(provider, metav1.FinalizerDeleteDependents) {
		return false, true, nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: provider.Namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return true, false, nil
		}
		return false, false, fmt.Errorf("get Namespace %s of Provider %s: %w", provider.Namespace, provider.Name, err)
	}
	if !ns.DeletionTimestamp.IsZero() || ns.Status.Phase == corev1.NamespaceTerminating {
		return false, false, nil
	}
	return true, false, nil
}

// recordProviderEvent records an event on the Provider; a nil Recorder (unit
// tests) records nothing.
func (r *ProviderReconciler) recordProviderEvent(provider *infravirtrigaudiov1beta1.Provider, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(provider, eventType, reason, msg)
	}
}

// terminatingProvider returns a request for the Provider key when it is being
// deleted, the only state in which what uses it matters.
func (r *ProviderReconciler) terminatingProvider(ctx context.Context, key types.NamespacedName) (reconcile.Request, bool) {
	provider := &infravirtrigaudiov1beta1.Provider{}
	if err := r.Get(ctx, key, provider); err != nil || provider.DeletionTimestamp.IsZero() {
		return reconcile.Request{}, false
	}
	return reconcile.Request{NamespacedName: key}, true
}

// providersForInUseVM maps a VirtualMachine event to its placement Provider,
// when that Provider is being deleted (ADR-0010 D4).
func (r *ProviderReconciler) providersForInUseVM(ctx context.Context, obj client.Object) []reconcile.Request {
	vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
	if !ok {
		return nil
	}
	if req, ok := r.terminatingProvider(ctx, placementProviderKey(vm)); ok {
		return []reconcile.Request{req}
	}
	return nil
}

// providersForInUseDependent maps a VMSnapshot, VMClone or VMMigration event
// to the Providers it can hold — its VM's placement Provider, and the
// Providers it names — that are being deleted (ADR-0010 D4).
func (r *ProviderReconciler) providersForInUseDependent(ctx context.Context, obj client.Object) []reconcile.Request {
	vms, providers := inUseRefs(obj)
	for _, vmKey := range vms {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		if err := r.Get(ctx, vmKey, vm); err == nil {
			providers = append(providers, placementProviderKey(vm))
		}
	}
	seen := map[types.NamespacedName]bool{}
	var reqs []reconcile.Request
	for _, key := range providers {
		if seen[key] {
			continue
		}
		seen[key] = true
		if req, ok := r.terminatingProvider(ctx, key); ok {
			reqs = append(reqs, req)
		}
	}
	return reqs
}

// providerUseSignature is what decides whether obj holds a Provider, and
// which one: an update that leaves it unchanged cannot release or newly hold
// any Provider, so it re-drives nothing.
func providerUseSignature(obj client.Object) string {
	switch o := obj.(type) {
	case *infravirtrigaudiov1beta1.VirtualMachine:
		return fmt.Sprintf("%s|%t", placementProviderKey(o), vmHoldsProvider(o))
	case *infravirtrigaudiov1beta1.VMSnapshot:
		return fmt.Sprintf("%v|%t", providerInUseIndexValues(o), snapshotHoldsProvider(o))
	case *infravirtrigaudiov1beta1.VMClone:
		return fmt.Sprintf("%v|%t", providerInUseIndexValues(o), cloneHoldsProvider(o))
	case *infravirtrigaudiov1beta1.VMMigration:
		return fmt.Sprintf("%v|%t", providerInUseIndexValues(o), migrationHoldsProvider(o))
	default:
		return ""
	}
}

// providerUseChanged is the predicate of the in-use watches: creates and
// deletes pass, and an update passes only when it changes providerUseSignature
// (a status or metadata write that does not change what the object holds is
// dropped, so a busy VM does not re-drive its Provider).
func providerUseChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return providerUseSignature(e.ObjectOld) != providerUseSignature(e.ObjectNew)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
