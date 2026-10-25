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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the review follow-ups of the snapshot outcome work: the V12
// probe is a read, a refused revert of an internal snapshot proves nothing but
// the job lock, an ejection repair that could not put the media back is an
// unknown outcome, typed and host-transport failures pass through the routed
// revert unchanged, and a create whose media could not be re-inserted is never
// reclassified as the V12 Conflict.

// TestOverlayProbeIsARead: on a routed create, a host that stops answering
// during or after the V12 probe, before the create, has only been read.
func TestOverlayProbeIsARead(t *testing.T) {
	assert.False(t, hostArgvMayWrite([]string{"sh", "-c", overlayCollisionScript, "sh", "/pool/web-disk.s1"}))
}

// v12InternalSnapXML is the snapshot-dumpxml of an internal (memory) snapshot
// whose stored definition's CD-ROM is empty.
const v12InternalSnapXML = "<domainsnapshot><name>s1</name><memory snapshot='internal'/><disks>" +
	"<disk name='vda' snapshot='internal'/></disks><domain><devices><disk type='file' device='cdrom'>" +
	"<target dev='sda'/><readonly/></disk></devices></domain></domainsnapshot>\n"

// TestSingleHostRevert_InternalSnapshotRefusal: an internal snapshot is
// reverted in place, disk by disk, so unchanged disk sources prove nothing:
// only the job lock is "not performed"; any other refusal is a failure.
func TestSingleHostRevert_InternalSnapshotRefusal(t *testing.T) {
	run := func(t *testing.T, stderr string) error {
		t.Helper()
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		r21ScriptedExits(t)
		fx.script("single", "snaps-"+scdDomain, "s1\n")
		fx.script("single", "snapxml-s1", v12InternalSnapXML)
		fx.script("single", "fail-snapshot-revert", "")
		fx.script("single", "stderr-snapshot-revert", stderr)
		_, err := NewServer(fx.p).SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{VmId: scdDomain, SnapshotId: "s1"})
		return err
	}
	t.Run("the job lock: not performed", func(t *testing.T) {
		assertRevertNotPerformed(t, run(t, revertBusy))
	})
	t.Run("any other refusal: a failure (a disk may already be reverted)", func(t *testing.T) {
		err := run(t, "error: Requested operation is not valid: Disk device 'vdb' does not support snapshotting\n")
		require.Error(t, err)
		assert.Equal(t, codes.Unknown, status.Code(err), "%v", err)
		assert.Equal(t, wireRevertFailed, status.Convert(err).Message())
	})
}

// TestRevertFailure_MediaNotRestoredIsUnknown: a repair of an interrupted
// ejection that could not put the media back is not "not performed".
func TestRevertFailure_MediaNotRestoredIsUnknown(t *testing.T) {
	err := fmt.Errorf("failed to revert to snapshot: %w", errMediaNotRestored)
	conn := &fakeSeamConn{id: "host-a"}
	assert.Equal(t, revertUnknown, classifyRevertFailure(context.Background(), conn, "d", err))
	assertRevertUnknown(t, singleHostRevertFailure(context.Background(), conn, "d", err))
	var roe *routedOpError
	require.ErrorAs(t, routedRevertFailure(context.Background(), conn, "d", err), &roe)
	assert.True(t, roe.outcomeUnknown)
}

// TestRoutedRevertFailure_PassesTypedAndTransportFailures: typed refusals and
// a host that stopped answering keep the routed answers they had.
func TestRoutedRevertFailure_PassesTypedAndTransportFailures(t *testing.T) {
	conn := &fakeSeamConn{id: "host-a"}
	unsafe := atStage(stageBefore, &snapshotRevertUnsafeError{snapshot: "s1", disks: []string{"hda"}})
	assert.Same(t, unsafe, routedRevertFailure(context.Background(), conn, "d", unsafe))
	inUse := atStage(stageBefore, &diskDependentsError{})
	assert.Same(t, inUse, routedRevertFailure(context.Background(), conn, "d", inUse))
	for _, stage := range []callStage{stageBefore, stageCommand} {
		dropped := atStage(stage, &VirshError{Command: "virsh x", ExitCode: -1, Stderr: "connection lost"})
		assert.Same(t, dropped, routedRevertFailure(context.Background(), conn, "d", dropped), "stage %d", stage)
	}
}

// TestOverlayExistsRefusal_OnlyTheCreatesOwnFailure: a failed create whose
// media could not be put back (left untagged by snapshotCreateWith) is never
// answered as the V12 Conflict, whatever libvirt said.
func TestOverlayExistsRefusal_OnlyTheCreatesOwnFailure(t *testing.T) {
	conn := &fakeSeamConn{id: "host-a"}
	refused := virshFailure(1, v12OverlayRefusal)
	joined := fmt.Errorf("failed to create snapshot: %w; %w", refused, errors.New("re-insert failed"))
	assert.Nil(t, overlayExistsRefusal(context.Background(), conn, "d", "x", joined))
	assert.Nil(t, overlayExistsRefusal(context.Background(), conn, "d", "x", refused), "untagged")
	tagged := atStageOf(stageCommand, "x", refused)
	assert.NotNil(t, overlayExistsRefusal(context.Background(), conn, "d", "x", tagged))
}
