/*
Copyright 2025.

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

package scaffold

// Tests of hack/module-versions.sh, the release script whose versions the
// scaffold pins: it tags the sdk and proto modules that a generated go.mod
// requires (docs/go-modules.md).

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moduleVersionsScript returns the absolute path of hack/module-versions.sh.
func moduleVersionsScript(t *testing.T) string {
	t.Helper()
	for _, tool := range []string{"bash", "git", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	path, err := filepath.Abs(filepath.Join("..", "..", "hack", "module-versions.sh"))
	require.NoError(t, err)
	require.FileExists(t, path)
	return path
}

// isolatedGitEnv is an environment in which git reads no user or system
// configuration (no hooks, no signing) and has an identity.
func isolatedGitEnv(t *testing.T) []string {
	t.Helper()
	env := []string{
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "GITHUB_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// runIn runs a command in dir with the isolated git environment and returns
// its combined output and error.
func runIn(t *testing.T, dir string, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = isolatedGitEnv(t)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustRunIn is runIn that fails the test on an error.
func mustRunIn(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	out, err := runIn(t, dir, name, args...)
	require.NoError(t, err, "%s %s: %s", name, strings.Join(args, " "), out)
	return out
}

// goodGoMods are go.mod files that follow the lockstep rule for v0.9.0.
var goodGoMods = map[string]string{
	"go.mod": `module github.com/projectbeskar/virtrigaud

go 1.26

replace github.com/projectbeskar/virtrigaud/proto => ./proto

replace github.com/projectbeskar/virtrigaud/sdk => ./sdk

require (
	github.com/projectbeskar/virtrigaud/proto v0.9.0
	github.com/projectbeskar/virtrigaud/sdk v0.9.0
	example.com/other v1.0.0
)
`,
	"sdk/go.mod": `module github.com/projectbeskar/virtrigaud/sdk

go 1.26

require (
	github.com/projectbeskar/virtrigaud v0.9.0
	github.com/projectbeskar/virtrigaud/proto v0.9.0
)

replace github.com/projectbeskar/virtrigaud/proto => ../proto

replace github.com/projectbeskar/virtrigaud => ../
`,
	"proto/go.mod": `module github.com/projectbeskar/virtrigaud/proto

go 1.26

require google.golang.org/grpc v1.0.0
`,
}

// fixtureRepo creates a git repository holding the given go.mod files
// (goodGoMods with overrides) in one commit and returns its directory.
func fixtureRepo(t *testing.T, overrides map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	mustRunIn(t, dir, "git", "init", "-q", "-b", "main")
	for path, content := range goodGoMods {
		if o, ok := overrides[path]; ok {
			content = o
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(content), 0o600))
	}
	mustRunIn(t, dir, "git", "add", "-A")
	mustRunIn(t, dir, "git", "commit", "-q", "-m", "fixture")
	return dir
}

func TestReleaseVersionPatternMatchesScript(t *testing.T) {
	script := moduleVersionsScript(t)
	data, err := os.ReadFile(script) //nolint:gosec // the repository's own script
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^readonly RELEASE_VERSION_RE='([^']+)'$`).FindSubmatch(data)
	require.NotNil(t, m, "RELEASE_VERSION_RE not found in %s", script)
	assert.Equal(t, releaseVersionPattern.String(), string(m[1]))

	for _, v := range []string{"v0.4.1", "v0.4.1-rc1", "v1.2.3-beta.2", "dev", "v2.0.0", "v0.4.0-3-gabc1234", "v0.4.1\nx"} {
		_, err := runIn(t, t.TempDir(), "bash", script, "validate", v)
		assert.Equal(t, IsReleaseVersion(v), err == nil, "validate %q", v)
	}
}

// TestRepositoryModuleVersionsAreConsistent keeps the repository's own go.mod
// files releasable: every in-repo require names one release version and the
// replace directives point at the working tree.
func TestRepositoryModuleVersionsAreConsistent(t *testing.T) {
	script := moduleVersionsScript(t)
	out, err := runIn(t, filepath.Join("..", ".."), "bash", script, "consistent")
	require.NoError(t, err, out)
	assert.True(t, IsReleaseVersion(strings.TrimSpace(out)), out)
}

func TestModuleVersionsCheck(t *testing.T) {
	script := moduleVersionsScript(t)
	tests := []struct {
		name      string
		overrides map[string]string
		wantErr   string
	}{
		{name: "lockstep"},
		{
			name: "single-line requires, require( without a space, comments and look-alikes",
			overrides: map[string]string{"sdk/go.mod": `module github.com/projectbeskar/virtrigaud/sdk

go 1.26

require github.com/projectbeskar/virtrigaud v0.9.0 // indirect
// require github.com/projectbeskar/virtrigaud/proto v0.1.0
require(
	github.com/projectbeskar/virtrigaud/proto v0.9.0
	github.com/projectbeskar/virtrigaud/sdkx v0.1.0
)

exclude (
	github.com/projectbeskar/virtrigaud/proto v0.1.0
)

replace (
	github.com/projectbeskar/virtrigaud/proto => ../proto
	github.com/projectbeskar/virtrigaud v0.9.0 => ..
)
`},
		},
		{
			name: "a require at another version",
			overrides: map[string]string{"go.mod": strings.Replace(goodGoMods["go.mod"],
				"virtrigaud/sdk v0.9.0", "virtrigaud/sdk v0.8.0", 1)},
			wantErr: "go.mod requires github.com/projectbeskar/virtrigaud/sdk v0.8.0, want v0.9.0",
		},
		{
			name: "proto requiring another VirtRigaud module",
			overrides: map[string]string{"proto/go.mod": goodGoMods["proto/go.mod"] +
				"\nrequire github.com/projectbeskar/virtrigaud/sdk v0.9.0\n"},
			wantErr: "the proto module must not depend on the other VirtRigaud modules",
		},
		{
			name: "sdk without proto",
			overrides: map[string]string{"sdk/go.mod": strings.Replace(goodGoMods["sdk/go.mod"],
				"\tgithub.com/projectbeskar/virtrigaud/proto v0.9.0\n", "", 1)},
			wantErr: "sdk/go.mod does not require github.com/projectbeskar/virtrigaud/proto",
		},
		{
			name: "a replace pointing outside the working tree",
			overrides: map[string]string{"go.mod": strings.Replace(goodGoMods["go.mod"],
				"virtrigaud/sdk => ./sdk", "virtrigaud/sdk => example.com/fork/sdk v0.9.0", 1)},
			wantErr: "go.mod replaces the VirtRigaud modules with",
		},
		{
			name: "a missing replace",
			overrides: map[string]string{"sdk/go.mod": strings.Replace(goodGoMods["sdk/go.mod"],
				"replace github.com/projectbeskar/virtrigaud => ../\n", "", 1)},
			wantErr: "sdk/go.mod replaces the VirtRigaud modules with",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := fixtureRepo(t, tt.overrides)
			out, err := runIn(t, dir, "bash", script, "check", "v0.9.0")
			if tt.wantErr == "" {
				require.NoError(t, err, out)
				// The same files at a commit.
				out, err = runIn(t, dir, "bash", script, "check", "v0.9.0", "HEAD")
				require.NoError(t, err, out)
				return
			}
			require.Error(t, err, out)
			assert.Contains(t, out, tt.wantErr)
		})
	}
}

func TestModuleVersionsSet(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	script := moduleVersionsScript(t)
	dir := fixtureRepo(t, nil)
	mustRunIn(t, dir, "bash", script, "set", "v0.9.1-rc1")
	mustRunIn(t, dir, "bash", script, "check", "v0.9.1-rc1")
	out, err := runIn(t, dir, "bash", script, "set", "dev")
	require.Error(t, err, out)
}

// remoteTag returns the commit a tag of the bare repository remote points at.
func remoteTag(t *testing.T, remote, tag string) string {
	t.Helper()
	out, err := runIn(t, remote, "git", "rev-parse", "-q", "--verify", "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func TestModuleVersionsTag(t *testing.T) {
	script := moduleVersionsScript(t)
	dir := fixtureRepo(t, nil)
	remote := t.TempDir()
	mustRunIn(t, remote, "git", "init", "-q", "--bare")
	mustRunIn(t, dir, "git", "remote", "add", "origin", remote)
	mustRunIn(t, dir, "git", "tag", "-a", "v0.9.0", "-m", "release v0.9.0")
	mustRunIn(t, dir, "git", "push", "-q", "origin", "main", "refs/tags/v0.9.0")
	release := strings.TrimSpace(mustRunIn(t, dir, "git", "rev-parse", "HEAD"))

	// A second commit for the tags that must not move.
	mustRunIn(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "later")
	later := strings.TrimSpace(mustRunIn(t, dir, "git", "rev-parse", "HEAD"))

	tag := func(args ...string) (string, error) {
		return runIn(t, dir, "bash", append([]string{script, "tag", "v0.9.0"}, args...)...)
	}
	preflight := func(args ...string) (string, error) {
		return runIn(t, dir, "bash", append([]string{script, "preflight", "v0.9.0"}, args...)...)
	}

	t.Run("a release commit other than the expected one is refused", func(t *testing.T) {
		out, err := tag("--push", "--expect-commit", later)
		require.Error(t, err, out)
		assert.Contains(t, out, "refusing to tag modules of another commit")
		out, err = preflight("--expect-commit", later)
		require.Error(t, err, out)
		assert.Empty(t, remoteTag(t, remote, "sdk/v0.9.0"))
	})

	t.Run("a partial remote gets only the missing tag", func(t *testing.T) {
		out, err := tag("--push", "--expect-commit", release, "proto")
		require.NoError(t, err, out)
		assert.Equal(t, release, remoteTag(t, remote, "proto/v0.9.0"))
		assert.Empty(t, remoteTag(t, remote, "sdk/v0.9.0"))

		out, err = preflight("--expect-commit", release)
		require.NoError(t, err, out)
		out, err = tag("--push", "--expect-commit", release)
		require.NoError(t, err, out)
		assert.Contains(t, out, "already has proto/v0.9.0")
		assert.Equal(t, release, remoteTag(t, remote, "sdk/v0.9.0"))
	})

	t.Run("a re-run does nothing", func(t *testing.T) {
		out, err := tag("--push")
		require.NoError(t, err, out)
		assert.Contains(t, out, "already has sdk/v0.9.0 on "+release+"; nothing to do")
		assert.NotContains(t, out, "pushed")
	})

	t.Run("the annotation names the release commit", func(t *testing.T) {
		out := mustRunIn(t, remote, "git", "for-each-ref", "--format=%(contents)", "refs/tags/sdk/v0.9.0")
		assert.Contains(t, out, "Release commit: "+release)
	})

	t.Run("a local module tag on another commit is refused", func(t *testing.T) {
		mustRunIn(t, dir, "git", "tag", "-f", "-a", "sdk/v0.9.0", "-m", "moved", later)
		out, err := tag("sdk")
		require.Error(t, err, out)
		assert.Contains(t, out, "tags are never moved")
	})

	t.Run("a remote module tag on another commit is refused", func(t *testing.T) {
		// Test-only: make the remote hold sdk/v0.9.0 on the later commit.
		mustRunIn(t, dir, "git", "push", "-q", "origin", "main")
		mustRunIn(t, remote, "git", "tag", "-d", "sdk/v0.9.0")
		mustRunIn(t, dir, "git", "push", "-q", "origin", "refs/tags/sdk/v0.9.0")
		mustRunIn(t, dir, "git", "tag", "-d", "sdk/v0.9.0")

		out, err := preflight()
		require.Error(t, err, out)
		assert.Contains(t, out, "origin already has sdk/v0.9.0 on "+later)
		out, err = tag("--push")
		require.Error(t, err, out)
		assert.Contains(t, out, "tags are never moved")
		assert.Equal(t, later, remoteTag(t, remote, "sdk/v0.9.0"), "the remote tag is not touched")
	})

	t.Run("a release tag that does not exist is refused", func(t *testing.T) {
		out, err := runIn(t, dir, "bash", script, "tag", "v0.9.9")
		require.Error(t, err, out)
		assert.Contains(t, out, "tag v0.9.9 does not exist")
	})
}
