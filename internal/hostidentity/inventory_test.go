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

package hostidentity_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/inventory"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// validInventory is a complete inventory that answers a request for version 1.
func validInventory() *providerv1.HostInventory {
	d := sha256.Sum256(nil)
	return &providerv1.HostInventory{
		InventoryVersion: 1, State: providerv1.InventoryState_INVENTORY_STATE_COMPLETE, SnapshotAgeSeconds: 30,
		Components: []*providerv1.HostComponent{
			{Name: "cloud-hypervisor", State: providerv1.ComponentState_COMPONENT_STATE_READY, ObservedVersion: "1.0.0",
				DeclaredVersion: "1.0.0", Source: "sysext", ManifestOrigin: providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY,
				ArtifactSha256: d[:], Files: []*providerv1.ComponentFile{{Name: "cloud-hypervisor", Sha256: d[:]}}},
			{Name: "firmware", State: providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED, StateReason: "present_undeclared",
				ObservedVersion: "2", ManifestOrigin: providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE},
		},
		Features: []*providerv1.HostFeature{
			{Name: "cgroup_v2", Result: providerv1.FeatureResult_FEATURE_RESULT_PRESENT},
			{Name: "hypervisor_pause_resume", Result: providerv1.FeatureResult_FEATURE_RESULT_ERROR, Reason: "not_evaluated"},
			{Name: "kvm", Result: providerv1.FeatureResult_FEATURE_RESULT_PRESENT},
		},
		ProcessIdentityTier: providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE,
		UnexpectedExtensions: []*providerv1.HostExtension{
			{Name: "debug-tools", Kind: providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY},
			{Name: "extra.raw", Kind: providerv1.ExtensionKind_EXTENSION_KIND_UNDECLARED_IMAGE, ImageSha256: d[:]},
		},
		RequestedVersion: 1, SupportedVersion: 1,
	}
}

// TestInventoryVectorIV1 decodes the published inventory encoding vector and
// checks that the inventory in it passes the field rules for a request of
// version 1.
func TestInventoryVectorIV1(t *testing.T) {
	body, err := hex.DecodeString("120676302e302e31" + "4a54" + "08011001181e" + "2246" + "0a10636c6f75642d68797065727669736f72" +
		"1001" + "2205312e302e30" + "2a05312e302e30" + "4002" +
		"4a20e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" + "3002" + "4801" + "5001")
	if err != nil {
		t.Fatal(err)
	}
	var a providerv1.HostAnswer
	if err := proto.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	if err := hostidentity.VerifyInventory(a.GetInventory(), 1); err != nil {
		t.Fatalf("the vector's inventory: %v", err)
	}
	m, err := hostidentity.AnswerSignatureInput(hostidentity.BodyTypeHostAnswer, body)
	if err != nil {
		t.Fatal(err)
	}
	if d := sha256.Sum256(m); hex.EncodeToString(d[:]) != "43b3a3f66dc30637eea72a7b65bde3ee4f93c9844186838322a88971569be858" {
		t.Fatalf("SHA-256(M) = %x", d)
	}
}

// TestInventoryValueSetPinned pins the closed value sets of inventory version 1.
// A change to any of them is a new inventory version.
func TestInventoryValueSetPinned(t *testing.T) {
	vs, ok := hostidentity.InventoryValueSetOf(1)
	if !ok || hostidentity.InventoryVersion != 1 {
		t.Fatal("inventory version 1 is not implemented")
	}
	if _, ok := hostidentity.InventoryValueSetOf(2); ok {
		t.Fatal("inventory version 2 is implemented")
	}
	want := hostidentity.InventoryValueSet{
		MaxState: 4, MaxComponentState: 6, MaxOrigin: 3, MaxResult: 3, MaxTier: 4, MaxKind: 3,
		OverLimitReasons:  []string{"too_many_components", "too_many_files", "too_many_features", "too_many_extensions", "inventory_too_large"},
		UnreadableReasons: []string{"io_error", "manifest_dir_unreadable", "probe_failed"},
		ComponentReasons: map[providerv1.ComponentState][]string{
			providerv1.ComponentState_COMPONENT_STATE_MISSING:      {"not_installed", "not_declared"},
			providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE: {"version_out_of_range", "version_unreadable"},
			providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY: {"unexpected_extension", "artifact_digest_mismatch", "file_digest_mismatch",
				"socket_unanswered", "self_test_failed", "digest_limit_exceeded", "not_content_addressed"},
			providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED: {"present_undeclared"},
			providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID: {"parse_error", "unsupported_digest", "duplicate_declaration",
				"version_too_long", "invalid_name", "too_large", "unknown_component"},
		},
		Features: []string{"cgroup_v2", "clone_into_cgroup", "pidfd", "pidfs_handle", "pidfd_info_exit", "initial_pid_namespace",
			"initial_cgroup_namespace", "initial_time_namespace", "hypervisor_pause_resume", "kvm", "tun", "secure_boot",
			"measured_boot", "extension_image_policy_signed"},
		FeatureReasons: []string{"probe_failed", "probe_timeout", "permission_denied", "not_evaluated"},
	}
	if !reflect.DeepEqual(vs, want) {
		t.Fatalf("value set of version 1 = %+v", vs)
	}
	// The enum maxima are the generated enums' highest values.
	for name, pair := range map[string][2]int{
		"InventoryState":      {int(vs.MaxState), len(providerv1.InventoryState_name) - 1},
		"ComponentState":      {int(vs.MaxComponentState), len(providerv1.ComponentState_name) - 1},
		"ManifestOrigin":      {int(vs.MaxOrigin), len(providerv1.ManifestOrigin_name) - 1},
		"FeatureResult":       {int(vs.MaxResult), len(providerv1.FeatureResult_name) - 1},
		"ProcessIdentityTier": {int(vs.MaxTier), len(providerv1.ProcessIdentityTier_name) - 1},
		"ExtensionKind":       {int(vs.MaxKind), len(providerv1.ExtensionKind_name) - 1},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: value set maximum %d, proto maximum %d", name, pair[0], pair[1])
		}
	}
	// A copy: editing it changes nothing.
	vs.Features[0] = "edited"
	if again, _ := hostidentity.InventoryValueSetOf(1); again.Features[0] != "cgroup_v2" {
		t.Fatal("InventoryValueSetOf returned the table itself")
	}
}

// TestVerifyInventoryAcceptsAgentReports: what the agent emits for a report
// that lists nothing passes, for every reason it may carry.
func TestVerifyInventoryAcceptsAgentReports(t *testing.T) {
	if err := hostidentity.VerifyInventory(validInventory(), 1); err != nil {
		t.Fatal(err)
	}
	if err := hostidentity.VerifyInventory(nil, 1); err != nil {
		t.Fatalf("an absent inventory: %v", err)
	}
	vs, _ := hostidentity.InventoryValueSetOf(1)
	for _, r := range vs.OverLimitReasons {
		if err := hostidentity.VerifyInventory(inventory.OverLimit(1, r), 1); err != nil {
			t.Errorf("over limit %s: %v", r, err)
		}
	}
	for _, r := range vs.UnreadableReasons {
		if err := hostidentity.VerifyInventory(inventory.Unreadable(1, r), 1); err != nil {
			t.Errorf("unreadable %s: %v", r, err)
		}
	}
	pending := &providerv1.HostInventory{InventoryVersion: 1, State: providerv1.InventoryState_INVENTORY_STATE_PENDING,
		RequestedVersion: 1, SupportedVersion: 1}
	if err := hostidentity.VerifyInventory(pending, 1); err != nil {
		t.Errorf("pending: %v", err)
	}
	// An agent that supports a later version answers a request for 1 with 1.
	later := validInventory()
	later.SupportedVersion = 3
	if err := hostidentity.VerifyInventory(later, 1); err != nil {
		t.Errorf("an agent of a later version: %v", err)
	}
}

// TestVerifyInventoryEcho: the version binding. A changed request is the
// relay's fault (ErrInventoryEcho); a wrong minimum or support is the agent's.
func TestVerifyInventoryEcho(t *testing.T) {
	inv := validInventory()
	for name, tc := range map[string]struct {
		edit func(*providerv1.HostInventory)
		sent uint32
		want error
	}{
		"inventory present though none was asked for": {sent: 0, want: hostidentity.ErrInventoryEcho},
		"request version lowered in transit": {sent: 2, edit: func(i *providerv1.HostInventory) {
			i.RequestedVersion, i.SupportedVersion = 1, 2
		}, want: hostidentity.ErrInventoryEcho},
		"request version raised in transit": {sent: 1, edit: func(i *providerv1.HostInventory) { i.RequestedVersion = 2 },
			want: hostidentity.ErrInventoryEcho},
		"inventory_version is not the minimum": {sent: 1, edit: func(i *providerv1.HostInventory) {
			i.InventoryVersion, i.SupportedVersion = 2, 2
		}, want: hostidentity.ErrInventoryInvalid},
		"supported_version 0": {sent: 1, edit: func(i *providerv1.HostInventory) { i.SupportedVersion = 0 },
			want: hostidentity.ErrInventoryInvalid},
		"inventory_version 0": {sent: 1, edit: func(i *providerv1.HostInventory) { i.InventoryVersion = 0 },
			want: hostidentity.ErrInventoryInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := proto.Clone(inv).(*providerv1.HostInventory)
			if tc.edit != nil {
				tc.edit(c)
			}
			err := hostidentity.VerifyInventory(c, tc.sent)
			if !errors.Is(err, tc.want) || !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("VerifyInventory = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestVerifyInventoryFieldRules: each ordering, bound, state-consistency and
// closed-set rule refuses the answer as the agent's fault.
func TestVerifyInventoryFieldRules(t *testing.T) {
	d := sha256.Sum256([]byte("x"))
	comp := func(i *providerv1.HostInventory) *providerv1.HostComponent { return i.Components[0] }
	many := func(n int, f func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = f(i)
		}
		return out
	}
	cases := map[string]func(*providerv1.HostInventory){
		// Ordering and uniqueness.
		"components unordered": func(i *providerv1.HostInventory) { i.Components[0], i.Components[1] = i.Components[1], i.Components[0] },
		"components repeated": func(i *providerv1.HostInventory) {
			c, _ := proto.Clone(i.Components[0]).(*providerv1.HostComponent)
			i.Components[1] = c
		},
		"features unordered": func(i *providerv1.HostInventory) { i.Features[0], i.Features[2] = i.Features[2], i.Features[0] },
		"extensions unordered": func(i *providerv1.HostInventory) {
			i.UnexpectedExtensions[0], i.UnexpectedExtensions[1] = i.UnexpectedExtensions[1], i.UnexpectedExtensions[0]
		},
		"files unordered": func(i *providerv1.HostInventory) {
			comp(i).Files = []*providerv1.ComponentFile{{Name: "b", Sha256: d[:]}, {Name: "a", Sha256: d[:]}}
		},
		// Bounds.
		"too many components": func(i *providerv1.HostInventory) {
			i.Components = nil
			for _, n := range many(hostidentity.MaxInventoryComponents+1, func(k int) string { return fmt.Sprintf("c%03d", k) }) {
				i.Components = append(i.Components, &providerv1.HostComponent{Name: n, State: providerv1.ComponentState_COMPONENT_STATE_MISSING,
					StateReason: "not_declared", ManifestOrigin: providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE})
			}
		},
		"too many files in a component": func(i *providerv1.HostInventory) {
			comp(i).Files = nil
			for _, n := range many(hostidentity.MaxInventoryComponentFiles+1, func(k int) string { return fmt.Sprintf("f%02d", k) }) {
				comp(i).Files = append(comp(i).Files, &providerv1.ComponentFile{Name: n, Sha256: d[:]})
			}
		},
		"too many features": func(i *providerv1.HostInventory) {
			i.Features = nil
			for range hostidentity.MaxInventoryFeatures + 1 {
				i.Features = append(i.Features, &providerv1.HostFeature{Name: "kvm", Result: providerv1.FeatureResult_FEATURE_RESULT_PRESENT})
			}
		},
		"too many extensions": func(i *providerv1.HostInventory) {
			i.UnexpectedExtensions = nil
			for _, n := range many(hostidentity.MaxInventoryExtensions+1, func(k int) string { return fmt.Sprintf("e%02d", k) }) {
				i.UnexpectedExtensions = append(i.UnexpectedExtensions, &providerv1.HostExtension{Name: n,
					Kind: providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY})
			}
		},
		"component name too long":   func(i *providerv1.HostInventory) { comp(i).Name = "c" + strings.Repeat("a", 63) },
		"component name with slash": func(i *providerv1.HostInventory) { comp(i).Name = "a/b" },
		"reserved prefix":           func(i *providerv1.HostInventory) { comp(i).Name = "manifest-0123456789abcdef" },
		"file name is a path":       func(i *providerv1.HostInventory) { comp(i).Files[0].Name = "/usr/bin/x" },
		"short file digest":         func(i *providerv1.HostInventory) { comp(i).Files[0].Sha256 = d[:31] },
		"short artifact digest":     func(i *providerv1.HostInventory) { comp(i).ArtifactSha256 = d[:16] },
		"version too long":          func(i *providerv1.HostInventory) { comp(i).ObservedVersion = "1" + strings.Repeat(".0", 32) },
		"declared version charset":  func(i *providerv1.HostInventory) { comp(i).DeclaredVersion = "1 0" },
		"source charset":            func(i *providerv1.HostInventory) { comp(i).Source = "Sysext" },
		"extension image digest of a directory": func(i *providerv1.HostInventory) {
			i.UnexpectedExtensions[0].ImageSha256 = d[:]
		},
		"undeclared image without a digest": func(i *providerv1.HostInventory) { i.UnexpectedExtensions[1].ImageSha256 = nil },
		"extension name is a path":          func(i *providerv1.HostInventory) { i.UnexpectedExtensions[0].Name = "../x" },
		// Inventory state consistency.
		"complete with a reason":  func(i *providerv1.HostInventory) { i.StateReason = "io_error" },
		"complete without a tier": func(i *providerv1.HostInventory) { i.ProcessIdentityTier = 0 },
		"unspecified state":       func(i *providerv1.HostInventory) { i.State = 0 },
		"state above the version": func(i *providerv1.HostInventory) { i.State = 5 },
		"tier above the version":  func(i *providerv1.HostInventory) { i.ProcessIdentityTier = 5 },
		"pending that lists":      func(i *providerv1.HostInventory) { i.State, i.ProcessIdentityTier, i.SnapshotAgeSeconds = 2, 0, 0 },
		"over limit that lists": func(i *providerv1.HostInventory) {
			i.State, i.ProcessIdentityTier, i.StateReason = 3, 0, "too_many_files"
		},
		"over limit with a reason of unreadable": func(i *providerv1.HostInventory) {
			*i = *inventory.OverLimit(1, "too_many_files")
			i.StateReason = "io_error"
		},
		"unreadable without a reason": func(i *providerv1.HostInventory) {
			*i = *inventory.Unreadable(1, "io_error")
			i.StateReason = ""
		},
		// Component state consistency.
		"ready with a reason": func(i *providerv1.HostInventory) { comp(i).StateReason = "not_installed" },
		"ready without a manifest": func(i *providerv1.HostInventory) {
			comp(i).ManifestOrigin = 1
			comp(i).DeclaredVersion, comp(i).Source = "", ""
		},
		"ready without a version": func(i *providerv1.HostInventory) { comp(i).ObservedVersion = "" },
		"reason outside the state's set": func(i *providerv1.HostInventory) {
			comp(i).State, comp(i).StateReason = providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY, "not_installed"
		},
		"unlisted reason": func(i *providerv1.HostInventory) {
			comp(i).State, comp(i).StateReason = providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY, "it_broke"
		},
		"unmanifested with a manifest origin": func(i *providerv1.HostInventory) { i.Components[1].ManifestOrigin = 2 },
		"no manifest in state unhealthy": func(i *providerv1.HostInventory) {
			i.Components[1].State, i.Components[1].StateReason = providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY, "self_test_failed"
		},
		"no manifest with a declared version":   func(i *providerv1.HostInventory) { i.Components[1].DeclaredVersion = "1" },
		"manifested without a declared version": func(i *providerv1.HostInventory) { comp(i).DeclaredVersion = "" },
		"incompatible without a range": func(i *providerv1.HostInventory) {
			comp(i).State, comp(i).StateReason = providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE, "version_out_of_range"
		},
		"range when not incompatible": func(i *providerv1.HostInventory) { comp(i).RequiredRange = ">=1" },
		"missing with files": func(i *providerv1.HostInventory) {
			comp(i).State, comp(i).StateReason, comp(i).ObservedVersion = providerv1.ComponentState_COMPONENT_STATE_MISSING, "not_installed", ""
		},
		"unspecified component state": func(i *providerv1.HostInventory) { comp(i).State = 0 },
		"unspecified origin":          func(i *providerv1.HostInventory) { comp(i).ManifestOrigin = 0 },
		// Features.
		"unlisted probe":               func(i *providerv1.HostInventory) { i.Features[0].Name = "cgroup_v1" },
		"error without a reason":       func(i *providerv1.HostInventory) { i.Features[1].Reason = "" },
		"present with a reason":        func(i *providerv1.HostInventory) { i.Features[0].Reason = "probe_failed" },
		"unlisted feature reason":      func(i *providerv1.HostInventory) { i.Features[1].Reason = "segfault" },
		"unspecified feature result":   func(i *providerv1.HostInventory) { i.Features[0].Result = 0 },
		"unspecified extension kind":   func(i *providerv1.HostInventory) { i.UnexpectedExtensions[0].Kind = 0 },
		"extension kind above version": func(i *providerv1.HostInventory) { i.UnexpectedExtensions[1].Kind = 4 },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			inv := validInventory()
			edit(inv)
			err := hostidentity.VerifyInventory(inv, 1)
			if !errors.Is(err, hostidentity.ErrInventoryInvalid) || !errors.Is(err, hostidentity.ErrAnswer) {
				t.Fatalf("VerifyInventory = %v, want ErrInventoryInvalid", err)
			}
		})
	}
}

// TestVerifyAnswerInventory: VerifyAnswer judges a HostAnswer's inventory
// against the inventory version the request sent, after every other check.
func TestVerifyAnswerInventory(t *testing.T) {
	f := newAnswerFixture(t)
	ha := func(inv *providerv1.HostInventory) *providerv1.HostAnswer {
		return &providerv1.HostAnswer{Context: f.ctx(), AgentVersion: "v0.0.1", AgentApiVersion: 2, GrantVersions: []uint32{1},
			Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY, Inventory: inv}
	}
	exp := f.exp(hostidentity.BodyTypeHostAnswer)
	exp.InventoryVersion = 1
	v, err := hostidentity.VerifyAnswer(f.sign(t, ha(validInventory())), exp)
	if err != nil || v.Host.GetInventory() == nil {
		t.Fatalf("VerifyAnswer = %v", err)
	}
	// Asked for, absent: a valid answer without inventory.
	if v, err := hostidentity.VerifyAnswer(f.sign(t, ha(nil)), exp); err != nil || v.Host.GetInventory() != nil {
		t.Fatalf("an answer without the inventory asked for: %v", err)
	}
	// Not asked for, present: the echo fails.
	noAsk := f.exp(hostidentity.BodyTypeHostAnswer)
	if _, err := hostidentity.VerifyAnswer(f.sign(t, ha(validInventory())), noAsk); !errors.Is(err, hostidentity.ErrInventoryEcho) {
		t.Fatalf("VerifyAnswer = %v, want ErrInventoryEcho", err)
	}
	// Asked for a version this manager does not implement.
	high := exp
	high.InventoryVersion = 2
	if _, err := hostidentity.VerifyAnswer(f.sign(t, ha(nil)), high); !errors.Is(err, hostidentity.ErrAnswer) {
		t.Fatalf("VerifyAnswer = %v, want ErrAnswer", err)
	}
	// An agent fault in an answer whose nonce is wrong is a nonce failure:
	// no inventory is judged before everything else passed.
	bad := validInventory()
	bad.Features[0].Name = "unlisted"
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(ha(bad))
	if err != nil {
		t.Fatal(err)
	}
	badBlob := signRaw(t, f.pki.HostKey, f.cert, hostidentity.BodyTypeHostAnswer, raw)
	wrongNonce := exp
	wrongNonce.Nonce = bytes.Repeat([]byte{9}, 16)
	_, err = hostidentity.VerifyAnswer(badBlob, wrongNonce)
	if !errors.Is(err, hostidentity.ErrAnswer) || errors.Is(err, hostidentity.ErrInventoryInvalid) {
		t.Fatalf("VerifyAnswer = %v, want a nonce refusal", err)
	}
	if _, err := hostidentity.VerifyAnswer(badBlob, exp); !errors.Is(err, hostidentity.ErrInventoryInvalid) {
		t.Fatalf("VerifyAnswer = %v, want ErrInventoryInvalid", err)
	}
}
