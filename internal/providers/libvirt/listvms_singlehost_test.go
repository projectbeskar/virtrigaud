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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// This file pins D9 for ListVMs, which ADR-0007 Addendum A slice 4 routes
// across hosts on a CLUSTERED provider: on a SINGLE-HOST provider ListVMs —
// driven through the gRPC Server, the path production serves — emits exactly
// the virsh command sequence and returns exactly the response and error it did
// before slice 4. The expected results live in
// testdata/single_host_listvms.golden.json, captured by running this test file
// against origin/main at 21dd588 (before any slice 4 change) with
// VIRTRIGAUD_UPDATE_CALLSEQ_GOLDEN=1; the slice 4 branch reproduces it byte for
// byte. A change to any of them — which would restart the ADR-0008 D5 soak
// window — fails here.

// listGoldenFile is the golden single-host ListVMs results.
const listGoldenFile = "testdata/single_host_listvms.golden.json"

// listDomainOpts shapes one listed domain's XML.
type listDomainOpts struct {
	uuid    string
	vcpu    string // "" omits <vcpu>
	memory  string // the <memory> element, verbatim
	owners  []contracts.ObjectIdentity
	disks   []string // "<format>:<path>" file-backed disks
	cdrom   string   // a cloud-init ISO path, "" for none
	macs    []string
	rawBody string // when set, the whole document (e.g. an unparseable one)
}

// listDomainXML renders a `virsh dumpxml` document for name.
func listDomainXML(name string, o listDomainOpts) string {
	if o.rawBody != "" {
		return o.rawBody
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<domain type='kvm'>\n  <name>%s</name>\n", name)
	if o.uuid != "" {
		fmt.Fprintf(&b, "  <uuid>%s</uuid>\n", o.uuid)
	}
	if len(o.owners) > 0 {
		b.WriteString("  <metadata>\n")
		for _, owner := range o.owners {
			b.WriteString("    " + renderOwnerElementXML(owner) + "\n")
		}
		b.WriteString("  </metadata>\n")
	}
	if o.memory != "" {
		b.WriteString("  " + o.memory + "\n")
	}
	if o.vcpu != "" {
		fmt.Fprintf(&b, "  <vcpu placement='static'>%s</vcpu>\n", o.vcpu)
	}
	b.WriteString("  <devices>\n")
	for i, d := range o.disks {
		format, path, _ := strings.Cut(d, ":")
		fmt.Fprintf(&b, "    <disk type='file' device='disk'>\n      <driver name='qemu' type='%s'/>\n"+
			"      <source file='%s'/>\n      <target dev='vd%c' bus='virtio'/>\n    </disk>\n", format, path, rune('a'+i))
	}
	if o.cdrom != "" {
		fmt.Fprintf(&b, "    <disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n"+
			"      <source file='%s'/>\n      <target dev='sda' bus='sata'/>\n      <readonly/>\n    </disk>\n", o.cdrom)
	}
	for _, mac := range o.macs {
		fmt.Fprintf(&b, "    <interface type='network'>\n      <mac address='%s'/>\n"+
			"      <source network='default'/>\n      <model type='virtio'/>\n    </interface>\n", mac)
	}
	b.WriteString("  </devices>\n</domain>\n")
	return b.String()
}

// listRow is one `virsh list --all` row.
type listRow struct{ id, name, state string }

// listTable renders `virsh list --all` output for rows.
func listTable(rows ...listRow) string {
	var b strings.Builder
	b.WriteString(" Id   Name                  State\n-------------------------------------\n")
	for _, r := range rows {
		fmt.Fprintf(&b, " %-4s %-21s %s\n", r.id, r.name, r.state)
	}
	return b.String()
}

// listScenario is one single-host ListVMs call whose results are pinned.
type listScenario struct {
	name    string
	domains map[string]string
	list    string // the `virsh list --all` output; "" removes it (list fails)
	setup   func(fx *scdFixture)
}

// singleHostListScenarios covers every branch of the single-host ListVMs a
// fake host can drive.
func singleHostListScenarios() []listScenario {
	web := listDomainXML("legacy-web", listDomainOpts{
		uuid: "aaaaaaaa-1111-4111-8111-111111111111", vcpu: "2",
		memory: "<memory unit='KiB'>2097152</memory>",
		disks:  []string{"qcow2:/var/lib/libvirt/images/legacy-web-disk.qcow2"},
		cdrom:  "/tmp/virtrigaud-cloudinit/legacy-web/cloud-init.iso",
		macs:   []string{"52:54:00:aa:00:01"},
	})
	db := listDomainXML("team-a.db", listDomainOpts{
		uuid: "bbbbbbbb-2222-4222-8222-222222222222", vcpu: "4",
		memory: "<memory unit='KiB'>8388608</memory>",
		owners: []contracts.ObjectIdentity{ownerTeamA},
		disks: []string{
			"qcow2:/var/lib/libvirt/images/team-a.db-disk.qcow2",
			"raw:/var/lib/libvirt/images/team-a.db-data.img",
		},
		macs: []string{"52:54:00:bb:00:01", "52:54:00:bb:00:02"},
	})
	noCPU := listDomainXML("no-vcpu", listDomainOpts{
		uuid: "cccccccc-3333-4333-8333-333333333333", memory: "<memory>1048576</memory>",
		disks: []string{":/var/lib/libvirt/images/no-vcpu.qcow2"},
	})
	paused := listDomainXML("paused-vm", listDomainOpts{
		uuid: "dddddddd-4444-4444-8444-444444444444", vcpu: "1",
		memory: "<memory unit='KiB'>524288</memory>",
	})
	twoOwners := listDomainXML("hand-edited", listDomainOpts{
		uuid: "eeeeeeee-5555-4555-8555-555555555555", vcpu: "1",
		memory: "<memory unit='KiB'>1048576</memory>",
		owners: []contracts.ObjectIdentity{ownerTeamA, ownerTeamB},
	})
	mibUnit := listDomainXML("mib-unit", listDomainOpts{
		uuid: "ffffffff-6666-4666-8666-666666666666", vcpu: "1",
		memory: "<memory unit='MiB'>1024</memory>",
	})
	garbage := listDomainXML("garbage", listDomainOpts{rawBody: "<domain type='kvm'><name>garbage</name>"})

	return []listScenario{
		{name: "empty-host", domains: map[string]string{}, list: listTable()},
		{
			name:    "mixed-domains",
			domains: map[string]string{"legacy-web": web, "team-a.db": db, "no-vcpu": noCPU, "paused-vm": paused},
			list: listTable(
				listRow{"3", "legacy-web", "running"},
				listRow{"-", "team-a.db", "shut off"},
				listRow{"-", "no-vcpu", "shut off"},
				listRow{"5", "paused-vm", "paused"},
			),
		},
		{
			name:    "dumpxml-missing-for-one",
			domains: map[string]string{"legacy-web": web},
			list:    listTable(listRow{"-", "ghost", "shut off"}, listRow{"3", "legacy-web", "running"}),
		},
		{
			name:    "unparseable-domain-skipped",
			domains: map[string]string{"garbage": garbage, "team-a.db": db},
			list:    listTable(listRow{"-", "garbage", "shut off"}, listRow{"-", "team-a.db", "shut off"}),
		},
		{
			name:    "non-kib-memory-skipped",
			domains: map[string]string{"mib-unit": mibUnit, "paused-vm": paused},
			list:    listTable(listRow{"-", "mib-unit", "shut off"}, listRow{"5", "paused-vm", "paused"}),
		},
		{
			name:    "two-owner-stamps",
			domains: map[string]string{"hand-edited": twoOwners},
			list:    listTable(listRow{"-", "hand-edited", "shut off"}),
		},
		{
			name:    "list-fails",
			domains: map[string]string{"legacy-web": web},
			list:    "",
		},
		{
			name:    "dumpxml-fails-for-all",
			domains: map[string]string{"legacy-web": web, "team-a.db": db},
			list:    listTable(listRow{"3", "legacy-web", "running"}, listRow{"-", "team-a.db", "shut off"}),
			setup:   func(fx *scdFixture) { fx.script("single", "fail-dumpxml", "") },
		},
		{
			name:    "no-header-separator",
			domains: map[string]string{"legacy-web": web},
			list:    "legacy-web running\n",
		},
	}
}

// TestSingleHost_ListVMs_ResultsUnchanged is the single-host equivalence proof
// for slice 4: every scenario's virsh command sequence, response and error
// equal what origin/main produced before the slice.
func TestSingleHost_ListVMs_ResultsUnchanged(t *testing.T) {
	got := map[string]scdResult{}
	for _, sc := range singleHostListScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			fx := newSCDFixture(t, sc.domains)
			listPath := filepath.Join(fx.dir, "single", "list.txt")
			if sc.list == "" {
				require.NoError(t, os.Remove(listPath))
			} else {
				require.NoError(t, os.WriteFile(listPath, []byte(sc.list), 0o600))
			}
			if sc.setup != nil {
				sc.setup(fx)
			}
			require.False(t, fx.p.clustered())
			res := scdResult{}
			resp, err := NewServer(fx.p).ListVMs(context.Background(), &providerv1.ListVMsRequest{})
			if err != nil {
				res.Err = fx.normalize(err.Error())
			}
			if resp != nil {
				b, merr := json.Marshal(resp)
				require.NoError(t, merr)
				res.Resp = fx.normalize(string(b))
			}
			res.Calls = fx.calls()
			got[sc.name] = res
		})
	}

	if os.Getenv(callSeqUpdateEnv) == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(listGoldenFile), 0o750))
		require.NoError(t, os.WriteFile(listGoldenFile, append(b, '\n'), 0o600))
		t.Logf("rewrote %s", listGoldenFile)
		return
	}

	raw, err := os.ReadFile(listGoldenFile)
	require.NoError(t, err)
	want := map[string]scdResult{}
	require.NoError(t, json.Unmarshal(raw, &want))
	require.Len(t, got, len(want), "every golden scenario is still exercised")
	for name, w := range want {
		g, ok := got[name]
		require.True(t, ok, "scenario %s not run", name)
		assert.Equal(t, w.Calls, g.Calls, "%s: the single-host command sequence changed", name)
		assert.Equal(t, w.Resp, g.Resp, "%s: the single-host response changed", name)
		assert.Equal(t, w.Err, g.Err, "%s: the single-host error changed", name)
	}
}
