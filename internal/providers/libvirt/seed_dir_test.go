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
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin R27: a cloud-init seed is built in the dedicated seed
// directory, private (ISO 0600, no user-data or meta-data left behind), only
// in a seed directory nobody else can list or write, and seeds of earlier
// releases (in the staging directory) are still found and removed.

// assertPrivateSeed checks a sealed seed directory: traverse-only, holding the
// ISO and the SSH user's private copy of it (both 0600, the same bytes), and
// none of genisoimage's inputs.
func assertPrivateSeed(t *testing.T, seedDir string) {
	t.Helper()
	st, err := os.Stat(seedDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o711), st.Mode().Perm(), "seed dir: traverse-only for the qemu user")
	iso := filepath.Join(seedDir, cloudInitISOName)
	priv := filepath.Join(seedDir, cloudInitISOPrivateCopyName)
	for _, f := range []string{iso, priv} {
		fst, err := os.Stat(f)
		require.NoError(t, err, "%s exists", f)
		assert.Equal(t, os.FileMode(0o600), fst.Mode().Perm(), "%s is private to its owner", f)
	}
	isoBytes, err := os.ReadFile(iso) //nolint:gosec // test reads its own fixture
	require.NoError(t, err)
	privBytes, err := os.ReadFile(priv) //nolint:gosec // test reads its own fixture
	require.NoError(t, err)
	assert.Equal(t, isoBytes, privBytes, "the private copy is the ISO")
	for _, name := range []string{cloudInitUserDataName, cloudInitMetaDataName} {
		assert.NoFileExists(t, filepath.Join(seedDir, name), "%s is removed once the ISO is built", name)
	}
}

// newSeedCreateHost is newCreateHost with a seed directory of its own (0711,
// owned by the user running the test), apart from the staging directory.
func newSeedCreateHost(t *testing.T) (*createHost, string) {
	t.Helper()
	c := newCreateHost(t)
	seeds := filepath.Join(t.TempDir(), "seeds")
	require.NoError(t, os.Mkdir(seeds, 0o700))
	require.NoError(t, os.Chmod(seeds, 0o711)) //nolint:gosec // the seed directory's documented mode
	c.p.hostSeedDir = seeds
	return c, seeds
}

// dirEntries lists the names in dir.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCreate_SeedIsPrivateInTheSeedDirectory(t *testing.T) {
	c, seeds := newSeedCreateHost(t)
	base := c.file(c.images, "ubuntu.qcow2")

	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, base))
	require.NoError(t, err)

	refs, err := parseDomainPathRefs(c.domainXML("team-a.web"))
	require.NoError(t, err)
	require.Len(t, refs.disks, 2, "primary disk + cloud-init CD-ROM")
	iso := refs.disks[1]
	seedDir := filepath.Dir(iso)
	assert.Equal(t, seeds, filepath.Dir(seedDir), "the seed is built in the seed directory")
	assert.True(t, isOwnSeedDirName(filepath.Base(seedDir), "team-a.web"), "seed dir %s", seedDir)
	assert.Equal(t, cloudInitISOName, filepath.Base(iso), "the domain references the ISO, never the private copy")
	assertPrivateSeed(t, seedDir)
	isoBytes, err := os.ReadFile(iso) //nolint:gosec // test reads the fake ISO
	require.NoError(t, err)
	assert.Contains(t, string(isoBytes), "secret-of-team-a", "the ISO carries the VM's user-data")

	assert.Empty(t, c.stagingEntries(), "nothing of the seed is staged in the (world-listable) staging directory")
	st, err := os.Stat(seeds)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o711), st.Mode().Perm(), "the seed directory itself is left as configured")
	assert.Contains(t, strings.Join(strings.Fields(c.log("genisoimage")), " "), "-output "+iso)
}

func TestCreate_RefusesAnUnusableSeedDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, seeds string) string
	}{
		{"listable by others (0755)", func(t *testing.T, seeds string) string {
			require.NoError(t, os.Chmod(seeds, 0o755)) //nolint:gosec // the refused mode under test
			return seeds
		}},
		{"readable by its group (0750)", func(t *testing.T, seeds string) string {
			require.NoError(t, os.Chmod(seeds, 0o750)) //nolint:gosec // the refused mode under test
			return seeds
		}},
		{"writable by others (0733)", func(t *testing.T, seeds string) string {
			require.NoError(t, os.Chmod(seeds, 0o733)) //nolint:gosec // the refused mode under test
			return seeds
		}},
		{"not a directory", func(t *testing.T, seeds string) string {
			f := filepath.Join(seeds, "file")
			require.NoError(t, os.WriteFile(f, nil, 0o600))
			return f
		}},
		{"missing and cannot be created", func(t *testing.T, seeds string) string {
			if os.Geteuid() == 0 {
				t.Skip("root may create the directory anyway")
			}
			ro := filepath.Join(seeds, "ro")
			require.NoError(t, os.Mkdir(ro, 0o500))
			t.Cleanup(func() { _ = os.Chmod(ro, 0o700) }) //nolint:gosec // let t.TempDir remove it
			return filepath.Join(ro, "seeds")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, seeds := newSeedCreateHost(t)
			c.p.hostSeedDir = tc.setup(t, seeds)
			base := c.file(c.images, "ubuntu.qcow2")
			before := dirEntries(t, seeds)

			_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, base))
			require.Error(t, err)
			assert.ErrorIs(t, err, errSeedRootUnusable)
			assert.NotContains(t, err.Error(), seeds, "the refusal is a constant message, without host paths")
			var pe *contracts.ProviderError
			require.True(t, errors.As(err, &pe))
			assert.True(t, pe.IsRetryable(), "the create resumes once the host is fixed")

			// No user-data, so the provider's default seed: refused too, never
			// a VM silently created without its seed.
			req := c.createReq(ownerTeamA, base)
			req.UserData = nil
			_, err = c.p.Create(context.Background(), req)
			assert.ErrorIs(t, err, errSeedRootUnusable)

			assert.Equal(t, before, dirEntries(t, seeds), "nothing is written into the refused directory")
			assert.Empty(t, c.stagingEntries())
			assert.NotContains(t, c.log("virsh"), "\tdefine ", "no domain is defined")
		})
	}
}

func TestCreate_CreatesAMissingSeedDirectoryWhenItMay(t *testing.T) {
	c, seeds := newSeedCreateHost(t)
	fresh := filepath.Join(seeds, "fresh")
	c.p.hostSeedDir = fresh

	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(t, err)
	st, err := os.Stat(fresh)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o711), st.Mode().Perm(), "created traverse-only, whatever the umask")
	seedDirs := dirEntries(t, fresh)
	require.Len(t, seedDirs, 1)
	assertPrivateSeed(t, filepath.Join(fresh, seedDirs[0]))
}

// TestDelete_FindsSeedsInTheSeedAndTheLegacyStagingDirectory: after the
// upgrade, a VM created by an earlier release keeps its seed in the staging
// directory and a new VM gets one in the seed directory; Delete removes each
// from where it is, and the leftover sweep looks in both.
func TestDelete_FindsSeedsInTheSeedAndTheLegacyStagingDirectory(t *testing.T) {
	c, seeds := newSeedCreateHost(t)
	ctx := context.Background()
	base := c.file(c.images, "ubuntu.qcow2")

	// Before the upgrade: seeds in the staging directory.
	c.p.hostSeedDir = c.staging
	for _, owner := range []contracts.ObjectIdentity{r24Source, r24CloneID} {
		_, err := c.p.Create(ctx, c.createReq(owner, base))
		require.NoError(t, err)
	}
	legacy, orphan := c.seedOf("team-a.t2s3"), c.seedOf("team-a.t2s3c")

	// After it: new seeds in the seed directory.
	c.p.hostSeedDir = seeds
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(t, err)
	require.Len(t, dirEntries(t, seeds), 1)
	assert.Contains(t, c.domainXML("team-a.t2s3"), filepath.Join(c.staging, legacy), "the live seed is never moved")

	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3"})
	require.NoError(t, err)
	assert.NotContains(t, c.seedEntries(), legacy, "the legacy seed is removed with its domain")

	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.web"})
	require.NoError(t, err)
	assert.Empty(t, dirEntries(t, seeds), "the new seed is removed with its domain")

	// A legacy seed its domain no longer references (a delete that undefined
	// it but did not get to remove the seed) is swept from the staging
	// directory too, once old enough.
	_, err = c.vp.runVirshCommand(ctx, "undefine", "team-a.t2s3c")
	require.NoError(t, err)
	old := time.Now().Add(-2 * orphanSeedMinAge)
	require.NoError(t, os.Chtimes(filepath.Join(c.staging, orphan), old, old))
	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3c"})
	require.NoError(t, err)
	assert.Empty(t, c.seedEntries(), "no seed is left in the staging directory")
}

func TestValidateSeedDir(t *testing.T) {
	for _, ok := range []string{DefaultSeedDir, "/var/lib/virtrigaud/seeds", "/srv/vm_seeds-1", "/a"} {
		assert.NoError(t, validateSeedDir(ok), ok)
	}
	for _, bad := range []string{
		"", "/", "relative/seeds", "/var/lib/../tmp", "/var/lib/./x", "/var/lib/x/", "//x",
		"/var/lib/x y", "/var/lib/x'y", "/var/lib/x\ny", "/var/lib/-x", "/var/lib/$HOME",
		"/tmp", "/tmp/seeds", "/var/tmp/seeds", "/run/seeds", "/dev/shm/seeds", "/proc/x",
		"/" + strings.Repeat("a", seedDirMaxLength),
	} {
		assert.Error(t, validateSeedDir(bad), "%q", bad)
	}
}

func TestSeedDirFromEnv(t *testing.T) {
	t.Setenv(EnvSeedDir, "")
	dir, err := seedDirFromEnv()
	require.NoError(t, err)
	assert.Equal(t, DefaultSeedDir, dir)

	t.Setenv(EnvSeedDir, " /srv/seeds ")
	dir, err = seedDirFromEnv()
	require.NoError(t, err)
	assert.Equal(t, "/srv/seeds", dir)

	t.Setenv(EnvSeedDir, "/tmp/seeds")
	_, err = seedDirFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), EnvSeedDir)

	p := &Provider{}
	assert.Error(t, p.loadSeedDir(), "a malformed value fails provider start-up")
	t.Setenv(EnvSeedDir, "/srv/seeds")
	require.NoError(t, p.loadSeedDir())
	assert.Equal(t, "/srv/seeds", p.seedDir())
	assert.Equal(t, []string{"/srv/seeds", defaultHostStagingDir}, p.seedRoots(), "the legacy staging directory is still a seed root")
}

func TestSeedRootProblem(t *testing.T) {
	for out, wantOK := range map[string]bool{
		"711 1001\n1001\n":           true,
		"700 1001\n1001\n":           true,
		"710 0\n0\n":                 true,
		"1711 1001\n1001":            true,
		"755 1001\n1001\n":           false, // listable by everyone
		"751 1001\n1001\n":           false, // listable by its group
		"731 1001\n1001\n":           false, // writable by its group
		"713 1001\n1001\n":           false, // writable by everyone
		"711 0\n1001\n":              false, // root's: the SSH user cannot use it
		"711 1002\n1001\n":           false, // another user's
		seedRootMissingMarker + "\n": false,
		seedRootNotDirMarker + "\n":  false,
		"":                           false,
		"711 1001":                   false,
		"7x1 1001\n1001\n":           false,
	} {
		assert.Equal(t, wantOK, seedRootProblem(out) == "", "%q: %s", out, seedRootProblem(out))
	}
}

func TestSeedRootScript_RealShell(t *testing.T) {
	requireRealHostTool(t, "stat")
	h := localHostVP("single")
	dir := filepath.Join(t.TempDir(), "seeds")

	require.NoError(t, ensureSeedRoot(context.Background(), h, dir), "a missing directory the user may create is created")
	st, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o711), st.Mode().Perm())

	require.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // the refused mode under test
	assert.ErrorIs(t, ensureSeedRoot(context.Background(), h, dir), errSeedRootUnusable)

	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Chmod(dir, 0o700))
	require.NoError(t, os.Symlink(dir, link))
	assert.NoError(t, ensureSeedRoot(context.Background(), h, link), "a symlink to a usable directory is followed")
}

// TestClusteredCreateDelete_SeedInTheSeedDirectory (R27, clustered): the
// clustered create builds the seed on its target host in the seed directory,
// private, and the owner-checked Delete removes a seed from the seed directory
// as well as one an earlier release left in the staging directory.
func TestClusteredCreateDelete_SeedInTheSeedDirectory(t *testing.T) {
	s := newSharedPool(t)
	seeds := filepath.Join(t.TempDir(), "seeds")
	require.NoError(t, os.Mkdir(seeds, 0o700))
	s.p.hostSeedDir = seeds

	_, err := s.createOnB()
	require.NoError(t, err)
	// (The fixture's define records the domain on its default host.)
	refs, err := parseDomainPathRefs(s.domainXML("team-a.web"))
	require.NoError(t, err)
	require.Len(t, refs.disks, 2)
	seedDir := filepath.Dir(refs.disks[1])
	assert.Equal(t, seeds, filepath.Dir(seedDir), "the clustered create builds the seed in the seed directory")
	assertPrivateSeed(t, seedDir)
	assert.Empty(t, s.stagingEntries())

	// The owner-checked Delete of a domain whose seed is in the seed directory,
	// then of one whose seed an earlier release left in the staging directory.
	legacy := filepath.Join(s.staging, cloudInitSeedDirPrefix+"team-a.web.AbCdEfGhIj")
	require.NoError(t, os.Mkdir(legacy, 0o711))                                                      //nolint:gosec // a sealed seed directory's mode
	require.NoError(t, os.WriteFile(filepath.Join(legacy, cloudInitISOName), []byte("seed"), 0o644)) //nolint:gosec // an earlier release's ISO mode
	for _, dir := range []string{seedDir, legacy} {
		x := strings.Replace(guardDomainXML("team-a.web", uuidVMOnB, s.disk("team-a.web-disk.qcow2", "live"), ownerTeamA, false),
			"</devices>", "<disk type='file' device='cdrom'><source file='"+filepath.Join(dir, cloudInitISOName)+"'/><readonly/></disk></devices>", 1)
		s.defineOn("host-b", "team-a.web", uuidVMOnB, x)
		_, err = s.p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web", HostID: "host-b", Owner: ownerTeamA})
		require.NoError(t, err)
		assert.NoDirExists(t, dir, "Delete removes the domain's seed directory")
	}
}

// TestSeed_SSHUserReadsItBackAfterTheQemuHandover (R27 with R26): a seed built
// under the new scheme (real genisoimage) can still be read by the provider's
// SSH user — copied by a clone, or its files read with isoinfo as the clone
// path does — after libvirt has given the domain's cloud-init.iso to the qemu
// user (simulated here by taking every permission off it): the read goes
// through seedCopySource, which picks the SSH user's private copy.
func TestSeed_SSHUserReadsItBackAfterTheQemuHandover(t *testing.T) {
	requireRealHostTool(t, "genisoimage")
	if _, err := exec.LookPath("isoinfo"); err != nil {
		t.Skip("isoinfo is not installed on this machine")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	ctx := context.Background()
	vp := localHostVP("single")
	seeds := filepath.Join(t.TempDir(), "seeds")
	require.NoError(t, os.Mkdir(seeds, 0o700))
	c := NewCloudInitProvider(vp)
	c.seedDir = seeds
	iso, err := c.PrepareCloudInit(ctx, CloudInitConfig{
		UserData: "#cloud-config\n# secret-of-team-a\n", InstanceID: "team-a.web", Hostname: "web",
	})
	require.NoError(t, err)
	assertPrivateSeed(t, filepath.Dir(iso))

	readFile := func(path, name string) (string, error) {
		res, rerr := runHost(ctx, vp, "isoinfo", "-R", "-i", path, "-x", "/"+name)
		if rerr != nil {
			return "", rerr
		}
		return res.Stdout, nil
	}
	ud, err := readFile(iso, cloudInitUserDataName)
	require.NoError(t, err, "before the domain starts the SSH user owns the ISO")
	assert.Contains(t, ud, "secret-of-team-a")

	// libvirt's handover at domain start: the ISO is now the qemu user's 0600.
	require.NoError(t, os.Chmod(iso, 0))
	t.Cleanup(func() { _ = os.Chmod(iso, 0o600) })
	_, err = readFile(iso, cloudInitUserDataName)
	require.Error(t, err, "the SSH user can no longer read the domain's ISO")

	p := &Provider{hostSeedDir: seeds}
	src := p.seedCopySource(ctx, vp, iso)
	assert.Equal(t, filepath.Join(filepath.Dir(iso), cloudInitISOPrivateCopyName), src)
	ud, err = readFile(src, cloudInitUserDataName)
	require.NoError(t, err, "the SSH user reads its private copy")
	assert.Contains(t, ud, "secret-of-team-a")
	md, err := readFile(src, cloudInitMetaDataName)
	require.NoError(t, err)
	assert.Contains(t, md, "instance-id: team-a.web")

	// A seed of an earlier release (no private copy) is read as it is.
	legacy := filepath.Join(t.TempDir(), cloudInitSeedDirPrefix+"team-a.old.AbCdEfGhIj", cloudInitISOName)
	assert.Equal(t, legacy, p.seedCopySource(ctx, vp, legacy))
}
