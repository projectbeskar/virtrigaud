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
package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// chainDomain separates the chain-head hash from every other SHA-256 use.
const chainDomain = "vrtg-journal-v2"

// Write operations recorded in a transaction digest.
const (
	writePut          byte = 1
	writeDelete       byte = 2
	writeCreateBucket byte = 3
)

// write is one write of a transaction, as written.
type write struct {
	op     byte
	bucket string
	key    []byte
	value  []byte
}

// txDigest is SHA-256 over the transaction's writes, sorted by bucket and
// key, with the last write to a key winning. Each write is encoded as
// be32(len(bucket)) || bucket || be32(len(key)) || key || op ||
// be32(len(value)) || value, over the exact bytes written (never
// re-encoded).
func txDigest(writes []write) [32]byte {
	last := map[string]int{}
	for i, w := range writes {
		last[w.bucket+"\x00"+string(w.key)] = i
	}
	idx := make([]int, 0, len(last))
	for _, i := range last {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool {
		wa, wb := writes[idx[a]], writes[idx[b]]
		if wa.bucket != wb.bucket {
			return wa.bucket < wb.bucket
		}
		return bytes.Compare(wa.key, wb.key) < 0
	})
	h := sha256.New()
	var n [4]byte
	for _, i := range idx {
		w := writes[i]
		binary.BigEndian.PutUint32(n[:], uint32(len(w.bucket))) // #nosec G115 -- bucket names are short constants
		h.Write(n[:])
		h.Write([]byte(w.bucket))
		binary.BigEndian.PutUint32(n[:], uint32(len(w.key))) // #nosec G115 -- bbolt keys are at most 32 KiB
		h.Write(n[:])
		h.Write(w.key)
		h.Write([]byte{w.op})
		binary.BigEndian.PutUint32(n[:], uint32(len(w.value))) // #nosec G115 -- bbolt values are below 4 GiB
		h.Write(n[:])
		h.Write(w.value)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// chainEntrySize is the size of a stored chain entry: digest || state || head.
const chainEntrySize = 96

// nextHead is SHA-256("vrtg-journal-v2" || prev || be64(seq) || digest ||
// state), where digest is the transaction digest and state the live-state
// digest after the commit. The head therefore commits to the state: a store
// whose recorded state digest was edited no longer verifies.
func nextHead(prev [32]byte, seq uint64, digest, state [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(chainDomain))
	h.Write(prev[:])
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], seq)
	h.Write(s[:])
	h.Write(digest[:])
	h.Write(state[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// seqKey is the big-endian key of a chain position.
func seqKey(seq uint64) []byte {
	var k [8]byte
	binary.BigEndian.PutUint64(k[:], seq)
	return k[:]
}

// chainEntry is the value stored per chain position: digest || state || head.
func chainEntry(digest, state, head [32]byte) []byte {
	v := make([]byte, chainEntrySize)
	copy(v[:32], digest[:])
	copy(v[32:64], state[:])
	copy(v[64:], head[:])
	return v
}
