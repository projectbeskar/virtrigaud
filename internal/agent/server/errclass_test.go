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

package server

import (
	"errors"
	"fmt"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
)

// TestErrorClassNamesNoText: a log line names a class, never the error's text,
// which can carry a certificate subject, a component name or a path.
func TestErrorClassNamesNoText(t *testing.T) {
	secret := errors.New("sign /var/lib/secret/host.key CN=SECRET-TOKEN")
	for err, want := range map[error]string{
		fmt.Errorf("%w: %w", answer.ErrSign, secret):             "sign",
		fmt.Errorf("%w: %w", answer.ErrInvalidInventory, secret): "invalid_inventory",
		fmt.Errorf("%w: %w", answer.ErrNoAnswer, secret):         "no_answer",
		fmt.Errorf("%w: %w: %w", answer.ErrSign, signer.ErrSigner, fmt.Errorf("%w: %w: connect: %v", signer.ErrSigner, signer.ErrUnavailable, secret)): "signer_unavailable",
		fmt.Errorf("%w: %w", answer.ErrSign, fmt.Errorf("%w: %w: x", signer.ErrSigner, signer.ErrRefused)):                                             "signer_refused",
		fmt.Errorf("%w: %w", answer.ErrSign, fmt.Errorf("%w: %w: x", signer.ErrSigner, signer.ErrRateLimited)):                                         "signer_rate_limited",
		secret: "other",
	} {
		if got := errorClass(err); got != want {
			t.Errorf("errorClass(%v) = %q, want %q", err, got, want)
		}
	}
}
