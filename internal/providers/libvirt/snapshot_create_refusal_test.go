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
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestSnapshotRefusedForConfig: libvirt's refusals of a snapshot the VM's
// configuration does not support are told from the transient ones (a busy
// domain, a domain in the wrong state).
func TestSnapshotRefusedForConfig(t *testing.T) {
	for stderr, want := range map[string]bool{
		"error: unsupported configuration: internal snapshot for disk vda unsupported for storage type raw":                 true,
		"error: Operation not supported: internal snapshots of a VM with pflash based firmware are not supported":           true,
		"error: argument unsupported: unsupported flags (0x400) in function qemuDomainSnapshotCreateXML":                    true,
		"error: invalid argument: disk 'vda' must use snapshot mode 'internal'":                                             true,
		"error: Timed out during operation: cannot acquire state change lock (held by monitor=x)":                           false,
		"error: Requested operation is not valid: domain is not running":                                                    false,
		"error: unable to save metadata for snapshot x":                                                                     false,
		"error: internal error: unable to execute QEMU command 'blockdev-snapshot-sync': Operation not supported by device": false,
	} {
		assert.Equal(t, want, snapshotRefusedForConfig(virshFailure(1, stderr)), stderr)
		if want {
			assert.True(t, snapshotRefusedBeforeExecution(virshFailure(1, stderr)), "a config refusal is a refusal before execution: %s", stderr)
		}
	}
}

// TestSingleHostSnapshotCreate_ConfigRefusal: a snapshot libvirt refuses for
// the VM's configuration — proven unchanged afterwards — is a definite
// InvalidArgument (the manager fails it; sending it again cannot help); a
// busy domain stays "not created", which the manager may send again.
func TestSingleHostSnapshotCreate_ConfigRefusal(t *testing.T) {
	run := func(t *testing.T, stderr string, leaves bool) error {
		t.Helper()
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		r21ScriptedExits(t)
		fx.script("single", "state", "running\n")
		fx.script("single", "fail-snapshot-create-as", "")
		fx.script("single", "stderr-snapshot-create-as", stderr)
		if leaves {
			fx.script("single", "leaves-snapshot-create-as", "")
		}
		_, err := NewServer(fx.p).SnapshotCreate(context.Background(),
			&providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: "mem", Description: "d", IncludeMemory: true})
		return err
	}
	const pflash = "error: Operation not supported: internal snapshots of a VM with pflash based firmware are not supported\n"

	t.Run("pflash memory snapshot: InvalidArgument, constant text", func(t *testing.T) {
		err := run(t, pflash, false)
		assertRefusalStatus(t, err, codes.InvalidArgument)
		assert.Equal(t, wireSnapshotRefusedForConfig, status.Convert(err).Message())
		assertConstantText(t, err, "pflash", "virsh", "vrgold")
	})
	t.Run("the refusal but the snapshot exists afterwards: unknown", func(t *testing.T) {
		assertUnknownOutcome(t, run(t, pflash, true))
	})
	t.Run("a busy domain: not created (not performed)", func(t *testing.T) {
		err := run(t, "error: Timed out during operation: cannot acquire state change lock (held by monitor=x)\n", false)
		assertNotPerformedStatus(t, err)
		assert.Equal(t, wireSnapshotNotCreated, status.Convert(err).Message())
	})
}
