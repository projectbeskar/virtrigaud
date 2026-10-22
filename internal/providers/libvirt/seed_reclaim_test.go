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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin R24: a single-host Delete leaves no cloud-init seed directory
// of its own behind. They run on the full fake host (createHost): real files,
// real find/rm, fake virsh.

var (
	r24Source  = contracts.ObjectIdentity{Namespace: "team-a", Name: "t2s3"}
	r24CloneID = contracts.ObjectIdentity{Namespace: "team-a", Name: "t2s3c"}
)

// seedEntries returns the names of the seed directories in the staging
// directory.
func (c *createHost) seedEntries() []string {
	c.t.Helper()
	var out []string
	for _, n := range c.stagingEntries() {
		if strings.HasPrefix(n, cloudInitSeedDirPrefix) {
			out = append(out, n)
		}
	}
	return out
}

// seedOf returns the name of the one seed directory of exactly domain.
func (c *createHost) seedOf(domain string) string {
	c.t.Helper()
	var got []string
	for _, n := range c.seedEntries() {
		if isOwnSeedDirName(n, domain) {
			got = append(got, n)
		}
	}
	require.Len(c.t, got, 1, "seed directories of %s in %v", domain, c.seedEntries())
	return got[0]
}

// t2s3Pair creates team-a.t2s3 and a full clone team-a.t2s3c of it, then
// turns the clone into one an OLDER release made: its CD-ROM references the
// SOURCE's seed and it has no seed of its own (a single-host Clone before R26
// left it so). The R24 reclaim must keep handling such pairs.
func (c *createHost) t2s3Pair() {
	c.t.Helper()
	ctx := context.Background()
	_, err := c.p.Create(ctx, c.createReq(r24Source, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(c.t, err)
	_, err = c.p.Clone(ctx, contracts.CloneRequest{
		Source: contracts.VMRef{ID: "team-a.t2s3"}, TargetName: "t2s3c", TargetVM: r24CloneID,
	})
	require.NoError(c.t, err)

	own, source := c.seedOf("team-a.t2s3c"), c.seedOf("team-a.t2s3")
	for _, f := range []string{"dom-team-a.t2s3c.xml", "dom-" + domainUUIDOf(c.t, c.domainXML("team-a.t2s3c")) + ".xml"} {
		path := filepath.Join(c.root, "h1", f)
		b, err := os.ReadFile(path) //nolint:gosec // test reads its own fixture
		require.NoError(c.t, err)
		require.NoError(c.t, os.WriteFile(path, []byte(strings.ReplaceAll(string(b), own, source)), 0o600))
	}
	require.NoError(c.t, os.RemoveAll(filepath.Join(c.staging, own)))
	require.Contains(c.t, c.domainXML("team-a.t2s3c"), source, "the legacy clone's CD-ROM names the source's seed")
	require.Len(c.t, c.seedEntries(), 1, "a legacy single-host clone has no seed of its own")
}

func TestIsOwnSeedDirName(t *testing.T) {
	tests := []struct {
		name, domain string
		want         bool
	}{
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "team-a.t2s3", true},
		{"virtrigaud-cloudinit-team-a.t2s3c.7VRO5wT7zX", "team-a.t2s3c", true},
		// A domain that is a prefix of another is not the other's.
		{"virtrigaud-cloudinit-team-a.t2s3c.7VRO5wT7zX", "team-a.t2s3", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "team-a.t2s3c", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "team-a", false},
		// The random part is exactly the mktemp part: ten letters and digits.
		{"virtrigaud-cloudinit-team-a.t2s3.x.7VRO5wT7zX", "team-a.t2s3", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7z", "team-a.t2s3", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zXY", "team-a.t2s3", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7z-", "team-a.t2s3", false},
		{"virtrigaud-cloudinit-team-a.t2s3", "team-a.t2s3", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "", false},
		{"other-team-a.t2s3.7VRO5wT7zX", "team-a.t2s3", false},
		// Glob and regexp characters in a domain name are literal, never patterns.
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "*", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "team-a.*", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "team-a.t2s?", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "team-a.t2s[3]", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "team-a.t2s3.+", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7zX", "team-a.t2s3|team-a", false},
		{"virtrigaud-cloudinit-team-a.t2s[3].7VRO5wT7zX", "team-a.t2s[3]", true},
		// Nor are they in the random part.
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7z*", "team-a.t2s3", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5w[7zX", "team-a.t2s3", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT7z?", "team-a.t2s3", false},
		{"virtrigaud-cloudinit-team-a.t2s3.7VRO5wT.zX", "team-a.t2s3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+tt.domain, func(t *testing.T) {
			assert.Equal(t, tt.want, isOwnSeedDirName(tt.name, tt.domain))
		})
	}
}

func TestIsSeedDirIn(t *testing.T) {
	assert.True(t, isSeedDirIn("/tmp/virtrigaud-cloudinit-a.b.0123456789", "/tmp"))
	assert.False(t, isSeedDirIn("/tmp/virtrigaud-cloudinit-", "/tmp"))
	assert.False(t, isSeedDirIn("/tmp/x/virtrigaud-cloudinit-a.b.0123456789", "/tmp"))
	assert.False(t, isSeedDirIn("/var/tmp/virtrigaud-cloudinit-a.b.0123456789", "/tmp"))
	assert.False(t, isSeedDirIn("/tmp/other", "/tmp"))
	assert.False(t, isSeedDirIn("/tmp", "/tmp"))
}

// TestDelete_SingleHost_SourceAndCloneDeletedTogether is the R24 leak: the
// source VM and its clone are deleted at the same time (a namespace cleanup).
// Each plan sees the other domain still defined and keeps the source's seed;
// whichever finishes last must remove it.
func TestDelete_SingleHost_SourceAndCloneDeletedTogether(t *testing.T) {
	for _, tt := range []struct{ name, first, second string }{
		{"source parked, clone deleted meanwhile", "team-a.t2s3", "team-a.t2s3c"},
		{"clone parked, source deleted meanwhile", "team-a.t2s3c", "team-a.t2s3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newCreateHost(t)
			c.t2s3Pair()
			ctx := context.Background()
			hostDir := filepath.Join(c.root, "h1")

			// The first delete has planned (the other domain is defined, so the
			// seed is kept) and is parked at its undefine.
			require.NoError(t, os.WriteFile(filepath.Join(hostDir, "hold-undefine-"+tt.first), nil, 0o600))
			done := make(chan error, 1)
			go func() {
				_, err := c.p.Delete(ctx, contracts.VMRef{ID: tt.first})
				done <- err
			}()
			require.Eventually(t, func() bool {
				_, err := os.Stat(filepath.Join(hostDir, "held-undefine-"+tt.first))
				return err == nil
			}, 10*time.Second, 10*time.Millisecond, "the first delete reaches its undefine")

			// The second delete runs to completion meanwhile.
			_, err := c.p.Delete(ctx, contracts.VMRef{ID: tt.second})
			require.NoError(t, err)
			assert.Len(t, c.seedEntries(), 1, "the seed is kept while the parked domain still references it")

			require.NoError(t, os.WriteFile(filepath.Join(hostDir, "release-undefine-"+tt.first), nil, 0o600))
			require.NoError(t, <-done)
			assert.Empty(t, c.seedEntries(), "the last domain deleted removes the seed")
		})
	}
}

// TestDelete_SingleHost_RemovesItsOwnSeedTheDefinitionNoLongerNames covers a
// domain whose CD-ROM no longer references its seed (detached, or replaced):
// Delete still removes the directory named after exactly that domain, and
// nothing named after another — t2s3 versus t2s3c share a prefix.
func TestDelete_SingleHost_RemovesItsOwnSeedTheDefinitionNoLongerNames(t *testing.T) {
	c := newCreateHost(t)
	ctx := context.Background()
	base := c.file(c.images, "ubuntu.qcow2")
	for _, owner := range []contracts.ObjectIdentity{r24Source, r24CloneID} {
		_, err := c.p.Create(ctx, c.createReq(owner, base))
		require.NoError(t, err)
	}
	seedSource, seedLonger := c.seedOf("team-a.t2s3"), c.seedOf("team-a.t2s3c")

	// Decoys: a symlink named like t2s3's seed (to a directory with a file in
	// it) and a directory whose random part has the wrong length.
	victim := filepath.Join(c.base, "victim")
	require.NoError(t, os.MkdirAll(victim, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600))
	link := cloudInitSeedDirPrefix + "team-a.t2s3.AAAAAAAAAA"
	require.NoError(t, os.Symlink(victim, filepath.Join(c.staging, link)))
	short := cloudInitSeedDirPrefix + "team-a.t2s3.short"
	require.NoError(t, os.Mkdir(filepath.Join(c.staging, short), 0o750))

	// Detach t2s3's seed CD-ROM from its stored definition.
	cdrom := regexp.MustCompile(`(?s)<disk type='file' device='cdrom'>.*?</disk>`)
	for _, f := range []string{"dom-team-a.t2s3.xml", "dom-" + domainUUIDOf(t, c.domainXML("team-a.t2s3")) + ".xml"} {
		path := filepath.Join(c.root, "h1", f)
		b, err := os.ReadFile(path) //nolint:gosec // test reads its own fixture
		require.NoError(t, err)
		require.True(t, cdrom.Match(b), "the definition has a CD-ROM to detach")
		require.NoError(t, os.WriteFile(path, cdrom.ReplaceAll(b, nil), 0o600))
	}

	// A seed found only by its name is removed once it is old enough that no
	// create of the name can still be preparing it.
	_, err := c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3"})
	require.NoError(t, err)
	assert.Contains(t, c.seedEntries(), seedSource, "a fresh seed the definition does not name is kept")
	old := time.Now().Add(-2 * orphanSeedMinAge)
	require.NoError(t, os.Chtimes(filepath.Join(c.staging, seedSource), old, old))
	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3"})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{seedLonger, link, short}, c.seedEntries(),
		"only t2s3's own seed is removed; t2s3c's, the symlink and the malformed name stay")
	assert.NotContains(t, c.seedEntries(), seedSource)
	assert.FileExists(t, filepath.Join(victim, "keep"), "a symlink is never followed")
	assert.FileExists(t, filepath.Join(c.staging, seedLonger, cloudInitISOName))
}

// TestDelete_SingleHost_AbsentDomainSweepsItsOldSeed covers a delete retried
// after the domain was already undefined: the seed it left is removed — but
// not a fresh one (a create of the name may be in flight), and not while a
// domain still references it.
func TestDelete_SingleHost_AbsentDomainSweepsItsOldSeed(t *testing.T) {
	c := newCreateHost(t)
	c.t2s3Pair()
	ctx := context.Background()
	seed := c.seedOf("team-a.t2s3")
	_, err := c.vp.runVirshCommand(ctx, "undefine", "team-a.t2s3")
	require.NoError(t, err)

	// Fresh: kept.
	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3"})
	require.NoError(t, err)
	assert.Equal(t, []string{seed}, c.seedEntries(), "a seed that may belong to an in-flight create is kept")

	// Old, but the clone's CD-ROM still references it: kept.
	old := time.Now().Add(-2 * orphanSeedMinAge)
	require.NoError(t, os.Chtimes(filepath.Join(c.staging, seed), old, old))
	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3"})
	require.NoError(t, err)
	assert.Equal(t, []string{seed}, c.seedEntries(), "a seed a domain still references is kept")

	// Old and unreferenced: removed.
	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3c"})
	require.NoError(t, err)
	assert.Empty(t, c.seedEntries(), "deleting the clone, the last user, removes the source's seed")
}

// TestDelete_SingleHost_AbsentDomainRemovesAnUnreferencedOldSeed is the retry
// of a delete that undefined the domain but did not get to remove its seed.
func TestDelete_SingleHost_AbsentDomainRemovesAnUnreferencedOldSeed(t *testing.T) {
	c := newCreateHost(t)
	ctx := context.Background()
	for _, owner := range []contracts.ObjectIdentity{r24Source, r24CloneID} {
		_, err := c.p.Create(ctx, c.createReq(owner, c.file(c.images, "ubuntu.qcow2")))
		require.NoError(t, err)
	}
	seed, other := c.seedOf("team-a.t2s3"), c.seedOf("team-a.t2s3c")
	_, err := c.vp.runVirshCommand(ctx, "undefine", "team-a.t2s3")
	require.NoError(t, err)
	old := time.Now().Add(-2 * orphanSeedMinAge)
	for _, n := range []string{seed, other} {
		require.NoError(t, os.Chtimes(filepath.Join(c.staging, n), old, old))
	}

	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3"})
	require.NoError(t, err)
	assert.Equal(t, []string{other}, c.seedEntries(), "t2s3's seed goes; t2s3c's (a longer name, still defined) stays")
}

// oldUnreferencedSeed creates team-a.t2s3, undefines it behind the provider's
// back, backdates its seed and returns the seed's directory path.
func (c *createHost) oldUnreferencedSeed() string {
	c.t.Helper()
	ctx := context.Background()
	_, err := c.p.Create(ctx, c.createReq(r24Source, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(c.t, err)
	seed := filepath.Join(c.staging, c.seedOf("team-a.t2s3"))
	_, err = c.vp.runVirshCommand(ctx, "undefine", "team-a.t2s3")
	require.NoError(c.t, err)
	old := time.Now().Add(-2 * orphanSeedMinAge)
	require.NoError(c.t, os.Chtimes(seed, old, old))
	return seed
}

// TestReclaimSeedDirs_FailsClosed pins that a seed is kept whenever the host
// cannot prove it unreferenced.
func TestReclaimSeedDirs_FailsClosed(t *testing.T) {
	t.Run("the domain list cannot be read", func(t *testing.T) {
		c := newCreateHost(t)
		seed := c.oldUnreferencedSeed()
		require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "fail-list-uuid"), nil, 0o600))
		c.p.reclaimSeedDirs(context.Background(), c.vp, "team-a.t2s3", "", orphanSeedMinAge)
		assert.DirExists(t, seed)
		c.p.reclaimSeedDirs(context.Background(), c.vp, "team-a.zzz", seed, 0)
		assert.DirExists(t, seed, "a kept seed too")
	})
	t.Run("a domain definition cannot be read", func(t *testing.T) {
		c := newCreateHost(t)
		seed := c.oldUnreferencedSeed()
		_, err := c.p.Create(context.Background(), c.createReq(r24CloneID, c.file(c.images, "ubuntu.qcow2")))
		require.NoError(t, err)
		require.NoError(t, os.Remove(filepath.Join(c.root, "h1", "dom-"+domainUUIDOf(t, c.domainXML("team-a.t2s3c"))+".xml")))
		c.p.reclaimSeedDirs(context.Background(), c.vp, "team-a.t2s3", "", orphanSeedMinAge)
		assert.DirExists(t, seed)
	})
	t.Run("find fails", func(t *testing.T) {
		c := newCreateHost(t)
		seed := c.oldUnreferencedSeed()
		bin := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(bin, "find"), []byte("#!/bin/sh\necho 'find: scripted failure' >&2\nexit 1\n"), 0o700)) //nolint:gosec // test fixture must be executable
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		c.p.reclaimSeedDirs(context.Background(), c.vp, "team-a.t2s3", "", orphanSeedMinAge)
		assert.DirExists(t, seed)
		c.p.reclaimSeedDirs(context.Background(), c.vp, "team-a.zzz", seed, 0)
		assert.DirExists(t, seed, "a kept seed that cannot be inspected is kept")
	})
	t.Run("a kept seed that is a symlink", func(t *testing.T) {
		c := newCreateHost(t)
		victim := filepath.Join(c.base, "victim")
		require.NoError(t, os.MkdirAll(victim, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600))
		link := filepath.Join(c.staging, cloudInitSeedDirPrefix+"team-a.t2s3.AAAAAAAAAA")
		require.NoError(t, os.Symlink(victim, link))
		c.p.reclaimSeedDirs(context.Background(), c.vp, "team-a.t2s3", link, 0)
		assert.FileExists(t, filepath.Join(victim, "keep"))
		_, err := os.Lstat(link)
		assert.NoError(t, err, "the symlink is left alone")
	})
	t.Run("a kept seed outside the staging directory", func(t *testing.T) {
		c := newCreateHost(t)
		outside := filepath.Join(c.base, cloudInitSeedDirPrefix+"team-a.t2s3.AAAAAAAAAA")
		require.NoError(t, os.MkdirAll(outside, 0o750))
		c.p.reclaimSeedDirs(context.Background(), c.vp, "team-a.t2s3", outside, 0)
		assert.DirExists(t, outside)
	})
}

// TestDelete_SingleHost_SymlinkedStagingDirectory pins that references are
// compared as canonical paths: with the staging directory reached through a
// symlink, the source's seed is still kept while its clone references it, and
// removed with the clone.
func TestDelete_SingleHost_SymlinkedStagingDirectory(t *testing.T) {
	c := newCreateHost(t)
	link := filepath.Join(t.TempDir(), "staging-link")
	require.NoError(t, os.Symlink(c.staging, link))
	c.p.hostStagingDir = link
	c.t2s3Pair()
	ctx := context.Background()

	// The clone's definition names the seed by its real path, the source's by
	// the symlinked one (an administrator's edit, or another tool's define).
	for _, f := range []string{"dom-team-a.t2s3c.xml", "dom-" + domainUUIDOf(t, c.domainXML("team-a.t2s3c")) + ".xml"} {
		path := filepath.Join(c.root, "h1", f)
		b, err := os.ReadFile(path) //nolint:gosec // test reads its own fixture
		require.NoError(t, err)
		require.Contains(t, string(b), link)
		require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(b), link, c.staging)), 0o600))
	}

	_, err := c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3"})
	require.NoError(t, err)
	assert.Len(t, c.seedEntries(), 1, "the clone still references the seed")
	for _, f := range []string{"dom-team-a.t2s3c.xml", "dom-" + domainUUIDOf(t, c.domainXML("team-a.t2s3c")) + ".xml"} {
		path := filepath.Join(c.root, "h1", f)
		b, rerr := os.ReadFile(path) //nolint:gosec // test reads its own fixture
		require.NoError(t, rerr)
		require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(b), c.staging, link)), 0o600))
	}
	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3c"})
	require.NoError(t, err)
	assert.Empty(t, c.seedEntries(), "the clone, the last user, removes the source's seed")
}

// TestCreateDelete_SingleHost_SameNameAreSerialized pins the recreate race: a
// re-sent Create of a name whose Delete is under way waits for it, then makes a
// complete VM — it does not bind the domain the delete is about to undefine
// (its owner stamp matches), and the delete's cleanup of leftovers cannot take
// the seed it makes.
func TestCreateDelete_SingleHost_SameNameAreSerialized(t *testing.T) {
	c := newCreateHost(t)
	ctx := context.Background()
	hostDir := filepath.Join(c.root, "h1")
	base := c.file(c.images, "ubuntu.qcow2")
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(t, err)
	oldSeed := c.seedOf("team-a.web")

	// The delete is parked at its undefine.
	require.NoError(t, os.WriteFile(filepath.Join(hostDir, "hold-undefine-team-a.web"), nil, 0o600))
	deleted := make(chan error, 1)
	go func() {
		_, derr := c.p.Delete(ctx, contracts.VMRef{ID: "team-a.web"})
		deleted <- derr
	}()
	require.Eventually(t, func() bool {
		_, serr := os.Stat(filepath.Join(hostDir, "held-undefine-team-a.web"))
		return serr == nil
	}, 10*time.Second, 10*time.Millisecond)

	created := make(chan error, 1)
	go func() {
		_, cerr := c.p.Create(ctx, c.createReq(ownerTeamA, base))
		created <- cerr
	}()
	select {
	case cerr := <-created:
		t.Fatalf("the create ran while the delete of the same name was under way (err=%v)", cerr)
	case <-time.After(300 * time.Millisecond):
	}

	require.NoError(t, os.WriteFile(filepath.Join(hostDir, "release-undefine-team-a.web"), nil, 0o600))
	require.NoError(t, <-deleted)
	require.NoError(t, <-created)

	newSeed := c.seedOf("team-a.web")
	assert.NotEqual(t, oldSeed, newSeed, "the new VM has a seed of its own")
	assert.FileExists(t, filepath.Join(c.staging, newSeed, cloudInitISOName))
	assert.Contains(t, c.domainXML("team-a.web"), newSeed, "the new domain exists and references its seed")
}

// domainUUIDOf returns the <uuid> of a domain definition.
func domainUUIDOf(t *testing.T, domainXML string) string {
	t.Helper()
	m := regexp.MustCompile(`<uuid>([^<]+)</uuid>`).FindStringSubmatch(domainXML)
	require.Len(t, m, 2)
	return m[1]
}
