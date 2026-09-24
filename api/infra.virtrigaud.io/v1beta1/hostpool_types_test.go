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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- HostPool defaulted-bool footgun ----------------------------------------

// TestPoolMigrationPolicy_DefaultLiveFalseSurvivesMarshal guards the
// defaulted-bool footgun (ADR-0006/PR#235): DefaultLive defaults to true, so
// with omitempty an explicit false would be dropped on marshal, the apiserver
// would re-apply the true default, and a pool configured for cold-only migration
// would silently flip back to live. The field must always serialize.
func TestPoolMigrationPolicy_DefaultLiveFalseSurvivesMarshal(t *testing.T) {
	b, err := json.Marshal(PoolMigrationPolicy{DefaultLive: false})
	if err != nil {
		t.Fatalf("marshal PoolMigrationPolicy: %v", err)
	}
	if !strings.Contains(string(b), `"defaultLive":false`) {
		t.Fatalf("PoolMigrationPolicy.DefaultLive=false was dropped on marshal (omitempty footgun); got %s", b)
	}

	var got PoolMigrationPolicy
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.DefaultLive {
		t.Fatalf("PoolMigrationPolicy.DefaultLive round-tripped to true; want false")
	}
}

// --- HostPool enum vocabulary -----------------------------------------------

// TestPoolStrategyConstants pins the wire values of the scheduling strategy enum.
func TestPoolStrategyConstants(t *testing.T) {
	if PoolStrategySpread != "Spread" {
		t.Errorf("PoolStrategySpread = %q, want Spread", PoolStrategySpread)
	}
	if PoolStrategyBinPack != "BinPack" {
		t.Errorf("PoolStrategyBinPack = %q, want BinPack", PoolStrategyBinPack)
	}
}

// TestMigrationStorageModeConstants pins the wire values of the storage-mode enum
// so they stay in lockstep with the CRD enum and the ADR-0007 D5 gRPC contract.
func TestMigrationStorageModeConstants(t *testing.T) {
	cases := map[string]string{
		MigrationStorageModeShared:    "shared",
		MigrationStorageModeBlockAll:  "block_all",
		MigrationStorageModeBlockInc:  "block_inc",
		MigrationStorageModeStageCopy: "stage_copy",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("storage-mode constant = %q, want %q", got, want)
		}
	}
}

// --- HostPool JSON round-trip -----------------------------------------------

// TestHostPool_JSONRoundTrip verifies a fully-populated HostPool serializes and
// deserializes without loss, and that nested policy/refs survive.
func TestHostPool_JSONRoundTrip(t *testing.T) {
	bw := int32(10000)
	dt := int32(500)
	orig := HostPool{
		TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "HostPool"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-a",
			Namespace: "cluster-a",
		},
		Spec: HostPoolSpec{
			ProviderRef:  LocalObjectReference{Name: "libvirt-cluster"},
			Strategy:     PoolStrategySpread,
			Overcommit:   &OvercommitRatios{CPU: "4.0", Memory: "1.0"},
			StoragePools: []StoragePoolRef{{Name: "nfs01"}, {Name: "local-ssd"}},
			Networks:     []PoolNetworkRef{{Name: "br-vlan100"}},
			Migration: &PoolMigrationPolicy{
				DefaultLive:        true,
				DefaultStorageMode: MigrationStorageModeShared,
				RequireTLS:         true,
				BandwidthMbps:      &bw,
				MaxDowntimeMs:      &dt,
			},
		},
		Status: HostPoolStatus{
			TotalHosts:         3,
			ReadyHosts:         3,
			SchedulableHosts:   2,
			ObservedGeneration: 4,
			Conditions: []metav1.Condition{{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "AllHostsReady",
			}},
		},
	}

	b1, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal HostPool: %v", err)
	}
	var got HostPool
	if err := json.Unmarshal(b1, &got); err != nil {
		t.Fatalf("unmarshal HostPool: %v", err)
	}
	b2, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal HostPool: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("HostPool round-trip not stable:\n first: %s\nsecond: %s", b1, b2)
	}

	if got.Spec.Strategy != PoolStrategySpread {
		t.Errorf("Strategy = %q, want %q", got.Spec.Strategy, PoolStrategySpread)
	}
	if got.Spec.Overcommit == nil || got.Spec.Overcommit.CPU != "4.0" {
		t.Errorf("Overcommit not preserved: %+v", got.Spec.Overcommit)
	}
	if len(got.Spec.StoragePools) != 2 || got.Spec.StoragePools[0].Name != "nfs01" {
		t.Errorf("StoragePools not preserved: %+v", got.Spec.StoragePools)
	}
	if got.Spec.Migration == nil || got.Spec.Migration.DefaultStorageMode != MigrationStorageModeShared {
		t.Errorf("Migration not preserved: %+v", got.Spec.Migration)
	}
}

// --- HostPool DeepCopy ------------------------------------------------------

// TestHostPool_DeepCopy verifies generated deepcopy independence and object
// safety across the pool's slices and pointer sub-structs.
func TestHostPool_DeepCopy(t *testing.T) {
	bw := int32(5000)
	orig := &HostPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: HostPoolSpec{
			ProviderRef:  LocalObjectReference{Name: "p"},
			Strategy:     PoolStrategyBinPack,
			Overcommit:   &OvercommitRatios{CPU: "4.0"},
			StoragePools: []StoragePoolRef{{Name: "nfs01"}},
			Networks:     []PoolNetworkRef{{Name: "br0"}},
			Migration:    &PoolMigrationPolicy{DefaultLive: true, BandwidthMbps: &bw},
		},
		Status: HostPoolStatus{
			TotalHosts: 3,
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}

	cp := orig.DeepCopy()

	// Mutate the original's deep fields.
	orig.Spec.Overcommit.CPU = "8.0"
	orig.Spec.StoragePools[0].Name = "mutated"
	orig.Spec.Networks[0].Name = "mutated"
	*orig.Spec.Migration.BandwidthMbps = 1
	orig.Status.Conditions[0].Reason = "mutated"

	if cp.Spec.Overcommit.CPU != "4.0" {
		t.Errorf("Overcommit not deep-copied: %q", cp.Spec.Overcommit.CPU)
	}
	if cp.Spec.StoragePools[0].Name != "nfs01" {
		t.Errorf("StoragePools not deep-copied: %q", cp.Spec.StoragePools[0].Name)
	}
	if cp.Spec.Networks[0].Name != "br0" {
		t.Errorf("Networks not deep-copied: %q", cp.Spec.Networks[0].Name)
	}
	if *cp.Spec.Migration.BandwidthMbps != 5000 {
		t.Errorf("Migration.BandwidthMbps not deep-copied: %d", *cp.Spec.Migration.BandwidthMbps)
	}
	if cp.Status.Conditions[0].Reason != "" {
		t.Errorf("Conditions not deep-copied: %q", cp.Status.Conditions[0].Reason)
	}

	if obj := orig.DeepCopyObject(); obj == nil {
		t.Error("HostPool.DeepCopyObject() returned nil")
	}
	if got := (*HostPool)(nil).DeepCopy(); got != nil {
		t.Error("(*HostPool)(nil).DeepCopy() should return nil")
	}

	list := &HostPoolList{Items: []HostPool{*orig}}
	if obj := list.DeepCopyObject(); obj == nil {
		t.Error("HostPoolList.DeepCopyObject() returned nil")
	}
}

// --- HostPool CRD schema (defaulting + enum) --------------------------------

// TestHostPoolCRDSchemaDefaults asserts the generated HostPool CRD encodes the
// Strategy=Spread default and enum, the migration-policy defaults
// (defaultLive=true, defaultStorageMode=shared, requireTLS=false) and its enum,
// and the hp shortName.
func TestHostPoolCRDSchemaDefaults(t *testing.T) {
	crd := loadCRD(t, "infra.virtrigaud.io_hostpools.yaml")

	if crd.Spec.Names.Kind != "HostPool" {
		t.Errorf("kind = %q, want HostPool", crd.Spec.Names.Kind)
	}
	if !containsStr(crd.Spec.Names.ShortNames, "hp") {
		t.Errorf("shortNames = %v, want to contain hp", crd.Spec.Names.ShortNames)
	}

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.prop(t, "spec")

	strategy := spec.prop(t, "strategy")
	if def := strategy.Default; def != "Spread" {
		t.Errorf("spec.strategy default = %v (%T), want Spread", def, def)
	}
	for _, want := range []string{"Spread", "BinPack"} {
		if !containsStr(strategy.Enum, want) {
			t.Errorf("spec.strategy enum = %v, want to contain %q", strategy.Enum, want)
		}
	}

	mig := spec.prop(t, "migration")
	if def := mig.prop(t, "defaultLive").Default; def != true {
		t.Errorf("migration.defaultLive default = %v (%T), want true", def, def)
	}
	if def := mig.prop(t, "requireTLS").Default; def != false {
		t.Errorf("migration.requireTLS default = %v (%T), want false", def, def)
	}
	sm := mig.prop(t, "defaultStorageMode")
	if def := sm.Default; def != "shared" {
		t.Errorf("migration.defaultStorageMode default = %v (%T), want shared", def, def)
	}
	for _, want := range []string{"shared", "block_all", "block_inc", "stage_copy"} {
		if !containsStr(sm.Enum, want) {
			t.Errorf("migration.defaultStorageMode enum = %v, want to contain %q", sm.Enum, want)
		}
	}
}
