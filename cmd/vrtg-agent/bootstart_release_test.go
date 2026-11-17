//go:build !virtrigaud_dev

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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseBuildHasNoBootStart: a release build's boot-start does
// nothing: exit 1, a message naming the development build, no marker.
func TestReleaseBuildHasNoBootStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNTIME_DIRECTORY", dir)
	var stdout, stderr bytes.Buffer
	if code := realMain([]string{"boot-start"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "development builds") {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(dir, "started")); err == nil {
		t.Fatal("a marker was created")
	}
}
