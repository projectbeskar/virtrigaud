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

func TestNFSRootIdentityRefusal(t *testing.T) {
	withIDs := func(uid, gid *int64) *infrav1beta1.VMMigration {
		m := nfsMigration("172.16.56.13", "/export/virtrigaud", "")
		m.Spec.Storage.NFS.UID, m.Spec.Storage.NFS.GID = uid, gid
		return m
	}

	refused := map[string]struct {
		m    *infrav1beta1.VMMigration
		want string
	}{
		"uid 0":                     {withIDs(int64Ptr(0), nil), "spec.storage.nfs.uid set to 0"},
		"gid 0":                     {withIDs(nil, int64Ptr(0)), "spec.storage.nfs.gid set to 0"},
		"both":                      {withIDs(int64Ptr(0), int64Ptr(0)), "spec.storage.nfs.uid and spec.storage.nfs.gid set to 0"},
		"uid 0 with a non-root gid": {withIDs(int64Ptr(0), int64Ptr(1000)), "spec.storage.nfs.uid set to 0"},
		"gid 0 with a non-root uid": {withIDs(int64Ptr(1000), int64Ptr(0)), "spec.storage.nfs.gid set to 0"},
	}
	for name, tc := range refused {
		msg := nfsRootIdentityRefusal(tc.m)
		assert.Contains(t, msg, tc.want, name)
		assert.Contains(t, msg, "root_squash", name)
		assert.Contains(t, msg, "Use a dedicated non-zero uid/gid that owns the export", name)
	}

	allowed := map[string]*infrav1beta1.VMMigration{
		"unset":                       withIDs(nil, nil),
		"non-root identity":           withIDs(int64Ptr(1000), int64Ptr(1000)),
		"not an nfs migration":        {},
		"nfs backend, no nfs section": {Spec: infrav1beta1.VMMigrationSpec{Storage: &infrav1beta1.MigrationStorage{Type: storagemigration.BackendNFS}}},
	}
	for name, m := range allowed {
		assert.Empty(t, nfsRootIdentityRefusal(m), name)
	}

	s3 := withIDs(int64Ptr(0), int64Ptr(0))
	s3.Spec.Storage.Type = storagemigration.BackendS3
	assert.Empty(t, nfsRootIdentityRefusal(s3), "only the nfs backend presents the identity")
}

// TestVMMigration_NFSRootIdentityFailsAtValidating drives Validating for every
// pair of provider types: an nfs migration with uid or gid 0 fails before any
// provider side effect, with the Validating condition naming why — and before
// the source VM or the providers are even read.
func TestVMMigration_NFSRootIdentityFailsAtValidating(t *testing.T) {
	types := []infrav1beta1.ProviderType{
		infrav1beta1.ProviderTypeLibvirt, infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeProxmox,
	}
	for _, src := range types {
		for _, tgt := range types {
			t.Run(string(src)+"->"+string(tgt), func(t *testing.T) {
				migration, objs := xnsMigration("")
				migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
				migration.Spec.Storage = &infrav1beta1.MigrationStorage{
					Type: storagemigration.BackendNFS,
					NFS: &infrav1beta1.NFSStorageConfig{Server: "172.16.56.13", Export: "/export/virtrigaud",
						UID: int64Ptr(1000), GID: int64Ptr(0)},
				}
				for _, o := range objs {
					p, ok := o.(*infrav1beta1.Provider)
					if !ok {
						continue
					}
					p.Spec.Type = src
					if p.Name == "tgt-prov" {
						p.Spec.Type = tgt
					}
					p.Status.ReportedCapabilities = &infrav1beta1.ReportedCapabilities{
						SupportedExportBackends: []string{"pvc", "nfs"},
						SupportedImportBackends: []string{"pvc", "nfs"},
						SupportedTransferModes:  []string{"relay"},
					}
				}
				spy := &migrationSpy{}
				r, _ := newXNSMigrationReconciler(t, capGatingScheme(t), spy, append(objs, migration)...)

				_, err := r.handleValidatingPhase(context.Background(), migration)
				require.NoError(t, err)
				got := getXNSMigration(t, r, migration)
				assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
				assert.Contains(t, got.Status.Message, "spec.storage.nfs.gid set to 0")
				validating := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
				require.NotNil(t, validating)
				assert.Equal(t, metav1.ConditionFalse, validating.Status)
				assert.Equal(t, ReasonNFSRootIdentityNotAllowed, validating.Reason)
				imports, exports, snapshots, power := spy.calls()
				assert.Zero(t, imports+exports+snapshots+power, "no provider side effect")
			})
		}
	}
}

// TestVMMigration_NFSRootIdentityIsCheckedFromTheSpecAlone: the refusal needs
// no source VM and no Provider — it is the first thing Validating does.
func TestVMMigration_NFSRootIdentityIsCheckedFromTheSpecAlone(t *testing.T) {
	migration, _ := xnsMigration("")
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	migration.Spec.Storage = &infrav1beta1.MigrationStorage{
		Type: storagemigration.BackendNFS,
		NFS:  &infrav1beta1.NFSStorageConfig{Server: "172.16.56.13", Export: "/export/virtrigaud", UID: int64Ptr(0)},
	}
	r, _ := newXNSMigrationReconciler(t, capGatingScheme(t), &migrationSpy{}, migration)

	_, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	validating := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
	require.NotNil(t, validating)
	assert.Equal(t, ReasonNFSRootIdentityNotAllowed, validating.Reason, "not a missing source VM or Provider")
}
