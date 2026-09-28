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

package libvirt

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fixtureImagesDir is the storage pool / allowed image directory of the
// routing and ops fixtures (newRoutingFixture, newOpsFixture): a scratch
// directory this test binary creates and removes, so no test runs a host
// command against the real /var/lib/libvirt/images. It is not below a
// forbiddenImageDirRoots entry (t.TempDir lives in /tmp, which the policy
// never accepts as an image directory), like scratchBase.
var fixtureImagesDir string

// TestMain creates fixtureImagesDir and the fixture disk paths inside it
// before any test runs, and removes it afterwards.
func TestMain(m *testing.M) {
	dir, err := newFixtureImagesDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "libvirt tests: %v\n", err)
		os.Exit(1)
	}
	fixtureImagesDir = dir
	routingDiskPath = filepath.Join(dir, "web-disk.qcow2")
	opsDiskPath = filepath.Join(dir, opsDomainName+"-disk.qcow2")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newFixtureImagesDir makes a canonical scratch directory under the package
// directory (or the home directory) that the image policy accepts.
func newFixtureImagesDir() (string, error) {
	var candidates []string
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, home)
	}
	for _, parent := range candidates {
		dir, err := os.MkdirTemp(parent, "_fixture_images_")
		if err != nil {
			continue
		}
		canon, err := filepath.EvalSymlinks(dir)
		if err == nil && !isForbiddenImageDir(canon) {
			return canon, nil
		}
		_ = os.RemoveAll(dir)
	}
	return "", fmt.Errorf("no writable scratch directory outside the forbidden image-dir roots")
}

// useFixtureImages points the image-directory policy at fixtureImagesDir and
// creates the fixture disks (an existing, regular file is what Delete removes).
func useFixtureImages(t *testing.T) {
	t.Helper()
	t.Setenv(EnvImageDirs, fixtureImagesDir)
	t.Setenv("FAKE_POOL_DIR", fixtureImagesDir)
	for _, p := range []string{routingDiskPath, opsDiskPath} {
		if err := os.WriteFile(p, []byte("disk"), 0o600); err != nil {
			t.Fatalf("create fixture disk %s: %v", p, err)
		}
	}
}
