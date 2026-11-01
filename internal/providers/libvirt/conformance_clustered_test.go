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
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/knownhosts"

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
// call die without an exit status, as a dropped SSH session does; "failops" makes the commands that change a domain fail cleanly, and
// "dropops" makes the connection die while they run.
const conformanceFakeVirsh = `#!/bin/sh
bindir=$(dirname "$0")
host=local
uri=""
if [ "$1" = "-c" ]; then uri="$2"; host="${2##*/}"; shift 2; fi
printf '%s %s\n' "$host" "$*" >> "$FAKE_VIRSH_DIR/calls.log"
d="$FAKE_VIRSH_DIR/$host"
if [ -f "$d/dead" ]; then kill -9 $$; fi
case "$1" in
  start|destroy|shutdown|undefine)
    if [ -f "$d/dropops" ]; then kill -9 $$; fi
    if [ -f "$d/failops" ]; then echo "error: operation failed: scripted failure" >&2; exit 1; fi ;;
esac
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

var (
	vcpuRE      = regexp.MustCompile(`<vcpu[^>]*>(\d+)</vcpu>`)
	memoryKiBRE = regexp.MustCompile(`<memory[^>]*>(\d+)</memory>`)
)

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
	removed     map[string]bool
	blackholed  map[string]bool
	stallAddr   string
	dialed      []string
	disks       []string
	reg         *hostconn.ClusterRegistry
}

var (
	_ clustered.Target          = (*libvirtConformanceTarget)(nil)
	_ clustered.DialObserver    = (*libvirtConformanceTarget)(nil)
	_ clustered.CreateTemplater = (*libvirtConformanceTarget)(nil)
	_ clustered.HostBlackholer  = (*libvirtConformanceTarget)(nil)
	_ clustered.HostFaulter     = (*libvirtConformanceTarget)(nil)
	_ clustered.HostRemover     = (*libvirtConformanceTarget)(nil)
)

// newLibvirtConformanceTarget builds the fake hosts and the clustered Provider
// over them. t must be a *testing.T: the fixtures it reuses need one.
func newLibvirtConformanceTarget(t clustered.T) clustered.Target {
	tt, ok := t.(*testing.T)
	if !ok {
		t.Errorf("the libvirt conformance adapter must run under *testing.T, not %T", t)
		t.FailNow()
		return nil
	}
	hosts := []string{"host-a", "host-b"}
	fx := newOpsFixture(tt, map[string]map[string]string{"host-a": {}, "host-b": {}})
	// The conformance virsh goes first on PATH; the ops fake it hands unknown
	// commands to sits next to it.
	bin := tt.TempDir()
	require.NoError(tt, os.WriteFile(filepath.Join(bin, "virsh"), []byte(conformanceFakeVirsh), 0o755)) //nolint:gosec // test shim must be executable
	require.NoError(tt, os.WriteFile(filepath.Join(bin, "virsh-ops"), []byte(opsFakeVirsh), 0o755))     //nolint:gosec // test shim must be executable
	tt.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, content := range map[string]string{
		"hostinfo-nodeinfo.txt":        nodeInfoFixture,
		"hostinfo-capabilities.txt":    capabilitiesFixture,
		"hostinfo-domcapabilities.txt": domCapabilitiesFixture,
		"hostinfo-pool-list.txt":       poolListFixture,
	} {
		require.NoError(tt, os.WriteFile(filepath.Join(fx.dir, name), []byte(content), 0o600))
	}

	target := &libvirtConformanceTarget{t: tt, dir: fx.dir, hosts: hosts, unreachable: map[string]bool{}, removed: map[string]bool{}, blackholed: map[string]bool{}}
	// A host that accepts the TCP connection and never completes the SSH
	// handshake, for the hosts the suite blackholes.
	stallAddr, _ := startHandshakeStallServer(tt)
	useTempKnownHosts(tt, knownhosts.Line([]string{stallAddr}, generateTestHostKey(tt).PublicKey())+"\n")
	target.stallAddr = stallAddr
	tt.Cleanup(func() {
		target.mu.Lock()
		defer target.mu.Unlock()
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
		Features: []clustered.Feature{clustered.FeatureCreate, clustered.FeatureHostInventory, clustered.FeatureHostBlackhole,
			clustered.FeatureHostFaults, clustered.FeatureHostRemoval, clustered.FeatureUnreadableStamp},
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

// inventory returns the hosts the Provider is configured with.
func (a *libvirtConformanceTarget) inventory() hostsecret.Inventory {
	inv := hostsecret.Inventory{SchemaVersion: hostsecret.SchemaVersion}
	for _, h := range a.hosts {
		if a.removed[h] {
			continue
		}
		inv.Hosts = append(inv.Hosts, hostsecret.Host{ID: h, Endpoint: "qemu+ssh://virt@" + h + "/system"})
	}
	return inv
}

// Server builds a clustered Provider over the hosts and returns its real gRPC
// Server. Each call is a fresh Provider process over the same host state; the
// registry of the previous one is closed.
func (a *libvirtConformanceTarget) Server() providerv1.ProviderServer {
	dial := func(_ context.Context, h hostsecret.Host) (hostconn.Conn, error) {
		a.mu.Lock()
		a.dialed = append(a.dialed, h.ID)
		down := a.unreachable[h.ID]
		hung := a.blackholed[h.ID]
		a.mu.Unlock()
		if down {
			return nil, fmt.Errorf("dial tcp: connect to %s: connection refused", h.ID)
		}
		if hung {
			// The host answers the TCP connect and never the SSH handshake: a
			// real clustered host connection (boundHostWaits, host_answer.go)
			// that waits for the dial until the call's own deadline margin.
			vp := testVirshProvider("virtrigaud", a.stallAddr, &Credentials{Password: "unused"})
			vp.boundHostWaits = true
			return newClusteredVirshConn(hostconn.HostID(h.ID), vp, func() { _ = vp.Cleanup() }), nil
		}
		return newClusteredVirshConn(hostconn.HostID(h.ID), localHostVP(h.ID), func() {}), nil
	}
	a.mu.Lock()
	inv := a.inventory()
	old := a.reg
	a.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	reg, err := hostconn.NewClusterRegistry(inv, dial, slog.Default())
	if err != nil {
		a.t.Errorf("build the cluster registry: %v", err)
		return NewServer(nil)
	}
	a.t.Cleanup(func() { _ = reg.Close() })
	a.mu.Lock()
	a.reg = reg
	a.mu.Unlock()
	p := &Provider{registry: reg, clusterReg: reg, logger: slog.Default()}
	p.virshProvider = newUnroutableVirshProvider()
	p.createOnHostFn = a.createOnHost
	return NewServer(p)
}

// CreateTemplate gives Create the class every libvirt create needs.
func (a *libvirtConformanceTarget) CreateTemplate() *providerv1.CreateRequest {
	return &providerv1.CreateRequest{ClassJson: `{"CPU":2,"MemoryMiB":1024}`}
}

// domainDir is a host's directory in the fake virsh state.
func (a *libvirtConformanceTarget) domainDir(host string) string { return filepath.Join(a.dir, host) }

// SeedVM defines a domain on a host, stamped with spec.Owner when it has one.
func (a *libvirtConformanceTarget) SeedVM(_ context.Context, spec clustered.SeedSpec) (clustered.VM, error) {
	owner := contracts.ObjectIdentity(spec.Owner)
	if err := a.writeDomain(spec.Host, spec.Name, owner, spec.Running, spec.Stamp); err != nil {
		return clustered.VM{}, err
	}
	return clustered.VM{ID: spec.Name, Host: spec.Host, Name: spec.Name}, nil
}

// writeDomain stores the definition and state of domain name on host.
func (a *libvirtConformanceTarget) writeDomain(host, name string, owner contracts.ObjectIdentity, running bool, stamp clustered.StampState) error {
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
	meta := renderOwnerMetadataXML(owner)
	switch stamp {
	case clustered.StampAmbiguous:
		// Two owner stamps: a hand-edited definition.
		other := contracts.ObjectIdentity{UID: "ffffffff-0000-4000-8000-000000000000", Namespace: "other", Name: name}
		meta = fmt.Sprintf("  <%s>\n    %s\n    %s\n  </%s>\n", domainXMLMetadataElement,
			renderOwnerElementXML(owner), renderOwnerElementXML(other), domainXMLMetadataElement)
	case clustered.StampUnreadable:
		// A stamp element that is cut off: the definition cannot be parsed.
		meta = fmt.Sprintf("  <%s>\n    <%s:%s xmlns:%s='%s' %s='%s'\n", domainXMLMetadataElement,
			ownerMetadataPrefix, ownerMetadataElement, ownerMetadataPrefix, ownerMetadataNamespaceURI, ownerAttrUID, xmlEscape(owner.UID))
	case clustered.StampNormal:
	}
	xml := fmt.Sprintf("<domain type='kvm'>\n  <name>%s</name>\n  <uuid>%s</uuid>\n%s"+
		"  <memory unit='KiB'>2097152</memory>\n  <vcpu placement='static'>2</vcpu>\n"+
		"  <devices>\n    <disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2'/>\n"+
		"      <source file='%s'/>\n      <target dev='vda' bus='virtio'/>\n    </disk>\n  </devices>\n</domain>\n",
		name, uuid, meta, disk)
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
	if m := vcpuRE.FindSubmatch(xml); m != nil {
		_, _ = fmt.Sscan(string(m[1]), &obs.CPU)
	}
	if m := memoryKiBRE.FindSubmatch(xml); m != nil {
		var kib int64
		_, _ = fmt.Sscan(string(m[1]), &kib)
		obs.MemoryMiB = kib / 1024
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
	old := a.reg
	a.reg = nil
	a.mu.Unlock()
	if old != nil {
		return old.Close()
	}
	return nil
}

// marker creates or removes a flag file in a host's directory, which the fake
// virsh reads.
func (a *libvirtConformanceTarget) marker(host, name string, on bool) error {
	hd := a.domainDir(host)
	if err := os.MkdirAll(hd, 0o700); err != nil {
		return err
	}
	f := filepath.Join(hd, name)
	if on {
		return os.WriteFile(f, nil, 0o600)
	}
	if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SetHostBlackholed makes the host's SSH handshake never finish: the Provider's
// next dial of the host gets a real SSH host connection to a server that
// accepts the TCP connection and never speaks SSH, so the Provider's own
// bound on a host that does not answer (host_answer.go) is what ends the call.
// It takes effect when the host is next dialed, so a case sets it before the
// Provider first uses the host.
func (a *libvirtConformanceTarget) SetHostBlackholed(host string, on bool) error {
	a.mu.Lock()
	a.blackholed[host] = on
	a.mu.Unlock()
	return nil
}

// FailHostOperations makes the commands that change a domain fail cleanly.
func (a *libvirtConformanceTarget) FailHostOperations(host string, on bool) error {
	return a.marker(host, "failops", on)
}

// DropHostDuringOperations makes the connection die while those commands run.
func (a *libvirtConformanceTarget) DropHostDuringOperations(host string, on bool) error {
	return a.marker(host, "dropops", on)
}

// RemoveHost reconfigures the Provider's registry without the host, as
// deleting its Host would.
func (a *libvirtConformanceTarget) RemoveHost(host string) error {
	a.mu.Lock()
	a.removed[host] = true
	inv := a.inventory()
	reg := a.reg
	a.mu.Unlock()
	if reg == nil {
		return fmt.Errorf("no running provider registry")
	}
	return reg.Reconcile(inv)
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
	if err := a.writeDomain(string(lease.HostID()), name, req.Owner, false, clustered.StampNormal); err != nil {
		return contracts.CreateResponse{}, err
	}
	return contracts.CreateResponse{ID: name}, nil
}

// libvirtExpectedFailures are the cases clustered libvirt is known to fail,
// each with where the deviation is recorded and a substring of the failure
// the suite must still see. The suite requires them to keep failing in that
// way, so an entry cannot outlive its fix or hide another failure. The
// Recommended cases that libvirt does not meet need no entry: they are
// reported in the run's summary.
var libvirtExpectedFailures = map[string]clustered.ExpectedFailure{
	"create.unavailable-names-host": {
		Pointer: "createRPCError answers a routed Create's HOST_UNAVAILABLE through hostUnavailableStatus, which sets no host metadata, " +
			"where Describe, Delete, Power, Reconfigure and TaskStatus use hostUnavailableStatusFor (contracts.HostUnavailableHostKey). " +
			"Nothing reads the key for a Create today (the manager reads it only on the Delete path), so nothing is lost. Recommended; recorded, not fixed.",
		Contains: "names the host the call was routed to",
	},
	"tasks.forged-inner-ref": {
		Pointer: "hostTaskComplete answers done for any host-local reference of a host the registry fronts: every routed libvirt call is " +
			"synchronous, so the provider issues no task and keeps no table to check one against. Recommended; recorded, not fixed.",
		Contains: "a task that was never issued must not be reported",
	},
}

// TestClusteredLibvirt_Conformance runs the reusable clustered-provider
// conformance suite against clustered libvirt through its real gRPC server.
// A deviation the suite finds is listed in libvirtExpectedFailures with a
// pointer; it must keep failing until the provider is fixed.
func TestClusteredLibvirt_Conformance(t *testing.T) {
	clustered.Run(t, newLibvirtConformanceTarget, clustered.WithExpectedFailures(libvirtExpectedFailures))
}
