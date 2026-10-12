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
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/projectbeskar/virtrigaud/internal/storage"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// StagingStageExport is the staging stage of the object a migration's
// ExportDisk writes and its ImportDisk reads — the only stage staged today.
const StagingStageExport = "export"

// stagingKeyRoot is the first element of every staged object's name: the S3 key
// prefix "vmmigrations/" and the NFS file-name prefix.
const stagingKeyRoot = "vmmigrations"

// Staging layouts: how a staged object's name is built (ADR-0011 D5). The
// manager records the layout of each migration when it sends the export
// (VMMigration status.storageInfo.stagingLayout), and every later step —
// the import URL and DeleteStagedDisk — uses that recorded layout, so a
// migration exported under one layout is never looked for under another.
const (
	// StagingLayoutV1 is the legacy layout: the nfs file
	// "vmmigrations-<namespace>-<name>-<stage>.qcow2". '-' appears in
	// namespaces and names, so two migrations can share a file (namespace
	// "team-prod" / name "db" and namespace "team" / name "prod-db"). An
	// empty recorded layout means V1 (a migration exported before V2).
	StagingLayoutV1 = "v1"
	// StagingLayoutV2 is the nfs file "vmmigrations_<namespace>_<name>_<stage>.qcow2":
	// '_' appears in no namespace (DNS-1123 label) and no name (DNS-1123
	// subdomain), so the name is unambiguous. New migrations use it.
	StagingLayoutV2 = "v2"
)

// normalizeStagingLayout maps a recorded or requested layout to V1 or V2; an
// unknown layout is an error.
func normalizeStagingLayout(layout string) (string, error) {
	switch layout {
	case "", StagingLayoutV1:
		return StagingLayoutV1, nil
	case StagingLayoutV2:
		return StagingLayoutV2, nil
	default:
		return "", fmt.Errorf("staging layout %q is not one of [%s %s]", layout, StagingLayoutV1, StagingLayoutV2)
	}
}

// StagedObjectKey is the single source of truth for where a migration stages
// its disk (ADR-0006, ADR-0011 D5). The manager builds the ExportDisk and
// ImportDisk URLs from it, and a provider's DeleteStagedDisk derives the object
// it removes from it, so the two can never name different objects.
//
//   - s3:  "[<prefix>/]vmmigrations/<namespace>/<name>/<stage>.vmdk", relative
//     to the bucket, in every layout. Whatever the prefix, the key ENDS with
//     the migration's own namespace and name ('/' separates them).
//   - nfs: one flat file name relative to the export (and its optional path):
//     libnfs cannot create parent directories (ADR-0006 Slice 4, finding 4).
//     Layout V2: "vmmigrations_<namespace>_<name>_<stage>.qcow2"; layout V1
//     (legacy, ambiguous): "vmmigrations-<namespace>-<name>-<stage>.qcow2".
//
// namespace must be a DNS-1123 label, name a DNS-1123 subdomain and stage one of
// the known stages; layout is V1 (or empty) or V2; s3Prefix is ignored for nfs.
// Any other backend is an error.
func StagedObjectKey(backend, layout, s3Prefix, namespace, name, stage string) (string, error) {
	if err := validateStagedDiskRef(namespace, name, stage); err != nil {
		return "", err
	}
	layout, err := normalizeStagingLayout(layout)
	if err != nil {
		return "", err
	}
	switch backend {
	case BackendS3:
		key := fmt.Sprintf("%s/%s/%s/%s.vmdk", stagingKeyRoot, namespace, name, stage)
		if prefix := strings.Trim(s3Prefix, "/"); prefix != "" {
			key = prefix + "/" + key
		}
		return key, nil
	case BackendNFS:
		if layout == StagingLayoutV2 {
			return fmt.Sprintf("%s_%s_%s_%s.qcow2", stagingKeyRoot, namespace, name, stage), nil
		}
		return fmt.Sprintf("%s-%s-%s-%s.qcow2", stagingKeyRoot, namespace, name, stage), nil
	default:
		return "", fmt.Errorf("backend %q has no staged object key", backend)
	}
}

// validateStagedDiskRef checks the identity a staged object is named from: a
// Kubernetes namespace (DNS-1123 label), a Kubernetes object name (DNS-1123
// subdomain) and a known stage. These are exactly the values a VMMigration can
// carry, and none of them can contain '/', "..", whitespace or URL delimiters.
func validateStagedDiskRef(namespace, name, stage string) error {
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return fmt.Errorf("migration namespace %q is not a DNS-1123 label", namespace)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("migration name %q is not a DNS-1123 subdomain", name)
	}
	if stage != StagingStageExport {
		return fmt.Errorf("staging stage %q is not one of [%s]", stage, StagingStageExport)
	}
	return nil
}

// StagedDisk is the staged object a DeleteStagedDisk request names, resolved by
// ResolveStagedDisk: the backend, its non-secret options and the object's key
// (and, for nfs, the libnfs URL of the object).
type StagedDisk struct {
	// Backend is BackendS3 or BackendNFS.
	Backend string
	// Options are the request's parsed storage options.
	Options StorageOptions
	// OptionsJSON is the request's storage_options_json, unchanged.
	OptionsJSON string
	// Key is the object's key: relative to the bucket for s3, relative to the
	// export's path for nfs (a flat file name).
	Key string
	// NFSURL is the object's nfs:// URL (NFSURL), with the migration's uid/gid;
	// empty for s3.
	NFSURL string
}

// ResolveStagedDisk validates a DeleteStagedDisk request and resolves the one
// object it may remove (ADR-0011 D5). The request names a migration, never a
// location: the key comes from StagedObjectKey, and for nfs the URL is built by
// NFSURL, which rejects traversal and URL smuggling in the tenant-controlled
// export and path. Every failure is a codes.InvalidArgument status that names
// no location.
func ResolveStagedDisk(req *providerv1.DeleteStagedDiskRequest) (StagedDisk, error) {
	invalid := func(format string, args ...any) error {
		return status.Errorf(codes.InvalidArgument, "DeleteStagedDisk: "+format, args...)
	}
	if req == nil {
		return StagedDisk{}, invalid("empty request")
	}
	backend := req.GetBackendType()
	if backend != BackendS3 && backend != BackendNFS {
		return StagedDisk{}, invalid("backend %q has no staged object to delete (only %q and %q)", backend, BackendNFS, BackendS3)
	}
	m := req.GetMigration()
	opts, err := ParseStorageOptions(req.GetStorageOptionsJson())
	if err != nil {
		return StagedDisk{}, invalid("storage options are not valid JSON")
	}
	if opts.Backend != "" && opts.Backend != backend {
		return StagedDisk{}, invalid("storage options are for backend %q, not %q", opts.Backend, backend)
	}
	key, err := StagedObjectKey(backend, req.GetLayout(), opts.Prefix, m.GetNamespace(), m.GetName(), req.GetStage())
	if err != nil {
		return StagedDisk{}, invalid("%v", err)
	}
	disk := StagedDisk{Backend: backend, Options: opts, OptionsJSON: req.GetStorageOptionsJson(), Key: key}
	switch backend {
	case BackendS3:
		if opts.Bucket == "" {
			return StagedDisk{}, invalid("s3 storage options name no bucket")
		}
	case BackendNFS:
		u, err := NFSURL(opts, key)
		if err != nil {
			return StagedDisk{}, invalid("nfs storage options (server, export, path, uid, gid) are not usable: %s", nfsOptionsProblem(err))
		}
		disk.NFSURL = u
	}
	return disk, nil
}

// nfsOptionsProblem names what NFSURL refused without echoing the value (an
// export or path is a location; it belongs in no status).
func nfsOptionsProblem(err error) string {
	msg := err.Error()
	for _, field := range []string{"nfs server", "nfs export", "nfs path", "nfs key", "nfs uid", "nfs gid"} {
		if strings.HasPrefix(msg, field) {
			return field + " refused"
		}
	}
	return "refused"
}

// StagedDiskError is a failed staged-object delete. GRPCStatus — what the
// manager receives — carries Code and Msg only, which name no bucket, key,
// path or endpoint. Error() appends Cause, which can name them, for the
// provider's own log. Return it to gRPC UNWRAPPED: a wrapping error would make
// gRPC use the full Error() text as the status message.
type StagedDiskError struct {
	// Code is the gRPC code returned to the manager.
	Code codes.Code
	// Msg is the location-free status message returned to the manager.
	Msg string
	// Cause is the underlying failure, for the provider's log only.
	Cause error
}

// Error returns Msg and, when set, Cause.
func (e *StagedDiskError) Error() string {
	if e.Cause == nil {
		return e.Msg
	}
	return e.Msg + ": " + e.Cause.Error()
}

// Unwrap returns Cause.
func (e *StagedDiskError) Unwrap() error { return e.Cause }

// GRPCStatus returns the status the manager receives: Code and Msg, never Cause.
func (e *StagedDiskError) GRPCStatus() *status.Status { return status.New(e.Code, e.Msg) }

// DeleteS3StagedDisk removes disk's object from its bucket with the request's
// credentials, from the provider pod (the pod is the S3 client for every
// provider, ADR-0006 D2). An object (or bucket) that does not exist is
// STAGED_DISK_DELETE_OUTCOME_NOT_FOUND. Credentials are never logged.
func DeleteS3StagedDisk(ctx context.Context, disk StagedDisk, creds map[string]string) (providerv1.StagedDiskDeleteOutcome, error) {
	cfg, err := S3StorageConfigFromRequest(disk.OptionsJSON, creds)
	if err != nil {
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_UNSPECIFIED,
			&StagedDiskError{Code: codes.InvalidArgument, Msg: "DeleteStagedDisk: s3 configuration or credentials are incomplete", Cause: err}
	}
	s3, err := storage.NewS3Storage(cfg)
	if err != nil {
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_UNSPECIFIED,
			&StagedDiskError{Code: codes.InvalidArgument, Msg: "DeleteStagedDisk: s3 client configuration is not usable", Cause: err}
	}
	defer func() { _ = s3.Close() }()
	existed, err := s3.RemoveObjectIfExists(ctx, disk.Key)
	if err != nil {
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_UNSPECIFIED,
			&StagedDiskError{Code: codes.Unavailable, Msg: "DeleteStagedDisk: the s3 delete of the staged object failed; the cause is in the provider log", Cause: err}
	}
	if !existed {
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, nil
	}
	return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, nil
}

// QemuImgRunner runs qemu-img with args where a provider runs its libnfs I/O
// (the libvirt host over SSH, or the vSphere provider pod) and returns its
// stdout and stderr. err is non-nil when qemu-img failed or could not run.
type QemuImgRunner func(ctx context.Context, args ...string) (stdout, stderr string, err error)

// nfsNoEntryMarkers are the libnfs answers to a lookup of a file that does not
// exist, as qemu-img reports them. A failed mount (MNT3ERR_*: the export or
// its path is missing or refused) is deliberately not among them.
var nfsNoEntryMarkers = []string{"NFS3ERR_NOENT", "No such file or directory"}

// nfsObjectMissing reports whether qemu-img's error output says the staged
// file does not exist (and not that the export could not be mounted).
func nfsObjectMissing(stderr string) bool {
	if strings.Contains(stderr, "MNT3ERR") || strings.Contains(strings.ToLower(stderr), "mount") {
		return false
	}
	for _, m := range nfsNoEntryMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

// qemuImgInfo is the part of `qemu-img info --output=json` read here.
type qemuImgInfo struct {
	VirtualSize int64 `json:"virtual-size"`
}

// EmptyNFSStagedDisk removes the data of the staged object at nfsURL through
// qemu-img's libnfs driver, for providers whose only NFS client is qemu-img
// (libvirt, vSphere). libnfs through qemu-img cannot unlink a file, so the
// object is truncated to zero bytes (`qemu-img create -f raw <url> 0` opens it
// with O_CREAT and truncates it): the tenant's data is gone and an empty file
// with the staging name remains (ADR-0011 D6). It is first read with
// `qemu-img info`, so a missing object is NOT_FOUND (and is never created) and
// an already-empty one is EMPTIED without a write. The URL carries the
// migration's uid/gid, so the same AUTH_SYS identity as the export and import
// is presented.
//
// Known limit: libnfs resolves a symbolic link the NFS server serves at the
// staging name, and qemu-img offers no lstat, so a link planted there (by
// whoever can write the export) is followed and its target — another file ON
// THE SAME NFS SERVER, writable by the presented uid/gid — is truncated
// instead. It never reaches a file of the host or pod: libnfs resolves links
// within the NFS namespace. Give each tenant its own export (ADR-0006 C6).
func EmptyNFSStagedDisk(ctx context.Context, run QemuImgRunner, nfsURL string) (providerv1.StagedDiskDeleteOutcome, error) {
	stdout, stderr, err := run(ctx, "info", "--output=json", "-f", "raw", nfsURL)
	if err != nil {
		if nfsObjectMissing(stderr) {
			return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, nil
		}
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_UNSPECIFIED, &StagedDiskError{
			Code:  codes.Unavailable,
			Msg:   "DeleteStagedDisk: qemu-img could not read the staged object over nfs; the cause is in the provider log",
			Cause: fmt.Errorf("qemu-img info: %w (stderr: %s)", err, strings.TrimSpace(stderr)),
		}
	}
	var info qemuImgInfo
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_UNSPECIFIED, &StagedDiskError{
			Code:  codes.Internal,
			Msg:   "DeleteStagedDisk: qemu-img info output on the staged object is not JSON",
			Cause: err,
		}
	}
	if info.VirtualSize == 0 {
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED, nil
	}
	if _, stderr, err := run(ctx, "create", "-f", "raw", nfsURL, "0"); err != nil {
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_UNSPECIFIED, &StagedDiskError{
			Code:  codes.Unavailable,
			Msg:   "DeleteStagedDisk: qemu-img could not empty the staged object over nfs; the cause is in the provider log",
			Cause: fmt.Errorf("qemu-img create: %w (stderr: %s)", err, strings.TrimSpace(stderr)),
		}
	}
	return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_EMPTIED, nil
}

// StagedDiskDeleteBackendsS3AndNFS is the staged-disk delete backend set of a
// provider that removes both s3 and nfs staging objects (ADR-0011 D7).
func StagedDiskDeleteBackendsS3AndNFS() []string { return []string{BackendNFS, BackendS3} }
