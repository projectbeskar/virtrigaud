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
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// landedPool is the default pool directory of the landed-disk tests.
const landedPool = "/var/lib/libvirt/images"

// landedTeamAWeb is where ImportDisk lands team-a/web's disk.
const landedTeamAWeb = landedPool + "/team-a.web-migrated.qcow2"

// landedTeamAWebName is team-a/web's landing volume name (the lock key).
const landedTeamAWebName = "team-a.web-migrated"

// landedHost is a single libvirt host for DeleteLandedDisk: the default pool
// in landedPool, what is at the landing name (kind: diskKindScript's answer),
// and at most one domain whose disk is domainDisk in format domainFormat
// (default raw). realpath resolves aliases; `qemu-img info` answers info[path]
// (default: a self-contained qcow2). It records every rm and counts the
// landing-name checks. Safe for concurrent use.
type landedHost struct {
	fakeSeamConn

	mu           sync.Mutex
	kind         string
	domainDisk   string
	domainFormat string
	aliases      map[string]string
	info         map[string]string
	listFails    bool
	rmFails      bool
	removed      []string
	kindChecks   int
}

func (h *landedHost) Virsh(_ context.Context, args ...string) (*hostconn.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.virshCalls = append(h.virshCalls, args)
	switch args[0] {
	case "pool-dumpxml":
		return &hostconn.Result{Stdout: "<pool type='dir'><name>default</name><target><path>" + landedPool + "/</path></target></pool>"}, nil
	case "list":
		if h.listFails {
			return &hostconn.Result{}, errors.New("virsh list failed")
		}
		if h.domainDisk == "" {
			return &hostconn.Result{}, nil
		}
		return &hostconn.Result{Stdout: seamDomainUUID + "\n"}, nil
	case "dumpxml":
		format := h.domainFormat
		if format == "" {
			format = "raw"
		}
		return &hostconn.Result{Stdout: fmt.Sprintf("<domain><name>other</name><uuid>%s</uuid><devices>"+
			"<disk type='file' device='disk'><driver name='qemu' type='%s'/><source file='%s'/></disk>"+
			"</devices></domain>", seamDomainUUID, format, h.domainDisk)}, nil
	}
	return &hostconn.Result{}, nil
}

func (h *landedHost) RunHost(_ context.Context, argv ...string) (*hostconn.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(argv) > 2 && argv[0] == "sh" {
		switch argv[2] {
		case diskKindScript:
			h.kindChecks++
			return &hostconn.Result{Stdout: h.kind + "\n"}, nil
		case backingKindScript:
			return &hostconn.Result{Stdout: backingKindFile + "\n"}, nil
		}
	}
	switch {
	case len(argv) > 0 && argv[0] == "rm":
		if h.rmFails {
			return &hostconn.Result{}, errors.New("rm: permission denied")
		}
		h.removed = append(h.removed, strings.Join(argv, " "))
		return &hostconn.Result{}, nil
	case len(argv) > 0 && argv[0] == "realpath":
		var out strings.Builder
		for i, a := range argv {
			if a == "--" {
				for _, p := range argv[i+1:] {
					if real, ok := h.aliases[p]; ok {
						p = real
					}
					out.WriteString(p + "\x00")
				}
				break
			}
		}
		return &hostconn.Result{Stdout: out.String()}, nil
	}
	for i, a := range argv {
		if a == "qemu-img" && i+1 < len(argv) && argv[i+1] == "info" {
			path := argv[len(argv)-1]
			if info, ok := h.info[path]; ok {
				return &hostconn.Result{Stdout: info}, nil
			}
			return &hostconn.Result{Stdout: `{"format":"qcow2"}`}, nil
		}
	}
	return &hostconn.Result{}, nil
}

func (h *landedHost) removedSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.removed...)
}

func (h *landedHost) kindChecksSnapshot() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.kindChecks
}

// landedServer is a single-host Server whose host is h.
func landedServer(h *landedHost) *Server {
	return &Server{provider: &fakeSeamProvider{hostConn: h}}
}

func landedRequest(namespace, name, path string) *providerv1.DeleteLandedDiskRequest {
	return &providerv1.DeleteLandedDiskRequest{
		Migration: &providerv1.ObjectIdentity{Uid: "m", Namespace: namespace, Name: "mig"},
		TargetVm:  &providerv1.ObjectIdentity{Namespace: namespace, Name: name},
		Path:      path,
	}
}

// requireDiskInUse asserts err is the VM_DISK_IN_USE refusal, naming no domain.
func requireDiskInUse(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.True(t, statusHasReasonForTest(err, contracts.VMDiskInUseReason))
	assert.NotContains(t, status.Convert(err).Message(), "other", "the refusal never names the domain")
}

// TestDeleteLandedDisk_RemovesAnUnusedLandedDisk: the target VM's landed disk,
// used by no domain, is removed with exactly one `rm -f -- <that file>`, with
// or without the recorded path; an absent one is NOT_FOUND and removes
// nothing.
func TestDeleteLandedDisk_RemovesAnUnusedLandedDisk(t *testing.T) {
	for _, path := range []string{landedTeamAWeb, ""} {
		h := &landedHost{kind: diskKindFile, domainDisk: landedPool + "/other-disk.qcow2"}
		resp, err := landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", path))
		require.NoError(t, err)
		assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
		assert.Equal(t, []string{"rm -f -- " + landedTeamAWeb}, h.removedSnapshot())
	}

	h := &landedHost{kind: diskKindAbsent}
	resp, err := landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", landedTeamAWeb))
	require.NoError(t, err)
	assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND, resp.GetOutcome())
	assert.Empty(t, h.removedSnapshot())
}

// TestDeleteLandedDisk_KeepsADiskADomainUses: a landed disk a domain uses —
// directly, through a symbolic link alias, or as the backing file of its disk
// — is refused with VM_DISK_IN_USE; a scan that cannot run fails closed
// (retryable). Nothing is removed.
func TestDeleteLandedDisk_KeepsADiskADomainUses(t *testing.T) {
	overlay := landedPool + "/other-disk.qcow2"
	alias := "/srv/vm/web.qcow2"
	for name, h := range map[string]*landedHost{
		"directly": {kind: diskKindFile, domainDisk: landedTeamAWeb},
		"through a symbolic link alias": {kind: diskKindFile, domainDisk: alias,
			aliases: map[string]string{alias: landedTeamAWeb}},
		"as a backing file": {kind: diskKindFile, domainDisk: overlay, domainFormat: "qcow2",
			info: map[string]string{overlay: fmt.Sprintf(
				`{"format":"qcow2","backing-filename":%q,"full-backing-filename":%q,"backing-filename-format":"qcow2"}`,
				landedTeamAWeb, landedTeamAWeb)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", ""))
			requireDiskInUse(t, err)
			assert.Empty(t, h.removedSnapshot())
		})
	}

	h := &landedHost{kind: diskKindFile, listFails: true}
	_, err := landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", ""))
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, landedDiskFailedMessage, status.Convert(err).Message())
	assert.Empty(t, h.removedSnapshot())
}

// TestDeleteLandedDisk_RefusesAnythingElse: a recorded path other than the
// target VM's landed disk, a symbolic link, directory or device at the landing
// name and an invalid identity are InvalidArgument; a failed rm is retryable.
// Nothing else is ever removed.
func TestDeleteLandedDisk_RefusesAnythingElse(t *testing.T) {
	for name, tc := range map[string]struct {
		req  *providerv1.DeleteLandedDiskRequest
		host *landedHost
		code codes.Code
	}{
		"another VM's landing":   {landedRequest("team-a", "web", landedPool+"/team-a.db-migrated.qcow2"), &landedHost{kind: diskKindFile}, codes.InvalidArgument},
		"another namespace":      {landedRequest("team-a", "web", landedPool+"/team-b.web-migrated.qcow2"), &landedHost{kind: diskKindFile}, codes.InvalidArgument},
		"legacy bare name":       {landedRequest("team-a", "web", landedPool+"/web-migrated.qcow2"), &landedHost{kind: diskKindFile}, codes.InvalidArgument},
		"another file":           {landedRequest("team-a", "web", "/etc/shadow"), &landedHost{kind: diskKindFile}, codes.InvalidArgument},
		"traversal":              {landedRequest("team-a", "../web", ""), &landedHost{kind: diskKindFile}, codes.InvalidArgument},
		"no target VM":           {&providerv1.DeleteLandedDiskRequest{Path: landedTeamAWeb}, &landedHost{kind: diskKindFile}, codes.InvalidArgument},
		"symbolic link":          {landedRequest("team-a", "web", ""), &landedHost{kind: diskKindSymlink}, codes.InvalidArgument},
		"directory":              {landedRequest("team-a", "web", ""), &landedHost{kind: diskKindOther}, codes.InvalidArgument},
		"rm fails":               {landedRequest("team-a", "web", ""), &landedHost{kind: diskKindFile, rmFails: true}, codes.Unavailable},
		"namespace with a space": {landedRequest("team a", "web", ""), &landedHost{kind: diskKindFile}, codes.InvalidArgument},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := landedServer(tc.host).DeleteLandedDisk(context.Background(), tc.req)
			require.Error(t, err)
			assert.Equal(t, tc.code, status.Code(err), "error: %v", err)
			if p := tc.req.GetPath(); p != "" {
				assert.NotContains(t, status.Convert(err).Message(), p, "the message never echoes the path")
			}
			assert.Empty(t, tc.host.removedSnapshot())
		})
	}
}

// TestDeleteLandedDisk_WaitsForACreateThenRefuses (security review): while a
// Create holds the landing name (its in-place attach, through define), the
// delete looks at nothing; once the Create attached the disk and released the
// name, the delete's scan sees the domain and refuses.
func TestDeleteLandedDisk_WaitsForACreateThenRefuses(t *testing.T) {
	h := &landedHost{kind: diskKindFile}
	unlock, err := landingLocks.lock(context.Background(), landedTeamAWebName)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", ""))
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("the delete did not wait for the create: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	assert.Zero(t, h.kindChecksSnapshot(), "nothing is checked while the create holds the name")

	h.mu.Lock()
	h.domainDisk = landedTeamAWeb // the create defined its domain on the disk
	h.mu.Unlock()
	unlock()

	select {
	case err := <-done:
		requireDiskInUse(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the delete did not resume after the create released the name")
	}
	assert.Empty(t, h.removedSnapshot())
}

// TestDeleteLandedDisk_ImportInFlightIsRetryable (security review): while an
// ImportDisk of the same landing name is in flight the delete answers
// Unavailable at once — never NOT_FOUND for a disk about to land — and
// touches nothing.
func TestDeleteLandedDisk_ImportInFlightIsRetryable(t *testing.T) {
	h := &landedHost{kind: diskKindAbsent}
	unlock, err := landingLocks.lockForImport(context.Background(), landedTeamAWebName)
	require.NoError(t, err)
	defer unlock()

	_, err = landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", ""))
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, landedDiskImportInFlightMessage, status.Convert(err).Message())
	assert.Zero(t, h.kindChecksSnapshot())
}

// TestImportDiskAndCreateHoldTheLandingName: ImportDisk and a Create that may
// attach an imported disk in place wait for the landing name.
func TestImportDiskAndCreateHoldTheLandingName(t *testing.T) {
	unlock, err := landingLocks.lock(context.Background(), landedTeamAWebName)
	require.NoError(t, err)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = landedServer(&landedHost{}).ImportDisk(ctx, &providerv1.ImportDiskRequest{
		SourceUrl: "pvc://mig/disk.qcow2", TargetName: "web-migrated",
		TargetVm: &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"},
	})
	assert.Equal(t, codes.Unavailable, status.Code(err), "the import waits for the landing name")

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	req := contracts.CreateRequest{Name: "web"}
	req.Image.ImportedDisk = true
	_, err = (&Provider{}).createVMWithCloudInit(ctx2, nil, req, "team-a.web", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gave up waiting", "the create waits for the landing name")
}

// TestDeleteLandedDisk_CapabilityAndClustered: a single-host provider reports
// the landed-disk delete; a clustered one neither reports nor serves it.
func TestDeleteLandedDisk_CapabilityAndClustered(t *testing.T) {
	assert.False(t, clusteredCapabilities().GetSupportsLandedDiskDelete())
	caps, err := landedServer(&landedHost{}).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsLandedDiskDelete())
}

// statusHasReasonForTest reports whether err's status carries the VirtRigaud
// ErrorInfo reason.
func statusHasReasonForTest(err error, reason string) bool {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(interface{ GetReason() string }); ok && info.GetReason() == reason {
			return true
		}
	}
	return false
}
