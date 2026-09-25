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

package libvirt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveLinkedCloneXML is `virsh dumpxml` of a RUNNING linked clone, the shape
// of the security review's repro: the clone's overlay is the top-level disk
// source, and libvirt lists its backing file — the SOURCE VM's disk — in a
// nested <backingStore>. The line scanner Delete used collected both.
const liveLinkedCloneXML = `<domain type='kvm' id='7'>
  <name>team-b.copy</name>
  <uuid>22222222-3333-4444-8555-666666666666</uuid>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/team-b.copy-disk.qcow2' index='2'/>
      <backingStore type='file' index='3'>
        <format type='qcow2'/>
        <source file='/var/lib/libvirt/images/team-a.template-disk.qcow2'/>
        <backingStore/>
      </backingStore>
      <target dev='vda' bus='virtio'/>
    </disk>
  </devices>
</domain>`

func TestDomainDiskFiles_RunningLinkedCloneExcludesBackingStore(t *testing.T) {
	doc, err := parseDomainDisks(liveLinkedCloneXML)
	require.NoError(t, err)
	assert.Equal(t, "team-b.copy", doc.Name)
	assert.Equal(t, "22222222-3333-4444-8555-666666666666", doc.UUID)
	assert.Equal(t, []string{"/var/lib/libvirt/images/team-b.copy-disk.qcow2"}, doc.diskFiles(),
		"only the clone's own overlay; never the source VM's disk from <backingStore>")
}

func TestDomainDiskFiles_OnlyTopLevelFileDisks(t *testing.T) {
	const x = `<domain type='kvm'>
  <name>web</name>
  <uuid>33333333-4444-4555-8666-777777777777</uuid>
  <os><nvram>/var/lib/libvirt/qemu/nvram/web_VARS.fd</nvram></os>
  <devices>
    <disk type="file" device="disk">
      <source file="/pool/web-disk.qcow2"/>
      <backingStore type="file"><source file="/pool/base.qcow2"/><backingStore type="file"><source file="/pool/base0.qcow2"/></backingStore></backingStore>
      <mirror type="file" job="copy"><source file="/pool/mirror-target.qcow2"/></mirror>
    </disk>
    <disk type='file'>
      <source file='/pool/data.qcow2'/>
    </disk>
    <disk type='block' device='disk'><source dev='/dev/sdz'/></disk>
    <disk type='volume' device='disk'><source pool='default' volume='vol1'/></disk>
    <disk type='network' device='disk'><source protocol='rbd' name='pool/img'/></disk>
    <disk type='file' device='disk'><source file='/pool/odd-cidata.iso'/></disk>
    <disk type='file' device='cdrom'><source file='/isos/installer.iso'/></disk>
    <disk type='file' device='cdrom'><source file='/tmp/virtrigaud-cloudinit-web.AbCdEfGhIj/cloud-init.iso'/></disk>
    <disk type='file' device='floppy'><source file='/pool/floppy.img'/></disk>
    <disk type='file' device='cdrom'/>
    <filesystem type='mount'><source dir='/srv/share'/></filesystem>
    <serial type='file'><source path='/var/log/web.log'/></serial>
  </devices>
</domain>`
	doc, err := parseDomainDisks(x)
	require.NoError(t, err)
	assert.Equal(t, []string{"/pool/web-disk.qcow2", "/pool/data.qcow2"}, doc.diskFiles(),
		"file-backed disks only (device omitted = disk); no backing/mirror sources, media, seeds or other devices")
}

func TestParseDomainDisks_RejectsNonDomain(t *testing.T) {
	for _, x := range []string{"", "<pool><name>x</name></pool>", "<domain><name>web"} {
		_, err := parseDomainDisks(x)
		assert.Error(t, err, "%q", x)
	}
}

func TestCloudInitSeedDir(t *testing.T) {
	const staging = "/tmp"
	cdrom := func(name, src string) string {
		return "<domain><name>" + name + "</name><devices><disk type='file' device='disk'><source file='/pool/" + name +
			"-disk.qcow2'/></disk><disk type='file' device='cdrom'><source file='" + src + "'/></disk></devices></domain>"
	}
	cases := []struct {
		name string
		xml  string
		want string
	}{
		{"per-create seed directory", cdrom("team-a.web", "/tmp/virtrigaud-cloudinit-team-a.web.AbCdEfGhIj/cloud-init.iso"),
			"/tmp/virtrigaud-cloudinit-team-a.web.AbCdEfGhIj"},
		{"legacy per-name seed directory", cdrom("web", "/tmp/virtrigaud-cloudinit/web/cloud-init.iso"), "/tmp/virtrigaud-cloudinit/web"},
		{"legacy directory of another domain", cdrom("web", "/tmp/virtrigaud-cloudinit/other/cloud-init.iso"), ""},
		{"seed in a shared pool directory", cdrom("web", "/var/lib/libvirt/images/cloud-init/web-cloud-init.iso"), ""},
		{"seed ISO directly in the pool", cdrom("web", "/var/lib/libvirt/images/cloud-init.iso"), ""},
		{"prefix only, no suffix", cdrom("web", "/tmp/virtrigaud-cloudinit-/cloud-init.iso"), ""},
		{"nested below a seed directory", cdrom("web", "/tmp/virtrigaud-cloudinit-web.x/sub/cloud-init.iso"), ""},
		{"traversal out of staging", cdrom("web", "/tmp/virtrigaud-cloudinit-web.x/../../etc/cloud-init.iso"), ""},
		{"installer ISO", cdrom("web", "/isos/ubuntu.iso"), ""},
		{"seed attached as a disk, not media", "<domain><name>web</name><devices><disk type='file' device='disk'>" +
			"<source file='/tmp/virtrigaud-cloudinit-web.x/cloud-init.iso'/></disk></devices></domain>", ""},
		{"seed only inside a backingStore", "<domain><name>web</name><devices><disk type='file' device='cdrom'>" +
			"<source file='/isos/a.iso'/><backingStore type='file'><source file='/tmp/virtrigaud-cloudinit-web.x/cloud-init.iso'/>" +
			"</backingStore></disk></devices></domain>", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := parseDomainDisks(tc.xml)
			require.NoError(t, err)
			assert.Equal(t, tc.want, doc.cloudInitSeedDir(staging))
		})
	}
}
