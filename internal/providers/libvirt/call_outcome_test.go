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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin the single-host answers of SnapshotCreate, ExportDisk and
// ImportDisk (rc2 R21, call_outcome.go): a failure whose outcome the code
// proves is answered with a typed status the manager reads as definite; a
// failure it cannot prove stays an untyped codes.Unknown.

// virshFailure is a *VirshError as a host command that ended with exitCode
// returns it.
func virshFailure(exitCode int, stderr string) error {
	ve := &VirshError{Command: "virsh x", ExitCode: exitCode, Stderr: stderr}
	if exitCode >= 0 {
		// A real exit status: the process ended (exec.ExitError locally,
		// ssh.ExitError over SSH).
		ve.Cause = &exec.ExitError{}
	}
	return ve
}

// TestHostCommandEnded: only a command that exited on its own with a status of
// its own, with the request alive, is known to have stopped.
func TestHostCommandEnded(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx  context.Context
		err  error
		want bool
	}{
		"exit 1":                    {context.Background(), virshFailure(1, "boom"), true},
		"exit 127":                  {context.Background(), virshFailure(127, ""), true},
		"wrapped exit 2":            {context.Background(), fmt.Errorf("convert: %w", virshFailure(2, "")), true},
		"no exit status (dropped)":  {context.Background(), virshFailure(-1, "connection reset"), false},
		"killed by a signal (137)":  {context.Background(), virshFailure(137, ""), false},
		"ssh error (255)":           {context.Background(), virshFailure(255, ""), false},
		"zero status with an error": {context.Background(), virshFailure(0, ""), false},
		"stdout past its bound (completed, reported failed)": {context.Background(),
			&VirshError{ExitCode: sshOutputOverflowExitCode, Cause: errors.New("remote command stdout exceeded its bound")}, false},
		"not a host command":    {context.Background(), errors.New("boom"), false},
		"request context ended": {cancelled, virshFailure(1, "boom"), false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, hostCommandEnded(tc.ctx, tc.err))
		})
	}
}

// TestDefiniteStatusError: typed answers are kept (or mapped from the
// contracts error), everything else is not definite.
func TestDefiniteStatusError(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		wantCode codes.Code // codes.OK: not definite
	}{
		"invalid argument":       {status.Error(codes.InvalidArgument, "x"), codes.InvalidArgument},
		"not found":              {status.Error(codes.NotFound, "x"), codes.NotFound},
		"already exists":         {status.Error(codes.AlreadyExists, "x"), codes.AlreadyExists},
		"failed precondition":    {status.Error(codes.FailedPrecondition, "x"), codes.FailedPrecondition},
		"permission denied":      {status.Error(codes.PermissionDenied, "x"), codes.PermissionDenied},
		"unimplemented":          {status.Error(codes.Unimplemented, "x"), codes.Unimplemented},
		"wrapped typed":          {fmt.Errorf("failed: %w", status.Error(codes.NotFound, "x")), codes.NotFound},
		"plain unavailable":      {status.Error(codes.Unavailable, "x"), codes.OK},
		"internal":               {status.Error(codes.Internal, "x"), codes.OK},
		"unknown":                {status.Error(codes.Unknown, "x"), codes.OK},
		"untyped":                {errors.New("boom"), codes.OK},
		"provider not found":     {contracts.NewNotFoundError("gone", errors.New("cause /secret/path")), codes.NotFound},
		"provider invalid spec":  {contracts.NewInvalidSpecError("bad", nil), codes.InvalidArgument},
		"provider conflict":      {contracts.NewConflictError("taken", nil), codes.AlreadyExists},
		"provider not supported": {contracts.NewNotSupportedError("no"), codes.Unimplemented},
		"provider retryable":     {contracts.NewRetryableError("later", nil), codes.OK},
	} {
		t.Run(name, func(t *testing.T) {
			got := definiteStatusError(tc.err)
			if tc.wantCode == codes.OK {
				assert.Nil(t, got)
				return
			}
			require.Error(t, got)
			assert.Equal(t, tc.wantCode, status.Code(got))
			assert.NotContains(t, got.Error(), "/secret/path", "a cause is logged, never sent")
		})
	}
	// Unavailable is definite only with a reason that says nothing was done.
	for _, reason := range []string{contracts.VMOperationFailedReason, contracts.HostUnavailableReason, contracts.VMDiskCheckFailedReason} {
		err := statusWithReasons(codes.Unavailable, "x", reason).Err()
		assert.NotNil(t, definiteStatusError(err), reason)
	}
	assert.Nil(t, definiteStatusError(statusWithReasons(codes.Unavailable, "x", contracts.ImageArtifactInProgressReason).Err()))
}

// TestNothingDone: a failure of a step that changed nothing is "not performed"
// with the constant text; a typed refusal stays.
func TestNothingDone(t *testing.T) {
	assert.NoError(t, nothingDone(wireExportNotPerformed, nil))

	err := nothingDone(wireExportNotPerformed, fmt.Errorf("convert /var/lib/libvirt/images/x.qcow2: %w", virshFailure(1, "stderr secret")))
	assertNotPerformedStatus(t, err)
	assert.Equal(t, wireExportNotPerformed, status.Convert(err).Message())
	assertConstantText(t, err, "/var/lib", "stderr secret", "virsh")

	// A cause that is a transport failure is "not performed" too: the step
	// changed nothing, whatever state the connection is in.
	assertNotPerformedStatus(t, nothingDone(wireExportNotPerformed, virshFailure(-1, "connection reset")))

	assertRefusalStatus(t, nothingDone(wireExportNotPerformed, status.Error(codes.FailedPrecondition, "refused")), codes.FailedPrecondition)
	assertRefusalStatus(t, nothingDone(wireExportNotPerformed, contracts.NewNotFoundError("gone", nil)), codes.NotFound)

	// A hidden cause (hostCheckFailed) is retryable and constant: not performed.
	assertNotPerformedStatus(t, nothingDone(wireImportNotPerformed, hostCheckFailed("read", virshFailure(1, "x"))))

	// A staging area without room keeps its constant text.
	noRoom := stagedCopyRefusal(errors.New("no room"))
	got := nothingDone(wireImportNotPerformed, noRoom)
	assertNotPerformedStatus(t, got)
	assert.Contains(t, status.Convert(got).Message(), "no room")
}

// TestVirshConnectionLost: virsh's own report that libvirtd went away.
func TestVirshConnectionLost(t *testing.T) {
	assert.True(t, virshConnectionLost(virshFailure(1, "error: Disconnected from qemu:///system due to end of file")))
	assert.True(t, virshConnectionLost(fmt.Errorf("x: %w", virshFailure(1, "error: End of file while reading data: Input/output error"))))
	assert.False(t, virshConnectionLost(virshFailure(1, "error: Domain snapshot x already exists")))
	assert.False(t, virshConnectionLost(errors.New("Disconnected")))
}

// TestEndedFailure: the command that changes state is answered "failed,
// nothing published" only when it ended on its own.
func TestEndedFailure(t *testing.T) {
	ctx := context.Background()
	assertNotPerformedStatus(t, endedFailure(ctx, wireExportFailed, virshFailure(1, "qemu-img: error")))
	assertConstantText(t, endedFailure(ctx, wireExportFailed, virshFailure(1, "qemu-img: error")), "qemu-img")

	for name, err := range map[string]error{
		"connection dropped": virshFailure(-1, "reset"),
		"killed":             virshFailure(137, ""),
		"untyped":            errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) { assertUnknownOutcome(t, endedFailure(ctx, wireExportFailed, err)) })
	}
	// A typed refusal stays a refusal.
	assertRefusalStatus(t, endedFailure(ctx, wireExportFailed, status.Error(codes.FailedPrecondition, "x")), codes.FailedPrecondition)
	// A request that ended is never known.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	assertUnknownOutcome(t, endedFailure(cancelled, wireExportFailed, virshFailure(1, "x")))
}

// TestStageErrorReadsAsItsCause: a stage tag changes neither the message nor
// the chain, so the clustered callers of the shared cores are unaffected.
func TestStageErrorReadsAsItsCause(t *testing.T) {
	cause := fmt.Errorf("failed to create snapshot: %w", virshFailure(1, "x"))
	tagged := atStageOf(stageCommand, "snap", cause)
	assert.Equal(t, cause.Error(), tagged.Error())
	var ve *VirshError
	assert.True(t, errors.As(tagged, &ve))
	stage, subject, ok := stageOf(fmt.Errorf("wrapped: %w", tagged))
	assert.True(t, ok)
	assert.Equal(t, stageCommand, stage)
	assert.Equal(t, "snap", subject)
	assert.NoError(t, atStage(stageBefore, nil))
	_, _, ok = stageOf(cause)
	assert.False(t, ok)
}

// TestUploadNotPublished: an s3 upload provably did not publish when the
// service refused it with a 4xx, the request was refused locally, or the source
// failed mid-stream; any other failure may have completed it.
func TestUploadNotPublished(t *testing.T) {
	read := errors.New("ssh: stream broke")
	assert.True(t, uploadNotPublished(read, errors.New("put failed")), "the source failed: the upload cannot complete")
	assert.False(t, uploadNotPublished(io.ErrUnexpectedEOF, errors.New("put failed")),
		"the S3 client reads ErrUnexpectedEOF as a clean end of input")
	assert.False(t, uploadNotPublished(nil, errors.New("connection reset")))
	assert.True(t, uploadNotPublished(nil, &storage.StorageError{Type: storage.ErrorTypeInvalidConfig, Message: "bad url"}))
}

// TestErrTrackingReader records the first read error other than a clean end.
func TestErrTrackingReader(t *testing.T) {
	boom := errors.New("boom")
	tr := &errTrackingReader{r: io.MultiReader(strings.NewReader("ab"), failingReader{boom})}
	_, err := io.ReadAll(tr)
	require.ErrorIs(t, err, boom)
	assert.ErrorIs(t, tr.failure(), boom)

	clean := &errTrackingReader{r: strings.NewReader("ab")}
	_, err = io.ReadAll(clean)
	require.NoError(t, err)
	assert.NoError(t, clean.failure(), "io.EOF is not a failure")
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// TestSingleHostFailureClassifiers: the stage tags map to the answers the
// manager reads, for every failure class of every call.
func TestSingleHostFailureClassifiers(t *testing.T) {
	ctx := context.Background()
	ended := virshFailure(1, "stderr")
	dropped := virshFailure(-1, "reset")

	t.Run("export", func(t *testing.T) {
		assertRefusalStatus(t, singleHostExportFailure(ctx, atStage(stageInvalid, errors.New("bad config"))), codes.InvalidArgument)
		assertNotPerformedStatus(t, singleHostExportFailure(ctx, atStage(stageBefore, dropped)))
		assertNotPerformedStatus(t, singleHostExportFailure(ctx, atStage(stageCommand, ended)))
		assertUnknownOutcome(t, singleHostExportFailure(ctx, atStage(stageCommand, dropped)))
		assertUnknownOutcome(t, singleHostExportFailure(ctx, errors.New("an upload whose result was lost")))
	})
	t.Run("import", func(t *testing.T) {
		assertRefusalStatus(t, singleHostImportFailure(ctx, atStage(stageInvalid, errors.New("bad config"))), codes.InvalidArgument)
		assertNotPerformedStatus(t, singleHostImportFailure(ctx, errors.New("a step before the rename")))
		assertNotPerformedStatus(t, singleHostImportFailure(ctx, dropped))
		assertNotPerformedStatus(t, singleHostImportFailure(ctx, atStage(stagePublish, ended)))
		assertNotPerformedStatus(t, singleHostImportFailure(ctx, atStage(stageCommand, ended)))
		assertUnknownOutcome(t, singleHostImportFailure(ctx, atStage(stageCommand, dropped)))
		assertUnknownOutcome(t, singleHostImportFailure(ctx, atStage(stageCommand, virshFailure(137, ""))))
		assertUnknownOutcome(t, singleHostImportFailure(ctx, atStage(stagePublish, dropped)))
		assertUnknownOutcome(t, singleHostImportFailure(ctx, atStage(stagePublish, virshFailure(137, ""))))
		assertRefusalStatus(t, singleHostImportFailure(ctx, status.Error(codes.FailedPrecondition, "size")), codes.FailedPrecondition)
	})
}

// scdSnapshotRequest is a SnapshotCreate of scdDomain named name.
func scdSnapshotRequest(name string) *providerv1.SnapshotCreateRequest {
	return &providerv1.SnapshotCreateRequest{VmId: scdDomain, NameHint: name, Description: "d"}
}

// r21Patch rewrites the fake host tools of fx (every copy of scdFakeTool on
// PATH's first entry): old becomes new.
func r21Patch(t *testing.T, old, repl string) {
	t.Helper()
	bin := filepath.SplitList(os.Getenv("PATH"))[0]
	entries, err := os.ReadDir(bin)
	require.NoError(t, err)
	for _, e := range entries {
		p := filepath.Join(bin, e.Name())
		b, err := os.ReadFile(p) //nolint:gosec // test reads its own fixture shim
		require.NoError(t, err)
		if !strings.Contains(string(b), old) {
			continue
		}
		require.NoError(t, os.WriteFile(p, []byte(strings.Replace(string(b), old, repl, 1)), 0o755)) //nolint:gosec // test shim must be executable
	}
}

// r21ScriptedExits lets a fake host tool end with a chosen exit status
// (<host>/exit-<name> holds it), and a failing `virsh snapshot-create-as`
// leave its snapshot behind (<host>/leaves-<name>).
func r21ScriptedExits(t *testing.T) {
	t.Helper()
	r21Patch(t, `fail() { if [ -f "$d/fail-$1" ]; then echo "error: scripted failure of $1" >&2; exit 1; fi; }`,
		`LDOM="$2"; LSNAP="$3"
fail() {
  if [ -f "$d/exit-$1" ]; then
    if [ -f "$d/stderr-$1" ]; then cat "$d/stderr-$1" >&2; else echo "error: scripted exit of $1" >&2; fi
    exit "$(cat "$d/exit-$1")"
  fi
  if [ -f "$d/fail-$1" ]; then
    if [ -f "$d/leaves-$1" ]; then echo "$LSNAP" >> "$d/snaps-$LDOM"; fi
    echo "error: scripted failure of $1" >&2; exit 1
  fi
}`)
}

// TestSingleHostSnapshotCreate_Outcomes walks every failure point of the
// single-host SnapshotCreate through the gRPC Server on a fake host.
func TestSingleHostSnapshotCreate_Outcomes(t *testing.T) {
	const leak = "scripted"
	run := func(t *testing.T, setup func(*scdFixture), req *providerv1.SnapshotCreateRequest) ([]string, error) {
		t.Helper()
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		r21ScriptedExits(t)
		if setup != nil {
			setup(fx)
		}
		_, err := NewServer(fx.p).SnapshotCreate(context.Background(), req)
		return fx.calls(), err
	}
	created := func(calls []string) bool {
		for _, c := range calls {
			if strings.Contains(c, "snapshot-create-as") {
				return true
			}
		}
		return false
	}

	t.Run("a: the domain's state cannot be read - nothing done", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "fail-domstate", "") }, scdSnapshotRequest("x"))
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, leak, "virsh", "qemu:///")
		assert.False(t, created(calls))
	})
	t.Run("a: the host is unreachable while the state is read - nothing done", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "exit-domstate", "255") }, scdSnapshotRequest("x"))
		assertNotPerformedStatus(t, err)
		assert.False(t, created(calls))
	})
	t.Run("a: the domain does not exist - NotFound", func(t *testing.T) {
		calls, err := run(t, nil, &providerv1.SnapshotCreateRequest{VmId: "gone", NameHint: "x", Description: "d"})
		assertRefusalStatus(t, err, codes.NotFound)
		assert.False(t, created(calls))
	})
	t.Run("a: the dependency check cannot run - kept typed", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "fail-dumpxml", "") }, scdSnapshotRequest("x"))
		require.Error(t, err)
		assert.True(t, contracts.HasErrorInfoReason(err, contracts.VMDiskCheckFailedReason), "%v", err)
		assert.False(t, created(calls))
	})
	t.Run("a: the snapshot name is taken - AlreadyExists, nothing created", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "snaps-"+scdDomain, "taken\n") }, scdSnapshotRequest("taken"))
		assertRefusalStatus(t, err, codes.AlreadyExists)
		assert.False(t, created(calls), "the existing snapshot is never touched")
	})
	t.Run("a: the snapshots cannot be listed - nothing done", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "fail-snapshot-list", "") }, scdSnapshotRequest("x"))
		assertNotPerformedStatus(t, err)
		assert.False(t, created(calls))
	})
	t.Run("b: the create fails and the snapshot is absent - not created", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "fail-snapshot-create-as", "") }, scdSnapshotRequest("x"))
		assertNotPerformedStatus(t, err)
		assertConstantText(t, err, leak, "virsh", "--atomic")
		assert.Equal(t, wireSnapshotNotCreated, status.Convert(err).Message())
		assert.Equal(t, "single virsh snapshot-list vrgold-web --name", calls[len(calls)-1], "the absence is read again")
	})
	t.Run("b: the domain vanished during the create - NotFound", func(t *testing.T) {
		_, err := run(t, func(fx *scdFixture) {
			// The create finds no domain: it was undefined after the checks.
			r21Patch(t, `snapshot-create-as) fail snapshot-create-as; [ -f "$d/dom-$2.xml" ] || nodom "$2";`,
				`snapshot-create-as) nodom "$2";`)
		}, scdSnapshotRequest("x"))
		assertRefusalStatus(t, err, codes.NotFound)
	})
	t.Run("c: the create fails but the snapshot exists afterwards - unknown", func(t *testing.T) {
		_, err := run(t, func(fx *scdFixture) {
			fx.script("single", "fail-snapshot-create-as", "")
			fx.script("single", "leaves-snapshot-create-as", "")
		}, scdSnapshotRequest("x"))
		assertUnknownOutcome(t, err)
	})
	t.Run("c: the create is killed (a signal) - unknown, nothing read again", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) { fx.script("single", "exit-snapshot-create-as", "137") }, scdSnapshotRequest("x"))
		assertUnknownOutcome(t, err)
		assert.Equal(t, 1, strings.Count(strings.Join(calls, "\n"), "snapshot-list"), "only the name check before the create")
	})
	t.Run("c: virsh lost libvirtd during the create (it exits 1) - unknown", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) {
			fx.script("single", "exit-snapshot-create-as", "1")
			fx.script("single", "stderr-snapshot-create-as", "error: Disconnected from qemu:///system due to end of file\nerror: End of file while reading data: Input/output error\n")
		}, scdSnapshotRequest("x"))
		assertUnknownOutcome(t, err)
		assert.Equal(t, 1, strings.Count(strings.Join(calls, "\n"), "snapshot-list"), "the snapshot is not trusted to be absent")
	})
	t.Run("c: the create loses its connection - unknown", func(t *testing.T) {
		_, err := run(t, func(fx *scdFixture) { fx.script("single", "exit-snapshot-create-as", "255") }, scdSnapshotRequest("x"))
		assertUnknownOutcome(t, err)
	})
	t.Run("c: the snapshots cannot be listed after a failed create - unknown", func(t *testing.T) {
		calls, err := run(t, func(fx *scdFixture) {
			fx.script("single", "fail-snapshot-create-as", "")
			// The list works once (the name check) and fails afterwards.
			r21Patch(t, `snapshot-list)
      fail snapshot-list`, `snapshot-list)
      if [ -f "$d/listed-once" ]; then echo "error: list failed" >&2; exit 1; fi; : > "$d/listed-once"`)
		}, scdSnapshotRequest("x"))
		assertUnknownOutcome(t, err)
		assert.Equal(t, 2, strings.Count(strings.Join(calls, "\n"), "snapshot-list"),
			"the name check before the create, and the read after it that failed")
	})
	t.Run("success is unchanged", func(t *testing.T) {
		fx := newSCDFixture(t, map[string]string{scdDomain: scdDomainXML(scdDomain, scdDomainOpts{})})
		resp, err := NewServer(fx.p).SnapshotCreate(context.Background(), scdSnapshotRequest("ok"))
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.SnapshotId)
	})
}

// TestClusteredSnapshotCreate_CommandSequenceUnchanged: the clustered create
// (routed, request-token idempotent) neither reads the name first nor tags its
// answers differently.
func TestClusteredSnapshotCreate_CommandSequenceUnchanged(t *testing.T) {
	conn := &fakeSeamConn{id: "single", domainState: "shut off"}
	_, err := snapshotCreateOn(context.Background(), conn, scdDomain, scdSnapshotRequest("x"))
	require.NoError(t, err)
	for _, args := range conn.virshCalls {
		assert.NotEqual(t, "snapshot-list", args[0], "no name pre-check on the shared core")
	}
}
