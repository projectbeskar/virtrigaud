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
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// sourceDomainXML is a representative virsh dumpxml output for a cloned VM: one
// virtio disk, a cloud-init CD-ROM, and one NIC with a MAC.
const sourceDomainXML = `<domain type='kvm'>
  <name>vm-source</name>
  <uuid>11111111-2222-3333-4444-555555555555</uuid>
  <memory unit='MiB'>2048</memory>
  <currentMemory unit='MiB'>2048</currentMemory>
  <vcpu placement='static'>2</vcpu>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/vm-source-disk.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/var/lib/libvirt/images/vm-source-cidata.iso'/>
      <target dev='hda' bus='ide'/>
      <readonly/>
    </disk>
    <interface type='bridge'>
      <mac address='52:54:00:aa:bb:cc'/>
      <source bridge='virbr0'/>
      <model type='virtio'/>
    </interface>
  </devices>
</domain>`

// TestRewriteDomainXMLForClone_FreshIdentity verifies the rewrite swaps the
// name, gives a fresh UUID and MAC, re-points only the primary disk, and leaves
// the cloud-init CD-ROM source untouched.
func TestRewriteDomainXMLForClone_FreshIdentity(t *testing.T) {
	const srcDisk = "/var/lib/libvirt/images/vm-source-disk.qcow2"
	const tgtDisk = "/var/lib/libvirt/images/vm-target-disk.qcow2"

	out, err := rewriteDomainXMLForClone(sourceDomainXML, "vm-target", srcDisk, tgtDisk)
	require.NoError(t, err)

	// Name is rewritten.
	assert.Contains(t, out, "<name>vm-target</name>")
	assert.NotContains(t, out, "<name>vm-source</name>")

	// UUID is fresh (differs from the source) and well-formed.
	assert.NotContains(t, out, "11111111-2222-3333-4444-555555555555",
		"clone must not reuse the source UUID")
	uuidRe := regexp.MustCompile(`<uuid>([0-9a-f-]{36})</uuid>`)
	m := uuidRe.FindStringSubmatch(out)
	require.Len(t, m, 2, "rewritten XML must contain a single well-formed UUID")

	// MAC is fresh (differs from the source) and in the 52:54:00 OUI.
	assert.NotContains(t, out, "52:54:00:aa:bb:cc", "clone must not reuse the source MAC")
	macRe := regexp.MustCompile(`<mac address='(52:54:00:[0-9a-f]{2}:[0-9a-f]{2}:[0-9a-f]{2})'/>`)
	assert.Len(t, macRe.FindAllString(out, -1), 1, "exactly one NIC MAC must be rewritten")

	// Primary disk is re-pointed; cloud-init CD-ROM is untouched.
	assert.Contains(t, out, "file='"+tgtDisk+"'")
	assert.NotContains(t, out, "file='"+srcDisk+"'")
	assert.Contains(t, out, "vm-source-cidata.iso",
		"cloud-init CD-ROM source must not be rewritten by the primary-disk re-point")
}

// TestRewriteDomainXMLForClone_MultiNIC verifies every NIC gets its own fresh
// MAC so a multi-homed source never collides with its clone on any segment.
func TestRewriteDomainXMLForClone_MultiNIC(t *testing.T) {
	xml := `<domain type='kvm'>
  <name>s</name>
  <uuid>aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee</uuid>
  <devices>
    <disk type='file' device='disk'><source file='/p/s-disk.qcow2'/></disk>
    <interface type='bridge'><mac address='52:54:00:11:11:11'/></interface>
    <interface type='bridge'><mac address='52:54:00:22:22:22'/></interface>
  </devices>
</domain>`

	out, err := rewriteDomainXMLForClone(xml, "t", "/p/s-disk.qcow2", "/p/t-disk.qcow2")
	require.NoError(t, err)

	assert.NotContains(t, out, "52:54:00:11:11:11")
	assert.NotContains(t, out, "52:54:00:22:22:22")
	macRe := regexp.MustCompile(`<mac address='52:54:00:[0-9a-f:]+'/>`)
	macs := macRe.FindAllString(out, -1)
	require.Len(t, macs, 2, "both NICs must be rewritten")
	assert.NotEqual(t, macs[0], macs[1], "each NIC must get a distinct fresh MAC")
}

// TestRewriteDomainXMLForClone_Errors covers the malformed-input paths.
func TestRewriteDomainXMLForClone_Errors(t *testing.T) {
	tests := []struct {
		name      string
		xml       string
		srcDisk   string
		tgtDisk   string
		errSubstr string
	}{
		{
			name:      "empty xml",
			xml:       "   ",
			srcDisk:   "/p/s.qcow2",
			tgtDisk:   "/p/t.qcow2",
			errSubstr: "empty",
		},
		{
			name:      "no name element",
			xml:       `<domain><uuid>x</uuid></domain>`,
			srcDisk:   "/p/s.qcow2",
			tgtDisk:   "/p/t.qcow2",
			errSubstr: "<name>",
		},
		{
			name:      "no uuid element",
			xml:       `<domain><name>s</name></domain>`,
			srcDisk:   "/p/s.qcow2",
			tgtDisk:   "/p/t.qcow2",
			errSubstr: "<uuid>",
		},
		{
			name:      "primary disk not found",
			xml:       `<domain><name>s</name><uuid>u</uuid><source file='/other.qcow2'/></domain>`,
			srcDisk:   "/p/s.qcow2",
			tgtDisk:   "/p/t.qcow2",
			errSubstr: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rewriteDomainXMLForClone(tt.xml, "t", tt.srcDisk, tt.tgtDisk)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}

// TestApplyClassOverrides verifies CPU/memory overrides are spliced in, and
// that empty/invalid ClassJSON is a no-op (the clone inherits source resources).
func TestApplyClassOverrides(t *testing.T) {
	base := `<domain>
  <memory unit='MiB'>2048</memory>
  <currentMemory unit='MiB'>2048</currentMemory>
  <vcpu placement='static'>2</vcpu>
</domain>`

	t.Run("cpu and memory override", func(t *testing.T) {
		out := applyClassOverrides(base, `{"CPU":4,"MemoryMiB":8192}`)
		assert.Contains(t, out, "<memory unit='MiB'>8192</memory>")
		assert.Contains(t, out, "<currentMemory unit='MiB'>8192</currentMemory>")
		assert.Contains(t, out, "<vcpu placement='static'>4</vcpu>")
	})

	t.Run("empty json is no-op", func(t *testing.T) {
		assert.Equal(t, base, applyClassOverrides(base, ""))
	})

	t.Run("invalid json is no-op", func(t *testing.T) {
		assert.Equal(t, base, applyClassOverrides(base, "{not json"))
	})

	t.Run("zero values are ignored", func(t *testing.T) {
		out := applyClassOverrides(base, `{"CPU":0,"MemoryMiB":0}`)
		assert.Equal(t, base, out, "zero CPU/memory must not overwrite inherited values")
	})
}

// TestGenerateRandomUUID verifies the generated UUID is RFC-4122 v4 shaped and
// unique across calls.
func TestGenerateRandomUUID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u, err := generateRandomUUID()
		require.NoError(t, err)
		assert.Regexp(t, re, u, "must be a v4 UUID")
		assert.False(t, seen[u], "UUIDs must be unique")
		seen[u] = true
	}
}

// TestGenerateRandomMAC verifies the MAC is in the locally-administered QEMU
// 52:54:00 space and unique across calls.
func TestGenerateRandomMAC(t *testing.T) {
	re := regexp.MustCompile(`^52:54:00:[0-9a-f]{2}:[0-9a-f]{2}:[0-9a-f]{2}$`)

	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		m, err := generateRandomMAC()
		require.NoError(t, err)
		assert.Regexp(t, re, m, "must be in the 52:54:00 QEMU OUI")
		assert.False(t, seen[m], "MACs should be unique")
		seen[m] = true
	}
}

// TestReplaceFirst verifies replaceFirst only touches the first match.
func TestReplaceFirst(t *testing.T) {
	re := regexp.MustCompile(`<x>.*?</x>`)
	in := `<x>a</x><x>b</x>`
	out := replaceFirst(re, in, "<x>z</x>")
	assert.Equal(t, "<x>z</x><x>b</x>", out)

	// No match → unchanged.
	assert.Equal(t, "no tags", replaceFirst(re, "no tags", "<x>z</x>"))
}

// TestClone_NilProvider verifies the provider-level guard rejects a clone when
// virsh is not wired, without panicking. The happy path requires a live host.
func TestClone_NilProvider(t *testing.T) {
	p := &Provider{}
	_, err := p.Clone(t.Context(), contracts.CloneRequest{SourceVmID: "s", TargetName: "t"})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "not initialized")
}

// TestClone_RequiredFields verifies the provider rejects empty source/target
// with a typed invalid-spec error before touching the host.
func TestClone_RequiredFields(t *testing.T) {
	// virshProvider must be non-nil so we get past the init guard and reach the
	// field validation. A bare VirshProvider never executes here because the
	// validation returns first.
	p := &Provider{virshProvider: &VirshProvider{}}

	_, err := p.Clone(t.Context(), contracts.CloneRequest{TargetName: "t"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source VM ID is required")

	_, err = p.Clone(t.Context(), contracts.CloneRequest{SourceVmID: "s"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target name is required")
}
