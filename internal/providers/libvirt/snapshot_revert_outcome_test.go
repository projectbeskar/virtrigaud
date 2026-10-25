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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin how a failed SnapshotRevert is answered
// (snapshot_revert_outcome.go): "not performed" whenever the provider proves
// nothing changed, a terminal refusal or failure when libvirt says so, and an
// unknown outcome (retry) only when the revert may have run.

// Libvirt's answers to a revert, as virsh prints them.
const (
	revertBusy      = "error: Timed out during operation: cannot acquire state change lock (held by monitor=remoteDispatchDomainRevertToSnapshot)\n"
	revertUnsupport = "error: unsupported configuration: revert to external snapshot not supported yet\n"
	revertNoSnap    = "error: Domain snapshot not found: no domain snapshot with matching name 's1'\n"
	revertInternal  = "error: internal error: unable to execute QEMU command 'blockdev-add': Could not open file\n"
	revertEOF       = "error: Disconnected from qemu:///system due to end of file\nerror: End of file while reading data: Input/output error\n"
)

// assertRevertUnknown asserts err is the "outcome unknown, retry" answer.
func assertRevertUnknown(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	assert.True(t, contracts.HasErrorInfoReason(err, contracts.OutcomeUnknownRetryReason), "%v", err)
}

// assertRevertNotPerformed asserts err is "not performed" (never unknown).
func assertRevertNotPerformed(t *testing.T, err error) {
	t.Helper()
	assertNotPerformedStatus(t, err)
	assert.False(t, contracts.HasErrorInfoReason(err, contracts.OutcomeUnknownRetryReason), "%v", err)
}

// revertRan reports whether calls hold a snapshot-revert.
func revertRan(calls []string) bool { return countCalls(calls, "snapshot-revert") > 0 }

func TestSingleHostSnapshotRevert_Outcomes(t *testing.T) {
	run := func(t *testing.T, setup func(*scdFixture), vmID string) ([]string, error) {
		t.Helper()
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		r21ScriptedExits(t)
		// A refused revert may still have rewritten the domain
		// (revert-rewrites): the rewrite happens before the scripted failure.
		r21Patch(t, `snapshot-revert) fail snapshot-revert;`,
			`snapshot-revert) if [ -f "$d/rewrite-then-fail" ]; then cat "$d/revert-rewrites" > "$d/dom-$2.xml"; fi; fail snapshot-revert;`)
		fx.script("single", "snaps-"+scdDomain, "s1\n")
		if setup != nil {
			setup(fx)
		}
		_, err := NewServer(fx.p).SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{VmId: vmID, SnapshotId: "s1"})
		return fx.calls(), err
	}
	failRevert := func(stderr string, more ...func(*scdFixture)) func(*scdFixture) {
		return func(fx *scdFixture) {
			fx.script("single", "fail-snapshot-revert", "")
			fx.script("single", "stderr-snapshot-revert", stderr)
			for _, m := range more {
				m(fx)
			}
		}
	}
	const leak = "scripted"

	// Before the revert command: nothing changed.
	t.Run("the snapshots cannot be listed: not performed", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "fail-snapshot-list", "") }, scdDomain)
		assertRevertNotPerformed(t, err)
		assert.Equal(t, wireRevertNotPerformed, status.Convert(err).Message())
		assertConstantText(t, err, leak, "virsh", "qemu:///")
		assert.False(t, revertRan(calls))
	})
	t.Run("the host drops the connection during a check: not performed", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "exit-domstate", "255") }, scdDomain)
		assertRevertNotPerformed(t, err)
		assert.False(t, revertRan(calls))
	})
	t.Run("the snapshot does not exist: NotFound", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "snaps-"+scdDomain, "other\n") }, scdDomain)
		assertRefusalStatus(t, err, codes.NotFound)
		assert.False(t, revertRan(calls))
	})
	t.Run("the domain does not exist: NotFound", func(t *testing.T) {
		calls, err := run(t, nil, "gone")
		assertRefusalStatus(t, err, codes.NotFound)
		assert.Equal(t, wireDomainNotFound, status.Convert(err).Message())
		assert.False(t, revertRan(calls))
	})
	t.Run("the dependency check cannot run: kept typed (VM_DISK_CHECK_FAILED)", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "fail-dumpxml", "") }, scdDomain)
		require.Error(t, err)
		assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMDiskCheckFailedReason), "%v", err)
		assert.False(t, revertRan(calls))
	})
	t.Run("the snapshot's definition cannot be read: not performed", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "fail-snapshot-dumpxml", "") }, scdDomain)
		assertRevertNotPerformed(t, err)
		assert.False(t, revertRan(calls))
	})

	// The revert command.
	t.Run("libvirt refuses: the domain is busy, disks unchanged: not performed", func(t *testing.T) {
		_, err := run(t, failRevert(revertBusy), scdDomain)
		assertRevertNotPerformed(t, err)
		assert.Equal(t, wireRevertNotRun, status.Convert(err).Message())
		assertConstantText(t, err, "monitor", "virsh", leak)
	})
	t.Run("libvirt refuses for the VM's configuration: terminal FailedPrecondition", func(t *testing.T) {
		_, err := run(t, failRevert(revertUnsupport), scdDomain)
		assertRefusalStatus(t, err, codes.FailedPrecondition)
		assert.Equal(t, wireRevertRefusedForConfig, status.Convert(err).Message())
		assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason), "%v", err)
	})
	t.Run("a refusal text but the disks changed: unknown", func(t *testing.T) {
		_, err := run(t, failRevert(revertBusy, func(fx *scdFixture) {
			fx.script("single", "rewrite-then-fail", "")
			fx.script("single", "revert-rewrites", strings.Replace(scdDomainXML(scdDomain, scdDomainOpts{}),
				scdDiskPath, "/var/lib/libvirt/images/vrgold-web-disk.1761400000", 1))
		}), scdDomain)
		assertRevertUnknown(t, err)
	})
	t.Run("a refusal text and the domain cannot be read again: unknown", func(t *testing.T) {
		_, err := run(t, failRevert(revertBusy, func(fx *scdFixture) {
			// dumpxml answers the checks before the revert, then fails.
			r21Patch(t, `dumpxml) fail dumpxml;`, `dumpxml) if [ -f "$d/reverted-once" ]; then echo "error: read failed" >&2; exit 1; fi; fail dumpxml;`)
			r21Patch(t, `fail snapshot-revert;`, `: > "$d/reverted-once"; fail snapshot-revert;`)
		}), scdDomain)
		assertRevertUnknown(t, err)
	})
	t.Run("libvirt does not find the snapshot: NotFound", func(t *testing.T) {
		_, err := run(t, failRevert(revertNoSnap), scdDomain)
		assertRefusalStatus(t, err, codes.NotFound)
		assert.Equal(t, wireRevertSnapshotNotFound, status.Convert(err).Message())
	})
	t.Run("libvirt fails while reverting: terminal failure, never 'not performed'", func(t *testing.T) {
		_, err := run(t, failRevert(revertInternal), scdDomain)
		require.Error(t, err)
		assert.Equal(t, codes.Unknown, status.Code(err), "%v", err)
		assert.Equal(t, wireRevertFailed, status.Convert(err).Message())
		assertConstantText(t, err, "blockdev", "virsh")
	})
	t.Run("virsh loses libvirtd during the revert: unknown", func(t *testing.T) {
		_, err := run(t, func(fx *scdFixture) {
			fx.script("single", "exit-snapshot-revert", "1")
			fx.script("single", "stderr-snapshot-revert", revertEOF)
		}, scdDomain)
		assertRevertUnknown(t, err)
	})
	t.Run("the revert is killed: unknown", func(t *testing.T) {
		_, err := run(t, func(fx *scdFixture) { fx.script("single", "exit-snapshot-revert", "137") }, scdDomain)
		assertRevertUnknown(t, err)
	})
	t.Run("the connection drops during the revert: unknown", func(t *testing.T) {
		_, err := run(t, func(fx *scdFixture) { fx.script("single", "exit-snapshot-revert", "255") }, scdDomain)
		assertRevertUnknown(t, err)
	})
	t.Run("success is unchanged", func(t *testing.T) {
		calls, err := run(t, nil, scdDomain)
		require.NoError(t, err)
		assert.True(t, revertRan(calls))
	})
}

func TestClusteredSnapshotRevert_Outcomes(t *testing.T) {
	webXML := scdDomainXML("web", scdDomainOpts{owner: ownerTeamA})
	run := func(t *testing.T, setup func(*routedSCD)) ([]string, error) {
		t.Helper()
		fx := ownedWebOnB(t, webXML)
		r21ScriptedExits(t)
		if setup != nil {
			setup(fx)
		}
		_, err := NewServer(fx.p).SnapshotRevert(context.Background(), &providerv1.SnapshotRevertRequest{
			VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, SnapshotId: "s1"})
		return fx.calls(), err
	}
	failRevert := func(stderr string) func(*routedSCD) {
		return func(fx *routedSCD) {
			fx.script("host-b", "fail-snapshot-revert", "")
			fx.script("host-b", "stderr-snapshot-revert", stderr)
		}
	}

	t.Run("a check fails before the revert: not performed (VM_OPERATION_FAILED, never unknown)", func(t *testing.T) {
		calls, err := run(t, func(fx *routedSCD) { fx.script("host-b", "fail-domstate", "") })
		assertRevertNotPerformed(t, err)
		assert.False(t, revertRan(calls))
	})
	t.Run("libvirt refuses: busy: not performed", func(t *testing.T) {
		_, err := run(t, failRevert(revertBusy))
		assertRevertNotPerformed(t, err)
		assert.Contains(t, status.Convert(err).Message(), wireRevertNotRun)
	})
	t.Run("libvirt refuses for the VM's configuration: FailedPrecondition", func(t *testing.T) {
		_, err := run(t, failRevert(revertUnsupport))
		assertRefusalStatus(t, err, codes.FailedPrecondition)
		assert.Contains(t, status.Convert(err).Message(), wireRevertRefusedForConfig)
	})
	t.Run("libvirt fails while reverting: Unknown VM_OPERATION_FAILED, as before", func(t *testing.T) {
		_, err := run(t, failRevert(revertInternal))
		require.Error(t, err)
		assert.Equal(t, codes.Unknown, status.Code(err), "%v", err)
		assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason), "%v", err)
		assert.NotContains(t, status.Convert(err).Message(), "blockdev")
	})
	t.Run("the revert is killed: unknown, retry", func(t *testing.T) {
		_, err := run(t, func(fx *routedSCD) { fx.script("host-b", "exit-snapshot-revert", "137") })
		assertRevertUnknown(t, err)
	})
	t.Run("the snapshot does not exist: NotFound", func(t *testing.T) {
		_, err := run(t, func(fx *routedSCD) { fx.script("host-b", "snaps-"+routingDomainUUID, "other\n") })
		assertRefusalStatus(t, err, codes.NotFound)
	})
	t.Run("success is unchanged", func(t *testing.T) {
		calls, err := run(t, nil)
		require.NoError(t, err)
		assert.True(t, revertRan(calls))
	})
}
