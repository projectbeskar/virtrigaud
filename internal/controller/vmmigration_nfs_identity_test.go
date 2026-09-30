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

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

func int64Ptr(v int64) *int64 { return &v }

func nfsIDProvider(name string, typ infrav1beta1.ProviderType) *infrav1beta1.Provider {
	p := readyProvider("mig", name)
	p.Spec.Type = typ
	return p
}

func TestNFSRootIdentityRefusal(t *testing.T) {
	libvirt := nfsIDProvider("lv", infrav1beta1.ProviderTypeLibvirt)
	vsphere := nfsIDProvider("vs", infrav1beta1.ProviderTypeVSphere)
	proxmox := nfsIDProvider("px", infrav1beta1.ProviderTypeProxmox)
	withIDs := func(uid, gid *int64) *infrav1beta1.VMMigration {
		m := nfsMigration("172.16.56.13", "/export/virtrigaud", "")
		m.Spec.Storage.NFS.UID, m.Spec.Storage.NFS.GID = uid, gid
		return m
	}

	refused := map[string]struct {
		m        *infrav1beta1.VMMigration
		src, tgt *infrav1beta1.Provider
		want     []string
	}{
		"uid 0, libvirt source": {withIDs(int64Ptr(0), nil), libvirt, vsphere,
			[]string{"spec.storage.nfs.uid set to 0", "the migration's source is a libvirt Provider"}},
		"gid 0, libvirt target": {withIDs(nil, int64Ptr(0)), vsphere, libvirt,
			[]string{"spec.storage.nfs.gid set to 0", "the migration's target is a libvirt Provider"}},
		"both, libvirt on both sides": {withIDs(int64Ptr(0), int64Ptr(0)), libvirt, libvirt,
			[]string{"spec.storage.nfs.uid and spec.storage.nfs.gid set to 0", "source and target"}},
		"uid 0 with a non-root gid": {withIDs(int64Ptr(0), int64Ptr(1000)), libvirt, proxmox,
			[]string{"spec.storage.nfs.uid set to 0"}},
	}
	for name, tc := range refused {
		msg := nfsRootIdentityRefusal(tc.m, tc.src, tc.tgt)
		for _, w := range tc.want {
			assert.Contains(t, msg, w, name)
		}
		assert.Contains(t, msg, "root_squash", name)
	}

	allowed := map[string]struct {
		m        *infrav1beta1.VMMigration
		src, tgt *infrav1beta1.Provider
	}{
		"unset":                       {withIDs(nil, nil), libvirt, libvirt},
		"non-root identity":           {withIDs(int64Ptr(1000), int64Ptr(1000)), libvirt, libvirt},
		"uid 0 without libvirt":       {withIDs(int64Ptr(0), int64Ptr(0)), vsphere, proxmox},
		"not an nfs migration":        {&infrav1beta1.VMMigration{}, libvirt, libvirt},
		"nfs backend, no nfs section": {&infrav1beta1.VMMigration{Spec: infrav1beta1.VMMigrationSpec{Storage: &infrav1beta1.MigrationStorage{Type: storagemigration.BackendNFS}}}, libvirt, libvirt},
	}
	for name, tc := range allowed {
		assert.Empty(t, nfsRootIdentityRefusal(tc.m, tc.src, tc.tgt), name)
	}

	s3 := withIDs(int64Ptr(0), int64Ptr(0))
	s3.Spec.Storage.Type = storagemigration.BackendS3
	assert.Empty(t, nfsRootIdentityRefusal(s3, libvirt, libvirt), "only the nfs backend presents the identity")
}

// TestVMMigration_NFSRootIdentityFailsAtValidating drives Validating: a
// libvirt-target nfs migration with uid 0 fails before any provider side
// effect, with the Validating condition naming why.
func TestVMMigration_NFSRootIdentityFailsAtValidating(t *testing.T) {
	migration, objs := xnsMigration("")
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	migration.Spec.Storage = &infrav1beta1.MigrationStorage{
		Type: storagemigration.BackendNFS,
		NFS:  &infrav1beta1.NFSStorageConfig{Server: "172.16.56.13", Export: "/export/virtrigaud", UID: int64Ptr(0)},
	}
	for _, o := range objs {
		if p, ok := o.(*infrav1beta1.Provider); ok {
			p.Status.ReportedCapabilities = &infrav1beta1.ReportedCapabilities{
				SupportedExportBackends: []string{"pvc", "nfs"},
				SupportedImportBackends: []string{"pvc", "nfs"},
				SupportedTransferModes:  []string{"relay"},
			}
		}
	}
	spy := &migrationSpy{}
	r, _ := newXNSMigrationReconciler(t, capGatingScheme(t), spy, append(objs, migration)...)

	_, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "spec.storage.nfs.uid set to 0")
	validating := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
	require.NotNil(t, validating)
	assert.Equal(t, metav1.ConditionFalse, validating.Status)
	assert.Equal(t, ReasonNFSRootIdentityNotAllowed, validating.Reason)
	imports, exports, snapshots, power := spy.calls()
	assert.Zero(t, imports+exports+snapshots+power, "no provider side effect")
}
