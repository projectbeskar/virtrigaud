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
package journal_test

import (
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	"google.golang.org/protobuf/proto"
)

func FuzzParseAnchorLog(f *testing.F) {
	rec := journal.AnchorRecord{Type: journal.AnchorCommit, Seq: 7}
	q := journal.AnchorRecord{Type: journal.AnchorQuarantine, Reason: journal.LostRolledBack, Seq: 8}
	f.Add([]byte{})
	f.Add(rec.Encode())
	f.Add(append(rec.Encode(), q.Encode()...))
	f.Add(append(rec.Encode(), make([]byte, journal.AnchorRecordSize)...))
	f.Add(append(rec.Encode(), 1, 2, 3))
	f.Fuzz(func(t *testing.T, raw []byte) {
		l, err := journal.ParseAnchorLog(raw)
		if err != nil {
			return
		}
		if l.ValidLen%journal.AnchorRecordSize != 0 || l.ValidLen > len(raw) {
			t.Fatalf("valid length %d of %d", l.ValidLen, len(raw))
		}
		if len(l.Records)*journal.AnchorRecordSize != l.ValidLen {
			t.Fatalf("%d records for %d bytes", len(l.Records), l.ValidLen)
		}
		if l.TornTail != (l.ValidLen < len(raw)) {
			t.Fatalf("torn tail %v with %d of %d bytes", l.TornTail, l.ValidLen, len(raw))
		}
		if len(raw)-l.ValidLen >= 2*journal.AnchorRecordSize {
			t.Fatal("more than one invalid record accepted as a torn tail")
		}
		for _, r := range l.Records {
			again, err := journal.DecodeAnchorRecord(r.Encode())
			if err != nil || again != r {
				t.Fatalf("record does not round-trip: %+v", r)
			}
		}
	})
}

func FuzzDecodeStoreMeta(f *testing.F) {
	b, _ := proto.Marshal(&journalpb.StoreMeta{StoreId: make([]byte, 16), SchemaVersion: 1, MinReaderVersion: 1})
	f.Add(b)
	f.Add([]byte{0xff, 0xff})
	f.Fuzz(func(t *testing.T, raw []byte) {
		m := &journalpb.StoreMeta{}
		if err := proto.Unmarshal(raw, m); err != nil {
			return
		}
		if _, err := proto.Marshal(m); err != nil {
			t.Fatalf("decoded meta does not re-encode: %v", err)
		}
	})
}
