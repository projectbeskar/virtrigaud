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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
)

func isAnchor(name string) bool { return filepath.Base(name) == journal.AnchorFile }

// sizedFS reports, for the anchor log opened for reading, the size its
// fstat returns (size), and counts the bytes read from it.
type sizedFS struct {
	*journaltest.MemFS
	size  int64
	read  atomic.Int64
	most  atomic.Int64
	plain atomic.Int32
}

type sizedFile struct {
	journal.File
	fs   *sizedFS
	this *atomic.Int64
}

func (f sizedFile) Stat() (fs.FileInfo, error) {
	fi, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return sizedInfo{FileInfo: fi, size: f.fs.size}, nil
}

func (f sizedFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.fs.read.Add(int64(n))
	if total := f.this.Add(int64(n)); total > f.fs.most.Load() {
		f.fs.most.Store(total)
	}
	return n, err
}

type sizedInfo struct {
	fs.FileInfo
	size int64
}

func (i sizedInfo) Size() int64 { return i.size }

func (s *sizedFS) OpenFile(name string, flag int, perm fs.FileMode) (journal.File, error) {
	f, err := s.MemFS.OpenFile(name, flag, perm)
	if err != nil || !isAnchor(name) || flag != os.O_RDONLY {
		return f, err
	}
	return sizedFile{File: f, fs: s, this: new(atomic.Int64)}, nil
}

func (s *sizedFS) ReadFile(name string) ([]byte, error) {
	if isAnchor(name) {
		s.plain.Add(1)
	}
	return s.MemFS.ReadFile(name)
}

func reopen(t *testing.T, e *env, fsys journal.FS) *journal.Store {
	t.Helper()
	o := e.opts
	o.Dir, o.FS = e.dir, fsys
	st, err := journal.Open(t.Context(), o)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestAnchorLogSizeCheckedBeforeRead: an anchor log whose open file is above
// the bound is anchor_corrupt, and not a byte of it is read.
func TestAnchorLogSizeCheckedBeforeRead(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	big := &sizedFS{MemFS: e.mem, size: int64(journal.MaxAnchorBytesForTest) + 1}
	st := reopen(t, e, big)
	if state, r := st.State(); state != journal.StateLost || r != journal.LostAnchorCorrupt {
		t.Fatalf("state = %v{%s}, want lost{%s}", state, r, journal.LostAnchorCorrupt)
	}
	if n := big.read.Load(); n != 0 {
		t.Fatalf("%d bytes of an oversized anchor log were read", n)
	}
	if n := big.plain.Load(); n != 0 {
		t.Fatalf("the anchor log was read whole %d times", n)
	}
}

// TestAnchorLogReadIsBounded: a log that grows past the bound after its size
// was taken is read only up to the bound.
func TestAnchorLogReadIsBounded(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	_ = s.Close()
	p := filepath.Join(e.dir, journal.AnchorFile)
	raw, _ := e.mem.ReadFile(p)
	e.mem.Put(p, append(raw, []byte(strings.Repeat("x", journal.MaxAnchorBytesForTest))...))
	small := &sizedFS{MemFS: e.mem, size: 1}
	st := reopen(t, e, small)
	if state, r := st.State(); state != journal.StateLost || r != journal.LostAnchorCorrupt {
		t.Fatalf("state = %v{%s}, want lost{%s}", state, r, journal.LostAnchorCorrupt)
	}
	if n := small.most.Load(); n > int64(journal.MaxAnchorBytesForTest)+1 {
		t.Fatalf("%d bytes read from one open, more than the bound", n)
	}
}
