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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin that a clustered clone or s3 export that fails — or whose
// copy is stopped by its time budget — never leaves a partial copy of the
// tenant's disk on the host: the clone's disk is removed under the clone's
// lock (unless another attempt holds it), and the export's temporary copy is
// removed whatever happens once it exists.

// clonedDiskRemoval is the logged removal of the clone's disk: through the
// guard, under the clone's lock (waiting for a copy that is still exiting),
// with sudo (the disk may already be libvirt-qemu's).
const clonedDiskRemoval = "local flock -w " + routedCleanupLockWait + " -E 75 " + cloneLock + " sudo rm -f -- " + cloneTargetDisk

// failQemuImgConvert puts a qemu-img in front of the routed fakes that fails
// every `convert` (after logging it) and hands every other call on.
func failQemuImgConvert(t *testing.T, fx *routedSCD) {
	t.Helper()
	script := "#!/bin/sh\nif [ \"$1\" = convert ]; then\n" +
		"  printf 'local qemu-img %s\\n' \"$*\" >> \"$FAKE_SCD_DIR/calls.log\"\n" +
		"  echo 'qemu-img: error while writing' >&2; exit 1\nfi\n" +
		"exec \"$FAKE_SCD_BIN/qemu-img\" \"$@\"\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "qemu-img"), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestClustered_Clone_FailedCopyIsRemoved(t *testing.T) {
	cases := map[string]struct {
		setup  func(fx *routedSCD)
		remove bool
		why    string
	}{
		"the copy failed": {
			setup: func(fx *routedSCD) { failQemuImgConvert(t, fx) }, remove: true,
			why: "a failed copy leaves a partial disk no domain references",
		},
		"the copy was stopped by its budget": {
			setup: func(fx *routedSCD) { fx.script("local", "timeout-expire", "") }, remove: true,
			why: "a stopped copy leaves a partial disk no domain references",
		},
		"another attempt holds the lock": {
			setup: func(fx *routedSCD) { fx.script("local", "flock-busy", "") }, remove: false,
			why: "the disk belongs to the attempt still writing it",
		},
		"a host tool is missing": {
			setup: func(fx *routedSCD) { fx.script("local", "flock-missing", "") }, remove: false,
			why: "nothing of this attempt ran",
		},
		"the guard refused the paths": {
			setup: func(fx *routedSCD) {
				require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(fx.staging, hostLockDirName)))
			},
			remove: false, why: "a refused path is never touched",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := sourceWeb(t, nil)
			tc.setup(fx)
			_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
			require.Error(t, err)
			calls := fx.calls()
			if tc.remove {
				assert.Contains(t, calls, clonedDiskRemoval, tc.why)
			} else {
				for _, c := range calls {
					assert.NotContains(t, c, "rm -f -- "+cloneTargetDisk, "%s: %q", tc.why, c)
				}
			}
			for _, c := range calls {
				assert.NotContains(t, c, " define ", "nothing is defined: %q", c)
			}
		})
	}
}

// TestClustered_Clone_FailureAfterTheCopyRemovesTheDisk: once the copy is
// complete, a failure before the clone's domain exists removes the disk; a
// define whose outcome is unknown keeps it (the domain may exist and use it,
// and its owner-checked Delete removes it with it).
func TestClustered_Clone_FailureAfterTheCopyRemovesTheDisk(t *testing.T) {
	t.Run("copying the cloud-init seed failed", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		failHostTool(t, "cp")
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		require.Error(t, err)
		assert.Contains(t, fx.calls(), clonedDiskRemoval)
	})
	t.Run("reading the source's definition failed: nothing was copied", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		failInactiveDumpxml(t)
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		require.Error(t, err)
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "qemu-img convert", "the definition is read before the copy: %q", c)
			assert.NotContains(t, c, "rm -f -- "+cloneTargetDisk, "%q", c)
		}
	})
	for name, absent := range map[string]bool{"define failed, domain provably absent": true, "define outcome unknown": false} {
		t.Run(name, func(t *testing.T) {
			fx := sourceWeb(t, nil)
			fx.script("local", "fail-define", "")
			if absent {
				answerDomuuidNoDomain(t, fx.dir)
			}
			_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
			st, ok := status.FromError(err)
			require.True(t, ok)
			assert.Equal(t, contracts.VMOperationFailedReason, errorInfoReason(st), "got %v", err)
			if absent {
				assert.Contains(t, fx.calls(), clonedDiskRemoval)
			} else {
				for _, c := range fx.calls() {
					assert.NotContains(t, c, "rm -f -- "+cloneTargetDisk, "the domain may use the disk: %q", c)
				}
			}
		})
	}
	t.Run("a successful clone keeps its disk", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		require.NoError(t, err)
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "rm -f -- "+cloneTargetDisk, "%q", c)
		}
	})
}

// TestClustered_Clone_DiskInUseIsNeverRemoved: a failed clone's disk that a
// domain on the host now uses (e.g. one defined over the path meanwhile) is
// left alone; so is one whose in-use check fails.
func TestClustered_Clone_DiskInUseIsNeverRemoved(t *testing.T) {
	user := strings.Replace(scdDomainXML("other", scdDomainOpts{}), scdDiskPath, cloneTargetDisk, 1)
	user = strings.Replace(user, routingDomainUUID, "33333333-4444-4555-8666-777777777777", 1)
	t.Run("in use", func(t *testing.T) {
		fx := sourceWeb(t, map[string]string{"other": user})
		failHostTool(t, "cp") // fails after the copy: the disk would be removed
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		require.Error(t, err)
		calls := fx.calls()
		assert.Contains(t, calls, "host-b virsh list --all --uuid", "the host-wide in-use check ran")
		for _, c := range calls {
			assert.NotContains(t, c, "rm -f -- "+cloneTargetDisk, "a disk a domain uses is never removed: %q", c)
		}
	})
	t.Run("the check fails", func(t *testing.T) {
		fx := sourceWeb(t, nil)
		failHostTool(t, "cp")
		failHostTool(t, "realpath")
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		require.Error(t, err)
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "rm -f -- "+cloneTargetDisk, "a disk whose use cannot be checked is left: %q", c)
		}
	})
}

// failHostTool puts tool in front of the routed fakes: it logs its call and
// fails.
func failHostTool(t *testing.T, tool string) {
	t.Helper()
	script := "#!/bin/sh\nprintf 'local " + tool + " %s\\n' \"$*\" >> \"$FAKE_SCD_DIR/calls.log\"\n" +
		"echo 'scripted failure of " + tool + "' >&2; exit 1\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// failInactiveDumpxml puts a virsh in front of the routed fakes that fails
// `dumpxml <x> --inactive` (after logging it) and hands every other call on.
func failInactiveDumpxml(t *testing.T) {
	t.Helper()
	script := "#!/bin/sh\ncase \"$*\" in *dumpxml*--inactive*)\n" +
		"  echo 'error: scripted failure of dumpxml --inactive' >&2; exit 1 ;;\nesac\n" +
		"exec \"$FAKE_SCD_TOOLS/virsh\" \"$@\"\n"
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "virsh"), []byte(script), 0o755)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestClustered_ExportDisk_S3FailedFlattenIsRemoved: the export's temporary
// copy is created (mode 0600, by mktemp) and its removal registered BEFORE the
// flatten runs, so a flatten that fails or is stopped never leaves a partial,
// readable copy of the tenant's disk in the pool.
func TestClustered_ExportDisk_S3FailedFlattenIsRemoved(t *testing.T) {
	for name, setup := range map[string]func(fx *routedSCD){
		"the flatten failed":                    func(fx *routedSCD) { failQemuImgConvert(t, fx) },
		"the flatten was stopped by its budget": func(fx *routedSCD) { fx.script("local", "timeout-expire", "") },
	} {
		t.Run(name, func(t *testing.T) {
			fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
			fx.p.hostDiskTransportFn = anyTransport
			setup(fx)
			_, err := NewServer(fx.p).ExportDisk(context.Background(), &providerv1.ExportDiskRequest{
				VmId: "web", TargetHostId: "host-b", Owner: teamAOwner, BackendType: "s3",
				DestinationUrl:     "s3://bucket/web.qcow2",
				StorageOptionsJson: `{"bucket":"bucket","endpoint":"http://127.0.0.1:1"}`,
				Credentials:        map[string]string{"accessKeyID": "a", "secretAccessKey": "s"},
			})
			require.Error(t, err)
			calls := fx.calls()
			mk, conv, rm := -1, -1, -1
			for i, c := range calls {
				switch {
				case c == "local mktemp --suffix=.qcow2 "+s3ExportTemplate:
					mk = i
				case strings.HasPrefix(c, "local qemu-img convert") || strings.HasPrefix(c, "local timeout"):
					if conv < 0 {
						conv = i
					}
				case c == "local rm -f -- "+s3ExportTemp:
					rm = i
				}
			}
			require.GreaterOrEqual(t, mk, 0, "the temp is made by mktemp: %v", calls)
			require.GreaterOrEqual(t, conv, 0, "the flatten ran: %v", calls)
			assert.Less(t, mk, conv, "the temp exists before the flatten writes it")
			assert.Greater(t, rm, conv, "the partial temp is removed after the failed flatten: %v", calls)
		})
	}
}
