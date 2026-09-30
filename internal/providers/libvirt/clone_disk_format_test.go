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
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin that a clone's primary disk is declared in the format it is
// written in — qcow2 — whatever the source's format: a raw source's clone used
// to keep <driver type='raw'> over the qcow2 copy, and did not boot.

// scdPrimaryDriver and scdRawPrimaryDriver are the scd source's primary-disk
// driver, as written by scdDomainXML, and the same for a raw source.
const (
	scdPrimaryDriver    = "<driver name='qemu' type='qcow2'/>\n      <source file='"
	scdRawPrimaryDriver = "<driver name='qemu' type='raw'/>\n      <source file='"
)

// rawSourceXML is scdDomainXML with a raw primary disk (the CD-ROM stays raw
// too, as it always is).
func rawSourceXML(name string, o scdDomainOpts) string {
	return strings.Replace(scdDomainXML(name, o), scdPrimaryDriver, scdRawPrimaryDriver, 1)
}

// primaryDriverOf returns the <driver .../> element of the disk whose source
// is file in domainXML.
func primaryDriverOf(t *testing.T, domainXML, file string) string {
	t.Helper()
	i := strings.Index(domainXML, "<source file='"+file+"'/>")
	require.GreaterOrEqual(t, i, 0, "no disk with source %s in %s", file, domainXML)
	j := strings.LastIndex(domainXML[:i], "<driver")
	require.GreaterOrEqual(t, j, 0)
	return domainXML[j : j+strings.Index(domainXML[j:], ">")+1]
}

func TestSetDiskDriverType(t *testing.T) {
	const disk = "/p/t-disk.qcow2"
	doc := func(driver string) string {
		return "<domain type='kvm'>\n  <name>t</name>\n  <devices>\n" +
			"    <disk type='file' device='disk'>\n      " + driver + "\n      <source file='" + disk + "'/>\n    </disk>\n" +
			"    <disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n      <source file='/p/seed.iso'/>\n    </disk>\n" +
			"  </devices>\n  <metadata><x:disk xmlns:x='urn:x'><driver type='raw'/><source file='" + disk + "'/></x:disk></metadata>\n</domain>"
	}
	for name, tc := range map[string]struct{ in, want string }{
		"raw, self-closing":        {"<driver name='qemu' type='raw'/>", "<driver name='qemu' type='qcow2'/>"},
		"raw, double quotes":       {`<driver name="qemu" type="raw" cache="none"/>`, `<driver name="qemu" type='qcow2' cache="none"/>`},
		"raw, with children":       {"<driver name='qemu' type='raw' io='native'></driver>", "<driver name='qemu' type='qcow2' io='native'></driver>"},
		"no type attribute":        {"<driver name='qemu' cache='none'/>", "<driver name='qemu' cache='none' type='qcow2'/>"},
		"no type, open tag":        {"<driver name='qemu'></driver>", "<driver name='qemu' type='qcow2'></driver>"},
		"already qcow2 (verbatim)": {"<driver  name='qemu'   type='qcow2' />", "<driver  name='qemu'   type='qcow2' />"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := setDiskDriverType(doc(tc.in), disk, cloneDiskFormat)
			require.NoError(t, err)
			assert.Equal(t, doc(tc.want), out, "only the primary disk's driver tag changes")
		})
	}

	t.Run("no driver element", func(t *testing.T) {
		in := "<domain><devices><disk type='file' device='disk'><source file='" + disk + "'/></disk></devices></domain>"
		out, err := setDiskDriverType(in, disk, cloneDiskFormat)
		require.NoError(t, err)
		assert.Equal(t, "<domain><devices><disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='"+disk+
			"'/></disk></devices></domain>", out)
	})
	t.Run("no such disk", func(t *testing.T) {
		in := doc("<driver name='qemu' type='raw'/>")
		out, err := setDiskDriverType(in, "/p/other.qcow2", cloneDiskFormat)
		require.NoError(t, err)
		assert.Equal(t, in, out)
	})
	t.Run("refusals", func(t *testing.T) {
		_, err := setDiskDriverType(doc(""), disk, "qcow2' evil='1")
		assert.Error(t, err, "only a qemu format name is written")
		_, err = setDiskDriverType("<domain><devices>", disk, cloneDiskFormat)
		assert.Error(t, err, "a document that cannot be read is an error")
	})
}

func TestRewriteDomainXMLForClone_RawSourceBecomesQcow2(t *testing.T) {
	const tgt = "/var/lib/libvirt/images/team-a.copy-disk.qcow2"
	out, _, _, err := rewriteDomainXMLForClone(rawSourceXML(scdDomain, scdDomainOpts{}), "team-a.copy", scdDiskPath, tgt)
	require.NoError(t, err)
	assert.Equal(t, "<driver name='qemu' type='qcow2'/>", primaryDriverOf(t, out, tgt))
	assert.Equal(t, "<driver name='qemu' type='raw'/>", primaryDriverOf(t, out, scdSeedISO), "the CD-ROM is untouched")
}

func TestSingleHost_Clone_RawSourceIsDefinedAsQcow2(t *testing.T) {
	fx := newSCDFixture(t, map[string]string{scdDomain: rawSourceXML(scdDomain, scdDomainOpts{})})
	_, err := NewServer(fx.p).Clone(context.Background(), scdFullClone)
	require.NoError(t, err)

	assert.Contains(t, fx.calls(), "local qemu-img convert -f raw -O qcow2 "+scdDiskPath+" "+scdCloneWriteFile,
		"the raw source is read as raw and written as qcow2")
	defined := fx.definedXML("team-a.copy")
	assert.Equal(t, "<driver name='qemu' type='qcow2'/>", primaryDriverOf(t, defined, "/var/lib/libvirt/images/team-a.copy-disk.qcow2"),
		"the clone declares the format its disk is written in")
	assert.Equal(t, "<driver name='qemu' type='raw'/>", primaryDriverOf(t, defined, scdSeedISO))
}

func TestClustered_Clone_RawSourceIsDefinedAsQcow2(t *testing.T) {
	fx := newRoutedSCD(t, map[string]map[string]string{"host-b": {"web": rawSourceXML("web", scdDomainOpts{owner: ownerTeamA})}})
	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	require.NoError(t, err)

	assert.Contains(t, fx.calls(), "local qemu-img convert -f raw -O qcow2 "+scdDiskPath+" "+cloneWriteFile)
	defined := fx.definedXML(cloneTargetDomain)
	assert.Equal(t, "<driver name='qemu' type='qcow2'/>", primaryDriverOf(t, defined, cloneTargetDisk))
}
