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
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/internal/storage/s3fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The single-host ExportDisk and ImportDisk of the s3 and nfs backends run on
// an ssh:// libvirt transport. These tests drive them through the gRPC Server
// over the fake host of snapshot_clone_disk_singlehost_test.go: a connection
// that reports an ssh:// URI, runs every host command on the fake tools, and
// lets a test script the byte streams (Stream, StreamIn). Every failure point
// is checked for the answer the manager will read (rc2 R21).

// transferConn is a libvirtConn over the fake host that reports an ssh://
// transport and whose byte streams a test scripts.
type transferConn struct {
	*virshConn
	stream       func(ctx context.Context, argv ...string) (io.ReadCloser, error)
	streamIn     func(ctx context.Context, r io.Reader, argv ...string) error
	copyToRemote func(ctx context.Context, src io.Reader, volumeName string) (string, error)
}

func (c *transferConn) uri() string { return "qemu+ssh://r21@single/system" }

// Unwrap lets virshOf reach the fake host's VirshProvider.
func (c *transferConn) Unwrap() hostconn.Conn { return c.virshConn }

func (c *transferConn) Stream(ctx context.Context, argv ...string) (io.ReadCloser, error) {
	return c.stream(ctx, argv...)
}

func (c *transferConn) StreamIn(ctx context.Context, r io.Reader, argv ...string) error {
	return c.streamIn(ctx, r, argv...)
}

func (c *transferConn) copyDiskToRemote(ctx context.Context, src io.Reader, volumeName string) (string, error) {
	return c.copyToRemote(ctx, src, volumeName)
}

// transferFixture is the fake host with an ssh:// connection and a fake S3.
type transferFixture struct {
	*scdFixture
	conn *transferConn
	s3   *s3fake.Server
}

const (
	transferBucket = "stage"
	transferNFSURL = "nfs://nfs.example/exports/vmmigrations_team-a_web_export.qcow2?gid=3000&uid=3000"
)

// newTransferFixture builds the fixture. The fake tools answer what the
// imports read (free space, a staged qcow2 header, qemu-img sub-commands that
// a test can fail or end with a chosen status).
func newTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
	r21ScriptedExits(t)
	r21Patch(t, "  fail qemu-img\n", "  fail qemu-img; fail \"qemu-img-$1\"\n")
	r21Patch(t, "*) exit 0 ;;\nesac\n", "stat) case \"$1\" in -f) echo \"100000000 4096\" ;; -c) echo 1048576 ;; esac ;;\n*) fail \"$tool\"; exit 0 ;;\nesac\n")
	// The pool is where the suite's TestMain says the default pool must be.
	r21Patch(t, "<path>/var/lib/libvirt/images</path>", "<path>"+defaultPoolRequiredDir+"</path>")
	head := base64.StdEncoding.EncodeToString(minimalQCOW2Head())
	r21Patch(t, "sh)\n  case \"$2\" in\n", "sh)\n  case \"$2\" in\n  "+shellQuote(stagedHeadScript)+") printf '%s\\n' '"+head+"' ;;\n")

	conn := &transferConn{virshConn: newVirshConn("single", fx.p.virshProvider)}
	conn.stream = func(context.Context, ...string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("flattened qcow2 bytes")), nil
	}
	conn.streamIn = func(_ context.Context, r io.Reader, _ ...string) error {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	conn.copyToRemote = func(context.Context, io.Reader, string) (string, error) {
		// A scratch path: nothing in these tests may name the real host's.
		return filepath.Join(defaultPoolRequiredDir, "team-a.web-migrated.qcow2"), nil
	}
	reg, err := hostconn.NewRegistry(conn)
	require.NoError(t, err)
	fx.p.registry = reg

	srv := s3fake.New(transferBucket)
	t.Cleanup(srv.Close)
	return &transferFixture{scdFixture: fx, conn: conn, s3: srv}
}

func (f *transferFixture) server() *Server { return NewServer(f.p) }

var transferCreds = map[string]string{migration.CredKeyAccessKeyID: "AK", migration.CredKeySecretAccessKey: "SK"}

func (f *transferFixture) s3Options(t *testing.T) string {
	t.Helper()
	js, err := migration.MarshalStorageOptions(migration.StorageOptions{Backend: migration.BackendS3, Bucket: transferBucket,
		Endpoint: f.s3.Endpoint(), UsePathStyle: true})
	require.NoError(t, err)
	return js
}

func (f *transferFixture) s3ExportRequest(t *testing.T) *providerv1.ExportDiskRequest {
	t.Helper()
	return &providerv1.ExportDiskRequest{VmId: scdDomain, BackendType: migration.BackendS3, DestinationUrl: "s3://" + transferBucket + "/exports/disk.qcow2",
		Credentials: transferCreds, StorageOptionsJson: f.s3Options(t)}
}

func nfsExportRequest() *providerv1.ExportDiskRequest {
	return &providerv1.ExportDiskRequest{VmId: scdDomain, BackendType: migration.BackendNFS, DestinationUrl: transferNFSURL}
}

func (f *transferFixture) s3ImportRequest(t *testing.T) *providerv1.ImportDiskRequest {
	t.Helper()
	f.s3.Put("imports/disk.qcow2", []byte("staged qcow2 object"))
	return &providerv1.ImportDiskRequest{BackendType: migration.BackendS3, SourceUrl: "s3://" + transferBucket + "/imports/disk.qcow2",
		Format: "qcow2", TargetName: "imported", Credentials: transferCreds, StorageOptionsJson: f.s3Options(t)}
}

func nfsImportRequest() *providerv1.ImportDiskRequest {
	return &providerv1.ImportDiskRequest{BackendType: migration.BackendNFS, SourceUrl: transferNFSURL, TargetName: "imported"}
}

// calledWith reports whether a logged host call contains all the fragments.
func (f *transferFixture) calledWith(fragments ...string) bool {
	for _, c := range f.calls() {
		ok := true
		for _, fr := range fragments {
			if !strings.Contains(c, fr) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestSingleHostExportS3_Outcomes: every failure point of the s3 export.
func TestSingleHostExportS3_Outcomes(t *testing.T) {
	const hostSecret = "/var/lib/libvirt/images"

	t.Run("a: an unusable storage configuration is InvalidArgument", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := fx.s3ExportRequest(t)
		req.Credentials = nil
		_, err := fx.server().ExportDisk(context.Background(), req)
		assertRefusalStatus(t, err, codes.InvalidArgument)
		assertConstantText(t, err, "AK", "SK", transferBucket)
		assert.False(t, fx.calledWith("qemu-img convert"), "nothing is flattened for a refused request")
	})
	t.Run("a: the source domain does not exist - NotFound", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := fx.s3ExportRequest(t)
		req.VmId = "gone"
		_, err := fx.server().ExportDisk(context.Background(), req)
		assertRefusalStatus(t, err, codes.NotFound)
	})
	t.Run("a: the source disk is not a disk of the domain - InvalidArgument", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := fx.s3ExportRequest(t)
		req.DiskId = "/etc/shadow"
		_, err := fx.server().ExportDisk(context.Background(), req)
		assertRefusalStatus(t, err, codes.InvalidArgument)
	})
	t.Run("a: the domain's definition cannot be read - nothing done", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("single", "fail-dumpxml", "")
		_, err := fx.server().ExportDisk(context.Background(), fx.s3ExportRequest(t))
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, "scripted", hostSecret)
	})
	t.Run("a: the source chain is refused - FailedPrecondition", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "symlink-"+filepath.Base(scdDiskPath), "")
		_, err := fx.server().ExportDisk(context.Background(), fx.s3ExportRequest(t))
		assertRefusalStatus(t, err, codes.FailedPrecondition)
		assert.False(t, fx.calledWith("qemu-img", "convert"), "a refused chain is never read")
	})
	t.Run("a: the flatten fails - nothing published, the private directory is removed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-qemu-img-convert", "")
		_, err := fx.server().ExportDisk(context.Background(), fx.s3ExportRequest(t))
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, "scripted", "qemu-img", hostSecret)
		assert.True(t, fx.calledWith("rm -rf"), "the staging directory is removed: %v", fx.calls())
		assert.Empty(t, fx.s3.Keys(), "nothing was uploaded")
	})
	t.Run("c: the flatten is killed or its connection drops - unknown (it may still run; a retry would start a second)", func(t *testing.T) {
		for name, code := range map[string]string{"killed": "137", "connection": "255"} {
			t.Run(name, func(t *testing.T) {
				fx := newTransferFixture(t)
				fx.script("local", "exit-qemu-img-convert", code)
				_, err := fx.server().ExportDisk(context.Background(), fx.s3ExportRequest(t))
				assertUnknownOutcome(t, err)
				assert.Empty(t, fx.s3.Keys(), "nothing was published in any case")
			})
		}
	})
	t.Run("a: the host stream cannot start - nothing published", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.conn.stream = func(context.Context, ...string) (io.ReadCloser, error) { return nil, errors.New("no session") }
		_, err := fx.server().ExportDisk(context.Background(), fx.s3ExportRequest(t))
		assertNotPerformedStatus(t, err)
	})
	t.Run("b: the service refuses the upload (4xx) - not published", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.s3.FailWith(403)
		_, err := fx.server().ExportDisk(context.Background(), fx.s3ExportRequest(t))
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, "403", transferBucket, "AccessDenied")
		assert.Zero(t, fx.s3.CompleteRequests(), "the client never sent the completing call: nothing can be published")
	})
	t.Run("b: the host stream breaks mid-upload - not published", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.conn.stream = func(context.Context, ...string) (io.ReadCloser, error) {
			return io.NopCloser(io.MultiReader(strings.NewReader("part"), failingReader{errors.New("ssh: stream broke")})), nil
		}
		_, err := fx.server().ExportDisk(context.Background(), fx.s3ExportRequest(t))
		assertNotPerformedStatus(t, err)
		assert.Empty(t, fx.s3.Keys(), "the object was never completed")
		var started bool
		for _, r := range fx.s3.Requests() {
			started = started || r.Method == "POST"
		}
		assert.True(t, started, "the upload had started: it is the broken source that stopped it")
		assert.Zero(t, fx.s3.CompleteRequests(), "an upload is completed only after its source was read to the end")
	})
	t.Run("c: the completing call took effect, its answer was lost and its retry got NoSuchUpload (404) - unknown", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.s3.LoseNextComplete()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := fx.server().ExportDisk(ctx, fx.s3ExportRequest(t))
		assertUnknownOutcome(t, err)
		assert.Equal(t, []string{"exports/disk.qcow2"}, fx.s3.Keys(), "the object was published although the call failed")
		assert.GreaterOrEqual(t, fx.s3.CompleteRequests(), 2, "the completing call was retried")
	})
	t.Run("c: the completing call took effect, its answer was lost and its retry got AccessDenied (403) - unknown", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.s3.LoseNextComplete()
		fx.s3.AnswerRetriedCompleteWith(403)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := fx.server().ExportDisk(ctx, fx.s3ExportRequest(t))
		assertUnknownOutcome(t, err)
		assert.Equal(t, []string{"exports/disk.qcow2"}, fx.s3.Keys(), "the object was published although the retried call was refused")
		assert.GreaterOrEqual(t, fx.s3.CompleteRequests(), 2)
	})
	t.Run("b: the service fails the upload (5xx) before the source was read to the end - not published", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.s3.FailWith(500)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := fx.server().ExportDisk(ctx, fx.s3ExportRequest(t))
		assertNotPerformedStatus(t, err)
		assert.Zero(t, fx.s3.CompleteRequests())
	})
	t.Run("b: the service cannot be reached - not published", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := fx.s3ExportRequest(t)
		fx.s3.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := fx.server().ExportDisk(ctx, req)
		assertNotPerformedStatus(t, err)
	})
	t.Run("c: the completing call fails (5xx) - unknown (the service may have applied it)", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.s3.FailCompleteWith(500)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := fx.server().ExportDisk(ctx, fx.s3ExportRequest(t))
		assertUnknownOutcome(t, err)
		assert.Positive(t, fx.s3.CompleteRequests())
	})
	t.Run("c: the completing call is refused (403) - still unknown once the source was read to the end", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.s3.FailCompleteWith(403)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := fx.server().ExportDisk(ctx, fx.s3ExportRequest(t))
		assertUnknownOutcome(t, err)
	})
	t.Run("success uploads the object", func(t *testing.T) {
		fx := newTransferFixture(t)
		resp, err := fx.server().ExportDisk(context.Background(), fx.s3ExportRequest(t))
		require.NoError(t, err)
		assert.NotEmpty(t, resp.GetChecksum())
		assert.Equal(t, []string{"exports/disk.qcow2"}, fx.s3.Keys())
	})
}

// TestSingleHostExportNFS_Outcomes: every failure point of the nfs export.
func TestSingleHostExportNFS_Outcomes(t *testing.T) {
	t.Run("a: a destination that is not nfs:// is InvalidArgument", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := nfsExportRequest()
		req.DestinationUrl = "http://example/x"
		_, err := fx.server().ExportDisk(context.Background(), req)
		assertRefusalStatus(t, err, codes.InvalidArgument)
		assertConstantText(t, err, "http://example")
	})
	t.Run("a: the source domain does not exist - NotFound", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := nfsExportRequest()
		req.VmId = "gone"
		_, err := fx.server().ExportDisk(context.Background(), req)
		assertRefusalStatus(t, err, codes.NotFound)
	})
	t.Run("a: the source chain is refused - FailedPrecondition, nothing written", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "symlink-"+filepath.Base(scdDiskPath), "")
		_, err := fx.server().ExportDisk(context.Background(), nfsExportRequest())
		assertRefusalStatus(t, err, codes.FailedPrecondition)
		assert.False(t, fx.calledWith("qemu-img", "convert"))
	})
	t.Run("a: the chain cannot be read - nothing written", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-qemu-img-info", "")
		_, err := fx.server().ExportDisk(context.Background(), nfsExportRequest())
		require.Error(t, err)
		assert.NotEqual(t, codes.Unknown, status.Code(err), "the checks before the convert only read: %v", err)
		assert.False(t, fx.calledWith("qemu-img", "convert"))
	})
	t.Run("b: the convert ends with an error - failed, nothing exported", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-qemu-img-convert", "")
		_, err := fx.server().ExportDisk(context.Background(), nfsExportRequest())
		assertNotPerformedStatus(t, err)
		assert.Equal(t, wireExportFailed, status.Convert(err).Message())
		assertConstantText(t, err, "nfs.example", "scripted", "exports")
	})
	t.Run("c: the convert is killed - unknown (it may still write the export)", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "exit-qemu-img-convert", "137")
		_, err := fx.server().ExportDisk(context.Background(), nfsExportRequest())
		assertUnknownOutcome(t, err)
	})
	t.Run("c: the connection drops during the convert - unknown", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "exit-qemu-img-convert", "255")
		_, err := fx.server().ExportDisk(context.Background(), nfsExportRequest())
		assertUnknownOutcome(t, err)
	})
	t.Run("success runs the convert", func(t *testing.T) {
		fx := newTransferFixture(t)
		_, err := fx.server().ExportDisk(context.Background(), nfsExportRequest())
		require.NoError(t, err)
		assert.True(t, fx.calledWith("qemu-img", "convert", "nfs://nfs.example"))
	})
}

// TestSingleHostExportPVC_Outcomes: the pvc export runs in the provider's own
// process, so every failure is definite.
func TestSingleHostExportPVC_Outcomes(t *testing.T) {
	run := func(t *testing.T, setup func(*scdFixture), req *providerv1.ExportDiskRequest) error {
		t.Helper()
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		r21ScriptedExits(t)
		if setup != nil {
			setup(fx)
		}
		_, err := NewServer(fx.p).ExportDisk(context.Background(), req)
		return err
	}
	pvcReq := func(url, format string) *providerv1.ExportDiskRequest {
		return &providerv1.ExportDiskRequest{VmId: scdDomain, DestinationUrl: url, Format: format}
	}
	t.Run("a: an unsupported format is InvalidArgument", func(t *testing.T) {
		assertRefusalStatus(t, run(t, nil, pvcReq("pvc://vol/k.vmdk", "vmdk")), codes.InvalidArgument)
	})
	t.Run("a: a destination without a volume is InvalidArgument", func(t *testing.T) {
		assertRefusalStatus(t, run(t, nil, pvcReq("pvc:///k.qcow2", "qcow2")), codes.InvalidArgument)
	})
	t.Run("a: the source domain does not exist - NotFound or nothing done", func(t *testing.T) {
		req := pvcReq("pvc://vol/k.qcow2", "qcow2")
		req.VmId = "gone"
		err := run(t, nil, req)
		require.Error(t, err)
		assert.NotEqual(t, codes.Unknown, status.Code(err), "%v", err)
	})
	t.Run("a: the disk cannot be read - nothing done", func(t *testing.T) {
		err := run(t, func(fx *scdFixture) { fx.script("local", "fail-qemu-img", "") }, pvcReq("pvc://vol/k.qcow2", "qcow2"))
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, "scripted", "qemu-img", "/var/lib")
	})
	t.Run("b: the migration volume is not mounted - failed, nothing is writing", func(t *testing.T) {
		err := run(t, nil, pvcReq("pvc://no-such-volume-r21/k.qcow2", "qcow2"))
		assertNotPerformedStatus(t, err)
		assert.Equal(t, wirePVCExportFailed, status.Convert(err).Message())
		assertConstantText(t, err, "no-such-volume-r21", "/mnt")
	})
}

// TestSingleHostImportS3_Outcomes: every failure point of the s3 import. All
// but the rename onto the landing name work in a private directory that is
// removed.
func TestSingleHostImportS3_Outcomes(t *testing.T) {
	poolSecret := defaultPoolRequiredDir
	t.Run("a: an unusable storage configuration is InvalidArgument", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := fx.s3ImportRequest(t)
		req.Credentials = nil
		_, err := fx.server().ImportDisk(context.Background(), req)
		assertRefusalStatus(t, err, codes.InvalidArgument)
		assert.False(t, fx.calledWith("qemu-img", "convert"))
	})
	t.Run("a: a conflicting target name is InvalidArgument", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := fx.s3ImportRequest(t)
		req.TargetName = "other"
		req.TargetVm = &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"}
		_, err := fx.server().ImportDisk(context.Background(), req)
		assertRefusalStatus(t, err, codes.InvalidArgument)
	})
	t.Run("a: the staged object does not exist - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := fx.s3ImportRequest(t)
		req.SourceUrl = "s3://" + transferBucket + "/imports/missing.qcow2"
		_, err := fx.server().ImportDisk(context.Background(), req)
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, "missing.qcow2", transferBucket)
		assert.False(t, fx.calledWith("mv"), "nothing is published")
	})
	t.Run("a: the host stage fails - nothing landed, the directory is removed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.conn.streamIn = func(_ context.Context, r io.Reader, _ ...string) error {
			_, _ = io.Copy(io.Discard, r)
			return errors.New("ssh: stage failed at /var/lib/libvirt/images/.virtrigaud-write-x")
		}
		_, err := fx.server().ImportDisk(context.Background(), fx.s3ImportRequest(t))
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, poolSecret, ".virtrigaud-write")
		assert.True(t, fx.calledWith("rm -rf"), "%v", fx.calls())
		assert.False(t, fx.calledWith("mv"))
	})
	t.Run("a: the staged header references another file - refused", func(t *testing.T) {
		fx := newTransferFixture(t)
		r21Patch(t, "if [ \"$1\" = info ]; then", "if [ \"$1\" = info ]; then printf '{\"format\":\"qcow2\",\"backing-filename\":\"/etc/x\"}\\n'; exit 0; fi\n  if [ \"$1\" = info-unused ]; then")
		_, err := fx.server().ImportDisk(context.Background(), fx.s3ImportRequest(t))
		assertRefusalStatus(t, err, codes.InvalidArgument)
		assert.False(t, fx.calledWith("mv"))
	})
	t.Run("a: the convert fails - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-qemu-img-convert", "")
		_, err := fx.server().ImportDisk(context.Background(), fx.s3ImportRequest(t))
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, "scripted", "qemu-img", poolSecret)
		assert.False(t, fx.calledWith("mv"))
	})
	t.Run("c: the convert is killed or the connection drops - unknown (it may still run; a retry would start a second)", func(t *testing.T) {
		for name, code := range map[string]string{"killed": "137", "connection": "255"} {
			t.Run(name, func(t *testing.T) {
				fx := newTransferFixture(t)
				fx.script("local", "exit-qemu-img-convert", code)
				_, err := fx.server().ImportDisk(context.Background(), fx.s3ImportRequest(t))
				assertUnknownOutcome(t, err)
				assert.False(t, fx.calledWith("mv"), "the rename is only ever run by this call, which has returned")
			})
		}
	})
	t.Run("a: the check of the converted disk fails - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-qemu-img-check", "")
		_, err := fx.server().ImportDisk(context.Background(), fx.s3ImportRequest(t))
		assertNotPerformedStatus(t, err)
		assert.False(t, fx.calledWith("mv"))
	})
	t.Run("b: the rename fails with an exit status - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-mv", "")
		_, err := fx.server().ImportDisk(context.Background(), fx.s3ImportRequest(t))
		assertNotPerformedStatus(t, err)
		assert.Equal(t, wireImportFailed, status.Convert(err).Message())
	})
	t.Run("c: the rename is killed or its connection drops - unknown", func(t *testing.T) {
		for name, code := range map[string]string{"killed": "137", "connection": "255"} {
			t.Run(name, func(t *testing.T) {
				fx := newTransferFixture(t)
				fx.script("local", "exit-mv", code)
				_, err := fx.server().ImportDisk(context.Background(), fx.s3ImportRequest(t))
				assertUnknownOutcome(t, err)
			})
		}
	})
	t.Run("success lands the disk", func(t *testing.T) {
		fx := newTransferFixture(t)
		resp, err := fx.server().ImportDisk(context.Background(), fx.s3ImportRequest(t))
		require.NoError(t, err)
		assert.Equal(t, "imported", resp.GetDiskId())
		assert.True(t, fx.calledWith("mv"))
	})
}

// TestSingleHostImportNFS_Outcomes: every failure point of the nfs import.
func TestSingleHostImportNFS_Outcomes(t *testing.T) {
	t.Run("a: a source that is not nfs:// is InvalidArgument", func(t *testing.T) {
		fx := newTransferFixture(t)
		req := nfsImportRequest()
		req.SourceUrl = "http://example/x"
		_, err := fx.server().ImportDisk(context.Background(), req)
		assertRefusalStatus(t, err, codes.InvalidArgument)
		assertConstantText(t, err, "http://example")
	})
	t.Run("a: the read of the staged object fails - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-qemu-img-info", "")
		_, err := fx.server().ImportDisk(context.Background(), nfsImportRequest())
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, "nfs.example", "exports")
		assert.False(t, fx.calledWith("mv"))
	})
	t.Run("a: the convert ends with an error - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-qemu-img-convert", "")
		_, err := fx.server().ImportDisk(context.Background(), nfsImportRequest())
		assertNotPerformedStatus(t, err)
		assert.False(t, fx.calledWith("mv"))
	})
	t.Run("c: the convert is killed - unknown (it may still run)", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "exit-qemu-img-convert", "137")
		_, err := fx.server().ImportDisk(context.Background(), nfsImportRequest())
		assertUnknownOutcome(t, err)
		assert.False(t, fx.calledWith("mv"))
	})
	t.Run("c: the bounded copy of the staged object is killed - unknown", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "exit-qemu-img-dd", "255")
		_, err := fx.server().ImportDisk(context.Background(), nfsImportRequest())
		assertUnknownOutcome(t, err)
	})
	t.Run("a: the bounded copy ends with an error - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-qemu-img-dd", "")
		_, err := fx.server().ImportDisk(context.Background(), nfsImportRequest())
		assertNotPerformedStatus(t, err)
	})
	t.Run("b: the rename fails with an exit status - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "fail-mv", "")
		_, err := fx.server().ImportDisk(context.Background(), nfsImportRequest())
		assertNotPerformedStatus(t, err)
	})
	t.Run("c: the rename is killed - unknown", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.script("local", "exit-mv", "137")
		_, err := fx.server().ImportDisk(context.Background(), nfsImportRequest())
		assertUnknownOutcome(t, err)
	})
	t.Run("success lands the disk", func(t *testing.T) {
		fx := newTransferFixture(t)
		_, err := fx.server().ImportDisk(context.Background(), nfsImportRequest())
		require.NoError(t, err)
		assert.True(t, fx.calledWith("mv"))
	})
}

// TestSingleHostImportPVC_Outcomes: the pvc import copies the staged disk onto
// the host (copyDiskToRemote: a private directory, then a rename onto the
// landing name). Until that rename a failure landed nothing; after it the disk
// is on the host and a failure keeps its historical answer.
func TestSingleHostImportPVC_Outcomes(t *testing.T) {
	staged := func(t *testing.T) string {
		p := filepath.Join(t.TempDir(), "disk.qcow2")
		require.NoError(t, os.WriteFile(p, []byte("staged"), 0o600))
		return p
	}
	req := func(url string) *providerv1.ImportDiskRequest {
		return &providerv1.ImportDiskRequest{SourceUrl: url, TargetName: "imported", BackendType: migration.BackendPVC}
	}
	t.Run("a: an unsupported source scheme is InvalidArgument", func(t *testing.T) {
		fx := newTransferFixture(t)
		_, err := fx.server().ImportDisk(context.Background(), req("ftp://x/y"))
		assertRefusalStatus(t, err, codes.InvalidArgument)
		assertConstantText(t, err, "ftp://")
	})
	t.Run("a: the staged disk is missing - NotFound", func(t *testing.T) {
		fx := newTransferFixture(t)
		_, err := fx.server().ImportDisk(context.Background(), req(stagedPVCURL(t, filepath.Join(t.TempDir(), "disk.qcow2"))))
		assertRefusalStatus(t, err, codes.NotFound)
		assertConstantText(t, err, "disk.qcow2")
	})
	t.Run("a: the check of the landing name fails - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		// A file exists at the landing name, and the host's domains cannot be
		// listed to tell whether another domain uses it.
		r21Patch(t, "sh)\n  case \"$2\" in\n", "sh)\n  case \"$2\" in\n  "+shellQuote(targetKindScript)+") echo "+pathExistsMarker+" ;;\n")
		r21Patch(t, "    list)\n      if [", "    list)\n      fail list\n      if [")
		fx.script("single", "fail-list", "")
		_, err := fx.server().ImportDisk(context.Background(), req(stagedPVCURL(t, staged(t))))
		assertNotPerformedStatus(t, err)
		assert.False(t, fx.calledWith("mv"))
	})
	t.Run("a: the copy to the host fails before the rename - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.conn.copyToRemote = func(context.Context, io.Reader, string) (string, error) {
			return "", errors.New("ssh: copy failed at /var/lib/libvirt/images/.virtrigaud-write-x")
		}
		_, err := fx.server().ImportDisk(context.Background(), req(stagedPVCURL(t, staged(t))))
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, "/var/lib", ".virtrigaud-write")
	})
	t.Run("b: the rename fails with an exit status - nothing landed", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.conn.copyToRemote = func(context.Context, io.Reader, string) (string, error) {
			return "", atStage(stagePublish, virshFailure(1, "mv: cannot move"))
		}
		_, err := fx.server().ImportDisk(context.Background(), req(stagedPVCURL(t, staged(t))))
		assertNotPerformedStatus(t, err)
		assert.Equal(t, wireImportFailed, status.Convert(err).Message())
	})
	t.Run("c: the rename loses its connection - unknown", func(t *testing.T) {
		fx := newTransferFixture(t)
		fx.conn.copyToRemote = func(context.Context, io.Reader, string) (string, error) {
			return "", atStage(stagePublish, virshFailure(-1, "connection reset"))
		}
		_, err := fx.server().ImportDisk(context.Background(), req(stagedPVCURL(t, staged(t))))
		assertUnknownOutcome(t, err)
	})
	t.Run("c: a failure after the disk landed keeps its historical answer - unknown", func(t *testing.T) {
		fx := newTransferFixture(t)
		// The copy lands the disk; the volume step then cannot find it (the
		// fake host has no such file), as any later failure would.
		_, err := fx.server().ImportDisk(context.Background(), req(stagedPVCURL(t, staged(t))))
		assertUnknownOutcome(t, err)
	})
}
