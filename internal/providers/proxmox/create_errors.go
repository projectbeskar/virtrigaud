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

package proxmox

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Create and Clone error classes.
//
// The VM controller decides from a failed Create whether a VM may exist on
// the hypervisor (and must be found or deleted later) or certainly does not.
// Every Create and Clone failure is therefore one of:
//
//   - InvalidSpec (codes.InvalidArgument): the request itself is wrong (an
//     unusable owner identity, a template id that is not a number, a
//     template that does not exist, or a request PVE rejected with HTTP 400).
//     Nothing was done.
//   - NotFound (Clone only): the source VM does not exist (a definite 404).
//     Nothing was done.
//   - PermissionDenied: PVE refused the request with HTTP 401/403. Nothing
//     was done.
//   - AlreadyExists (ownershipConflictError): the provider cannot tell which
//     VM this VirtualMachine owns. Nothing was done.
//   - codes.Unavailable with ErrorInfo CREATE_NOT_PERFORMED: the provider
//     made nothing; retrying is safe and the caller may treat the Create as
//     refused. Two scopes:
//   - provider-wide (createNotPerformed: CREATE_NOT_PERFORMED alone, so it
//     counts toward the manager's circuit breaker): client not configured,
//     node discovery, /cluster/nextid, the cluster list of the owner lookup,
//     the migration SSH data plane not configured;
//   - one VM (vmCreateNotPerformed: CREATE_NOT_PERFORMED plus
//     VM_OPERATION_FAILED, which keeps it out of the breaker): reading the
//     template, the clone source or a same-named VM's owner stamp; PVE
//     refused the create/clone request without acting; or a step after the
//     VM was made failed and that VM was removed again (rollbackCreatedVM).
//   - codes.Unavailable WITHOUT CREATE_NOT_PERFORMED (outcomeUnknown): the
//     outcome is unknown. A VM may exist (a transport error or 5xx on the
//     create/clone request, a task that could not be followed, a failed step
//     whose VM could not be removed).

// rollbackTimeout bounds the removal of a VM a failed Create or Clone made. It
// runs detached from the request's context, which may be what expired.
const rollbackTimeout = 2 * time.Minute

// maxWireDetailLen bounds the PVE answer text a wire message quotes (the body
// of an HTTP 400, which names the rejected parameters).
const maxWireDetailLen = 512

// createNotPerformed is the answer of a Create or Clone that made nothing
// because of a provider-wide failure: codes.Unavailable with a
// google.rpc.ErrorInfo{Reason: CREATE_NOT_PERFORMED}. The wire message is msg
// only: cause (which can carry the PVE URL or address) goes to the provider
// log.
func (p *Provider) createNotPerformed(msg string, cause error) error {
	p.logCause(msg, cause)
	return statusWithReasons(codes.Unavailable, msg, contracts.CreateNotPerformedReason)
}

// vmCreateNotPerformed is the answer of a Create or Clone that made nothing
// because of a failure scoped to one VM: codes.Unavailable with the
// CREATE_NOT_PERFORMED and VM_OPERATION_FAILED ErrorInfos. Like
// createNotPerformed, cause is logged, never sent.
func (p *Provider) vmCreateNotPerformed(msg string, cause error) error {
	p.logCause(msg, cause)
	return statusWithReasons(codes.Unavailable, msg,
		contracts.CreateNotPerformedReason, contracts.VMOperationFailedReason)
}

// logCause logs the cause of a Create, Clone or Delete answer, which the wire
// message leaves out.
func (p *Provider) logCause(answer string, cause error) {
	if cause != nil {
		p.logger.Warn("Proxmox call failed", "answer", answer, "error", cause)
	}
}

// wireDetail is s trimmed and cut to maxWireDetailLen, for a wire message.
func wireDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxWireDetailLen {
		cut := maxWireDetailLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut-- // never split a multi-byte character
		}
		s = s[:cut] + "..."
	}
	return s
}

// statusWithReasons is a gRPC status error of code with msg and one
// google.rpc.ErrorInfo (in contracts.ErrorInfoDomain) per reason.
func statusWithReasons(code codes.Code, msg string, reasons ...string) error {
	st := status.New(code, msg)
	withInfo := st
	for _, r := range reasons {
		next, err := withInfo.WithDetails(&errdetails.ErrorInfo{Reason: r, Domain: contracts.ErrorInfoDomain})
		if err != nil {
			// Unreachable in practice (ErrorInfo always marshals).
			return st.Err()
		}
		withInfo = next
	}
	return withInfo.Err()
}

// outcomeUnknown is the answer of a Create or Clone after which a VM may
// exist: codes.Unavailable without CREATE_NOT_PERFORMED. cause is logged,
// never sent.
func (p *Provider) outcomeUnknown(msg string, cause error) error {
	msg += "; a VM may have been created (outcome unknown)"
	p.logCause(msg, cause)
	return &errors.ProviderError{Code: codes.Unavailable, Message: msg, Retryable: true}
}

// classifySubmitError classifies the failure err of the request that makes VM
// vmid (POST create or clone; op names it; stamp is the owner stamp
// it carries, nil without an owner).
//
// Only an answer that certainly comes from PVE (a PVE JSON body) and that PVE
// gives before acting means nothing was done: HTTP 400 (parameter
// verification, InvalidSpec) and 401/403 (PermissionDenied). An HTTP 5xx
// saying the VMID "already exists" means the VMID was taken since
// /cluster/nextid — unless the VM at that VMID carries this request's stamp,
// or is a temporary config still being cloned/created, or cannot be read:
// then the outcome is unknown. Any other answer, or a transport failure,
// leaves the outcome unknown: PVE may have started the task.
func (p *Provider) classifySubmitError(ctx context.Context, op string, vmid int, stamp *ownerStamp, err error) error {
	var apiErr *pveapi.APIError
	if !stderrors.As(err, &apiErr) || !isPVEJSONBody(apiErr.Body) {
		return p.outcomeUnknown(fmt.Sprintf("the PVE %s request failed", op), err)
	}
	switch {
	case apiErr.StatusCode == http.StatusBadRequest:
		p.logCause("PVE rejected the "+op+" request", err)
		return errors.NewInvalidSpec("PVE rejected the %s request: %s", op, wireDetail(apiErrorDetail(apiErr)))
	case apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden:
		p.logCause("PVE refused the "+op+" request", err)
		return errors.NewPermissionDenied(op)
	case apiErr.StatusCode >= http.StatusInternalServerError && strings.Contains(apiErr.Text(), "already exists"):
		if p.vmidTakenByAnother(ctx, vmid, stamp) {
			return p.vmCreateNotPerformed(fmt.Sprintf("PVE refused the %s request because the VMID is taken; "+
				"nothing was created (will retry with a new VMID)", op), err)
		}
	}
	return p.outcomeUnknown(fmt.Sprintf("the PVE %s request failed", op), err)
}

// vmidTakenByAnother reports whether the VM at vmid is certainly not one this
// request made: its config, read on whichever node of the cluster has it
// (vmConfigAnywhere), does not carry stamp's owner and is not a temporary
// config still being cloned or created. A VMID that turns out free, or a
// config that cannot be read, proves nothing (false).
func (p *Provider) vmidTakenByAnother(ctx context.Context, vmid int, stamp *ownerStamp) bool {
	cfg, err := p.vmConfigAnywhere(ctx, vmid)
	if err != nil || beingMade(cfg) {
		return false
	}
	read := readOwnerStamp(configString(cfg, pveConfigKeyDescription))
	if stamp != nil && read.state == "" && read.stamp != nil && read.stamp.owner.UID == stamp.owner.UID {
		return false
	}
	return true
}

// isPVEJSONBody reports whether an HTTP answer body is PVE's JSON (an object
// with a "data" member), as opposed to a proxy's or load balancer's page.
func isPVEJSONBody(body string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &obj) != nil {
		return false
	}
	_, ok := obj["data"]
	return ok
}

// apiErrorDetail is the error text of a PVE answer for a wire message: the
// reason phrase of its status line (where PVE puts its message) and the
// "message" and per-parameter "errors" of its JSON body; the raw body when
// neither says anything.
func apiErrorDetail(apiErr *pveapi.APIError) string {
	parts := []string{}
	if r := strings.TrimSpace(apiErr.Reason); r != "" {
		parts = append(parts, r)
	}
	if b := pveBodyErrorText(apiErr.Body); b != "" {
		parts = append(parts, b)
	}
	if len(parts) == 0 {
		return apiErr.Body
	}
	return strings.Join(parts, "; ")
}

// pveBodyErrorText is the error text of a PVE JSON body: its "message" and
// its per-parameter "errors" ("" when it has neither, e.g. {"data":null});
// a body that is not JSON is returned as it is.
func pveBodyErrorText(body string) string {
	var obj struct {
		Message string            `json:"message"`
		Errors  map[string]string `json:"errors"`
	}
	if json.Unmarshal([]byte(body), &obj) != nil {
		return body
	}
	parts := []string{}
	if m := strings.TrimSpace(obj.Message); m != "" {
		parts = append(parts, m)
	}
	keys := make([]string, 0, len(obj.Errors))
	for k := range obj.Errors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+": "+strings.TrimSpace(obj.Errors[k]))
	}
	return strings.Join(parts, "; ")
}

// rollbackCreatedVM removes the VM vmid on node that a failed Create or Clone
// made, and returns nil only when that VM is gone. It removes it only when it
// is verifiably that VM: its owner stamp records stamp's owner (stamp is what
// this call last wrote: a VM stamped "ready" is removed only when this call
// wrote "ready"), or — for a request without an owner (stamp nil) — it
// carries no stamp and has the requested name; it is not locked (no PVE task is still working on it), and
// it is stopped. taskFinished says whether the task that made the VM is known
// to have ended: only then does "no such VM" mean that nothing remains.
func (p *Provider) rollbackCreatedVM(ctx context.Context, node string, vmid int, name string, stamp *ownerStamp, taskFinished bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		return p.confirmNothingRemains(ctx, vmid, taskFinished)
	}
	if err != nil {
		return fmt.Errorf("read VM %d: %w", vmid, err)
	}
	if lock := configString(cfg, pveConfigKeyLock); lock != "" {
		return fmt.Errorf("VM %d is locked (%s)", vmid, lock)
	}
	read := readOwnerStamp(configString(cfg, pveConfigKeyDescription))
	if stamp != nil {
		if read.state != "" || read.stamp == nil || read.stamp.owner != stamp.owner {
			return fmt.Errorf("VM %d does not carry this request's owner stamp", vmid)
		}
		// A VM stamped ready was completed — by this call only when stamp
		// (what this call last wrote) is ready too. Never remove a VM another
		// call completed.
		if read.stamp.phase == ownerStampPhaseReady && stamp.phase != ownerStampPhaseReady {
			return fmt.Errorf("VM %d is stamped ready by another Create or Clone", vmid)
		}
	} else if read.state != "" || read.stamp != nil || configString(cfg, pveConfigKeyName) != name {
		return fmt.Errorf("VM %d is not the unstamped VM %q this request made", vmid, name)
	}
	vm, err := p.client.GetVM(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("read status of VM %d: %w", vmid, err)
	}
	if vm.Status != pveStatusStopped {
		return fmt.Errorf("VM %d is %s, not stopped", vmid, vm.Status)
	}
	task, err := p.client.DeleteVM(ctx, node, vmid, true)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		return p.confirmNothingRemains(ctx, vmid, true)
	}
	if err != nil {
		return fmt.Errorf("destroy VM %d: %w", vmid, err)
	}
	if err := p.client.WaitForTask(ctx, node, task); err != nil {
		return fmt.Errorf("destroy VM %d: %w", vmid, err)
	}
	if err := p.confirmNothingRemains(ctx, vmid, true); err != nil {
		return err
	}
	// The VM is gone: its seed build lock file goes too (best effort; seed.go).
	p.removeSeedLock(ctx, vmid)
	return nil
}

// sshExitConnectionFailed is the exit status ssh reports when the connection
// failed or broke (it cannot tell whether the remote command ran).
const sshExitConnectionFailed = 255

// sshMayHaveRun reports whether a failed SSH command may have run, or still
// run, on the node: ctx ended, ssh exited 255 (connection failed or broke),
// or the process was killed. A remote command that ran and exited with its
// own status (1-254), or an ssh that never started, did not leave anything
// running.
func sshMayHaveRun(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	var exitErr *exec.ExitError
	if !stderrors.As(err, &exitErr) {
		return false
	}
	code := exitErr.ExitCode()
	return code == sshExitConnectionFailed || code < 0
}

// importStageReady checks the prerequisites of rebuilding an imported-disk VM:
// the SSH data plane is configured and the staged disk is on the node (an
// import stage path that passes importStageNodeCheck). Its error says which
// one is missing, in words fit for a wire message (never the path).
func (p *Provider) importStageReady(ctx context.Context, stagePath string) error {
	if p.ssh == nil {
		return fmt.Errorf("the migration SSH data plane is not configured")
	}
	if !isImportStagePath(stagePath) {
		return fmt.Errorf("the staged disk path is not an import stage")
	}
	if _, _, err := p.ssh.runSSH(ctx, importStageNodeCheck(stagePath)); err != nil {
		p.logCause("checking the staged disk failed", err)
		if sshMayHaveRun(ctx, err) {
			return fmt.Errorf("the staged disk could not be checked")
		}
		return fmt.Errorf("the staged disk is no longer on the node")
	}
	return nil
}

// pgrepNoMatch is pgrep's exit status when no process matches.
const pgrepNoMatch = 1

// importdiskRunningPattern is the pgrep -f pattern of a `qm importdisk <vmid>`
// process: "qm" as its own word (after "/", a space or the start), then
// " importdisk <vmid>" and a space or the end. The "[q]" keeps the pattern
// from matching the remote shell that runs pgrep itself.
func importdiskRunningPattern(vmid int) string {
	return fmt.Sprintf("(^|[/ ])[q]m importdisk %d( |$)", vmid)
}

// importdiskIdle checks over SSH that no `qm importdisk <vmid>`, and no seed
// build of the VM (its lock held), is still
// running on the node (an earlier, interrupted Create's). Any doubt — the
// check cannot run, or such a process exists — is an error, in words fit for
// a wire message.
func (p *Provider) importdiskIdle(ctx context.Context, vmid int) error {
	// A seed build of the VM (seed.go) holds its lock for its whole run, also
	// before and after its `qm importdisk`: a held lock answers "running".
	lock := proxmoxShellQuote(seedLockPath(vmid))
	_, _, err := p.ssh.runSSH(ctx, "if [ -e "+lock+" ] && ! flock -n "+lock+" true; then exit 0; fi; "+
		"pgrep -f -- "+proxmoxShellQuote(importdiskRunningPattern(vmid)))
	if err == nil {
		return fmt.Errorf("a qm importdisk for it is still running on the node")
	}
	var exitErr *exec.ExitError
	if stderrors.As(err, &exitErr) && exitErr.ExitCode() == pgrepNoMatch && ctx.Err() == nil {
		return nil
	}
	p.logCause("checking for a running qm importdisk failed", err)
	return fmt.Errorf("whether a qm importdisk for it is still running could not be checked")
}

// confirmNothingRemains returns nil only when the VMID a failed Create or
// Clone used is free in the whole cluster (vmGone) and the task that made
// the VM has ended (taskFinished): a VM "missing" from one node may have
// moved to another, and one whose task still runs may yet appear.
func (p *Provider) confirmNothingRemains(ctx context.Context, vmid int, taskFinished bool) error {
	if !taskFinished {
		return fmt.Errorf("VM %d is not there, but the task that makes it may still be running", vmid)
	}
	gone, err := p.vmGone(ctx, vmid)
	if err != nil {
		return fmt.Errorf("confirm VM %d is gone: %w", vmid, err)
	}
	if !gone {
		return fmt.Errorf("VMID %d is still in use in the cluster", vmid)
	}
	return nil
}

// failAfterSubmit answers a Create or Clone whose step what failed (cause)
// after PVE accepted the request that makes VM vmid. It removes that VM
// (rollbackCreatedVM) and answers vmCreateNotPerformed when it is gone, and
// outcomeUnknown otherwise.
func (p *Provider) failAfterSubmit(ctx context.Context, node string, vmid int, name string, stamp *ownerStamp,
	taskFinished bool, what string, cause error) error {
	if rbErr := p.rollbackCreatedVM(ctx, node, vmid, name, stamp, taskFinished); rbErr != nil {
		p.logger.Error("A failed create left a VM that could not be removed", "vmid", vmid, "node", node,
			"name", name, "step", what, "error", cause, "rollback_error", rbErr)
		return p.outcomeUnknown(fmt.Sprintf("%s; VM %d could not be removed", what, vmid), cause)
	}
	p.logger.Warn("A create failed; the VM it made was removed", "vmid", vmid, "node", node, "name", name, "step", what, "error", cause)
	return p.vmCreateNotPerformed(fmt.Sprintf("%s; the VM it made (%d) was removed, nothing remains (will retry)", what, vmid), cause)
}

// pveConfigLockTimeout matches the end of a failed task's error (pveapi
// WaitForTask: "... with exit code: <exitstatus>") when the task died because
// it could not take a VM's config lock in time: PVE::Tools::lock_file_full's
// "can't lock file '/var/lock/qemu-server/lock-<vmid>.conf' - got timeout".
// Submatch 1 is the VMID.
var pveConfigLockTimeout = regexp.MustCompile(`exit code: can't lock file '/var/lock/qemu-server/lock-(\d+)\.conf' - got timeout$`)

// cloneTaskLockedOut reports whether err, the failed wait for the clone task
// that makes VM target from VM source, says that the task ended because it
// could not lock source's or target's config. qemu-server's clone worker
// takes those locks first (the source's, then the target's, each with a
// one-second timeout) and runs the clone only once it holds both; every
// failure after that is reported as "clone failed: ...", and the error of a
// lock it could not take ends the worker before anything is made. Such a task
// made nothing. The target's lock is held by another task when another
// Create or Clone is making a VM with the same VMID (two requests that got
// the same /cluster/nextid answer), so the VM at that VMID, if any, is never
// this request's and must not be touched.
func cloneTaskLockedOut(err error, source, target int) bool {
	if !stderrors.Is(err, pveapi.ErrTaskFailed) {
		return false
	}
	m := pveConfigLockTimeout.FindStringSubmatch(err.Error())
	if m == nil {
		return false
	}
	id, convErr := strconv.Atoi(m[1])
	return convErr == nil && (id == source || id == target)
}

// cloneLockedOut is the answer of a Create or Clone whose clone task could
// not lock its source's or target's config (cloneTaskLockedOut): nothing was
// cloned, and a retry gets a new VMID.
func (p *Provider) cloneLockedOut(target int, cause error) error {
	return p.vmCreateNotPerformed(fmt.Sprintf("the PVE clone task for VM %d could not lock the configuration of the clone "+
		"or of its source (another task holds it, e.g. a concurrent create given the same VMID); nothing was cloned "+
		"(will retry with a new VMID)", target), cause)
}

// failAfterTask answers a failed wait for the task that makes VM vmid: a
// task that finished with a failure (pveapi.ErrTaskFailed) is rolled back like
// any later step; a task whose end is unknown (a timeout, a failed status
// query) is rolled back only when the VM is verifiably there and unlocked.
func (p *Provider) failAfterTask(ctx context.Context, node string, vmid int, name string, stamp *ownerStamp, what string, cause error) error {
	return p.failAfterSubmit(ctx, node, vmid, name, stamp, stderrors.Is(cause, pveapi.ErrTaskFailed), what, cause)
}
