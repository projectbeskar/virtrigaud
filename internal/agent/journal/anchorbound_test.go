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
	"io/fs"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
)

// oversizeAnchorFS reports the anchor log as larger than the bound and counts
// every attempt to read it.
type oversizeAnchorFS struct {
	*journaltest.MemFS
	reads atomic.Int32
}

func isAnchor(name string) bool { return filepath.Base(name) == journal.AnchorFile }

func (o *oversizeAnchorFS) Size(name string) (int64, error) {
	if isAnchor(name) {
		return int64(journal.MaxAnchorBytesForTest) + 1, nil
	}
	return o.MemFS.Size(name)
}

func (o *oversizeAnchorFS) ReadFile(name string) ([]byte, error) {
	if isAnchor(name) {
		o.reads.Add(1)
	}
	return o.MemFS.ReadFile(name)
}

func (o *oversizeAnchorFS) OpenFile(name string, flag int, perm fs.FileMode) (journal.File, error) {
	if isAnchor(name) {
		o.reads.Add(1)
	}
	return o.MemFS.OpenFile(name, flag, perm)
}

// TestAnchorLogSizeCheckedBeforeRead: an anchor log above the bound is
// anchor_corrupt, and Open never opens or reads it.
func TestAnchorLogSizeCheckedBeforeRead(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()

	big := &oversizeAnchorFS{MemFS: e.mem}
	o := e.opts
	o.Dir, o.FS = e.dir, big
	st, err := journal.Open(t.Context(), o)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if state, r := st.State(); state != journal.StateLost || r != journal.LostAnchorCorrupt {
		t.Fatalf("state = %v{%s}, want lost{%s}", state, r, journal.LostAnchorCorrupt)
	}
	// The quarantine append opens the log for writing; no open may read it.
	if n := big.reads.Load(); n > 1 {
		t.Fatalf("anchor log opened %d times; an oversized log must not be read", n)
	}
}

// TestAnchorLogReadIsBounded: a log that grows past the bound between its
// size check and its read is still read only up to the bound.
func TestAnchorLogReadIsBounded(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	p := filepath.Join(e.dir, journal.AnchorFile)
	raw, _ := e.mem.ReadFile(p)
	grown := append(raw, []byte(strings.Repeat("x", int(journal.MaxAnchorBytesForTest)))...)
	e.mem.Put(p, grown)
	small := &smallSizeFS{MemFS: e.mem}
	o := e.opts
	o.Dir, o.FS = e.dir, small
	st, err := journal.Open(t.Context(), o)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if state, r := st.State(); state != journal.StateLost || r != journal.LostAnchorCorrupt {
		t.Fatalf("state = %v{%s}, want lost{%s}", state, r, journal.LostAnchorCorrupt)
	}
}

// smallSizeFS under-reports the anchor log's size, as a log that grew after
// its size was taken.
type smallSizeFS struct{ *journaltest.MemFS }

func (s *smallSizeFS) Size(name string) (int64, error) {
	if isAnchor(name) {
		return 1, nil
	}
	return s.MemFS.Size(name)
}
