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
	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// providerType returns the spec.type of providerCR, "" for a nil Provider.
func providerType(providerCR *infravirtrigaudiov1beta1.Provider) infravirtrigaudiov1beta1.ProviderType {
	if providerCR == nil {
		return ""
	}
	return providerCR.Spec.Type
}

// applyNetworkBlocks copies the target-specific settings of net, a
// VMNetworkAttachment, into attachment, the network sent to the provider.
//
// A VMNetworkAttachment can carry a block per hypervisor
// (spec.network.vsphere, .libvirt, .proxmox). Only the block of the target
// Provider's type is read: a vSphere VLAN or a libvirt bridge in an attachment
// that also has a Proxmox block must not become the Proxmox NIC's tag or
// bridge (R36). A Proxmox target whose attachment has no proxmox block gets no
// bridge, VLAN or model, so the provider's defaults apply. For a Provider type
// with no block of its own (firecracker, qemu) or an unknown one (nil
// Provider), every block is read, as before.
func applyNetworkBlocks(attachment *contracts.NetworkAttachment, net *infravirtrigaudiov1beta1.VMNetworkAttachment, target infravirtrigaudiov1beta1.ProviderType) {
	switch target {
	case infravirtrigaudiov1beta1.ProviderTypeVSphere:
		applyVSphereBlock(attachment, net)
	case infravirtrigaudiov1beta1.ProviderTypeLibvirt:
		applyLibvirtBlock(attachment, net)
	case infravirtrigaudiov1beta1.ProviderTypeProxmox:
		applyProxmoxBlock(attachment, net)
	default:
		applyVSphereBlock(attachment, net)
		applyLibvirtBlock(attachment, net)
		applyProxmoxBlock(attachment, net)
	}
}

// applyVSphereBlock reads net's vSphere block: the portgroup, the VLAN and the
// PCI slot.
func applyVSphereBlock(attachment *contracts.NetworkAttachment, net *infravirtrigaudiov1beta1.VMNetworkAttachment) {
	vs := net.Spec.Network.VSphere
	if vs == nil {
		return
	}
	attachment.NetworkName = vs.Portgroup
	if vs.VLAN != nil && vs.VLAN.VlanID != nil {
		attachment.VLAN = *vs.VLAN.VlanID
	}
	// Pass PCI slot number for predictable interface naming (e.g., ens192)
	if vs.PCISlotNumber != nil {
		attachment.PCISlotNumber = vs.PCISlotNumber
	}
}

// applyLibvirtBlock reads net's libvirt block: the network name, the bridge
// and the NIC model.
func applyLibvirtBlock(attachment *contracts.NetworkAttachment, net *infravirtrigaudiov1beta1.VMNetworkAttachment) {
	lv := net.Spec.Network.Libvirt
	if lv == nil {
		return
	}
	attachment.NetworkName = lv.NetworkName
	if lv.Bridge != nil {
		attachment.Bridge = lv.Bridge.Name
	}
	attachment.Model = lv.Model
}

// applyProxmoxBlock reads net's Proxmox block: the bridge, the NIC model and
// the VLAN tag. (Its firewall, rate limit and MTU are not sent.)
func applyProxmoxBlock(attachment *contracts.NetworkAttachment, net *infravirtrigaudiov1beta1.VMNetworkAttachment) {
	px := net.Spec.Network.Proxmox
	if px == nil {
		return
	}
	attachment.Bridge = px.Bridge
	attachment.Model = px.Model
	if px.VLANTag != nil {
		attachment.VLAN = *px.VLANTag
	}
}
