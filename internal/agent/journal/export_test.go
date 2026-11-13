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

import bolt "go.etcd.io/bbolt"

// Test-only access to internals for the external test package.
var (
	BucketMetaForTest    = bucketMeta
	BucketChainForTest   = bucketChain
	KeyStoreMetaForTest  = keyStoreMeta
	KeyQuarantineForTest = keyQuarantine
	// KeyStateDigestForTest is the meta key of the live-state digest.
	KeyStateDigestForTest = keyStateDigest
	// TrailerSizeForTest is the size of a record trailer.
	TrailerSizeForTest = trailerSize
	// MaxAnchorBytesForTest is the anchor log size bound.
	MaxAnchorBytesForTest = maxAnchorBytes
)

// SetTestBoltOptions installs f to adjust bbolt options in Open; nil removes
// it.
func SetTestBoltOptions(f func(*bolt.Options)) { testBoltOptions = f }

// RawUpdateForTest runs fn in a bbolt write transaction on the store's own
// database, outside the chain: it damages a store that is open.
func (s *Store) RawUpdateForTest(fn func(btx *bolt.Tx) error) error { return s.db.Update(fn) }

// SealRecordForTest returns value || trailer as the store writes it, and the
// record digest.
func SealRecordForTest(storeID [16]byte, bucket string, key []byte, seq uint64, value []byte) ([]byte, [32]byte) {
	return sealRecord(storeID, bucket, key, seq, value)
}

// AddSumForTest and SubSumForTest are the digest arithmetic.
func AddSumForTest(a, b [32]byte) [32]byte { return addSum(a, b) }

// SubSumForTest is subSum.
func SubSumForTest(a, b [32]byte) [32]byte { return subSum(a, b) }

// TxDigestPutsForTest is the transaction digest of the puts (bucket, key,
// value triples).
func TxDigestPutsForTest(puts [][3][]byte) [32]byte {
	var ws []write
	for _, p := range puts {
		ws = append(ws, write{op: writePut, bucket: string(p[0]), key: p[1], value: p[2]})
	}
	return txDigest(ws)
}

// NextHeadForTest is nextHead.
func NextHeadForTest(prev [32]byte, seq uint64, digest, state [32]byte) [32]byte {
	return nextHead(prev, seq, digest, state)
}

// ChainEntrySizeForTest is the size of a stored chain entry.
const ChainEntrySizeForTest = chainEntrySize

// UnfinishedSuffixForTest names the set-aside store files of a restarted
// first open.
const UnfinishedSuffixForTest = unfinishedSuffix

// AnchorNextFileForTest is the anchor rotation's temporary file.
const AnchorNextFileForTest = anchorNextFile

// LockForTest holds the store's mutex until the returned function is called.
func (s *Store) LockForTest() (unlock func()) {
	s.mu.Lock()
	return s.mu.Unlock
}

// TaintForTest marks the database as left unusable by a panic, as dbView does.
func (s *Store) TaintForTest() { s.tainted.Store(true) }

// OnViewFaultForTest runs the handler of a faulted read.
func (s *Store) OnViewFaultForTest() { s.onViewFault() }
