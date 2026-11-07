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
	"encoding/binary"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// checkpoint records the head at seq as a checkpoint and prunes chain entries
// older than the previous checkpoint. Checkpoints themselves are kept for the
// life of the store (32 bytes per interval), so that an external party can
// later ask whether an older position lies on this store's chain.
func checkpoint(btx *bolt.Tx, seq uint64, head [32]byte, every uint64) error {
	if err := btx.Bucket([]byte(bucketCheckpoints)).Put(seqKey(seq), append([]byte(nil), head[:]...)); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if seq < 2*every {
		return nil
	}
	keepFrom := seq - every
	chain := btx.Bucket([]byte(bucketChain))
	c := chain.Cursor()
	var stale [][]byte
	for k, _ := c.First(); k != nil && binary.BigEndian.Uint64(k) < keepFrom; k, _ = c.Next() {
		stale = append(stale, append([]byte(nil), k...))
	}
	for _, k := range stale {
		if err := chain.Delete(k); err != nil {
			return fmt.Errorf("prune chain entry: %w", err)
		}
	}
	return nil
}

// lastCheckpoint returns the newest checkpoint at or below seq.
func lastCheckpoint(btx *bolt.Tx, seq uint64) (uint64, [32]byte, bool) {
	var head [32]byte
	c := btx.Bucket([]byte(bucketCheckpoints)).Cursor()
	k, v := c.Seek(seqKey(seq))
	if k == nil || binary.BigEndian.Uint64(k) > seq {
		k, v = c.Prev()
	}
	if k == nil || len(v) != 32 {
		return 0, head, false
	}
	copy(head[:], v)
	return binary.BigEndian.Uint64(k), head, true
}

// headAt returns the head the store recorded at seq, from its chain entry or
// its checkpoint.
func headAt(btx *bolt.Tx, seq uint64) ([32]byte, bool) {
	var head [32]byte
	if v := btx.Bucket([]byte(bucketChain)).Get(seqKey(seq)); len(v) == 64 {
		copy(head[:], v[32:])
		return head, true
	}
	if v := btx.Bucket([]byte(bucketCheckpoints)).Get(seqKey(seq)); len(v) == 32 {
		copy(head[:], v)
		return head, true
	}
	return head, false
}

// errChainBroken reports a chain that does not verify.
var errChainBroken = errors.New("journal chain does not verify")

// verifyChain recomputes the chain from the last checkpoint at or below
// pos.Seq and compares every stored head and the position's head.
func verifyChain(btx *bolt.Tx, pos Position) error {
	cpSeq, head, ok := lastCheckpoint(btx, pos.Seq)
	if !ok {
		return fmt.Errorf("%w: no checkpoint", errChainBroken)
	}
	chain := btx.Bucket([]byte(bucketChain))
	for seq := cpSeq + 1; seq <= pos.Seq; seq++ {
		v := chain.Get(seqKey(seq))
		if len(v) != 64 {
			return fmt.Errorf("%w: entry %d missing", errChainBroken, seq)
		}
		var digest, stored [32]byte
		copy(digest[:], v[:32])
		copy(stored[:], v[32:])
		head = nextHead(head, seq, digest)
		if head != stored {
			return fmt.Errorf("%w: head mismatch at %d", errChainBroken, seq)
		}
	}
	if head != pos.Head {
		return fmt.Errorf("%w: position head mismatch", errChainBroken)
	}
	return nil
}
