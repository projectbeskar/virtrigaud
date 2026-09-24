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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// These tests pin the VMMigration controller's side of ADR-0007 Addendum A, A1:
// a clustered TARGET provider is rejected until disk import is routed (P3), and
// a clustered SOURCE VM with no confirmed host binding is waited for, never
// sent a per-VM call.

func clusteredMigrationFixture(t *testing.T, srcClustered, tgtClustered bool, srcPlacement *infravirtrigaudiov1beta1.PlacementStatus) (*VMMigrationReconciler, *infravirtrigaudiov1beta1.VMMigration) {
	t.Helper()
	scheme := mountTestScheme(t)
	migration := &infravirtrigaudiov1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-c", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
			Source: infravirtrigaudiov1beta1.MigrationSource{
				VMRef:       infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"},
				ProviderRef: &infravirtrigaudiov1beta1.ObjectRef{Name: "src-prov"},
			},
			Target: infravirtrigaudiov1beta1.MigrationTarget{
				Name:        "tgt-vm",
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "tgt-prov"},
			},
		},
		Status: infravirtrigaudiov1beta1.VMMigrationStatus{Phase: infravirtrigaudiov1beta1.MigrationPhaseValidating},
	}
	srcVM := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "src-vm", Namespace: "default"},
		Status:     infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "src-vm", Placement: srcPlacement},
	}
	srcProv := readyProvider("default", "src-prov")
	tgtProv := readyProvider("default", "tgt-prov")
	if srcClustered {
		srcProv.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	}
	if tgtClustered {
		tgtProv.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(migration, srcVM, srcProv, tgtProv).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VMMigration{}).
		Build()
	return &VMMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(16)}, migration
}

func getMigration(t *testing.T, r *VMMigrationReconciler) *infravirtrigaudiov1beta1.VMMigration {
	t.Helper()
	m := &infravirtrigaudiov1beta1.VMMigration{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "mig-c", Namespace: "default"}, m))
	return m
}

func TestHandleValidatingPhase_ClusteredTargetRejected(t *testing.T) {
	r, migration := clusteredMigrationFixture(t, false, true, nil)
	_, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	got := getMigration(t, r)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "topology: cluster")
}

func TestHandleValidatingPhase_ClusteredUnboundSourceWaits(t *testing.T) {
	r, migration := clusteredMigrationFixture(t, true, false, nil)
	res, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "an unbound source is waited for")
	got := getMigration(t, r)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseValidating, got.Status.Phase, "not failed, not advanced")
	assert.Contains(t, got.Status.Message, "host binding")
}
