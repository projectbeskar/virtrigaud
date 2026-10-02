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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// transportFailure is a provider RPC error as the transport client maps a
// gRPC connection failure (mapGRPCError: Unavailable → a retryable
// *ProviderError wrapping the status): its text names the provider Service's
// address.
func transportFailure(op string) error {
	st := status.New(codes.Unavailable,
		`connection error: desc = "transport: Error while dialing: dial tcp 10.96.12.7:9443: connect: connection refused"`)
	return contracts.NewRetryableError(fmt.Sprintf("%s: %s", op, st.Message()), st.Err())
}

func TestRPCFailureDetail(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		want      string
		wantNotIn []string
	}{
		"transport failure": {err: transportFailure("clone"), want: providerUnreachableDetail, wantNotIn: []string{"10.96.12.7"}},
		"wrapped transport failure": {err: fmt.Errorf("power off source VM: %w", transportFailure("power")),
			want: providerUnreachableDetail},
		"provider answer": {err: contracts.NewRetryableError("clone: datastore busy", nil), want: "clone: datastore busy"},
		"provider Unavailable answer that is not the transport": {
			err:  contracts.NewRetryableError("snapshotDelete: libvirtd restarting", status.Error(codes.Unavailable, "libvirtd restarting")),
			want: "snapshotDelete: libvirtd restarting"},
		"credential in a URL": {err: errors.New("import from https://admin:s3cret@vcenter.example/sdk failed"),
			wantNotIn: []string{"s3cret", "admin:"}},
		"multi-line": {err: errors.New("line one\nline two"), want: "line one line two"},
	} {
		t.Run(name, func(t *testing.T) {
			got := rpcFailureDetail(tc.err)
			if tc.want != "" {
				assert.Equal(t, tc.want, got)
			}
			for _, s := range tc.wantNotIn {
				assert.NotContains(t, got, s)
			}
		})
	}
	assert.False(t, isTransportFailure(nil))
}

// errResolver is a ProviderResolver that always fails with err.
type errResolver struct{ err error }

func (e errResolver) GetProvider(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
	return nil, e.err
}

// cloneDisclosureFixture returns a single-host VMClone of the provisioned VM "src-vm"
// on Provider "prov-1" in namespace default.
func cloneDisclosureFixture() (*infrav1beta1.Provider, *infrav1beta1.VirtualMachine, *infrav1beta1.VMClone) {
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: "default"},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	return runningProvider("default", "prov-1"), sourceVMWithID("default", "src-vm", "prov-1", "vm-1"), clone
}

// requireCloneNoLeak asserts the clone's status, conditions and events carry none
// of leaks, and returns the clone and those texts joined.
func requireCloneNoLeak(t *testing.T, r *VMCloneReconciler, clone *infrav1beta1.VMClone, leaks ...string) (*infrav1beta1.VMClone, string) {
	t.Helper()
	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	texts := append(drainRecordedEvents(fakeRecorderOf(t, r.Recorder)), got.Status.Message)
	for _, c := range got.Status.Conditions {
		texts = append(texts, c.Message)
	}
	joined := strings.Join(texts, "\n")
	for _, leak := range leaks {
		assert.NotContains(t, joined, leak)
	}
	return got, joined
}

// TestVMClone_ResolverErrorNeverReachesStatus: the resolver's error (TLS
// Secret, Service address, runtime phase) goes to the manager log only; the
// clone's consumer-readable status says the provider cannot be reached.
func TestVMClone_ResolverErrorNeverReachesStatus(t *testing.T) {
	prov, src, clone := cloneDisclosureFixture()
	r := newCloneReconciler(cloneTestScheme(t), errResolver{err: errors.New(leakyResolverDetail)}, prov, src, clone)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	got, _ := requireCloneNoLeak(t, r, clone, "10.96.12.7", "provider-tls", "phase Pending")
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Contains(t, got.Status.Message, providerResolveFailedDetail)
}

// TestVMClone_FailedCloneDetailIsSanitized: a Clone RPC that failed in the
// transport (held and retried, its answer in the CloneRetrying event), or
// whose answer carries a credential in a URL (failed), records neither in the
// clone's status or events.
func TestVMClone_FailedCloneDetailIsSanitized(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		leaks     []string
		wantPhase infrav1beta1.ClonePhase
		want      string
	}{
		"transport failure": {err: transportFailure("clone"), leaks: []string{"10.96.12.7", "dial tcp"},
			wantPhase: infrav1beta1.ClonePhasePending, want: providerUnreachableDetail},
		"credential in the answer": {err: contracts.NewInvalidSpecError("clone: fetch https://admin:s3cret@images.example/a.ova failed", nil),
			leaks: []string{"s3cret"}, wantPhase: infrav1beta1.ClonePhaseFailed, want: "images.example/a.ova"},
	} {
		t.Run(name, func(t *testing.T) {
			prov, src, clone := cloneDisclosureFixture()
			cp := &clonerProvider{cloneErr: tc.err}
			r := newCloneReconciler(cloneTestScheme(t), &okResolver{provider: cp}, prov, src, clone)

			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
			got, texts := requireCloneNoLeak(t, r, clone, tc.leaks...)
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			assert.Contains(t, texts, tc.want)
		})
	}
}

// okResolver is a ProviderResolver that always resolves to provider.
type okResolver struct{ provider contracts.Provider }

func (e *okResolver) GetProvider(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
	return e.provider, nil
}

// TestVMMigration_ResolverErrorNeverReachesStatus: a migration that fails
// because its source provider's client cannot be resolved records a constant
// cause; the resolver's error stays in the manager log.
func TestVMMigration_ResolverErrorNeverReachesStatus(t *testing.T) {
	vm, p, m := snapshotFixture()
	m.Status.SnapshotID = ""
	m.Status.Phase = infrav1beta1.MigrationPhaseSnapshotting
	h := newSnapshotHoldHarness(t, &scriptedSnapshotDeleteProvider{}, vm, p, m)
	h.resolveErr = errors.New(leakyResolverDetail)

	_, err := h.r.handleSnapshottingPhase(context.Background(), m)
	require.NoError(t, err)
	got := h.get(t, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Equal(t, "Failed to get provider instance: "+providerResolveFailedDetail, got.Status.Message)
	texts := append(drainRecordedEvents(h.rec), got.Status.Message)
	for _, c := range got.Status.Conditions {
		texts = append(texts, c.Message)
	}
	assertNoResolverText(t, texts...)
}

// TestMigrationFailureDetail pins the VMMigration renderer: a resolve failure
// is replaced, a provider answer is sanitized.
func TestMigrationFailureDetail(t *testing.T) {
	assert.Equal(t, providerResolveFailedDetail,
		migrationFailureDetail(fmt.Errorf("get source provider instance: %w", &providerResolveError{err: errors.New(leakyResolverDetail)})))
	assert.Equal(t, providerUnreachableDetail, migrationFailureDetail(transportFailure("exportDisk")))
	assert.Equal(t, "exportDisk: no space left", migrationFailureDetail(contracts.NewRetryableError("exportDisk: no space left", nil)))
}
