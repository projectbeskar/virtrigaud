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

package enrol

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
)

// atomicFiles are the files of the key material directory written through
// writeAtomic, whose temporary files RemoveStrayTemps removes.
var atomicFiles = []string{trust.FileHostCert, FileStartEpoch, FileRenewalCSR, FileRenewalSince, FileRenewalCert}

// tempNonceLen is the length of a temporary file's random suffix: 8 bytes in
// hexadecimal.
const tempNonceLen = 16

// RemoveStrayTemps removes from pkiDir the temporary files that a write of
// one of the key material directory's files left behind when the process
// was killed between creating the file and renaming it into place:
// ".<file>.<16 lowercase hex>" for host.crt, start-epoch and the renewal
// files, regular files only. Nothing else is touched. The agent calls it at
// start, holding its state directory's lock, so no write of its own is in
// flight; a concurrent "vrtg-agent enroll install" (which an operator runs
// with the agent stopped) would lose its temporary file and fail without
// installing anything. A pkiDir that is not a real directory (a symbolic
// link, say) is left alone; the host checks refuse it at start. Errors are
// ignored: a file that cannot be removed is never in the way, because every
// write draws a new name. It returns the names removed.
func RemoveStrayTemps(pkiDir string) []string {
	if fi, err := os.Lstat(pkiDir); err != nil || !fi.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(pkiDir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range entries {
		if !isStrayTemp(e.Name()) {
			continue
		}
		p := filepath.Join(pkiDir, e.Name())
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && os.Remove(p) == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}

// isStrayTemp reports whether name is a temporary file of one of
// atomicFiles.
func isStrayTemp(name string) bool {
	for _, f := range atomicFiles {
		rest, ok := strings.CutPrefix(name, "."+f+".")
		if !ok || len(rest) != tempNonceLen {
			continue
		}
		if strings.Trim(rest, "0123456789abcdef") == "" {
			return true
		}
	}
	return false
}
