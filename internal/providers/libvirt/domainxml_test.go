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

import "testing"

const sampleDomainXML = `<domain type='kvm'>
  <name>web-01</name>
  <uuid>4dea22b3-1d52-d8f3-2516-782e98ab3fa0</uuid>
  <memory unit='KiB'>2097152</memory>
  <currentMemory unit='KiB'>2097152</currentMemory>
  <vcpu placement='static'>2</vcpu>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/web-01.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/var/lib/libvirt/images/web-01-cidata.iso'/>
      <target dev='sda' bus='sata'/>
    </disk>
    <interface type='network'>
      <mac address='52:54:00:12:34:56'/>
      <source network='default'/>
    </interface>
  </devices>
</domain>`

func TestParseDomainXML(t *testing.T) {
	dx, err := parseDomainXML(sampleDomainXML)
	if err != nil {
		t.Fatalf("parseDomainXML: %v", err)
	}
	if dx.Name != "web-01" {
		t.Errorf("Name = %q, want web-01", dx.Name)
	}
	if dx.UUID != "4dea22b3-1d52-d8f3-2516-782e98ab3fa0" {
		t.Errorf("UUID = %q", dx.UUID)
	}
	if dx.VCPU != 2 {
		t.Errorf("VCPU = %d, want 2", dx.VCPU)
	}
	if dx.Memory.KiB != 2097152 {
		t.Errorf("Memory = %d, want 2097152 KiB", dx.Memory.KiB)
	}
	if mib, err := dx.MemoryMiB(); err != nil || mib != 2048 {
		t.Errorf("MemoryMiB() = %d, %v; want 2048, nil", mib, err)
	}
}

// A non-KiB memory unit (possible from go-libvirt XML, never from dumpxml) must
// error rather than silently mis-scale via a blind /1024.
func TestDomainXMLMemoryMiB_rejectsNonKiB(t *testing.T) {
	dx, err := parseDomainXML(`<domain><memory unit='MiB'>2048</memory></domain>`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dx.MemoryMiB(); err == nil {
		t.Error("MemoryMiB() = nil error, want error for unit='MiB'")
	}
}

// Disks() must keep only device="disk", drop the cdrom/cloud-init ISO, take the
// format from <driver type>, and leave size 0 (not present in XML).
func TestDomainXMLDisks_filtersCdromAndCloudInit(t *testing.T) {
	dx, err := parseDomainXML(sampleDomainXML)
	if err != nil {
		t.Fatal(err)
	}
	disks := dx.Disks("web-01")
	if len(disks) != 1 {
		t.Fatalf("got %d disks, want 1 (cdrom/cidata excluded): %+v", len(disks), disks)
	}
	d := disks[0]
	if d.Path != "/var/lib/libvirt/images/web-01.qcow2" {
		t.Errorf("Path = %q", d.Path)
	}
	if d.Format != "qcow2" {
		t.Errorf("Format = %q, want qcow2", d.Format)
	}
	if d.SizeGiB != 0 {
		t.Errorf("SizeGiB = %d, want 0 (not in XML)", d.SizeGiB)
	}
}

func TestDomainXMLNetworks_extractsMAC(t *testing.T) {
	dx, err := parseDomainXML(sampleDomainXML)
	if err != nil {
		t.Fatal(err)
	}
	nets := dx.Networks()
	if len(nets) != 1 {
		t.Fatalf("got %d networks, want 1", len(nets))
	}
	if nets[0].MAC != "52:54:00:12:34:56" {
		t.Errorf("MAC = %q", nets[0].MAC)
	}
	if nets[0].IPAddress != "" {
		t.Errorf("IPAddress = %q, want empty at list time", nets[0].IPAddress)
	}
}

// parseDomainListTable replaces the per-domain `virsh domstate` N+1: one
// `virsh list --all` gives name+state. State can be two words ("shut off").
func TestParseDomainListTable(t *testing.T) {
	// "Windows Server 2019" exercises a name with interior spaces: column-offset
	// slicing must keep it whole, where the old Fields() split truncated it to
	// "Windows" and folded the rest into the state.
	out := ` Id   Name                  State
------------------------------------------
 1    web-01                running
 -    Windows Server 2019   shut off
 3    cache-01              paused
`
	domains := parseDomainListTable(out)
	if len(domains) != 3 {
		t.Fatalf("got %d domains, want 3: %+v", len(domains), domains)
	}
	want := map[string]string{"web-01": "running", "Windows Server 2019": "shut off", "cache-01": "paused"}
	for _, d := range domains {
		if want[d.Name] != d.State {
			t.Errorf("domain %q state = %q, want %q", d.Name, d.State, want[d.Name])
		}
	}
}

func TestParseDomainListTable_empty(t *testing.T) {
	out := ` Id   Name   State
--------------------------
`
	if domains := parseDomainListTable(out); len(domains) != 0 {
		t.Errorf("got %d domains, want 0", len(domains))
	}
}
