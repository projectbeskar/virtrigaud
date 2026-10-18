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

// t2s3Pair creates team-a.t2s3 and a full clone team-a.t2s3c of it — whose
// CD-ROM, as a single-host Clone leaves it, references the SOURCE's seed.
func (c *createHost) t2s3Pair() {
	c.t.Helper()
	ctx := context.Background()
	_, err := c.p.Create(ctx, c.createReq(r24Source, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(c.t, err)
	_, err = c.p.Clone(ctx, contracts.CloneRequest{
		Source: contracts.VMRef{ID: "team-a.t2s3"}, TargetName: "t2s3c", TargetVM: r24CloneID,
	})
	require.NoError(c.t, err)
	require.Contains(c.t, c.domainXML("team-a.t2s3c"), c.seedOf("team-a.t2s3"), "the clone's CD-ROM names the source's seed")
	require.Len(c.t, c.seedEntries(), 1, "a single-host clone makes no seed of its own")
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

	_, err := c.p.Delete(ctx, contracts.VMRef{ID: "team-a.t2s3"})
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

// domainUUIDOf returns the <uuid> of a domain definition.
func domainUUIDOf(t *testing.T, domainXML string) string {
	t.Helper()
	m := regexp.MustCompile(`<uuid>([^<]+)</uuid>`).FindStringSubmatch(domainXML)
	require.Len(t, m, 2)
	return m[1]
}
