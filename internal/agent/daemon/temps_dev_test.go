//go:build virtrigaud_dev

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

package daemon_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStartRemovesStrayTemps: a temporary file an interrupted write left in
// the key material directory is removed at start; the files the agent reads
// stay.
func TestStartRemovesStrayTemps(t *testing.T) {
	h := newHost(t)
	stray := filepath.Join(h.cfg.PKIDir(), ".start-epoch.9aa53d29ce613652")
	if err := os.WriteFile(stray, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := h.run()
	waitLog(t, h, "removed temporary files left by an interrupted write")
	if _, err := os.Lstat(stray); !os.IsNotExist(err) {
		t.Fatalf("the stray file is still there: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(h.cfg.PKIDir(), "host.crt")); err != nil {
		t.Fatal(err)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}
