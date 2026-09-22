/*
Copyright 2025.

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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	yaml "gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Shared CRD-schema helpers (used by host + hostpool schema tests) --------
//
// These read the generated CRD under config/crd/bases so the defaulting/enum
// assertions verify the actual schema controller-gen produced from the
// kubebuilder markers, not a re-declaration of them. make test regenerates the
// CRDs first, so a removed or changed marker fails the schema tests.

// crdDoc captures just the parts of a generated CRD the schema tests assert on.
type crdDoc struct {
	Spec struct {
		Names struct {
			Kind       string   `yaml:"kind"`
			Plural     string   `yaml:"plural"`
			ShortNames []string `yaml:"shortNames"`
		} `yaml:"names"`
		Versions []struct {
			Name   string `yaml:"name"`
			Schema struct {
				OpenAPIV3Schema apiSchemaNode `yaml:"openAPIV3Schema"`
			} `yaml:"schema"`
		} `yaml:"versions"`
	} `yaml:"spec"`
}

// apiSchemaNode is a minimal OpenAPI v3 schema node: enough to walk properties
// and read defaults/enums.
type apiSchemaNode struct {
	Type       string                   `yaml:"type"`
	Default    interface{}              `yaml:"default"`
	Enum       []string                 `yaml:"enum"`
	Properties map[string]apiSchemaNode `yaml:"properties"`
}

// prop returns the named child property, failing the test if it is absent.
func (n apiSchemaNode) prop(t *testing.T, name string) apiSchemaNode {
	t.Helper()
	p, ok := n.Properties[name]
	if !ok {
		t.Fatalf("schema property %q not found (have: %v)", name, keysOf(n.Properties))
	}
	return p
}

func keysOf(m map[string]apiSchemaNode) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// loadCRD reads and parses a generated CRD from config/crd/bases. The path is
// resolved from this test file's own location so it is independent of the test's
// working directory.
func loadCRD(t *testing.T, filename string) crdDoc {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate CRD bases")
	}
	// thisFile: <repo>/api/infra.virtrigaud.io/v1beta1/host_types_test.go
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	path := filepath.Join(repoRoot, "config", "crd", "bases", filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated CRD %s: %v", path, err)
	}
	var crd crdDoc
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal CRD %s: %v", filename, err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatalf("CRD %s declares no versions", filename)
	}
	return crd
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// --- Host defaulted-bool footgun -------------------------------------------

// TestHostSpec_SchedulableFalseSurvivesMarshal guards the defaulted-bool
// footgun (ADR-0006/PR#235): Schedulable defaults to true, so if its JSON tag
// carried omitempty an explicit false would be dropped on marshal (e.g. a
// controller Update), the apiserver would re-apply the true default, and a
// cordoned host would silently un-cordon. The field must always serialize.
func TestHostSpec_SchedulableFalseSurvivesMarshal(t *testing.T) {
	b, err := json.Marshal(HostSpec{
		ProviderRef: ObjectRef{Name: "p"},
		PoolRef:     LocalObjectReference{Name: "pool"},
		Endpoint:    "qemu+ssh://u@h/system",
		Schedulable: false,
	})
	if err != nil {
		t.Fatalf("marshal HostSpec: %v", err)
	}
	if !strings.Contains(string(b), `"schedulable":false`) {
		t.Fatalf("HostSpec.Schedulable=false was dropped on marshal (omitempty footgun); got %s", b)
	}

	var got HostSpec
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Schedulable {
		t.Fatalf("HostSpec.Schedulable round-tripped to true; want false")
	}
}

// --- Host enum vocabulary ---------------------------------------------------

// TestHostHealthConstants pins the wire values of the HostHealth enum so a rename
// of a constant cannot silently diverge from the CRD enum / provider contract.
func TestHostHealthConstants(t *testing.T) {
	cases := map[HostHealth]string{
		HostHealthReady:    "Ready",
		HostHealthNotReady: "NotReady",
		HostHealthUnknown:  "Unknown",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("HostHealth constant = %q, want %q", got, want)
		}
	}
}

// --- Host JSON round-trip ---------------------------------------------------

// TestHost_JSONRoundTrip verifies a fully-populated Host serializes and
// deserializes without loss. Round-trip stability is asserted by comparing the
// marshaled form before and after an unmarshal (avoids time.Time DeepEqual
// pitfalls), plus explicit checks on the enum-valued and pointer fields.
func TestHost_JSONRoundTrip(t *testing.T) {
	ts := metav1.Time{Time: time.Unix(1700000000, 0).UTC()}
	cpu := int32(64)
	mem := int64(262144)
	stor := int64(2 << 40)
	orig := Host{
		TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "Host"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host-a",
			Namespace: "cluster-a",
		},
		Spec: HostSpec{
			ProviderRef:         ObjectRef{Name: "libvirt-cluster"},
			PoolRef:             LocalObjectReference{Name: "pool-a"},
			Endpoint:            "qemu+ssh://virt@host-a/system",
			CredentialSecretRef: &ObjectRef{Name: "host-a-creds", Namespace: "cluster-a"},
			Labels: map[string]string{
				"storage.virtrigaud.io/pool-nfs01": "true",
				"net.virtrigaud.io/br-vlan100":     "true",
			},
			Schedulable: true,
		},
		Status: HostStatus{
			Health:                  HostHealthReady,
			AllocatableCPU:          &cpu,
			AllocatableMemoryMiB:    &mem,
			AllocatableStorageBytes: &stor,
			CPUModel:                "Cascadelake-Server",
			CPUFeatures:             []string{"avx512f", "vmx"},
			MachineTypes:            []string{"pc-q35-8.2"},
			EmulatorVersion:         "8.2.0",
			BoundVMs:                3,
			LastHeartbeatTime:       &ts,
			ObservedGeneration:      7,
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "HostReachable",
				Message:            "libvirtd reachable",
				LastTransitionTime: ts,
			}},
		},
	}

	b1, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal Host: %v", err)
	}
	var got Host
	if err := json.Unmarshal(b1, &got); err != nil {
		t.Fatalf("unmarshal Host: %v", err)
	}
	b2, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal Host: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("Host round-trip not stable:\n first: %s\nsecond: %s", b1, b2)
	}

	if got.Status.Health != HostHealthReady {
		t.Errorf("Health = %q, want %q", got.Status.Health, HostHealthReady)
	}
	if got.Spec.CredentialSecretRef == nil || got.Spec.CredentialSecretRef.Name != "host-a-creds" {
		t.Errorf("CredentialSecretRef not preserved: %+v", got.Spec.CredentialSecretRef)
	}
	if got.Status.AllocatableCPU == nil || *got.Status.AllocatableCPU != 64 {
		t.Errorf("AllocatableCPU not preserved: %+v", got.Status.AllocatableCPU)
	}
	if got.Spec.Labels["net.virtrigaud.io/br-vlan100"] != "true" {
		t.Errorf("Labels not preserved: %+v", got.Spec.Labels)
	}
}

// --- Host DeepCopy ----------------------------------------------------------

// TestHost_DeepCopy verifies generated deepcopy independence and object safety:
// mutating the source must not touch the copy, DeepCopyObject returns a distinct
// non-nil runtime.Object, and a nil receiver is handled.
func TestHost_DeepCopy(t *testing.T) {
	cpu := int32(32)
	orig := &Host{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
		Spec: HostSpec{
			ProviderRef:         ObjectRef{Name: "p"},
			PoolRef:             LocalObjectReference{Name: "pool"},
			Endpoint:            "qemu+ssh://u@h/system",
			CredentialSecretRef: &ObjectRef{Name: "creds"},
			Labels:              map[string]string{"k": "v"},
			Schedulable:         true,
		},
		Status: HostStatus{
			Health:         HostHealthReady,
			AllocatableCPU: &cpu,
			CPUFeatures:    []string{"vmx"},
			Conditions:     []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}

	cp := orig.DeepCopy()

	// Mutate the original's deep fields.
	orig.Spec.Labels["k"] = "mutated"
	orig.Spec.CredentialSecretRef.Name = "mutated"
	*orig.Status.AllocatableCPU = 1
	orig.Status.CPUFeatures[0] = "mutated"
	orig.Status.Conditions[0].Reason = "mutated"

	if cp.Spec.Labels["k"] != "v" {
		t.Errorf("Labels map not deep-copied: %q", cp.Spec.Labels["k"])
	}
	if cp.Spec.CredentialSecretRef.Name != "creds" {
		t.Errorf("CredentialSecretRef not deep-copied: %q", cp.Spec.CredentialSecretRef.Name)
	}
	if *cp.Status.AllocatableCPU != 32 {
		t.Errorf("AllocatableCPU pointer not deep-copied: %d", *cp.Status.AllocatableCPU)
	}
	if cp.Status.CPUFeatures[0] != "vmx" {
		t.Errorf("CPUFeatures slice not deep-copied: %q", cp.Status.CPUFeatures[0])
	}
	if cp.Status.Conditions[0].Reason != "" {
		t.Errorf("Conditions slice not deep-copied: %q", cp.Status.Conditions[0].Reason)
	}

	if obj := orig.DeepCopyObject(); obj == nil {
		t.Error("Host.DeepCopyObject() returned nil")
	}
	if got := (*Host)(nil).DeepCopy(); got != nil {
		t.Error("(*Host)(nil).DeepCopy() should return nil")
	}

	list := &HostList{Items: []Host{*orig}}
	if obj := list.DeepCopyObject(); obj == nil {
		t.Error("HostList.DeepCopyObject() returned nil")
	}
}

// --- Host CRD schema (defaulting + enum) ------------------------------------

// TestHostCRDSchemaDefaults asserts the generated Host CRD encodes the
// schedulable=true default, the HostHealth enum, and the hvh shortName.
func TestHostCRDSchemaDefaults(t *testing.T) {
	crd := loadCRD(t, "infra.virtrigaud.io_hosts.yaml")

	if crd.Spec.Names.Kind != "Host" {
		t.Errorf("kind = %q, want Host", crd.Spec.Names.Kind)
	}
	if !containsStr(crd.Spec.Names.ShortNames, "hvh") {
		t.Errorf("shortNames = %v, want to contain hvh", crd.Spec.Names.ShortNames)
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := root.prop(t, "spec")

	if def := spec.prop(t, "schedulable").Default; def != true {
		t.Errorf("spec.schedulable default = %v (%T), want true", def, def)
	}

	health := root.prop(t, "status").prop(t, "health")
	for _, want := range []string{"Ready", "NotReady", "Unknown"} {
		if !containsStr(health.Enum, want) {
			t.Errorf("status.health enum = %v, want to contain %q", health.Enum, want)
		}
	}
}
