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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// proxmoxXNSMigration is xnsMigration(targetNamespace) with a Proxmox target
// Provider, the target namespace granted to the migration's namespace.
func proxmoxXNSMigration(t *testing.T, targetNamespace string) (*infrav1beta1.VMMigration, []client.Object) {
	t.Helper()
	migration, objs := xnsMigration(targetNamespace)
	found := false
	for _, o := range objs {
		if p, ok := o.(*infrav1beta1.Provider); ok && p.Name == "tgt-prov" {
			p.Spec.Type = infrav1beta1.ProviderTypeProxmox
			found = true
		}
	}
	require.True(t, found)
	return migration, append(objs, grantNamespace(xnsTarget, strPtr(xnsSource)))
}

func TestProxmoxCrossNamespaceTargetRefused(t *testing.T) {
	proxmox := &infrav1beta1.Provider{Spec: infrav1beta1.ProviderSpec{Type: infrav1beta1.ProviderTypeProxmox}}
	libvirt := &infrav1beta1.Provider{Spec: infrav1beta1.ProviderSpec{Type: infrav1beta1.ProviderTypeLibvirt}}
	vsphere := &infrav1beta1.Provider{Spec: infrav1beta1.ProviderSpec{Type: infrav1beta1.ProviderTypeVSphere}}
	other, _ := xnsMigration(xnsTarget)
	own, _ := xnsMigration(xnsSource)
	unset, _ := xnsMigration("")

	assert.True(t, proxmoxCrossNamespaceTargetRefused(other, proxmox))
	assert.False(t, proxmoxCrossNamespaceTargetRefused(own, proxmox))
	assert.False(t, proxmoxCrossNamespaceTargetRefused(unset, proxmox))
	assert.False(t, proxmoxCrossNamespaceTargetRefused(other, libvirt), "libvirt copies the disk instead")
	assert.False(t, proxmoxCrossNamespaceTargetRefused(other, vsphere), "vSphere refuses at Create, as before")
	assert.False(t, proxmoxCrossNamespaceTargetRefused(other, nil))
}

// TestVMMigrationProxmoxXNS_FailsAtValidating: a granted migration into
// another namespace through a Proxmox target Provider fails at Validating with
// the constant condition, before any provider call, with one Warning event.
func TestVMMigrationProxmoxXNS_FailsAtValidating(t *testing.T) {
	migration, objs := proxmoxXNSMigration(t, xnsTarget)
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	spy := &migrationSpy{}
	r, rec := newXNSMigrationReconciler(t, mountTestScheme(t), spy, append(objs, migration)...)

	res, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter, "no immediate requeue")

	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Equal(t, proxmoxCrossNamespaceTargetMessage, got.Status.Message)
	validating := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
	require.NotNil(t, validating)
	assert.Equal(t, metav1.ConditionFalse, validating.Status)
	assert.Equal(t, ReasonProxmoxCrossNamespaceTarget, validating.Reason)
	assert.Equal(t, proxmoxCrossNamespaceTargetMessage, validating.Message)

	imports, exports, snapshots, power := spy.calls()
	assert.Zero(t, imports+exports+snapshots+power, "no provider side effect")
	assert.Equal(t, 1, countEvents(rec, "MigrationFailed"))
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

// TestVMMigrationProxmoxXNS_FailsBeforeImport: a migration that passed
// Validating before the check existed fails at Importing before ImportDisk.
func TestVMMigrationProxmoxXNS_FailsBeforeImport(t *testing.T) {
	migration, objs := proxmoxXNSMigration(t, xnsTarget)
	migration.Status.Phase = infrav1beta1.MigrationPhaseImporting
	spy := &migrationSpy{}
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), spy, append(objs, migration)...)

	_, err := r.handleImportingPhase(context.Background(), migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	validating := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
	require.NotNil(t, validating)
	assert.Equal(t, ReasonProxmoxCrossNamespaceTarget, validating.Reason)
	imports, _, _, _ := spy.calls()
	assert.Zero(t, imports, "ImportDisk is never called")
}

// TestVMMigrationProxmoxXNS_SameNamespaceNotRefused: a Proxmox migration into
// its own namespace is not refused by this check.
func TestVMMigrationProxmoxXNS_SameNamespaceNotRefused(t *testing.T) {
	for _, target := range []string{"", xnsSource} {
		t.Run("target="+target, func(t *testing.T) {
			migration, objs := proxmoxXNSMigration(t, target)
			migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
			r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), &migrationSpy{}, append(objs, migration)...)

			_, err := r.handleValidatingPhase(context.Background(), migration)
			require.NoError(t, err)
			got := getXNSMigration(t, r, migration)
			if v := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionValidating); v != nil {
				assert.NotEqual(t, ReasonProxmoxCrossNamespaceTarget, v.Reason)
			}
			assert.NotEqual(t, proxmoxCrossNamespaceTargetMessage, got.Status.Message)
		})
	}
}
