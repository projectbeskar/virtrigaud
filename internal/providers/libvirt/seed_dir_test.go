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
	seeds := filepath.Join(safeTempDir(t), "seeds")
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
	report := func(leaf string, ancestors ...string) string {
		out := "self 1001\nleaf " + leaf + " /srv/seeds\n"
		for _, a := range ancestors {
			out += "anc " + a + "\n"
		}
		return out
	}
	ok := []string{"755 0 /srv", "755 0 /"}
	for name, tc := range map[string]struct {
		out    string
		wantOK bool
	}{
		"0711 owned by the SSH user":        {report("711 1001", ok...), true},
		"0700":                              {report("700 1001", ok...), true},
		"sticky leaf":                       {report("1711 1001", ok...), true},
		"ancestor owned by the SSH user":    {report("711 1001", "700 1001 /srv", "755 0 /"), true},
		"sticky world-writable ancestor":    {report("711 1001", "1777 0 /srv", "755 0 /"), true},
		"ancestor path with a space":        {report("711 1001", "755 0 /srv/my dir", "755 0 /"), true},
		"listable by everyone":              {report("755 1001", ok...), false},
		"listable by its group":             {report("751 1001", ok...), false},
		"writable by its group":             {report("731 1001", ok...), false},
		"writable by everyone":              {report("713 1001", ok...), false},
		"root's":                            {report("711 0", ok...), false},
		"another user's":                    {report("711 1002", ok...), false},
		"group-writable ancestor":           {report("711 1001", "775 1001 /srv", "755 0 /"), false},
		"world-writable ancestor":           {report("711 1001", "757 0 /srv", "755 0 /"), false},
		"ancestor of another user":          {report("711 1001", "755 1002 /srv", "755 0 /"), false},
		"no ancestor reported":              {report("711 1001"), false},
		"no self":                           {"leaf 711 1001 /srv/seeds\nanc 755 0 /\n", false},
		"missing":                           {seedRootMissingMarker + "\n", false},
		"not a directory":                   {seedRootNotDirMarker + "\n", false},
		"empty":                             {"", false},
		"old format":                        {"711 1001\n1001\n", false},
		"bad mode":                          {report("7x1 1001", ok...), false},
		"truncated line":                    {"self 1001\nleaf 711\nanc 755 0 /\n", false},
	} {
		mode, reason := seedRootProblem(tc.out)
		assert.Equal(t, tc.wantOK, reason == "", "%s: %q: %s", name, tc.out, reason)
		if tc.wantOK {
			assert.NotZero(t, mode, name)
		}
	}
}

func TestSeedRootScript_RealShell(t *testing.T) {
	requireRealHostTool(t, "stat")
	requireRealHostTool(t, "realpath")
	h := localHostVP("single")
	ctx := context.Background()
	dir := filepath.Join(safeTempDir(t), "seeds")

	require.NoError(t, ensureSeedRoot(ctx, h, dir), "a missing directory the user may create is created")
	st, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o711), st.Mode().Perm())

	require.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // the refused mode under test
	assert.ErrorIs(t, ensureSeedRoot(ctx, h, dir), errSeedRootUnusable)
	require.NoError(t, os.Chmod(dir, 0o700))

	link := filepath.Join(safeTempDir(t), "link")
	require.NoError(t, os.Symlink(dir, link))
	assert.NoError(t, ensureSeedRoot(ctx, h, link), "a symlink to a usable directory, through safe directories, is followed")

	// A parent another account (here: the group) can write: it could rename the
	// seed directory away and plant its own between the check and the write.
	weak := filepath.Join(safeTempDir(t), "weak")
	require.NoError(t, os.Mkdir(weak, 0o700))
	require.NoError(t, os.Chmod(weak, 0o770)) //nolint:gosec // the refused mode under test
	inWeak := filepath.Join(weak, "seeds")
	require.NoError(t, os.Mkdir(inWeak, 0o700))
	assert.ErrorIs(t, ensureSeedRoot(ctx, h, inWeak), errSeedRootUnusable, "a group-writable parent is refused")

	// The same, sticky: only an entry's owner may rename it. Accepted.
	require.NoError(t, os.Chmod(weak, 0o770|os.ModeSticky)) //nolint:gosec // the accepted sticky mode under test
	assert.NoError(t, ensureSeedRoot(ctx, h, inWeak), "a sticky group-writable parent is accepted")
	require.NoError(t, os.Chmod(weak, 0o770)) //nolint:gosec // the refused mode under test

	// A symlink in a safe directory whose TARGET lies below the weak parent.
	viaLink := filepath.Join(safeTempDir(t), "via")
	require.NoError(t, os.Symlink(inWeak, viaLink))
	assert.ErrorIs(t, ensureSeedRoot(ctx, h, viaLink), errSeedRootUnusable, "the canonical path's parents are checked")

	// A symlink that itself sits in the weak parent, pointing at a safe directory.
	linkInWeak := filepath.Join(weak, "link")
	require.NoError(t, os.Symlink(dir, linkInWeak))
	assert.ErrorIs(t, ensureSeedRoot(ctx, h, linkInWeak), errSeedRootUnusable, "the configured path's parents are checked")
}

// TestCreate_RefusesASeedDirectoryOfAnotherUser: end to end, a seed directory
// not owned by the SSH user (here: the SSH user's uid, as `id -u` reports it,
// is another than the directory's owner) is refused with nothing written.
func TestCreate_RefusesASeedDirectoryOfAnotherUser(t *testing.T) {
	c, seeds := newSeedCreateHost(t)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 4242\n"), 0o700)) //nolint:gosec // test shim must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	assert.ErrorIs(t, err, errSeedRootUnusable)
	assert.Empty(t, dirEntries(t, seeds), "nothing is written")
	assert.NotContains(t, c.log("virsh"), "\tdefine ")
}

// TestClusteredCreate_RefusesAnUnusableSeedDirectory: a clustered create on a
// host whose seed directory others can list writes nothing there and defines
// nothing.
func TestClusteredCreate_RefusesAnUnusableSeedDirectory(t *testing.T) {
	s := newSharedPool(t)
	seeds := filepath.Join(safeTempDir(t), "seeds")
	require.NoError(t, os.Mkdir(seeds, 0o700))
	require.NoError(t, os.Chmod(seeds, 0o755)) //nolint:gosec // the refused mode under test
	s.p.hostSeedDir = seeds

	_, err := s.createOnB()
	require.Error(t, err)
	assert.ErrorIs(t, err, errSeedRootUnusable)
	assert.Empty(t, dirEntries(t, seeds))
	assert.NotContains(t, s.log("qemu-img"), "convert", "no disk was written")
	assert.NotContains(t, s.log("virsh"), "\tdefine ", "nothing was defined")
}

// TestCreate_SealFailureRemovesTheSeed: a failure at any step of sealing the
// seed (or the create's context cancelled while the ISO is built) removes
// the per-seed directory — user-data with it — and defines nothing.
func TestCreate_SealFailureRemovesTheSeed(t *testing.T) {
	for _, tc := range []struct{ name, tool, pattern string }{
		// Shell case patterns: the literal parts are quoted.
		{"the ISO's chmod", "chmod", "'0600 -- '*/" + cloudInitISOName},
		{"the private copy", "cp", "*" + cloudInitISOPrivateCopyName},
		{"the private copy's chmod", "chmod", "'0600 -- '*/" + cloudInitISOPrivateCopyName},
		{"removing the inputs", "rm", "'-f -- '*/" + cloudInitUserDataName + "*"},
		{"opening the directory", "chmod", "'0711 -- '*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireRealHostTool(t, tc.tool)
			c, seeds := newSeedCreateHost(t)
			bin := t.TempDir()
			shim := "#!/bin/sh\ncase \"$*\" in " + tc.pattern + ") echo \"" + tc.tool + ": scripted failure\" >&2; exit 1 ;; esac\n" +
				"exec '" + hostGuardRealTools[tc.tool] + "' \"$@\"\n"
			require.NoError(t, os.WriteFile(filepath.Join(bin, tc.tool), []byte(shim), 0o700)) //nolint:gosec // test shim must be executable
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			_, err := c.p.Create(context.Background(), c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "scripted failure", "the scripted step is the one that failed")
			assert.Empty(t, dirEntries(t, seeds), "the per-seed directory is removed")
			assert.NotContains(t, c.log("virsh"), "\tdefine ")
		})
	}

	t.Run("context cancelled while the ISO is built", func(t *testing.T) {
		c, seeds := newSeedCreateHost(t)
		bin := t.TempDir()
		started := filepath.Join(bin, "started")
		shim := "#!/bin/sh\n: > '" + started + "'\nwhile :; do sleep 0.05; done\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, "genisoimage"), []byte(shim), 0o700)) //nolint:gosec // test shim must be executable
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, cerr := c.p.Create(ctx, c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
			done <- cerr
		}()
		require.Eventually(t, func() bool {
			_, serr := os.Stat(started)
			return serr == nil
		}, 10*time.Second, 10*time.Millisecond, "genisoimage runs after user-data was written")
		cancel()
		select {
		case cerr := <-done:
			require.Error(t, cerr)
		case <-time.After(30 * time.Second):
			t.Fatal("the cancelled create did not return")
		}
		assert.Empty(t, dirEntries(t, seeds), "the per-seed directory, user-data with it, is removed")
		assert.NotContains(t, c.log("virsh"), "\tdefine ")
	})
}

// TestCreate_RemovesOlderUnreferencedSeedsOfTheDomain (R27): a successful
// create removes the seed directories an earlier attempt of the same domain
// name left behind once they are old enough, keeps a fresh one (a create may
// still be preparing it), and keeps its own.
func TestCreate_RemovesOlderUnreferencedSeedsOfTheDomain(t *testing.T) {
	c, seeds := newSeedCreateHost(t)
	ctx := context.Background()
	base := c.file(c.images, "ubuntu.qcow2")
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(t, err)
	first := dirEntries(t, seeds)
	require.Len(t, first, 1)
	// The domain vanishes behind the provider's back (or an earlier attempt's
	// define never happened): its seed is left, unreferenced, and old.
	_, err = c.vp.runVirshCommand(ctx, "undefine", "team-a.web")
	require.NoError(t, err)
	old := time.Now().Add(-2 * orphanSeedMinAge)
	require.NoError(t, os.Chtimes(filepath.Join(seeds, first[0]), old, old))
	fresh := filepath.Join(seeds, cloudInitSeedDirPrefix+"team-a.web.FrEsHfReSh")
	require.NoError(t, os.Mkdir(fresh, 0o700))
	other := filepath.Join(seeds, cloudInitSeedDirPrefix+"team-b.web.OlDoLdOlDo")
	require.NoError(t, os.Mkdir(other, 0o700))
	require.NoError(t, os.Chtimes(other, old, old))

	_, err = c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(t, err)
	refs, err := parseDomainPathRefs(c.domainXML("team-a.web"))
	require.NoError(t, err)
	own := filepath.Base(filepath.Dir(refs.disks[1]))
	assert.ElementsMatch(t, []string{own, filepath.Base(fresh), filepath.Base(other)}, dirEntries(t, seeds),
		"the old unreferenced seed of this domain is gone; its own, a fresh one and another domain's stay")
}

// TestSeedCopySource covers how the SSH user's read of an existing seed picks
// its file: the private copy in a per-create or a legacy per-name seed
// directory when it exists, the ISO itself otherwise, and the ISO when the
// existence check fails.
func TestSeedCopySource(t *testing.T) {
	ctx := context.Background()
	vp := localHostVP("single")
	staging := safeTempDir(t)
	p := &Provider{hostSeedDir: safeTempDir(t), hostStagingDir: staging}

	legacyDir := filepath.Join(staging, legacyCloudInitSeedRoot, "web")
	require.NoError(t, os.MkdirAll(legacyDir, 0o700))
	legacyISO := filepath.Join(legacyDir, cloudInitISOName)
	assert.Equal(t, legacyISO, p.seedCopySource(ctx, vp, legacyISO), "no private copy yet: the ISO itself")
	require.NoError(t, os.WriteFile(filepath.Join(legacyDir, cloudInitISOPrivateCopyName), []byte("seed"), 0o600))
	assert.Equal(t, filepath.Join(legacyDir, cloudInitISOPrivateCopyName), p.seedCopySource(ctx, vp, legacyISO),
		"a legacy per-name seed an operator gave a private copy is read through it")

	perCreate := filepath.Join(p.seedDir(), cloudInitSeedDirPrefix+"web.AbCdEfGhIj", cloudInitISOName)
	assert.Equal(t, perCreate, p.seedCopySource(ctx, failingRunner{}, perCreate),
		"a failed existence check falls back to the ISO")
	elsewhere := filepath.Join(safeTempDir(t), "web", cloudInitISOName)
	assert.Equal(t, elsewhere, p.seedCopySource(ctx, vp, elsewhere), "outside the seed roots: never a private copy")

	assert.True(t, isLegacySeedDirIn(legacyDir, p.seedRoots()...))
	assert.False(t, isLegacySeedDirIn(filepath.Join(staging, legacyCloudInitSeedRoot), p.seedRoots()...))
	assert.False(t, isLegacySeedDirIn(filepath.Join(staging, "other", "web"), p.seedRoots()...))
}

// failingRunner is a host whose every command fails.
type failingRunner struct{}

func (failingRunner) runVirshCommand(context.Context, ...string) (*VirshResult, error) {
	return nil, errors.New("scripted host failure")
}

// TestClusteredCreateDelete_SeedInTheSeedDirectory (R27, clustered): the
// clustered create builds the seed on its target host in the seed directory,
// private, and the owner-checked Delete removes a seed from the seed directory
// as well as one an earlier release left in the staging directory.
func TestClusteredCreateDelete_SeedInTheSeedDirectory(t *testing.T) {
	s := newSharedPool(t)
	seeds := filepath.Join(safeTempDir(t), "seeds")
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
	seeds := filepath.Join(safeTempDir(t), "seeds")
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
