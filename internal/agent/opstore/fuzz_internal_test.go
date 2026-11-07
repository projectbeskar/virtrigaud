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

package opstore

import (
	"testing"

	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// Record decoding never panics, and a decoded record re-encodes and can be
// viewed.
func FuzzDecodeRecord(f *testing.F) {
	id := &pb.OpIdentity{ProviderUid: "p", EnrolmentId: make([]byte, 16), OpId: make([]byte, 16), VmUid: "vm",
		BindingId: make([]byte, 16), Kind: 1, Attempt: 1, IntentDigest: make([]byte, 32), MintEpoch: &pb.Epoch{Counter: 2}}
	rec, _ := marshal.Marshal(&pb.OpRecord{Identity: id, State: StateExecuting, Steps: []*pb.Step{{Name: "a", Once: true, Intent: true}}})
	tomb, _ := marshal.Marshal(&pb.Tombstone{Identity: id, State: StateCompleted, Irrev: true})
	vm, _ := marshal.Marshal(&pb.VMRecord{VmUid: "vm", BindingId: make([]byte, 16)})
	f.Add(rec)
	f.Add(tomb)
	f.Add(vm)
	f.Add([]byte{0x0a, 0xff, 0xff, 0xff, 0xff, 0x0f})
	f.Fuzz(func(t *testing.T, raw []byte) {
		r := &pb.OpRecord{}
		if unmarshalRecord(raw, r) == nil {
			_ = viewOfRecord(r)
			if _, err := marshal.Marshal(r); err != nil {
				t.Fatalf("decoded record does not re-encode: %v", err)
			}
			_ = Terminal(r.GetState())
			_ = anyIntent(r)
		}
		tb := &pb.Tombstone{}
		if unmarshalRecord(raw, tb) == nil {
			_ = viewOfTombstone(tb)
		}
		v := &pb.VMRecord{}
		_ = unmarshalRecord(raw, v)
		e := &pb.Enrolment{}
		_ = unmarshalRecord(raw, e)
		m := &pb.AgentMeta{}
		_ = unmarshalRecord(raw, m)
	})
}
