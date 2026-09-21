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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// domainWithMetadataNS is a domain whose <metadata> element declares the
// namespaces its children use ON THE <metadata> TAG ITSELF — the exact shape
// libvirtxml's `,innerxml` mapping drops on round-trip (ADR-0008 Fact 6).
const domainWithMetadataNS = `<domain type='kvm'>
  <name>meta-vm</name>
  <uuid>11111111-2222-3333-4444-555555555555</uuid>
  <memory unit='KiB'>4194304</memory>
  <vcpu placement='static'>2</vcpu>
  <metadata xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0" xmlns:cockpit_machines="https://cockpit-project.org/xmlns/cockpit-machines/1.0">
    <libosinfo:libosinfo>
      <libosinfo:os id="http://ubuntu.com/ubuntu/22.04"/>
    </libosinfo:libosinfo>
    <cockpit_machines:data>
      <cockpit_machines:has_install_phase>false</cockpit_machines:has_install_phase>
    </cockpit_machines:data>
  </metadata>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/meta-vm.qcow2'/>
    </disk>
    <interface type='network'>
      <mac address='52:54:00:aa:bb:cc'/>
    </interface>
  </devices>
</domain>`

// TestParseDomainLibvirtxml verifies the typed parse extracts identity/config and
// captures the <metadata> inner content.
func TestParseDomainLibvirtxml(t *testing.T) {
	d, err := parseDomainLibvirtxml(domainWithMetadataNS)
	require.NoError(t, err)
	assert.Equal(t, "meta-vm", d.Name)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", d.UUID)
	require.NotNil(t, d.VCPU)
	assert.Equal(t, uint(2), d.VCPU.Value)
	require.NotNil(t, d.Memory)
	assert.Equal(t, uint(4194304), d.Memory.Value)
	require.NotNil(t, d.Metadata)
	assert.Contains(t, d.Metadata.XML, "libosinfo:os", "inner metadata children are captured")
}

// TestMetadataNamespaceFixRoundTrip is the D2/Q6 carried-fix proof. It demonstrates
// (a) the upstream bug — a naive libvirtxml Marshal DROPS the xmlns:* declarations
// that live on the <metadata> tag, producing namespace-invalid XML — and (b) that
// marshalDomainLibvirtxml restores them. If libvirtxml is ever fixed upstream so
// the naive marshal already preserves them, the "bug still present" assertion here
// fails loudly, which is the signal to drop the carry.
func TestMetadataNamespaceFixRoundTrip(t *testing.T) {
	d, err := parseDomainLibvirtxml(domainWithMetadataNS)
	require.NoError(t, err)

	naive, err := d.Marshal()
	require.NoError(t, err)

	fixed, err := marshalDomainLibvirtxml(d, domainWithMetadataNS)
	require.NoError(t, err)

	metaTag := func(xml string) string {
		i := strings.Index(xml, "<metadata")
		require.GreaterOrEqual(t, i, 0, "marshaled XML has a <metadata> tag")
		j := strings.IndexByte(xml[i:], '>')
		require.GreaterOrEqual(t, j, 0)
		return xml[i : i+j+1]
	}

	naiveTag := metaTag(naive)
	fixedTag := metaTag(fixed)

	// (a) The bug is still present in the pinned libvirtxml: the naive marshal's
	// <metadata> tag lost the namespace declarations.
	if strings.Contains(naiveTag, "xmlns:libosinfo") && strings.Contains(naiveTag, "xmlns:cockpit_machines") {
		t.Fatalf("libvirtxml no longer drops <metadata> namespaces (naive tag=%q) — the carried fix (restoreMetadataNamespaces) and this test can be removed; see ADR-0008 D2/Q6 upstream follow-up", naiveTag)
	}

	// (b) The carried fix restores BOTH declarations on the <metadata> tag.
	assert.Contains(t, fixedTag, `xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0"`, "fix restores libosinfo xmlns")
	assert.Contains(t, fixedTag, `xmlns:cockpit_machines="https://cockpit-project.org/xmlns/cockpit-machines/1.0"`, "fix restores cockpit_machines xmlns")

	// The inner content and the rest of the document survive unchanged.
	assert.Contains(t, fixed, "libosinfo:os")
	assert.Contains(t, fixed, "cockpit_machines:has_install_phase")
	assert.Contains(t, fixed, "meta-vm")
}

// TestRestoreMetadataNamespacesIdempotentAndSafe covers the edge cases of the fix
// in isolation: no <metadata>, a <metadata> with no namespaces, and not
// duplicating a declaration the marshaled output already carries.
func TestRestoreMetadataNamespacesIdempotentAndSafe(t *testing.T) {
	// No <metadata> in source: output unchanged.
	assert.Equal(t, "<domain><name>x</name></domain>",
		restoreMetadataNamespaces("<domain><name>x</name></domain>", "<domain><name>x</name></domain>"))

	// Source <metadata> has no xmlns: output unchanged.
	src := `<domain><metadata><foo/></metadata></domain>`
	assert.Equal(t, `<domain><metadata><bar/></metadata></domain>`,
		restoreMetadataNamespaces(`<domain><metadata><bar/></metadata></domain>`, src))

	// Marshaled output already has the declaration: not duplicated.
	srcNS := `<domain><metadata xmlns:a="urn:a"><a:x/></metadata></domain>`
	marshaledWithNS := `<domain><metadata xmlns:a="urn:a"><a:x/></metadata></domain>`
	got := restoreMetadataNamespaces(marshaledWithNS, srcNS)
	assert.Equal(t, 1, strings.Count(got, `xmlns:a="urn:a"`), "must not duplicate an existing declaration")

	// Missing declaration is injected.
	marshaledNoNS := `<domain><metadata><a:x/></metadata></domain>`
	got = restoreMetadataNamespaces(marshaledNoNS, srcNS)
	assert.Contains(t, got, `xmlns:a="urn:a"`)
	assert.Contains(t, got, "<a:x/>")
}

// TestDomainMemoryMiB verifies the D2 memory-unit contract guard mirrors
// domainxml.go: KiB (or unit-absent) -> MiB, a non-KiB unit is rejected, nil is
// simply absent.
func TestDomainMemoryMiB(t *testing.T) {
	d, err := parseDomainLibvirtxml(domainWithMetadataNS)
	require.NoError(t, err)
	mib, present, err := domainMemoryMiB(d)
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, int64(4096), mib, "4194304 KiB -> 4096 MiB")

	// Unit-absent is treated as KiB (libvirt's formatter always emits KiB).
	noUnit, err := parseDomainLibvirtxml(`<domain><name>x</name><memory>2097152</memory></domain>`)
	require.NoError(t, err)
	mib, present, err = domainMemoryMiB(noUnit)
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, int64(2048), mib)

	// A non-KiB unit is rejected rather than mis-scaled.
	badUnit, err := parseDomainLibvirtxml(`<domain><name>x</name><memory unit='MiB'>4096</memory></domain>`)
	require.NoError(t, err)
	_, _, err = domainMemoryMiB(badUnit)
	assert.Error(t, err, "non-KiB unit must be rejected")

	// Absent <memory> is (0, false, nil), not an error.
	noMem, err := parseDomainLibvirtxml(`<domain><name>x</name></domain>`)
	require.NoError(t, err)
	mib, present, err = domainMemoryMiB(noMem)
	require.NoError(t, err)
	assert.False(t, present)
	assert.Equal(t, int64(0), mib)
}
