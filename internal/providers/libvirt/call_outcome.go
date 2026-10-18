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
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Definite outcomes of the single-host SnapshotCreate, ExportDisk and
// ImportDisk (rc2 R21).
//
// The manager records a durable "requested" marker before each of these calls
// and keeps it — holding the object for an operator — for any failure that
// does not prove what the call did (docs/provider-call-outcomes.md). A
// single-host libvirt provider used to answer almost every failure as an
// unclassified codes.Unknown, so an ordinary failure (a qemu-img error, a
// missing source domain, a refusal before any side effect) held the object.
// The single-host handlers now answer a failure whose outcome the code proves:
//
//   - nothing was done: the failing step only reads, validates or writes in a
//     private area that is removed, and the call's one publishing step (the S3
//     object's completion, the rename onto the landing name) never ran. The
//     answer is codes.Unavailable carrying VM_OPERATION_FAILED, which the
//     manager already treats as "the provider did not perform the operation"
//     (contracts.ErrOperationNotPerformed, contracts.IsNothingDone) and keeps
//     out of the circuit breaker. A failure that already carries a typed
//     refusal (InvalidArgument, NotFound, AlreadyExists, FailedPrecondition,
//     PermissionDenied, Unimplemented) keeps it.
//   - failed with a known end state: a host command that ended with an exit
//     status of its own (never a dropped connection, never a signal, and with
//     the request still alive) and whose end state the code then proves or
//     needs no proof of — answered the same way.
//   - unknown: everything else (a lost connection or a deadline while a host
//     command may still run, a publishing step whose result was lost) keeps its
//     untyped answer, so the manager still holds the object.
//
// Status text is constant and never carries a host path, a command line, a
// stderr or a URL: the detail is logged here, in the provider.

// callStage names how far a failed single-host call got, for the failures a
// shared core returns. The routed (clustered) callers of those cores ignore
// it: stageError reads as the error it wraps.
type callStage int

const (
	// stageBefore marks a failure before the call changed anything that
	// outlives it: a read, a check, work in a private area that the call removes
	// whatever happens (the publishing step runs only in the same sequential
	// call, so a command that outlives a cancelled call cannot publish).
	stageBefore callStage = iota + 1
	// stageCommand marks a failure of the one host command that changes state
	// outside any private area (snapshot-create-as, the in-place NFS convert).
	stageCommand
	// stagePublish marks a failure of the step that publishes the call's
	// result (the rename onto the landing name).
	stagePublish
	// stageInvalid marks the refusal of a request whose storage configuration
	// is unusable (an s3 request without its bucket or credentials). Nothing
	// was done, and retrying cannot help.
	stageInvalid
)

// stageError is err tagged with the stage it failed in. It reads as err
// (message and chain), so a caller that does not look for the stage is
// unaffected.
type stageError struct {
	stage callStage
	// subject names what the stage worked on, where the classifier needs it
	// (the snapshot's name).
	subject string
	// detail carries what the classifier compared against after the failure
	// (the disk sources before a snapshot create).
	detail string
	err    error
}

// Error returns the wrapped error's message.
func (e *stageError) Error() string { return e.err.Error() }

// Unwrap returns the wrapped error.
func (e *stageError) Unwrap() error { return e.err }

// atStage tags err with stage (nil stays nil).
func atStage(stage callStage, err error) error {
	return atStageOf(stage, "", err)
}

// atStageOf is atStage with the stage's subject.
func atStageOf(stage callStage, subject string, err error) error {
	return atStageDetail(stage, subject, "", err)
}

// atStageDetail is atStageOf with the stage's detail.
func atStageDetail(stage callStage, subject, detail string, err error) error {
	if err == nil {
		return nil
	}
	return &stageError{stage: stage, subject: subject, detail: detail, err: err}
}

// stageDetailOf returns the detail err's stage was tagged with.
func stageDetailOf(err error) string {
	var se *stageError
	if stderrors.As(err, &se) {
		return se.detail
	}
	return ""
}

// stageOf returns the stage and subject err was tagged with, and whether it
// was tagged at all.
func stageOf(err error) (callStage, string, bool) {
	var se *stageError
	if stderrors.As(err, &se) {
		return se.stage, se.subject, true
	}
	return 0, "", false
}

// Constant wire texts of the single-host long calls. Each says what the
// provider can prove and where the detail is.
const (
	wireProviderNotInitialized = "libvirt provider not initialized"
	wireExportNeedsSSH         = "disk export not performed: it requires an ssh:// libvirt transport (host-side qemu-img)"
	wireImportNeedsSSH         = "disk import not performed: it requires an ssh:// libvirt transport (host-side qemu-img)"
	wireSnapshotNotPerformed   = "snapshot create not performed: a check or command on the libvirt host failed " +
		"before the snapshot was taken (details are in the provider log)"
	wireSnapshotNotCreated = "snapshot create failed: libvirt refused the snapshot and it was not created " +
		"(details are in the provider log)"
	wireSnapshotExists = "a snapshot with this name already exists on the libvirt domain; nothing was created"
	wireDomainNotFound = "the libvirt domain was not found on the host"
	wireExportRefused  = "disk export not performed: the request was refused before anything was exported " +
		"(details are in the provider log)"
	wireExportNotPerformed = "disk export not performed: a step failed before the export was published " +
		"(details are in the provider log)"
	wirePVCExportFailed = "disk export failed: it did not complete and nothing is still writing; any partial file " +
		"on the migration volume is replaced by the next export (details are in the provider log)"
	wireExportFailed = "disk export failed: the host command ended with an error and the export did not complete " +
		"(details are in the provider log)"
	wireImportRefused = "disk import not performed: the request was refused before anything was imported " +
		"(details are in the provider log)"
	wireStagedDiskMissing  = "disk import not performed: the staged disk was not found on the provider's migration volume"
	wireImportNotPerformed = "disk import not performed: a step failed before the disk was landed " +
		"(details are in the provider log)"
	wireImportFailed = "disk import failed: the host command ended with an error and no disk was landed " +
		"(details are in the provider log)"
)

// notPerformedError is a failure whose outcome the provider proves: the call
// did not do what it was asked. It crosses gRPC as codes.Unavailable carrying
// a VM_OPERATION_FAILED ErrorInfo and the constant wire text; the cause is
// logged and reachable through Unwrap only.
type notPerformedError struct {
	wire  string
	cause error
}

// Error returns the constant wire text.
func (e *notPerformedError) Error() string { return e.wire }

// Unwrap returns the cause, for classification inside the provider only.
func (e *notPerformedError) Unwrap() error { return e.cause }

// GRPCStatus renders the failure as codes.Unavailable with a
// VM_OPERATION_FAILED ErrorInfo: the manager maps it to a retryable error
// marked contracts.ErrOperationNotPerformed (contracts.IsNothingDone) and
// never counts it toward the Provider's circuit breaker.
func (e *notPerformedError) GRPCStatus() *status.Status {
	return statusWithReasons(codes.Unavailable, e.wire, contracts.VMOperationFailedReason)
}

// notPerformed logs cause (provider side only) and returns the failure the
// manager reads as "the provider did not perform the operation", with the
// constant text wire.
func notPerformed(wire string, cause error) error {
	log.Printf("ERROR libvirt call outcome known (not performed): %s: %v", wire, cause)
	return &notPerformedError{wire: wire, cause: cause}
}

// refusal logs cause and returns a typed refusal with the constant text wire:
// the provider refused the call before doing anything.
func refusal(code codes.Code, wire string, cause error) error {
	log.Printf("ERROR libvirt call refused (%s): %s: %v", code, wire, cause)
	return status.Error(code, wire)
}

// definiteStatusError returns err, or an equivalent error with a constant
// text, when err already is a typed answer the manager reads as a definite
// refusal or as "not performed"; otherwise nil. It looks at the wire form: a
// gRPC status (InvalidArgument, NotFound, AlreadyExists, FailedPrecondition,
// PermissionDenied, Unimplemented, or Unavailable carrying one of the
// reasons that mean the call did nothing) or a contracts.ProviderError, which
// crosses gRPC as an untyped Unknown unless it is mapped here.
func definiteStatusError(err error) error {
	// The status as its own error produced it - its code, text and details -
	// never the text of the errors that wrapped it ("failed to ...: <path>:
	// <stderr>"): the wire text is the typed answer's own, constant by design.
	if st := innerStatus(err); st != nil {
		switch st.Code() {
		case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.FailedPrecondition,
			codes.PermissionDenied, codes.Unimplemented:
			return st.Err()
		case codes.Unavailable:
			for _, reason := range []string{contracts.VMOperationFailedReason, contracts.HostUnavailableReason,
				contracts.VMDiskCheckFailedReason} {
				if contracts.HasErrorInfoReason(st.Err(), reason) {
					return st.Err()
				}
			}
		case codes.ResourceExhausted:
			// A staging area without room (stagedCopyRefusal): nothing was
			// written outside the private directory, and its text is constant.
			return &notPerformedError{wire: st.Message(), cause: err}
		}
		return nil
	}
	var pe *contracts.ProviderError
	if !stderrors.As(err, &pe) {
		return nil
	}
	var code codes.Code
	switch pe.Type {
	case contracts.ErrorTypeNotFound:
		code = codes.NotFound
	case contracts.ErrorTypeInvalidSpec:
		code = codes.InvalidArgument
	case contracts.ErrorTypeConflict:
		code = codes.AlreadyExists
	case contracts.ErrorTypeNotSupported:
		code = codes.Unimplemented
	case contracts.ErrorTypeUnauthorized:
		code = codes.PermissionDenied
	default:
		return nil
	}
	if pe.Cause != nil {
		log.Printf("ERROR libvirt call refused (%s): %s: %v", code, pe.Message, pe.Cause)
	}
	return status.Error(code, pe.Message)
}

// innerStatus returns the gRPC status of the first error in err's chain that
// carries one (status.Error, an error with a GRPCStatus method), exactly as
// that error renders it, or nil.
func innerStatus(err error) *status.Status {
	var gs interface{ GRPCStatus() *status.Status }
	if !stderrors.As(err, &gs) {
		return nil
	}
	return gs.GRPCStatus()
}

// nothingDone answers a failure of a step that changed nothing that outlives
// the call (stageBefore): a typed refusal stays what it is, every other
// failure — a host command that failed, a connection that dropped, a
// deadline — is "not performed" with the constant text wire, because the
// step only reads, or writes into a private area the call removes.
func nothingDone(wire string, err error) error {
	if err == nil {
		return nil
	}
	if typed := definiteStatusError(err); typed != nil {
		return typed
	}
	return notPerformed(wire, err)
}

// hostCommandEnded reports whether err is a host command that ended on its
// own with a non-zero exit status while the request was still alive: the
// command is no longer running. A dropped connection (no exit status), a
// signal (128+n: a sudo or a shell killed while its child may live on), a
// request whose context ended, and a command reported as failed for what it
// printed (a stdout past its bound completes with status 0 and is given exit
// code 1: its Cause is not a process exit) are not.
func hostCommandEnded(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var ve *VirshError
	if !stderrors.As(err, &ve) {
		return false
	}
	var sshExit *ssh.ExitError
	var localExit *exec.ExitError
	if !stderrors.As(ve.Cause, &sshExit) && !stderrors.As(ve.Cause, &localExit) {
		return false
	}
	return ve.ExitCode > 0 && ve.ExitCode < 128
}

// virshConnectionLostMarkers are the texts (lower case) with which virsh
// reports that its connection to libvirtd ended while it ran. virsh then exits
// with a status of its own, but libvirtd — which did the work — may have gone
// on, or died half way: the outcome of the command is unknown.
var virshConnectionLostMarkers = []string{
	"keepalive timeout",
	"client socket is closed",
	"end of file while reading data",
	"disconnected from",
	"cannot recv data",
	"cannot write data",
	"failed to connect to the hypervisor",
	"connection reset",
	"broken pipe",
}

// virshConnectionLost reports whether err is a virsh command whose stderr says
// its connection to libvirtd was lost.
func virshConnectionLost(err error) bool {
	var ve *VirshError
	if !stderrors.As(err, &ve) {
		return false
	}
	stderr := strings.ToLower(ve.Stderr)
	for _, m := range virshConnectionLostMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

// snapshotRefusalMarkers are the texts (lower case) of the errors with which
// libvirt refuses a snapshot create BEFORE it executes anything: the domain is
// busy with another job, in a state the operation does not apply to, or the
// request is not supported. A failure with any other text may have happened
// after the snapshot took effect (libvirt saves the domain's metadata after it
// switched the disk sources, for one, and removes the snapshot again when that
// save fails), so it is not read as "not created".
var snapshotRefusalMarkers = []string{
	"cannot acquire state change lock",
	"requested operation is not valid",
	"unsupported configuration",
	"already exists",
}

// snapshotRefusedBeforeExecution reports whether err is a virsh command whose
// first "error:" line - libvirt's own error; the lines after it are context,
// and a later one can name another failure - is one of snapshotRefusalMarkers,
// or says the domain does not exist.
func snapshotRefusedBeforeExecution(err error) bool {
	var ve *VirshError
	if !stderrors.As(err, &ve) {
		return false
	}
	line := firstErrorLine(ve.Stderr)
	if line == "" {
		return false
	}
	for _, m := range append([]string{"failed to get domain", "domain not found"}, snapshotRefusalMarkers...) {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
}

// firstErrorLine returns the first line of stderr that starts with "error:",
// in lower case, or "".
func firstErrorLine(stderr string) string {
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.ToLower(strings.TrimSpace(l)); strings.HasPrefix(l, "error:") {
			return l
		}
	}
	return ""
}

// endedFailure answers a failure of the one host command that changes state
// (stageCommand, stagePublish) whose end state the caller needs no further
// proof of: when the command ended on its own (hostCommandEnded) the answer is
// "failed, nothing published" with the constant text wire; otherwise the
// command may still run, and err is returned unchanged — an unknown outcome.
func endedFailure(ctx context.Context, wire string, err error) error {
	if err == nil {
		return nil
	}
	if typed := definiteStatusError(err); typed != nil {
		return typed
	}
	if hostCommandEnded(ctx, err) {
		return notPerformed(wire, err)
	}
	log.Printf("WARN libvirt call outcome unknown: %s: %v", wire, err)
	return err
}

// errTrackingReader is r that remembers whether the S3 client read it to its
// end. The s3 export reads the flattened disk from the host through it. The
// client (minio-go's putObjectMultipartStreamNoLength) sends the completing
// call of an upload only after it has read the source to a clean end, so a
// source that was not read to its end proves no completing call was sent, and
// no upload error - a credentials refusal included - means the object was
// published.
type errTrackingReader struct {
	r     io.Reader
	mu    sync.Mutex
	ended bool
}

// Read reads from the wrapped reader and notes when it ends. io.ErrUnexpectedEOF
// counts as an end: the S3 client reads it as one.
func (t *errTrackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && (stderrors.Is(err, io.EOF) || stderrors.Is(err, io.ErrUnexpectedEOF)) {
		t.mu.Lock()
		t.ended = true
		t.mu.Unlock()
	}
	return n, err
}

// readToEnd reports whether the source was read to its end (or to an error
// the S3 client takes for its end).
func (t *errTrackingReader) readToEnd() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ended
}

// uploadNotPublished reports whether a failed s3 upload provably did not
// publish the object: its source was never read to the end, so the client
// never sent the completing call (errTrackingReader). Once the source was read
// to its end the completing call may have been sent and applied - and the
// client retries it when its answer is lost, with any answer, NoSuchUpload or
// a credentials refusal included - so the upload's own error proves nothing.
func uploadNotPublished(sourceReadToEnd bool) bool {
	return !sourceReadToEnd
}

// singleHostImportFailure answers a failed single-host s3 or nfs import with
// the outcome the provider can prove (call_outcome.go). Both imports stage,
// convert and check the disk in a private directory next to the landing name
// and publish it with one rename (diskWriteDir.publish), and no step after the
// rename can fail:
//
//   - stageInvalid: the request is unusable (a bad source URL, a bad storage
//     configuration) — InvalidArgument. (A local transport was already
//     refused with FailedPrecondition.)
//   - stagePublish: the rename. It either replaced the landing name or did
//     not; "failed, nothing landed" only when mv ended on its own with an exit
//     status, otherwise the untyped error (the rename may have happened).
//   - stageCommand: a qemu-img convert or dd that rewrites the whole disk in
//     the private directory. Nothing landed in any case, but a command whose
//     session dropped (or that was killed) may still run, and a retry would
//     start a second full conversion: known only when the command ended on its
//     own with an exit status (endedFailure), otherwise the untyped error.
//   - anything else is before those: reads, checks, the download and the
//     stage into the private directory — which is removed, and the rename runs
//     only in the same sequential call, which has returned. Nothing landed,
//     whatever state the connection is in. A typed refusal stays.
func singleHostImportFailure(ctx context.Context, err error) error {
	stage, _, _ := stageOf(err)
	switch stage {
	case stageInvalid:
		return refusal(codes.InvalidArgument, wireImportRefused, err)
	case stagePublish, stageCommand:
		return endedFailure(ctx, wireImportFailed, err)
	}
	return nothingDone(wireImportNotPerformed, err)
}

// stagedSourceOpenOutcome classifies a failure of openStagedImportSource for
// the manager (rc2 R21). Its typed refusals (InvalidArgument for a bad URL,
// FailedPrecondition for a refused link or special file, NotFound for a
// missing staged disk) are definite as they are. Any other failure to open the
// staged disk happened before anything was imported, so it is answered "not
// performed" instead of the generic Internal, which the manager would hold as
// an unknown outcome.
func stagedSourceOpenOutcome(err error) error {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.NotFound:
		return err
	default:
		return nothingDone(wireImportNotPerformed, err)
	}
}
