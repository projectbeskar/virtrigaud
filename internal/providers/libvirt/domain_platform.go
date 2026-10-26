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
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The virtual platform of a new domain: machine type, firmware and the
// platform features a VMClass asks for (docs/libvirt-firmware.md).
//
// A VMClass that asks for nothing below gets the historical i440fx ("pc")
// domain, whose XML is byte-identical to earlier releases. UEFI firmware
// (firmware: UEFI or securityProfile.secureBoot) and an emulated IOMMU
// (securityProfile.vtdEnabled) need the q35 machine type: on q35 the PCI root
// is a pcie-root and there is no IDE controller, so the i440fx device layout —
// pci-root, an IDE CD-ROM, devices at fixed PCI slots — is refused by libvirt
// ("The PCI controller with index='0' must be model='pcie-root' for this
// machine type"). A q35 domain therefore gets its own device layout in which
// libvirt assigns every PCI address itself (adding the pcie-root and the
// pcie-root-ports the devices need), and the cloud-init CD-ROM is on SATA.
//
// Firmware is never named by path. A UEFI domain asks libvirt to pick it
// (<os firmware='efi'>, libvirt >= 7.2 for the <firmware><feature> filters)
// from the firmware descriptors the host's OVMF package installs
// (/usr/share/qemu/firmware/*.json), so the same XML works on Debian/Ubuntu
// (ovmf), Fedora/RHEL (edk2-ovmf) and others:
//
//   - secure boot: a Secure Boot capable build whose variable-store template
//     has the default (Microsoft) keys enrolled, so Secure Boot is enforced
//     from the first boot (a template without enrolled keys boots in setup
//     mode, where Secure Boot is not enforced). SMM is turned on, which a
//     Secure Boot OVMF build requires;
//   - UEFI without secure boot: a build that does not implement Secure Boot.
//
// libvirt creates the domain's own variable store from the template
// (/var/lib/libvirt/qemu/nvram/<domain>_VARS.fd); Delete removes it with the
// domain (undefine --nvram).

const (
	// machineTypeQ35 is the libvirt alias of the newest q35 machine type of
	// the host's QEMU.
	machineTypeQ35 = "q35"
	// machineTypePC is the libvirt alias of the newest i440fx machine type,
	// the historical default.
	machineTypePC = "pc"
	// firmwareUEFI is the VMClass firmware value (case-insensitive) that
	// selects UEFI firmware, as on vSphere.
	firmwareUEFI = "UEFI"
)

// domainPlatform is the virtual platform of a new domain.
type domainPlatform struct {
	// q35 selects the q35 machine type and its device layout
	// (q35DevicesTailXML); otherwise the historical i440fx layout.
	q35 bool
	// uefi asks libvirt for UEFI (OVMF) firmware.
	uefi bool
	// secureBoot asks for Secure Boot firmware with enrolled keys, and SMM.
	secureBoot bool
	// iommu adds an emulated Intel IOMMU (securityProfile.vtdEnabled).
	iommu bool
}

// domainPlatformFor derives the platform of a new domain from its VMClass.
// firmware UEFI (case-insensitive) selects UEFI firmware; securityProfile.
// secureBoot selects UEFI Secure Boot firmware whatever firmware says (as on
// vSphere); any other firmware value — BIOS, EFI, or none — keeps BIOS
// (SeaBIOS) firmware, as on vSphere. UEFI and an emulated IOMMU need the q35
// machine type.
func domainPlatformFor(class contracts.VMClass) domainPlatform {
	var pl domainPlatform
	if class.SecurityProfile != nil {
		pl.secureBoot = class.SecurityProfile.SecureBoot
		pl.iommu = class.SecurityProfile.VTDEnabled
	}
	pl.uefi = pl.secureBoot || strings.EqualFold(strings.TrimSpace(class.Firmware), firmwareUEFI)
	pl.q35 = pl.uefi || pl.iommu
	return pl
}

// machineType is the <type machine='...'> of the platform.
func (pl domainPlatform) machineType() string {
	if pl.q35 {
		return machineTypeQ35
	}
	return machineTypePC
}

// osXML is the content of the domain's <os> element, and the <os> start tag
// itself (with the firmware attribute for UEFI).
func (pl domainPlatform) osXML() (startTag, content string) {
	var b strings.Builder
	b.WriteString("    <type arch='x86_64' machine='" + pl.machineType() + "'>hvm</type>\n")
	startTag = "<os>"
	if pl.uefi {
		startTag = "<os firmware='efi'>"
		if pl.secureBoot {
			b.WriteString(`    <firmware>
      <feature enabled='yes' name='secure-boot'/>
      <feature enabled='yes' name='enrolled-keys'/>
    </firmware>
    <loader secure='yes'/>
`)
		} else {
			b.WriteString(`    <firmware>
      <feature enabled='no' name='secure-boot'/>
    </firmware>
`)
		}
	}
	b.WriteString(`    <boot dev='hd'/>
    <boot dev='cdrom'/>`)
	return startTag, b.String()
}

// featuresXML is the content of the domain's <features> element.
func (pl domainPlatform) featuresXML() string {
	x := `    <acpi/>
    <apic/>`
	if pl.secureBoot {
		// A Secure Boot OVMF build requires SMM; libvirt refuses
		// <loader secure='yes'> without it.
		x += `
    <smm state='on'/>`
	}
	if pl.iommu {
		// Interrupt remapping (intremap on the <iommu> device) needs the
		// split I/O APIC, emulated by QEMU.
		x += `
    <ioapic driver='qemu'/>`
	}
	return x
}

// cpuXML is the domain's <cpu> element. Nested virtualization passes the
// host CPU through (host-passthrough), so the guest sees VT-x (Intel) or
// AMD-V (AMD) whichever the host has — when the host's kvm_intel / kvm_amd
// module has nested virtualization enabled. Requiring both vmx and svm, as
// earlier releases did, cannot be met by any host CPU.
func cpuXML(nested bool) string {
	if nested {
		return `<cpu mode='host-passthrough' check='none'/>`
	}
	return `<cpu mode='host-model' check='partial'></cpu>`
}

// q35DevicesTailXML is the q35 device layout that follows the disks in
// <devices>: no PCI addresses (libvirt assigns them on pcie-root and the
// pcie-root-ports it adds), a USB 3 controller for the tablet, the virtio
// serial channel of the guest agent, the network interfaces, the serial
// console, VNC on localhost, standard VGA (which OVMF drives) and the memory
// balloon; then the TPM and IOMMU devices when asked for.
func (pl domainPlatform) q35DevicesTailXML(interfacesXML string, tpm bool) string {
	var b strings.Builder
	b.WriteString(`    <controller type='usb' index='0' model='qemu-xhci'/>
    <controller type='virtio-serial' index='0'/>
`)
	b.WriteString(interfacesXML)
	b.WriteString(`
    <serial type='pty'>
      <target type='isa-serial' port='0'>
        <model name='isa-serial'/>
      </target>
    </serial>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
      <address type='virtio-serial' controller='0' bus='0' port='1'/>
    </channel>
    <input type='tablet' bus='usb'/>
    <input type='mouse' bus='ps2'/>
    <input type='keyboard' bus='ps2'/>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <video>
      <model type='vga' vram='16384' heads='1' primary='yes'/>
    </video>
    <memballoon model='virtio'/>`)
	if tpm {
		b.WriteString("\n" + tpmDeviceXML)
	}
	if pl.iommu {
		b.WriteString(`
    <iommu model='intel'>
      <driver intremap='on'/>
    </iommu>`)
	}
	return b.String()
}

// tpmDeviceXML is the emulated TPM 2.0 device (swtpm on the host).
const tpmDeviceXML = `    <tpm model='tpm-tis'>
      <backend type='emulator' version='2.0'/>
    </tpm>`
