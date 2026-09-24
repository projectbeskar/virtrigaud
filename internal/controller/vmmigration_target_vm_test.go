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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the operator side of namespaced libvirt domain names for a
// migration: the import names the VirtualMachine the disk is for (namespace +
// name) and leaves the landing name to the provider; the landing path the
// provider returns is recorded unchanged and is exactly what isOwnMigrationDisk
// accepts for that VirtualMachine.

// namespacedLandingPath is where the libvirt provider lands the disk imported
// for default/target-vm ("<namespace>.<name>-migrated.qcow2").
const namespacedLandingPath = "/var/lib/libvirt/images/default.target-vm-migrated.qcow2"

func TestImportingPhase_ThreadsTargetVMIdentity(t *testing.T) {
	for _, tc := range []struct {
		name            string
		targetNamespace string
		wantNamespace   string
	}{
		{"target in the migration's namespace", "", "default"},
		// Another namespace is only reachable with its grant (see
		// vmmigration_crossnamespace_test.go for the refusals).
		{"target in another namespace that grants it", "team-b", "team-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &capturingMigrationProvider{importResp: contracts.ImportDiskResponse{
				DiskId: "default.target-vm-migrated", Path: namespacedLandingPath,
			}}
			sourceVM, sourceProvider, targetProvider, migration := directionFixture(
				infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeLibvirt)
			migration.Spec.Target.Namespace = tc.targetNamespace
			r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret(),
				grantNamespace("team-b", strPtr("default")))

			_, err := r.handleImportingPhase(context.Background(), migration)
			require.NoError(t, err)
			require.Equal(t, 1, prov.importCalls)

			assert.Equal(t, contracts.ObjectIdentity{Namespace: tc.wantNamespace, Name: "target-vm"}, prov.lastImportReq.TargetVM,
				"the provider is told which VirtualMachine the disk is for")
			assert.Equal(t, "target-vm"+contracts.ImportedDiskNameSuffix, prov.lastImportReq.TargetName,
				"the legacy name is still sent for providers that ignore TargetVM")
			require.NotNil(t, migration.Status.DiskInfo)
			assert.Equal(t, namespacedLandingPath, migration.Status.DiskInfo.TargetPath,
				"the provider's landing path is recorded as returned, never re-derived")
		})
	}
}

func TestMigrationTargetVMKey(t *testing.T) {
	m := &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns-a"},
		Spec: infrav1beta1.VMMigrationSpec{
			Source: infrav1beta1.MigrationSource{VMRef: infrav1beta1.LocalObjectReference{Name: "src"}},
			Target: infrav1beta1.MigrationTarget{Name: "web"},
		},
	}
	assert.Equal(t, client.ObjectKey{Namespace: "ns-a", Name: "web"}, migrationTargetVMKey(m))
	m.Spec.Target.Namespace = "ns-b"
	assert.Equal(t, client.ObjectKey{Namespace: "ns-b", Name: "web"}, migrationTargetVMKey(m))
	m.Spec.Target.Name = ""
	assert.Equal(t, client.ObjectKey{Namespace: "ns-b", Name: "src-migrated"}, migrationTargetVMKey(m))
}

// TestBuildCreateRequest_NamespacedLandingDiskIsOwn: the target VM carries the
// provider's namespaced landing path (spec.importedDisk.path copied from
// status.diskInfo.targetPath), so isOwnMigrationDisk — a path equality, not a
// name derivation — marks it ImportedDisk, and the libvirt provider then
// attaches it in place because it is "<namespace>.<name>-migrated.qcow2" for
// this VM. A VM of the same name in another namespace gets no such flag.
func TestBuildCreateRequest_NamespacedLandingDiskIsOwn(t *testing.T) {
	vmClass := &infrav1beta1.VMClass{
		Spec: infrav1beta1.VMClassSpec{CPU: 1, Memory: resource.MustParse("1Gi")},
	}
	migration := &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec:       infrav1beta1.VMMigrationSpec{Target: infrav1beta1.MigrationTarget{Name: "target-vm"}},
		Status: infrav1beta1.VMMigrationStatus{
			ImportID: "default.target-vm-migrated",
			DiskInfo: &infrav1beta1.MigrationDiskInfo{TargetPath: namespacedLandingPath},
		},
	}
	vmFor := func(ns string) *infrav1beta1.VirtualMachine {
		vm := baseVM(ns)
		vm.Name = "target-vm"
		vm.Spec.ImportedDisk = &infrav1beta1.ImportedDiskRef{
			DiskID:       migration.Status.ImportID,
			Path:         namespacedLandingPath,
			Source:       "migration",
			MigrationRef: &infrav1beta1.LocalObjectReference{Name: "m1"},
		}
		return vm
	}
	r := newTestReconciler(coverageTestScheme(t), nil, migration)

	req, err := r.buildCreateRequest(context.Background(), vmFor("default"), "", vmClass, nil, nil)
	require.NoError(t, err)
	assert.True(t, req.Image.ImportedDisk)
	assert.Equal(t, namespacedLandingPath, req.Image.Path)
	assert.Equal(t, contracts.ObjectIdentity{UID: req.Owner.UID, Namespace: "default", Name: "target-vm"}, req.Owner,
		"the provider names the domain (and the file it adopts) from this owner")

	req, err = r.buildCreateRequest(context.Background(), vmFor("other"), "", vmClass, nil, nil)
	require.NoError(t, err)
	assert.False(t, req.Image.ImportedDisk, "the migration lives in another namespace")
}
