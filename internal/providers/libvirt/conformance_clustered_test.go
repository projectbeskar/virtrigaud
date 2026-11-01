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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
)

// This file is the clustered libvirt adapter of the reusable clustered-provider
// conformance suite (test/conformance/clustered). The suite drives this
// package's real gRPC Server through a gRPC client; the adapter only builds
// the world underneath it, with the test seams the package already has: a
// *Provider over a ClusterRegistry whose hosts are real VirshProviders on
// LOCAL per-host URIs, a fake `virsh` on PATH that keeps each host's domains
// in a directory, and the createOnHostFn seam for Create. No hypervisor, no
// libvirtd, no ssh.

// conformanceFakeVirsh is a stateful fake `virsh` for the conformance suite.
// It routes on the -c URI (the host tag is the URI path) and keeps each host's
// domains in its directory: dom-<name|uuid>.xml and state-<name>. It answers
// list, dumpxml, dominfo, domstate, start, destroy, shutdown and undefine from
// that state (with /bin/rm, because PATH holds the logging rm shim of the ops
// fixture), so a power operation or a delete is visible to the next read,
// and hands every other command to the shared ops fake (opsFakeVirsh, installed
// next to it as virsh-ops). A host directory containing "dead" makes every
// call die without an exit status, as a dropped SSH session does.
const conformanceFakeVirsh = `#!/bin/sh
bindir=$(dirname "$0")
host=local
uri=""
if [ "$1" = "-c" ]; then uri="$2"; host="${2##*/}"; shift 2; fi
printf '%s %s\n' "$host" "$*" >> "$FAKE_VIRSH_DIR/calls.log"
d="$FAKE_VIRSH_DIR/$host"
if [ -f "$d/dead" ]; then kill -9 $$; fi
nodom() { echo "error: failed to get domain '$1'" >&2; exit 1; }
uuid_of() { sed -n 's:.*<uuid>\(.*\)</uuid>.*:\1:p' "$d/dom-$1.xml" | head -1; }
name_of() { sed -n 's:.*<name>\(.*\)</name>.*:\1:p' "$d/dom-$1.xml" | head -1; }
state_of() { n=$(name_of "$1"); if [ -f "$d/state-$n" ]; then cat "$d/state-$n"; else echo "shut off"; fi; }
setstate() { n=$(name_of "$1"); printf '%s\n' "$2" > "$d/state-$n"; }
ops() { if [ -n "$uri" ]; then exec "$bindir/virsh-ops" -c "$uri" "$@"; else exec "$bindir/virsh-ops" "$@"; fi; }
case "$1" in
  list)
    if [ "$3" = "--uuid" ]; then
      for f in "$d"/dom-????????-????-????-????-????????????.xml; do
        [ -e "$f" ] || continue; b=${f##*/dom-}; echo "${b%.xml}"
      done
    else
      printf ' %-4s %-40s %s\n' Id Name State; echo "------------------------------------------------------"
      for f in "$d"/dom-*.xml; do
        [ -e "$f" ] || continue; b=${f##*/dom-}; b=${b%.xml}
        case "$b" in ????????-????-????-????-????????????) continue ;; esac
        s=$(state_of "$b")
        id=-; if [ "$s" = "running" ]; then id=1; fi
        printf ' %-4s %-40s %s\n' "$id" "$b" "$s"
      done
    fi ;;
  dumpxml)
    case "$*" in *--inactive*) ops "$@" ;; esac
    if [ -f "$d/dom-$2.xml" ]; then cat "$d/dom-$2.xml"; else nodom "$2"; fi ;;
  dominfo)
    [ -f "$d/dom-$2.xml" ] || nodom "$2"
    s=$(state_of "$2"); id=-; if [ "$s" = "running" ]; then id=1; fi
    printf 'Id:             %s\nName:           %s\nUUID:           %s\nState:          %s\nCPU(s):         2\nMax memory:     2097152 KiB\nUsed memory:    2097152 KiB\n' "$id" "$(name_of "$2")" "$(uuid_of "$2")" "$s" ;;
  domstate) [ -f "$d/dom-$2.xml" ] || nodom "$2"; state_of "$2" ;;
  start)
    [ -f "$d/dom-$2.xml" ] || nodom "$2"
    if [ "$(state_of "$2")" = "running" ]; then echo "error: Domain is already active" >&2; exit 1; fi
    setstate "$2" running ;;
  destroy|shutdown)
    [ -f "$d/dom-$2.xml" ] || nodom "$2"
    if [ "$(state_of "$2")" != "running" ]; then echo "error: Requested operation is not valid: domain is not running" >&2; exit 1; fi
    setstate "$2" "shut off" ;;
  undefine)
    [ -f "$d/dom-$2.xml" ] || nodom "$2"
    n=$(name_of "$2"); u=$(uuid_of "$2")
    /bin/rm -f "$d/dom-$n.xml" "$d/dom-$u.xml" "$d/state-$n" ;;
  nodeinfo|capabilities|domcapabilities)
    cat "$FAKE_VIRSH_DIR/hostinfo-$1.txt" ;;
  pool-list) cat "$FAKE_VIRSH_DIR/hostinfo-pool-list.txt" ;;
  *) ops "$@" ;;
esac
`

var ownerUIDAttrRE = regexp.MustCompile(ownerAttrUID + `='([^']*)'`)

// libvirtConformanceTarget adapts the clustered libvirt Provider to
// clustered.Target.
type libvirtConformanceTarget struct {
	t     *testing.T
	dir   string
	hosts []string
	seq   int

	mu          sync.Mutex
	unreachable map[string]bool
	dialed      []string
	disks       []string
}

var (
	_ clustered.Target       = (*libvirtConformanceTarget)(nil)
	_ clustered.DialObserver = (*libvirtConformanceTarget)(nil)
)

// newLibvirtConformanceTarget builds the fake hosts and the clustered Provider
// over them. t must be a *testing.T: the fixtures it reuses need one.
func newLibvirtConformanceTarget(t clustered.T) clustered.Target {
	tt, ok := t.(*testing.T)
	if !ok {
		panic("the libvirt conformance adapter must run under *testing.T")
	}
	hosts := []string{"host-a", "host-b"}
	fx := newOpsFixture(tt, map[string]map[string]string{"host-a": {}, "host-b": {}})
	bin, err := exec.LookPath("virsh")
	require.NoError(tt, err)
	bindir := filepath.Dir(bin)
	require.NoError(tt, os.Rename(bin, filepath.Join(bindir, "virsh-ops")))
	require.NoError(tt, os.WriteFile(bin, []byte(conformanceFakeVirsh), 0o755)) //nolint:gosec // test shim must be executable
	for name, content := range map[string]string{
		"hostinfo-nodeinfo.txt":        nodeInfoFixture,
		"hostinfo-capabilities.txt":    capabilitiesFixture,
		"hostinfo-domcapabilities.txt": domCapabilitiesFixture,
		"hostinfo-pool-list.txt":       poolListFixture,
	} {
		require.NoError(tt, os.WriteFile(filepath.Join(fx.dir, name), []byte(content), 0o600))
	}

	target := &libvirtConformanceTarget{t: tt, dir: fx.dir, hosts: hosts, unreachable: map[string]bool{}}
	tt.Cleanup(func() {
		for _, d := range target.disks {
			_ = os.Remove(d)
		}
	})
	return target
}

// Name labels the adapter in reports.
func (a *libvirtConformanceTarget) Name() string { return "libvirt-clustered" }

// Capabilities declares what clustered libvirt offers, with the reason for
// each optional behaviour it does not.
func (a *libvirtConformanceTarget) Capabilities() clustered.Capabilities {
	return clustered.Capabilities{
		Features: []clustered.Feature{clustered.FeatureCreate, clustered.FeatureHostInventory},
		Reasons: map[clustered.Feature]string{
			clustered.FeatureAsyncTasks: "every routed libvirt call is synchronous and returns no task; " +
				"the host-encoded task reference is in place for a future asynchronous call (hostTaskComplete, ADR-0007 Addendum A slice 3)",
			clustered.FeatureVMNotOwnedReason: "clustered libvirt refuses another owner's VM with a plain NotFound (the contract's requirement); " +
				"the VM_NOT_OWNED reason is a refinement only the Proxmox provider adds",
			clustered.FeatureCreateOwnerRebind: "Create runs through the createOnHostFn seam here, because the create pipeline needs libvirtd; " +
				"the idempotent rebind and the AlreadyExists refusal are pinned against the real pipeline by create_*_test.go and server_create_owner_test.go",
			clustered.FeatureProviderNotReady: "the clustered Provider has no seam that makes it unready without racing its server",
		},
	}
}

// Hosts returns the fronted host ids.
func (a *libvirtConformanceTarget) Hosts() []string { return append([]string(nil), a.hosts...) }

// Server builds a clustered Provider over the hosts and returns its real gRPC
// Server. Each call is a fresh Provider process over the same host state.
func (a *libvirtConformanceTarget) Server() providerv1.ProviderServer {
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	for _, h := range a.hosts {
		inv.Hosts = append(inv.Hosts, hostsecret.Host{ID: h, Endpoint: "qemu+ssh://virt@" + h + "/system"})
	}
	dial := func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) {
		a.mu.Lock()
		a.dialed = append(a.dialed, h.ID)
		down := a.unreachable[h.ID]
		a.mu.Unlock()
		if down {
			return nil, fmt.Errorf("dial tcp: connect to %s: connection refused", h.ID)
		}
		return newClusteredVirshConn(hostconn.HostID(h.ID), localHostVP(h.ID), func() {}), nil
	}
	p, _ := newClusteredProviderForTest(a.t, inv, dial)
	p.virshProvider = newUnroutableVirshProvider()
	p.createOnHostFn = a.createOnHost
	return NewServer(p)
}

// domainDir is a host's directory in the fake virsh state.
func (a *libvirtConformanceTarget) domainDir(host string) string { return filepath.Join(a.dir, host) }

// SeedVM defines a domain on a host, stamped with spec.Owner when it has one.
func (a *libvirtConformanceTarget) SeedVM(_ context.Context, spec clustered.SeedSpec) (clustered.VM, error) {
	owner := contracts.ObjectIdentity(spec.Owner)
	if err := a.writeDomain(spec.Host, spec.Name, owner, spec.Running); err != nil {
		return clustered.VM{}, err
	}
	return clustered.VM{ID: spec.Name, Host: spec.Host, Name: spec.Name}, nil
}

// writeDomain stores the definition and state of domain name on host.
func (a *libvirtConformanceTarget) writeDomain(host, name string, owner contracts.ObjectIdentity, running bool) error {
	a.mu.Lock()
	a.seq++
	n := a.seq
	a.mu.Unlock()
	hd := a.domainDir(host)
	if err := os.MkdirAll(hd, 0o700); err != nil {
		return err
	}
	uuid := fmt.Sprintf("5eab%04x-0000-4000-8000-%012x", n, n)
	disk := filepath.Join(fixtureImagesDir, strings.ReplaceAll(host+"-"+name, "/", "_")+".qcow2")
	if err := os.WriteFile(disk, []byte("disk"), 0o600); err != nil {
		return err
	}
	a.mu.Lock()
	a.disks = append(a.disks, disk)
	a.mu.Unlock()
	xml := fmt.Sprintf("<domain type='kvm'>\n  <name>%s</name>\n  <uuid>%s</uuid>\n%s"+
		"  <memory unit='KiB'>2097152</memory>\n  <vcpu placement='static'>2</vcpu>\n"+
		"  <devices>\n    <disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2'/>\n"+
		"      <source file='%s'/>\n      <target dev='vda' bus='virtio'/>\n    </disk>\n  </devices>\n</domain>\n",
		name, uuid, renderOwnerMetadataXML(owner), disk)
	for _, f := range []string{"dom-" + name + ".xml", "dom-" + uuid + ".xml"} {
		if err := os.WriteFile(filepath.Join(hd, f), []byte(xml), 0o600); err != nil {
			return err
		}
	}
	state := "shut off"
	if running {
		state = "running"
	}
	return os.WriteFile(filepath.Join(hd, "state-"+name), []byte(state+"\n"), 0o600)
}

// Observe reads the domain's definition and state from the host's directory,
// not through the Provider.
func (a *libvirtConformanceTarget) Observe(_ context.Context, vm clustered.VM) (clustered.Observation, error) {
	hd := a.domainDir(vm.Host)
	xml, err := os.ReadFile(filepath.Join(hd, "dom-"+vm.ID+".xml")) //nolint:gosec // test reads its own fixture
	if os.IsNotExist(err) {
		return clustered.Observation{}, nil
	}
	if err != nil {
		return clustered.Observation{}, err
	}
	obs := clustered.Observation{Exists: true}
	if m := ownerUIDAttrRE.FindSubmatch(xml); m != nil {
		obs.OwnerUID = string(m[1])
	}
	if st, err := os.ReadFile(filepath.Join(hd, "state-"+vm.ID)); err == nil { //nolint:gosec // test reads its own fixture
		obs.Running = strings.TrimSpace(string(st)) == "running"
	}
	return obs, nil
}

// SetHostReachable makes a host stop answering. A host that was never dialed
// refuses the dial; one that was already dialed has its virsh die without an
// exit status, as a dropped SSH session does.
func (a *libvirtConformanceTarget) SetHostReachable(host string, reachable bool) error {
	a.mu.Lock()
	a.unreachable[host] = !reachable
	a.mu.Unlock()
	hd := a.domainDir(host)
	if err := os.MkdirAll(hd, 0o700); err != nil {
		return err
	}
	dead := filepath.Join(hd, "dead")
	if reachable {
		if err := os.Remove(dead); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(dead, nil, 0o600)
}

// Restart drops the Provider and its registry (its connections and every
// in-memory table). The hosts' domains stay.
func (a *libvirtConformanceTarget) Restart() error {
	a.mu.Lock()
	a.dialed = nil
	a.mu.Unlock()
	return nil
}

// DialedHosts returns the host ids the Provider's registry dialed.
func (a *libvirtConformanceTarget) DialedHosts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.dialed...)
}

// createOnHost is the createOnHostFn seam: it defines the domain on the leased
// host, stamped with the request's owner. The create pipeline itself needs a
// libvirtd (see the file comment); the routing around it, which is what the
// suite checks, is the production code.
func (a *libvirtConformanceTarget) createOnHost(_ context.Context, lease hostconn.Conn, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	name, err := createDomainName(req)
	if err != nil {
		return contracts.CreateResponse{}, err
	}
	if err := a.writeDomain(string(lease.HostID()), name, req.Owner, false); err != nil {
		return contracts.CreateResponse{}, err
	}
	return contracts.CreateResponse{ID: name}, nil
}

// libvirtExpectedFailures are the cases clustered libvirt is known to fail,
// each with where the deviation is recorded. The suite requires them to keep
// failing, so an entry cannot outlive its fix.
var libvirtExpectedFailures = map[string]string{
	"create.unavailable-names-host": "createRPCError answers a routed Create's HOST_UNAVAILABLE with hostUnavailableStatus(pe), " +
		"which sets no metadata, where Describe, Delete, Power, Reconfigure and TaskStatus name the host through hostUnavailableStatusFor " +
		"(contracts.HostUnavailableHostKey, host_unavailable.go). Only the manager's status text is affected: it names the host only " +
		"when the key is present. Recorded, not fixed, by the conformance change.",
}

// TestClusteredLibvirt_Conformance runs the reusable clustered-provider
// conformance suite against clustered libvirt through its real gRPC server.
// A deviation the suite finds is listed in libvirtExpectedFailures with a
// pointer; it must keep failing until the provider is fixed.
func TestClusteredLibvirt_Conformance(t *testing.T) {
	clustered.Run(t, newLibvirtConformanceTarget, clustered.WithExpectedFailures(libvirtExpectedFailures))
}
