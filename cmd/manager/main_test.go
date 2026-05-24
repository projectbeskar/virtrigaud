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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/version"
)

// TestVersionString verifies the banner emitted by `--version`.
//
// We test versionString() directly rather than invoking main() with
// os.Args = []string{"manager", "--version"}, because the production
// handler exits the process via os.Exit(0). Subprocess execution
// (os/exec on the built binary) is exercised by `make build` + manual
// run; the unit test pins the *content* of the banner so dashboards or
// release-verification scripts that grep for "virtrigaud-manager"
// continue to work.
//
// Pinned by H1 PR-1 / #114.
func TestVersionString(t *testing.T) {
	s := versionString()

	require.NotEmpty(t, s, "versionString() must never return empty")
	assert.True(t, strings.HasPrefix(s, "virtrigaud-manager "),
		"banner must start with 'virtrigaud-manager ' so release-verification grep patterns keep working; got %q", s)

	tail := strings.TrimPrefix(s, "virtrigaud-manager ")
	assert.NotEmpty(t, tail,
		"banner must include the version.String() payload after the prefix; got bare %q", s)

	// Pin that we delegate to internal/version.String() so the two
	// callers (this banner + virtrigaud_build_info metric label) stay
	// in lockstep. If someone refactors versionString() to embed a
	// hardcoded string, this catches the regression.
	assert.Equal(t, "virtrigaud-manager "+version.String(), s,
		"banner must be 'virtrigaud-manager ' + version.String() verbatim")
}
