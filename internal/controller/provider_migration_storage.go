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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// Migration storage (the pvc backend) is mounted into a provider pod — the pod
// that holds the hypervisor credentials — only for what a live VMMigration
// references. A PVC is mounted into Provider P when ALL of these hold:
//
//   - a VMMigration in P's namespace (a pod mounts PVCs from its own namespace
//     only) uses the pvc backend, is not being deleted and is in flight: not
//     Ready and not terminally Failed (migrationStorageInFlight);
//   - P is that migration's source Provider (the source VM's own Provider,
//     migrationSourceProviderRef) or its target Provider;
//   - the PVC is the one the migration controller recorded in
//     status.storagePVCName, and migrationPVCReferenceError accepts it: it
//     carries the migration-storage label AND is either the PVC the migration
//     created (the UID it recorded in status.storagePVCUID from its own create;
//     an ownerReference alone can be forged) or the PVC spec.storage.pvc.name
//     names, not controlled by another VMMigration;
//   - the PVC is not being deleted and is Bound — or, only while the migration
//     waits for the mount (phase Validating), Pending on a WaitForFirstConsumer
//     StorageClass, which binds only once a pod uses it;
//   - for a Failed migration that will be retried: the mount had worked, i.e.
//     a Ready pod of P carries the PVC. Otherwise (a Bound PVC P's pod cannot
//     use, e.g. a ReadWriteOnce or node-pinned volume) it is dropped, so it
//     cannot keep P down between retries.
//
// The source mounts the PVC read-write (it exports into it); a Provider that is
// only the target mounts it read-only (it only reads the staged disk).

const (
	// migrationPVCLabelKey and migrationPVCLabelValue mark a PVC as usable as
	// migration storage. The VMMigration controller sets the label on the PVCs
	// it creates; the owner of a PVC named in spec.storage.pvc.name sets it to
	// consent to that use. It is necessary but NOT sufficient for a mount: the
	// PVC must also be referenced by a live VMMigration (see above).
	migrationPVCLabelKey   = "virtrigaud.io/component"
	migrationPVCLabelValue = "migration-storage"

	// migrationStorageMountRoot is where provider pods find migration PVCs:
	// each at <root>/<pvc-name>, which is what the providers derive from a
	// pvc://<pvc-name>/<path> URL.
	migrationStorageMountRoot = "/mnt/migration-storage"

	// migrationStorageVolumePrefix prefixes the pod volume name of a migration
	// PVC.
	migrationStorageVolumePrefix = "migration-"

	// vmMigrationKind is the Kind of a VMMigration in an ownerReference.
	vmMigrationKind = "VMMigration"

	// eventReasonMigrationPVCRetained is the Normal event a deleted
	// VMMigration records when its migration PVC is not one it created (a
	// PVC named in spec.storage.pvc.name): VirtRigaud never deletes it.
	eventReasonMigrationPVCRetained = "MigrationPVCRetained"
	// migrationPVCRetainedMessage is that event's message, formatted with
	// the PVC name.
	migrationPVCRetainedMessage = "Migration PVC %s was not created by this VMMigration; " +
		"it is no longer mounted into the provider pods and is left for its owner to delete"

	// migrationPVCNameSuffix makes the name of the PVC a migration creates:
	// <migration-name>-storage.
	migrationPVCNameSuffix = "-storage"

	// providerPodNameLabel / providerPodInstanceLabel select a Provider's
	// runtime pods (their value of providerPodInstanceLabel is the Provider's
	// name).
	providerPodNameLabel     = "app.kubernetes.io/name"
	providerPodNameValue     = "virtrigaud-provider"
	providerPodInstanceLabel = "app.kubernetes.io/instance"

	// migrationStorageVolumeHashLen is the number of hex digits of the PVC
	// name's SHA-256 in a volume name that cannot carry the name itself.
	migrationStorageVolumeHashLen = 16

	// errReasonMigrationStorageDiscovery is the metrics reason of a failed
	// migration-storage discovery (the Deployment is left as it is).
	errReasonMigrationStorageDiscovery = "migration-storage-discovery-failed"
	// reasonMigrationStorageDiscoveryFailed is the ProviderRuntimeReady
	// reason when a Deployment cannot be created because migration storage
	// could not be read.
	reasonMigrationStorageDiscoveryFailed = "MigrationStorageDiscoveryFailed"
	// migrationStorageDiscoveryFailedMessage is that condition's message.
	migrationStorageDiscoveryFailedMessage = "The migration storage to mount into the provider pods could not be read; retrying"
	// migrationStorageDiscoveryRetry is the requeue delay after such a failure.
	migrationStorageDiscoveryRetry = 30 * time.Second
)

// Reasons migrationPVCReferenceError refuses a PVC. The VMMigration controller
// reports them when it validates the migration's storage.
var (
	// errMigrationPVCNotLabelled: the PVC lacks the migration-storage label.
	errMigrationPVCNotLabelled = stderrors.New("the PVC is not labelled " +
		migrationPVCLabelKey + "=" + migrationPVCLabelValue + "; its owner must label it to allow its use as migration storage")
	// errMigrationPVCNotOwned: an auto-created migration PVC name is taken by
	// a PVC this migration did not create.
	errMigrationPVCNotOwned = stderrors.New("the PVC was not created by this VMMigration; " +
		"set spec.storage.pvc.name to use an existing PVC")
	// errMigrationPVCOtherMigration: the named PVC belongs to another
	// VMMigration.
	errMigrationPVCOtherMigration = stderrors.New("the PVC belongs to another VMMigration")
	// errMigrationPVCNameMismatch: the PVC is not the one the spec names.
	errMigrationPVCNameMismatch = stderrors.New("the PVC is not the one spec.storage.pvc.name names")
	// errMigrationPVCSpecChanged: spec.storage.pvc no longer names the PVC the
	// migration recorded.
	errMigrationPVCSpecChanged = stderrors.New("spec.storage.pvc changed after the migration recorded its PVC; " +
		"re-create the VMMigration to use another PVC")
	// errMigrationStorageDiscovery wraps a read error of discoverMigrationStorage.
	errMigrationStorageDiscovery = stderrors.New("discover migration storage")
)

// migrationStorageMount is one migration PVC mounted into a provider pod.
type migrationStorageMount struct {
	// pvcName is the PVC's name (in the Provider's namespace).
	pvcName string
	// readOnly is true when the Provider only reads the PVC (it is the target,
	// never the source, of every migration that references it).
	readOnly bool
	// volumeName is the pod volume name (assignMigrationVolumeNames).
	volumeName string
}

// migrationAutoPVCName is the name of the PVC migration m creates when
// spec.storage.pvc.name is empty.
func migrationAutoPVCName(m *infravirtrigaudiov1beta1.VMMigration) string {
	return m.Name + migrationPVCNameSuffix
}

// migrationCreatedPVC reports whether pvc is the PVC m created: the one whose
// UID m recorded in status.storagePVCUID from the API server's answer to its
// own create. Anyone who can create PVCs can also create one under m's PVC
// name carrying an ownerReference to m (its name and UID are readable), so
// the ownerReference alone proves nothing. A migration whose PVC an older
// manager created (status.storagePVCName set, no UID recorded — only a
// manager writes status) falls back to the controller ownerReference with
// m's UID, as before.
func migrationCreatedPVC(pvc *corev1.PersistentVolumeClaim, m *infravirtrigaudiov1beta1.VMMigration) bool {
	if m.Status.StoragePVCUID != "" {
		return string(pvc.UID) == m.Status.StoragePVCUID
	}
	return m.Status.StoragePVCName != "" && pvc.Name == m.Status.StoragePVCName &&
		pvc.Name == migrationAutoPVCName(m) && metav1.IsControlledBy(pvc, m)
}

// migrationPVCSpecChangedError returns errMigrationPVCSpecChanged when m has
// recorded a PVC that its spec no longer names (spec.storage.pvc.name set,
// changed or removed after the record), nil otherwise.
func migrationPVCSpecChangedError(m *infravirtrigaudiov1beta1.VMMigration) error {
	if m.Status.StoragePVCName == "" {
		return nil
	}
	want := migrationNamedPVC(m)
	if want == "" {
		want = migrationAutoPVCName(m)
	}
	if m.Status.StoragePVCName != want {
		return errMigrationPVCSpecChanged
	}
	return nil
}

// migrationPVCReferenceError reports why migration m may not use pvc as its
// migration storage, or nil when it may: pvc carries the migration-storage
// label and is either the PVC spec.storage.pvc.name names (and not controlled
// by another VMMigration) or, with no name in the spec, the PVC m created
// (migrationCreatedPVC).
func migrationPVCReferenceError(pvc *corev1.PersistentVolumeClaim, m *infravirtrigaudiov1beta1.VMMigration) error {
	if pvc.Labels[migrationPVCLabelKey] != migrationPVCLabelValue {
		return errMigrationPVCNotLabelled
	}
	if named := migrationNamedPVC(m); named != "" {
		if pvc.Name != named {
			return errMigrationPVCNameMismatch
		}
		if owner := metav1.GetControllerOf(pvc); owner != nil && owner.Kind == vmMigrationKind && owner.UID != m.UID {
			return errMigrationPVCOtherMigration
		}
		return nil
	}
	if !migrationCreatedPVC(pvc, m) {
		return errMigrationPVCNotOwned
	}
	return nil
}

// migrationNamedPVC is the existing PVC spec.storage.pvc.name names, or "".
func migrationNamedPVC(m *infravirtrigaudiov1beta1.VMMigration) string {
	if m.Spec.Storage == nil || m.Spec.Storage.PVC == nil {
		return ""
	}
	return m.Spec.Storage.PVC.Name
}

// migrationStorageInFlight reports whether m may still need its migration PVC
// mounted: a pvc-backend migration that is not being deleted, has recorded its
// PVC, and is neither Ready nor Failed with no retry left. A Failed migration
// that will be retried keeps its mount, so the retry does not roll the
// provider pods again.
func migrationStorageInFlight(m *infravirtrigaudiov1beta1.VMMigration) bool {
	if !m.DeletionTimestamp.IsZero() || m.Status.StoragePVCName == "" {
		return false
	}
	if migrationBackendType(m) != storagemigration.BackendPVC {
		return false
	}
	if m.Status.Phase == infravirtrigaudiov1beta1.MigrationPhaseReady {
		return false
	}
	return !migrationTerminallyFailed(m)
}

// migrationStorageProviders returns the Providers in m's namespace that use
// m's migration PVC: its target Provider and, when its source VM can be
// resolved, the source VM's own Provider. A Provider in another namespace can
// never mount the PVC and is left out. source is nil when the source Provider
// is not in m's namespace or cannot be resolved (the source VM is gone, or
// its references disagree — the migration fails on that anyway).
func migrationStorageProviders(ctx context.Context, reader client.Reader, m *infravirtrigaudiov1beta1.VMMigration) (source *types.NamespacedName, target *types.NamespacedName, err error) {
	if m.Spec.Target.ProviderRef.Name != "" {
		if key := migrationTargetProviderKey(m); key.Namespace == m.Namespace {
			target = &key
		}
	}
	if m.Spec.Source.VMRef.Name == "" {
		return nil, target, nil
	}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Spec.Source.VMRef.Name}, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, target, nil
		}
		return nil, nil, fmt.Errorf("get source VirtualMachine %s/%s: %w", m.Namespace, m.Spec.Source.VMRef.Name, err)
	}
	ref, refErr := migrationSourceProviderRef(m, vm)
	if refErr != nil || ref.Namespace != m.Namespace {
		return nil, target, nil
	}
	key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	return &key, target, nil
}

// discoverMigrationStorage returns the migration PVCs to mount into provider's
// pod, sorted by PVC name so an unchanged set renders an unchanged pod
// template (no rollout). See the comment at the top of this file for the
// rules. A read error is returned, wrapping errMigrationStorageDiscovery: the
// caller must leave the Deployment as it is rather than unmount the storage of
// migrations in flight.
func (r *ProviderReconciler) discoverMigrationStorage(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) ([]migrationStorageMount, error) {
	mounts, err := r.collectMigrationStorage(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errMigrationStorageDiscovery, err)
	}
	return assignMigrationVolumeNames(ctx, mounts), nil
}

// collectMigrationStorage is discoverMigrationStorage without the volume names.
func (r *ProviderReconciler) collectMigrationStorage(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) ([]migrationStorageMount, error) {
	migrations := &infravirtrigaudiov1beta1.VMMigrationList{}
	if err := r.List(ctx, migrations, client.InNamespace(provider.Namespace)); err != nil {
		return nil, fmt.Errorf("list VMMigrations in %s: %w", provider.Namespace, err)
	}
	self := types.NamespacedName{Namespace: provider.Namespace, Name: provider.Name}

	readOnly := map[string]bool{}
	for i := range migrations.Items {
		m := &migrations.Items[i]
		if !migrationStorageInFlight(m) {
			continue
		}
		source, target, err := migrationStorageProviders(ctx, r.Client, m)
		if err != nil {
			return nil, err
		}
		isSource := source != nil && *source == self
		isTarget := target != nil && *target == self
		if !isSource && !isTarget {
			continue
		}
		mountable, err := r.migrationPVCMountable(ctx, m)
		if err != nil {
			return nil, err
		}
		if !mountable {
			continue
		}
		name := m.Status.StoragePVCName
		if m.Status.Phase == infravirtrigaudiov1beta1.MigrationPhaseFailed {
			// Between retries keep only a mount that worked, so a Bound PVC
			// the pod cannot use does not hold the provider down.
			held, err := r.providerPodCarriesPVC(ctx, provider, name)
			if err != nil {
				return nil, err
			}
			if !held {
				continue
			}
		}
		// Read-write when any migration exports into it through this Provider.
		ro, seen := readOnly[name]
		readOnly[name] = (!seen || ro) && !isSource
	}

	mounts := make([]migrationStorageMount, 0, len(readOnly))
	for name, ro := range readOnly {
		mounts = append(mounts, migrationStorageMount{pvcName: name, readOnly: ro})
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].pvcName < mounts[j].pvcName })
	return mounts, nil
}

// migrationStorageDiscoveryFailed handles a read error of
// discoverMigrationStorage: the Deployment was left as it is. When it exists
// the Provider's runtime status is not downgraded (the runtime is fine; only
// the mount set could not be recomputed); when it does not, ProviderRuntimeReady
// is False with reason MigrationStorageDiscoveryFailed and a constant message.
// Either way the render is retried after migrationStorageDiscoveryRetry.
func (r *ProviderReconciler) migrationStorageDiscoveryFailed(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider,
	deploymentName string, err error) (ctrl.Result, error) {
	log.FromContext(ctx).Error(err, "Failed to read migration storage; leaving the provider Deployment as it is")
	metrics.RecordError(errReasonMigrationStorageDiscovery, metrics.ComponentManager)
	getErr := r.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: deploymentName}, &appsv1.Deployment{})
	if apierrors.IsNotFound(getErr) {
		k8s.SetCondition(&provider.Status.Conditions, "ProviderRuntimeReady", metav1.ConditionFalse,
			reasonMigrationStorageDiscoveryFailed, migrationStorageDiscoveryFailedMessage, provider.Generation)
	}
	return ctrl.Result{RequeueAfter: migrationStorageDiscoveryRetry}, nil
}

// providerPodCarriesPVC reports whether a Running, Ready, non-terminating pod
// of provider has the PVC pvcName as a volume: the mount worked.
func (r *ProviderReconciler) providerPodCarriesPVC(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, pvcName string) (bool, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(provider.Namespace), client.MatchingLabels{
		providerPodNameLabel:     providerPodNameValue,
		providerPodInstanceLabel: provider.Name,
	}); err != nil {
		return false, fmt.Errorf("list pods of Provider %s/%s: %w", provider.Namespace, provider.Name, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp.IsZero() && pod.Status.Phase == corev1.PodRunning &&
			podConditionTrue(pod, corev1.PodReady) && volumesContainPVC(pod.Spec.Volumes, pvcName) {
			return true, nil
		}
	}
	return false, nil
}

// assignMigrationVolumeNames gives each mount (sorted by PVC name) its pod
// volume name: migrationStorageVolumeName, or, when another mount already
// has that name (a hashed name can equal another PVC's literal one), the
// "migration-" prefix and a longer hash of the PVC name alone. A mount whose
// name still clashes is left out (and logged), so the rendered Deployment is
// never invalid.
func assignMigrationVolumeNames(ctx context.Context, mounts []migrationStorageMount) []migrationStorageMount {
	used := map[string]bool{}
	out := make([]migrationStorageMount, 0, len(mounts))
	for _, m := range mounts {
		name := migrationStorageVolumeName(m.pvcName)
		if used[name] {
			sum := sha256.Sum256([]byte(m.pvcName))
			name = migrationStorageVolumePrefix + hex.EncodeToString(sum[:])[:validation.DNS1123LabelMaxLength-len(migrationStorageVolumePrefix)]
		}
		if used[name] {
			log.FromContext(ctx).Info("Not mounting a migration PVC whose volume name clashes with another's", "pvc", m.pvcName)
			continue
		}
		used[name] = true
		m.volumeName = name
		out = append(out, m)
	}
	return out
}

// migrationPVCMountable reports whether m's recorded migration PVC may be
// mounted now: it exists in m's namespace, migrationPVCReferenceError accepts
// it, it is not being deleted (mounting a Terminating PVC makes the new pod
// unschedulable and keeps the PVC from ever being deleted, issue #184), and
// it is Bound — or Pending on a WaitForFirstConsumer StorageClass while m
// waits for the mount (pendingPVCMountable). An unbound PVC is otherwise
// never mounted: a pod that cannot schedule would wedge the provider rollout,
// and with the Recreate strategy take the provider down.
func (r *ProviderReconciler) migrationPVCMountable(ctx context.Context, m *infravirtrigaudiov1beta1.VMMigration) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Status.StoragePVCName}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get migration PVC %s/%s: %w", m.Namespace, m.Status.StoragePVCName, err)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return false, nil
	}
	refErr := migrationPVCSpecChangedError(m)
	if refErr == nil {
		refErr = migrationPVCReferenceError(pvc, m)
	}
	if refErr != nil {
		log.FromContext(ctx).V(1).Info("Not mounting a migration PVC the VMMigration may not use",
			"vmmigration", m.Name, "pvc", pvc.Name, "reason", refErr.Error())
		return false, nil
	}
	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		return true, nil
	case corev1.ClaimPending:
		return r.pendingPVCMountable(ctx, pvc, m), nil
	default:
		return false, nil
	}
}

// pendingPVCMountable reports whether a Pending migration PVC may be mounted:
// only while m waits for its storage mount (phase Validating, bounded by the
// migration's mount deadline) and only when the PVC's StorageClass binds on
// WaitForFirstConsumer, so the pod that mounts it is what binds it. A PVC
// with Immediate binding (or no StorageClass) binds on its own and is mounted
// once Bound. When the StorageClass cannot be read (no permission, e.g. a
// namespace-scoped install) it is mounted during Validating as before: the
// mount deadline still bounds the wait.
func (r *ProviderReconciler) pendingPVCMountable(ctx context.Context, pvc *corev1.PersistentVolumeClaim, m *infravirtrigaudiov1beta1.VMMigration) bool {
	if m.Status.Phase != infravirtrigaudiov1beta1.MigrationPhaseValidating {
		return false
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return false
	}
	sc := &storagev1.StorageClass{}
	if err := r.liveReader().Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return false
		}
		log.FromContext(ctx).V(1).Info("Cannot read the StorageClass of a pending migration PVC; mounting it while the migration waits for it",
			"pvc", pvc.Name, "storageClass", *pvc.Spec.StorageClassName, "error", err.Error())
		return true
	}
	return sc.VolumeBindingMode != nil && *sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer
}

// migrationStorageVolumeName is the pod volume name of a migration PVC:
// "migration-<pvc-name>" when that is a valid DNS-1123 label (the name used
// before, so an upgrade does not rename the volumes of a running pod), and
// otherwise "migration-" plus a prefix of the name and a hash of it — a PVC
// name may be up to 253 characters and contain dots, a volume name may not.
func migrationStorageVolumeName(pvcName string) string {
	name := migrationStorageVolumePrefix + pvcName
	if len(validation.IsDNS1123Label(name)) == 0 {
		return name
	}
	sum := sha256.Sum256([]byte(pvcName))
	suffix := hex.EncodeToString(sum[:])[:migrationStorageVolumeHashLen]
	stem := make([]byte, 0, len(pvcName))
	for i := 0; i < len(pvcName) && len(stem) < validation.DNS1123LabelMaxLength-len(migrationStorageVolumePrefix)-len(suffix)-1; i++ {
		c := pvcName[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			stem = append(stem, c)
		} else {
			stem = append(stem, '-')
		}
	}
	return migrationStorageVolumePrefix + string(stem) + "-" + suffix
}

// migrationStorageVolumes renders the pod volumes of mounts.
func migrationStorageVolumes(mounts []migrationStorageMount) []corev1.Volume {
	volumes := make([]corev1.Volume, 0, len(mounts))
	for _, m := range mounts {
		volumes = append(volumes, corev1.Volume{
			Name: m.volumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.pvcName},
			},
		})
	}
	return volumes
}

// migrationStorageVolumeMounts renders the container mounts of mounts, each
// at <migrationStorageMountRoot>/<pvc-name>.
func migrationStorageVolumeMounts(mounts []migrationStorageMount) []corev1.VolumeMount {
	out := make([]corev1.VolumeMount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, corev1.VolumeMount{
			Name:      m.volumeName,
			MountPath: migrationStorageMountRoot + "/" + m.pvcName,
			ReadOnly:  m.readOnly,
		})
	}
	return out
}

// providersForMigration maps a VMMigration event to the Providers whose
// Deployments may have to mount or unmount its migration PVC: while it is in
// flight, its source and target Providers in its own namespace. When it is
// deleted (gone from the cache, which is updated before handlers run), no
// longer in flight, or its source Provider cannot be resolved (the source VM
// is gone), every Provider in its namespace is re-rendered instead: a Provider
// that mounted the PVC earlier can no longer be found through the source VM,
// and must not keep a stale read-write mount (rendering is idempotent).
func (r *ProviderReconciler) providersForMigration(ctx context.Context, obj client.Object) []reconcile.Request {
	m, ok := obj.(*infravirtrigaudiov1beta1.VMMigration)
	if !ok {
		return nil
	}
	if !migrationStorageInFlight(m) || apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(m), &infravirtrigaudiov1beta1.VMMigration{})) {
		return r.providersInNamespace(ctx, m.Namespace)
	}
	source, target, err := migrationStorageProviders(ctx, r.Client, m)
	if err != nil || source == nil {
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to resolve the Providers of a VMMigration's migration storage", "vmmigration", m.Name)
		}
		return r.providersInNamespace(ctx, m.Namespace)
	}
	var reqs []reconcile.Request
	for _, key := range []*types.NamespacedName{source, target} {
		if key == nil {
			continue
		}
		if len(reqs) == 1 && reqs[0].NamespacedName == *key {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: *key})
	}
	return reqs
}

// providersForMigrationPVC maps a PVC event to every Provider in its
// namespace when the PVC carries the migration-storage label or a VMMigration
// there records it, so a PVC that becomes Bound is mounted and one that
// starts deleting is unmounted promptly (issue #184) — also by a Provider the
// PVC can no longer be traced to (its migration or source VM is gone).
// Rendering is idempotent, so the other Providers do not roll. Any other PVC
// maps to nothing.
func (r *ProviderReconciler) providersForMigrationPVC(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[migrationPVCLabelKey] != migrationPVCLabelValue {
		migrations := &infravirtrigaudiov1beta1.VMMigrationList{}
		if err := r.List(ctx, migrations, client.InNamespace(obj.GetNamespace())); err != nil {
			log.FromContext(ctx).Error(err, "failed to list VMMigrations for a PVC event", "namespace", obj.GetNamespace())
			return nil
		}
		referenced := false
		for i := range migrations.Items {
			if migrations.Items[i].Status.StoragePVCName == obj.GetName() {
				referenced = true
				break
			}
		}
		if !referenced {
			return nil
		}
	}
	return r.providersInNamespace(ctx, obj.GetNamespace())
}

// providersInNamespace returns a reconcile request for every Provider in
// namespace.
func (r *ProviderReconciler) providersInNamespace(ctx context.Context, namespace string) []reconcile.Request {
	providers := &infravirtrigaudiov1beta1.ProviderList{}
	if err := r.List(ctx, providers, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Providers to re-render their migration storage", "namespace", namespace)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(providers.Items))
	for i := range providers.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&providers.Items[i])})
	}
	return reqs
}

// migrationStorageSignature is what decides a VMMigration's migration-storage
// mounts: an update that leaves it unchanged cannot change any mount.
func migrationStorageSignature(m *infravirtrigaudiov1beta1.VMMigration) string {
	var sourceRef string
	if m.Spec.Source.ProviderRef != nil {
		sourceRef = m.Spec.Source.ProviderRef.Namespace + "/" + m.Spec.Source.ProviderRef.Name
	}
	return fmt.Sprintf("%s|%s|%t|%t|%t|%s|%s|%s|%s", m.Status.StoragePVCName, m.Status.StoragePVCUID, migrationStorageInFlight(m),
		m.Status.Phase == infravirtrigaudiov1beta1.MigrationPhaseValidating,
		m.Status.Phase == infravirtrigaudiov1beta1.MigrationPhaseFailed, migrationNamedPVC(m),
		m.Spec.Source.VMRef.Name, sourceRef, migrationTargetProviderKey(m))
}

// migrationStorageChanged is the predicate of the VMMigration watch that
// drives migration-storage mounts: a migration that has recorded a PVC passes
// on create and delete, and an update passes only when it changes
// migrationStorageSignature (progress and status writes are dropped).
func migrationStorageChanged() predicate.Predicate {
	hasPVC := func(obj client.Object) bool {
		m, ok := obj.(*infravirtrigaudiov1beta1.VMMigration)
		return ok && m.Status.StoragePVCName != ""
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return hasPVC(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldM, okOld := e.ObjectOld.(*infravirtrigaudiov1beta1.VMMigration)
			newM, okNew := e.ObjectNew.(*infravirtrigaudiov1beta1.VMMigration)
			return okOld && okNew && migrationStorageSignature(oldM) != migrationStorageSignature(newM)
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return hasPVC(e.Object) },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// migrationPVCSignature is what the mount decision reads from a PVC.
func migrationPVCSignature(obj client.Object) string {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return ""
	}
	var owner types.UID
	if ref := metav1.GetControllerOf(pvc); ref != nil {
		owner = ref.UID
	}
	return fmt.Sprintf("%s|%t|%s|%s", pvc.Status.Phase, pvc.DeletionTimestamp.IsZero(), pvc.Labels[migrationPVCLabelKey], owner)
}

// migrationPVCChanged is the predicate of the PVC watch: creates and deletes
// pass, an update only when it changes migrationPVCSignature.
func migrationPVCChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return migrationPVCSignature(e.ObjectOld) != migrationPVCSignature(e.ObjectNew)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
