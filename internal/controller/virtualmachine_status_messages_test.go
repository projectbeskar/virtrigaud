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
	stderrors "errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a VirtualMachine's status never carries what the
// manager could not do to reach its Provider (ADR-0007 A6, threat 5): the
// resolver's errors (TLS Secret, Service address, runtime phase) and the
// gRPC transport's (the dial target) go to the manager log only.

// errResolverTLS is a resolver error of the kind internal/runtime/remote
// returns: it names the TLS Secret and the provider Service's address.
var errResolverTLS = stderrors.New("failed to build TLS config: get TLS Secret infra/provider-mtls: " +
	"secrets \"provider-mtls\" not found; endpoint 10.96.12.7:9443")

// transportUnavailable is what the transport client returns for a call whose
// connection was refused: a Retryable provider error wrapping the gRPC
// Unavailable status, whose text names the dial target.
func transportUnavailable(op string) error {
	st := status.New(codes.Unavailable, "connection error: desc = \"transport: Error while dialing: "+
		"dial tcp 10.96.12.7:9443: connect: connection refused\"")
	return contracts.NewRetryableError(fmt.Sprintf("%s: %s", op, st.Message()), st.Err())
}

// boundSingleHostVM seeds a bound single-host VM with its Provider and class
// and returns the reconciler and the VM as stored.
func boundSingleHostVM(t *testing.T, resolver ProviderResolver) (*VirtualMachineReconciler, *infravirtrigaudiov1beta1.VirtualMachine) {
	t.Helper()
	prov, class := providerAndClass("default")
	vm := baseVM("default")
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Status.ID = "vm-100"
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: prov.Name}
	r := newTestReconciler(coverageTestScheme(t), resolver, prov, class, vm)
	stored := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(vm), stored))
	return r, stored
}

func TestReconcileVM_ResolverErrorStaysOutOfStatus(t *testing.T) {
	ctx := context.Background()
	resolver := &stubResolver{err: errResolverTLS}
	r, vm := boundSingleHostVM(t, resolver)

	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)

	held := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), held))
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonProviderError, ready.Reason)
	assert.Equal(t, providerResolveFailedMessage, ready.Message, "a constant message")
	for _, leak := range []string{"provider-mtls", "10.96.12.7", "9443", "infra/"} {
		assert.NotContains(t, ready.Message, leak)
	}

	// Another resolver error leaves the status as it is.
	resolver.err = stderrors.New("remote provider runtime is not ready: phase=Pending")
	_, err = r.reconcileVM(ctx, held.DeepCopy())
	require.NoError(t, err)
	again := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), again))
	assert.True(t, equality.Semantic.DeepEqual(held.Status, again.Status), "a repeated failure leaves the status unchanged")
}

func TestReconcileVM_TransportErrorStaysOutOfStatus(t *testing.T) {
	ctx := context.Background()
	prov := &fakeDescribeProvider{DescribeFn: func(context.Context, string) (contracts.DescribeResponse, error) {
		return contracts.DescribeResponse{}, transportUnavailable("describe")
	}}
	r, vm := boundSingleHostVM(t, &stubResolver{provider: prov})

	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	held := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), held))
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, "Failed to describe VM: "+providerNotReachedDetail, ready.Message)
	assert.NotContains(t, ready.Message, "10.96.12.7")
}

func TestProviderFailureDetail(t *testing.T) {
	answered := contracts.NewNotFoundError("describe: VM vm-100 not found", status.Error(codes.NotFound, "VM vm-100 not found"))
	cases := []struct {
		name      string
		err       error
		transport bool
	}{
		{name: "transport Unavailable (wrapped)", err: transportUnavailable("power"), transport: true},
		{name: "bare gRPC DeadlineExceeded", err: status.Error(codes.DeadlineExceeded, "context deadline exceeded"), transport: true},
		{name: "context deadline", err: fmt.Errorf("describe failed: %w", context.DeadlineExceeded), transport: true},
		{name: "provider error typed Unavailable", err: &contracts.ProviderError{Type: contracts.ErrorTypeUnavailable, Message: "down"}, transport: true},
		{name: "provider answered NotFound", err: answered, transport: false},
		{name: "provider answered Internal", err: fmt.Errorf("create failed: %s", status.Error(codes.Internal, "template missing").Error()), transport: false},
		{name: "marked HOST_UNAVAILABLE is the provider's answer", err: contracts.NewHostUnavailableError("host down", nil), transport: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.transport, isProviderTransportFailure(tc.err))
			if tc.transport {
				assert.Equal(t, providerNotReachedDetail, providerFailureDetail(tc.err))
			} else {
				assert.NotEqual(t, providerNotReachedDetail, providerFailureDetail(tc.err))
			}
		})
	}
	assert.False(t, isProviderTransportFailure(nil))
}
