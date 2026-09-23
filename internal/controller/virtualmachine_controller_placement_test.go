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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// ─── recordingCreateProvider ──────────────────────────────────────────────────

// recordingCreateProvider embeds stubProvider and records every Create: whether
// it was called, the request it received (so a test can assert TargetHostID), and
// returns a configurable response/error. This is the ADR-0007 binding-controller
// analogue of deleteStubProvider.
type recordingCreateProvider struct {
	stubProvider
	createCalls int
	lastReq     contracts.CreateRequest
	resp        contracts.CreateResponse
	err         error
}

func (p *recordingCreateProvider) Create(_ context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	p.createCalls++
	p.lastReq = req
	return p.resp, p.err
}

// ─── builders ─────────────────────────────────────────────────────────────────

func i32p(v int32) *int32 { return &v }
func i64p(v int64) *int64 { return &v }

// clusteredProviderCR returns a Provider CR with topology=cluster.
func clusteredProviderCR(name, ns string) *infravirtrigaudiov1beta1.Provider {
	return &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.ProviderSpec{
			Type:     infravirtrigaudiov1beta1.ProviderTypeLibvirt,
			Topology: infravirtrigaudiov1beta1.ProviderTopologyCluster,
		},
	}
}

// singleProviderCR returns a Provider CR with the default (single) topology.
func singleProviderCR(name, ns string) *infravirtrigaudiov1beta1.Provider {
	return &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.ProviderSpec{
			Type: infravirtrigaudiov1beta1.ProviderTypeLibvirt,
			// Topology left empty → single (the default).
		},
	}
}

func hostPoolCR(name, ns, providerName string) *infravirtrigaudiov1beta1.HostPool {
	return &infravirtrigaudiov1beta1.HostPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.HostPoolSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: providerName},
			Strategy:    infravirtrigaudiov1beta1.PoolStrategySpread,
		},
	}
}

// readyHost returns a schedulable, Ready host with generous allocatable capacity.
func readyHost(name, ns, poolName, providerName string) *infravirtrigaudiov1beta1.Host {
	return &infravirtrigaudiov1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.HostSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: providerName},
			PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: poolName},
			Schedulable: true,
		},
		Status: infravirtrigaudiov1beta1.HostStatus{
			Health:               infravirtrigaudiov1beta1.HostHealthReady,
			AllocatableCPU:       i32p(8),
			AllocatableMemoryMiB: i64p(16384),
		},
	}
}

// clusterVM returns a VM pointing at a clustered provider, with an ImageRef so
// createVM's imageRef-XOR-importedDisk validation passes.
func clusterVM(name, ns, providerName string) *infravirtrigaudiov1beta1.VirtualMachine {
	return &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: providerName},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "test-class"},
			ImageRef:    &infravirtrigaudiov1beta1.ObjectRef{Name: "test-image"},
		},
	}
}

func smallVMClass(ns string) *infravirtrigaudiov1beta1.VMClass {
	return &infravirtrigaudiov1beta1.VMClass{
		ObjectMeta: metav1.ObjectMeta{Name: "test-class", Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VMClassSpec{
			CPU:    2,
			Memory: resource.MustParse("4Gi"),
		},
	}
}

func minimalVMImage(ns string) *infravirtrigaudiov1beta1.VMImage {
	return &infravirtrigaudiov1beta1.VMImage{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image", Namespace: ns},
	}
}

// provisioningReason returns the reason of the Provisioning condition, or "".
func provisioningReason(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if c := k8s.GetCondition(vm.Status.Conditions, k8s.ConditionProvisioning); c != nil {
		return c.Reason
	}
	return ""
}

// ─── single-host parity ───────────────────────────────────────────────────────

// TestCreateVM_SingleHost_NoScheduling proves the single-host path is a true
// no-op for scheduling: no HostPool/Host is consulted, TargetHostID stays empty,
// and status.placement is never written — byte-for-byte today's behavior.
func TestCreateVM_SingleHost_NoScheduling(t *testing.T) {
	const ns = "default"
	s := coverageTestScheme(t)
	providerCR := singleProviderCR("prov-single", ns)
	vm := clusterVM("vm-single", ns, providerCR.Name)
	vmClass := smallVMClass(ns)
	vmImage := minimalVMImage(ns)

	prov := &recordingCreateProvider{resp: contracts.CreateResponse{ID: "vm-100"}}
	// Deliberately seed NO HostPool/Host: the single-host path must not need them.
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, providerCR, vmClass, vmImage)

	res, err := r.createVM(context.Background(), vm, prov, providerCR, vmClass, vmImage, nil)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, res.RequeueAfter)
	require.Equal(t, 1, prov.createCalls, "single-host create must still happen")
	assert.Empty(t, prov.lastReq.TargetHostID, "single-host create must not carry a target host")
	assert.Nil(t, vm.Status.Placement, "single-host VM must never get a placement binding")
	assert.Equal(t, "vm-100", vm.Status.ID)
}

// ─── clustered happy path ─────────────────────────────────────────────────────

// TestCreateVM_Clustered_HappyPath schedules a VM onto a HostPool host, threads
// the chosen host as TargetHostID, and writes status.placement AFTER Create.
func TestCreateVM_Clustered_HappyPath(t *testing.T) {
	const ns = "default"
	s := coverageTestScheme(t)
	providerCR := clusteredProviderCR("prov-cluster", ns)
	pool := hostPoolCR("pool-a", ns, providerCR.Name)
	host := readyHost("host-alpha", ns, pool.Name, providerCR.Name)
	vm := clusterVM("vm-clustered", ns, providerCR.Name)
	vmClass := smallVMClass(ns)
	vmImage := minimalVMImage(ns)

	prov := &recordingCreateProvider{resp: contracts.CreateResponse{ID: "vm-200"}}
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, providerCR, pool, host, vmClass, vmImage)

	res, err := r.createVM(context.Background(), vm, prov, providerCR, vmClass, vmImage, nil)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, res.RequeueAfter)

	require.Equal(t, 1, prov.createCalls, "clustered create must happen after scheduling")
	assert.Equal(t, "host-alpha", prov.lastReq.TargetHostID, "chosen host must be threaded to the wire")

	require.NotNil(t, vm.Status.Placement, "placement binding must be written after Create success")
	assert.Equal(t, "host-alpha", vm.Status.Placement.Host)
	assert.Equal(t, "pool-a", vm.Status.Placement.Pool)
	assert.NotEmpty(t, vm.Status.Placement.Reason, "decision trace must be recorded")
	require.NotNil(t, vm.Status.Placement.LastScheduledTime)
	assert.False(t, vm.Status.Placement.LastScheduledTime.IsZero())
	assert.Equal(t, "vm-200", vm.Status.ID)
}

// ─── honesty-first: Create fails → no placement written ───────────────────────

// TestCreateVM_Clustered_HonestyFirst_CreateErrorLeavesPlacementUnset proves
// ADR-0007 D3: when Create fails, status.placement is NOT written (it must never
// claim a host the provider has not accepted the VM on).
func TestCreateVM_Clustered_HonestyFirst_CreateErrorLeavesPlacementUnset(t *testing.T) {
	const ns = "default"
	s := coverageTestScheme(t)
	providerCR := clusteredProviderCR("prov-cluster", ns)
	pool := hostPoolCR("pool-a", ns, providerCR.Name)
	host := readyHost("host-alpha", ns, pool.Name, providerCR.Name)
	vm := clusterVM("vm-fail", ns, providerCR.Name)
	vmClass := smallVMClass(ns)
	vmImage := minimalVMImage(ns)

	prov := &recordingCreateProvider{err: stderrors.New("libvirt: define failed on host-alpha")}
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, providerCR, pool, host, vmClass, vmImage)

	res, err := r.createVM(context.Background(), vm, prov, providerCR, vmClass, vmImage, nil)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, res.RequeueAfter)

	require.Equal(t, 1, prov.createCalls, "scheduling succeeded, so Create must have been attempted")
	assert.Equal(t, "host-alpha", prov.lastReq.TargetHostID, "the VM was scheduled before Create was attempted")
	assert.Nil(t, vm.Status.Placement, "honesty-first: a failed Create must leave placement unwritten")
	assert.Empty(t, vm.Status.ID, "no VM id on a failed create")
	assert.Equal(t, k8s.ReasonProviderError, provisioningReason(vm))
}

// ─── not-schedulable / misconfigured: Create must NOT be called ───────────────

// TestCreateVM_Clustered_NotSchedulable is the table of clustered-create outcomes
// that must set a Provisioning=False condition and requeue WITHOUT calling Create.
func TestCreateVM_Clustered_NotSchedulable(t *testing.T) {
	const ns = "default"

	cases := []struct {
		name        string
		extra       func(providerName string) []client.Object // pools/hosts/policies beyond the VM
		mutateVM    func(vm *infravirtrigaudiov1beta1.VirtualMachine)
		wantReason  string
		wantRequeue time.Duration
	}{
		{
			name:        "no HostPool",
			extra:       func(string) []client.Object { return nil },
			wantReason:  k8s.ReasonNoHostPool,
			wantRequeue: placementConfigRetryInterval,
		},
		{
			name: "multiple HostPools",
			extra: func(providerName string) []client.Object {
				return []client.Object{
					hostPoolCR("pool-a", ns, providerName),
					hostPoolCR("pool-b", ns, providerName),
				}
			},
			wantReason:  k8s.ReasonMultipleHostPools,
			wantRequeue: placementConfigRetryInterval,
		},
		{
			name: "no feasible host (all NotReady)",
			extra: func(providerName string) []client.Object {
				pool := hostPoolCR("pool-a", ns, providerName)
				host := readyHost("host-alpha", ns, pool.Name, providerName)
				host.Status.Health = infravirtrigaudiov1beta1.HostHealthNotReady
				return []client.Object{pool, host}
			},
			wantReason:  k8s.ReasonUnschedulable,
			wantRequeue: placementUnschedulableRetryInterval,
		},
		{
			name: "dangling placement policy",
			extra: func(providerName string) []client.Object {
				pool := hostPoolCR("pool-a", ns, providerName)
				host := readyHost("host-alpha", ns, pool.Name, providerName)
				return []client.Object{pool, host}
			},
			mutateVM: func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
				vm.Spec.PlacementRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: "missing-policy"}
			},
			wantReason:  k8s.ReasonPlacementPolicyNotFound,
			wantRequeue: placementConfigRetryInterval,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := coverageTestScheme(t)
			providerCR := clusteredProviderCR("prov-cluster", ns)
			vm := clusterVM("vm-block", ns, providerCR.Name)
			if tc.mutateVM != nil {
				tc.mutateVM(vm)
			}
			vmClass := smallVMClass(ns)
			vmImage := minimalVMImage(ns)

			objs := []client.Object{vm, providerCR, vmClass, vmImage}
			objs = append(objs, tc.extra(providerCR.Name)...)

			prov := &recordingCreateProvider{resp: contracts.CreateResponse{ID: "should-not-be-used"}}
			r := newTestReconciler(s, &stubResolver{provider: prov}, objs...)

			res, err := r.createVM(context.Background(), vm, prov, providerCR, vmClass, vmImage, nil)
			require.NoError(t, err)
			assert.Equal(t, 0, prov.createCalls, "Create must NOT be called when the VM cannot be scheduled")
			assert.Nil(t, vm.Status.Placement, "no placement binding when scheduling did not succeed")
			assert.Empty(t, vm.Status.ID)
			assert.Equal(t, tc.wantReason, provisioningReason(vm))
			assert.Equal(t, tc.wantRequeue, res.RequeueAfter)
		})
	}
}

// ─── idempotent re-selection (D4) ─────────────────────────────────────────────

// TestCreateVM_Clustered_IdempotentReselection proves ADR-0007 D4: a VM already
// bound (status.placement.host) to a still-feasible host re-selects THAT host even
// when the deterministic tie-break would otherwise pick a different one. Both
// hosts are identical and feasible; the ascending-id tie-break alone would pick
// host-alpha, so choosing the pre-bound host-bravo proves idempotency, not luck.
func TestCreateVM_Clustered_IdempotentReselection(t *testing.T) {
	const ns = "default"
	s := coverageTestScheme(t)
	providerCR := clusteredProviderCR("prov-cluster", ns)
	pool := hostPoolCR("pool-a", ns, providerCR.Name)
	hostA := readyHost("host-alpha", ns, pool.Name, providerCR.Name)
	hostB := readyHost("host-bravo", ns, pool.Name, providerCR.Name)

	vm := clusterVM("vm-rebind", ns, providerCR.Name)
	// Pre-bind to host-bravo (the host the tie-break would NOT choose), with an
	// empty Status.ID as on the recreate path.
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{
		Host: "host-bravo",
		Pool: pool.Name,
	}
	vmClass := smallVMClass(ns)
	vmImage := minimalVMImage(ns)

	prov := &recordingCreateProvider{resp: contracts.CreateResponse{ID: "vm-300"}}
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, providerCR, pool, hostA, hostB, vmClass, vmImage)

	_, err := r.createVM(context.Background(), vm, prov, providerCR, vmClass, vmImage, nil)
	require.NoError(t, err)

	require.Equal(t, 1, prov.createCalls)
	assert.Equal(t, "host-bravo", prov.lastReq.TargetHostID, "idempotency must re-select the current binding")
	require.NotNil(t, vm.Status.Placement)
	assert.Equal(t, "host-bravo", vm.Status.Placement.Host)
	assert.Equal(t, "pool-a", vm.Status.Placement.Pool)
}

// ─── requiredNetworksForScheduling (pure) ─────────────────────────────────────

func TestRequiredNetworksForScheduling(t *testing.T) {
	libvirtNet := func(networkName, bridge string) *infravirtrigaudiov1beta1.VMNetworkAttachment {
		cfg := &infravirtrigaudiov1beta1.LibvirtNetworkConfig{NetworkName: networkName}
		if bridge != "" {
			cfg.Bridge = &infravirtrigaudiov1beta1.BridgeConfig{Name: bridge}
		}
		return &infravirtrigaudiov1beta1.VMNetworkAttachment{
			Spec: infravirtrigaudiov1beta1.VMNetworkAttachmentSpec{
				Network: infravirtrigaudiov1beta1.NetworkConfig{Libvirt: cfg},
			},
		}
	}
	vsphereNet := &infravirtrigaudiov1beta1.VMNetworkAttachment{
		Spec: infravirtrigaudiov1beta1.VMNetworkAttachmentSpec{
			Network: infravirtrigaudiov1beta1.NetworkConfig{
				VSphere: &infravirtrigaudiov1beta1.VSphereNetworkConfig{Portgroup: "pg-1"},
			},
		},
	}

	cases := []struct {
		name string
		in   []*infravirtrigaudiov1beta1.VMNetworkAttachment
		want []string
	}{
		{name: "nil slice", in: nil, want: nil},
		{name: "nil entry (template NIC) contributes nothing", in: []*infravirtrigaudiov1beta1.VMNetworkAttachment{nil}, want: nil},
		{name: "prefers network name", in: []*infravirtrigaudiov1beta1.VMNetworkAttachment{libvirtNet("vlan100", "br-vlan100")}, want: []string{"vlan100"}},
		{name: "falls back to bridge when no network name", in: []*infravirtrigaudiov1beta1.VMNetworkAttachment{libvirtNet("", "br-vlan100")}, want: []string{"br-vlan100"}},
		{name: "non-libvirt attachment contributes nothing", in: []*infravirtrigaudiov1beta1.VMNetworkAttachment{vsphereNet}, want: nil},
		{name: "empty libvirt identity contributes nothing", in: []*infravirtrigaudiov1beta1.VMNetworkAttachment{libvirtNet("", "")}, want: nil},
		{
			name: "collapses duplicates, keeps order",
			in: []*infravirtrigaudiov1beta1.VMNetworkAttachment{
				libvirtNet("vlan100", ""),
				libvirtNet("vlan200", ""),
				libvirtNet("vlan100", ""),
			},
			want: []string{"vlan100", "vlan200"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, requiredNetworksForScheduling(tc.in))
		})
	}
}
