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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestRoutedImportTarget_MovedPendingHost pins B4 and #416 M4: an import of
// unknown outcome is re-sent only to the host it was recorded for; the
// automatic re-send to a target whose pending host moved is held for an
// operator, and the operator's confirmation sends it to the current pending
// host.
func TestRoutedImportTarget_MovedPendingHost(t *testing.T) {
	setup := func(t *testing.T, confirm bool) (*VMMigrationReconciler, *infravirtrigaudiov1beta1.VMMigration) {
		t.Helper()
		scheme := mountTestScheme(t)
		m := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: "default", UID: "mig-uid"},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src"}},
				Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "c"}},
			},
			Status: infravirtrigaudiov1beta1.VMMigrationStatus{
				Phase:       infravirtrigaudiov1beta1.MigrationPhaseImporting,
				TargetVMUID: "vm-uid",
				StorageInfo: &infravirtrigaudiov1beta1.MigrationStorageInfo{ImportTarget: &infravirtrigaudiov1beta1.MigrationImportTarget{
					Namespace: "default", Name: "web", ProviderUID: "p-uid", Host: "kvm-01"}},
			},
		}
		setRequestMarker(&m.Status.Conditions, migrationConditionImportRequested, migrationConditionImportRequested, "requested", 0,
			time.Now().Add(-time.Hour))
		if confirm {
			m.Annotations = map[string]string{operationOutcomeAnnotation: operationOutcomeNotPerformedPrefix +
				requestMarkerAttemptID(m.Status.Conditions, migrationConditionImportRequested)}
		}
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID("vm-uid")},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{
				Source: "migration", MigrationRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "mig"}}},
			Status: infravirtrigaudiov1beta1.VirtualMachineStatus{Placement: &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "kvm-02"}},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m, vm).
			WithStatusSubresource(&infravirtrigaudiov1beta1.VMMigration{}, &infravirtrigaudiov1beta1.VirtualMachine{}).Build()
		got := &infravirtrigaudiov1beta1.VMMigration{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mig"}, got))
		return &VMMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(16)}, got
	}

	t.Run("an automatic re-send to a moved pending host is held", func(t *testing.T) {
		r, m := setup(t, false)
		host, _, held, _, err := r.routedImportTarget(context.Background(), m)
		require.NoError(t, err)
		assert.True(t, held)
		assert.Empty(t, host)
		c := meta.FindStatusCondition(m.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionReady)
		require.NotNil(t, c)
		assert.Equal(t, migrationReasonImportOutcomeUnknown, c.Reason)
		assert.Equal(t, routedImportHostMovedMessage, c.Message)
	})
	t.Run("the operator's confirmation sends it to the current pending host", func(t *testing.T) {
		r, m := setup(t, true)
		host, uid, held, _, err := r.routedImportTarget(context.Background(), m)
		require.NoError(t, err)
		assert.False(t, held)
		assert.Equal(t, "kvm-02", host)
		assert.Equal(t, "vm-uid", uid)
	})
}

// TestRoutedTargetDiskMatches pins #416 M2.
func TestRoutedTargetDiskMatches(t *testing.T) {
	m := &infravirtrigaudiov1beta1.VMMigration{ObjectMeta: metav1.ObjectMeta{Name: "mig"}}
	m.Status.ImportID = "d1"
	m.Status.DiskInfo = &infravirtrigaudiov1beta1.MigrationDiskInfo{TargetPath: "/p/d1.qcow2"}
	vm := func(name, id, path string) *infravirtrigaudiov1beta1.VirtualMachine {
		return &infravirtrigaudiov1beta1.VirtualMachine{Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{
			DiskID: id, Path: path, MigrationRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: name}}}}
	}
	assert.True(t, routedTargetDiskMatches(m, vm("mig", "d1", "/p/d1.qcow2")))
	assert.False(t, routedTargetDiskMatches(m, vm("other", "d1", "/p/d1.qcow2")), "another migration")
	assert.False(t, routedTargetDiskMatches(m, vm("mig", "d2", "/p/d1.qcow2")), "another disk id")
	assert.False(t, routedTargetDiskMatches(m, vm("mig", "d1", "/p/other.qcow2")), "another path")
	assert.False(t, routedTargetDiskMatches(m, &infravirtrigaudiov1beta1.VirtualMachine{}), "no imported disk")
}
