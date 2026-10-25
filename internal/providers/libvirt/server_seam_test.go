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
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests are the proof of the ADR-0008 PR 2 de-weld: the six RPCs that used
// to type-assert s.provider.(*Provider) now run against a fake providerBackend +
// fake libvirtConn, with NO concrete *Provider anywhere. If the gRPC layer still
// depended on the concrete type, this file would not compile.

// fakeSeamConn is a libvirtConn implementation with no live host, used to drive
// the snapshot RPCs through the seam and record the virsh commands issued.
type fakeSeamConn struct {
	id           hostconn.HostID
	domainState  string
	domainErr    error
	snapExists   bool
	snapErr      error
	virshErr     error
	virshCalls   [][]string
	runHostErr   error
	runHostOut   string
	uriVal       string // defaults to "qemu+ssh://user@host/system" via uri() below when empty
	streamInErr  error
	streamInArgs []string
	copyCalls    int    // copyDiskToRemote calls
	copied       []byte // what the last copyDiskToRemote read from its source
}

func (f *fakeSeamConn) HostID() hostconn.HostID { return f.id }

// seamDomainUUID is the UUID of the one domain a fakeSeamConn host defines.
const seamDomainUUID = "5eab0000-0000-4000-8000-000000000001"

func (f *fakeSeamConn) Virsh(_ context.Context, args ...string) (*hostconn.Result, error) {
	f.virshCalls = append(f.virshCalls, args)
	if f.virshErr != nil {
		return nil, f.virshErr
	}
	// The snapshot RPCs' disk dependency guard reads the domain and lists the
	// host's domains: this host has just the one, so nothing depends on it.
	if len(args) > 0 {
		switch args[0] {
		case "dumpxml":
			return &hostconn.Result{Stdout: fmt.Sprintf("<domain><name>%s</name><uuid>%s</uuid><devices>"+
				"<disk type='file' device='disk'><source file='/var/lib/libvirt/images/%s-disk.qcow2'/></disk>"+
				"</devices></domain>", args[1], seamDomainUUID, args[1])}, nil
		case "list":
			return &hostconn.Result{Stdout: seamDomainUUID + "\n"}, nil
		case "snapshot-dumpxml":
			// SnapshotRevert reads the snapshot before reverting it
			// (snapshot_readonly_disks.go): an external disk-only snapshot
			// of the one disk.
			return &hostconn.Result{Stdout: fmt.Sprintf("<domainsnapshot><name>%s</name><disks>"+
				"<disk name='vda' snapshot='external' type='file'><source file='/var/lib/libvirt/images/%s-disk.%s'/></disk>"+
				"</disks><domain><devices/></domain></domainsnapshot>", args[2], args[1], args[2])}, nil
		}
	}
	return &hostconn.Result{Stdout: "ok"}, nil
}

func (f *fakeSeamConn) RunHost(_ context.Context, argv ...string) (*hostconn.Result, error) {
	if f.runHostErr != nil {
		return nil, f.runHostErr
	}
	if len(argv) > 0 && argv[0] == "realpath" {
		// `realpath -m -z -- <paths>`: already canonical here.
		var out strings.Builder
		for i, a := range argv {
			if a == "--" {
				for _, p := range argv[i+1:] {
					out.WriteString(p + "\x00")
				}
				break
			}
		}
		return &hostconn.Result{Stdout: out.String()}, nil
	}
	if f.runHostOut == "" && slices.Contains(argv, "qemu-img") && slices.Contains(argv, "info") {
		// A SnapshotDelete reads the snapshot's overlay header for internal
		// snapshots (snapshot_delete_guard.go): a plain qcow2 here.
		return &hostconn.Result{Stdout: `{"format":"qcow2"}`}, nil
	}
	return &hostconn.Result{Stdout: f.runHostOut}, nil
}

// virshCallOf returns the args of the first virsh command sub the conn saw.
func virshCallOf(t *testing.T, c *fakeSeamConn, sub string) []string {
	t.Helper()
	for _, args := range c.virshCalls {
		if len(args) > 0 && args[0] == sub {
			return args
		}
	}
	require.Failf(t, "missing virsh call", "no %q among %v", sub, c.virshCalls)
	return nil
}

func (f *fakeSeamConn) Stream(_ context.Context, _ ...string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeSeamConn) Libvirt(_ context.Context) (*golibvirt.Libvirt, error) {
	return nil, errors.New("fakeSeamConn: Libvirt not implemented")
}

func (f *fakeSeamConn) callLibvirt(_ context.Context, _ func(*golibvirt.Libvirt) error) error {
	return errors.New("fakeSeamConn: callLibvirt not implemented")
}

func (f *fakeSeamConn) Close() error { return nil }

func (f *fakeSeamConn) getDomainState(_ context.Context, _ string) (string, error) {
	return f.domainState, f.domainErr
}

func (f *fakeSeamConn) snapshotExists(_ context.Context, _, _ string) (bool, error) {
	return f.snapExists, f.snapErr
}

func (f *fakeSeamConn) uri() string {
	if f.uriVal != "" {
		return f.uriVal
	}
	return "qemu+ssh://user@host/system"
}

func (f *fakeSeamConn) copyDiskToRemote(_ context.Context, src io.Reader, _ string) (string, error) {
	f.copyCalls++
	b, err := io.ReadAll(src)
	if err != nil {
		return "", err
	}
	f.copied = b
	return "/var/lib/libvirt/images/x.qcow2", nil
}

func (f *fakeSeamConn) storageProvider() *StorageProvider { return nil }

func (f *fakeSeamConn) StreamIn(_ context.Context, _ io.Reader, argv ...string) error {
	f.streamInArgs = argv
	return f.streamInErr
}

// fakeSeamProvider is a providerBackend that is NOT a *Provider — the whole point.
// It hands the Server a fake connection and scripts Clone/imagePrepare.
type fakeSeamProvider struct {
	providerBackend // embedded (nil): base contract methods are never called here

	hostConn  libvirtConn
	connErr   error
	cloneReq  contracts.CloneRequest
	cloneResp contracts.CloneResponse
	cloneErr  error
	prepID    string
	prepPath  string
	prepErr   error
	overlays  []string
}

// clustered reports single-host topology: this fake stands in for a single-host
// backend (the Server gates clustered-only routing on it).
func (f *fakeSeamProvider) clustered() bool { return false }

func (f *fakeSeamProvider) conn(_ context.Context) (libvirtConn, error) {
	return f.hostConn, f.connErr
}

func (f *fakeSeamProvider) Clone(_ context.Context, req contracts.CloneRequest) (contracts.CloneResponse, error) {
	f.cloneReq = req
	return f.cloneResp, f.cloneErr
}

// removeSnapshotOverlays records the overlays a SnapshotDelete handed over for
// cleanup; the fake host has no files to remove.
func (f *fakeSeamProvider) removeSnapshotOverlays(_ context.Context, _ libvirtConn, _, _ string, overlays []string) {
	f.overlays = append(f.overlays, overlays...)
}

func (f *fakeSeamProvider) imagePrepare(_ context.Context, _ imageartifact.Request, _, _ string) (imagePrepareResult, error) {
	return imagePrepareResult{ID: f.prepID, Path: f.prepPath}, f.prepErr
}

// TestServer_SnapshotCreate_ThroughSeam proves SnapshotCreate reaches the host
// via conn.getDomainState + conn.Virsh (no *Provider assertion). A running VM
// with IncludeMemory yields a memory snapshot: snapshot-create-as WITHOUT
// --disk-only.
func TestServer_SnapshotCreate_ThroughSeam(t *testing.T) {
	fc := &fakeSeamConn{id: "host-a", domainState: "running"}
	s := &Server{provider: &fakeSeamProvider{hostConn: fc}}

	resp, err := s.SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{
		VmId:          "vm-1",
		NameHint:      "snap-mem",
		IncludeMemory: true,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "snap-mem", resp.SnapshotId)

	args := virshCallOf(t, fc, "snapshot-create-as")
	assert.Equal(t, "snapshot-create-as", args[0])
	assert.NotContains(t, args, "--disk-only", "a running VM + IncludeMemory must be a full (memory) snapshot")
}

// TestServer_SnapshotCreate_StoppedDowngradesToDiskOnly verifies the honest
// downgrade path still runs through the seam: a stopped VM gets a --disk-only
// snapshot even when IncludeMemory is requested.
func TestServer_SnapshotCreate_StoppedDowngradesToDiskOnly(t *testing.T) {
	fc := &fakeSeamConn{id: "host-a", domainState: "shut off"}
	s := &Server{provider: &fakeSeamProvider{hostConn: fc}}

	_, err := s.SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{
		VmId:          "vm-1",
		NameHint:      "snap-x",
		IncludeMemory: true,
	})
	require.NoError(t, err)
	assert.Contains(t, virshCallOf(t, fc, "snapshot-create-as"), "--disk-only")
}

// TestServer_SnapshotDelete_ThroughSeam proves SnapshotDelete reaches the host
// via conn.snapshotExists + conn.Virsh.
func TestServer_SnapshotDelete_ThroughSeam(t *testing.T) {
	fc := &fakeSeamConn{id: "host-a", snapExists: true}
	s := &Server{provider: &fakeSeamProvider{hostConn: fc}}

	_, err := s.SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{
		VmId:       "vm-1",
		SnapshotId: "snap-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "snapshot-delete", virshCallOf(t, fc, "snapshot-delete")[0])
}

// TestServer_SnapshotDelete_MissingIsSuccess verifies the seam preserves the
// "delete a non-existent snapshot is a no-op success" behaviour (no virsh call).
func TestServer_SnapshotDelete_MissingIsSuccess(t *testing.T) {
	fc := &fakeSeamConn{id: "host-a", snapExists: false}
	s := &Server{provider: &fakeSeamProvider{hostConn: fc}}

	_, err := s.SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{
		VmId:       "vm-1",
		SnapshotId: "ghost",
	})
	require.NoError(t, err)
	assert.Empty(t, fc.virshCalls, "no snapshot-delete should be issued for a missing snapshot")
}

// TestServer_SnapshotRevert_ThroughSeam proves SnapshotRevert reaches the host
// via conn.snapshotExists + conn.getDomainState + conn.Virsh, keeping a running
// VM running (--running).
func TestServer_SnapshotRevert_ThroughSeam(t *testing.T) {
	fc := &fakeSeamConn{id: "host-a", snapExists: true, domainState: "running"}
	s := &Server{provider: &fakeSeamProvider{hostConn: fc}}

	_, err := s.SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{
		VmId:       "vm-1",
		SnapshotId: "snap-1",
	})
	require.NoError(t, err)
	args := virshCallOf(t, fc, "snapshot-revert")
	assert.Equal(t, "snapshot-revert", args[0])
	assert.Contains(t, args, "--running")
}

// TestServer_Clone_ThroughSeam proves Clone reaches s.provider.Clone (defined on
// *Provider, absent from contracts.Provider) via providerBackend — no assertion.
func TestServer_Clone_ThroughSeam(t *testing.T) {
	fp := &fakeSeamProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone", TaskRef: "task-1"}}
	s := &Server{provider: fp}

	resp, err := s.Clone(context.Background(), &providerv1.CloneRequest{
		SourceVmId: "vm-src",
		TargetName: "vm-clone",
		Linked:     true,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "vm-clone", resp.TargetVmId)
	require.NotNil(t, resp.Task)
	assert.Equal(t, "task-1", resp.Task.Id)

	// Request mapping preserved through the interface.
	assert.Equal(t, "vm-src", fp.cloneReq.Source.ID)
	assert.Equal(t, "vm-clone", fp.cloneReq.TargetName)
	assert.True(t, fp.cloneReq.Linked)
}

// TestServer_ImagePrepare_ThroughSeam proves ImagePrepare reaches
// s.provider.imagePrepare (an unexported *Provider method) via providerBackend.
func TestServer_ImagePrepare_ThroughSeam(t *testing.T) {
	s := &Server{provider: &fakeSeamProvider{prepID: "fedora", prepPath: "/pool/fedora.qcow2"}}

	resp, err := s.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		TargetName: "fedora",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "fedora", resp.PreparedImageId)
	assert.Equal(t, "/pool/fedora.qcow2", resp.PreparedImagePath)
}

// TestServer_SnapshotCreate_ConnErrorSurfaces verifies a failure to resolve the
// per-host connection is surfaced (not panicked, not swallowed) — the seam's
// error path. It is answered as "not performed" (nothing was done) with a
// constant text: the connection error itself (it can name the host) is only
// logged (rc2 R21).
func TestServer_SnapshotCreate_ConnErrorSurfaces(t *testing.T) {
	s := &Server{provider: &fakeSeamProvider{connErr: errors.New("registry cold")}}

	_, err := s.SnapshotCreate(context.Background(), &providerv1.SnapshotCreateRequest{VmId: "vm-1"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "registry cold")
	assertNotPerformedStatus(t, err)
}

// TestServer_SingleHostSnapshotIDIsChecked: on a single-host provider too, a
// snapshot id virsh would read as an option, or one that is not a plain file
// name component, is refused before anything reaches the host
// (SnapshotDelete, its Provider-level twin, and SnapshotRevert).
func TestServer_SingleHostSnapshotIDIsChecked(t *testing.T) {
	for _, id := range []string{"", "--metadata", "-x", "../web-disk", "a/b", "s\n1"} {
		fc := &fakeSeamConn{id: "host-a", snapExists: true}
		s := &Server{provider: &fakeSeamProvider{hostConn: fc}}

		_, err := s.SnapshotDelete(context.Background(), &providerv1.SnapshotDeleteRequest{VmId: "vm-1", SnapshotId: id})
		require.Error(t, err, "%q", id)
		assert.True(t, contracts.IsInvalidSpec(err), "%q: %v", id, err)
		_, err = s.SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{VmId: "vm-1", SnapshotId: id})
		require.Error(t, err, "%q", id)
		// The single-host revert answers its typed refusals on the wire
		// (snapshot_revert_outcome.go): InvalidArgument, which the manager
		// maps to InvalidSpec.
		assertRefusalStatus(t, err, codes.InvalidArgument)
		assert.Empty(t, fc.virshCalls, "%q: nothing reaches the host", id)
	}
	_, err := (&Provider{virshProvider: NewVirshProvider(&ProviderConfig{})}).SnapshotDelete(context.Background(),
		contracts.VMRef{ID: "vm-1"}, "--metadata")
	assert.True(t, contracts.IsInvalidSpec(err), "the Provider-level SnapshotDelete shares the check: %v", err)
	assert.NoError(t, checkSnapshotIDArgument("snapshot-1700000000"))
	assert.NoError(t, checkSnapshotIDArgument("_legacy.name"), "looser than the routed check: earlier names still work")
}
