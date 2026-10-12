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

import "errors"

// ErrRequestNotSent marks (in an error's chain) a provider RPC that failed
// before its request reached the provider: the gRPC client never wrote the
// request's headers (no connection could be made before the call's deadline,
// the name did not resolve), or the circuit breaker refused the call. Nothing
// was done, so a non-idempotent operation may be sent again. A transport
// failure after the request was written ("error reading from server",
// "transport is closing", a provider pod killed mid-RPC) is NOT marked: its
// outcome is unknown.
var ErrRequestNotSent = errors.New("the request was not sent to the provider")

// ErrOperationNotPerformed marks (in an error's chain) a provider's retryable
// answer that it did not perform the operation: gRPC Unavailable with the
// VMOperationFailedReason ErrorInfo (e.g. a read that failed before anything
// was copied, the same copy still running from an earlier attempt).
var ErrOperationNotPerformed = errors.New("the provider did not perform the operation")

// IsRequestNotSent reports whether err is a provider RPC error whose request
// never reached the provider (ErrRequestNotSent).
func IsRequestNotSent(err error) bool {
	return errors.Is(err, ErrRequestNotSent)
}

// IsNothingDone reports whether err proves that a provider operation did
// nothing, so that a non-idempotent operation (one carrying no request token)
// may be sent again: the request never reached the provider
// (IsRequestNotSent), or the provider answered, retryably, that it did not
// perform it — ErrOperationNotPerformed, a host that could not be reached
// (IsHostUnavailable), a disk check that could not run (IsVMDiskCheckFailed).
// Any other failure, a bare transport Unavailable or a DeadlineExceeded
// included, leaves the outcome unknown.
func IsNothingDone(err error) bool {
	if err == nil {
		return false
	}
	return IsRequestNotSent(err) ||
		(IsRetryable(err) && errors.Is(err, ErrOperationNotPerformed)) ||
		IsHostUnavailable(err) || IsVMDiskCheckFailed(err)
}
