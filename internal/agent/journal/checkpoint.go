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
	"encoding/binary"
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// checkpointSize is the size of a stored checkpoint: head || previous head.
// The previous head lets the newest step be recomputed from the checkpoint's
// own chain entry once the entries before it are pruned, which authenticates
// the state digest that entry holds.
const checkpointSize = 64

// checkpoint records the head at seq (and the head before it) as a checkpoint
// and prunes chain entries older than the previous checkpoint. Checkpoints
// themselves are kept for the life of the store (64 bytes per interval), so
// that an external party can later ask whether an older position lies on this
// store's chain.
func checkpoint(btx *bolt.Tx, seq uint64, head, prev [32]byte, every uint64) error {
	cp := make([]byte, checkpointSize)
	copy(cp[:32], head[:])
	copy(cp[32:], prev[:])
	if err := btx.Bucket([]byte(bucketCheckpoints)).Put(seqKey(seq), cp); err != nil {
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
	cp, head, _, ok := lastCheckpointFull(btx, seq)
	return cp, head, ok
}

// lastCheckpointFull is lastCheckpoint with the head before the checkpoint.
func lastCheckpointFull(btx *bolt.Tx, seq uint64) (cpSeq uint64, head, prev [32]byte, ok bool) {
	c := btx.Bucket([]byte(bucketCheckpoints)).Cursor()
	k, v := c.Seek(seqKey(seq))
	if k == nil || binary.BigEndian.Uint64(k) > seq {
		k, v = c.Prev()
	}
	if k == nil || len(v) != checkpointSize {
		return 0, head, prev, false
	}
	copy(head[:], v[:32])
	copy(prev[:], v[32:])
	return binary.BigEndian.Uint64(k), head, prev, true
}

// headAt returns the head the store recorded at seq, from its chain entry or
// its checkpoint.
func headAt(btx *bolt.Tx, seq uint64) ([32]byte, bool) {
	var head [32]byte
	if v := btx.Bucket([]byte(bucketChain)).Get(seqKey(seq)); len(v) == chainEntrySize {
		copy(head[:], v[64:])
		return head, true
	}
	if v := btx.Bucket([]byte(bucketCheckpoints)).Get(seqKey(seq)); len(v) == checkpointSize {
		copy(head[:], v[:32])
		return head, true
	}
	return head, false
}

// errChainBroken reports a chain that does not verify.
var errChainBroken = errors.New("journal chain does not verify")

// verifyChain recomputes the chain from the last checkpoint at or below
// pos.Seq, compares every stored head and the position's head, and checks that
// the newest entry's state digest is the live-state digest recorded in the
// meta bucket. The head of every step commits to its state digest, so a
// recorded digest that was edited (with or without the records adjusted to
// match) does not verify. The entry at pos.Seq is never pruned: pruning keeps
// every entry from one checkpoint interval before the newest checkpoint, and
// a checkpoint at pos.Seq keeps its own entry.
func verifyChain(btx *bolt.Tx, pos Position) error {
	cpSeq, head, cpPrev, ok := lastCheckpointFull(btx, pos.Seq)
	if !ok {
		return fmt.Errorf("%w: no checkpoint", errChainBroken)
	}
	chain := btx.Bucket([]byte(bucketChain))
	var state [32]byte // the empty store's state digest, for pos.Seq == cpSeq == 0
	for seq := cpSeq + 1; seq <= pos.Seq; seq++ {
		v := chain.Get(seqKey(seq))
		if len(v) != chainEntrySize {
			return fmt.Errorf("%w: entry %d missing", errChainBroken, seq)
		}
		var digest, stored [32]byte
		copy(digest[:], v[:32])
		copy(state[:], v[32:64])
		copy(stored[:], v[64:])
		head = nextHead(head, seq, digest, state)
		if head != stored {
			return fmt.Errorf("%w: head mismatch at %d", errChainBroken, seq)
		}
	}
	if head != pos.Head {
		return fmt.Errorf("%w: position head mismatch", errChainBroken)
	}
	if pos.Seq == cpSeq && pos.Seq > 0 {
		// The newest step is the checkpoint itself: its entry is retained but
		// was not part of the loop above. Recompute it from the checkpoint's
		// previous head, which authenticates the state it holds.
		v := chain.Get(seqKey(pos.Seq))
		if len(v) != chainEntrySize {
			return fmt.Errorf("%w: entry %d missing", errChainBroken, pos.Seq)
		}
		var digest [32]byte
		copy(digest[:], v[:32])
		copy(state[:], v[32:64])
		if nextHead(cpPrev, pos.Seq, digest, state) != pos.Head {
			return fmt.Errorf("%w: head mismatch at %d", errChainBroken, pos.Seq)
		}
	}
	m := btx.Bucket([]byte(bucketMeta))
	if m == nil {
		return fmt.Errorf("%w: meta bucket missing", errChainBroken)
	}
	if v := m.Get(keyStateDigest); len(v) != len(state) || !bytes.Equal(v, state[:]) {
		return fmt.Errorf("%w: the recorded live-state digest is not the one the chain committed to", errChainBroken)
	}
	return nil
}
