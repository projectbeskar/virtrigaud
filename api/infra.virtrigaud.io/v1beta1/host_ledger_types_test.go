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

package v1beta1

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The Host ledger mirrors carry exactly their display fields: a store id
// prefix and counters, never a full store id, a chain head or a session id.
func TestHostCRDSchema_LedgerMirrorsCarryNoFullIDs(t *testing.T) {
	status := loadCRD(t, "infra.virtrigaud.io_hosts.yaml").Spec.Versions[0].Schema.OpenAPIV3Schema.prop(t, "status")
	for name, want := range map[string][]string{
		"agentLedger":    {"attestationState", "attestedAt", "checkpointSeq", "ledgerGeneration", "seq", "storeIDPrefix", "updatedAt"},
		"agentLastStore": {"checkpointSeq", "seq", "storeIDPrefix", "updatedAt"},
	} {
		node := status.prop(t, name)
		got := keysOf(node.Properties)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("status.%s properties = %v, want exactly %v", name, got, want)
		}
		if len(node.Required) != 0 {
			t.Errorf("status.%s required = %v, want none (optional mirror)", name, node.Required)
		}
		if p := node.prop(t, "storeIDPrefix").Pattern; p != "^[0-9a-f]{8}$" {
			t.Errorf("status.%s.storeIDPrefix pattern = %q", name, p)
		}
	}
	if got := status.prop(t, "agentLedger").prop(t, "attestationState").Enum; !slices.Equal(got, []string{"Pending", "Attested"}) {
		t.Errorf("status.agentLedger.attestationState enum = %v", got)
	}
	agent := status.prop(t, "agent")
	for _, name := range []string{"agentLedger", "agentLastStore", "ledger", "lastStore"} {
		if _, ok := agent.Properties[name]; ok {
			t.Errorf("status.agent.%s exists: the mirrors must not live under status.agent", name)
		}
	}
}

// The attempt phase enum gains Accepted, keeping the earlier values; the
// receipt carries all five fields, each required once a receipt is written.
func TestVirtualMachineCRDSchema_AttemptAcceptedAndReceipt(t *testing.T) {
	status := loadCRD(t, "infra.virtrigaud.io_virtualmachines.yaml").Spec.Versions[0].Schema.OpenAPIV3Schema.prop(t, "status")
	items := status.prop(t, "operationAttempts").Items
	if items == nil {
		t.Fatal("status.operationAttempts has no items schema")
	}
	if got := items.prop(t, "phase").Enum; !slices.Equal(got, []string{"Prepared", "Sent", "Accepted", "Terminal"}) {
		t.Errorf("phase enum = %v", got)
	}
	if slices.Contains(items.Required, "receipt") || slices.Contains(items.Required, "ledgerGeneration") {
		t.Errorf("receipt and ledgerGeneration must be optional, required = %v", items.Required)
	}
	receipt := items.prop(t, "receipt")
	want := []string{"attestationID", "head", "receiptTag", "seq", "storeID"}
	got := keysOf(receipt.Properties)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("receipt properties = %v, want %v", got, want)
	}
	req := slices.Clone(receipt.Required)
	slices.Sort(req)
	if !slices.Equal(req, want) {
		t.Errorf("receipt required = %v, want %v", req, want)
	}
	for field, pattern := range map[string]string{
		"storeID": "^[0-9a-f]{32}$", "attestationID": "^[0-9a-f]{32}$", "head": "^[0-9a-f]{64}$", "receiptTag": "^[0-9a-f]{64}$",
	} {
		if p := receipt.prop(t, field).Pattern; p != pattern {
			t.Errorf("receipt.%s pattern = %q, want %q", field, p, pattern)
		}
	}
}

// The receipt tag's domain-separation prefix is versioned and NUL-terminated,
// so it neither is a prefix of, nor has as a prefix, any other context.
func TestOperationAttemptReceiptTagContext(t *testing.T) {
	if OperationAttemptReceiptTagContext != "virtrigaud.io/operation-receipt/v1\x00" {
		t.Fatalf("context = %q", OperationAttemptReceiptTagContext)
	}
	for _, other := range []string{"virtrigaud.io/operation-intent/v1\x00", "virtrigaud.io/operation-task/v2\x00",
		"virtrigaud.io/operation-intent-redacted/v1\x00"} {
		if strings.HasPrefix(OperationAttemptReceiptTagContext, other) || strings.HasPrefix(other, OperationAttemptReceiptTagContext) {
			t.Errorf("context overlaps %q", other)
		}
	}
}

// The new fields are omitted when unset, so a status every current writer
// produces serialises exactly as before.
func TestReservedLedgerFieldsOmittedWhenUnset(t *testing.T) {
	b, err := json.Marshal(OperationAttempt{Kind: OperationAttemptKindCreate, Attempt: 1, Phase: OperationAttemptPhaseSent})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"receipt"`, `"ledgerGeneration"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("unset %s serialised: %s", key, b)
		}
	}
	b, err = json.Marshal(HostStatus{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"agentLedger"`, `"agentLastStore"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("unset %s serialised: %s", key, b)
		}
	}
}
