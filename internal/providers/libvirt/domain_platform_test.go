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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"libvirt.org/go/libvirtxml"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the virtual platform of a new domain (domain_platform.go).
// Every generated variant below was also defined and started (paused) on
// libvirt 10.0 / QEMU 8.2 with the distribution's OVMF package while this was
// written; a host check is in docs/libvirt-firmware.md.

func TestDomainPlatformFor(t *testing.T) {
	sec := func(sb, vtd bool) *contracts.SecurityProfile {
		return &contracts.SecurityProfile{SecureBoot: sb, VTDEnabled: vtd}
	}
	for _, tc := range []struct {
		name  string
		class contracts.VMClass
		want  domainPlatform
	}{
		{"nothing asked: historical i440fx + BIOS", contracts.VMClass{}, domainPlatform{}},
		{"firmware BIOS", contracts.VMClass{Firmware: "BIOS"}, domainPlatform{}},
		{"firmware EFI is BIOS, as on vSphere", contracts.VMClass{Firmware: "EFI"}, domainPlatform{}},
		{"firmware UEFI", contracts.VMClass{Firmware: "UEFI"}, domainPlatform{q35: true, uefi: true}},
		{"firmware uefi (any case)", contracts.VMClass{Firmware: " uefi "}, domainPlatform{q35: true, uefi: true}},
		{"secure boot forces UEFI", contracts.VMClass{Firmware: "BIOS", SecurityProfile: sec(true, false)},
			domainPlatform{q35: true, uefi: true, secureBoot: true}},
		{"secure boot with UEFI", contracts.VMClass{Firmware: "UEFI", SecurityProfile: sec(true, false)},
			domainPlatform{q35: true, uefi: true, secureBoot: true}},
		{"VT-d needs q35, keeps BIOS", contracts.VMClass{SecurityProfile: sec(false, true)},
			domainPlatform{q35: true, iommu: true}},
		{"TPM alone keeps i440fx", contracts.VMClass{SecurityProfile: &contracts.SecurityProfile{TPMEnabled: true}},
			domainPlatform{}},
		{"nested alone keeps i440fx", contracts.VMClass{PerformanceProfile: &contracts.PerformanceProfile{NestedVirtualization: true}},
			domainPlatform{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, domainPlatformFor(tc.class))
		})
	}
}

// platformTestXML generates the create-path domain XML for class (and
// networks), with a cloud-init seed, against a local VirshProvider.
func platformTestXML(t *testing.T, class contracts.VMClass, networks ...contracts.NetworkAttachment) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only: the /dev/kvm probe execs test(1)")
	}
	req := contracts.CreateRequest{Name: "web", Owner: ownerTeamA, Class: class, Networks: networks}
	x, err := (&Provider{}).generateDomainXMLWithStorage(context.Background(), localTestVirshProvider(), req,
		"team-a.web", "/var/lib/libvirt/images/team-a.web-disk.qcow2", "/seed/x.iso")
	require.NoError(t, err)
	return x
}

// normalizeDomainXML replaces what differs between runs and hosts: the random
// UUID and the KVM/TCG domain type.
func normalizeDomainXML(x string) string {
	x = regexp.MustCompile(`<uuid>[^<]*</uuid>`).ReplaceAllString(x, "<uuid>U</uuid>")
	return regexp.MustCompile(`^<domain type='(kvm|qemu)'>`).ReplaceAllString(x, "<domain type='kvm'>")
}

// TestDomainXML_I440fxDefaultIsHistorical: a VMClass that asks for no UEFI,
// secure boot or IOMMU gets the historical i440fx domain, byte for byte.
func TestDomainXML_I440fxDefaultIsHistorical(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "domain_i440fx_default.golden.xml"))
	require.NoError(t, err)
	got := normalizeDomainXML(platformTestXML(t, contracts.VMClass{}))
	// The golden has no owner stamp (it was generated without one).
	got = regexp.MustCompile(`(?s)  <metadata>.*</metadata>\n`).ReplaceAllString(got, "")
	assert.Equal(t, string(want), got)
	assert.Contains(t, got, "<controller type='pci' index='0' model='pci-root'/>")
	assert.Contains(t, got, "machine='pc'")
}

// TestDomainXML_SecureBootGolden pins the secure-boot domain: q35, firmware
// chosen by libvirt with the secure-boot and enrolled-keys features, SMM on,
// and the q35 device layout.
func TestDomainXML_SecureBootGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "domain_q35_secureboot.golden.xml"))
	require.NoError(t, err)
	got := normalizeDomainXML(platformTestXML(t, contracts.VMClass{
		SecurityProfile: &contracts.SecurityProfile{SecureBoot: true}}))
	got = regexp.MustCompile(`(?s)  <metadata>.*</metadata>\n`).ReplaceAllString(got, "")
	assert.Equal(t, string(want), got)
}

// firmwareFeatures maps the <os><firmware><feature> names to enabled.
func firmwareFeatures(d *libvirtxml.Domain) map[string]string {
	out := map[string]string{}
	if d.OS == nil || d.OS.FirmwareInfo == nil {
		return out
	}
	for _, f := range d.OS.FirmwareInfo.Features {
		out[f.Name] = f.Enabled
	}
	return out
}

// assertQ35Layout checks what libvirt requires of a q35 domain: no pci-root,
// no IDE (controller or disk), and no fixed PCI address anywhere (libvirt
// assigns them on pcie-root).
func assertQ35Layout(t *testing.T, x string, d *libvirtxml.Domain) {
	t.Helper()
	require.NotNil(t, d.OS.Type)
	assert.Equal(t, machineTypeQ35, d.OS.Type.Machine)
	assert.NotContains(t, x, "pci-root")
	assert.NotContains(t, x, "type='ide'")
	assert.NotContains(t, x, "bus='ide'")
	assert.NotContains(t, x, "<address type='pci'")
	assert.NotContains(t, x, "/usr/share/", "firmware is never named by path")
	for _, disk := range d.Devices.Disks {
		if disk.Device == "cdrom" {
			require.NotNil(t, disk.Target)
			assert.Equal(t, "sata", disk.Target.Bus, "the cloud-init CD-ROM is on SATA on q35")
		}
	}
}

func TestDomainXML_SecureBoot(t *testing.T) {
	x := platformTestXML(t, contracts.VMClass{Firmware: "BIOS",
		SecurityProfile: &contracts.SecurityProfile{SecureBoot: true, TPMEnabled: true}},
		contracts.NetworkAttachment{Bridge: "br0"}, contracts.NetworkAttachment{NetworkName: "default"})
	d, err := parseDomainLibvirtxml(x)
	require.NoError(t, err)
	assertQ35Layout(t, x, d)

	assert.Equal(t, "efi", d.OS.Firmware)
	assert.Equal(t, map[string]string{"secure-boot": "yes", "enrolled-keys": "yes"}, firmwareFeatures(d),
		"a Secure Boot build with the default keys enrolled, so Secure Boot is enforced")
	require.NotNil(t, d.OS.Loader)
	assert.Equal(t, "yes", d.OS.Loader.Secure)
	assert.Empty(t, d.OS.Loader.Path)
	assert.Nil(t, d.OS.NVRam, "libvirt creates the varstore from the selected template")
	require.NotNil(t, d.Features)
	require.NotNil(t, d.Features.SMM, "Secure Boot OVMF requires SMM")
	assert.Equal(t, "on", d.Features.SMM.State)
	assert.Nil(t, d.Features.IOAPIC)
	assert.Len(t, d.Devices.Interfaces, 2)
	assert.Len(t, d.Devices.TPMs, 1)
	assert.Nil(t, d.Devices.IOMMU)
}

func TestDomainXML_UEFIWithoutSecureBoot(t *testing.T) {
	x := platformTestXML(t, contracts.VMClass{Firmware: "UEFI"})
	d, err := parseDomainLibvirtxml(x)
	require.NoError(t, err)
	assertQ35Layout(t, x, d)

	assert.Equal(t, "efi", d.OS.Firmware)
	assert.Equal(t, map[string]string{"secure-boot": "no"}, firmwareFeatures(d),
		"firmware UEFI asks for a build without Secure Boot")
	assert.Nil(t, d.OS.Loader)
	assert.Nil(t, d.Features.SMM)
}

func TestDomainXML_VTD(t *testing.T) {
	x := platformTestXML(t, contracts.VMClass{SecurityProfile: &contracts.SecurityProfile{VTDEnabled: true}})
	d, err := parseDomainLibvirtxml(x)
	require.NoError(t, err)
	assertQ35Layout(t, x, d)

	assert.Empty(t, d.OS.Firmware, "VT-d alone keeps BIOS firmware")
	assert.NotContains(t, x[strings.Index(x, "<features>"):strings.Index(x, "</features>")], "iommu",
		"<iommu> is a device, not a feature (libvirt: unexpected feature 'iommu')")
	require.NotNil(t, d.Devices.IOMMU)
	assert.Equal(t, "intel", d.Devices.IOMMU.Model)
	require.NotNil(t, d.Devices.IOMMU.Driver)
	assert.Equal(t, "on", d.Devices.IOMMU.Driver.IntRemap)
	require.NotNil(t, d.Features.IOAPIC, "interrupt remapping needs the QEMU I/O APIC")
	assert.Equal(t, "qemu", d.Features.IOAPIC.Driver)
}

func TestDomainXML_NestedVirtualization(t *testing.T) {
	x := platformTestXML(t, contracts.VMClass{PerformanceProfile: &contracts.PerformanceProfile{NestedVirtualization: true}})
	d, err := parseDomainLibvirtxml(x)
	require.NoError(t, err)
	require.NotNil(t, d.CPU)
	assert.Equal(t, "host-passthrough", d.CPU.Mode)
	assert.Empty(t, d.CPU.Features, "no host CPU has both vmx and svm; host-passthrough carries whichever the host has")
	assert.Equal(t, machineTypePC, d.OS.Type.Machine)

	plain, err := parseDomainLibvirtxml(platformTestXML(t, contracts.VMClass{}))
	require.NoError(t, err)
	assert.Equal(t, "host-model", plain.CPU.Mode)
}

// TestDomainXML_I440fxMultiNICNoSlotCollision: on i440fx only the first NIC
// has a fixed PCI slot, so no two devices claim one.
func TestDomainXML_I440fxMultiNICNoSlotCollision(t *testing.T) {
	x := platformTestXML(t, contracts.VMClass{},
		contracts.NetworkAttachment{Bridge: "br0"}, contracts.NetworkAttachment{NetworkName: "a"},
		contracts.NetworkAttachment{NetworkName: "b"}, contracts.NetworkAttachment{})
	d, err := parseDomainLibvirtxml(x)
	require.NoError(t, err)
	require.Len(t, d.Devices.Interfaces, 4)
	slots := map[string]int{}
	for _, m := range regexp.MustCompile(`<address type='pci' domain='0x0000' bus='0x00' slot='(0x[0-9a-f]+)' function='(0x[0-9a-f])'`).
		FindAllStringSubmatch(x, -1) {
		slots[m[1]+"."+m[2]]++
	}
	for slot, n := range slots {
		assert.Equal(t, 1, n, "PCI address %s is used %d times", slot, n)
	}
}

// TestDomainXML_ValidatesAgainstLibvirtSchema runs virt-xml-validate (libvirt's
// RelaxNG schema) over every variant when the tool is installed.
func TestDomainXML_ValidatesAgainstLibvirtSchema(t *testing.T) {
	tool, err := exec.LookPath("virt-xml-validate")
	if err != nil {
		t.Skip("virt-xml-validate is not installed")
	}
	nets := []contracts.NetworkAttachment{{Bridge: "br0", MacAddress: "52:54:00:aa:bb:cc"}, {NetworkName: "default"}, {}}
	for name, class := range map[string]contracts.VMClass{
		"bios":        {},
		"uefi":        {Firmware: "UEFI", SecurityProfile: &contracts.SecurityProfile{TPMEnabled: true}},
		"secure-boot": {SecurityProfile: &contracts.SecurityProfile{SecureBoot: true}},
		"vtd":         {SecurityProfile: &contracts.SecurityProfile{VTDEnabled: true}},
		"all": {Firmware: "UEFI", PerformanceProfile: &contracts.PerformanceProfile{NestedVirtualization: true,
			CPUHotAddEnabled: true, MemoryHotAddEnabled: true},
			SecurityProfile: &contracts.SecurityProfile{SecureBoot: true, VTDEnabled: true, TPMEnabled: true}},
		"nested": {PerformanceProfile: &contracts.PerformanceProfile{NestedVirtualization: true}},
	} {
		t.Run(name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), name+".xml")
			require.NoError(t, os.WriteFile(f, []byte(platformTestXML(t, class, nets...)), 0o600))
			out, err := exec.Command(tool, f, "domain").CombinedOutput() //nolint:gosec // test runs the schema validator on its own file
			assert.NoError(t, err, "virt-xml-validate: %s", out)
		})
	}
}
