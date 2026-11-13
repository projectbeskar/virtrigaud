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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// A key that holds a nested bucket is not an absent key.
func TestGet_NestedBucketIsCorruptNotAbsent(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	require.NoError(t, s.RawUpdateForTest(func(btx *bolt.Tx) error {
		_, err := btx.Bucket([]byte(testBucket)).CreateBucket([]byte("n"))
		return err
	}))
	_, err := get(t, s, testBucket, "n")
	requireCorrupt(t, s, err)
}

func TestForEach_NestedBucketIsCorrupt(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	require.NoError(t, s.RawUpdateForTest(func(btx *bolt.Tx) error {
		_, err := btx.Bucket([]byte(testBucket)).CreateBucket([]byte("n"))
		return err
	}))
	var seen []string
	err := s.View(context.Background(), func(tx *journal.Tx) error {
		return tx.ForEach(testBucket, nil, func(k, _ []byte) error {
			seen = append(seen, string(k))
			return nil
		})
	})
	requireCorrupt(t, s, err)
	assert.Equal(t, []string{"a"}, seen, "the nested bucket is never passed to the callback")
}

func TestPut_OverNestedBucketIsCorrupt(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	require.NoError(t, s.RawUpdateForTest(func(btx *bolt.Tx) error {
		_, err := btx.Bucket([]byte(testBucket)).CreateBucket([]byte("n"))
		return err
	}))
	_, err := s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte("n"), []byte("v"))
	})
	require.ErrorIs(t, err, journal.ErrCorruptRecord)
	wantLost(t, s, journal.LostJournalCorrupt)
}

// The client read path refuses the journal's own buckets.
func TestGetAndForEach_RefuseReservedBuckets(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	for _, name := range []string{journal.BucketMetaForTest, journal.BucketChainForTest, "journal.anything"} {
		err := s.View(context.Background(), func(tx *journal.Tx) error {
			_, err := tx.Get(name, []byte("position"))
			return err
		})
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "reserved")
		err = s.View(context.Background(), func(tx *journal.Tx) error {
			return tx.ForEach(name, nil, func(_, _ []byte) error { return nil })
		})
		require.Error(t, err, name)
	}
	st, _ := s.State()
	assert.Equal(t, journal.StateReady, st, "a refused name is a caller error, not a loss")
}

// The store file of an unfinished first open is set aside, not removed.
func TestOpen_FirstOpenRestartSetsTheOldStoreAside(t *testing.T) {
	e := newEnv(t)
	initialDB(t, e)
	before, err := os.ReadFile(filepath.Join(e.dir, journal.StoreFile))
	require.NoError(t, err)
	s := e.open()
	st, reason := s.State()
	require.Equal(t, journal.StateUnattested, st, reason)
	matches, err := filepath.Glob(filepath.Join(e.dir, journal.StoreFile+journal.UnfinishedSuffixForTest+"*"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	after, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.Equal(t, before, after, "the old file is kept as it was")
	assert.FileExists(t, filepath.Join(e.dir, journal.StoreFile))
}

// A leftover anchor rotation file is evidence of a store that had committed.
func TestOpen_FirstOpenRestartCountsAnchorRotationFileAsEvidence(t *testing.T) {
	e := newEnv(t)
	initialDB(t, e)
	e.mem.Put(filepath.Join(e.dir, journal.AnchorNextFileForTest), make([]byte, journal.AnchorRecordSize))
	s := e.open()
	wantLost(t, s, journal.LostJournalCorrupt)
	matches, err := filepath.Glob(filepath.Join(e.dir, journal.StoreFile+journal.UnfinishedSuffixForTest+"*"))
	require.NoError(t, err)
	assert.Empty(t, matches, "nothing is set aside: the file is kept in place")
}

// The same file with a store whose seq is 0 and no marker (the other restart
// path) is evidence too.
func TestOpen_EmptyStoreRestartCountsAnchorRotationFileAsEvidence(t *testing.T) {
	e := newEnv(t)
	s := e.open()
	require.NoError(t, s.Close())
	require.NoError(t, e.mem.Remove(filepath.Join(e.dir, journal.MarkerFile)))
	require.NoError(t, e.mem.Remove(filepath.Join(e.dir, journal.AnchorFile)))
	e.mem.Put(filepath.Join(e.dir, journal.AnchorNextFileForTest), make([]byte, journal.AnchorRecordSize))
	s2 := e.open()
	st, _ := s2.State()
	assert.Equal(t, journal.StateLost, st)
}

func TestOpen_MissingStoreWithAnchorRotationFileIsMissing(t *testing.T) {
	e := newEnv(t)
	e.mem.Put(filepath.Join(e.dir, journal.AnchorNextFileForTest), make([]byte, journal.AnchorRecordSize))
	wantLost(t, e.open(), journal.LostJournalMissing)
}

// A runtime corruption writes the same quarantine records as one found at
// open: the marker file and the anchor record, not the store record, which
// would write into the damaged store.
func TestQuarantine_RuntimeCorruptionKeepsTheStoreAsEvidence(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	rawSet(t, s, "a", []byte("short"))
	_, err := get(t, s, testBucket, "a")
	requireCorrupt(t, s, err)
	assert.True(t, hasFileRecord(e))
	assert.True(t, hasAnchorRecord(t, e))
	require.NoError(t, s.Close())
	assert.False(t, hasStoreRecord(t, e), "no write into the corrupt store")
	got := e.open()
	wantLost(t, got, journal.LostJournalCorrupt)
	require.NoError(t, got.Close())
	assert.False(t, hasStoreRecord(t, e), "the open does not write it either")
}

// After a memory fault that leaves bbolt's locks held, no call enters bbolt
// again, and concurrent callers all come back. (A call that was already
// waiting inside bbolt when the fault happened is not released: see the
// package documentation.)
func TestStore_FaultedStoreServesNoMoreCalls(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	truncateFile(t, filepath.Join(e.dir, journal.StoreFile), 0)
	// The first read faults and taints the database.
	err := viewAfterFault(s)
	require.ErrorIs(t, err, journal.ErrLost)

	const callers = 6
	done := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			var err error
			if i%2 == 0 {
				err = s.View(context.Background(), func(tx *journal.Tx) error {
					_, err := tx.Get(testBucket, []byte("a"))
					return err
				})
			} else {
				_, err = s.Update(context.Background(), func(tx *journal.Tx) error {
					return tx.Put(testBucket, []byte("b"), []byte("2"))
				})
			}
			done <- err
		}(i)
	}
	for i := 0; i < callers; i++ {
		select {
		case err := <-done:
			require.ErrorIs(t, err, journal.ErrLost)
		case <-timeAfter():
			t.Fatal("a caller hung after a memory fault")
		}
	}
	wantLost(t, s, journal.LostJournalCorrupt)
	err = s.View(context.Background(), func(tx *journal.Tx) error { return nil })
	require.ErrorIs(t, err, journal.ErrLost)
	require.ErrorIs(t, s.Scrub(context.Background()), journal.ErrLost)
}

// The goroutine that faults never waits for the store lock, which a writer
// stuck inside bbolt would hold for good: the quarantine is applied by the
// next call that takes the lock.
func TestStore_FaultDoesNotWaitForTheStoreLock(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	s.TaintForTest()

	unlock := s.LockForTest()
	done := make(chan struct{})
	go func() {
		s.OnViewFaultForTest()
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfter():
		unlock()
		t.Fatal("the faulting call waited for the store lock")
	}
	unlock()
	wantLost(t, s, journal.LostJournalCorrupt)
	assert.True(t, hasFileRecord(e), "the pending quarantine was applied by the next call")
}

func viewAfterFault(s *journal.Store) error {
	return s.View(context.Background(), func(tx *journal.Tx) error {
		_, err := tx.Get(testBucket, []byte("a"))
		return err
	})
}
