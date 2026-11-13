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
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// secretValue must never appear in an error, an event or a quarantine detail.
const secretValue = "super-secret-value-0123"

func get(t *testing.T, s *journal.Store, bucket, key string) ([]byte, error) {
	t.Helper()
	var v []byte
	err := s.View(context.Background(), func(tx *journal.Tx) (err error) {
		v, err = tx.Get(bucket, []byte(key))
		return err
	})
	return v, err
}

// rawValue reads the stored bytes (value and trailer) of a record from the
// open store.
func rawValue(t *testing.T, s *journal.Store, key string) []byte {
	t.Helper()
	var v []byte
	require.NoError(t, s.RawUpdateForTest(func(btx *bolt.Tx) error {
		v = append([]byte(nil), btx.Bucket([]byte(testBucket)).Get([]byte(key))...)
		return nil
	}))
	return v
}

func rawSet(t *testing.T, s *journal.Store, key string, v []byte) {
	t.Helper()
	require.NoError(t, s.RawUpdateForTest(func(btx *bolt.Tx) error {
		return btx.Bucket([]byte(testBucket)).Put([]byte(key), v)
	}))
}

func rawDelete(t *testing.T, s *journal.Store, key string) {
	t.Helper()
	require.NoError(t, s.RawUpdateForTest(func(btx *bolt.Tx) error {
		return btx.Bucket([]byte(testBucket)).Delete([]byte(key))
	}))
}

// openEvents is env.open with the quarantine events collected.
func (e *env) openEvents(events *[]journal.Event) *journal.Store {
	e.opts.Events = func(ev journal.Event) { *events = append(*events, ev) }
	return e.open()
}

func requireCorrupt(t *testing.T, s *journal.Store, err error) {
	t.Helper()
	require.Error(t, err)
	assert.ErrorIs(t, err, journal.ErrCorruptRecord)
	assert.ErrorIs(t, err, journal.ErrLost)
	assert.NotContains(t, err.Error(), secretValue)
	wantLost(t, s, journal.LostJournalCorrupt)
}

func TestRecord_TrailerFormat(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	pos := put(t, s, "a", secretValue)
	raw := rawValue(t, s, "a")
	require.Len(t, raw, len(secretValue)+journal.TrailerSizeForTest)
	body, tr := raw[:len(secretValue)], raw[len(secretValue):]
	assert.Equal(t, secretValue, string(body))
	assert.Equal(t, byte(1), tr[0])
	assert.Equal(t, pos.Seq, binary.BigEndian.Uint64(tr[1:9]))
	want, d := journal.SealRecordForTest(pos.StoreID, testBucket, []byte("a"), pos.Seq, []byte(secretValue))
	assert.Equal(t, want, raw)
	assert.Equal(t, d[:], tr[9:])
	got, err := get(t, s, testBucket, "a")
	require.NoError(t, err)
	assert.Equal(t, secretValue, string(got))
	missing, err := get(t, s, testBucket, "nope")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

// The chain step commits to the live-state digest: the head is the hash of
// the previous head, the position, the transaction digest and the state. The
// transaction digest covers the client writes only; the meta write of the state
// digest is not repeated in it.
func TestRecord_ChainCommitsToStateDigest(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	before := s.Position()
	pos := put(t, s, "a", "1")
	var entry []byte
	require.NoError(t, s.RawUpdateForTest(func(btx *bolt.Tx) error {
		entry = append([]byte(nil), btx.Bucket([]byte(journal.BucketChainForTest)).Get(binary.BigEndian.AppendUint64(nil, pos.Seq))...)
		return nil
	}))
	require.Len(t, entry, journal.ChainEntrySizeForTest)
	sealed, d := journal.SealRecordForTest(pos.StoreID, testBucket, []byte("a"), pos.Seq, []byte("1"))
	want := journal.TxDigestPutsForTest([][3][]byte{{[]byte(testBucket), []byte("a"), sealed}})
	assert.Equal(t, want[:], entry[:32], "the transaction digest covers the client writes")
	assert.Equal(t, d[:], entry[32:64], "the only live record's digest is the whole state")
	head := journal.NextHeadForTest(before.Head, pos.Seq, want, d)
	assert.Equal(t, head[:], entry[64:], "the head commits to the state digest")
	assert.Equal(t, head, pos.Head)
}

// forgeState replaces the record under key with another valid record and
// edits the recorded live-state digest to match, as a consistent forgery of
// everything but the chain.
func forgeState(t *testing.T, dir, key string) {
	t.Helper()
	meta := readMeta(t, dir)
	var id [16]byte
	copy(id[:], meta.GetStoreId())
	withBolt(t, dir, func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(testBucket))
		old := b.Get([]byte(key))
		oldSeq := binary.BigEndian.Uint64(old[len(old)-journal.TrailerSizeForTest+1:])
		body := old[:len(old)-journal.TrailerSizeForTest]
		_, od := journal.SealRecordForTest(id, testBucket, []byte(key), oldSeq, body)
		sealed, nd := journal.SealRecordForTest(id, testBucket, []byte(key), oldSeq, []byte("forged"))
		if err := b.Put([]byte(key), sealed); err != nil {
			return err
		}
		m := tx.Bucket([]byte(journal.BucketMetaForTest))
		var st [32]byte
		copy(st[:], m.Get(journal.KeyStateDigestForTest))
		st = journal.AddSumForTest(journal.SubSumForTest(st, od), nd)
		return m.Put(journal.KeyStateDigestForTest, st[:])
	})
}

// A record changed together with the recorded state digest verifies against
// the records and the digest, but not against the chain.
func TestRecord_EditedStateDigestIsDetectedByTheChain(t *testing.T) {
	for _, every := range []uint64{0, 3} {
		for _, commits := range []int{1, 3, 4, 7} {
			t.Run(fmt.Sprintf("every=%d commits=%d", every, commits), func(t *testing.T) {
				e := newEnv(t)
				e.opts.CheckpointEvery = every
				s := e.ready()
				for i := 0; i < commits; i++ {
					put(t, s, fmt.Sprintf("k%d", i), "v")
				}
				require.NoError(t, s.Close())
				// Untouched, the store opens (checkpoints keep verifying).
				ok := e.open()
				st, reason := ok.State()
				require.Equal(t, journal.StateUnattested, st, reason)
				require.NoError(t, ok.Scrub(context.Background()))
				require.NoError(t, ok.Close())

				forgeState(t, e.dir, "k0")
				var events []journal.Event
				bad := e.openEvents(&events)
				wantLost(t, bad, journal.LostJournalCorrupt)
				require.NotEmpty(t, events)
				assert.Contains(t, events[0].Detail, "live-state digest", "the chain, not the record scan, found it")
			})
		}
	}
}

// Scrub also checks the chain's commitment to the state digest of a running
// store.
func TestRecord_ScrubDetectsEditedStateDigest(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	put(t, s, "b", "2")
	require.NoError(t, s.RawUpdateForTest(func(btx *bolt.Tx) error {
		id := s.Position().StoreID
		b := btx.Bucket([]byte(testBucket))
		old := b.Get([]byte("a"))
		oldSeq := binary.BigEndian.Uint64(old[len(old)-journal.TrailerSizeForTest+1:])
		_, od := journal.SealRecordForTest(id, testBucket, []byte("a"), oldSeq, []byte("1"))
		sealed, nd := journal.SealRecordForTest(id, testBucket, []byte("a"), oldSeq, []byte("forged"))
		if err := b.Put([]byte("a"), sealed); err != nil {
			return err
		}
		m := btx.Bucket([]byte(journal.BucketMetaForTest))
		var st [32]byte
		copy(st[:], m.Get(journal.KeyStateDigestForTest))
		st = journal.AddSumForTest(journal.SubSumForTest(st, od), nd)
		return m.Put(journal.KeyStateDigestForTest, st[:])
	}))
	err := s.Scrub(context.Background())
	requireCorrupt(t, s, err)
}

func TestRecord_DigestArithmetic(t *testing.T) {
	var ones, one, zero [32]byte
	for i := range ones {
		ones[i] = 0xff
	}
	one[31] = 1
	assert.Equal(t, zero, journal.AddSumForTest(ones, one), "wraps modulo 2^256")
	assert.Equal(t, ones, journal.SubSumForTest(zero, one))
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		var a, b [32]byte
		rng.Read(a[:])
		rng.Read(b[:])
		assert.Equal(t, a, journal.SubSumForTest(journal.AddSumForTest(a, b), b))
		assert.Equal(t, journal.AddSumForTest(a, b), journal.AddSumForTest(b, a))
	}
}

// A damaged record that still decodes is an error, never a missing key, and
// the store is lost with a detail that names the record and not its value.
func TestRecord_FlippedByteIsDetectedOnRead(t *testing.T) {
	for name, flip := range map[string]int{"value": 0, "trailer digest": len(secretValue) + 20, "trailer seq": len(secretValue) + 3} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			var events []journal.Event
			s := e.openEvents(&events)
			require.NoError(t, s.EstablishContinuity(context.Background()))
			put(t, s, "a", secretValue)
			put(t, s, "b", "other")
			raw := rawValue(t, s, "a")
			raw[flip] ^= 0x01
			rawSet(t, s, "a", raw)

			v, err := get(t, s, testBucket, "a")
			assert.Nil(t, v)
			requireCorrupt(t, s, err)
			require.NotEmpty(t, events)
			detail := events[len(events)-1].Detail
			assert.Contains(t, detail, testBucket)
			assert.Contains(t, detail, fmt.Sprintf("%x", "a"))
			assert.NotContains(t, detail, secretValue)
			// An untouched record is still served.
			v, err = get(t, s, testBucket, "b")
			require.NoError(t, err)
			assert.Equal(t, "other", string(v))
		})
	}
}

func TestRecord_ForEachPropagatesCorruption(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	put(t, s, "b", "2")
	raw := rawValue(t, s, "b")
	raw[0] ^= 0xff
	rawSet(t, s, "b", raw)
	var seen []string
	err := s.View(context.Background(), func(tx *journal.Tx) error {
		return tx.ForEach(testBucket, nil, func(k, _ []byte) error {
			seen = append(seen, string(k))
			return nil
		})
	})
	assert.Equal(t, []string{"a"}, seen)
	requireCorrupt(t, s, err)
}

// A caller that drops the error still cannot read past the damage unnoticed:
// the store is quarantined when the transaction ends.
func TestRecord_SwallowedErrorStillQuarantines(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	raw := rawValue(t, s, "a")
	raw[0] ^= 0xff
	rawSet(t, s, "a", raw)
	err := s.View(context.Background(), func(tx *journal.Tx) error {
		_, _ = tx.Get(testBucket, []byte("a"))
		return nil
	})
	requireCorrupt(t, s, err)
}

func TestRecord_ShortValueBadVersionAndFutureSeq(t *testing.T) {
	cases := map[string]func(forged func(seq uint64) []byte, committed uint64) []byte{
		"short": func(f func(uint64) []byte, c uint64) []byte { return f(c)[:journal.TrailerSizeForTest-1] },
		"empty": func(func(uint64) []byte, uint64) []byte { return []byte{} },
		"bad version": func(f func(uint64) []byte, c uint64) []byte {
			r := f(c)
			r[len(r)-journal.TrailerSizeForTest] = 2
			return r
		},
		"future seq": func(f func(uint64) []byte, c uint64) []byte { return f(c + 1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			s := e.ready()
			pos := put(t, s, "a", "1")
			forged := func(seq uint64) []byte {
				sealed, _ := journal.SealRecordForTest(pos.StoreID, testBucket, []byte("f"), seq, []byte("1"))
				return sealed
			}
			rawSet(t, s, "f", mutate(forged, pos.Seq))
			_, err := get(t, s, testBucket, "f")
			requireCorrupt(t, s, err)
		})
	}
}

// A record that is valid for another key is rejected: the trailer binds the
// key, so two swapped values are both caught.
func TestRecord_SwappedValuesAreDetected(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	put(t, s, "b", "2")
	ra, rb := rawValue(t, s, "a"), rawValue(t, s, "b")
	rawSet(t, s, "a", rb)
	rawSet(t, s, "b", ra)
	_, err := get(t, s, testBucket, "a")
	requireCorrupt(t, s, err)
}

func TestRecord_WriteOverCorruptRecordFails(t *testing.T) {
	for name, op := range map[string]func(tx *journal.Tx) error{
		"put":    func(tx *journal.Tx) error { return tx.Put(testBucket, []byte("a"), []byte("2")) },
		"delete": func(tx *journal.Tx) error { return tx.Delete(testBucket, []byte("a")) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			s := e.ready()
			put(t, s, "a", "1")
			before := s.Position()
			raw := rawValue(t, s, "a")
			raw[0] ^= 0xff
			rawSet(t, s, "a", raw)
			_, err := s.Update(context.Background(), op)
			require.ErrorIs(t, err, journal.ErrCorruptRecord)
			require.ErrorIs(t, err, journal.ErrLost)
			assert.Equal(t, before, s.Position())
			wantLost(t, s, journal.LostJournalCorrupt)
		})
	}
}

// A damaged record found at open: the store opens lost, before serving.
func TestRecord_OpenScanDetectsDamage(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	put(t, s, "b", "2")
	require.NoError(t, s.Close())
	withBolt(t, e.dir, func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(testBucket))
		v := append([]byte(nil), b.Get([]byte("a"))...)
		v[0] ^= 1
		return b.Put([]byte("a"), v)
	})
	wantLost(t, e.open(), journal.LostJournalCorrupt)
}

// An old, valid version of a record is not detectable on a single read; open
// and Scrub catch it through the live-state digest.
func TestRecord_RestoredOldVersionIsDetectedByScrubAndOpen(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	put(t, s, "b", "2")
	old := rawValue(t, s, "a")
	put(t, s, "a", "3")
	require.NoError(t, s.Scrub(context.Background()))
	rawSet(t, s, "a", old)
	v, err := get(t, s, testBucket, "a")
	require.NoError(t, err, "the old record is internally valid")
	assert.Equal(t, "1", string(v))
	err = s.Scrub(context.Background())
	require.ErrorIs(t, err, journal.ErrCorruptRecord)
	wantLost(t, s, journal.LostJournalCorrupt)
	require.NoError(t, s.Close())

	// The same damage, found at open.
	e2 := newEnv(t)
	s2 := e2.ready()
	put(t, s2, "a", "1")
	old = rawValue(t, s2, "a")
	put(t, s2, "a", "3")
	require.NoError(t, s2.Close())
	withBolt(t, e2.dir, func(tx *bolt.Tx) error { return tx.Bucket([]byte(testBucket)).Put([]byte("a"), old) })
	wantLost(t, e2.open(), journal.LostJournalCorrupt)
}

func TestRecord_LostAndResurrectedRecordsAreDetected(t *testing.T) {
	t.Run("record deleted behind the store's back", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		put(t, s, "b", "2")
		rawDelete(t, s, "a")
		v, err := get(t, s, testBucket, "a")
		require.NoError(t, err, "a lost record reads as missing; the scrub finds it")
		assert.Nil(t, v)
		require.ErrorIs(t, s.Scrub(context.Background()), journal.ErrCorruptRecord)
		wantLost(t, s, journal.LostJournalCorrupt)
		require.NoError(t, s.Close())
	})
	t.Run("lost record found at open", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		require.NoError(t, s.Close())
		withBolt(t, e.dir, func(tx *bolt.Tx) error { return tx.Bucket([]byte(testBucket)).Delete([]byte("a")) })
		wantLost(t, e.open(), journal.LostJournalCorrupt)
	})
	t.Run("deleted record resurrected", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		old := rawValue(t, s, "a")
		_, err := s.Update(context.Background(), func(tx *journal.Tx) error { return tx.Delete(testBucket, []byte("a")) })
		require.NoError(t, err)
		require.NoError(t, s.Scrub(context.Background()))
		rawSet(t, s, "a", old)
		require.ErrorIs(t, s.Scrub(context.Background()), journal.ErrCorruptRecord)
		wantLost(t, s, journal.LostJournalCorrupt)
	})
	t.Run("record added behind the store's back", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		pos := put(t, s, "a", "1")
		forged, _ := journal.SealRecordForTest(pos.StoreID, testBucket, []byte("z"), pos.Seq, []byte("9"))
		rawSet(t, s, "z", forged)
		require.ErrorIs(t, s.Scrub(context.Background()), journal.ErrCorruptRecord)
		wantLost(t, s, journal.LostJournalCorrupt)
	})
	t.Run("state digest damaged", func(t *testing.T) {
		e := newEnv(t)
		s := e.ready()
		put(t, s, "a", "1")
		require.NoError(t, s.RawUpdateForTest(func(btx *bolt.Tx) error {
			m := btx.Bucket([]byte(journal.BucketMetaForTest))
			v := append([]byte(nil), m.Get(journal.KeyStateDigestForTest)...)
			v[5] ^= 1
			return m.Put(journal.KeyStateDigestForTest, v)
		}))
		require.ErrorIs(t, s.Scrub(context.Background()), journal.ErrCorruptRecord)
		wantLost(t, s, journal.LostJournalCorrupt)
	})
}

// Overwrites and deletes keep the digest exact: after a random sequence of
// operations a scrub and a reopen both pass.
func TestRecord_StateDigestTracksRandomOperations(t *testing.T) {
	e := newEnv(t)
	e.opts.Buckets = []string{testBucket, "second"}
	s := e.ready()
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 300; i++ {
		_, err := s.Update(context.Background(), func(tx *journal.Tx) error {
			for j := 0; j < 1+rng.Intn(4); j++ {
				bucket := []string{testBucket, "second"}[rng.Intn(2)]
				key := []byte(fmt.Sprintf("k%d", rng.Intn(12)))
				if rng.Intn(3) == 0 {
					if err := tx.Delete(bucket, key); err != nil {
						return err
					}
					continue
				}
				if err := tx.Put(bucket, key, bytes.Repeat([]byte{byte(rng.Intn(256))}, rng.Intn(40))); err != nil {
					return err
				}
			}
			return nil
		})
		require.NoError(t, err)
		if i%50 == 0 {
			require.NoError(t, s.Scrub(context.Background()))
		}
	}
	require.NoError(t, s.Scrub(context.Background()))
	require.NoError(t, s.VerifyChain(context.Background()))
	require.NoError(t, s.Close())
	s2 := e.open()
	st, reason := s2.State()
	require.Equal(t, journal.StateUnattested, st, reason)
	require.NoError(t, s2.Scrub(context.Background()))
}

func TestRecord_ScrubOnLostStoreReturnsTheLoss(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	s.Quarantine(journal.LostJournalCorrupt, "test")
	err := s.Scrub(context.Background())
	var le *journal.LostError
	require.ErrorAs(t, err, &le)
	assert.Equal(t, journal.LostJournalCorrupt, le.Reason)
}

// A store from before record verification is refused, not migrated.
func TestSchema_StoreBeforeRecordVerificationIsRefused(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	require.NoError(t, s.Close())
	withBolt(t, e.dir, func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(journal.BucketMetaForTest)).Delete(journal.KeyStateDigestForTest)
	})
	rewriteMeta(t, e.dir, func(m *journalpb.StoreMeta) { m.SchemaVersion, m.MinReaderVersion = 1, 1 })
	var events []journal.Event
	got := e.openEvents(&events)
	wantLost(t, got, journal.LostJournalCorrupt)
	require.NotEmpty(t, events)
	assert.Contains(t, events[0].Detail, "store predates record verification")
	assert.Equal(t, uint64(1), events[0].Seq, "the loss names the store's position")
	assert.True(t, hasFileRecord(e))
	// The anchor's quarantine record names the store.
	var found bool
	for _, r := range anchorRecords(t, e) {
		if r.Type == journal.AnchorQuarantine {
			found = true
			assert.Equal(t, got.Position().StoreID, r.StoreID)
			assert.Equal(t, uint64(1), r.Seq)
		}
	}
	assert.True(t, found)
}

// A store at the current schema that lost its digest is refused too.
func TestSchema_MissingStateDigestIsRefused(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	require.NoError(t, s.Close())
	withBolt(t, e.dir, func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(journal.BucketMetaForTest)).Delete(journal.KeyStateDigestForTest)
	})
	var events []journal.Event
	wantLost(t, e.openEvents(&events), journal.LostJournalCorrupt)
	require.NotEmpty(t, events)
	assert.Contains(t, events[0].Detail, "live-state digest missing")
}

// A store that needs a newer reader is refused with ErrSchemaTooNew before
// any loss decision: nothing is quarantined, and nothing is written.
func TestOpen_NewerStoreIsNeverQuarantined(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	require.NoError(t, s.Close())
	withBolt(t, e.dir, func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(journal.BucketMetaForTest)).Delete(journal.KeyStateDigestForTest)
	})
	rewriteMeta(t, e.dir, func(m *journalpb.StoreMeta) { m.MinReaderVersion = journal.ReaderMax + 1 })
	anchorBefore := anchorRecords(t, e)
	o := e.opts
	o.Dir, o.FS = e.dir, e.mem
	_, err := journal.Open(context.Background(), o)
	require.ErrorIs(t, err, journal.ErrSchemaTooNew)
	assert.False(t, hasFileRecord(e), "no quarantine file")
	assert.Equal(t, anchorBefore, anchorRecords(t, e), "no anchor quarantine record")
	assert.False(t, hasStoreRecord(t, e), "no store quarantine record")
}

// An agent that predates the record format refuses a current store.
func TestSchema_OlderAgentRefusesCurrentStore(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	require.NoError(t, s.Close())
	o := e.opts
	o.Dir, o.FS, o.ReaderMax = e.dir, e.mem, 1
	_, err := journal.Open(context.Background(), o)
	assert.ErrorIs(t, err, journal.ErrSchemaTooNew)
}

// A client migration reads and writes verified records.
func TestSchema_ClientMigrationSeesTrailers(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	require.NoError(t, s.Close())
	next := journal.SchemaVersion + 1
	e.opts.SchemaVersion = next
	e.opts.Migrations = []journal.Migration{{ToVersion: next, Apply: func(tx *journal.Tx) error {
		v, err := tx.Get(testBucket, []byte("a"))
		if err != nil {
			return err
		}
		return tx.Put(testBucket, []byte("a"), append(v, '!'))
	}}}
	s2 := e.ready()
	v, err := get(t, s2, testBucket, "a")
	require.NoError(t, err)
	assert.Equal(t, "1!", string(v))
	require.NoError(t, s2.Scrub(context.Background()))
}

func TestOpen_RefusesSchemaBelowRecordVerification(t *testing.T) {
	e := newEnv(t)
	e.opts.SchemaVersion = 1
	o := e.opts
	o.Dir, o.FS = e.dir, e.mem
	_, err := journal.Open(context.Background(), o)
	require.Error(t, err)
}

// initialDB creates, in e.dir, the file bbolt writes before the journal
// commits anything: valid meta pages and no bucket.
func initialDB(t *testing.T, e *env) {
	t.Helper()
	db, err := bolt.Open(filepath.Join(e.dir, journal.StoreFile), 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

// A first open that died after bbolt created its file but before the journal's
// first commit left nothing: the next open starts again.
func TestOpen_FirstOpenCrashedBeforeInitialCommitStartsAgain(t *testing.T) {
	e := newEnv(t)
	initialDB(t, e)
	s := e.open()
	st, reason := s.State()
	require.Equal(t, journal.StateUnattested, st, reason)
	require.NoError(t, s.EstablishContinuity(context.Background()))
	put(t, s, "a", "1")
	require.NoError(t, s.Close())
	s2 := e.open()
	st, reason = s2.State()
	require.Equal(t, journal.StateUnattested, st, reason)
}

// The restart never hides a loss: any other evidence keeps the store lost.
func TestOpen_FirstOpenRestartKeepsRealLosses(t *testing.T) {
	cases := map[string]func(t *testing.T, e *env){
		"anchor log": func(t *testing.T, e *env) {
			e.mem.Put(filepath.Join(e.dir, journal.AnchorFile), make([]byte, journal.AnchorRecordSize))
		},
		"marker": func(t *testing.T, e *env) { e.mem.Put(filepath.Join(e.dir, journal.MarkerFile), []byte{1}) },
		"quarantine record": func(t *testing.T, e *env) {
			e.mem.Put(filepath.Join(e.dir, journal.QuarantineFile), []byte(journal.LostRolledBack))
		},
		"a bucket": func(t *testing.T, e *env) {
			withBolt(t, e.dir, func(tx *bolt.Tx) error {
				_, err := tx.CreateBucket([]byte(testBucket))
				return err
			})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			initialDB(t, e)
			setup(t, e)
			s := e.open()
			st, reason := s.State()
			require.Equal(t, journal.StateLost, st)
			assert.NotEmpty(t, reason)
		})
	}
}

// pageSizeAndFreelist returns the page size and the freelist page of the
// current meta page of a bbolt file.
func freelistPage(t *testing.T, raw []byte) (pageSize, pgid uint64) {
	t.Helper()
	pageSize = uint64(binary.LittleEndian.Uint32(raw[24:]))
	best, bestTx := uint64(0), uint64(0)
	for i := uint64(0); i < 2; i++ {
		m := raw[i*pageSize+16:]
		if tx := binary.LittleEndian.Uint64(m[48:]); i == 0 || tx > bestTx {
			best, bestTx = binary.LittleEndian.Uint64(m[32:]), tx
		}
	}
	return pageSize, best
}

// bbolt panics while opening a file whose freelist page is damaged. The
// store reports the loss, and a second open in the same process gets the
// loss again, not ErrLocked from a leaked file lock.
func TestOpen_PanicInsideBboltReleasesTheFileLock(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	require.NoError(t, s.Close())
	path := filepath.Join(e.dir, journal.StoreFile)
	raw := readFile(t, path)
	ps, fl := freelistPage(t, raw)
	require.NotZero(t, fl)
	// Page flags at offset 8: not a freelist page any more.
	binary.LittleEndian.PutUint16(raw[fl*ps+8:], 0)
	writeFile(t, path, raw)

	first := e.open()
	wantLost(t, first, journal.LostJournalCorrupt)
	require.NoError(t, first.Close())
	second := e.open() // fails the test on ErrLocked
	wantLost(t, second, journal.LostJournalCorrupt)
}

// Truncating the store file under bbolt's live mapping makes the next read a
// SIGBUS. The fault becomes a recoverable panic, the store is lost, and the
// process lives.
func TestStore_MemoryFaultOnMappingBecomesLoss(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	truncateFile(t, filepath.Join(e.dir, journal.StoreFile), 0)

	err := s.View(context.Background(), func(tx *journal.Tx) error {
		_, err := tx.Get(testBucket, []byte("a"))
		return err
	})
	var le *journal.LostError
	require.ErrorAs(t, err, &le)
	assert.Equal(t, journal.LostJournalCorrupt, le.Reason)
	wantLost(t, s, journal.LostJournalCorrupt)
	_, err = s.Update(context.Background(), func(tx *journal.Tx) error { return tx.Put(testBucket, []byte("b"), nil) })
	require.ErrorIs(t, err, journal.ErrLost)
	assert.Zero(t, s.Size())
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("Close hung after a memory fault")
	}
}

// A store whose pages cannot be read at open is lost, not a crash.
func TestOpen_MemoryFaultAtOpenBecomesLoss(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	for i := 0; i < 50; i++ {
		put(t, s, fmt.Sprintf("k%d", i), strings.Repeat("x", 200))
	}
	require.NoError(t, s.Close())
	path := filepath.Join(e.dir, journal.StoreFile)
	raw := readFile(t, path)
	// Keep the two meta pages and cut the rest: the root bucket page is past
	// the end of the file, so bbolt's mapping faults when it reads it.
	ps, _ := freelistPage(t, raw)
	truncateFile(t, path, int64(2*ps))
	got := e.open()
	st, _ := got.State()
	assert.Equal(t, journal.StateLost, st)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, b, 0o600))
}

func truncateFile(t *testing.T, path string, size int64) {
	t.Helper()
	require.NoError(t, os.Truncate(path, size))
}

func timeAfter() <-chan time.Time { return time.After(10 * time.Second) }
