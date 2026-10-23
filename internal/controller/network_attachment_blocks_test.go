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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the R36 review finding M1: a VMNetworkAttachment with a block
// for several hypervisors must send only the block of the target Provider's
// type. Before, a vSphere VLAN or a libvirt bridge leaked into a Proxmox NIC.

// multiBlockAttachment is an attachment with all three blocks, each with
// values no other block shares.
func multiBlockAttachment() *infravirtrigaudiov1beta1.VMNetworkAttachment {
	slot := int32(192)
	vlan := int32(100)
	tag := int32(30)
	return &infravirtrigaudiov1beta1.VMNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VMNetworkAttachmentSpec{
			Network: infravirtrigaudiov1beta1.NetworkConfig{
				VSphere: &infravirtrigaudiov1beta1.VSphereNetworkConfig{
					Portgroup:     "pg-prod",
					VLAN:          &infravirtrigaudiov1beta1.VLANConfig{Type: "vlan", VlanID: &vlan},
					PCISlotNumber: &slot,
				},
				Libvirt: &infravirtrigaudiov1beta1.LibvirtNetworkConfig{
					NetworkName: "lv-net",
					Bridge:      &infravirtrigaudiov1beta1.BridgeConfig{Name: "br0"},
					Model:       "e1000",
				},
				Proxmox: &infravirtrigaudiov1beta1.ProxmoxNetworkConfig{
					Bridge:  "vmbr3",
					Model:   "vmxnet3",
					VLANTag: &tag,
				},
			},
		},
	}
}

func TestApplyNetworkBlocks_OnlyTheTargetsBlock(t *testing.T) {
	slot := int32(192)
	tests := []struct {
		name   string
		target infravirtrigaudiov1beta1.ProviderType
		want   contracts.NetworkAttachment
	}{
		{"vsphere", infravirtrigaudiov1beta1.ProviderTypeVSphere,
			contracts.NetworkAttachment{NetworkName: "pg-prod", VLAN: 100, PCISlotNumber: &slot}},
		{"libvirt", infravirtrigaudiov1beta1.ProviderTypeLibvirt,
			contracts.NetworkAttachment{NetworkName: "lv-net", Bridge: "br0", Model: "e1000"}},
		{"proxmox", infravirtrigaudiov1beta1.ProviderTypeProxmox,
			contracts.NetworkAttachment{Bridge: "vmbr3", Model: "vmxnet3", VLAN: 30}},
		// A type with no block of its own, or an unknown one, reads all of them in
		// the old order: the later block wins a shared field.
		{"qemu", infravirtrigaudiov1beta1.ProviderTypeQEMU,
			contracts.NetworkAttachment{NetworkName: "lv-net", Bridge: "vmbr3", Model: "vmxnet3", VLAN: 30, PCISlotNumber: &slot}},
		{"unknown", "",
			contracts.NetworkAttachment{NetworkName: "lv-net", Bridge: "vmbr3", Model: "vmxnet3", VLAN: 30, PCISlotNumber: &slot}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got contracts.NetworkAttachment
			applyNetworkBlocks(&got, multiBlockAttachment(), tc.target)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestApplyNetworkBlocks_ProxmoxWithoutItsBlockSendsNothing: a libvirt-only or
// vSphere-only attachment referenced by a Proxmox VM sends no bridge, VLAN or
// model (the provider's defaults apply), and a Proxmox block without a VLAN tag
// sends no VLAN even when the attachment has a vSphere one.
func TestApplyNetworkBlocks_ProxmoxWithoutItsBlockSendsNothing(t *testing.T) {
	net := multiBlockAttachment()
	net.Spec.Network.Proxmox = nil
	var got contracts.NetworkAttachment
	applyNetworkBlocks(&got, net, infravirtrigaudiov1beta1.ProviderTypeProxmox)
	assert.Equal(t, contracts.NetworkAttachment{}, got)

	net = multiBlockAttachment()
	net.Spec.Network.Proxmox.VLANTag = nil
	got = contracts.NetworkAttachment{}
	applyNetworkBlocks(&got, net, infravirtrigaudiov1beta1.ProviderTypeProxmox)
	assert.Equal(t, contracts.NetworkAttachment{Bridge: "vmbr3", Model: "vmxnet3"}, got, "the vSphere VLAN 100 does not leak")
}

// TestBuildCreateRequest_NetworksFollowTheProviderType: the same VM and
// attachment, built for a Provider of each type.
func TestBuildCreateRequest_NetworksFollowTheProviderType(t *testing.T) {
	s := cloudInitScheme(t)
	r := &VirtualMachineReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "c"},
			Networks: []infravirtrigaudiov1beta1.VMNetworkRef{{
				Name:       "lan",
				NetworkRef: &infravirtrigaudiov1beta1.ObjectRef{Name: "multi"},
				IPAddress:  "10.0.0.5", Prefix: 24, Gateway: "10.0.0.1", DNS: "10.0.0.2",
			}},
		},
	}
	vmClass := &infravirtrigaudiov1beta1.VMClass{
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}

	for _, tc := range []struct {
		target                infravirtrigaudiov1beta1.ProviderType
		bridge, model, portgp string
		vlan                  int32
	}{
		{infravirtrigaudiov1beta1.ProviderTypeVSphere, "", "", "pg-prod", 100},
		{infravirtrigaudiov1beta1.ProviderTypeLibvirt, "br0", "e1000", "lv-net", 0},
		{infravirtrigaudiov1beta1.ProviderTypeProxmox, "vmbr3", "vmxnet3", "", 30},
	} {
		t.Run(string(tc.target), func(t *testing.T) {
			provider := &infravirtrigaudiov1beta1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
				Spec:       infravirtrigaudiov1beta1.ProviderSpec{Type: tc.target},
			}
			req, err := r.buildCreateRequest(context.Background(), vm, provider, vmClass, nil,
				[]*infravirtrigaudiov1beta1.VMNetworkAttachment{multiBlockAttachment()})
			require.NoError(t, err)
			require.Len(t, req.Networks, 1)
			n := req.Networks[0]
			assert.Equal(t, tc.bridge, n.Bridge)
			assert.Equal(t, tc.model, n.Model)
			assert.Equal(t, tc.portgp, n.NetworkName)
			assert.Equal(t, tc.vlan, n.VLAN)
			assert.Equal(t, "10.0.0.5", n.StaticIP, "the VM's own IP settings are sent for every type")
			assert.Equal(t, int32(24), n.Prefix)
		})
	}
}
