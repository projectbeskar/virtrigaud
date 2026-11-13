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

// enrollNoStepStatus is the exit status of "enroll" with no step: a release
// build has none.
const enrollNoStepStatus = 1

// TestReleaseBuildHasNoEnrolment: a release build's enroll steps do nothing:
// exit 1, a message naming the development build, no key and no file.
func TestReleaseBuildHasNoEnrolment(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "host.key")
	out := filepath.Join(dir, "host.csr")
	for _, args := range [][]string{
		{"enroll"}, {"enroll", "csr", "--key-file=" + key, "--out=" + out}, {"enroll", "install", "--csr=x", "--certificate=y"},
	} {
		var stdout, stderr bytes.Buffer
		if code := realMain(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "development builds") {
			t.Errorf("%v: exit %d\n%s", args, code, stderr.String())
		}
	}
	for _, p := range []string{key, out} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
}
