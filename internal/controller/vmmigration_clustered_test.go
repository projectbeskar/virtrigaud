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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMMigration controller's side of ADR-0007 Addendum A, A1:
// a clustered TARGET provider is rejected until disk import is routed (P3), and
// a clustered SOURCE VM with no confirmed host binding is waited for, never
// sent a per-VM call. Since slice 3 a bound clustered source exports through
// the host-side s3 / nfs backends; pvc is refused up front.

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
		// spec.source.providerRef must name the Provider the source VM runs on.
		Spec:   infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "src-prov"}},
		Status: infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "src-vm", Placement: srcPlacement},
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

// capsStub is a provider that reports caps (or fails GetCapabilities with err).
type capsStub struct {
	stubProvider
	caps contracts.Capabilities
	err  error
}

func (c *capsStub) GetCapabilities(context.Context) (contracts.Capabilities, error) {
	return c.caps, c.err
}

// TestHandleValidatingPhase_ClusteredTargetRejected pins ADR-0007 Addendum B,
// B5: a clustered target whose provider does not report supportsRoutedImport
// still fails at Validating, before any side effect, with a constant message.
func TestHandleValidatingPhase_ClusteredTargetRejected(t *testing.T) {
	for name, inst := range map[string]contracts.Provider{
		"no capability reporter":       &stubProvider{},
		"supportsRoutedImport not set": &capsStub{caps: contracts.Capabilities{SupportsClustering: true, SupportsDiskImport: true}},
	} {
		t.Run(name, func(t *testing.T) {
			r, migration := clusteredMigrationFixture(t, false, true, nil)
			r.providerInstanceFn = func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
				return inst, nil
			}
			_, err := r.handleValidatingPhase(context.Background(), migration)
			require.NoError(t, err)
			got := getMigration(t, r)
			assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.Equal(t, clusteredTargetUnsupportedMessage, got.Status.Message)
			assert.Contains(t, got.Status.Message, "topology: cluster")
			c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionFailed)
			require.NotNil(t, c)
			assert.Equal(t, ReasonClusteredTargetUnsupported, c.Reason)
			assert.Empty(t, got.Status.TargetVMClaim, "no target VM is created")
		})
	}
}

// TestHandleValidatingPhase_ClusteredTargetCapabilitiesUnreadableWaits: a
// capability query that fails is waited for, never a refusal.
func TestHandleValidatingPhase_ClusteredTargetCapabilitiesUnreadableWaits(t *testing.T) {
	r, migration := clusteredMigrationFixture(t, false, true, nil)
	r.providerInstanceFn = func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
		return &capsStub{err: errors.New("unavailable")}, nil
	}
	res, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)
	got := getMigration(t, r)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseValidating, got.Status.Phase)
	c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionValidating)
	require.NotNil(t, c)
	assert.Equal(t, migrationReasonCapabilitiesUnknown, c.Reason)
}

// TestGateMigrationStorageBackend_ClusteredTargetRefusesPVC pins B3 / B7.5.
func TestGateMigrationStorageBackend_ClusteredTargetRefusesPVC(t *testing.T) {
	source := readyProvider("default", "src-prov")
	source.Status.ReportedCapabilities = &infravirtrigaudiov1beta1.ReportedCapabilities{
		SupportedExportBackends: []string{"pvc", "s3", "nfs"}, SupportedTransferModes: []string{"relay"},
	}
	target := readyProvider("default", "tgt-prov")
	target.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	target.Status.ReportedCapabilities = &infravirtrigaudiov1beta1.ReportedCapabilities{
		SupportedImportBackends: []string{"s3", "nfs"}, SupportedTransferModes: []string{"relay"},
	}
	r := &VMMigrationReconciler{}
	for _, backend := range []string{"pvc", ""} {
		m := &infravirtrigaudiov1beta1.VMMigration{}
		if backend != "" {
			m.Spec.Storage = &infravirtrigaudiov1beta1.MigrationStorage{Type: backend}
		}
		assert.Equal(t, clusteredTargetPVCRefusedMessage, r.gateMigrationStorageBackend(m, source, target), "backend %q", backend)
	}
	for _, backend := range []string{"s3", "nfs"} {
		m := &infravirtrigaudiov1beta1.VMMigration{Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
			Storage: &infravirtrigaudiov1beta1.MigrationStorage{Type: backend}}}
		assert.Empty(t, r.gateMigrationStorageBackend(m, source, target), "backend %q", backend)
	}
}

// TestGateMigrationStorageBackend_ClusteredSource pins ADR-0007 Addendum A
// slice 3 on the migration side: a clustered SOURCE may export through the
// host-side s3 and nfs backends it advertises, but never through pvc (which
// reads the disk from the provider pod) — even when an older clustered provider
// advertised no export backends at all.
func TestGateMigrationStorageBackend_ClusteredSource(t *testing.T) {
	clustered := readyProvider("default", "src-prov")
	clustered.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	clustered.Status.ReportedCapabilities = &infravirtrigaudiov1beta1.ReportedCapabilities{
		SupportedExportBackends: []string{"s3", "nfs"},
		SupportedTransferModes:  []string{"relay"},
	}
	olderClustered := readyProvider("default", "src-prov")
	olderClustered.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	target := readyProvider("default", "tgt-prov")
	target.Status.ReportedCapabilities = &infravirtrigaudiov1beta1.ReportedCapabilities{
		SupportedImportBackends: []string{"pvc", "s3", "nfs"},
		SupportedTransferModes:  []string{"relay"},
	}
	r := &VMMigrationReconciler{}
	migrationWith := func(backend string) *infravirtrigaudiov1beta1.VMMigration {
		m := &infravirtrigaudiov1beta1.VMMigration{}
		if backend != "" {
			m.Spec.Storage = &infravirtrigaudiov1beta1.MigrationStorage{Type: backend}
		}
		return m
	}

	for _, backend := range []string{"s3", "nfs"} {
		assert.Empty(t, r.gateMigrationStorageBackend(migrationWith(backend), clustered, target),
			"a clustered source exports through %s", backend)
	}
	for name, src := range map[string]*infravirtrigaudiov1beta1.Provider{"slice 3 provider": clustered, "older clustered provider": olderClustered} {
		for _, backend := range []string{"pvc", ""} {
			msg := r.gateMigrationStorageBackend(migrationWith(backend), src, target)
			assert.Contains(t, msg, "clustered provider", "%s, backend %q: pvc is refused up front", name, backend)
		}
	}
}

// TestHandleValidatingPhase_ClusteredBoundSourcePassesTheRoutingGate: a bound
// clustered source is no longer waited for or refused by the routing gate.
func TestHandleValidatingPhase_ClusteredBoundSourcePassesTheRoutingGate(t *testing.T) {
	r, migration := clusteredMigrationFixture(t, true, false, &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-a"})
	_, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	got := getMigration(t, r)
	assert.NotContains(t, got.Status.Message, "host binding", "a bound clustered source is addressable")
	assert.Contains(t, got.Status.Message, "clustered provider", "and a pvc export from it is refused with an honest message")
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseFailed, got.Status.Phase)
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

// TestGateRoutedImport_CrossNamespaceTargetRefused: a clustered target VM in
// another namespace could never prove its landed disk (isOwnMigrationDisk
// looks the migration up in the VM's namespace), so it is refused up front.
func TestGateRoutedImport_CrossNamespaceTargetRefused(t *testing.T) {
	r, migration := clusteredMigrationFixture(t, false, true, nil)
	migration.Spec.Target.Namespace = "team-b"
	called := false
	r.providerInstanceFn = func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
		called = true
		return &capsStub{caps: contracts.Capabilities{SupportsRoutedImport: true}}, nil
	}
	proceed, _, err := r.gateRoutedImport(context.Background(), migration, readyProvider("default", "tgt-prov"))
	require.NoError(t, err)
	assert.False(t, proceed)
	assert.False(t, called, "refused before any provider call")
	got := getMigration(t, r)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Equal(t, clusteredTargetCrossNamespaceMessage, got.Status.Message)
}
