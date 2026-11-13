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

package providerv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// These tests pin the host inventory wire shape: the messages, enums and field
// numbers of HostAnswer.inventory and GetHostInfoRequest.inventory_version
// are a frozen contract, and the encoding vectors below are published in
// docs/operation-grants-wire-shape.md.

const (
	hostInventoryMessageName = "provider.v1.HostInventory"
	hostComponentMessageName = "provider.v1.HostComponent"
	componentFileMessageName = "provider.v1.ComponentFile"
	hostFeatureMessageName   = "provider.v1.HostFeature"
	hostExtensionMessageName = "provider.v1.HostExtension"

	inventoryStateEnumName       = "provider.v1.InventoryState"
	componentStateEnumName       = "provider.v1.ComponentState"
	manifestOriginEnumName       = "provider.v1.ManifestOrigin"
	featureResultEnumName        = "provider.v1.FeatureResult"
	processIdentityTierEnumName  = "provider.v1.ProcessIdentityTier"
	extensionKindEnumName        = "provider.v1.ExtensionKind"
	maxInventoryBytesForTesting  = 32 << 10
	maximalInventoryEncodedBytes = 25532
)

func fullComponentFile() *ComponentFile {
	return &ComponentFile{Name: "cloud-hypervisor", Sha256: fill(0x31, 32)}
}

func fullHostComponent() *HostComponent {
	return &HostComponent{
		Name:            "cloud-hypervisor",
		State:           ComponentState_COMPONENT_STATE_UNHEALTHY,
		StateReason:     "file_digest_mismatch",
		ObservedVersion: "1.0.0",
		DeclaredVersion: "1.0.0",
		RequiredRange:   ">=1.0.0, <2.0.0",
		Source:          "sysext",
		ManifestOrigin:  ManifestOrigin_MANIFEST_ORIGIN_WRITABLE,
		ArtifactSha256:  fill(0x32, 32),
		Files:           []*ComponentFile{fullComponentFile()},
	}
}

func fullHostFeature() *HostFeature {
	return &HostFeature{Name: "pidfd", Result: FeatureResult_FEATURE_RESULT_ERROR, Reason: "probe_failed"}
}

func fullHostExtension() *HostExtension {
	return &HostExtension{Name: "extra.raw", Kind: ExtensionKind_EXTENSION_KIND_UNDECLARED_IMAGE, ImageSha256: fill(0x33, 32)}
}

func fullHostInventory() *HostInventory {
	return &HostInventory{
		InventoryVersion:     1,
		State:                InventoryState_INVENTORY_STATE_COMPLETE,
		SnapshotAgeSeconds:   30,
		Components:           []*HostComponent{fullHostComponent()},
		Features:             []*HostFeature{fullHostFeature()},
		ProcessIdentityTier:  ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE,
		UnexpectedExtensions: []*HostExtension{fullHostExtension()},
		StateReason:          "probe_failed",
		RequestedVersion:     1,
		SupportedVersion:     1,
	}
}

func inventoryMessages() []proto.Message {
	return []proto.Message{
		fullHostInventory(),
		fullHostComponent(),
		fullComponentFile(),
		fullHostFeature(),
		fullHostExtension(),
	}
}

func TestHostInventory_MessagesRoundTrip(t *testing.T) {
	for _, m := range inventoryMessages() {
		assertEveryFieldSet(t, m)
		roundTrip(t, m)
	}
	ha := fullHostAnswer()
	assertEveryFieldSet(t, ha)
	roundTrip(t, ha)

	// Unset: the new request field reads as zero.
	if got := roundTrip(t, &GetHostInfoRequest{HostId: "h"}).GetInventoryVersion(); got != 0 {
		t.Errorf("unset inventory_version = %d, want 0", got)
	}
	req := roundTrip(t, &GetHostInfoRequest{HostId: "h", AnswerNonce: fill(0x01, 16), InventoryVersion: 7})
	if req.GetInventoryVersion() != 7 {
		t.Errorf("inventory_version = %d, want 7", req.GetInventoryVersion())
	}
	if (&HostAnswer{}).GetInventory() != nil {
		t.Error("an unset inventory is not nil")
	}
}

func TestHostInventory_UnknownFieldsObservable(t *testing.T) {
	extra := protowire.AppendTag(nil, 99, protowire.VarintType)
	extra = protowire.AppendVarint(extra, 1)
	for _, m := range inventoryMessages() {
		raw, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		out := m.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(append(raw, extra...), out); err != nil {
			t.Fatalf("unmarshal %s with field 99: %v", m.ProtoReflect().Descriptor().FullName(), err)
		}
		if len(out.ProtoReflect().GetUnknown()) == 0 {
			t.Errorf("%s: unknown field 99 is not observable", m.ProtoReflect().Descriptor().FullName())
		}
	}
	// An unknown field inside the nested inventory of a HostAnswer is
	// observable on the nested message, not lost.
	inv := fullHostInventory()
	inv.ProtoReflect().SetUnknown(extra)
	raw, err := proto.Marshal(&HostAnswer{Inventory: inv})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out HostAnswer
	if err := proto.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.GetInventory().ProtoReflect().GetUnknown()) == 0 {
		t.Error("unknown field inside the nested inventory is not observable")
	}
}

func TestHostInventory_FieldNumbersPinned(t *testing.T) {
	checkFields(t, (&HostInventory{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"inventory_version", 1, protoreflect.Uint32Kind, "", false},
		{"state", 2, protoreflect.EnumKind, inventoryStateEnumName, false},
		{"snapshot_age_seconds", 3, protoreflect.Uint32Kind, "", false},
		{"components", 4, protoreflect.MessageKind, hostComponentMessageName, true},
		{"features", 5, protoreflect.MessageKind, hostFeatureMessageName, true},
		{"process_identity_tier", 6, protoreflect.EnumKind, processIdentityTierEnumName, false},
		{"unexpected_extensions", 7, protoreflect.MessageKind, hostExtensionMessageName, true},
		{"state_reason", 8, protoreflect.StringKind, "", false},
		{"requested_version", 9, protoreflect.Uint32Kind, "", false},
		{"supported_version", 10, protoreflect.Uint32Kind, "", false},
	}, true)
	checkFields(t, (&HostComponent{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"name", 1, protoreflect.StringKind, "", false},
		{"state", 2, protoreflect.EnumKind, componentStateEnumName, false},
		{"state_reason", 3, protoreflect.StringKind, "", false},
		{"observed_version", 4, protoreflect.StringKind, "", false},
		{"declared_version", 5, protoreflect.StringKind, "", false},
		{"required_range", 6, protoreflect.StringKind, "", false},
		{"source", 7, protoreflect.StringKind, "", false},
		{"manifest_origin", 8, protoreflect.EnumKind, manifestOriginEnumName, false},
		{"artifact_sha256", 9, protoreflect.BytesKind, "", false},
		{"files", 10, protoreflect.MessageKind, componentFileMessageName, true},
	}, true)
	checkFields(t, (&ComponentFile{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"name", 1, protoreflect.StringKind, "", false},
		{"sha256", 2, protoreflect.BytesKind, "", false},
	}, true)
	checkFields(t, (&HostFeature{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"name", 1, protoreflect.StringKind, "", false},
		{"result", 2, protoreflect.EnumKind, featureResultEnumName, false},
		{"reason", 3, protoreflect.StringKind, "", false},
	}, true)
	checkFields(t, (&HostExtension{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"name", 1, protoreflect.StringKind, "", false},
		{"kind", 2, protoreflect.EnumKind, extensionKindEnumName, false},
		{"image_sha256", 3, protoreflect.BytesKind, "", false},
	}, true)
}

func TestHostInventory_EnumValuesPinned(t *testing.T) {
	checkEnum(t, InventoryState(0).Descriptor(), []protoreflect.Name{
		"INVENTORY_STATE_UNSPECIFIED",
		"INVENTORY_STATE_COMPLETE",
		"INVENTORY_STATE_PENDING",
		"INVENTORY_STATE_OVER_LIMIT",
		"INVENTORY_STATE_UNREADABLE",
	})
	checkEnum(t, ComponentState(0).Descriptor(), []protoreflect.Name{
		"COMPONENT_STATE_UNSPECIFIED",
		"COMPONENT_STATE_READY",
		"COMPONENT_STATE_MISSING",
		"COMPONENT_STATE_INCOMPATIBLE",
		"COMPONENT_STATE_UNHEALTHY",
		"COMPONENT_STATE_UNMANIFESTED",
		"COMPONENT_STATE_MANIFEST_INVALID",
	})
	checkEnum(t, ManifestOrigin(0).Descriptor(), []protoreflect.Name{
		"MANIFEST_ORIGIN_UNSPECIFIED",
		"MANIFEST_ORIGIN_NONE",
		"MANIFEST_ORIGIN_READ_ONLY",
		"MANIFEST_ORIGIN_WRITABLE",
	})
	checkEnum(t, FeatureResult(0).Descriptor(), []protoreflect.Name{
		"FEATURE_RESULT_UNSPECIFIED",
		"FEATURE_RESULT_PRESENT",
		"FEATURE_RESULT_ABSENT",
		"FEATURE_RESULT_ERROR",
	})
	checkEnum(t, ProcessIdentityTier(0).Descriptor(), []protoreflect.Name{
		"PROCESS_IDENTITY_TIER_UNSPECIFIED",
		"PROCESS_IDENTITY_TIER_PIDFD_STORE",
		"PROCESS_IDENTITY_TIER_PIDFS_HANDLE",
		"PROCESS_IDENTITY_TIER_PIDFS_INODE",
		"PROCESS_IDENTITY_TIER_START_TIME",
	})
	checkEnum(t, ExtensionKind(0).Descriptor(), []protoreflect.Name{
		"EXTENSION_KIND_UNSPECIFIED",
		"EXTENSION_KIND_DIRECTORY",
		"EXTENSION_KIND_UNDECLARED_IMAGE",
		"EXTENSION_KIND_OVERLAYS_COMPONENT",
	})
}

// TestHostInventory_BodyTypesUnchanged confirms the new messages added no
// signed body type: the closed set stays the three published strings.
func TestHostInventory_BodyTypesUnchanged(t *testing.T) {
	if len(hostSignedBodyTypes) != 3 {
		t.Fatalf("body type set has %d members, want 3", len(hostSignedBodyTypes))
	}
	if got := string((&HostAnswer{}).ProtoReflect().Descriptor().FullName()); got != "provider.v1.HostAnswer" {
		t.Errorf("HostAnswer full name = %q", got)
	}
}

// TestHostInventory_MaximalEncodedSize builds an inventory at every documented
// bound and pins its encoded size under the 32 KiB limit.
func TestHostInventory_MaximalEncodedSize(t *testing.T) {
	inv := &HostInventory{
		InventoryVersion:    1<<32 - 1,
		State:               InventoryState_INVENTORY_STATE_COMPLETE,
		SnapshotAgeSeconds:  1<<32 - 1,
		ProcessIdentityTier: ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME,
		RequestedVersion:    1<<32 - 1,
		SupportedVersion:    1<<32 - 1,
	}
	for i := 0; i < 32; i++ {
		c := &HostComponent{
			Name:            strings.Repeat("c", 63),
			State:           ComponentState_COMPONENT_STATE_UNHEALTHY,
			StateReason:     strings.Repeat("r", 64),
			ObservedVersion: strings.Repeat("o", 64),
			DeclaredVersion: strings.Repeat("d", 64),
			RequiredRange:   strings.Repeat(">", 64),
			Source:          strings.Repeat("s", 32),
			ManifestOrigin:  ManifestOrigin_MANIFEST_ORIGIN_WRITABLE,
			ArtifactSha256:  fill(0xa1, 32),
		}
		for j := 0; j < 2; j++ {
			c.Files = append(c.Files, &ComponentFile{Name: strings.Repeat("f", 64), Sha256: fill(0xa2, 32)})
		}
		inv.Components = append(inv.Components, c)
	}
	for i := 0; i < 32; i++ {
		inv.Features = append(inv.Features, &HostFeature{
			Name: strings.Repeat("n", 64), Result: FeatureResult_FEATURE_RESULT_ERROR, Reason: strings.Repeat("e", 64),
		})
	}
	for i := 0; i < 16; i++ {
		inv.UnexpectedExtensions = append(inv.UnexpectedExtensions, &HostExtension{
			Name: strings.Repeat("x", 64), Kind: ExtensionKind_EXTENSION_KIND_UNDECLARED_IMAGE, ImageSha256: fill(0xa3, 32),
		})
	}
	roundTrip(t, inv)
	raw, err := proto.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw) != maximalInventoryEncodedBytes {
		t.Errorf("maximal inventory encodes to %d bytes, want %d", len(raw), maximalInventoryEncodedBytes)
	}
	if len(raw) > maxInventoryBytesForTesting {
		t.Errorf("maximal inventory is %d bytes, over %d", len(raw), maxInventoryBytesForTesting)
	}
}

// ivInventoryBody builds the signed body of the published inventory vector.
func ivInventoryBody(t *testing.T) []byte {
	t.Helper()
	empty := sha256.Sum256(nil)
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(&HostAnswer{
		AgentVersion: "v0.0.1",
		Inventory: &HostInventory{
			InventoryVersion:   1,
			State:              InventoryState_INVENTORY_STATE_COMPLETE,
			SnapshotAgeSeconds: 30,
			Components: []*HostComponent{{
				Name:            "cloud-hypervisor",
				State:           ComponentState_COMPONENT_STATE_READY,
				ObservedVersion: "1.0.0",
				DeclaredVersion: "1.0.0",
				ManifestOrigin:  ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY,
				ArtifactSha256:  empty[:],
			}},
			ProcessIdentityTier: ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE,
			RequestedVersion:    1,
			SupportedVersion:    1,
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// TestHostInventory_SignatureInputVector reproduces the published encoding of
// an inventory inside the signed body (not a complete answer: no context) and
// its signature input, and confirms the framing vector of the host answer is
// unchanged.
func TestHostInventory_SignatureInputVector(t *testing.T) {
	const wantBody = "120676302e302e31" + "4a54" + "08011001181e" + "2246" +
		"0a10636c6f75642d68797065727669736f72" + "1001" + "2205312e302e30" + "2a05312e302e30" + "4002" +
		"4a20e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" +
		"3002" + "4801" + "5001"
	body := ivInventoryBody(t)
	if got := hex.EncodeToString(body); got != wantBody {
		t.Fatalf("inventory vector body = %s, want %s", got, wantBody)
	}
	if len(body) != 94 {
		t.Errorf("body is %d bytes, want 94", len(body))
	}

	bodyType := string((&HostAnswer{}).ProtoReflect().Descriptor().FullName())
	if len(bodyType) != 22 {
		t.Errorf("body_type is %d bytes, want 22", len(bodyType))
	}
	input := append([]byte(hostSignedDomainSeparation), 0x00)
	input = binary.BigEndian.AppendUint16(input, uint16(len(bodyType)))
	input = append(input, bodyType...)
	input = binary.BigEndian.AppendUint32(input, uint32(len(body)))
	input = append(input, body...)

	const wantPrefix = "766972747269676175642e686f73742d7369676e65642e763100" +
		"0016" + "70726f76696465722e76312e486f7374416e73776572" + "0000005e"
	if got := hex.EncodeToString(input[:len(input)-len(body)]); got != wantPrefix {
		t.Errorf("signature input prefix = %s, want %s", got, wantPrefix)
	}
	if len(input) != 148 {
		t.Errorf("signature input is %d bytes, want 148", len(input))
	}
	digest := sha256.Sum256(input)
	if got, want := hex.EncodeToString(digest[:]), "43b3a3f66dc30637eea72a7b65bde3ee4f93c9844186838322a88971569be858"; got != want {
		t.Errorf("SHA-256(signature input) = %s, want %s", got, want)
	}

	// The published framing vector (a body of field 2 only) is unchanged.
	plain, err := proto.Marshal(&HostAnswer{AgentVersion: "v0.0.1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	framed := append([]byte(hostSignedDomainSeparation), 0x00)
	framed = binary.BigEndian.AppendUint16(framed, uint16(len(bodyType)))
	framed = append(framed, bodyType...)
	framed = binary.BigEndian.AppendUint32(framed, uint32(len(plain)))
	framed = append(framed, plain...)
	framedDigest := sha256.Sum256(framed)
	if got, want := hex.EncodeToString(framedDigest[:]), "92b2ed124ec68b2661cef2f6188a1f937121db3f0d25bb0ae8b98319bd696cea"; got != want {
		t.Errorf("framing vector digest = %s, want %s", got, want)
	}
}

// TestHostInventory_RequestVector reproduces the published encoding of a
// GetHostInfoRequest that asks for an inventory.
func TestHostInventory_RequestVector(t *testing.T) {
	nonce := make([]byte, 16)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&GetHostInfoRequest{AnswerNonce: nonce, InventoryVersion: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := hex.EncodeToString(raw), "1210000102030405060708090a0b0c0d0e0f"+"2001"; got != want {
		t.Errorf("request vector = %s, want %s", got, want)
	}
	if !bytes.HasSuffix(raw, []byte{0x20, 0x01}) {
		t.Error("inventory_version is not field 4 varint")
	}
}
