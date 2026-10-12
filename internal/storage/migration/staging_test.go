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

package migration

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage/s3fake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestStagedObjectKey_Layout pins both staging layouts: V1 is the layout the
// manager has always used for its ExportDisk / ImportDisk URLs (a migration
// exported under it must be imported and deleted under it), V2 the
// unambiguous nfs name new migrations use. The s3 key is the same in both.
func TestStagedObjectKey_Layout(t *testing.T) {
	for name, tc := range map[string]struct {
		backend, layout, prefix, want string
	}{
		"s3 without prefix":         {BackendS3, StagingLayoutV2, "", "vmmigrations/team-a/web/export.vmdk"},
		"s3 with prefix":            {BackendS3, StagingLayoutV2, "tenant/a", "tenant/a/vmmigrations/team-a/web/export.vmdk"},
		"s3 prefix slashes":         {BackendS3, StagingLayoutV1, "/tenant/", "tenant/vmmigrations/team-a/web/export.vmdk"},
		"s3 legacy is the same key": {BackendS3, "", "", "vmmigrations/team-a/web/export.vmdk"},
		"nfs v2":                    {BackendNFS, StagingLayoutV2, "", "vmmigrations_team-a_web_export.qcow2"},
		"nfs v1 (legacy)":           {BackendNFS, StagingLayoutV1, "", "vmmigrations-team-a-web-export.qcow2"},
		"nfs empty layout is v1":    {BackendNFS, "", "", "vmmigrations-team-a-web-export.qcow2"},
		"nfs ignores s3 prefixes":   {BackendNFS, StagingLayoutV2, "tenant", "vmmigrations_team-a_web_export.qcow2"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := StagedObjectKey(tc.backend, tc.layout, tc.prefix, "team-a", "web", StagingStageExport)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	_, err := StagedObjectKey(BackendNFS, "v3", "", "team-a", "web", StagingStageExport)
	assert.Error(t, err, "an unknown layout is refused")
}

// TestStagedObjectKey_V2SeparatesCollidingIdentities (review D3): the legacy
// nfs name of namespace "team-prod" / migration "db" is the same file as that
// of namespace "team" / migration "prod-db"; under V2 they differ, so one
// tenant's export, import or delete can never reach the other's object.
func TestStagedObjectKey_V2SeparatesCollidingIdentities(t *testing.T) {
	a1, err := StagedObjectKey(BackendNFS, StagingLayoutV1, "", "team-prod", "db", StagingStageExport)
	require.NoError(t, err)
	b1, err := StagedObjectKey(BackendNFS, StagingLayoutV1, "", "team", "prod-db", StagingStageExport)
	require.NoError(t, err)
	assert.Equal(t, a1, b1, "the legacy layout collides (why V2 exists)")

	a2, err := StagedObjectKey(BackendNFS, StagingLayoutV2, "", "team-prod", "db", StagingStageExport)
	require.NoError(t, err)
	b2, err := StagedObjectKey(BackendNFS, StagingLayoutV2, "", "team", "prod-db", StagingStageExport)
	require.NoError(t, err)
	assert.NotEqual(t, a2, b2)
	// Dotted names (a DNS-1123 subdomain) do not collide either.
	c2, err := StagedObjectKey(BackendNFS, StagingLayoutV2, "", "team", "prod.db", StagingStageExport)
	require.NoError(t, err)
	assert.NotEqual(t, a2, c2)
	assert.NotEqual(t, b2, c2)
}

// TestResolveStagedDisk_LayoutRoundTrips: the delete names the same file the
// export URL named, in each layout, and refuses an unknown layout.
func TestResolveStagedDisk_LayoutRoundTrips(t *testing.T) {
	opts := StorageOptions{Backend: BackendNFS, Server: "nfs.example", Export: "/exports"}
	for layout, want := range map[string]string{
		"":              "vmmigrations-team-a-web-export.qcow2",
		StagingLayoutV1: "vmmigrations-team-a-web-export.qcow2",
		StagingLayoutV2: "vmmigrations_team-a_web_export.qcow2",
	} {
		req := deleteReq(t, BackendNFS, opts)
		req.Layout = layout
		d, err := ResolveStagedDisk(req)
		require.NoError(t, err)
		assert.Equal(t, want, d.Key)
		key, err := StagedObjectKey(BackendNFS, layout, "", "team-a", "web", StagingStageExport)
		require.NoError(t, err)
		u, err := NFSURL(opts, key)
		require.NoError(t, err)
		assert.Equal(t, u, d.NFSURL, "the delete names the export URL's file")
	}
	req := deleteReq(t, BackendNFS, opts)
	req.Layout = "../v2"
	_, err := ResolveStagedDisk(req)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestStagedObjectKey_RefusesWhatIsNotAMigration proves the identity a staged
// object is named from cannot carry a path: only a real namespace, object name
// and stage are accepted.
func TestStagedObjectKey_RefusesWhatIsNotAMigration(t *testing.T) {
	for name, tc := range map[string]struct {
		backend, ns, name, stage string
	}{
		"empty namespace":         {BackendS3, "", "web", StagingStageExport},
		"namespace with a slash":  {BackendS3, "team-a/../x", "web", StagingStageExport},
		"namespace with a dot":    {BackendNFS, "team.a", "web", StagingStageExport},
		"name with a slash":       {BackendS3, "team-a", "web/../../other", StagingStageExport},
		"name with dot-dot":       {BackendNFS, "team-a", "..", StagingStageExport},
		"name with whitespace":    {BackendS3, "team-a", "web x", StagingStageExport},
		"unknown stage":           {BackendS3, "team-a", "web", "import"},
		"stage with a path":       {BackendS3, "team-a", "web", "../export"},
		"pvc has no staged key":   {BackendPVC, "team-a", "web", StagingStageExport},
		"unknown backend":         {"ftp", "team-a", "web", StagingStageExport},
		"upper-case name refused": {BackendS3, "team-a", "Web", StagingStageExport},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := StagedObjectKey(tc.backend, StagingLayoutV2, "", tc.ns, tc.name, tc.stage)
			assert.Error(t, err)
		})
	}
}

// deleteReq is a DeleteStagedDiskRequest for team-a/web's export.
func deleteReq(t *testing.T, backend string, opts StorageOptions) *providerv1.DeleteStagedDiskRequest {
	t.Helper()
	js, err := MarshalStorageOptions(opts)
	require.NoError(t, err)
	return &providerv1.DeleteStagedDiskRequest{
		Migration:          &providerv1.ObjectIdentity{Uid: "uid-1", Namespace: "team-a", Name: "web"},
		Stage:              StagingStageExport,
		BackendType:        backend,
		StorageOptionsJson: js,
	}
}

func TestResolveStagedDisk(t *testing.T) {
	uid, gid := int64(1000), int64(1000)
	t.Run("s3 resolves the migration's own key", func(t *testing.T) {
		d, err := ResolveStagedDisk(deleteReq(t, BackendS3, StorageOptions{Backend: BackendS3, Bucket: "b", Prefix: "p"}))
		require.NoError(t, err)
		assert.Equal(t, "p/vmmigrations/team-a/web/export.vmdk", d.Key)
		assert.Empty(t, d.NFSURL)
	})
	t.Run("nfs resolves the migration's own file under the export path", func(t *testing.T) {
		d, err := ResolveStagedDisk(deleteReq(t, BackendNFS, StorageOptions{
			Backend: BackendNFS, Server: "nfs.example", Export: "/exports/mig", Path: "sub", UID: &uid, GID: &gid,
		}))
		require.NoError(t, err)
		assert.Equal(t, "vmmigrations-team-a-web-export.qcow2", d.Key)
		assert.Equal(t, "nfs://nfs.example/exports/mig/sub/vmmigrations-team-a-web-export.qcow2?gid=1000&uid=1000", d.NFSURL)
	})

	for name, req := range map[string]*providerv1.DeleteStagedDiskRequest{
		"nil request": nil,
		"pvc backend": deleteReq(t, BackendPVC, StorageOptions{}),
		"no backend":  deleteReq(t, "", StorageOptions{Bucket: "b"}),
		"no bucket":   deleteReq(t, BackendS3, StorageOptions{Backend: BackendS3}),
		"options mismatch": deleteReq(t, BackendS3, StorageOptions{Backend: BackendNFS, Bucket: "b",
			Server: "nfs.example", Export: "/e"}),
		"nfs export traversal": deleteReq(t, BackendNFS, StorageOptions{Backend: BackendNFS, Server: "nfs.example",
			Export: "/exports/../etc"}),
		"nfs path traversal": deleteReq(t, BackendNFS, StorageOptions{Backend: BackendNFS, Server: "nfs.example",
			Export: "/exports", Path: "../../etc"}),
		"nfs path query smuggling": deleteReq(t, BackendNFS, StorageOptions{Backend: BackendNFS, Server: "nfs.example",
			Export: "/exports", Path: "x?uid=0"}),
		"nfs relative export": deleteReq(t, BackendNFS, StorageOptions{Backend: BackendNFS, Server: "nfs.example",
			Export: "exports"}),
		"missing migration": {Stage: StagingStageExport, BackendType: BackendS3, StorageOptionsJson: `{"bucket":"b"}`},
		"bad stage": {Migration: &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"},
			Stage: "../../x", BackendType: BackendS3, StorageOptionsJson: `{"bucket":"b"}`},
		"bad json": {Migration: &providerv1.ObjectIdentity{Namespace: "team-a", Name: "web"},
			Stage: StagingStageExport, BackendType: BackendS3, StorageOptionsJson: `{`},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			_, err := ResolveStagedDisk(req)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
			// No location reaches the manager in the status message.
			msg := status.Convert(err).Message()
			assert.NotContains(t, msg, "/exports")
			assert.NotContains(t, msg, "nfs.example")
		})
	}
}

const fakeBucket = "staging"

// s3Disk resolves team-a/web's s3 staged object on the fake server.
func s3Disk(t *testing.T, srv *s3fake.Server) StagedDisk {
	t.Helper()
	d, err := ResolveStagedDisk(deleteReq(t, BackendS3, StorageOptions{
		Backend: BackendS3, Bucket: fakeBucket, Endpoint: srv.Endpoint(), UsePathStyle: true, Prefix: "p",
	}))
	require.NoError(t, err)
	return d
}

var fakeCreds = map[string]string{CredKeyAccessKeyID: "AK", CredKeySecretAccessKey: "SK"}

func TestDeleteS3StagedDisk(t *testing.T) {
	ctx := context.Background()
	const own = "p/vmmigrations/team-a/web/export.vmdk"
	const other = "p/vmmigrations/team-b/web/export.vmdk"

	t.Run("deletes only the migration's own object", func(t *testing.T) {
		srv := s3fake.New(fakeBucket)
		defer srv.Close()
		srv.Put(own, []byte("disk"))
		srv.Put(other, []byte("other tenant"))

		out, err := DeleteS3StagedDisk(ctx, s3Disk(t, srv), fakeCreds)
		require.NoError(t, err)
		assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, out)
		assert.False(t, srv.Has(own))
		assert.True(t, srv.Has(other), "another namespace's staged object is never touched")
		for _, r := range srv.Requests() {
			assert.Equal(t, own, r.Key, "%s touched another key", r.Method)
		}

		// Idempotent: the second delete finds nothing and removes nothing.
		out, err = DeleteS3StagedDisk(ctx, s3Disk(t, srv), fakeCreds)
		require.NoError(t, err)
		assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, out)
		assert.True(t, srv.Has(other))
	})

	t.Run("a missing bucket is not found", func(t *testing.T) {
		srv := s3fake.New("another-bucket")
		defer srv.Close()
		out, err := DeleteS3StagedDisk(ctx, s3Disk(t, srv), fakeCreds)
		require.NoError(t, err)
		assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, out)
	})

	t.Run("a backend failure is retryable and names no location", func(t *testing.T) {
		srv := s3fake.New(fakeBucket)
		defer srv.Close()
		srv.Put(own, []byte("disk"))
		srv.FailWith(http.StatusInternalServerError)
		_, err := DeleteS3StagedDisk(ctx, s3Disk(t, srv), fakeCreds)
		require.Error(t, err)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		msg := status.Convert(err).Message()
		assert.NotContains(t, msg, fakeBucket)
		assert.NotContains(t, msg, "vmmigrations")
		assert.NotContains(t, msg, srv.Endpoint())
		srv.FailWith(0)
		assert.True(t, srv.Has(own))
	})

	t.Run("missing credentials are invalid and never dial", func(t *testing.T) {
		srv := s3fake.New(fakeBucket)
		defer srv.Close()
		_, err := DeleteS3StagedDisk(ctx, s3Disk(t, srv), map[string]string{CredKeyAccessKeyID: "AK"})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Empty(t, srv.Requests())
		assert.NotContains(t, err.Error(), "SK")
	})
}

// scriptedQemuImg is a QemuImgRunner answering `info` and `create` from a
// script and recording every call.
type scriptedQemuImg struct {
	infoOut, infoErrOut string
	infoErr, createErr  error
	calls               [][]string
}

func (q *scriptedQemuImg) run(_ context.Context, args ...string) (string, string, error) {
	q.calls = append(q.calls, args)
	switch args[0] {
	case "info":
		return q.infoOut, q.infoErrOut, q.infoErr
	case "create":
		return "", "permission denied", q.createErr
	}
	return "", "", errors.New("unexpected qemu-img command")
}

func TestEmptyNFSStagedDisk(t *testing.T) {
	ctx := context.Background()
	const url = "nfs://nfs.example/exports/vmmigrations-team-a-web-export.qcow2?gid=1000&uid=1000"

	t.Run("truncates an object holding data", func(t *testing.T) {
		q := &scriptedQemuImg{infoOut: `{"virtual-size": 10737418240, "format": "raw"}`}
		out, err := EmptyNFSStagedDisk(ctx, q.run, url)
		require.NoError(t, err)
		assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED, out)
		require.Len(t, q.calls, 2)
		assert.Equal(t, []string{"info", "--output=json", "-f", "raw", url}, q.calls[0])
		assert.Equal(t, []string{"create", "-f", "raw", url, "0"}, q.calls[1])
	})
	t.Run("an empty object is not written again", func(t *testing.T) {
		q := &scriptedQemuImg{infoOut: `{"virtual-size": 0}`}
		out, err := EmptyNFSStagedDisk(ctx, q.run, url)
		require.NoError(t, err)
		assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED, out)
		assert.Len(t, q.calls, 1)
	})
	t.Run("a missing object is not found and never created", func(t *testing.T) {
		q := &scriptedQemuImg{infoErr: errors.New("exit status 1"),
			infoErrOut: `qemu-img: Could not open '` + url + `': Failed to open file : NFS: Lookup of /vmmigrations-team-a-web-export.qcow2 failed with NFS3ERR_NOENT(-2)`}
		out, err := EmptyNFSStagedDisk(ctx, q.run, url)
		require.NoError(t, err)
		assert.Equal(t, providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, out)
		assert.Len(t, q.calls, 1, "a missing object must never be created by a truncation")
	})
	t.Run("a mount failure is retryable, not not-found", func(t *testing.T) {
		q := &scriptedQemuImg{infoErr: errors.New("exit status 1"),
			infoErrOut: `qemu-img: Could not open '` + url + `': Failed to mount nfs share: mount/mnt call failed with "MNT3ERR_NOENT(2)"`}
		_, err := EmptyNFSStagedDisk(ctx, q.run, url)
		require.Error(t, err)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		assert.NotContains(t, status.Convert(err).Message(), "nfs.example", "no location in the status")
		assert.True(t, strings.Contains(err.Error(), "MNT3ERR_NOENT"), "the cause is kept for the provider log")
		assert.Len(t, q.calls, 1)
	})
	t.Run("a failed truncation is retryable", func(t *testing.T) {
		q := &scriptedQemuImg{infoOut: `{"virtual-size": 1}`, createErr: errors.New("exit status 1")}
		_, err := EmptyNFSStagedDisk(ctx, q.run, url)
		require.Error(t, err)
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
}

// TestStagedDiskError_StatusCarriesNoCause proves the status the manager gets
// has only the location-free message, while Error() keeps the cause for the
// provider's log.
func TestStagedDiskError_StatusCarriesNoCause(t *testing.T) {
	err := &StagedDiskError{Code: codes.Unavailable, Msg: "DeleteStagedDisk: failed", Cause: errors.New("s3://bucket/key 500")}
	assert.Equal(t, "DeleteStagedDisk: failed", status.Convert(err).Message())
	assert.Contains(t, err.Error(), "s3://bucket/key")
}

// TestCheckStagedImageHeader (review High): only a self-contained image of the
// expected format passes.
func TestCheckStagedImageHeader(t *testing.T) {
	const self = "/var/tmp/stage.qcow2"
	assert.NoError(t, CheckStagedImageHeader(`{"format":"qcow2","virtual-size":1}`, "qcow2", self))
	assert.NoError(t, CheckStagedImageHeader(`{"format":"vmdk","format-specific":{"type":"vmdk","data":{"extents":[{"filename":"/tmp/x.vmdk"}]}}}`,
		"vmdk", "/tmp/x.vmdk"), "a VMDK whose only extent is itself")
	for name, info := range map[string]string{
		"backing file":       `{"format":"qcow2","backing-filename":"/etc/shadow"}`,
		"full backing json:": `{"format":"qcow2","full-backing-filename":"json:{}"}`,
		"data file":          `{"format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"/dev/sda"}}}`,
		"wrong format":       `{"format":"raw"}`,
		"foreign extent":     `{"format":"vmdk","format-specific":{"type":"vmdk","data":{"extents":[{"filename":"/root/.ssh/id_rsa"}]}}}`,
		"not json":           `qemu-img: error`,
	} {
		want := "qcow2"
		if strings.Contains(name, "extent") {
			want = "vmdk"
		}
		err := CheckStagedImageHeader(info, want, self)
		assert.ErrorIs(t, err, ErrStagedImageRejected, name)
		if err != nil {
			assert.NotContains(t, err.Error(), "/etc/shadow", "no path in the rejection")
			assert.NotContains(t, err.Error(), "id_rsa")
		}
	}
}

// TestStagedCopyLimitAndCheck (re-review Medium 1): the copy bound is the
// provider default (or its environment override), lowered by a request bound;
// an object above it is too large, one that does not fit StagedCopySpaceFactor
// times in the free space is refused for space.
func TestStagedCopyLimitAndCheck(t *testing.T) {
	t.Setenv(EnvStagedImportMaxGiB, "")
	limit, err := StagedCopyLimit(0)
	require.NoError(t, err)
	assert.Equal(t, int64(DefaultStagedImportMaxGiB)<<30, limit)
	t.Setenv(EnvStagedImportMaxGiB, "8")
	limit, err = StagedCopyLimit(0)
	require.NoError(t, err)
	assert.Equal(t, int64(8)<<30, limit)
	limit, err = StagedCopyLimit(1 << 30)
	require.NoError(t, err)
	assert.Equal(t, int64(1)<<30+int64(1)<<24+stagedImageMetadataSlack, limit,
		"a smaller request bound wins, with room for the image's metadata")
	limit, err = StagedCopyLimit(8<<30 - 1)
	require.NoError(t, err)
	assert.Equal(t, int64(8)<<30, limit, "the room never lifts the provider bound")
	limit, err = StagedCopyLimit(1 << 40)
	require.NoError(t, err)
	assert.Equal(t, int64(8)<<30, limit, "a request cannot raise the provider bound")
	for _, bad := range []string{"0", "-1", "lots", "99999999"} {
		t.Setenv(EnvStagedImportMaxGiB, bad)
		_, err = StagedCopyLimit(0)
		assert.Error(t, err, bad)
	}

	assert.NoError(t, CheckStagedCopy(100, 1000, 200))
	assert.ErrorIs(t, CheckStagedCopy(1001, 1000, 1<<40), ErrStagedObjectTooLarge)
	assert.ErrorIs(t, CheckStagedCopy(-1, 1000, 1<<40), ErrStagedObjectTooLarge)
	assert.ErrorIs(t, CheckStagedCopy(100, 1000, 199), ErrStagingSpaceExhausted)
	assert.Equal(t, int64(1), StagedCopyDDCount(1))
	assert.Equal(t, int64(2), StagedCopyDDCount(1<<20), "limit+1 bytes need a second MiB block")
}
