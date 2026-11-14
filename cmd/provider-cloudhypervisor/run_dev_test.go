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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDevelopmentFlags: the agent client certificate is required, and a
// certificate or inventory that cannot be read stops the Provider.
func TestDevelopmentFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := realMain([]string{"--port=1"}, &out, &errOut); code != exitUsage || !strings.Contains(errOut.String(), "--agent-client-cert") {
		t.Errorf("no client certificate: exit %d\n%s", code, errOut.String())
	}
	errOut.Reset()
	if code := realMain([]string{"--agent-client-cert=c", "--agent-client-key=k", "extra"}, &out, &errOut); code != exitUsage {
		t.Errorf("extra argument: exit %d", code)
	}
	errOut.Reset()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.pem")
	if code := realMain([]string{"--agent-client-cert=" + missing, "--agent-client-key=" + missing,
		"--hosts-file=" + filepath.Join(dir, "hosts.json")}, &out, &errOut); code != exitError {
		t.Errorf("unreadable certificate: exit %d\n%s", code, errOut.String())
	}
	out.Reset()
	if code := realMain([]string{"--version"}, &out, &errOut); code != exitOK || !strings.Contains(out.String(), "development build") {
		t.Errorf("--version: exit %d\n%s", code, out.String())
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("the test expects the certificate to be missing")
	}
}
