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

package answer_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/inventory"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// iv1Inventory is the inventory of the published vector IV-1.
func iv1Inventory() *providerv1.HostInventory {
	empty := sha256.Sum256(nil)
	return &providerv1.HostInventory{
		InventoryVersion: 1, State: providerv1.InventoryState_INVENTORY_STATE_COMPLETE, SnapshotAgeSeconds: 30,
		Components: []*providerv1.HostComponent{{
			Name: "cloud-hypervisor", State: providerv1.ComponentState_COMPONENT_STATE_READY, ObservedVersion: "1.0.0", DeclaredVersion: "1.0.0",
			ManifestOrigin: providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY, ArtifactSha256: empty[:],
		}},
		ProcessIdentityTier: providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE, RequestedVersion: 1, SupportedVersion: 1,
	}
}

// TestInventoryVectorIV1 reproduces the published vector of an inventory
// inside the signed body from the generated types, and signs it through a
// test signer.
func TestInventoryVectorIV1(t *testing.T) {
	body := &providerv1.HostAnswer{AgentVersion: "v0.0.1", Inventory: iv1Inventory()}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)
	wantBody := "120676302e302e31" + "4a54" + "08011001181e" + "2246" + "0a10636c6f75642d68797065727669736f72" + "1001" + "2205312e302e30" +
		"2a05312e302e30" + "4002" + "4a20e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" + "3002" + "4801" + "5001"
	assert.Equal(t, wantBody, hex.EncodeToString(raw))
	assert.Len(t, raw, 94)

	m, err := answer.SignatureInput(answer.BodyTypeHostAnswer, raw)
	require.NoError(t, err)
	wantM := "766972747269676175642e686f73742d7369676e65642e763100" + "0016" + "70726f76696465722e76312e486f7374416e73776572" + "0000005e" + wantBody
	assert.Equal(t, wantM, hex.EncodeToString(m))
	assert.Len(t, m, 148)
	d := sha256.Sum256(m)
	assert.Equal(t, "43b3a3f66dc30637eea72a7b65bde3ee4f93c9844186838322a88971569be858", hex.EncodeToString(d[:]))

	// The inventory satisfies the field rules for a request with version 1,
	// and signs through a test signer (the body has no context: only a
	// verifier refuses it).
	require.NoError(t, inventory.Validate(body.GetInventory(), 1))
	p := trusttest.New(t)
	blob, err := answer.Sign(p.HostKey, leafDER(t, p), body)
	require.NoError(t, err)
	assert.Equal(t, raw, blob.GetBody(), "the signed bytes are the deterministic encoding of the vector")
	verify(t, blob, &providerv1.HostAnswer{})
}

func TestHostAnswerInventory(t *testing.T) {
	p := trusttest.New(t)
	valid := func() *providerv1.HostInventory { return iv1Inventory() }

	t.Run("asked and valid", func(t *testing.T) {
		h := testHost(p)
		h.InventoryRequested, h.Inventory = 1, valid()
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		assert.True(t, proto.Equal(valid(), a.GetInventory()))
		a.Inventory.Components[0].Name = "changed"
		assert.Equal(t, "cloud-hypervisor", h.Inventory.GetComponents()[0].GetName(), "the answer holds a copy")
		assert.EqualValues(t, 2, answer.AgentAPIVersion)
	})
	t.Run("asked, nothing to report", func(t *testing.T) {
		h := testHost(p)
		h.InventoryRequested = 1
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		assert.Nil(t, a.GetInventory(), "an answer without inventory stays valid for every other decision")
	})
	t.Run("not asked: never an inventory", func(t *testing.T) {
		h := testHost(p)
		h.Inventory = valid()
		_, err := answer.HostAnswer(h)
		assert.ErrorIs(t, err, answer.ErrInvalidInventory)
		h = testHost(p)
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		assert.Nil(t, a.GetInventory())
	})
	t.Run("echo and version rules", func(t *testing.T) {
		for name, edit := range map[string]func(*providerv1.HostInventory){
			"requested differs":         func(i *providerv1.HostInventory) { i.RequestedVersion = 2 },
			"inventory_version not min": func(i *providerv1.HostInventory) { i.InventoryVersion = 2 },
			"supported 0":               func(i *providerv1.HostInventory) { i.SupportedVersion = 0 },
			"unsorted components": func(i *providerv1.HostInventory) {
				i.Components = append(i.Components, &providerv1.HostComponent{Name: "a", State: providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED,
					StateReason: inventory.ReasonPresentUndeclared, ManifestOrigin: providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE})
			},
			"READY with a reason":  func(i *providerv1.HostInventory) { i.Components[0].StateReason = inventory.ReasonNotInstalled },
			"unlisted probe name":  func(i *providerv1.HostInventory) { i.Features = []*providerv1.HostFeature{{Name: "sneaky", Result: 1}} },
			"COMPLETE with reason": func(i *providerv1.HostInventory) { i.StateReason = inventory.ReasonIOError },
		} {
			h := testHost(p)
			h.InventoryRequested, h.Inventory = 1, valid()
			edit(h.Inventory)
			_, err := answer.HostAnswer(h)
			assert.ErrorIs(t, err, answer.ErrInvalidInventory, name)
		}
		// requested 5 with supported 1: version 1 content, 5 echoed.
		h := testHost(p)
		inv := inventory.OverLimit(5, inventory.ReasonTooManyFiles)
		h.InventoryRequested, h.Inventory = 5, inv
		a, err := answer.HostAnswer(h)
		require.NoError(t, err)
		assert.EqualValues(t, 5, a.GetInventory().GetRequestedVersion())
		assert.EqualValues(t, 1, a.GetInventory().GetInventoryVersion())
		h.InventoryRequested = 1
		_, err = answer.HostAnswer(h)
		assert.ErrorIs(t, err, answer.ErrInvalidInventory, "an echo of 5 for a request of 1 is a violation")
	})
}

// TestSignRefusesAnInvalidInventory: whatever built the body, Sign never
// signs an inventory that breaks a field rule.
func TestSignRefusesAnInvalidInventory(t *testing.T) {
	p := trusttest.New(t)
	bad := iv1Inventory()
	bad.Components[0].ArtifactSha256 = []byte{1, 2, 3}
	_, err := answer.Sign(p.HostKey, leafDER(t, p), &providerv1.HostAnswer{Inventory: bad})
	assert.ErrorIs(t, err, answer.ErrSign)
	assert.ErrorIs(t, err, answer.ErrInvalidInventory)
	assert.ErrorIs(t, err, inventory.ErrInvalid)
	zero := iv1Inventory()
	zero.RequestedVersion = 0
	_, err = answer.Sign(p.HostKey, leafDER(t, p), &providerv1.HostAnswer{Inventory: zero})
	assert.ErrorIs(t, err, answer.ErrInvalidInventory)
	// A body of another type is not looked into, and an answer without
	// inventory signs as before.
	_, err = answer.Sign(p.HostKey, leafDER(t, p), &providerv1.HostAnswer{AgentVersion: "x"})
	assert.NoError(t, err)
}

// TestMaximalInventorySigns: the largest report the bounds allow fits the body
// and blob limits with its answer context.
func TestMaximalInventorySigns(t *testing.T) {
	p := trusttest.New(t)
	d := sha256.Sum256([]byte("x"))
	inv := &providerv1.HostInventory{
		State: providerv1.InventoryState_INVENTORY_STATE_COMPLETE, SnapshotAgeSeconds: 1<<32 - 1,
		ProcessIdentityTier: providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME, RequestedVersion: 1, SupportedVersion: 1, InventoryVersion: 1,
	}
	for i := range inventory.MaxComponents {
		c := &providerv1.HostComponent{
			Name: fmt.Sprintf("c%s%02d", strings.Repeat("x", 60), i), State: providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE,
			StateReason: inventory.ReasonVersionOutOfRange, ObservedVersion: strings.Repeat("9", 64), DeclaredVersion: strings.Repeat("9", 64),
			RequiredRange: strings.Repeat(">", 64), Source: strings.Repeat("s", 32), ManifestOrigin: providerv1.ManifestOrigin_MANIFEST_ORIGIN_WRITABLE,
			ArtifactSha256: d[:],
		}
		if i < 32 {
			for f := range 2 {
				c.Files = append(c.Files, &providerv1.ComponentFile{Name: fmt.Sprintf("%s%d", strings.Repeat("f", 63), f), Sha256: d[:]})
			}
		}
		inv.Components = append(inv.Components, c)
	}
	h := testHost(p)
	h.InventoryRequested, h.Inventory = 1, inv
	a, err := answer.HostAnswer(h)
	require.NoError(t, err)
	blob, err := answer.Sign(p.HostKey, leafDER(t, p), a)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(blob.GetBody()), answer.MaxBodyBytes)
	assert.LessOrEqual(t, proto.Size(blob), answer.MaxBlobBytes)
	got := &providerv1.HostAnswer{}
	verify(t, blob, got)
	assert.True(t, proto.Equal(inv, got.GetInventory()))
	assert.True(t, bytes.Equal(h.Nonce, got.GetContext().GetNonce()))
}
