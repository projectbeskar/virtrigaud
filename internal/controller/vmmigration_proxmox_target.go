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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// ReasonProxmoxCrossNamespaceTarget is the VMMigration Validating condition
// reason when the target Provider is a Proxmox Provider and the target VM is
// in another namespace than the migration. The migration fails at Validating
// (or, for one already past it, before its import), before any export, import
// or other side effect.
const ReasonProxmoxCrossNamespaceTarget = "ProxmoxCrossNamespaceTarget"

// proxmoxCrossNamespaceTargetMessage is the constant message of that refusal.
const proxmoxCrossNamespaceTargetMessage = "a migration into a Proxmox provider must create its target VM in the " +
	"VMMigration's own namespace: the Proxmox provider imports a migrated disk only for a VM whose own-namespace " +
	"VMMigration verifies it, so the target VM's create would be refused. Create the VMMigration in the target " +
	"namespace, or leave spec.target.namespace unset"

// proxmoxCrossNamespaceTargetRefused reports whether migration must not run
// because its target Provider is a Proxmox Provider and its target VM
// (migrationTargetVMKey) is in another namespace than the migration. The
// target VM's Create verifies the imported disk with a VMMigration in the VM's
// own namespace (isOwnMigrationDisk), which a cross-namespace migration is
// not, and the Proxmox provider refuses an unverified imported disk; refusing
// here keeps the migration from exporting, staging and importing a disk that
// can never be used.
func proxmoxCrossNamespaceTargetRefused(migration *infrav1beta1.VMMigration, targetProvider *infrav1beta1.Provider) bool {
	if targetProvider == nil || targetProvider.Spec.Type != infrav1beta1.ProviderTypeProxmox {
		return false
	}
	return migrationTargetVMKey(migration).Namespace != migration.Namespace
}

// failProxmoxCrossNamespaceTarget fails migration terminally with
// Validating=False/ReasonProxmoxCrossNamespaceTarget and the constant message
// (transitionToFailed: one status write, one Warning event, no immediate
// requeue).
func (r *VMMigrationReconciler) failProxmoxCrossNamespaceTarget(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionValidating,
		metav1.ConditionFalse, ReasonProxmoxCrossNamespaceTarget, proxmoxCrossNamespaceTargetMessage, migration.Generation)
	return r.transitionToFailed(ctx, migration, proxmoxCrossNamespaceTargetMessage)
}
