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

package resilience

import (
	"errors"
	"fmt"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestIsRetryable_SeesWrappedProviderErrors: a ProviderError wrapped by
// fmt.Errorf or marked with contracts.MarkCreateNotPerformed keeps its
// retryability.
func TestIsRetryable_SeesWrappedProviderErrors(t *testing.T) {
	retryable := contracts.NewRetryableError("create: nothing was created", nil)
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                       {nil, false},
		"plain error":               {errors.New("boom"), false},
		"ProviderError":             {retryable, true},
		"wrapped":                   {fmt.Errorf("op: %w", retryable), true},
		"marked create-not-perform": {contracts.MarkCreateNotPerformed(retryable), true},
		"non-retryable, marked":     {contracts.MarkCreateNotPerformed(contracts.NewInvalidSpecError("bad", nil)), false},
	}
	for name, tc := range cases {
		if got := IsRetryable(tc.err); got != tc.want {
			t.Errorf("%s: IsRetryable = %v, want %v", name, got, tc.want)
		}
	}
}
