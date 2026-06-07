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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// capReporterProvider is a contracts.Provider that ALSO implements
// contracts.CapabilityReporter, returning a fixed Capabilities set (or a
// fixed error). It embeds stubProvider (defined in
// virtualmachine_controller_test.go) so it satisfies the full
// contracts.Provider interface with no extra boilerplate.
type capReporterProvider struct {
	stubProvider
	caps contracts.Capabilities
	err  error
}

// GetCapabilities makes capReporterProvider a contracts.CapabilityReporter.
func (p *capReporterProvider) GetCapabilities(_ context.Context) (contracts.Capabilities, error) {
	if p.err != nil {
		return contracts.Capabilities{}, p.err
	}
	return p.caps, nil
}

// Compile-time assertions that the test fakes satisfy the relevant
// interfaces. stubProvider is the "not a CapabilityReporter" fail-open fake;
// capReporterProvider is the reporting fake.
var (
	_ contracts.Provider           = (*stubProvider)(nil)
	_ contracts.Provider           = (*capReporterProvider)(nil)
	_ contracts.CapabilityReporter = (*capReporterProvider)(nil)
)

func capGatingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, infrav1beta1.AddToScheme(s))
	return s
}

// readyCondition returns the Ready condition from a list, or nil.
func readyCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// VMSnapshot capability gating
// ---------------------------------------------------------------------------

func TestGateSnapshotCreate(t *testing.T) {
	tests := []struct {
		name           string
		enforce        bool
		provider       contracts.Provider
		includeMemory  bool
		wantBlocked    bool
		wantPhase      infrav1beta1.SnapshotPhase
		wantReadyFalse bool
	}{
		{
			name:        "enforcement off does not block even when unsupported",
			enforce:     false,
			provider:    &capReporterProvider{caps: contracts.Capabilities{SupportsSnapshots: false}},
			wantBlocked: false,
		},
		{
			name:        "enforcement on, snapshots supported, proceeds",
			enforce:     true,
			provider:    &capReporterProvider{caps: contracts.Capabilities{SupportsSnapshots: true}},
			wantBlocked: false,
		},
		{
			name:           "enforcement on, snapshots unsupported, blocks",
			enforce:        true,
			provider:       &capReporterProvider{caps: contracts.Capabilities{SupportsSnapshots: false}},
			wantBlocked:    true,
			wantPhase:      infrav1beta1.SnapshotPhaseFailed,
			wantReadyFalse: true,
		},
		{
			name:    "enforcement on, snapshots supported but memory requested and unsupported, blocks",
			enforce: true,
			provider: &capReporterProvider{caps: contracts.Capabilities{
				SupportsSnapshots:       true,
				SupportsMemorySnapshots: false,
			}},
			includeMemory:  true,
			wantBlocked:    true,
			wantPhase:      infrav1beta1.SnapshotPhaseFailed,
			wantReadyFalse: true,
		},
		{
			name:    "enforcement on, memory requested and supported, proceeds",
			enforce: true,
			provider: &capReporterProvider{caps: contracts.Capabilities{
				SupportsSnapshots:       true,
				SupportsMemorySnapshots: true,
			}},
			includeMemory: true,
			wantBlocked:   false,
		},
		{
			name:        "enforcement on, provider is not a CapabilityReporter, fails open",
			enforce:     true,
			provider:    &stubProvider{}, // does NOT implement CapabilityReporter
			wantBlocked: false,
		},
		{
			name:        "enforcement on, GetCapabilities errors, fails open",
			enforce:     true,
			provider:    &capReporterProvider{err: fmt.Errorf("boom")},
			wantBlocked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := capGatingScheme(t)
			snapshot := &infrav1beta1.VMSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "default"},
			}
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(snapshot).
				WithStatusSubresource(snapshot).
				Build()

			r := &VMSnapshotReconciler{
				Client:              c,
				Scheme:              scheme,
				Recorder:            record.NewFakeRecorder(10),
				EnforceCapabilities: tc.enforce,
			}

			req := contracts.SnapshotCreateRequest{
				VmId:          "vm-1",
				IncludeMemory: tc.includeMemory,
			}

			blocked, _ := r.gateSnapshotCreate(context.Background(), snapshot, tc.provider, req)
			assert.Equal(t, tc.wantBlocked, blocked)

			if tc.wantBlocked {
				assert.Equal(t, tc.wantPhase, snapshot.Status.Phase)
				if tc.wantReadyFalse {
					ready := readyCondition(snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
					require.NotNil(t, ready, "expected Ready condition to be set")
					assert.Equal(t, metav1.ConditionFalse, ready.Status)
					assert.Equal(t, snapshotReasonUnsupportedByProvider, ready.Reason)
				}
			} else {
				// Not blocked: the gate must not have flipped the snapshot to
				// Failed (it leaves status to the normal create path).
				assert.NotEqual(t, infrav1beta1.SnapshotPhaseFailed, snapshot.Status.Phase)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// VMMigration capability gating
// ---------------------------------------------------------------------------

func TestGateProviderCapability(t *testing.T) {
	exportSupported := func(caps contracts.Capabilities) bool { return caps.SupportsDiskExport }
	importSupported := func(caps contracts.Capabilities) bool { return caps.SupportsDiskImport }

	tests := []struct {
		name        string
		enforce     bool
		provider    contracts.Provider
		supported   func(contracts.Capabilities) bool
		wantBlocked bool
	}{
		{
			name:        "enforcement off does not block export even when unsupported",
			enforce:     false,
			provider:    &capReporterProvider{caps: contracts.Capabilities{SupportsDiskExport: false}},
			supported:   exportSupported,
			wantBlocked: false,
		},
		{
			name:        "enforcement on, export supported, proceeds",
			enforce:     true,
			provider:    &capReporterProvider{caps: contracts.Capabilities{SupportsDiskExport: true}},
			supported:   exportSupported,
			wantBlocked: false,
		},
		{
			name:        "enforcement on, export unsupported, blocks",
			enforce:     true,
			provider:    &capReporterProvider{caps: contracts.Capabilities{SupportsDiskExport: false}},
			supported:   exportSupported,
			wantBlocked: true,
		},
		{
			name:        "enforcement on, import unsupported, blocks",
			enforce:     true,
			provider:    &capReporterProvider{caps: contracts.Capabilities{SupportsDiskImport: false}},
			supported:   importSupported,
			wantBlocked: true,
		},
		{
			name:        "enforcement on, import supported, proceeds",
			enforce:     true,
			provider:    &capReporterProvider{caps: contracts.Capabilities{SupportsDiskImport: true}},
			supported:   importSupported,
			wantBlocked: false,
		},
		{
			name:        "enforcement on, provider is not a CapabilityReporter, fails open",
			enforce:     true,
			provider:    &stubProvider{}, // does NOT implement CapabilityReporter
			supported:   exportSupported,
			wantBlocked: false,
		},
		{
			name:        "enforcement on, GetCapabilities errors, fails open",
			enforce:     true,
			provider:    &capReporterProvider{err: fmt.Errorf("boom")},
			supported:   exportSupported,
			wantBlocked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := capGatingScheme(t)
			migration := &infrav1beta1.VMMigration{
				ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: "default"},
			}
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(migration).
				WithStatusSubresource(migration).
				Build()

			r := &VMMigrationReconciler{
				Client:              c,
				Scheme:              scheme,
				Recorder:            record.NewFakeRecorder(10),
				EnforceCapabilities: tc.enforce,
			}

			blocked, _ := r.gateProviderCapability(context.Background(), migration, tc.provider, tc.supported, "unsupported")
			assert.Equal(t, tc.wantBlocked, blocked)

			if tc.wantBlocked {
				// transitionToFailed sets phase + Failed/Ready conditions.
				assert.Equal(t, infrav1beta1.MigrationPhaseFailed, migration.Status.Phase)
				ready := readyCondition(migration.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
				require.NotNil(t, ready, "expected Ready condition to be set")
				assert.Equal(t, metav1.ConditionFalse, ready.Status)
			} else {
				assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, migration.Status.Phase)
			}
		})
	}
}
