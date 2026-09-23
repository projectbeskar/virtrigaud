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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests pin VirtualMachine.status.placement — the durable scheduler
// binding added for ADR-0007 P1 (D3). Nothing WRITES it yet (binding PR 2 does);
// this PR only lands the additive field, so the tests assert it (a) round-trips
// without loss, (b) is omitted when unset (additive: no churn on existing VMs),
// (c) deep-copies independently, and (d) is present in the generated CRD schema.

// TestPlacementStatus_JSONRoundTrip verifies a fully-populated placement binding
// serializes and deserializes without loss. Round-trip stability is asserted by
// comparing the marshaled form before and after an unmarshal (avoids time.Time
// DeepEqual pitfalls), plus explicit checks on each field.
func TestPlacementStatus_JSONRoundTrip(t *testing.T) {
	ts := metav1.Time{Time: time.Unix(1700000000, 0).UTC()}
	orig := VirtualMachineStatus{
		Phase: VirtualMachinePhaseRunning,
		Placement: &PlacementStatus{
			Host:              "host-a",
			Pool:              "pool-a",
			LastScheduledTime: &ts,
			Reason:            "spread: host-a scored highest (most free memory)",
		},
	}

	b1, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal VirtualMachineStatus: %v", err)
	}
	var got VirtualMachineStatus
	if err := json.Unmarshal(b1, &got); err != nil {
		t.Fatalf("unmarshal VirtualMachineStatus: %v", err)
	}

	if got.Placement == nil {
		t.Fatal("placement dropped on round-trip")
	}
	if got.Placement.Host != "host-a" {
		t.Errorf("placement.host = %q, want host-a", got.Placement.Host)
	}
	if got.Placement.Pool != "pool-a" {
		t.Errorf("placement.pool = %q, want pool-a", got.Placement.Pool)
	}
	if got.Placement.LastScheduledTime == nil || !got.Placement.LastScheduledTime.Equal(&ts) {
		t.Errorf("placement.lastScheduledTime = %v, want %v", got.Placement.LastScheduledTime, ts)
	}
	if !strings.Contains(got.Placement.Reason, "host-a scored highest") {
		t.Errorf("placement.reason = %q, want the decision trace preserved", got.Placement.Reason)
	}

	b2, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal VirtualMachineStatus: %v", err)
	}
	if string(b1) != string(b2) {
		t.Errorf("round-trip not stable:\n before: %s\n  after: %s", b1, b2)
	}
}

// TestPlacementStatus_OmittedWhenNil guards the additive contract: a VM with no
// placement binding (every single-host / thin-client VM, and every clustered VM
// before the operator confirms) must serialize WITHOUT a placement key, so the
// new field adds zero churn to existing VM status.
func TestPlacementStatus_OmittedWhenNil(t *testing.T) {
	b, err := json.Marshal(VirtualMachineStatus{Phase: VirtualMachinePhaseRunning})
	if err != nil {
		t.Fatalf("marshal VirtualMachineStatus: %v", err)
	}
	if strings.Contains(string(b), "placement") {
		t.Fatalf("nil placement must be omitted from status JSON; got %s", b)
	}
}

// TestPlacementStatus_DeepCopy verifies the generated deepcopy makes an
// independent copy — mutating the copy (including the LastScheduledTime pointer)
// must not touch the original — both directly and through VirtualMachineStatus.
func TestPlacementStatus_DeepCopy(t *testing.T) {
	ts := metav1.Time{Time: time.Unix(1700000000, 0).UTC()}
	orig := &PlacementStatus{
		Host:              "host-a",
		Pool:              "pool-a",
		LastScheduledTime: &ts,
		Reason:            "initial",
	}

	cp := orig.DeepCopy()
	if cp == orig {
		t.Fatal("DeepCopy returned the same pointer")
	}
	if cp.LastScheduledTime == orig.LastScheduledTime {
		t.Fatal("DeepCopy shared the LastScheduledTime pointer")
	}
	cp.Host = "host-b"
	cp.Reason = "rebound"
	cp.LastScheduledTime = &metav1.Time{Time: time.Unix(1800000000, 0).UTC()}
	if orig.Host != "host-a" || orig.Reason != "initial" || !orig.LastScheduledTime.Equal(&ts) {
		t.Fatalf("mutation of the copy leaked into the original: %+v", orig)
	}

	// Same independence through the enclosing status struct.
	vs := &VirtualMachineStatus{Placement: orig}
	vcp := vs.DeepCopy()
	if vcp.Placement == vs.Placement {
		t.Fatal("VirtualMachineStatus.DeepCopy shared the placement pointer")
	}
	vcp.Placement.Host = "host-c"
	if vs.Placement.Host != "host-a" {
		t.Fatalf("nested placement mutation leaked: %q", vs.Placement.Host)
	}
}

// TestVirtualMachineCRD_StatusPlacementSchema asserts the generated CRD carries
// status.placement with its four properties — proving the kubebuilder markers
// produced the schema, not just the Go struct. make test regenerates the CRD
// first, so a dropped field or marker fails here.
func TestVirtualMachineCRD_StatusPlacementSchema(t *testing.T) {
	crd := loadCRD(t, "infra.virtrigaud.io_virtualmachines.yaml")
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	placement := root.prop(t, "status").prop(t, "placement")
	if placement.Type != "object" {
		t.Fatalf("status.placement type = %q, want object", placement.Type)
	}
	for _, field := range []string{"host", "pool", "lastScheduledTime", "reason"} {
		if got := placement.prop(t, field); got.Type == "" {
			t.Errorf("status.placement.%s has no type in the generated schema", field)
		}
	}
}
