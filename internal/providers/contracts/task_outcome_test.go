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

package contracts

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

func TestTaskOutcome_NotPerformedAndTyped(t *testing.T) {
	notPerformed := map[TaskOutcome]bool{TaskOutcomeRetired: true, TaskOutcomeCancelled: true}
	for _, o := range []TaskOutcome{TaskOutcomeUnspecified, TaskOutcomeRunning, TaskOutcomeSucceeded,
		TaskOutcomeFailedClean, TaskOutcomeFailedDirty, TaskOutcomeUnknown, TaskOutcomeRetired, TaskOutcomeCancelled} {
		if got := o.NotPerformed(); got != notPerformed[o] {
			t.Errorf("%q.NotPerformed() = %v", o, got)
		}
		if got := o.Typed(); got != (o != TaskOutcomeUnspecified) {
			t.Errorf("%q.Typed() = %v", o, got)
		}
	}
}

func TestIsNotPerformed(t *testing.T) {
	err := NewNotPerformedError("task not performed: RETIRED{deadline}", nil)
	if !IsNotPerformed(err) || !IsNotPerformed(fmt.Errorf("wrapped: %w", err)) {
		t.Fatal("a NotPerformed error, wrapped or not, is IsNotPerformed")
	}
	if IsRetryable(err) || IsNotFound(err) || IsNothingDone(err) {
		t.Fatal("NotPerformed is neither retryable, a NotFound nor an RPC-level nothing-done")
	}
	for _, other := range []error{nil, errors.New("plain"), NewNotFoundError("x", nil), NewRetryableError("x", nil)} {
		if IsNotPerformed(other) {
			t.Fatalf("IsNotPerformed(%v) = true", other)
		}
	}
}

func TestIsUnknownToAgent(t *testing.T) {
	marked := NewNotFoundError("taskStatus: no record", fmt.Errorf("%w: %w", ErrUnknownToAgent, errors.New("rpc")))
	if !IsUnknownToAgent(marked) {
		t.Fatal("a NotFound marked ErrUnknownToAgent is IsUnknownToAgent")
	}
	if IsUnknownToAgent(NewNotFoundError("plain", nil)) {
		t.Fatal("a plain NotFound is not UNKNOWN_TO_AGENT")
	}
	if IsUnknownToAgent(NewRetryableError("x", ErrUnknownToAgent)) {
		t.Fatal("only a NotFound can be UNKNOWN_TO_AGENT")
	}
	if IsNotPerformed(marked) {
		t.Fatal("UNKNOWN_TO_AGENT is never a not-performed")
	}
}

func TestPartAReasons_NotPerformedClassification(t *testing.T) {
	definite := map[string]bool{
		ProviderMismatchReason:             true,
		DigestMismatchReason:               true,
		DeadlineExceededNotPerformedReason: true,
		GrantUnauthenticatedReason:         true,
		GrantMismatchReason:                true,
		NonceConsumedReason:                true,
		RequestConsumedReason:              true,
	}
	seen := map[string]bool{}
	for _, r := range PartAReasons() {
		if seen[r] {
			t.Fatalf("duplicate reason %s", r)
		}
		seen[r] = true
		if got := IsDefiniteNotPerformedReason(r); got != definite[r] {
			t.Errorf("IsDefiniteNotPerformedReason(%s) = %v", r, got)
		}
	}
	if len(seen) != 23 {
		t.Fatalf("PartAReasons() has %d reasons, want 23 (8 Part A + 15 operation-grant)", len(seen))
	}
	if got := PartAReasons()[7]; got != DeadlineExceededNotPerformedReason {
		t.Fatalf("PartAReasons()[7] = %s: the Part A reasons must keep their order, new ones are appended", got)
	}
	for _, r := range []string{
		UnknownToAgentReason, CancelRefusedReason, HostUnavailableReason, "",
		IdentityRequiredReason, FencingRequiredReason, StaleEpochReason, StaleIssuedReason,
		EpochNotActivatedReason, JumpTooLargeReason, QuarantinedReason, OpIDConflictReason,
		BadNonceReason, VMBindingStaleReason, ClockUncorroboratedReason,
	} {
		if IsDefiniteNotPerformedReason(r) {
			t.Errorf("%q must not be a definite not-performed", r)
		}
	}
}

func TestHasPartAReason(t *testing.T) {
	with := func(reason, domain string) error {
		st, err := status.New(codes.Unavailable, "x").WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain})
		if err != nil {
			t.Fatal(err)
		}
		return st.Err()
	}
	for _, r := range PartAReasons() {
		if !HasPartAReason(with(r, ErrorInfoDomain)) {
			t.Errorf("HasPartAReason(%s) = false", r)
		}
		if HasPartAReason(with(r, "example.com")) {
			t.Errorf("%s in another domain must not count", r)
		}
	}
	if HasPartAReason(with(HostUnavailableReason, ErrorInfoDomain)) || HasPartAReason(errors.New("plain")) || HasPartAReason(nil) {
		t.Fatal("only Part A reasons count")
	}
}

// TestPartAReasons_MatchSDK keeps the SDK's copy of the reasons (provider
// authors use it) equal to the manager's.
func TestPartAReasons_MatchSDK(t *testing.T) {
	if sdkerrors.ErrorInfoDomain != ErrorInfoDomain {
		t.Fatalf("SDK domain %q != %q", sdkerrors.ErrorInfoDomain, ErrorInfoDomain)
	}
	pairs := map[string]string{
		sdkerrors.ReasonUnknownToAgent:               UnknownToAgentReason,
		sdkerrors.ReasonVMBusy:                       VMBusyReason,
		sdkerrors.ReasonProviderMismatch:             ProviderMismatchReason,
		sdkerrors.ReasonUnattested:                   UnattestedReason,
		sdkerrors.ReasonAgentLost:                    AgentLostReason,
		sdkerrors.ReasonDigestMismatch:               DigestMismatchReason,
		sdkerrors.ReasonCancelRefused:                CancelRefusedReason,
		sdkerrors.ReasonDeadlineExceededNotPerformed: DeadlineExceededNotPerformedReason,
		sdkerrors.ReasonGrantUnauthenticated:         GrantUnauthenticatedReason,
		sdkerrors.ReasonGrantMismatch:                GrantMismatchReason,
		sdkerrors.ReasonNonceConsumed:                NonceConsumedReason,
		sdkerrors.ReasonRequestConsumed:              RequestConsumedReason,
		sdkerrors.ReasonIdentityRequired:             IdentityRequiredReason,
		sdkerrors.ReasonFencingRequired:              FencingRequiredReason,
		sdkerrors.ReasonStaleEpoch:                   StaleEpochReason,
		sdkerrors.ReasonStaleIssued:                  StaleIssuedReason,
		sdkerrors.ReasonEpochNotActivated:            EpochNotActivatedReason,
		sdkerrors.ReasonJumpTooLarge:                 JumpTooLargeReason,
		sdkerrors.ReasonQuarantined:                  QuarantinedReason,
		sdkerrors.ReasonOpIDConflict:                 OpIDConflictReason,
		sdkerrors.ReasonBadNonce:                     BadNonceReason,
		sdkerrors.ReasonVMBindingStale:               VMBindingStaleReason,
		sdkerrors.ReasonClockUncorroborated:          ClockUncorroboratedReason,
	}
	if len(pairs) != len(PartAReasons()) {
		t.Fatalf("the SDK has %d Part A reasons, the manager %d", len(pairs), len(PartAReasons()))
	}
	for sdk, mgr := range pairs {
		if sdk != mgr {
			t.Errorf("SDK reason %q != manager reason %q", sdk, mgr)
		}
	}
}
