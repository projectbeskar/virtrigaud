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
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
)

// File names inside Options.Dir.
const (
	// StoreFile is the bbolt store.
	StoreFile = "state.db"
	// AnchorFile is the anchor log.
	AnchorFile = "anchor.log"
	// anchorNextFile is the anchor log being written by a rotation.
	anchorNextFile = "anchor.log.next"
	// MarkerFile holds the store_id; it tells a missing store from a fresh
	// host.
	MarkerFile = "store.marker"
	// QuarantineFile holds the lost reason of a quarantined store.
	QuarantineFile = "quarantine"
	// LockFile is the single-writer lock. Open takes it before it touches any
	// other file and holds it until Close.
	LockFile = "state.db.lock"
)

// maxAnchorBytes bounds the anchor log Open reads. A log the store wrote is
// bounded by rotation; a larger one is anchor_corrupt.
const maxAnchorBytes = 64 << 20

// lossSources records which durable quarantine records exist.
type lossSources struct{ file, store, anchor bool }

func (l lossSources) any() bool { return l.file || l.store || l.anchor }

// Reserved bucket names. Client buckets may not start with reservedPrefix.
const (
	reservedPrefix    = "journal."
	bucketMeta        = "journal.meta"
	bucketChain       = "journal.chain"
	bucketCheckpoints = "journal.checkpoints"
)

// Keys of bucketMeta.
var (
	keyStoreMeta  = []byte("store")
	keyPosition   = []byte("position")
	keyQuarantine = []byte("quarantine")
)

// Defaults.
const (
	// DefaultCheckpointEvery is the default number of commits between chain
	// checkpoints.
	DefaultCheckpointEvery = 1024
	// DefaultAnchorRotateBytes is the default anchor log size that triggers
	// a rotation.
	DefaultAnchorRotateBytes = 1 << 20
	// DefaultOpenTimeout is the default wait for the single-writer lock.
	DefaultOpenTimeout = time.Second
)

// Schema versions of this binary.
const (
	// SchemaVersion is the schema this binary writes.
	SchemaVersion uint32 = 1
	// ReaderMax is the highest min_reader_version this binary can read.
	ReaderMax uint32 = 1
	// MinReaderVersion is the min_reader_version this binary writes into a
	// new store.
	MinReaderVersion uint32 = 1
)

// testBoltOptions, when set by a test of this package, adjusts the bbolt
// options before the store is opened.
var testBoltOptions func(*bolt.Options)

// Migration is an additive schema migration. Migrations run in one chained
// transaction, and only after continuity is established: a migration commit
// before the continuity checks would hide a rollback.
type Migration struct {
	// ToVersion is the schema version after the migration.
	ToVersion uint32
	// MinReader is the min_reader_version the migration requires. A
	// migration that changes the meaning of a record raises it.
	MinReader uint32
	// Apply performs the migration.
	Apply func(tx *Tx) error
}

// Options configures Open.
type Options struct {
	// Dir is the state directory. It holds the bbolt store on the real file
	// system and, through FS, the anchor log and the markers.
	Dir string
	// FS is the file system for every file except the bbolt store. Nil uses
	// OSFS.
	FS FS
	// Buckets are the client buckets. They are created with a new store, and
	// a missing one is created by the migration transaction.
	Buckets []string
	// Hooks are test fault-injection hooks.
	Hooks Hooks
	// Events receives quarantine events. Nil discards them.
	Events func(Event)
	// CheckpointEvery is the number of commits between chain checkpoints.
	// Zero uses DefaultCheckpointEvery.
	CheckpointEvery uint64
	// AnchorRotateBytes is the anchor log size that triggers a rotation. Zero
	// uses DefaultAnchorRotateBytes.
	AnchorRotateBytes int64
	// OpenTimeout bounds the wait for the single-writer lock. Zero uses
	// DefaultOpenTimeout.
	OpenTimeout time.Duration
	// SchemaVersion is the schema this binary writes. Zero uses
	// SchemaVersion.
	SchemaVersion uint32
	// ReaderMax is the highest min_reader_version this binary reads. Zero
	// uses ReaderMax.
	ReaderMax uint32
	// Migrations are applied, in order of ToVersion, to a store whose schema
	// is older than SchemaVersion.
	Migrations []Migration
}

// State is the trust state of an open store.
type State int

const (
	// StateUnattested: the continuity checks at open passed; no commit is
	// allowed until EstablishContinuity.
	StateUnattested State = iota + 1
	// StateReady: commits are allowed.
	StateReady
	// StateLost: the store lost its persistence guarantee. Only reads are
	// served (when the store could be opened at all).
	StateLost
)

// String implements fmt.Stringer.
func (s State) String() string {
	switch s {
	case StateUnattested:
		return "unattested"
	case StateReady:
		return "ready"
	case StateLost:
		return "lost"
	}
	return "unknown"
}

// Position is a chain position.
type Position struct {
	// StoreID is the store's random identifier.
	StoreID [16]byte
	// Seq is the number of chained commits; it counts from 1 (0 is the empty
	// store).
	Seq uint64
	// Head is the chain head at Seq.
	Head [32]byte
}

// Store is the durable journal: a bbolt store with full fsync, a hash chain
// inside it, and a separately fsynced anchor log. See the package
// documentation for its guarantees and limits.
//
// A Store is safe for concurrent use. Commits are serialised.
type Store struct {
	mu   sync.Mutex
	opts Options
	fsys FS

	db     *bolt.DB
	anchor File

	storeID    [16]byte
	seq        uint64
	head       [32]byte
	anchorSize int64

	state   State
	lost    LostReason
	crashed bool
	closed  bool

	// lock is the held single-writer lock file.
	lock *fileLock
	// had records which quarantine records Open found.
	had lossSources
	// marker is the content of the store marker.
	marker *journalpb.StoreMarker

	// pendingAnchor is used only when the AnchorBeforeAck guard is off (a
	// mutation test): the record is appended at the next commit.
	pendingAnchor []byte
}

func (s *Store) path(name string) string { return filepath.Join(s.opts.Dir, name) }

// Open opens or creates the store in opts.Dir and runs the continuity checks:
// schema and min_reader_version, the chain from the last checkpoint, the
// anchor log under its tail rule, the store marker and any quarantine
// record. It never commits.
//
// A store that fails a continuity check is still returned, in StateLost, with
// a nil error: the caller serves reads and refuses mutations. Open returns an
// error only when it cannot decide (bad options, the single-writer lock is
// held, a file-system error before any check, unsynchronised bbolt options) or
// when the store needs a newer reader (ErrSchemaTooNew), in which case the
// agent must not start.
func Open(ctx context.Context, opts Options) (st *Store, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	if opts.Dir == "" {
		return nil, errors.New("open journal: no directory")
	}
	for _, b := range opts.Buckets {
		if b == "" || strings.HasPrefix(b, reservedPrefix) {
			return nil, fmt.Errorf("open journal: bucket name %q is reserved or empty", b)
		}
	}
	if opts.FS == nil {
		opts.FS = OSFS{}
	}
	if opts.CheckpointEvery == 0 {
		opts.CheckpointEvery = DefaultCheckpointEvery
	}
	if opts.AnchorRotateBytes == 0 {
		opts.AnchorRotateBytes = DefaultAnchorRotateBytes
	}
	if opts.OpenTimeout == 0 {
		opts.OpenTimeout = DefaultOpenTimeout
	}
	if opts.SchemaVersion == 0 {
		opts.SchemaVersion = SchemaVersion
	}
	if opts.ReaderMax == 0 {
		opts.ReaderMax = ReaderMax
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("open journal: create %s: %w", opts.Dir, err)
	}
	if err := checkDirMode(opts.Dir); err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	if err := checkLocalFS(opts.Dir); err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	s := &Store{opts: opts, fsys: opts.FS}
	// The single-writer lock comes first: a second process must not touch
	// any file of a store another one runs.
	if !guard.Off(guard.LockFile) {
		l, err := acquireLock(filepath.Join(opts.Dir, LockFile))
		if err != nil {
			return nil, fmt.Errorf("open journal: %w", err)
		}
		s.lock = l
	}
	if err := s.fsys.MkdirAll(opts.Dir, 0o700); err != nil {
		_ = s.lock.release()
		return nil, fmt.Errorf("open journal: create %s: %w", opts.Dir, err)
	}
	defer func() {
		// bbolt may panic on corrupt pages: a corrupt store is lost, never a
		// crashed agent.
		if r := recover(); r != nil {
			s.setLost(LostJournalCorrupt)
			s.recordOpenLossSafe()
			st, err = s, nil
		}
	}()
	if err := s.open(); err != nil {
		_ = s.closeFiles()
		_ = s.lock.release()
		return nil, err
	}
	s.recordOpenLoss()
	return s, nil
}

// recordOpenLoss makes a loss found at open durable. Whatever source reported
// it, every missing quarantine record is written again: the marker file, a
// quarantine record in the anchor log (when it exists, even if it is
// damaged) and, when the store opened and the loss is not one whose evidence
// a write would disturb (a rolled-back, corrupt or inconsistent store), the
// store record. A restart therefore never lifts a quarantine because one
// record was removed or never reached the disk.
func (s *Store) recordOpenLoss() {
	if s.state != StateLost {
		return
	}
	if guard.Off(guard.QuarantineRewrite) && s.had.any() {
		return
	}
	reason := s.lost
	var errs []error
	persist := !guard.Off(guard.QuarantinePersist)
	if persist && !s.had.file {
		if err := WriteFileDurable(s.fsys, s.opts.Dir, s.path(QuarantineFile), []byte(reason)); err != nil {
			errs = append(errs, err)
		}
	}
	if persist && !s.had.anchor {
		if err := s.appendQuarantineByPath(reason); err != nil {
			errs = append(errs, err)
		}
	}
	if persist && !s.had.store && s.db != nil && storeWriteSafe(reason) {
		if err := s.writeStoreQuarantine(reason, "recorded when the store was opened"); err != nil {
			errs = append(errs, err)
		}
	}
	if s.opts.Events != nil {
		d := "found when the store was opened"
		if len(errs) > 0 {
			d = fmt.Sprintf("%s (not every quarantine record was written: %v)", d, errors.Join(errs...))
		}
		s.opts.Events(Event{Kind: EventQuarantine, Reason: reason, Seq: s.seq, Detail: d})
	}
}

// recordOpenLossSafe is recordOpenLoss on the panic path of Open.
func (s *Store) recordOpenLossSafe() {
	defer func() { _ = recover() }()
	s.recordOpenLoss()
}

// storeWriteSafe reports whether a quarantine record may be written into the
// store at open for reason. A rolled-back, corrupt or inconsistent store, and
// one whose anchor log is missing (which may be an older store restored
// without its anchor), is kept as it is, as evidence: a write could reuse the
// pages of a lost commit. For a missing anchor log, the anchor's own
// quarantine record is written into a new anchor log instead.
func storeWriteSafe(reason LostReason) bool {
	switch reason {
	case LostRolledBack, LostJournalCorrupt, LostInconsistentStore, LostAnchorMissing:
		return false
	}
	return true
}

// appendQuarantineByPath appends a quarantine record to the anchor log
// through a fresh descriptor, and fsyncs it. A missing anchor log is created
// holding only that record (and its directory fsynced), so that the loss stays
// recorded without writing into the store.
func (s *Store) appendQuarantineByPath(reason LostReason) error {
	ok, err := s.fsys.Exists(s.path(AnchorFile))
	if err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_APPEND
	if !ok {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := s.fsys.OpenFile(s.path(AnchorFile), flags, 0o600)
	if err != nil {
		return fmt.Errorf("open anchor log: %w", err)
	}
	if !ok {
		defer func() { _ = s.fsys.SyncDir(s.opts.Dir) }()
	}
	rec := AnchorRecord{Type: AnchorQuarantine, Reason: reason, StoreID: s.storeID, Seq: s.seq, Head: s.head}
	if _, err := f.Write(rec.Encode()); err != nil {
		_ = f.Close()
		return fmt.Errorf("append quarantine record: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync anchor log: %w", err)
	}
	return f.Close()
}

// scanAnchorQuarantine reports whether the anchor log holds a valid
// quarantine record, reading record by record so that a damaged log still
// counts, and returns the first one's reason.
func (s *Store) scanAnchorQuarantine(raw []byte) (LostReason, bool) {
	for off := 0; off+AnchorRecordSize <= len(raw); off += AnchorRecordSize {
		r, err := DecodeAnchorRecord(raw[off : off+AnchorRecordSize])
		if err == nil && r.Type == AnchorQuarantine {
			return r.Reason, true
		}
	}
	return "", false
}

// readAnchor reads the anchor log, bounded by maxAnchorBytes.
func (s *Store) readAnchor() ([]byte, bool, error) {
	raw, err := s.fsys.ReadFile(s.path(AnchorFile))
	if err != nil {
		return nil, false, fmt.Errorf("open journal: read anchor log: %w", err)
	}
	return raw, len(raw) <= maxAnchorBytes, nil
}

// open does the work of Open. The caller holds the lock file.
func (s *Store) open() error {
	dbExists, err := fileExists(s.path(StoreFile))
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	anchorExists, err := s.fsys.Exists(s.path(AnchorFile))
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	markerExists, err := s.fsys.Exists(s.path(MarkerFile))
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	// A rotation that did not finish leaves the next file behind; the old
	// anchor log is still complete.
	if ok, _ := s.fsys.Exists(s.path(anchorNextFile)); ok && anchorExists {
		if err := s.fsys.Remove(s.path(anchorNextFile)); err != nil {
			return fmt.Errorf("open journal: remove unfinished anchor rotation: %w", err)
		}
	}
	if q, ok := s.readQuarantineFile(); ok {
		s.setLost(q)
		s.had.file = true
	}
	if anchorExists {
		raw, _, err := s.readAnchor()
		if err != nil {
			return err
		}
		if q, ok := s.scanAnchorQuarantine(raw); ok {
			s.setLost(q)
			s.had.anchor = true
		}
	}

	if !dbExists {
		if anchorExists || markerExists || s.state == StateLost {
			s.setLost(LostJournalMissing)
			return nil
		}
		return s.initFresh()
	}

	if err := s.openBolt(); err != nil {
		return err
	}
	if s.db == nil {
		return nil // lost{journal_corrupt}
	}
	meta, pos, quarantine, err := s.readMeta()
	if err != nil {
		s.setLost(LostJournalCorrupt)
		return nil
	}
	if meta.GetMinReaderVersion() > s.opts.ReaderMax {
		return fmt.Errorf("open journal: %w: the store requires agent schema >= %d, this agent reads up to %d",
			ErrSchemaTooNew, meta.GetMinReaderVersion(), s.opts.ReaderMax)
	}
	copy(s.storeID[:], meta.GetStoreId())
	s.seq, s.head = pos.Seq, pos.Head
	if quarantine != nil {
		s.setLost(parseReason(quarantine.GetReason()))
		s.had.store = true
	}

	if !markerExists && pos.Seq == 0 && s.state != StateLost && s.anchorHasNoCommit(anchorExists) {
		// The very first open crashed before its anchor log or marker was
		// complete: nothing was ever committed or acknowledged. Start again.
		_ = s.closeFiles()
		if anchorExists {
			if err := s.fsys.Remove(s.path(AnchorFile)); err != nil {
				return fmt.Errorf("open journal: remove unfinished anchor log: %w", err)
			}
		}
		if err := os.Remove(s.path(StoreFile)); err != nil {
			return fmt.Errorf("open journal: remove unfinished store: %w", err)
		}
		return s.initFresh()
	}
	if markerExists {
		if err := s.readMarker(); err != nil {
			return err
		}
		if s.marker == nil || !bytes.Equal(s.marker.GetStoreId(), s.storeID[:]) {
			if !guard.Off(guard.MarkerCheck) {
				s.setLost(LostInconsistentStore)
			}
		}
	}
	if !anchorExists {
		s.setLost(LostAnchorMissing)
		return nil
	}
	if err := s.db.View(func(btx *bolt.Tx) error { return verifyChain(btx, pos) }); err != nil {
		s.setLost(LostJournalCorrupt)
		return nil
	}
	if err := s.checkAnchor(pos); err != nil {
		return err
	}
	if s.state == StateLost {
		return nil
	}
	if !markerExists {
		// A store that has committed must find its marker: a missing marker
		// means files were removed or replaced. Only an empty store may get
		// its marker written again.
		if pos.Seq > 0 && !guard.Off(guard.MarkerCheck) {
			s.setLost(LostInconsistentStore)
			return nil
		}
		s.marker = &journalpb.StoreMarker{StoreId: s.storeID[:]}
		if err := s.writeMarker(); err != nil {
			s.setLost(LostIOError)
			return nil
		}
	}
	s.state = StateUnattested
	return nil
}

// anchorHasNoCommit reports whether the anchor log is missing or holds no
// valid commit record.
func (s *Store) anchorHasNoCommit(exists bool) bool {
	if !exists {
		return true
	}
	raw, err := s.fsys.ReadFile(s.path(AnchorFile))
	if err != nil {
		return false
	}
	l, err := ParseAnchorLog(raw)
	if err != nil {
		return false
	}
	for _, r := range l.Records {
		if r.Type == AnchorCommit {
			return false
		}
	}
	return true
}

// readMarker reads and decodes the store marker. A marker that does not
// decode leaves s.marker nil.
func (s *Store) readMarker() error {
	b, err := s.fsys.ReadFile(s.path(MarkerFile))
	if err != nil {
		return fmt.Errorf("open journal: read store marker: %w", err)
	}
	m := &journalpb.StoreMarker{}
	if err := proto.Unmarshal(b, m); err != nil || len(m.GetStoreId()) != 16 {
		s.marker = nil
		return nil
	}
	s.marker = m
	return nil
}

// writeMarker writes s.marker durably.
func (s *Store) writeMarker() error {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(s.marker)
	if err != nil {
		return fmt.Errorf("encode store marker: %w", err)
	}
	return WriteFileDurable(s.fsys, s.opts.Dir, s.path(MarkerFile), b)
}

// MarkerBinding is the host and enrolment binding of the store marker.
type MarkerBinding struct {
	// HostID is the host identity of the active enrolment.
	HostID string
	// EnrolmentID is the active enrolment, or nil.
	EnrolmentID []byte
	// PendingEnrolmentID is an enrolment being activated, or nil.
	PendingEnrolmentID []byte
}

// Marker returns the store marker's binding.
func (s *Store) Marker() MarkerBinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return MarkerBinding{HostID: s.marker.GetHostId(), EnrolmentID: s.marker.GetEnrolmentId(),
		PendingEnrolmentID: s.marker.GetPendingEnrolmentId()}
}

// BindMarker writes the store marker with binding b, durably (file and
// directory fsync). A write failure quarantines the store with LostIOError.
func (s *Store) BindMarker(b MarkerBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return err
	}
	s.marker = &journalpb.StoreMarker{StoreId: s.storeID[:], HostId: b.HostID,
		EnrolmentId: append([]byte(nil), b.EnrolmentID...), PendingEnrolmentId: append([]byte(nil), b.PendingEnrolmentID...)}
	if err := s.writeMarker(); err != nil {
		s.quarantineLocked(LostIOError, "store marker write failed")
		return fmt.Errorf("bind store marker: %w", errors.Join(err, &LostError{Reason: LostIOError}))
	}
	return nil
}

// checkAnchor compares the store position with the anchor log (tail rule),
// repairs a torn tail and re-appends an unacknowledged last commit. It leaves
// the anchor log open for appends when the store is consistent.
func (s *Store) checkAnchor(pos Position) error {
	raw, bounded, err := s.readAnchor()
	if err != nil {
		return err
	}
	log, perr := ParseAnchorLog(raw)
	if !bounded || perr != nil || len(log.Records) == 0 {
		s.setLost(LostAnchorCorrupt)
		return nil
	}
	var last AnchorRecord
	haveCommit := false
	for _, r := range log.Records {
		if r.StoreID != s.storeID {
			s.setLost(LostInconsistentStore)
			return nil
		}
		if r.Type == AnchorQuarantine {
			s.setLost(r.Reason)
			s.had.anchor = true
			continue
		}
		// Commit records only move forward.
		if haveCommit && r.Seq <= last.Seq {
			s.setLost(LostAnchorCorrupt)
			return nil
		}
		last, haveCommit = r, true
	}
	if s.state == StateLost {
		return nil
	}
	if !haveCommit {
		s.setLost(LostAnchorCorrupt)
		return nil
	}
	switch {
	case pos.Seq < last.Seq:
		s.setLost(LostRolledBack)
		return nil
	case pos.Seq > last.Seq+1:
		s.setLost(LostInconsistentStore)
		return nil
	case pos.Seq == last.Seq && pos.Head != last.Head:
		s.setLost(LostInconsistentStore)
		return nil
	case pos.Seq == last.Seq+1:
		// The store committed and the process died before the anchor append:
		// that commit was never acknowledged. Accept it only when the store's
		// head at the anchored position matches the anchor.
		var at [32]byte
		var ok bool
		if err := s.db.View(func(btx *bolt.Tx) error {
			at, ok = headAt(btx, last.Seq)
			return nil
		}); err != nil || !ok || at != last.Head {
			s.setLost(LostInconsistentStore)
			return nil
		}
	}
	f, err := s.fsys.OpenFile(s.path(AnchorFile), os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open journal: open anchor log: %w", err)
	}
	s.anchor = f
	s.anchorSize = int64(log.ValidLen)
	if log.TornTail {
		if err := f.Truncate(int64(log.ValidLen)); err != nil {
			s.setLost(LostIOError)
			return nil
		}
		if err := f.Sync(); err != nil {
			s.setLost(LostIOError)
			return nil
		}
	}
	if pos.Seq == last.Seq+1 {
		if err := s.appendAnchor(AnchorRecord{Type: AnchorCommit, StoreID: s.storeID, Seq: pos.Seq, Head: pos.Head}); err != nil {
			s.setLost(LostIOError)
			return nil
		}
	}
	return nil
}

// checkDirMode refuses a state directory that others may write.
func checkDirMode(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is writable by group or others", ErrInsecureDir, dir)
	}
	return nil
}

// openBolt opens the bbolt store. A failure to open is lost{journal_corrupt},
// except a lock timeout, which is ErrLocked.
func (s *Store) openBolt() error {
	bo := &bolt.Options{Timeout: s.opts.OpenTimeout}
	if testBoltOptions != nil {
		testBoltOptions(bo)
	}
	db, err := bolt.Open(s.path(StoreFile), 0o600, bo)
	if err != nil {
		if errors.Is(err, berrors.ErrTimeout) {
			return fmt.Errorf("open journal: %w", ErrLocked)
		}
		s.setLost(LostJournalCorrupt)
		return nil
	}
	if db.NoSync && !guard.Off(guard.RefuseNoSync) {
		_ = db.Close()
		return fmt.Errorf("open journal: %w", ErrNoSync)
	}
	s.db = db
	return nil
}

// initFresh creates a new store: the bbolt file with its buckets and genesis
// position, then the anchor log with the genesis record, then the store
// marker, each fsynced with its directory.
func (s *Store) initFresh() error {
	if _, err := rand.Read(s.storeID[:]); err != nil {
		return fmt.Errorf("open journal: store id: %w", err)
	}
	if err := s.openBolt(); err != nil {
		return err
	}
	if s.db == nil {
		return errors.New("open journal: cannot create the store")
	}
	meta := &journalpb.StoreMeta{
		StoreId:          s.storeID[:],
		SchemaVersion:    s.opts.SchemaVersion,
		MinReaderVersion: MinReaderVersion,
		WriterVersion:    s.opts.SchemaVersion,
	}
	mb, err := proto.MarshalOptions{Deterministic: true}.Marshal(meta)
	if err != nil {
		return fmt.Errorf("open journal: encode meta: %w", err)
	}
	if err := s.db.Update(func(btx *bolt.Tx) error {
		for _, name := range append([]string{bucketMeta, bucketChain, bucketCheckpoints}, s.opts.Buckets...) {
			if _, err := btx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}
		m := btx.Bucket([]byte(bucketMeta))
		if err := m.Put(keyStoreMeta, mb); err != nil {
			return err
		}
		if err := m.Put(keyPosition, encodePosition(0, [32]byte{})); err != nil {
			return err
		}
		return btx.Bucket([]byte(bucketCheckpoints)).Put(seqKey(0), make([]byte, 32))
	}); err != nil {
		return fmt.Errorf("open journal: initialise store: %w", err)
	}
	if err := s.fsys.SyncDir(s.opts.Dir); err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	f, err := s.fsys.OpenFile(s.path(AnchorFile), os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open journal: create anchor log: %w", err)
	}
	s.anchor = f
	if err := s.appendAnchor(AnchorRecord{Type: AnchorCommit, StoreID: s.storeID}); err != nil {
		return fmt.Errorf("open journal: write anchor log: %w", err)
	}
	if err := s.fsys.SyncDir(s.opts.Dir); err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	s.marker = &journalpb.StoreMarker{StoreId: s.storeID[:]}
	if err := s.writeMarker(); err != nil {
		return fmt.Errorf("open journal: write store marker: %w", err)
	}
	s.state = StateUnattested
	return nil
}

// appendAnchor appends one record and fsyncs the anchor log.
func (s *Store) appendAnchor(r AnchorRecord) error {
	if _, err := s.anchor.Write(r.Encode()); err != nil {
		return fmt.Errorf("append anchor record: %w", err)
	}
	s.anchorSize += AnchorRecordSize
	if guard.Off(guard.AnchorFsync) {
		return nil
	}
	if err := s.anchor.Sync(); err != nil {
		return fmt.Errorf("fsync anchor log: %w", err)
	}
	return nil
}

// readMeta reads the store meta, the position and any quarantine record.
func (s *Store) readMeta() (meta *journalpb.StoreMeta, pos Position, q *journalpb.Quarantine, err error) {
	err = s.db.View(func(btx *bolt.Tx) error {
		m := btx.Bucket([]byte(bucketMeta))
		if m == nil || btx.Bucket([]byte(bucketChain)) == nil || btx.Bucket([]byte(bucketCheckpoints)) == nil {
			return errors.New("journal buckets missing")
		}
		meta = &journalpb.StoreMeta{}
		if err := proto.Unmarshal(m.Get(keyStoreMeta), meta); err != nil {
			return fmt.Errorf("decode store meta: %w", err)
		}
		if len(meta.GetStoreId()) != 16 {
			return errors.New("store meta has no store id")
		}
		var ok bool
		pos, ok = decodePosition(m.Get(keyPosition))
		if !ok {
			return errors.New("store position invalid")
		}
		copy(pos.StoreID[:], meta.GetStoreId())
		if qb := m.Get(keyQuarantine); qb != nil {
			q = &journalpb.Quarantine{}
			if err := proto.Unmarshal(qb, q); err != nil {
				q = &journalpb.Quarantine{Reason: string(LostJournalCorrupt)}
			}
		}
		return nil
	})
	return meta, pos, q, err
}

// readQuarantineFile returns the reason of the quarantine marker file.
func (s *Store) readQuarantineFile() (LostReason, bool) {
	ok, err := s.fsys.Exists(s.path(QuarantineFile))
	if err != nil {
		return LostIOError, true
	}
	if !ok {
		return "", false
	}
	b, err := s.fsys.ReadFile(s.path(QuarantineFile))
	if err != nil {
		return LostIOError, true
	}
	return parseReason(strings.TrimSpace(string(b))), true
}

// setLost moves the store to StateLost. The first reason wins.
func (s *Store) setLost(r LostReason) {
	if s.state != StateLost {
		s.state, s.lost = StateLost, r
	}
}

// State returns the store's trust state and, when lost, the reason.
func (s *Store) State() (State, LostReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.lost
}

// Position returns the store's current chain position.
func (s *Store) Position() Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Position{StoreID: s.storeID, Seq: s.seq, Head: s.head}
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.opts.Dir }

// FS returns the store's file system.
func (s *Store) FS() FS { return s.fsys }

// EstablishContinuity ends the unattested state. With the local store alone,
// continuity is what Open checked: the chain, the anchor log and the markers.
// It then applies pending migrations, and creates missing client buckets, in
// one chained transaction, under the store lock; the store becomes ready only
// when that transaction succeeds. A migration therefore never runs before the
// continuity checks, and no other commit can precede it.
func (s *Store) EstablishContinuity(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("establish continuity: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return err
	}
	if s.state == StateReady {
		return nil
	}
	if err := s.migrateLocked(); err != nil {
		return fmt.Errorf("establish continuity: %w", err)
	}
	s.state = StateReady
	return nil
}

// usableLocked fails for a crashed or closed store.
func (s *Store) usableLocked() error {
	switch {
	case s.closed:
		return ErrClosed
	case s.crashed:
		return ErrCrashed
	case s.state == StateLost:
		return &LostError{Reason: s.lost}
	}
	return nil
}

// CrashPoint is called by the store's clients at their own crash points. It
// returns ErrCrashed, and makes the store refuse every later call, when the
// Crash hook asks for a crash at p.
func (s *Store) CrashPoint(p Point) error {
	if s.opts.Hooks.Crash == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.crashLocked(p)
}

func (s *Store) crashLocked(p Point) error {
	if s.crashed {
		return ErrCrashed
	}
	if s.opts.Hooks.Crash != nil && s.opts.Hooks.Crash(p) {
		s.crashed = true
		return fmt.Errorf("at %s: %w", p, ErrCrashed)
	}
	return nil
}

// Update runs fn in a write transaction and commits it as one chain step.
//
// The order is normative: bbolt commit (fsync) → anchor append (fsync) →
// return. The caller may make the change observable only after Update returns
// nil. A transaction that writes nothing commits nothing and returns the
// current position. Any commit or anchor failure quarantines the store with
// LostIOError; Update never retries a failed fsync.
func (s *Store) Update(ctx context.Context, fn func(tx *Tx) error) (Position, error) {
	if err := ctx.Err(); err != nil {
		return Position{}, fmt.Errorf("journal update: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(fn, false)
}

// txFuncError marks an error that came from inside the transaction function
// (the caller's or the journal's own writes), as opposed to the commit.
type txFuncError struct{ err error }

func (e *txFuncError) Error() string { return e.err.Error() }
func (e *txFuncError) Unwrap() error { return e.err }

// updateLocked commits fn. migrating lets the migration transaction of
// EstablishContinuity through before the store is ready.
func (s *Store) updateLocked(fn func(tx *Tx) error, migrating bool) (pos Position, err error) {
	if err := s.usableLocked(); err != nil {
		return Position{}, err
	}
	if s.state != StateReady && !migrating && !guard.Off(guard.CommitAfterContinuity) {
		return Position{}, ErrUnattested
	}
	if s.pendingAnchor != nil {
		// Mutation only: the previous commit's anchor record lands now.
		if _, werr := s.anchor.Write(s.pendingAnchor); werr == nil {
			_ = s.anchor.Sync()
			s.anchorSize += AnchorRecordSize
		}
		s.pendingAnchor = nil
	}
	defer func() {
		if r := recover(); r != nil {
			s.quarantineLocked(LostJournalCorrupt, "store read failed")
			pos, err = Position{}, &LostError{Reason: LostJournalCorrupt}
		}
	}()

	seq := s.seq + 1
	var head [32]byte
	empty := false
	errEmpty := errors.New("empty transaction")
	berr := s.db.Update(func(btx *bolt.Tx) error {
		tx := &Tx{btx: btx, writable: true, seq: seq}
		if err := fn(tx); err != nil {
			return &txFuncError{err: err}
		}
		if len(tx.writes) == 0 {
			empty = true
			return &txFuncError{err: errEmpty}
		}
		digest := txDigest(tx.writes)
		head = nextHead(s.head, seq, digest)
		if err := btx.Bucket([]byte(bucketChain)).Put(seqKey(seq), chainEntry(digest, head)); err != nil {
			return &txFuncError{err: fmt.Errorf("write chain entry: %w", err)}
		}
		if seq%s.opts.CheckpointEvery == 0 {
			if err := checkpoint(btx, seq, head, s.opts.CheckpointEvery); err != nil {
				return &txFuncError{err: err}
			}
		}
		if err := btx.Bucket([]byte(bucketMeta)).Put(keyPosition, encodePosition(seq, head)); err != nil {
			return &txFuncError{err: fmt.Errorf("write position: %w", err)}
		}
		if s.opts.Hooks.StoreCommitError != nil {
			if err := s.opts.Hooks.StoreCommitError(); err != nil {
				// Not a txFuncError: treated as a failed commit.
				return &commitError{err: err}
			}
		}
		return nil
	})
	if empty {
		return Position{StoreID: s.storeID, Seq: s.seq, Head: s.head}, nil
	}
	if berr != nil {
		var fe *txFuncError
		if errors.As(berr, &fe) {
			return Position{}, fe.err
		}
		// Anything else came from bbolt's commit: a page write, an fdatasync
		// (returned by bbolt as a bare errno), or the meta write. The state of
		// the file and of bbolt's in-memory meta is unknown; the store is
		// never trusted again.
		if guard.Off(guard.CommitErrorQuarantine) {
			return Position{}, berr
		}
		s.quarantineLocked(LostIOError, "store commit failed")
		return Position{}, fmt.Errorf("journal commit: %w", errors.Join(berr, &LostError{Reason: LostIOError}))
	}
	s.seq, s.head = seq, head
	if err := s.crashLocked(PointAfterStoreCommit); err != nil {
		return Position{}, err
	}
	rec := AnchorRecord{Type: AnchorCommit, StoreID: s.storeID, Seq: seq, Head: head}
	if guard.Off(guard.AnchorBeforeAck) {
		s.pendingAnchor = rec.Encode()
	} else if err := s.appendAnchor(rec); err != nil {
		s.quarantineLocked(LostIOError, "anchor append failed")
		return Position{}, fmt.Errorf("journal commit: %w", &LostError{Reason: LostIOError})
	}
	if s.anchorSize >= s.opts.AnchorRotateBytes {
		if err := s.rotateLocked(rec); err != nil {
			return Position{}, err
		}
	}
	if err := s.crashLocked(PointAfterAnchor); err != nil {
		return Position{}, err
	}
	return Position{StoreID: s.storeID, Seq: seq, Head: head}, nil
}

// commitError marks an injected commit failure.
type commitError struct{ err error }

func (e *commitError) Error() string { return "injected commit failure: " + e.err.Error() }
func (e *commitError) Unwrap() error { return e.err }

// rotateLocked replaces the anchor log with a new one whose first record
// repeats the old log's last record. The new file is written and fsynced
// before it atomically replaces the old one; the directory is fsynced after.
func (s *Store) rotateLocked(last AnchorRecord) error {
	renamed := false
	fail := func(what string, err error) error {
		if renamed {
			// The old descriptor names an unlinked file: record the
			// quarantine in the log that is now at the path.
			_ = s.anchor.Close()
			s.anchor = nil
			if a, oerr := s.fsys.OpenFile(s.path(AnchorFile), os.O_RDWR|os.O_APPEND, 0o600); oerr == nil {
				s.anchor = a
			}
		}
		s.quarantineLocked(LostIOError, "anchor rotation failed")
		return fmt.Errorf("journal anchor rotation: %s: %w", what, errors.Join(err, &LostError{Reason: LostIOError}))
	}
	next := s.path(anchorNextFile)
	f, err := s.fsys.OpenFile(next, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail("create", err)
	}
	if _, err := f.Write(last.Encode()); err != nil {
		_ = f.Close()
		return fail("write", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fail("fsync", err)
	}
	if err := f.Close(); err != nil {
		return fail("close", err)
	}
	if err := s.crashLocked(PointMidAnchorRotation); err != nil {
		return err
	}
	if err := s.fsys.Rename(next, s.path(AnchorFile)); err != nil {
		return fail("rename", err)
	}
	renamed = true
	if err := s.fsys.SyncDir(s.opts.Dir); err != nil {
		return fail("fsync directory", err)
	}
	_ = s.anchor.Close()
	a, err := s.fsys.OpenFile(s.path(AnchorFile), os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fail("reopen", err)
	}
	s.anchor, s.anchorSize = a, AnchorRecordSize
	return nil
}

// Quarantine moves the store to StateLost with reason and records the
// quarantine durably: a quarantine marker file, a quarantine record in the
// anchor log and, when the store is still writable, a quarantine record in
// the store (outside the chain), and reports it to Options.Events. Each
// record is enough to keep the store quarantined across a restart. Write
// errors are reported in the event detail and never retried.
func (s *Store) Quarantine(reason LostReason, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.crashed || s.closed {
		return
	}
	s.quarantineLocked(reason, detail)
}

func (s *Store) quarantineLocked(reason LostReason, detail string) {
	ioFailure := reason == LostIOError
	if s.state == StateLost && s.lost != reason {
		reason = s.lost
	}
	s.state, s.lost = StateLost, reason
	var errs []error
	if !guard.Off(guard.QuarantinePersist) {
		if err := WriteFileDurable(s.fsys, s.opts.Dir, s.path(QuarantineFile), []byte(reason)); err != nil {
			errs = append(errs, err)
		}
		if s.anchor != nil {
			if err := s.appendAnchor(AnchorRecord{Type: AnchorQuarantine, Reason: reason, StoreID: s.storeID, Seq: s.seq, Head: s.head}); err != nil {
				errs = append(errs, err)
			}
		}
		if s.db != nil && reason != LostIOError {
			if err := s.writeStoreQuarantine(reason, detail); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if s.opts.Events != nil {
		d := detail
		if len(errs) > 0 {
			d = fmt.Sprintf("%s (not every quarantine record was written: %v)", detail, errors.Join(errs...))
		}
		s.opts.Events(Event{Kind: EventQuarantine, Reason: reason, Seq: s.seq, Detail: d})
	}
	if ioFailure && s.db != nil && !guard.Off(guard.CloseAfterIOError) {
		// After a failed commit, bbolt's in-memory meta may show a
		// transaction that never reached the disk. Stop reading through it:
		// reads of this store are refused until it is reopened from disk.
		_ = s.db.Close()
		s.db = nil
	}
}

// writeStoreQuarantine puts the quarantine record into the store's meta
// bucket, outside the chain.
func (s *Store) writeStoreQuarantine(reason LostReason, detail string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("store write panicked")
		}
	}()
	qb, err := proto.Marshal(&journalpb.Quarantine{Reason: string(reason), Seq: s.seq, Detail: detail})
	if err != nil {
		return fmt.Errorf("encode quarantine: %w", err)
	}
	return s.db.Update(func(btx *bolt.Tx) error {
		m := btx.Bucket([]byte(bucketMeta))
		if m == nil {
			return errors.New("meta bucket missing")
		}
		return m.Put(keyQuarantine, qb)
	})
}

// View runs fn in a read transaction. Reads are served in every state in
// which the store could be opened, including StateLost.
func (s *Store) View(ctx context.Context, fn func(tx *Tx) error) (err error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("journal view: %w", err)
	}
	s.mu.Lock()
	db := s.db
	bad := s.closed || s.crashed
	s.mu.Unlock()
	if bad {
		return ErrClosed
	}
	if db == nil {
		_, reason := s.State()
		if reason == "" {
			reason = LostJournalCorrupt
		}
		return &LostError{Reason: reason}
	}
	defer func() {
		if r := recover(); r != nil {
			s.Quarantine(LostJournalCorrupt, "store read failed")
			err = &LostError{Reason: LostJournalCorrupt}
		}
	}()
	return db.View(func(btx *bolt.Tx) error { return fn(&Tx{btx: btx}) })
}

// VerifyChain recomputes the chain from the last checkpoint to the current
// position.
func (s *Store) VerifyChain(ctx context.Context) error {
	return s.View(ctx, func(tx *Tx) error {
		m := tx.btx.Bucket([]byte(bucketMeta))
		pos, ok := decodePosition(m.Get(keyPosition))
		if !ok {
			return errors.New("store position invalid")
		}
		return verifyChain(tx.btx, pos)
	})
}

// LiveSize returns the bytes the store's live pages use: its size less its
// free and pending pages. Unlike Size, it falls when records are deleted.
func (s *Store) LiveSize() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0
	}
	var n int64
	_ = s.db.View(func(btx *bolt.Tx) error {
		n = btx.Size()
		return nil
	})
	st := s.db.Stats()
	free := int64(st.FreePageN+st.PendingPageN) * int64(s.db.Info().PageSize)
	if free > n {
		return 0
	}
	return n - free
}

// Size returns the size of the bbolt store in bytes.
func (s *Store) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0
	}
	var n int64
	_ = s.db.View(func(btx *bolt.Tx) error {
		n = btx.Size()
		return nil
	})
	return n
}

// Close closes the store's files. It writes nothing.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.closeFiles(), s.lock.release())
}

func (s *Store) closeFiles() error {
	var errs []error
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			errs = append(errs, err)
		}
		s.db = nil
	}
	if s.anchor != nil {
		if err := s.anchor.Close(); err != nil {
			errs = append(errs, err)
		}
		s.anchor = nil
	}
	return errors.Join(errs...)
}

// PositionSize is the size of an encoded Position.
const PositionSize = 16 + 8 + 32

// MarshalBinary encodes the position as store_id || be64(seq) || head
// (PositionSize bytes), the form an agent signs and a manager compares.
func (p Position) MarshalBinary() ([]byte, error) {
	b := make([]byte, PositionSize)
	copy(b, p.StoreID[:])
	binary.BigEndian.PutUint64(b[16:], p.Seq)
	copy(b[24:], p.Head[:])
	return b, nil
}

// UnmarshalBinary decodes a position encoded by MarshalBinary.
func (p *Position) UnmarshalBinary(b []byte) error {
	if len(b) != PositionSize {
		return fmt.Errorf("journal position: %d bytes, want %d", len(b), PositionSize)
	}
	copy(p.StoreID[:], b[:16])
	p.Seq = binary.BigEndian.Uint64(b[16:24])
	copy(p.Head[:], b[24:])
	return nil
}

// encodePosition is be64(seq) || head.
func encodePosition(seq uint64, head [32]byte) []byte {
	b := make([]byte, 40)
	binary.BigEndian.PutUint64(b, seq)
	copy(b[8:], head[:])
	return b
}

func decodePosition(b []byte) (Position, bool) {
	var p Position
	if len(b) != 40 {
		return p, false
	}
	p.Seq = binary.BigEndian.Uint64(b)
	copy(p.Head[:], b[8:])
	return p, true
}

func fileExists(name string) (bool, error) { return OSFS{}.Exists(name) }
