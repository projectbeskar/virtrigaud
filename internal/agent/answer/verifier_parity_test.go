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

package answer

import (
	"slices"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
)

// TestQuarantineReasonsMatchVerifier: every quarantine reason the agent may
// sign is one the manager's verifier accepts, and conversely, so a
// quarantined host is never read as "no answer" (or a made-up reason
// accepted).
func TestQuarantineReasonsMatchVerifier(t *testing.T) {
	if len(lostReasons) != len(hostidentity.QuarantineReasons) {
		t.Fatalf("agent reasons %v, verifier reasons %v", lostReasons, hostidentity.QuarantineReasons)
	}
	for _, r := range lostReasons {
		if !slices.Contains(hostidentity.QuarantineReasons, string(r)) {
			t.Fatalf("the verifier refuses quarantine reason %q", r)
		}
	}
}
