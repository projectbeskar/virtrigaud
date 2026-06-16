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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// capturingMigrationProvider embeds stubProvider, records the ImportDiskRequest
// it received, and returns a caller-supplied ImportDiskResponse (including a
// real Path). It is the test seam for the direction-aware format/path threading
// added in ADR-0006 Slice 2: tests inspect lastImportReq.Format and assert that
// importResp.Path flows through to Status.DiskInfo.TargetPath and onward to the
// created VM's Spec.ImportedDisk.Path.
type capturingMigrationProvider struct {
	stubProvider
	importResp    contracts.ImportDiskResponse
	lastImportReq contracts.ImportDiskRequest
	importCalls   int
}

// ImportDisk records the request and returns the preconfigured response.
func (p *capturingMigrationProvider) ImportDisk(_ context.Context, req contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	p.importCalls++
	p.lastImportReq = req
	return p.importResp, nil
}

var _ contracts.Provider = (*capturingMigrationProvider)(nil)

// directionReconciler builds a VMMigrationReconciler pinned to the supplied fake
// provider instance, with a fake client seeded with the supplied objects and
// status-subresource support. Mirrors newRaceReconciler but lets the caller pass
// a capturing provider.
func directionReconciler(t *testing.T, prov contracts.Provider, objs ...client.Object) (*VMMigrationReconciler, client.Client) {
	t.Helper()
	scheme := capGatingScheme(t)
	// The importing phase resolves the s3 credentials Secret via the client, so
	// corev1 must be registered alongside the infra types.
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(objs...).
		Build()

	r := &VMMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(50),
		providerInstanceFn: func(_ context.Context, _ *infrav1beta1.Provider) (contracts.Provider, error) {
			return prov, nil
		},
	}
	return r, c
}

// directionFixture stages a migration in the Importing phase with the source
// checksum already recorded (as after a successful export), plus a source VM, a
// typed source Provider and a typed target Provider. The provider types drive
// the direction-aware format/path derivations under test.
func directionFixture(sourceType, targetType infrav1beta1.ProviderType) (
	*infrav1beta1.VirtualMachine,
	*infrav1beta1.Provider,
	*infrav1beta1.Provider,
	*infrav1beta1.VMMigration,
) {
	sourceVM := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "source-vm", Namespace: "default"},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "source-provider"},
		},
	}
	sourceVM.Status.ID = "vm-123"

	sourceProvider := &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "source-provider", Namespace: "default"},
		Spec:       infrav1beta1.ProviderSpec{Type: sourceType},
	}
	targetProvider := &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "target-provider", Namespace: "default"},
		Spec:       infrav1beta1.ProviderSpec{Type: targetType},
	}

	migration := &infrav1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "dir-migration",
			Namespace:  "default",
			UID:        "uid-dir-1",
			Generation: 1,
		},
		Spec: infrav1beta1.VMMigrationSpec{
			Source: infrav1beta1.MigrationSource{
				VMRef: infrav1beta1.LocalObjectReference{Name: "source-vm"},
			},
			Target: infrav1beta1.MigrationTarget{
				Name:        "target-vm",
				ProviderRef: infrav1beta1.ObjectRef{Name: "target-provider"},
			},
			// s3 backend: the staged object is the SOURCE's native format and the
			// import format derivation is exercised. Credentials are not needed
			// because the providerInstanceFn seam bypasses real S3 access and the
			// importing phase reads the secret only for the (no-op here) creds map.
			Storage: &infrav1beta1.MigrationStorage{
				Type:         "s3",
				TransferMode: "relay",
				S3: &infrav1beta1.S3StorageConfig{
					Bucket: "mig-bucket",
					CredentialsSecretRef: infrav1beta1.ObjectRef{
						Name: "s3-creds",
					},
				},
			},
		},
	}
	migration.Status.Phase = infrav1beta1.MigrationPhaseImporting
	migration.Status.DiskInfo = &infrav1beta1.MigrationDiskInfo{SourceChecksum: "checksum-src"}
	return sourceVM, sourceProvider, targetProvider, migration
}

// s3CredsSecret returns the Secret the importing phase resolves for the s3
// backend. Values are dummy; the providerInstanceFn seam never uses them.
func s3CredsSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: "default"},
		Data: map[string][]byte{
			"accessKeyID":     []byte("AKIA-test"),
			"secretAccessKey": []byte("secret-test"),
		},
	}
}

// TestImportingPhase_ReverseLibvirtToVSphere_ThreadsQcow2AndPropagatesPath is
// the core reverse-direction assertion (Gap A + Gap B): for a libvirt source ->
// vSphere target migration over s3, the import format threaded to the target
// provider must be qcow2 (libvirt's native staged format), the landed
// TargetFormat must be labeled vmdk (vSphere's native format), and the real
// datastore Path returned by ImportDisk must be recorded in
// Status.DiskInfo.TargetPath.
func TestImportingPhase_ReverseLibvirtToVSphere_ThreadsQcow2AndPropagatesPath(t *testing.T) {
	ctx := context.Background()

	const vspherePath = "[datastore1] target-vm-migrated/target-vm-migrated.vmdk"
	prov := &capturingMigrationProvider{
		importResp: contracts.ImportDiskResponse{
			DiskId:          "target-vm-migrated",
			Path:            vspherePath,
			ActualSizeBytes: 4096,
			Checksum:        "imported-checksum",
		},
	}

	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infrav1beta1.ProviderTypeLibvirt, infrav1beta1.ProviderTypeVSphere)
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret())

	res, err := r.handleImportingPhase(ctx, migration)
	require.NoError(t, err)
	require.Equal(t, 1, prov.importCalls, "import must be issued exactly once")

	// Gap B: the staged object is libvirt's native qcow2; the target reads qcow2.
	assert.Equal(t, "qcow2", prov.lastImportReq.Format,
		"reverse libvirt->vSphere must thread Format=qcow2 to ImportDisk")

	// Checksum threading from Slice 1 stays intact.
	assert.Equal(t, "checksum-src", prov.lastImportReq.ExpectedChecksum,
		"source checksum must flow to import.ExpectedChecksum")

	require.NotNil(t, migration.Status.DiskInfo)
	// Gap A: the real datastore path is recorded, not dropped.
	assert.Equal(t, vspherePath, migration.Status.DiskInfo.TargetPath,
		"importResp.Path must be recorded in Status.DiskInfo.TargetPath")
	// Gap B cont.: the landed disk on a vSphere target is vmdk.
	assert.Equal(t, "vmdk", migration.Status.DiskInfo.TargetFormat,
		"vSphere target must label TargetFormat=vmdk")

	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, migration.Status.Phase)
	assert.False(t, res.Requeue, "synchronous import must not immediate-requeue")
}

// TestImportingPhase_ForwardVSphereToLibvirt_Unchanged proves the forward path
// is byte-identical to its pre-Slice-2 behavior: a vSphere source -> libvirt
// target migration threads vmdk (vSphere's native staged format) to import and
// labels the landed TargetFormat as qcow2 (libvirt's native format). The
// libvirt provider returns a pool path, which is propagated like any other.
func TestImportingPhase_ForwardVSphereToLibvirt_Unchanged(t *testing.T) {
	ctx := context.Background()

	const libvirtPath = "/var/lib/libvirt/images/target-vm-migrated.qcow2"
	prov := &capturingMigrationProvider{
		importResp: contracts.ImportDiskResponse{
			DiskId:          "target-vm-migrated",
			Path:            libvirtPath,
			ActualSizeBytes: 4096,
			Checksum:        "imported-checksum",
		},
	}

	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeLibvirt)
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret())

	res, err := r.handleImportingPhase(ctx, migration)
	require.NoError(t, err)
	require.Equal(t, 1, prov.importCalls)

	// Forward: staged object is vSphere's native vmdk.
	assert.Equal(t, "vmdk", prov.lastImportReq.Format,
		"forward vSphere->libvirt must thread Format=vmdk to ImportDisk")
	assert.Equal(t, "checksum-src", prov.lastImportReq.ExpectedChecksum)

	require.NotNil(t, migration.Status.DiskInfo)
	// Landed disk on a libvirt target is qcow2 — unchanged from before Slice 2.
	assert.Equal(t, "qcow2", migration.Status.DiskInfo.TargetFormat,
		"libvirt target must label TargetFormat=qcow2 (unchanged)")
	// The libvirt pool path returned by the provider is recorded.
	assert.Equal(t, libvirtPath, migration.Status.DiskInfo.TargetPath)

	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, migration.Status.Phase)
	assert.False(t, res.Requeue)
}

// TestCreatingPhase_PropagatesTargetPathToImportedDisk asserts the creating
// phase copies Status.DiskInfo.TargetPath (and TargetFormat) into the created
// VirtualMachine's Spec.ImportedDisk, so the VM controller attaches the disk at
// the provider-native path rather than synthesizing a libvirt default. Covers
// both directions via the recorded TargetPath.
func TestCreatingPhase_PropagatesTargetPathToImportedDisk(t *testing.T) {
	ctx := context.Background()

	const vspherePath = "[datastore1] target-vm-migrated/target-vm-migrated.vmdk"
	prov := &capturingMigrationProvider{}

	_, sourceProvider, targetProvider, migration := directionFixture(
		infrav1beta1.ProviderTypeLibvirt, infrav1beta1.ProviderTypeVSphere)
	// Stage as if import already completed.
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.ImportID = "target-vm-migrated"
	migration.Status.DiskInfo.TargetDiskID = "target-vm-migrated"
	migration.Status.DiskInfo.TargetFormat = "vmdk"
	migration.Status.DiskInfo.TargetPath = vspherePath
	migration.Status.DiskInfo.TargetChecksum = "imported-checksum"

	r, c := directionReconciler(t, prov, sourceProvider, targetProvider, migration)

	_, err := r.handleCreatingPhase(ctx, migration)
	require.NoError(t, err)

	created := &infrav1beta1.VirtualMachine{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "target-vm"}, created))

	require.NotNil(t, created.Spec.ImportedDisk)
	assert.Equal(t, vspherePath, created.Spec.ImportedDisk.Path,
		"creating phase must set ImportedDisk.Path from Status.DiskInfo.TargetPath")
	assert.Equal(t, "vmdk", created.Spec.ImportedDisk.Format)
	assert.Equal(t, "target-vm-migrated", created.Spec.ImportedDisk.DiskID)
	assert.Equal(t, "migration", created.Spec.ImportedDisk.Source)
}

// TestNativeDiskFormat_DerivesFromProviderType pins the direction-agnostic
// format derivation: vSphere -> vmdk, libvirt/proxmox/unknown -> qcow2. This is
// the single source of truth for both the import-format and target-format
// derivations, so the two can never disagree for the same provider type.
func TestNativeDiskFormat_DerivesFromProviderType(t *testing.T) {
	cases := []struct {
		name string
		typ  infrav1beta1.ProviderType
		want string
	}{
		{"vsphere", infrav1beta1.ProviderTypeVSphere, "vmdk"},
		{"libvirt", infrav1beta1.ProviderTypeLibvirt, "qcow2"},
		{"proxmox", infrav1beta1.ProviderTypeProxmox, "qcow2"},
		{"unknown-defaults-qcow2", infrav1beta1.ProviderType("mock"), "qcow2"},
		{"empty-defaults-qcow2", infrav1beta1.ProviderType(""), "qcow2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &infrav1beta1.Provider{Spec: infrav1beta1.ProviderSpec{Type: tc.typ}}
			assert.Equal(t, tc.want, nativeDiskFormat(p))
			// stagedImportFormat (source) and landedTargetFormat (target) are the
			// same derivation applied to the source vs target provider.
			assert.Equal(t, tc.want, stagedImportFormat(p))
			assert.Equal(t, tc.want, landedTargetFormat(p))
		})
	}
	// Nil provider must not panic and falls back to qcow2.
	assert.Equal(t, "qcow2", nativeDiskFormat(nil))
}
