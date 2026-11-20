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

package providerv1

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// These tests pin the store capacity wire shape: StoreCapacity, carried in
// HostAnswer.store_capacity, asked for by
// GetHostInfoRequest.store_capacity_version.

const (
	storeCapacityMessageName = "provider.v1.StoreCapacity"
	storeLevelEnumName       = "provider.v1.StoreLevel"
)

func fullStoreCapacity() *StoreCapacity {
	return &StoreCapacity{LiveBytes: 200 << 20, ThresholdBytes: 256 << 20, Level: StoreLevel_STORE_LEVEL_WARNING, RequestedVersion: 1, Version: 1}
}

func TestStoreCapacity_RoundTrip(t *testing.T) {
	c := fullStoreCapacity()
	assertEveryFieldSet(t, c)
	if got := roundTrip(t, c); !proto.Equal(got, c) {
		t.Errorf("round trip = %v, want %v", got, c)
	}
	if got := roundTrip(t, &GetHostInfoRequest{HostId: "h"}).GetStoreCapacityVersion(); got != 0 {
		t.Errorf("unset store_capacity_version = %d, want 0", got)
	}
	if got := roundTrip(t, &GetHostInfoRequest{HostId: "h", AnswerNonce: fill(0x01, 16), StoreCapacityVersion: 1}).GetStoreCapacityVersion(); got != 1 {
		t.Errorf("store_capacity_version = %d, want 1", got)
	}
	if (&HostAnswer{}).GetStoreCapacity() != nil {
		t.Error("an unset store capacity is not nil")
	}
	if got := roundTrip(t, &HostAnswer{StoreCapacity: c}).GetStoreCapacity(); !proto.Equal(got, c) {
		t.Errorf("HostAnswer.store_capacity round trip = %v, want %v", got, c)
	}
}

func TestStoreCapacity_FieldNumbersPinned(t *testing.T) {
	checkFields(t, (&StoreCapacity{}).ProtoReflect().Descriptor(), []fieldSpec{
		{"live_bytes", 1, protoreflect.Uint64Kind, "", false},
		{"threshold_bytes", 2, protoreflect.Uint64Kind, "", false},
		{"level", 3, protoreflect.EnumKind, storeLevelEnumName, false},
		{"requested_version", 4, protoreflect.Uint32Kind, "", false},
		{"version", 5, protoreflect.Uint32Kind, "", false},
	}, true)
	if got := string((&StoreCapacity{}).ProtoReflect().Descriptor().FullName()); got != storeCapacityMessageName {
		t.Errorf("StoreCapacity full name = %q", got)
	}
	checkEnum(t, StoreLevel(0).Descriptor(), []protoreflect.Name{
		"STORE_LEVEL_UNSPECIFIED",
		"STORE_LEVEL_OK",
		"STORE_LEVEL_WARNING",
		"STORE_LEVEL_FULL",
		"STORE_LEVEL_NOT_MEASURED",
	})
}

// TestStoreCapacity_InSignedBody: the capacity is part of the HostAnswer body,
// the exact bytes a host signs (SignedBlob.body, the tail of the signature
// input), so any change to it changes what is signed; and a body without it encodes exactly as before, so an
// answer to a request that did not ask for it is byte-for-byte what an older
// agent signs.
func TestStoreCapacity_InSignedBody(t *testing.T) {
	det := proto.MarshalOptions{Deterministic: true}
	base := &HostAnswer{Context: fullAnswerContext(), AgentVersion: "v0.0.1", AgentApiVersion: 1, GrantVersions: []uint32{1},
		Mode: AgentMode_AGENT_MODE_IDENTITY}
	plain, err := det.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	with := proto.CloneOf(base)
	with.StoreCapacity = fullStoreCapacity()
	signed, err := det.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(signed, plain) {
		t.Fatal("the capacity does not follow the older fields in the deterministic body")
	}
	num, typ, n := protowire.ConsumeTag(signed[len(plain):])
	if n < 0 || num != 11 || typ != protowire.BytesType {
		t.Fatalf("the body's trailing field is %d/%d, want store_capacity (11, length-delimited)", num, typ)
	}
	for _, mutate := range []func(*StoreCapacity){
		func(c *StoreCapacity) { c.LiveBytes++ },
		func(c *StoreCapacity) { c.ThresholdBytes-- },
		func(c *StoreCapacity) { c.Level = StoreLevel_STORE_LEVEL_OK },
		func(c *StoreCapacity) { c.RequestedVersion = 0 },
		func(c *StoreCapacity) { c.Version = 2 },
	} {
		m := proto.CloneOf(with)
		mutate(m.StoreCapacity)
		b, err := det.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(signed, b) {
			t.Error("a changed capacity left the signed body unchanged")
		}
	}
}
