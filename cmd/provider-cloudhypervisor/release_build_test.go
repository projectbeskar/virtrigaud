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
	"strings"
	"testing"
)

// TestReleaseBuildRefusesToRun: a release build does not run the Provider,
// whatever its flags.
func TestReleaseBuildRefusesToRun(t *testing.T) {
	for _, args := range [][]string{nil, {"--port=9443"}, {"--hosts-file=/srv/hosts.json", "--agent-client-cert=c", "--agent-client-key=k"}} {
		var out, errOut bytes.Buffer
		if code := realMain(args, &out, &errOut); code != exitError || !strings.Contains(errOut.String(), "development builds only") {
			t.Errorf("%v: exit %d\n%s", args, code, errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := realMain([]string{"--version"}, &out, &errOut); code != exitOK || !strings.Contains(out.String(), "not runnable") {
		t.Errorf("--version: exit %d\n%s", code, out.String())
	}
}
