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
)

// The migration source is namespace-local by type (spec.source.vmRef is a
// LocalObjectReference), but spec.source.providerRef could name ANY Provider.
// Exporting the source VM's provider ID through a Provider the VM does not run
// on would read whatever unrelated VM has that ID there — possibly another
// tenant's. These tests pin that only the VM's own Provider is ever used.

func TestMigrationSourceProviderRef(t *testing.T) {
	vm := func(ref infrav1beta1.ObjectRef) *infrav1beta1.VirtualMachine {
		return &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "src-vm", Namespace: "team-a"},
			Spec:       infrav1beta1.VirtualMachineSpec{ProviderRef: ref},
		}
	}
	mig := func(ref *infrav1beta1.ObjectRef) *infrav1beta1.VMMigration {
		return &infrav1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "team-a"},
			Spec:       infrav1beta1.VMMigrationSpec{Source: infrav1beta1.MigrationSource{ProviderRef: ref}},
		}
	}
	local := infrav1beta1.ObjectRef{Name: "p"}
	shared := infrav1beta1.ObjectRef{Name: "p", Namespace: "virtrigaud-system"}

	cases := map[string]struct {
		vmRef   infrav1beta1.ObjectRef
		specRef *infrav1beta1.ObjectRef
		want    infrav1beta1.ObjectRef
		wantErr bool
	}{
		"unset: the VM's provider, namespace made explicit": {local, nil, infrav1beta1.ObjectRef{Name: "p", Namespace: "team-a"}, false},
		"unset: a shared provider stays shared":             {shared, nil, shared, false},
		"same provider, implicit namespace":                 {local, &infrav1beta1.ObjectRef{Name: "p"}, infrav1beta1.ObjectRef{Name: "p", Namespace: "team-a"}, false},
		"same provider, explicit namespace":                 {local, &infrav1beta1.ObjectRef{Name: "p", Namespace: "team-a"}, infrav1beta1.ObjectRef{Name: "p", Namespace: "team-a"}, false},
		"same shared provider":                              {shared, &infrav1beta1.ObjectRef{Name: "p", Namespace: "virtrigaud-system"}, shared, false},
		"different name":                                    {local, &infrav1beta1.ObjectRef{Name: "other"}, infrav1beta1.ObjectRef{}, true},
		"same name in another namespace":                    {local, &infrav1beta1.ObjectRef{Name: "p", Namespace: "team-b"}, infrav1beta1.ObjectRef{}, true},
		"shared VM provider, local spec ref":                {shared, &infrav1beta1.ObjectRef{Name: "p"}, infrav1beta1.ObjectRef{}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := migrationSourceProviderRef(mig(tc.specRef), vm(tc.vmRef))
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "is not the Provider the source VM src-vm runs on")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// mismatchedSourceMigration returns the xnsMigration fixture (same-namespace
// target) whose spec.source.providerRef names a ready Provider the source VM
// does NOT run on, plus that decoy Provider.
func mismatchedSourceMigration(phase infrav1beta1.MigrationPhase) (*infrav1beta1.VMMigration, []*infrav1beta1.Provider) {
	migration, _ := xnsMigration("")
	migration.Spec.Source.ProviderRef = &infrav1beta1.ObjectRef{Name: "victim-prov", Namespace: "team-b"}
	migration.Status.Phase = phase
	decoy := readyProvider("team-b", "victim-prov")
	decoy.Spec.Type = infrav1beta1.ProviderTypeVSphere
	return migration, []*infrav1beta1.Provider{decoy}
}

// TestSourceProviderMismatch_FailsBeforeAnyProviderCall runs each phase that
// resolves the source provider with a mismatched spec.source.providerRef: the
// migration fails with an actionable message and no snapshot, export or
// power operation reaches any Provider.
func TestSourceProviderMismatch_FailsBeforeAnyProviderCall(t *testing.T) {
	for _, phase := range []infrav1beta1.MigrationPhase{
		infrav1beta1.MigrationPhaseValidating,
		infrav1beta1.MigrationPhaseSnapshotting,
		infrav1beta1.MigrationPhaseExporting,
	} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			migration, decoys := mismatchedSourceMigration(phase)
			migration.Spec.Source.PowerOffBeforeMigration = true
			migration.Spec.Source.CreateSnapshot = true
			_, objs := xnsMigration("")
			for _, d := range decoys {
				objs = append(objs, d)
			}
			spy := &migrationSpy{}
			r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), spy, append(objs, migration)...)

			var err error
			switch phase {
			case infrav1beta1.MigrationPhaseValidating:
				_, err = r.handleValidatingPhase(ctx, migration)
			case infrav1beta1.MigrationPhaseSnapshotting:
				_, err = r.handleSnapshottingPhase(ctx, migration)
			case infrav1beta1.MigrationPhaseExporting:
				_, err = r.handleExportingPhase(ctx, migration)
			}
			require.NoError(t, err)

			got := getXNSMigration(t, r, migration)
			assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.Contains(t, got.Status.Message, "team-b/victim-prov is not the Provider the source VM src-vm runs on (team-a/src-prov)")
			imports, exports, snapshots, power := spy.calls()
			assert.Zero(t, imports+exports+snapshots+power, "no provider is asked to act on the source VM's ID")
		})
	}
}

// TestSourceProviderMatch_Proceeds: naming the VM's own Provider explicitly
// keeps working.
func TestSourceProviderMatch_Proceeds(t *testing.T) {
	migration, objs := xnsMigration("")
	migration.Spec.Source.ProviderRef = &infrav1beta1.ObjectRef{Name: "src-prov"}
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), &migrationSpy{}, append(objs, migration)...)

	_, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, getXNSMigration(t, r, migration).Status.Phase)
}

// TestSourceProviderMismatch_SnapshotCleanupRefused: cleanup of a
// migration-created snapshot also goes only through the VM's own Provider.
func TestSourceProviderMismatch_SnapshotCleanupRefused(t *testing.T) {
	migration, decoys := mismatchedSourceMigration(infrav1beta1.MigrationPhaseReady)
	migration.Status.SnapshotID = "snap-1"
	_, objs := xnsMigration("")
	for _, d := range decoys {
		objs = append(objs, d)
	}
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), &migrationSpy{}, append(objs, migration)...)

	err := r.deleteSourceSnapshot(context.Background(), migration)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not the Provider the source VM")
}
