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

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
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

// TestLedgerQuarantineReasonsMatchVerifier: every ledger loss reason the
// agent may sign to a ledger-aware request is one the manager's verifier
// accepts there, and conversely, so a host the ledger protocol quarantined
// reaches the manager's quarantine hold instead of reading as "no answer".
func TestLedgerQuarantineReasonsMatchVerifier(t *testing.T) {
	if len(journal.LedgerReasons) != len(hostidentity.LedgerQuarantineReasons) {
		t.Fatalf("agent reasons %v, verifier reasons %v", journal.LedgerReasons, hostidentity.LedgerQuarantineReasons)
	}
	for _, r := range journal.LedgerReasons {
		if !slices.Contains(hostidentity.LedgerQuarantineReasons, string(r)) {
			t.Fatalf("the verifier refuses ledger quarantine reason %q", r)
		}
	}
}

// TestAuthorityReasonsMatchVerifier: the outcome reasons the agent signs in
// an authority answer are exactly the ones the manager's verifier accepts.
func TestAuthorityReasonsMatchVerifier(t *testing.T) {
	if len(outcomeReasons) != len(hostidentity.AuthorityReasons) {
		t.Fatalf("agent reasons %v, verifier reasons %v", outcomeReasons, hostidentity.AuthorityReasons)
	}
	for _, r := range outcomeReasons {
		if !slices.Contains(hostidentity.AuthorityReasons, r) {
			t.Fatalf("the verifier refuses outcome reason %q", r)
		}
	}
}
