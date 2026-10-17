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

package proxmox

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func landedStageReq(namespace, name, path string) *providerv1.DeleteLandedDiskRequest {
	return &providerv1.DeleteLandedDiskRequest{
		Migration: &providerv1.ObjectIdentity{Uid: "m", Namespace: namespace, Name: "mig"},
		TargetVm:  &providerv1.ObjectIdentity{Namespace: namespace, Name: name},
		Path:      path,
	}
}

func landedStageProvider(node *stagedDeleteNode) *Provider {
	return &Provider{logger: slog.Default(), ssh: &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}}
}

// TestDeleteLandedDisk_RemovesOnlyTheTargetVMsStages: the script names exactly
// the target VM's stage in every format, quoted; the outcome follows the
// marker; a failure or an answer without a marker is retryable.
func TestDeleteLandedDisk_RemovesOnlyTheTargetVMsStages(t *testing.T) {
	stage := "/var/lib/virtrigaud/import-stage/team-a_web-migrated.qcow2"
	for name, tc := range map[string]struct {
		node *stagedDeleteNode
		want providerv1.LandedDiskDeleteOutcome
		code codes.Code
	}{
		"deleted":   {node: &stagedDeleteNode{stdout: landedStageDeletedMarker + "\n"}, want: providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED},
		"absent":    {node: &stagedDeleteNode{stdout: landedStageAbsentMarker + "\n"}, want: providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND},
		"failed":    {node: &stagedDeleteNode{err: errors.New("exit status 4")}, code: codes.Unavailable},
		"no marker": {node: &stagedDeleteNode{stdout: "garbage"}, code: codes.Unavailable},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := landedStageProvider(tc.node).DeleteLandedDisk(context.Background(), landedStageReq("team-a", "web", stage))
			require.Len(t, tc.node.scripts, 1)
			assert.Contains(t, tc.node.scripts[0], "for f in '/var/lib/virtrigaud/import-stage/team-a_web-migrated.qcow2' "+
				"'/var/lib/virtrigaud/import-stage/team-a_web-migrated.raw' "+
				"'/var/lib/virtrigaud/import-stage/team-a_web-migrated.vmdk'; do")
			if tc.code != codes.OK {
				assert.Equal(t, tc.code, status.Code(err))
				assert.NotContains(t, status.Convert(err).Message(), "/var/lib", "the status names no path")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.GetOutcome())
		})
	}
}

// TestDeleteLandedDisk_Refusals: an invalid identity or a recorded path that
// is not the target VM's stage runs nothing on the node; without the SSH data
// plane nothing can have been staged.
func TestDeleteLandedDisk_Refusals(t *testing.T) {
	for name, req := range map[string]*providerv1.DeleteLandedDiskRequest{
		"traversal name":    landedStageReq("team-a", "../../etc", ""),
		"namespace with _":  landedStageReq("team_a", "web", ""),
		"no target":         {Path: "/var/lib/virtrigaud/import-stage/team-a_web-migrated.qcow2"},
		"another VM":        landedStageReq("team-a", "web", "/var/lib/virtrigaud/import-stage/team-a_db-migrated.qcow2"),
		"another namespace": landedStageReq("team-a", "web", "/var/lib/virtrigaud/import-stage/team-b_web-migrated.qcow2"),
		"legacy stage":      landedStageReq("team-a", "web", "/var/tmp/.virtrigaud-import-web-migrated.qcow2"),
		"another file":      landedStageReq("team-a", "web", "/etc/pve/priv/authkey.key"),
	} {
		t.Run(name, func(t *testing.T) {
			node := &stagedDeleteNode{stdout: landedStageDeletedMarker}
			_, err := landedStageProvider(node).DeleteLandedDisk(context.Background(), req)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, node.scripts, "nothing runs on the node")
		})
	}

	// Without the SSH data plane nothing can be checked: a refusal, never a
	// NOT_FOUND the manager would record as deleted (security review).
	_, err := (&Provider{logger: slog.Default()}).DeleteLandedDisk(context.Background(), landedStageReq("team-a", "web", ""))
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Equal(t, landedDiskNoSSHMessage, status.Convert(err).Message())
}

// TestDeleteLandedDisk_ExcludesAnImportOfTheSameStage (security review): while
// an import of the VM's stage runs the delete answers Unavailable and runs
// nothing on the node; while a delete runs an import of that stage is refused
// (retryably); another VM's stage is unaffected.
func TestDeleteLandedDisk_ExcludesAnImportOfTheSameStage(t *testing.T) {
	name := importStageName("team-a", "web")
	release, ok := stageGuards.beginImport(name)
	require.True(t, ok)
	node := &stagedDeleteNode{stdout: landedStageDeletedMarker}
	_, err := landedStageProvider(node).DeleteLandedDisk(context.Background(), landedStageReq("team-a", "web", ""))
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Empty(t, node.scripts, "nothing runs on the node")

	resp, err := landedStageProvider(node).DeleteLandedDisk(context.Background(), landedStageReq("team-a", "db", ""))
	require.NoError(t, err, "another VM's stage is not held")
	assert.Equal(t, providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED, resp.GetOutcome())
	release()

	releaseDelete, ok := stageGuards.beginDelete(name)
	require.True(t, ok)
	_, err = (&Provider{logger: slog.Default()}).ImportDisk(context.Background(), &providerv1.ImportDiskRequest{
		BackendType: migration.BackendS3, TransferMode: "relay", TargetName: "web-migrated",
		TargetVm: &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"},
	})
	assert.Equal(t, codes.Unavailable, status.Code(err), "an import waits for the delete to finish")
	releaseDelete()

	_, ok = stageGuards.beginDelete(name)
	assert.True(t, ok, "released")
	stageGuards.mu.Lock()
	delete(stageGuards.deleting, name)
	stageGuards.mu.Unlock()
}

// TestBuildLandedStageDeleteScript_RunsAsWritten runs the script against real
// files: only the named paths are removed (a symbolic link itself, never its
// target); a second run finds nothing; shell metacharacters are data.
func TestBuildLandedStageDeleteScript_RunsAsWritten(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "team-a_web-migrated.qcow2")
	ownRaw := filepath.Join(dir, "team-a_web-migrated.raw")
	other := filepath.Join(dir, "team-b_web-migrated.qcow2")
	require.NoError(t, os.WriteFile(own, []byte("disk"), 0o600))
	require.NoError(t, os.WriteFile(other, []byte("other tenant"), 0o600))
	precious := filepath.Join(t.TempDir(), "precious")
	require.NoError(t, os.WriteFile(precious, []byte("x"), 0o600))
	require.NoError(t, os.Symlink(precious, ownRaw))

	run := func(paths ...string) string {
		out, err := exec.Command("sh", "-c", buildLandedStageDeleteScript(paths)).Output() //nolint:gosec // test runs the script under test
		require.NoError(t, err)
		return string(out)
	}
	assert.Contains(t, run(own, ownRaw), landedStageDeletedMarker)
	assert.NoFileExists(t, own)
	_, err := os.Lstat(ownRaw)
	assert.True(t, os.IsNotExist(err), "the link is removed")
	assert.FileExists(t, precious, "never what a link points to")
	assert.FileExists(t, other, "another tenant's stage is untouched")

	assert.Contains(t, run(own, ownRaw), landedStageAbsentMarker, "idempotent")
	assert.Contains(t, run(filepath.Join(dir, "$(touch pwned)")), landedStageAbsentMarker)
	assert.NoFileExists(t, filepath.Join(dir, "pwned"))
}

// TestCapabilities_LandedDiskDelete: Proxmox reports DeleteLandedDisk.
func TestCapabilities_LandedDiskDelete(t *testing.T) {
	resp, err := GetProviderCapabilities().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, resp.GetSupportsLandedDiskDelete())
}
