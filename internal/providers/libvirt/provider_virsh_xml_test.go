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

package libvirt

import (
	"encoding/xml"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// --- generateNetworkInterfacesXML -----------------------------------------
//
// Golden cases pin the exact byte output for benign (non-metacharacter) input
// to the historical template (issue #260's fix is a no-op for such input —
// see TestXMLEscape_Benign). Adversarial cases assert the escaped document
// still parses as well-formed XML with exactly the expected element count
// (no sibling element injected) and that the malicious value round-trips
// intact through the attribute.

// TestGenerateNetworkInterfacesXML_Golden_NoNetworks pins the zero-networks
// default (a single NAT 'user' interface).
func TestGenerateNetworkInterfacesXML_Golden_NoNetworks(t *testing.T) {
	p := &Provider{}
	got := p.generateNetworkInterfacesXML(nil)
	want := `    <interface type='user'>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x03' function='0x0'/>
    </interface>`
	assert.Equal(t, want, got)
}

// TestGenerateNetworkInterfacesXML_Golden_Bridge pins a single bridge-network
// NIC with a MAC address and the default model.
func TestGenerateNetworkInterfacesXML_Golden_Bridge(t *testing.T) {
	p := &Provider{}
	got := p.generateNetworkInterfacesXML([]contracts.NetworkAttachment{
		{Bridge: "virbr0", MacAddress: "52:54:00:aa:bb:cc"},
	})
	want := `    <interface type='bridge'>
      <mac address='52:54:00:aa:bb:cc'/>
      <source bridge='virbr0'/>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x03' function='0x0'/>
    </interface>`
	assert.Equal(t, want, got)
}

// TestGenerateNetworkInterfacesXML_Golden_ManagedNetwork pins a single
// libvirt-managed-network NIC with an explicit model, no MAC.
func TestGenerateNetworkInterfacesXML_Golden_ManagedNetwork(t *testing.T) {
	p := &Provider{}
	got := p.generateNetworkInterfacesXML([]contracts.NetworkAttachment{
		{NetworkName: "mgmt-net", Model: "e1000"},
	})
	want := `    <interface type='network'>
      <source network='mgmt-net'/>
      <model type='e1000'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x03' function='0x0'/>
    </interface>`
	assert.Equal(t, want, got)
}

// TestGenerateNetworkInterfacesXML_Golden_UserNetwork pins a single
// NAT/'user' NIC produced by an attachment with neither Bridge nor
// NetworkName set (distinct from the nil-slice default: this exercises the
// per-attachment fallback branch).
func TestGenerateNetworkInterfacesXML_Golden_UserNetwork(t *testing.T) {
	p := &Provider{}
	got := p.generateNetworkInterfacesXML([]contracts.NetworkAttachment{{}})
	want := `    <interface type='user'>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x03' function='0x0'/>
    </interface>`
	assert.Equal(t, want, got)
}

// TestGenerateNetworkInterfacesXML_Golden_MultiNIC pins a three-NIC mix
// (bridge, managed network, user) verifying PCI slot increments and the
// blank-line separator between interfaces.
func TestGenerateNetworkInterfacesXML_Golden_MultiNIC(t *testing.T) {
	p := &Provider{}
	got := p.generateNetworkInterfacesXML([]contracts.NetworkAttachment{
		{Bridge: "virbr0"},
		{NetworkName: "mgmt-net", MacAddress: "52:54:00:12:34:56"},
		{},
	})
	want := `    <interface type='bridge'>
      <source bridge='virbr0'/>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x03' function='0x0'/>
    </interface>
    <interface type='network'>
      <mac address='52:54:00:12:34:56'/>
      <source network='mgmt-net'/>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x04' function='0x0'/>
    </interface>
    <interface type='user'>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x0'/>
    </interface>`
	assert.Equal(t, want, got)
}

// networkInterfacesDoc mirrors just enough of the <devices> subtree to assert
// well-formedness and element counts for the adversarial cases below.
type networkInterfacesDoc struct {
	XMLName    xml.Name `xml:"devices"`
	Interfaces []struct {
		Type string `xml:"type,attr"`
		Mac  *struct {
			Address string `xml:"address,attr"`
		} `xml:"mac"`
		Source struct {
			Bridge  string `xml:"bridge,attr"`
			Network string `xml:"network,attr"`
		} `xml:"source"`
		Model struct {
			Type string `xml:"type,attr"`
		} `xml:"model"`
	} `xml:"interface"`
	Disks []struct{} `xml:"disk"`
}

// TestGenerateNetworkInterfacesXML_Adversarial feeds XML-metacharacter and
// attribute-breakout payloads through every CR-derived field (Bridge,
// NetworkName, Model, MacAddress) and asserts: the wrapped document still
// parses as well-formed XML, exactly one <interface> and zero <disk>
// elements exist (no sibling element was spliced in), and each malicious
// value round-trips intact through its attribute (issue #260).
func TestGenerateNetworkInterfacesXML_Adversarial(t *testing.T) {
	const diskInjection = `x'/><disk type='block'><source dev='/dev/sda'/></disk><x a='`

	tests := []struct {
		name string
		net  contracts.NetworkAttachment
	}{
		{"bridge breakout", contracts.NetworkAttachment{Bridge: diskInjection}},
		{"network breakout", contracts.NetworkAttachment{NetworkName: diskInjection}},
		{"model breakout", contracts.NetworkAttachment{Bridge: "virbr0", Model: diskInjection}},
		{"mac breakout", contracts.NetworkAttachment{Bridge: "virbr0", MacAddress: diskInjection}},
		{"ampersand", contracts.NetworkAttachment{Bridge: "a&b"}},
		{"lt", contracts.NetworkAttachment{Bridge: "<"}},
		{"double quote", contracts.NetworkAttachment{Bridge: `a"b`}},
		{"newline", contracts.NetworkAttachment{Bridge: "a\nb"}},
	}

	p := &Provider{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frag := p.generateNetworkInterfacesXML([]contracts.NetworkAttachment{tt.net})
			doc := "<devices>\n" + frag + "\n</devices>"

			var parsed networkInterfacesDoc
			require.NoError(t, xml.Unmarshal([]byte(doc), &parsed), "generated XML must be well-formed: %s", doc)

			require.Len(t, parsed.Interfaces, 1, "exactly one <interface> element expected, no siblings injected")
			assert.Empty(t, parsed.Disks, "no <disk> element must have been injected")

			// The malicious value must round-trip intact through whichever
			// attribute it landed in.
			iface := parsed.Interfaces[0]
			switch {
			case tt.net.Bridge != "":
				assert.Equal(t, tt.net.Bridge, iface.Source.Bridge)
			case tt.net.NetworkName != "":
				assert.Equal(t, tt.net.NetworkName, iface.Source.Network)
			}
			if tt.net.Model != "" {
				assert.Equal(t, tt.net.Model, iface.Model.Type)
			}
			if tt.net.MacAddress != "" {
				require.NotNil(t, iface.Mac)
				assert.Equal(t, tt.net.MacAddress, iface.Mac.Address)
			}
		})
	}
}

// --- buildDiskDevicesXML ----------------------------------------------------

// TestBuildDiskDevicesXML_Golden_DiskOnly pins the primary-disk-only output
// (no cloud-init CD-ROM).
func TestBuildDiskDevicesXML_Golden_DiskOnly(t *testing.T) {
	got := buildDiskDevicesXML("/var/lib/libvirt/images/vm-disk.qcow2", "")
	want := `    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/vm-disk.qcow2'/>
      <target dev='vda' bus='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x07' function='0x0'/>
    </disk>`
	assert.Equal(t, want, got)
}

// TestBuildDiskDevicesXML_Golden_WithCloudInit pins the primary-disk plus
// cloud-init CD-ROM output.
func TestBuildDiskDevicesXML_Golden_WithCloudInit(t *testing.T) {
	got := buildDiskDevicesXML(
		"/var/lib/libvirt/images/vm-disk.qcow2",
		"/var/lib/libvirt/images/vm-cidata.iso",
	)
	want := `    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/vm-disk.qcow2'/>
      <target dev='vda' bus='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x07' function='0x0'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/var/lib/libvirt/images/vm-cidata.iso'/>
      <target dev='hda' bus='ide'/>
      <readonly/>
      <address type='drive' controller='0' bus='0' target='0' unit='0'/>
    </disk>`
	assert.Equal(t, want, got)
}

// diskDevicesDoc mirrors just enough of the <devices> subtree to assert
// well-formedness and element counts for the adversarial cases below.
type diskDevicesDoc struct {
	XMLName xml.Name `xml:"devices"`
	Disks   []struct {
		Device string `xml:"device,attr"`
		Source struct {
			File string `xml:"file,attr"`
		} `xml:"source"`
	} `xml:"disk"`
	Interfaces []struct{} `xml:"interface"`
}

// TestBuildDiskDevicesXML_Adversarial feeds an attribute-breakout payload
// through the disk path and the cloud-init ISO path and asserts the document
// stays well-formed with exactly the expected <disk> count (no sibling
// element injected) and the payload round-trips intact (issue #260).
func TestBuildDiskDevicesXML_Adversarial(t *testing.T) {
	const injection = `x'/><interface type='network'><source network='default'/></interface><x a='`

	tests := []struct {
		name             string
		diskPath         string
		cloudInitISOPath string
		wantDisks        int
	}{
		{"disk path breakout", injection, "", 1},
		{"cloud-init path breakout", "/p/vm-disk.qcow2", injection, 2},
		{"ampersand", "a&b.qcow2", "", 1},
		{"single quote only", "a'b.qcow2", "", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frag := buildDiskDevicesXML(tt.diskPath, tt.cloudInitISOPath)
			doc := "<devices>\n" + frag + "\n</devices>"

			var parsed diskDevicesDoc
			require.NoError(t, xml.Unmarshal([]byte(doc), &parsed), "generated XML must be well-formed: %s", doc)

			require.Len(t, parsed.Disks, tt.wantDisks)
			assert.Empty(t, parsed.Interfaces, "no <interface> element must have been injected")

			assert.Equal(t, tt.diskPath, parsed.Disks[0].Source.File)
			if tt.cloudInitISOPath != "" {
				require.Len(t, parsed.Disks, 2)
				assert.Equal(t, tt.cloudInitISOPath, parsed.Disks[1].Source.File)
			}
		})
	}
}
