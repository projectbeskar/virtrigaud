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

// TestIsInvalidSpec pins the classification the VM controller relies on to back
// off (rather than hot-retry) a request the provider rejected on its merits.
func TestIsInvalidSpec(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                {nil, false},
		"plain error":        {errors.New("boom"), false},
		"invalid spec":       {NewInvalidSpecError("bad path", nil), true},
		"wrapped invalid":    {fmt.Errorf("create: %w", NewInvalidSpecError("bad path", nil)), true},
		"retryable":          {NewRetryableError("transient", nil), false},
		"not found":          {NewNotFoundError("gone", nil), false},
		"retryable wrapping": {NewRetryableError("outer", NewInvalidSpecError("inner", nil)), false},
	}
	for name, tc := range cases {
		if got := IsInvalidSpec(tc.err); got != tc.want {
			t.Errorf("%s: IsInvalidSpec() = %v, want %v", name, got, tc.want)
		}
	}
}

// TestIsInProgress pins the ADR-0009 D4 "still being prepared" class: typed,
// retryable, and distinct from a plain retryable error.
func TestIsInProgress(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":         {nil, false},
		"plain error": {errors.New("boom"), false},
		"in progress": {NewInProgressError("still importing", nil), true},
		"wrapped":     {fmt.Errorf("prepare: %w", NewInProgressError("still importing", nil)), true},
		"retryable":   {NewRetryableError("transient", nil), false},
	}
	for name, tc := range cases {
		if got := IsInProgress(tc.err); got != tc.want {
			t.Errorf("%s: IsInProgress() = %v, want %v", name, got, tc.want)
		}
	}
	if !IsRetryable(NewInProgressError("still importing", nil)) {
		t.Error("an InProgress error must be retryable")
	}
}
