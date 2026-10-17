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
	"testing"

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

// landedHost is a single libvirt host for DeleteLandedDisk: the default pool
// in landedPool, the landed file's kind at its name, and at most one domain
// whose (raw) disk is domainDisk. It records every rm.
type landedHost struct {
	fakeSeamConn
	kind       string // targetKindScript's answer: "", pathExistsMarker or targetKindSymlink
	domainDisk string // "" = no domain on the host
	listFails  bool
	rmFails    bool
	removed    []string
}

func (h *landedHost) Virsh(_ context.Context, args ...string) (*hostconn.Result, error) {
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
		return &hostconn.Result{Stdout: fmt.Sprintf("<domain><name>other</name><uuid>%s</uuid><devices>"+
			"<disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='%s'/></disk>"+
			"</devices></domain>", seamDomainUUID, h.domainDisk)}, nil
	}
	return &hostconn.Result{}, nil
}

func (h *landedHost) RunHost(ctx context.Context, argv ...string) (*hostconn.Result, error) {
	if len(argv) > 2 && argv[0] == "sh" {
		switch argv[2] {
		case targetKindScript:
			return &hostconn.Result{Stdout: h.kind + "\n"}, nil
		case backingKindScript:
			return &hostconn.Result{Stdout: backingKindFile + "\n"}, nil
		}
	}
	if len(argv) > 0 && argv[0] == "rm" {
		if h.rmFails {
			return &hostconn.Result{}, errors.New("rm: permission denied")
		}
		h.removed = append(h.removed, strings.Join(argv, " "))
		return &hostconn.Result{}, nil
	}
	return h.fakeSeamConn.RunHost(ctx, argv...)
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

// TestDeleteLandedDisk_RemovesAnUnusedLandedDisk: the target VM's landed disk,
// used by no domain, is removed with exactly one `rm -f -- <that file>`, with
// or without the recorded path; an absent one is NOT_FOUND and removes
// nothing.
func TestDeleteLandedDisk_RemovesAnUnusedLandedDisk(t *testing.T) {
	for _, path := range []string{landedTeamAWeb, ""} {
		h := &landedHost{kind: pathExistsMarker, domainDisk: landedPool + "/other-disk.qcow2"}
		resp, err := landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", path))
		require.NoError(t, err)
		assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
		assert.Equal(t, []string{"rm -f -- " + landedTeamAWeb}, h.removed)
	}

	h := &landedHost{}
	resp, err := landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", landedTeamAWeb))
	require.NoError(t, err)
	assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND, resp.GetOutcome())
	assert.Empty(t, h.removed)
}

// TestDeleteLandedDisk_KeepsADiskADomainUses: a landed disk some domain uses
// (the target VM created on it, or anything else) is refused with
// VM_DISK_IN_USE; a scan that cannot run fails closed (retryable). Nothing is
// removed.
func TestDeleteLandedDisk_KeepsADiskADomainUses(t *testing.T) {
	h := &landedHost{kind: pathExistsMarker, domainDisk: landedTeamAWeb}
	_, err := landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", ""))
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.True(t, statusHasReasonForTest(err, contracts.VMDiskInUseReason))
	assert.NotContains(t, status.Convert(err).Message(), "other", "the refusal never names the domain")
	assert.Empty(t, h.removed)

	h = &landedHost{kind: pathExistsMarker, listFails: true}
	_, err = landedServer(h).DeleteLandedDisk(context.Background(), landedRequest("team-a", "web", ""))
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, landedDiskFailedMessage, status.Convert(err).Message())
	assert.Empty(t, h.removed)
}

// TestDeleteLandedDisk_RefusesAnythingElse: a recorded path other than the
// target VM's landed disk, a symbolic link at the landing name and an invalid
// identity are InvalidArgument; a failed rm is retryable. Nothing else is
// ever removed.
func TestDeleteLandedDisk_RefusesAnythingElse(t *testing.T) {
	for name, tc := range map[string]struct {
		req  *providerv1.DeleteLandedDiskRequest
		host *landedHost
		code codes.Code
	}{
		"another VM's landing":   {landedRequest("team-a", "web", landedPool+"/team-a.db-migrated.qcow2"), &landedHost{kind: pathExistsMarker}, codes.InvalidArgument},
		"another namespace":      {landedRequest("team-a", "web", landedPool+"/team-b.web-migrated.qcow2"), &landedHost{kind: pathExistsMarker}, codes.InvalidArgument},
		"legacy bare name":       {landedRequest("team-a", "web", landedPool+"/web-migrated.qcow2"), &landedHost{kind: pathExistsMarker}, codes.InvalidArgument},
		"another file":           {landedRequest("team-a", "web", "/etc/shadow"), &landedHost{kind: pathExistsMarker}, codes.InvalidArgument},
		"traversal":              {landedRequest("team-a", "../web", ""), &landedHost{kind: pathExistsMarker}, codes.InvalidArgument},
		"no target VM":           {&providerv1.DeleteLandedDiskRequest{Path: landedTeamAWeb}, &landedHost{kind: pathExistsMarker}, codes.InvalidArgument},
		"symbolic link":          {landedRequest("team-a", "web", ""), &landedHost{kind: targetKindSymlink}, codes.InvalidArgument},
		"rm fails":               {landedRequest("team-a", "web", ""), &landedHost{kind: pathExistsMarker, rmFails: true}, codes.Unavailable},
		"namespace with a space": {landedRequest("team a", "web", ""), &landedHost{kind: pathExistsMarker}, codes.InvalidArgument},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := landedServer(tc.host).DeleteLandedDisk(context.Background(), tc.req)
			require.Error(t, err)
			assert.Equal(t, tc.code, status.Code(err), "error: %v", err)
			if p := tc.req.GetPath(); p != "" {
				assert.NotContains(t, status.Convert(err).Message(), p, "the message never echoes the path")
			}
			assert.Empty(t, tc.host.removed)
		})
	}
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
