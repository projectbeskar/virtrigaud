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
)

func TestObjectIdentityIsZero(t *testing.T) {
	if !(ObjectIdentity{}).IsZero() {
		t.Error("empty identity must be zero")
	}
	if !(ObjectIdentity{Namespace: "ns", Name: "n"}).IsZero() {
		t.Error("namespace/name without a UID prove nothing and must count as zero")
	}
	if (ObjectIdentity{UID: "u"}).IsZero() {
		t.Error("an identity with a UID is not zero")
	}
}

func TestIsConflictAndIsInvalidSpec(t *testing.T) {
	conflict := fmt.Errorf("wrapped: %w", NewConflictError("taken", nil))
	if !IsConflict(conflict) || IsInvalidSpec(conflict) {
		t.Errorf("wrapped Conflict misclassified")
	}
	invalid := fmt.Errorf("wrapped: %w", NewInvalidSpecError("bad", nil))
	if !IsInvalidSpec(invalid) || IsConflict(invalid) {
		t.Errorf("wrapped InvalidSpec misclassified")
	}
	for _, err := range []error{nil, errors.New("plain"), NewRetryableError("busy", nil)} {
		if IsConflict(err) || IsInvalidSpec(err) {
			t.Errorf("%v must be neither Conflict nor InvalidSpec", err)
		}
	}
}

func TestIsRetryable(t *testing.T) {
	for _, err := range []error{
		NewRetryableError("unavailable", nil),
		fmt.Errorf("wrapped: %w", NewUnavailableError("host down", nil)),
		NewTimeoutError("deadline", nil),
	} {
		if !IsRetryable(err) {
			t.Errorf("%v must be retryable", err)
		}
	}
	for _, err := range []error{nil, errors.New("plain"), NewConflictError("taken", nil),
		NewInvalidSpecError("bad", nil), NewNotFoundError("gone", nil), NewNotSupportedError("no")} {
		if IsRetryable(err) {
			t.Errorf("%v must not be retryable", err)
		}
	}
}

func TestVMRefRouted(t *testing.T) {
	if (VMRef{ID: "vm"}).Routed() {
		t.Error("a ref without a host is not routed")
	}
	if (VMRef{ID: "vm", HostID: "  "}).Routed() {
		t.Error("a whitespace host is not a binding")
	}
	if !(VMRef{ID: "vm", HostID: "host-a"}).Routed() {
		t.Error("a ref with a host is routed")
	}
}
