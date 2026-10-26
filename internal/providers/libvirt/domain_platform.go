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
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"libvirt.org/go/libvirtxml"

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

// errNoUEFIFirmware and errNoSecureBootFirmware are the refusals of
// uefiFirmwarePreflight, for the requesting VM's status.
const (
	errNoUEFIFirmware = "the libvirt host offers no UEFI firmware for the q35 machine type (install an OVMF " +
		"package, such as ovmf or edk2-ovmf); nothing was created"
	errNoSecureBootFirmware = "the libvirt host offers no Secure Boot capable UEFI firmware for the q35 machine type " +
		"(install an OVMF package with Secure Boot builds); nothing was created"
)

// uefiFirmwarePreflight refuses a UEFI create (pl.uefi) BEFORE any disk is
// written when the host behind vp positively reports that it has no firmware
// for it: `virsh domcapabilities` for the q35 machine lists no "efi" value in
// its firmware enum, or — for secure boot — no "yes" in the loader's secure
// enum (an InvalidSpec, as libvirt's own refusal at define would be, without
// copying the image first on every re-check). A host that cannot be asked, or
// whose answer does not have the enum (an older libvirt), is not refused here:
// the define decides. Firmware with enrolled keys is not visible in these
// enums; a host without it is refused by the define.
func uefiFirmwarePreflight(ctx context.Context, vp *VirshProvider, pl domainPlatform, domainType string) error {
	if !pl.uefi {
		return nil
	}
	res, err := vp.runVirshCommand(withExpectedFailures(ctx), "domcapabilities",
		"--machine", machineTypeQ35, "--arch", "x86_64", "--virttype", domainType)
	if err != nil {
		log.Printf("INFO Could not read the host's domain capabilities before a UEFI create (the define decides): %v", err)
		return nil
	}
	caps := &libvirtxml.DomainCaps{}
	if err := caps.Unmarshal(res.Stdout); err != nil || caps.OS == nil || caps.OS.Supported == "no" {
		return nil
	}
	if values, ok := domainCapsEnum(caps.OS.Enums, "firmware"); ok && !containsString(values, "efi") {
		return contracts.NewInvalidSpecError(errNoUEFIFirmware, nil)
	}
	if pl.secureBoot && caps.OS.Loader != nil {
		if values, ok := domainCapsEnum(caps.OS.Loader.Enums, "secure"); ok && !containsString(values, "yes") {
			return contracts.NewInvalidSpecError(errNoSecureBootFirmware, nil)
		}
	}
	return nil
}

// domainCapsEnum returns the values of the enum name in enums, and whether
// the enum is there at all.
func domainCapsEnum(enums []libvirtxml.DomainCapsEnum, name string) ([]string, bool) {
	for _, e := range enums {
		if e.Name == name {
			return e.Values, true
		}
	}
	return nil, false
}

// uefiVarstoreSuffix is the file name suffix of a domain's own UEFI variable
// store: libvirt names it <domain>_VARS.fd, and a clone's copy is named after
// the clone (rewriteNVRAMPath).
const uefiVarstoreSuffix = "_VARS.fd"

// undefineNVRAMFor decides what the undefine of domainName (definition
// domainXML) does with its UEFI variable store:
//
//   - no variable store (no <nvram> and no <os firmware='efi'>): plain
//     undefine, as for every BIOS domain;
//   - the domain's own store — <dir>/<domainName>_VARS.fd, or one libvirt has
//     not named yet — is removed with it (--nvram);
//   - any other path (an adopted or hand-made domain naming a custom or shared
//     file) is kept (--keep-nvram) and logged: it is not provably the
//     domain's own.
//
// A definition that cannot be parsed gets a plain undefine: libvirt then
// refuses a domain whose store exists, which fails the delete (retried)
// rather than remove a file that may not be the domain's.
func undefineNVRAMFor(domainName, domainXML string) undefineNVRAM {
	d, err := parseDomainLibvirtxml(domainXML)
	if err != nil || d.OS == nil {
		return undefineNoNVRAM
	}
	if d.OS.NVRam == nil && d.OS.Firmware != "efi" {
		return undefineNoNVRAM
	}
	path := ""
	if d.OS.NVRam != nil {
		path = strings.TrimSpace(d.OS.NVRam.NVRam)
	}
	if path == "" || filepath.Base(path) == domainName+uefiVarstoreSuffix {
		return undefineRemoveNVRAM
	}
	log.Printf("WARN Keeping the UEFI variable store %s of libvirt domain %s: it is not named after the domain, so it "+
		"may not be its own", path, domainName)
	return undefineKeepNVRAM
}

// ensureFreshUEFIVarstore runs once a NEW UEFI domain domainName is defined on
// the host behind vp, before it is ever started. libvirt creates the domain's
// variable store from the firmware's template at the first start, but only
// when no file is at the store's path; a file already there — left by an
// earlier domain of the same name (`undefine --keep-nvram`, a host restore,
// a domain made outside VirtRigaud) — would be used as it is, and with it that
// domain's Secure Boot state (setup mode, other keys). So:
//
//   - the firmware libvirt selected (loader, template, store path) is logged
//     for audit;
//   - nothing at the store's path: nothing to do (libvirt creates it fresh);
//   - a regular file no other domain names as its store: removed, so the
//     first start creates the store from the template;
//   - a symbolic link, or a file another domain names as its store: refused
//     (Conflict); nothing is removed.
//
// libvirt releases that select the firmware only at start have no store path
// in the new definition yet: nothing can be checked then, and a file already
// at the path is used as it is (docs/libvirt-firmware.md).
//
// A refusal or a failed check is returned; the caller undefines the new
// domain, keeping the file, which is not its own.
func ensureFreshUEFIVarstore(ctx context.Context, vp *VirshProvider, domainName string) error {
	res, err := vp.runVirshCommand(ctx, "dumpxml", domainName, "--inactive")
	if err != nil {
		return contracts.NewRetryableError("read the new domain's definition to check its UEFI variable store", err)
	}
	d, err := parseDomainLibvirtxml(res.Stdout)
	if err != nil {
		return contracts.NewRetryableError("read the new domain's definition to check its UEFI variable store", err)
	}
	var loader, secure, template, store string
	if d.OS != nil && d.OS.Loader != nil {
		loader, secure = strings.TrimSpace(d.OS.Loader.Path), d.OS.Loader.Secure
	}
	if d.OS != nil && d.OS.NVRam != nil {
		template, store = d.OS.NVRam.Template, strings.TrimSpace(d.OS.NVRam.NVRam)
	}
	if store == "" {
		log.Printf("INFO libvirt domain %s: its UEFI firmware is selected when the domain starts (this libvirt does "+
			"not select it at define); its variable store cannot be checked now", domainName)
		return nil
	}
	log.Printf("INFO libvirt domain %s: UEFI firmware selected by libvirt: loader=%s secure=%s nvram-template=%s nvram=%s",
		domainName, loader, secure, template, store)

	kind, err := runHost(ctx, vp, "sh", "-c", targetKindScript, "sh", store)
	if err != nil {
		return contracts.NewRetryableError("check the new domain's UEFI variable store path on the host", err)
	}
	switch strings.TrimSpace(kind.Stdout) {
	case "":
		return nil
	case targetKindSymlink:
		log.Printf("WARN Refusing libvirt domain %s: its UEFI variable store path %s is a symbolic link", domainName, store)
		return contracts.NewConflictError(fmt.Sprintf(
			"the UEFI variable store path of libvirt domain %q is a symbolic link on the host; it is never used "+
				"(details are in the provider log)", domainName), nil)
	}
	used, err := nvramUsedByOtherDomain(ctx, vp, domainName, store)
	if err != nil {
		return contracts.NewRetryableError("check whether another domain uses the new domain's UEFI variable store path", err)
	}
	if used {
		log.Printf("WARN Refusing libvirt domain %s: its UEFI variable store path %s is another domain's", domainName, store)
		return contracts.NewConflictError(fmt.Sprintf(
			"the UEFI variable store path of libvirt domain %q is used by another domain on the host; it is never "+
				"shared (details are in the provider log)", domainName), nil)
	}
	log.Printf("WARN Removing the UEFI variable store %s that an earlier domain named %s left: the new domain starts "+
		"from the firmware's template, never from an inherited store", store, domainName)
	if _, err := runHost(ctx, vp, "sudo", "rm", "-f", "--", store); err != nil {
		return contracts.NewRetryableError("remove a UEFI variable store an earlier domain of the same name left", err)
	}
	return nil
}

// nvramUsedByOtherDomain reports whether a domain on the host behind vp other
// than domainName names path as its UEFI variable store.
func nvramUsedByOtherDomain(ctx context.Context, vp *VirshProvider, domainName, path string) (bool, error) {
	domains, err := vp.listDomains(ctx)
	if err != nil {
		return false, err
	}
	want := filepath.Clean(path)
	for _, dom := range domains {
		if dom.Name == domainName {
			continue
		}
		res, err := vp.runVirshCommand(ctx, "dumpxml", dom.Name)
		if err != nil {
			if virshNoDomain(err) {
				continue // undefined meanwhile
			}
			return false, fmt.Errorf("read the definition of domain %s: %w", dom.Name, err)
		}
		d, err := parseDomainLibvirtxml(res.Stdout)
		if err != nil {
			return false, fmt.Errorf("parse the definition of domain %s: %w", dom.Name, err)
		}
		if d.OS != nil && d.OS.NVRam != nil && filepath.Clean(strings.TrimSpace(d.OS.NVRam.NVRam)) == want {
			return true, nil
		}
	}
	return false, nil
}
