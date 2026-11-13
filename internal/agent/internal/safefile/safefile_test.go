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

package safefile_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/safefile"
)

func TestRead(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	me := uint32(os.Getuid()) // #nosec G115 -- a uid is 32 bits
	other := me + 1
	if b, err := safefile.Read(f, safefile.Policy{Owner: &me, DenyMode: 0o077, Max: 4}); err != nil || string(b) != "data" {
		t.Fatalf("read = %q, %v", b, err)
	}
	link := filepath.Join(dir, "l")
	if err := os.Symlink(f, link); err != nil {
		t.Fatal(err)
	}
	if _, err := safefile.Read(link, safefile.Policy{Max: 4}); err == nil {
		t.Error("read through a symbolic link")
	}
	for name, p := range map[string]safefile.Policy{
		"too large":   {Max: 3},
		"other owner": {Owner: &other, Max: 4},
		"mode":        {DenyMode: 0o600, Max: 4},
	} {
		if _, err := safefile.Read(f, p); !errors.Is(err, safefile.ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := safefile.Read(dir, safefile.Policy{Max: 4}); !errors.Is(err, safefile.ErrRefused) {
		t.Errorf("directory: %v", err)
	}
	if _, err := safefile.Read(f, safefile.Policy{}); err == nil {
		t.Error("no bound accepted")
	}
}

func TestReadOwnersAny(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	me := uint32(os.Geteuid()) // #nosec G115 -- a uid is 32 bits
	if _, err := safefile.Read(path, safefile.Policy{Owners: []uint32{me + 1, me}, Max: 10}); err != nil {
		t.Fatalf("an owner in the list: %v", err)
	}
	if _, err := safefile.Read(path, safefile.Policy{Owners: []uint32{me + 1, me + 2}, Max: 10}); !errors.Is(err, safefile.ErrRefused) {
		t.Fatalf("an owner not in the list: %v", err)
	}
}
