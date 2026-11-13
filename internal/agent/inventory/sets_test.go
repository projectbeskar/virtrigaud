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

package inventory

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestValueSetsArePinned pins the closed sets of inventory version 1 to the
// published table: a change to any string is a new inventory version.
func TestValueSetsArePinned(t *testing.T) {
	vs := valueSets[1]
	require.NotNil(t, vs)
	assert.Equal(t, []string{"too_many_components", "too_many_files", "too_many_features", "too_many_extensions", "inventory_too_large"}, vs.overLimit)
	assert.Equal(t, []string{"io_error", "manifest_dir_unreadable", "probe_failed"}, vs.unreadable)
	assert.Equal(t, map[providerv1.ComponentState][]string{
		stMissing:      {"not_installed", "not_declared"},
		stIncompatible: {"version_out_of_range", "version_unreadable"},
		stUnhealthy: {"unexpected_extension", "artifact_digest_mismatch", "file_digest_mismatch", "socket_unanswered", "self_test_failed",
			"digest_limit_exceeded", "not_content_addressed"},
		stUnmanifested: {"present_undeclared"},
		stInvalid:      {"parse_error", "unsupported_digest", "duplicate_declaration", "version_too_long", "invalid_name", "too_large", "unknown_component"},
	}, vs.component)
	assert.Equal(t, []string{"cgroup_v2", "clone_into_cgroup", "pidfd", "pidfs_handle", "pidfd_info_exit", "initial_pid_namespace",
		"initial_cgroup_namespace", "initial_time_namespace", "hypervisor_pause_resume", "secure_boot", "measured_boot",
		"extension_image_policy_signed"}, vs.features)
	assert.Equal(t, []string{"probe_failed", "probe_timeout", "permission_denied"}, vs.featReasons)
	assert.Equal(t, [6]int32{4, 6, 3, 3, 4, 3}, [6]int32{vs.maxState, vs.maxComponentState, vs.maxOrigin, vs.maxResult, vs.maxTier, vs.maxKind})
	assert.EqualValues(t, 1, SupportedVersion)
	// The probes the collector runs are exactly the set.
	var names []string
	for _, p := range defaultProbes() {
		names = append(names, p.name)
	}
	assert.ElementsMatch(t, vs.features, names)
	// Every reason the code can produce is in its set.
	for _, r := range []string{ReasonSocketUnanswered, ReasonSelfTestFailed, ReasonNotContentAddressed} {
		assert.Contains(t, vs.component[stUnhealthy], r)
	}
}

// validInventory is a COMPLETE inventory that satisfies every rule.
func validInventory() *providerv1.HostInventory {
	d := rawDigest([]byte("x"))
	inv := &providerv1.HostInventory{
		State: providerv1.InventoryState_INVENTORY_STATE_COMPLETE, SnapshotAgeSeconds: 3,
		ProcessIdentityTier: providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE,
		Components: []*providerv1.HostComponent{
			{Name: "cloud-hypervisor", State: stReady, ObservedVersion: "1.0.0", DeclaredVersion: "1.0.0", Source: "sysext", ManifestOrigin: originRO,
				ArtifactSha256: d, Files: []*providerv1.ComponentFile{{Name: "a.bin", Sha256: d}, {Name: "b.bin", Sha256: d}}},
			{Name: "guest-kernel", State: stUnmanifested, StateReason: ReasonPresentUndeclared, ManifestOrigin: originNone},
		},
		Features: []*providerv1.HostFeature{
			{Name: FeatureCgroupV2, Result: providerv1.FeatureResult_FEATURE_RESULT_PRESENT},
			{Name: FeaturePidfd, Result: providerv1.FeatureResult_FEATURE_RESULT_ERROR, Reason: FeatureReasonProbeFailed},
		},
		UnexpectedExtensions: []*providerv1.HostExtension{
			{Name: "a-dir", Kind: providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY},
			{Name: "b.raw", Kind: providerv1.ExtensionKind_EXTENSION_KIND_UNDECLARED_IMAGE, ImageSha256: d},
		},
	}
	bind(inv, 1)
	return inv
}

func TestValidateAccepts(t *testing.T) {
	require.NoError(t, Validate(validInventory(), 1))
	// Echo and min rule: a manager that asks for a higher version gets 1.
	inv := validInventory()
	bind(inv, 7)
	require.NoError(t, Validate(inv, 7))
	assert.EqualValues(t, 1, inv.GetInventoryVersion())
	assert.EqualValues(t, 7, inv.GetRequestedVersion())
	assert.EqualValues(t, 1, inv.GetSupportedVersion())
}

func TestValidateRefuses(t *testing.T) {
	type mut = func(*providerv1.HostInventory)
	comp := func(i int) *providerv1.HostComponent { return nil }
	_ = comp
	tests := []struct {
		name string
		mut  mut
		req  uint32
	}{
		{"requested 0", func(i *providerv1.HostInventory) {}, 0},
		{"requested differs from the request", func(i *providerv1.HostInventory) { i.RequestedVersion = 2 }, 1},
		{"supported 0", func(i *providerv1.HostInventory) { i.SupportedVersion = 0 }, 1},
		{"supported above the implemented", func(i *providerv1.HostInventory) { i.SupportedVersion = 2 }, 1},
		{"inventory_version not min", func(i *providerv1.HostInventory) { i.InventoryVersion = 2 }, 1},
		{"inventory_version zero", func(i *providerv1.HostInventory) { i.InventoryVersion = 0 }, 1},
		{"unspecified state", func(i *providerv1.HostInventory) { i.State = 0 }, 1},
		{"state beyond the set", func(i *providerv1.HostInventory) { i.State = 9 }, 1},
		{"unknown field", func(i *providerv1.HostInventory) { i.ProtoReflect().SetUnknown([]byte{0xf8, 0x06, 0x01}) }, 1},
		{"unknown field in a component", func(i *providerv1.HostInventory) { i.Components[0].ProtoReflect().SetUnknown([]byte{0xf8, 0x06, 0x01}) }, 1},
		{"unknown field in a feature", func(i *providerv1.HostInventory) { i.Features[0].ProtoReflect().SetUnknown([]byte{0xf8, 0x06, 0x01}) }, 1},
		{"unknown field in an extension", func(i *providerv1.HostInventory) {
			i.UnexpectedExtensions[0].ProtoReflect().SetUnknown([]byte{0xf8, 0x06, 0x01})
		}, 1},
		{"unknown field in a file", func(i *providerv1.HostInventory) {
			i.Components[0].Files[0].ProtoReflect().SetUnknown([]byte{0xf8, 0x06, 0x01})
		}, 1},
		{"COMPLETE with a reason", func(i *providerv1.HostInventory) { i.StateReason = ReasonIOError }, 1},
		{"COMPLETE without a tier", func(i *providerv1.HostInventory) { i.ProcessIdentityTier = 0 }, 1},
		{"tier beyond the set", func(i *providerv1.HostInventory) { i.ProcessIdentityTier = 9 }, 1},
		{"PENDING that lists", func(i *providerv1.HostInventory) {
			i.State, i.SnapshotAgeSeconds, i.ProcessIdentityTier = providerv1.InventoryState_INVENTORY_STATE_PENDING, 0, 0
			i.Features, i.UnexpectedExtensions = nil, nil
		}, 1},
		{"PENDING with an age", func(i *providerv1.HostInventory) {
			*i = providerv1.HostInventory{State: providerv1.InventoryState_INVENTORY_STATE_PENDING, SnapshotAgeSeconds: 1}
			bind(i, 1)
		}, 1},
		{"OVER_LIMIT that lists", func(i *providerv1.HostInventory) {
			i.State, i.StateReason, i.ProcessIdentityTier = providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT, ReasonTooManyFiles, 0
		}, 1},
		{"OVER_LIMIT with an unreadable reason", func(i *providerv1.HostInventory) {
			*i = providerv1.HostInventory{State: providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT, StateReason: ReasonIOError}
			bind(i, 1)
		}, 1},
		{"UNREADABLE with an over-limit reason", func(i *providerv1.HostInventory) {
			*i = providerv1.HostInventory{State: providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, StateReason: ReasonTooManyFiles}
			bind(i, 1)
		}, 1},
		{"UNREADABLE without a reason", func(i *providerv1.HostInventory) {
			*i = providerv1.HostInventory{State: providerv1.InventoryState_INVENTORY_STATE_UNREADABLE}
			bind(i, 1)
		}, 1},
		{"components out of order", func(i *providerv1.HostInventory) { i.Components[0], i.Components[1] = i.Components[1], i.Components[0] }, 1},
		{"duplicate component", func(i *providerv1.HostInventory) { i.Components[1].Name = i.Components[0].Name }, 1},
		{"component name charset", func(i *providerv1.HostInventory) { i.Components[0].Name = "Cloud_Hypervisor" }, 1},
		{"reserved prefix on a valid component", func(i *providerv1.HostInventory) { i.Components[0].Name = "manifest-0123456789abcdef" }, 1},
		{"component state unspecified", func(i *providerv1.HostInventory) { i.Components[0].State = 0 }, 1},
		{"component state beyond the set", func(i *providerv1.HostInventory) { i.Components[0].State = 7 }, 1},
		{"origin unspecified", func(i *providerv1.HostInventory) { i.Components[0].ManifestOrigin = 0 }, 1},
		{"origin beyond the set", func(i *providerv1.HostInventory) { i.Components[0].ManifestOrigin = 4 }, 1},
		{"READY with a reason", func(i *providerv1.HostInventory) { i.Components[0].StateReason = ReasonNotInstalled }, 1},
		{"READY without origin", func(i *providerv1.HostInventory) {
			i.Components[0].ManifestOrigin, i.Components[0].DeclaredVersion, i.Components[0].Source = originNone, "", ""
		}, 1},
		{"READY without an observed version", func(i *providerv1.HostInventory) { i.Components[0].ObservedVersion = "" }, 1},
		{"non-READY without a reason", func(i *providerv1.HostInventory) { i.Components[1].StateReason = "" }, 1},
		{"reason of another state", func(i *providerv1.HostInventory) { i.Components[1].StateReason = ReasonNotInstalled }, 1},
		{"unlisted reason", func(i *providerv1.HostInventory) { i.Components[1].StateReason = "something_new" }, 1},
		{"UNMANIFESTED with an origin", func(i *providerv1.HostInventory) { i.Components[1].ManifestOrigin = originRO }, 1},
		{"origin NONE in a state that needs a manifest", func(i *providerv1.HostInventory) {
			i.Components[1].State, i.Components[1].StateReason, i.Components[1].RequiredRange = stIncompatible, ReasonVersionOutOfRange, ">=1"
		}, 1},
		{"origin NONE with a declared version", func(i *providerv1.HostInventory) { i.Components[1].DeclaredVersion = "1" }, 1},
		{"origin NONE with a source", func(i *providerv1.HostInventory) { i.Components[1].Source = "sysext" }, 1},
		{"manifested without a declared version", func(i *providerv1.HostInventory) { i.Components[0].DeclaredVersion = "" }, 1},
		{"INCOMPATIBLE without a range", func(i *providerv1.HostInventory) {
			i.Components[0].State, i.Components[0].StateReason = stIncompatible, ReasonVersionOutOfRange
		}, 1},
		{"range on a READY component", func(i *providerv1.HostInventory) { i.Components[0].RequiredRange = ">=1.0.0" }, 1},
		{"MISSING with a version", func(i *providerv1.HostInventory) {
			i.Components[0].State, i.Components[0].StateReason, i.Components[0].Files, i.Components[0].ArtifactSha256 =
				stMissing, ReasonNotInstalled, nil, nil
		}, 1},
		{"MISSING with files", func(i *providerv1.HostInventory) {
			i.Components[0].State, i.Components[0].StateReason, i.Components[0].ObservedVersion = stMissing, ReasonNotInstalled, ""
		}, 1},
		{"version charset", func(i *providerv1.HostInventory) { i.Components[0].ObservedVersion = "1 0" }, 1},
		{"version too long", func(i *providerv1.HostInventory) { i.Components[0].DeclaredVersion = strings.Repeat("1", 65) }, 1},
		{"range charset", func(i *providerv1.HostInventory) {
			i.Components[0].State, i.Components[0].StateReason, i.Components[0].RequiredRange = stIncompatible, ReasonVersionOutOfRange, ">=1;"
		}, 1},
		{"source charset", func(i *providerv1.HostInventory) { i.Components[0].Source = "Sys Ext" }, 1},
		{"artifact digest of 31 bytes", func(i *providerv1.HostInventory) { i.Components[0].ArtifactSha256 = make([]byte, 31) }, 1},
		{"file digest of 33 bytes", func(i *providerv1.HostInventory) { i.Components[0].Files[0].Sha256 = make([]byte, 33) }, 1},
		{"file without a digest", func(i *providerv1.HostInventory) { i.Components[0].Files[0].Sha256 = nil }, 1},
		{"files out of order", func(i *providerv1.HostInventory) {
			f := i.Components[0].Files
			f[0], f[1] = f[1], f[0]
		}, 1},
		{"file name with a slash", func(i *providerv1.HostInventory) { i.Components[0].Files[0].Name = "a/b" }, 1},
		{"file name with NUL", func(i *providerv1.HostInventory) { i.Components[0].Files[0].Name = "a\x00b" }, 1},
		{"file name with a trailing newline", func(i *providerv1.HostInventory) { i.Components[0].Files[0].Name = "a\n" }, 1},
		{"feature outside the probe set", func(i *providerv1.HostInventory) { i.Features[0].Name = "new_probe" }, 1},
		{"features out of order", func(i *providerv1.HostInventory) { i.Features[0], i.Features[1] = i.Features[1], i.Features[0] }, 1},
		{"duplicate feature", func(i *providerv1.HostInventory) { i.Features[1].Name = i.Features[0].Name }, 1},
		{"feature result unspecified", func(i *providerv1.HostInventory) { i.Features[0].Result = 0 }, 1},
		{"feature result beyond the set", func(i *providerv1.HostInventory) { i.Features[0].Result = 4 }, 1},
		{"ERROR without a reason", func(i *providerv1.HostInventory) { i.Features[1].Reason = "" }, 1},
		{"reason without ERROR", func(i *providerv1.HostInventory) { i.Features[0].Reason = FeatureReasonProbeFailed }, 1},
		{"feature reason outside the set", func(i *providerv1.HostInventory) { i.Features[1].Reason = "kaboom" }, 1},
		{"extensions out of order", func(i *providerv1.HostInventory) {
			e := i.UnexpectedExtensions
			e[0], e[1] = e[1], e[0]
		}, 1},
		{"extension kind unspecified", func(i *providerv1.HostInventory) { i.UnexpectedExtensions[0].Kind = 0 }, 1},
		{"extension kind beyond the set", func(i *providerv1.HostInventory) { i.UnexpectedExtensions[0].Kind = 4 }, 1},
		{"DIRECTORY with a digest", func(i *providerv1.HostInventory) { i.UnexpectedExtensions[0].ImageSha256 = make([]byte, 32) }, 1},
		{"image without a digest", func(i *providerv1.HostInventory) { i.UnexpectedExtensions[1].ImageSha256 = nil }, 1},
		{"extension digest of 16 bytes", func(i *providerv1.HostInventory) { i.UnexpectedExtensions[1].ImageSha256 = make([]byte, 16) }, 1},
		{"extension name with a slash", func(i *providerv1.HostInventory) { i.UnexpectedExtensions[0].Name = "../x" }, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := validInventory()
			tt.mut(inv)
			err := Validate(inv, tt.req)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalid)
		})
	}
	t.Run("nil", func(t *testing.T) { assert.ErrorIs(t, Validate(nil, 1), ErrInvalid) })
}

func TestValidateBounds(t *testing.T) {
	d := rawDigest([]byte("x"))
	t.Run("33 components", func(t *testing.T) {
		inv := validInventory()
		inv.Components = nil
		for i := range MaxComponents + 1 {
			inv.Components = append(inv.Components, &providerv1.HostComponent{Name: fmt.Sprintf("c%02d", i), State: stUnmanifested, StateReason: ReasonPresentUndeclared, ManifestOrigin: originNone})
		}
		assert.ErrorIs(t, Validate(inv, 1), ErrInvalid)
	})
	t.Run("9 files in a component", func(t *testing.T) {
		inv := validInventory()
		inv.Components[0].Files = nil
		for i := range MaxFilesPerComponent + 1 {
			inv.Components[0].Files = append(inv.Components[0].Files, &providerv1.ComponentFile{Name: fmt.Sprintf("f%d", i), Sha256: d})
		}
		assert.ErrorIs(t, Validate(inv, 1), ErrInvalid)
	})
	t.Run("65 files in all", func(t *testing.T) {
		inv := validInventory()
		inv.Components = nil
		for c := range 9 {
			comp := &providerv1.HostComponent{Name: fmt.Sprintf("c%d", c), State: stUnmanifested, StateReason: ReasonPresentUndeclared, ManifestOrigin: originNone}
			for i := range MaxFilesPerComponent {
				comp.Files = append(comp.Files, &providerv1.ComponentFile{Name: fmt.Sprintf("f%d", i), Sha256: d})
			}
			inv.Components = append(inv.Components, comp)
		}
		assert.ErrorIs(t, Validate(inv, 1), ErrInvalid)
	})
	t.Run("17 extensions", func(t *testing.T) {
		inv := validInventory()
		inv.UnexpectedExtensions = nil
		for i := range MaxExtensions + 1 {
			inv.UnexpectedExtensions = append(inv.UnexpectedExtensions, &providerv1.HostExtension{Name: fmt.Sprintf("e%02d", i), Kind: providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY})
		}
		assert.ErrorIs(t, Validate(inv, 1), ErrInvalid)
	})
	t.Run("33 features", func(t *testing.T) {
		inv := validInventory()
		inv.Features = nil
		for i := range MaxFeatures + 1 {
			inv.Features = append(inv.Features, &providerv1.HostFeature{Name: fmt.Sprintf("p%02d", i), Result: providerv1.FeatureResult_FEATURE_RESULT_PRESENT})
		}
		assert.ErrorIs(t, Validate(inv, 1), ErrInvalid)
	})
}

// maxInventory is a COMPLETE inventory at the largest the bounds allow: 32
// components at every string maximum with an artifact digest, 64 files in all,
// 32 feature entries and 16 extensions.
func maxInventory() *providerv1.HostInventory {
	d := rawDigest([]byte("x"))
	inv := &providerv1.HostInventory{
		State: providerv1.InventoryState_INVENTORY_STATE_COMPLETE, SnapshotAgeSeconds: 1<<32 - 1,
		ProcessIdentityTier: providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME,
	}
	longVersion := strings.Repeat("9", 64)
	for i := range MaxComponents {
		c := &providerv1.HostComponent{
			Name: fmt.Sprintf("c%s%02d", strings.Repeat("x", 60), i), State: stIncompatible, StateReason: ReasonVersionOutOfRange,
			ObservedVersion: longVersion, DeclaredVersion: longVersion, RequiredRange: strings.Repeat(">", 64),
			Source: strings.Repeat("s", 32), ManifestOrigin: originW, ArtifactSha256: d,
		}
		if i < MaxFiles/2 {
			for f := range 2 {
				c.Files = append(c.Files, &providerv1.ComponentFile{Name: fmt.Sprintf("%s%d", strings.Repeat("f", 63), f), Sha256: d})
			}
		}
		inv.Components = append(inv.Components, c)
	}
	for i := range MaxExtensions {
		inv.UnexpectedExtensions = append(inv.UnexpectedExtensions, &providerv1.HostExtension{
			Name: fmt.Sprintf("%s%02d", strings.Repeat("e", 62), i), Kind: providerv1.ExtensionKind_EXTENSION_KIND_UNDECLARED_IMAGE, ImageSha256: d,
		})
	}
	for _, name := range valueSets[1].features {
		inv.Features = append(inv.Features, &providerv1.HostFeature{Name: name, Result: providerv1.FeatureResult_FEATURE_RESULT_ERROR, Reason: FeatureReasonPermissionDenied})
	}
	sortInventory(inv)
	bind(inv, 1)
	return inv
}

func sortInventory(inv *providerv1.HostInventory) {
	inv.Features = sortedFeatures(inv.Features)
}

func sortedFeatures(fs []*providerv1.HostFeature) []*providerv1.HostFeature {
	out := append([]*providerv1.HostFeature(nil), fs...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func TestWorstCaseSize(t *testing.T) {
	inv := maxInventory()
	require.NoError(t, Validate(inv, 1))
	n := proto.Size(inv)
	assert.LessOrEqual(t, n, MaxInventoryBytes)
	assert.Greater(t, n, 10<<10, "a maximal report is large; the bound must hold with room to spare")
	t.Logf("maximal inventory: %d bytes (bound %d)", n, MaxInventoryBytes)
}
