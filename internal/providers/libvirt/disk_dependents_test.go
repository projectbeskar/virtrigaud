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
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// UUIDs of the domains the dependency tests define.
const (
	uuidSource = "aaaaaaaa-0000-4000-8000-000000000001"
	uuidClone  = "aaaaaaaa-0000-4000-8000-000000000002"
	uuidFull   = "aaaaaaaa-0000-4000-8000-000000000003"
	uuidOther  = "aaaaaaaa-0000-4000-8000-000000000004"
)

// depDomainXML is a domain definition with one file-backed disk. backing, when
// set, is listed in a <backingStore> the way `virsh dumpxml` shows a RUNNING
// domain's chain (a shut-off domain's definition omits it).
func depDomainXML(name, uuid, disk, backing string) string {
	bs := ""
	if backing != "" {
		bs = fmt.Sprintf("<backingStore type='file' index='3'><format type='qcow2'/><source file='%s'/><backingStore/></backingStore>", backing)
	}
	return fmt.Sprintf("<domain type='kvm'><name>%s</name><uuid>%s</uuid><devices>"+
		"<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='%s'/>%s<target dev='vda' bus='virtio'/></disk>"+
		"</devices></domain>", name, uuid, disk, bs)
}

// define records a domain on fake host `host`, addressable by name and UUID.
func (h *fakeHost) define(host, name, uuid, domXML string) {
	h.t.Helper()
	h.domain(host, uuid, domXML)
	require.NoError(h.t, os.WriteFile(filepath.Join(h.root, host, "dom-"+name+".xml"), []byte(domXML), 0o600))
}

// overlay makes disk a qcow2 overlay of base for qemu-img's --backing-chain
// query — the only way a SHUT-OFF linked clone's dependency is visible.
func (h *fakeHost) overlay(disk, base string) {
	h.t.Helper()
	chain := fmt.Sprintf(`[{"filename":%q,"format":"qcow2","backing-filename":%q,"full-backing-filename":%q},{"filename":%q,"format":"qcow2"}]`,
		disk, base, base, base)
	require.NoError(h.t, os.WriteFile(disk+".chain.json", []byte(chain), 0o600))
}

// undefine removes a domain from fake host `host`.
func (h *fakeHost) undefine(host, name, uuid string) {
	h.t.Helper()
	dir := filepath.Join(h.root, host)
	b, err := os.ReadFile(filepath.Join(dir, "uuids")) //nolint:gosec // test reads its own fixture
	require.NoError(h.t, err)
	var keep []byte
	for _, line := range splitLines(string(b)) {
		if line != uuid {
			keep = append(keep, []byte(line+"\n")...)
		}
	}
	require.NoError(h.t, os.WriteFile(filepath.Join(dir, "uuids"), keep, 0o600))
	_ = os.Remove(filepath.Join(dir, "dom-"+uuid+".xml"))
	_ = os.Remove(filepath.Join(dir, "dom-"+name+".xml"))
}

// splitLines splits s into its non-empty lines.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// linkedCloneHost defines, on fake host h1, a source VM and a linked clone of
// it (running: its <backingStore> lists the source disk; shut off: only the
// overlay's qemu-img chain does) and returns the host and both disk paths.
func linkedCloneHost(t *testing.T, running bool) (*fakeHost, *VirshProvider, string, string) {
	t.Helper()
	h := newFakeHost(t)
	vp := h.host("h1")
	src := h.file(h.images, "team-a.template-disk.qcow2")
	clone := h.file(h.images, "team-b.copy-disk.qcow2")
	h.overlay(clone, src)
	h.define("h1", "team-a.template", uuidSource, depDomainXML("team-a.template", uuidSource, src, ""))
	backing := ""
	if running {
		backing = src
	}
	h.define("h1", "team-b.copy", uuidClone, depDomainXML("team-b.copy", uuidClone, clone, backing))
	return h, vp, src, clone
}

func TestDiskDependents_LinkedClone(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			_, vp, src, clone := linkedCloneHost(t, running)
			ctx := context.Background()

			n, err := diskDependents(ctx, vp, uuidSource, []string{src})
			require.NoError(t, err)
			assert.Equal(t, 1, n, "the clone depends on the source's disk")

			n, err = diskDependents(ctx, vp, uuidClone, []string{clone})
			require.NoError(t, err)
			assert.Zero(t, n, "nothing depends on the clone's own overlay")
		})
	}
}

func TestDiskDependents_FullCloneIsIndependent(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	src := h.file(h.images, "team-a.template-disk.qcow2")
	full := h.file(h.images, "team-b.full-disk.qcow2") // qemu-img convert: no backing file
	h.define("h1", "team-a.template", uuidSource, depDomainXML("team-a.template", uuidSource, src, ""))
	h.define("h1", "team-b.full", uuidFull, depDomainXML("team-b.full", uuidFull, full, ""))

	for self, disk := range map[string]string{uuidSource: src, uuidFull: full} {
		n, err := diskDependents(context.Background(), vp, self, []string{disk})
		require.NoError(t, err)
		assert.Zero(t, n, "a full clone and its source share no file")
	}
}

func TestDiskDependents_CountsEveryDependentAndSharedDisks(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	src := h.file(h.images, "src-disk.qcow2")
	c1 := h.file(h.images, "c1-disk.qcow2")
	h.overlay(c1, src)
	h.define("h1", "src", uuidSource, depDomainXML("src", uuidSource, src, ""))
	h.define("h1", "c1", uuidClone, depDomainXML("c1", uuidClone, c1, ""))
	// Another domain attaches the source's disk file directly.
	h.define("h1", "sharer", uuidOther, depDomainXML("sharer", uuidOther, src, ""))

	n, err := diskDependents(context.Background(), vp, uuidSource, []string{src})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestDiskDependents_SymlinkedPathIsCanonicalized(t *testing.T) {
	h, vp, src, _ := linkedCloneHost(t, false)
	link := filepath.Join(h.images, "alias.qcow2")
	require.NoError(t, os.Symlink(src, link))
	n, err := diskDependents(context.Background(), vp, uuidSource, []string{link})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a symlink to the backing file is the backing file")
}

func TestDiskDependents_FailsClosed(t *testing.T) {
	_, vp, src, clone := linkedCloneHost(t, false)
	require.NoError(t, os.WriteFile(clone+".chainfail", nil, 0o600))
	require.NoError(t, os.WriteFile(clone+".info.json", []byte(`{"format":"qcow2"`), 0o600)) // unparsable
	_, err := diskDependents(context.Background(), vp, uuidSource, []string{src})
	require.Error(t, err, "an unreadable backing chain of another domain is never read as 'no dependents'")

	_, err = diskDependents(context.Background(), vp, "", []string{src})
	require.Error(t, err, "without its own UUID the domain would count as its own dependent")
}

func TestRefuseIfDiskHasDependents(t *testing.T) {
	h, vp, _, _ := linkedCloneHost(t, true)
	ctx := context.Background()

	for _, op := range []string{guardOpSnapshotCreate, guardOpSnapshotDelete, guardOpSnapshotRevert} {
		err := refuseIfDiskHasDependents(ctx, vp, "team-a.template", op)
		var de *diskDependentsError
		require.True(t, stderrors.As(err, &de), "%s of the source is refused: %v", op, err)
		assert.Equal(t, 1, de.dependents)
		requireDiskInUseStatus(t, err, false)
		assert.Contains(t, err.Error(), `"team-a.template"`)
		assert.Contains(t, err.Error(), "delete the linked clones first")
		assert.NotContains(t, err.Error(), h.images, "no host path in the tenant-visible message")
		assert.NotContains(t, err.Error(), "team-b.copy", "never names the other domain")

		// Addressed by UUID (a clustered core's handle), the same answer.
		require.Error(t, refuseIfDiskHasDependents(ctx, vp, uuidSource, op))

		// The clone's own snapshots are its own business.
		require.NoError(t, refuseIfDiskHasDependents(ctx, vp, "team-b.copy", op))
	}

	// Once the clone is gone, the source is free again.
	h.undefine("h1", "team-b.copy", uuidClone)
	require.NoError(t, refuseIfDiskHasDependents(ctx, vp, "team-a.template", guardOpSnapshotRevert))
}

func TestRefuseIfDiskHasDependents_UnreadableDomainFailsClosed(t *testing.T) {
	h := newFakeHost(t)
	vp := h.host("h1")
	err := refuseIfDiskHasDependents(context.Background(), vp, "ghost", guardOpSnapshotRevert)
	require.Error(t, err)
	assert.True(t, contracts.IsRetryable(err), "a failed check is retryable, never a pass: %v", err)
	assert.NotContains(t, err.Error(), "virsh", "host command details stay in the provider log")
}

// requireDiskInUseStatus asserts err crosses gRPC as FailedPrecondition with
// the VM_DISK_IN_USE ErrorInfo (and VM_OPERATION_FAILED when routed).
func requireDiskInUseStatus(t *testing.T, err error, routed bool) {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "must carry a gRPC status: %v", err)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	reasons := map[string]bool{}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == contracts.ErrorInfoDomain {
			reasons[info.GetReason()] = true
		}
	}
	assert.True(t, reasons[contracts.VMDiskInUseReason], "VM_DISK_IN_USE detail")
	assert.Equal(t, routed, reasons[contracts.VMOperationFailedReason], "VM_OPERATION_FAILED detail only on the routed path")
}
