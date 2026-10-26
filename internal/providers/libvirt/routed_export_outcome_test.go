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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/internal/storage/s3fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the v0.4.1-rc2 fixes of the clustered (routed) ExportDisk:
// a failure of the shared s3 / nfs core that the single-host export proves
// definite — an nfs mount the server refused, an S3 upload the service
// refused before the client read the source to its end — is answered the
// same way on a clustered host ("not performed"), instead of an unknown
// outcome that held the VMMigration for 35 minutes; genuinely unknown
// outcomes stay unknown; and no log line names the staged object's location
// (R28): the nfs server, export, path and uid/gid, the S3 bucket, key and
// endpoint.

const (
	r2NFSServer   = "nfs.example.com"
	r2NFSExport   = "/exports/tenant-a"
	r2NFSFile     = "vmmigrations_team-a_web_export.qcow2"
	r2NFSURL      = "nfs://" + r2NFSServer + r2NFSExport + "/" + r2NFSFile + "?gid=1000&uid=1000"
	r2S3Bucket    = "tenant-bucket"
	r2S3Key       = "vmmigrations/team-a/web/export.vmdk"
	r2MountDenied = `qemu-img: Could not open '` + r2NFSURL + `': Failed to mount nfs share: mount/mnt call failed with ` +
		`"RPC error: Mount failed with error MNT3ERR_ACCES(13) Permission denied(13)" (` + r2NFSServer + ":" + r2NFSExport + ")\n"
)

// streamScriptedConn is a routed host's leased connection whose Stream (the
// s3 export's read of the flattened disk) a test scripts.
type streamScriptedConn struct {
	libvirtConn
	stream func(ctx context.Context, argv ...string) (io.ReadCloser, error)
}

func (c *streamScriptedConn) Stream(ctx context.Context, argv ...string) (io.ReadCloser, error) {
	return c.stream(ctx, argv...)
}

// Unwrap lets virshOf reach the leased host's VirshProvider.
func (c *streamScriptedConn) Unwrap() hostconn.Conn { return c.libvirtConn }

// routedNFSExportReq is a routed nfs export of team-a's "web" on host-b.
func routedNFSExportReq() *providerv1.ExportDiskRequest {
	return &providerv1.ExportDiskRequest{VmId: "web", TargetHostId: "host-b", Owner: teamAOwner,
		BackendType: migration.BackendNFS, DestinationUrl: r2NFSURL}
}

// routedS3Export is the routed fixture with a fake S3 service and a scripted
// host stream.
type routedS3Export struct {
	*routedSCD
	s3 *s3fake.Server
}

func newRoutedS3Export(t *testing.T) *routedS3Export {
	t.Helper()
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.p.hostDiskTransportFn = anyTransport
	fx.p.routedDiskConnFn = func(c libvirtConn) libvirtConn {
		return &streamScriptedConn{libvirtConn: c, stream: func(context.Context, ...string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("flattened qcow2 bytes")), nil
		}}
	}
	srv := s3fake.New(r2S3Bucket)
	t.Cleanup(srv.Close)
	return &routedS3Export{routedSCD: fx, s3: srv}
}

func (f *routedS3Export) request(t *testing.T) *providerv1.ExportDiskRequest {
	t.Helper()
	js, err := migration.MarshalStorageOptions(migration.StorageOptions{Backend: migration.BackendS3, Bucket: r2S3Bucket,
		Endpoint: f.s3.Endpoint(), UsePathStyle: true})
	require.NoError(t, err)
	return &providerv1.ExportDiskRequest{VmId: "web", TargetHostId: "host-b", Owner: teamAOwner,
		BackendType: migration.BackendS3, DestinationUrl: "s3://" + r2S3Bucket + "/" + r2S3Key,
		StorageOptionsJson: js, Credentials: transferCreds}
}

// assertNoStagedLocation asserts that neither the provider log nor the wire
// text names any of the staged object's locations.
func assertNoStagedLocation(t *testing.T, logs string, err error, leaks ...string) {
	t.Helper()
	for _, l := range leaks {
		assert.NotContains(t, logs, l, "the provider log names %q", l)
		assert.NotContains(t, status.Convert(err).Message(), l, "the wire text names %q", l)
	}
}

// nfsLocationLeaks are the texts that locate the routed nfs export's object.
var nfsLocationLeaks = []string{r2NFSServer, r2NFSExport, r2NFSFile, "uid=1000", "gid=1000"}

// TestRoutedExport_NFSMountRefusedIsNotPerformed: the lab's MNT3ERR_ACCES — the
// host's qemu-img ended with an exit status of its own because the NFS server
// refused the mount, before anything was written — is "failed, nothing
// exported", as on a single host, not an unknown outcome. Nothing in the log
// or on the wire names the export.
func TestRoutedExport_NFSMountRefusedIsNotPerformed(t *testing.T) {
	fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
	fx.p.hostDiskTransportFn = anyTransport
	fx.script("local", "stderr-qemu-img-convert", r2MountDenied)
	logs := captureLog(t)

	_, err := NewServer(fx.p).ExportDisk(context.Background(), routedNFSExportReq())
	assertNotPerformedStatus(t, err)
	assert.Equal(t, wireExportFailed, status.Convert(err).Message())
	assert.True(t, slicesContainsPrefix(fx.calls(), "local qemu-img convert"), "the convert ran: %v", fx.calls())
	assertNoStagedLocation(t, logs.String(), err, nfsLocationLeaks...)
	assert.Contains(t, logs.String(), "MNT3ERR_ACCES", "the cause is logged for the operator")
}

// TestRoutedExport_NFSUnknownOutcomesStayUnknown: a convert that did not end
// on its own may still be writing the export; its outcome stays unknown (R25),
// and its log names no location either.
func TestRoutedExport_NFSUnknownOutcomesStayUnknown(t *testing.T) {
	for name, local := range map[string]map[string]string{
		"the convert's session drops":    {"dead-qemu-img-convert": ""},
		"the convert is killed (137)":    {"exit-qemu-img-convert": "137"},
		"the convert is signalled (143)": {"exit-qemu-img-convert": "143"},
	} {
		t.Run(name, func(t *testing.T) {
			fx := ownedWebOnB(t, scdDomainXML("web", scdDomainOpts{owner: ownerTeamA}))
			fx.p.hostDiskTransportFn = anyTransport
			for f, content := range local {
				fx.script("local", f, content)
			}
			logs := captureLog(t)
			_, err := NewServer(fx.p).ExportDisk(context.Background(), routedNFSExportReq())
			assert.Equal(t, codes.Unknown, status.Code(err), "%v", err)
			assert.Nil(t, definiteStatusError(err), "never a definite answer: %v", err)
			assertNoStagedLocation(t, logs.String(), err, nfsLocationLeaks...)
		})
	}
}

// TestRoutedExport_S3RefusedUploadIsNotPerformed: the lab's
// SignatureDoesNotMatch — the S3 service refused the upload before the client
// read the flattened disk to its end, so the completing call was never sent
// and nothing was published — is "not performed", as on a single host. The
// flattened copy is removed, and no log line names the bucket, key or
// endpoint.
func TestRoutedExport_S3RefusedUploadIsNotPerformed(t *testing.T) {
	for name, fail := range map[string]struct {
		code   int
		s3Code string
	}{
		"SignatureDoesNotMatch": {http.StatusForbidden, "SignatureDoesNotMatch"},
		"AccessDenied":          {http.StatusForbidden, ""},
		"InternalError (5xx)":   {http.StatusInternalServerError, ""},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newRoutedS3Export(t)
			fx.s3.FailWithCode(fail.code, fail.s3Code)
			logs := captureLog(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			_, err := NewServer(fx.p).ExportDisk(ctx, fx.request(t))
			assertNotPerformedStatus(t, err)
			assert.Equal(t, wireExportNotPerformed, status.Convert(err).Message())
			assert.Zero(t, fx.s3.CompleteRequests(), "the client never sent the completing call")
			assert.Empty(t, fx.s3.Keys(), "nothing was published")
			assert.Contains(t, fx.calls(), "local rm -rf -- "+s3ExportStageDir, "the flattened copy is removed")
			assertNoStagedLocation(t, logs.String(), err, r2S3Bucket, r2S3Key, "export.vmdk",
				strings.TrimPrefix(fx.s3.Endpoint(), "http://"), "SK")
		})
	}
}

// TestRoutedExport_S3UnknownOutcomesStayUnknown: an upload whose completing
// call may have taken effect, and a flatten whose session dropped, keep their
// unknown outcome; the log names no location.
func TestRoutedExport_S3UnknownOutcomesStayUnknown(t *testing.T) {
	t.Run("the completing call took effect and its answer was lost", func(t *testing.T) {
		fx := newRoutedS3Export(t)
		fx.s3.LoseNextComplete()
		logs := captureLog(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := NewServer(fx.p).ExportDisk(ctx, fx.request(t))
		assert.Equal(t, codes.Unknown, status.Code(err), "%v", err)
		assert.Nil(t, definiteStatusError(err), "never a definite answer: %v", err)
		assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMOperationFailedReason), "%v", err)
		assert.Equal(t, []string{r2S3Key}, fx.s3.Keys(), "the object was published although the call failed")
		assertNoStagedLocation(t, logs.String(), err, r2S3Bucket, r2S3Key, strings.TrimPrefix(fx.s3.Endpoint(), "http://"))
	})
	t.Run("the flatten's session drops", func(t *testing.T) {
		fx := newRoutedS3Export(t)
		fx.script("local", "dead-qemu-img-convert", "")
		_, err := NewServer(fx.p).ExportDisk(context.Background(), fx.request(t))
		assert.Equal(t, outcomeUnknown, hostOutcomeOf(t, err))
		assert.Empty(t, fx.s3.Keys())
	})
}

// TestRoutedExport_S3UnusableConfigurationIsInvalidArgument: an s3 request
// without its credentials is refused (InvalidArgument), as on a single host,
// before anything is flattened.
func TestRoutedExport_S3UnusableConfigurationIsInvalidArgument(t *testing.T) {
	fx := newRoutedS3Export(t)
	req := fx.request(t)
	req.Credentials = nil
	_, err := NewServer(fx.p).ExportDisk(context.Background(), req)
	assertRefusalStatus(t, err, codes.InvalidArgument)
	assertConstantText(t, err, r2S3Bucket, r2S3Key)
	assert.False(t, slicesContainsPrefix(fx.calls(), "local qemu-img convert"), "nothing is flattened: %v", fx.calls())
}

// TestExportStagedLocations: the texts redacted for an export's staged object.
func TestExportStagedLocations(t *testing.T) {
	nfs := exportStagedLocations(&providerv1.ExportDiskRequest{BackendType: migration.BackendNFS, DestinationUrl: r2NFSURL})
	assert.Contains(t, nfs, r2NFSURL)
	assert.Contains(t, nfs, r2NFSServer)
	assert.Contains(t, nfs, r2NFSExport)
	assert.Contains(t, nfs, r2NFSFile)

	s3 := exportStagedLocations(&providerv1.ExportDiskRequest{BackendType: migration.BackendS3,
		DestinationUrl:     "s3://" + r2S3Bucket + "/" + r2S3Key,
		StorageOptionsJson: `{"bucket":"` + r2S3Bucket + `","endpoint":"https://s3.example.com:9000"}`})
	for _, want := range []string{"s3://" + r2S3Bucket + "/" + r2S3Key, r2S3Key, r2S3Bucket, "s3.example.com:9000", "s3.example.com"} {
		assert.Contains(t, s3, want)
	}
	other := exportStagedLocations(&providerv1.ExportDiskRequest{BackendType: migration.BackendS3,
		DestinationUrl: "s3://another/" + r2S3Key, StorageOptionsJson: `not json`})
	assert.Contains(t, other, "another/"+r2S3Key, "a key under another bucket name is redacted whole")
}

// slicesContainsPrefix reports whether any of calls starts with prefix.
func slicesContainsPrefix(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}
