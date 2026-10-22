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
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	yaml "sigs.k8s.io/yaml/goyaml.v3"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin R26: a libvirt clone (single-host and clustered) gets its
// OWN cloud-init seed, owned and removed with the clone, carrying a NEW
// cloud-init identity — so its first boot runs cloud-init's per-instance
// modules (new SSH host keys) instead of keeping the source's host identity.

// r26Target is a clone target with a UID (a current manager).
var r26Target = contracts.ObjectIdentity{UID: "0f9e8d7c-6b5a-4493-8271-605f4e3d2c1b", Namespace: "team-a", Name: "web-copy"}

// r26CloneReq clones team-a.web into r26Target.
func r26CloneReq(customize string) contracts.CloneRequest {
	return contracts.CloneRequest{Source: contracts.VMRef{ID: "team-a.web"}, TargetName: r26Target.Name, TargetVM: r26Target,
		CustomizeJSON: customize}
}

// seedFiles returns the NoCloud files packed in the seed directory dir (a
// staging entry name).
func (c *createHost) seedFiles(dir string) map[string]string {
	c.t.Helper()
	out := map[string]string{}
	for _, n := range seedFileNames {
		b, err := os.ReadFile(filepath.Join(c.staging, dir, n)) //nolint:gosec // test reads its own fixture
		if err == nil {
			out[n] = string(b)
		}
	}
	return out
}

// metaOf parses a meta-data document.
func metaOf(t *testing.T, doc string) map[string]any {
	t.Helper()
	m := map[string]any{}
	require.NoError(t, yaml.Unmarshal([]byte(doc), &m), "%s", doc)
	return m
}

// webAndClone creates team-a.web (a default, user-data-less create: the
// provider's own default seed) and clones it into r26Target with customize.
func (c *createHost) webAndClone(customize string) {
	c.t.Helper()
	ctx := context.Background()
	req := c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2"))
	req.UserData = nil
	_, err := c.p.Create(ctx, req)
	require.NoError(c.t, err)
	resp, err := c.p.Clone(ctx, r26CloneReq(customize))
	require.NoError(c.t, err)
	require.Equal(c.t, "team-a.web-copy", resp.TargetVmID)
}

func TestSingleHostClone_GetsItsOwnSeedWithANewIdentity(t *testing.T) {
	c := newCreateHost(t)
	c.webAndClone("")

	srcSeed, cloneSeed := c.seedOf("team-a.web"), c.seedOf("team-a.web-copy")
	cloneXML := c.domainXML("team-a.web-copy")
	assert.Contains(t, cloneXML, filepath.Join(cloneSeed, cloudInitISOName), "the clone's CD-ROM is its own seed")
	assert.NotContains(t, cloneXML, srcSeed, "never the source's seed")

	src, clone := c.seedFiles(srcSeed), c.seedFiles(cloneSeed)
	assert.Equal(t, "team-a.web", metaOf(t, src[seedFileMetaData])["instance-id"])
	meta := metaOf(t, clone[seedFileMetaData])
	assert.Equal(t, "virtrigaud-"+r26Target.UID, meta["instance-id"], "a new instance: cloud-init runs its per-instance modules")
	assert.Equal(t, "web-copy", meta["local-hostname"])
	assert.Equal(t, metaOf(t, src[seedFileMetaData])["network"], meta["network"], "the rest of the meta-data is kept")

	assert.Contains(t, src[seedFileUserData], "hostname: web\n")
	assert.Equal(t, strings.Replace(src[seedFileUserData], "hostname: web\n", "hostname: \"web-copy\"\n", 1), clone[seedFileUserData],
		"the source's user-data, with the clone's hostname")

	st, err := os.Stat(filepath.Join(c.staging, cloneSeed))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o711), st.Mode().Perm(), "made like Create's seed")
}

func TestSingleHostClone_CustomizationSetsHostnameAndUserData(t *testing.T) {
	c := newCreateHost(t)
	inline := "#cloud-config\nhostname: from-inline\nruncmd: [true]\n"
	c.webAndClone(`{"hostname":"web2","domain":"lab.example","userData":{"cloudInit":{"inline":` + jsonString(inline) + `}},"timeZone":"UTC"}`)

	clone := c.seedFiles(c.seedOf("team-a.web-copy"))
	assert.Equal(t, inline, clone[seedFileUserData], "the VMClone's inline user-data, verbatim")
	meta := metaOf(t, clone[seedFileMetaData])
	assert.Equal(t, "web2.lab.example", meta["local-hostname"])
	assert.Equal(t, "virtrigaud-"+r26Target.UID, meta["instance-id"])
}

// jsonString quotes s as a JSON string.
func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

// TestSingleHostClone_ResentCloneRendersTheSameSeed: the identity is derived
// from the target VirtualMachine, so a Clone sent again for it (after the
// first clone was deleted, or an attempt failed) writes the same seed.
func TestSingleHostClone_ResentCloneRendersTheSameSeed(t *testing.T) {
	c := newCreateHost(t)
	c.webAndClone("")
	first := c.seedFiles(c.seedOf("team-a.web-copy"))

	ctx := context.Background()
	_, err := c.p.Delete(ctx, contracts.VMRef{ID: "team-a.web-copy"})
	require.NoError(t, err)
	_, err = c.p.Clone(ctx, r26CloneReq(""))
	require.NoError(t, err)
	assert.Equal(t, first, c.seedFiles(c.seedOf("team-a.web-copy")))
}

// TestSingleHostClone_SourceDeleteLeavesTheClonesSeed: the source's Delete
// removes the source's seed — no domain references it any more — and never
// the clone's; the clone's Delete then removes its own.
func TestSingleHostClone_SourceDeleteLeavesTheClonesSeed(t *testing.T) {
	c := newCreateHost(t)
	c.webAndClone("")
	srcSeed, cloneSeed := c.seedOf("team-a.web"), c.seedOf("team-a.web-copy")

	ctx := context.Background()
	_, err := c.p.Delete(ctx, contracts.VMRef{ID: "team-a.web"})
	require.NoError(t, err)
	assert.Equal(t, []string{cloneSeed}, c.seedEntries(), "the source's seed is gone, the clone's stays")
	assert.NotContains(t, c.seedEntries(), srcSeed)
	assert.FileExists(t, filepath.Join(c.staging, cloneSeed, cloudInitISOName))

	_, err = c.p.Delete(ctx, contracts.VMRef{ID: "team-a.web-copy"})
	require.NoError(t, err)
	assert.Empty(t, c.seedEntries())
}

// TestSingleHostClone_UnreadableSourceSeedCopiesNothing: a source seed that
// cannot be read fails the clone — retryably — before anything is copied:
// never a clone carrying the source's identity.
func TestSingleHostClone_UnreadableSourceSeedCopiesNothing(t *testing.T) {
	c := newCreateHost(t)
	ctx := context.Background()
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "fail-isoinfo"), nil, 0o600))

	_, err = NewServer(c.p).Clone(ctx, &providerv1.CloneRequest{SourceVmId: "team-a.web", TargetName: r26Target.Name,
		TargetVm: &providerv1.ObjectIdentity{Uid: r26Target.UID, Namespace: r26Target.Namespace, Name: r26Target.Name}})
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "retried, never failed for good: %v", err)
	assert.Contains(t, err.Error(), "isoinfo")
	assert.NoFileExists(t, filepath.Join(c.images, "team-a.web-copy-disk.qcow2"), "nothing was copied")
	assert.Len(t, c.seedEntries(), 1, "no seed was made for the clone")
	assert.NotContains(t, c.log("virsh"), "team-a.web-copy", "nothing was defined")
}

// TestSingleHostClone_DefineFailureRemovesTheClonesSeed: when the define
// failed and the domain provably does not exist, the clone's seed is removed.
func TestSingleHostClone_DefineFailureRemovesTheClonesSeed(t *testing.T) {
	c := newCreateHost(t)
	ctx := context.Background()
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "fail-define"), nil, 0o600))
	_, err = c.p.Clone(ctx, r26CloneReq(""))
	require.Error(t, err)
	assert.Len(t, c.seedEntries(), 1, "only the source's seed remains")
}

func TestClustered_Clone_ResentCloneRendersTheSameSeed(t *testing.T) {
	var seeds []map[string]string
	for range 2 {
		fx := sourceWeb(t, nil)
		_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
		require.NoError(t, err)
		files := map[string]string{}
		for _, n := range []string{seedFileUserData, seedFileMetaData} {
			files[n] = fx.seedFile(cloneTargetDomain, n)
		}
		seeds = append(seeds, files)
	}
	assert.Equal(t, seeds[0], seeds[1])
}

// TestClustered_Clone_UnreadableSourceSeedIsRetriedBeforeTheCopy: a seed
// read that fails is a transient failure before the copy (Unavailable).
func TestClustered_Clone_UnreadableSourceSeedIsRetriedBeforeTheCopy(t *testing.T) {
	fx := sourceWeb(t, nil)
	fx.script("local", "fail-isoinfo", "")
	_, err := NewServer(fx.p).Clone(context.Background(), routedCloneReq())
	assert.Equal(t, codes.Unavailable, status.Code(err), "got %v", err)
	for _, c := range fx.calls() {
		assert.NotContains(t, c, "qemu-img convert", "nothing is copied: %q", c)
		assert.NotContains(t, c, " define ", "nothing is defined: %q", c)
	}
}

func TestReadSeedISO_Refusals(t *testing.T) {
	t.Run("an ISO without NoCloud files", func(t *testing.T) {
		c := newCreateHost(t)
		iso := filepath.Join(c.base, "other.iso")
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600))
		writeTar(t, iso, dir, "README")
		_, err := readSeedISO(context.Background(), c.vp, iso)
		require.Error(t, err)
		assert.False(t, contracts.IsRetryable(err), "a permanent refusal: %v", err)
		assert.Contains(t, err.Error(), "holds no NoCloud user-data or meta-data")
	})
	t.Run("a file over the bound", func(t *testing.T) {
		c := newCreateHost(t)
		iso := filepath.Join(c.base, "big.iso")
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, seedFileUserData), []byte(strings.Repeat("x", maxSeedFileBytes+1)), 0o600))
		writeTar(t, iso, dir, seedFileUserData)
		_, err := readSeedISO(context.Background(), c.vp, iso)
		require.Error(t, err)
		assert.False(t, contracts.IsRetryable(err), "%v", err)
		assert.Contains(t, err.Error(), "larger than")
	})
}

// writeTar packs names of dir into iso the way fakeGenisoimageScript does.
func writeTar(t *testing.T, iso, dir string, names ...string) {
	t.Helper()
	out, err := exec.Command("tar", append([]string{"-C", dir, "-cf", iso}, names...)...).CombinedOutput() //nolint:gosec // test fixture
	require.NoError(t, err, "%s", out)
}

// TestCloneSeed_RealISORoundTrip builds the seed with the REAL genisoimage,
// reads it with the REAL isoinfo, and gives a clone its seed the same way:
// the clone's ISO really holds the new identity.
func TestCloneSeed_RealISORoundTrip(t *testing.T) {
	requireRealHostTool(t, "genisoimage")
	requireRealHostTool(t, isoinfoTool)
	h := newFakeHost(t)
	vp := h.host("h1")
	staging := t.TempDir()
	p := &Provider{virshProvider: vp, hostStagingDir: staging}
	ctx := context.Background()

	cip := NewCloudInitProvider(vp)
	cip.stagingDir = staging
	srcISO, err := cip.PrepareCloudInit(ctx, CloudInitConfig{
		UserData: p.generateDefaultCloudInit("web"), InstanceID: "team-a.web", Hostname: "web",
		NetworkConfig: "version: 2\nethernets: {}\n",
	})
	require.NoError(t, err)
	src, err := readSeedISO(ctx, vp, srcISO)
	require.NoError(t, err)
	assert.Contains(t, src[seedFileMetaData], "instance-id: team-a.web")
	assert.Equal(t, "version: 2\nethernets: {}\n", src[seedFileNetworkConfig])

	plan, err := planCloneSeed(ctx, vp, srcISO, r26CloneReq(""), "team-a.web-copy")
	require.NoError(t, err)
	domainXML := "<domain><devices><disk type='file' device='cdrom'><source file='" + srcISO + "'/></disk></devices></domain>"
	out, seedDir, err := p.writeCloneSeed(ctx, vp, domainXML, plan)
	require.NoError(t, err)
	cloneISO := filepath.Join(seedDir, cloudInitISOName)
	assert.Contains(t, out, cloneISO)
	assert.True(t, isOwnSeedDirName(filepath.Base(seedDir), "team-a.web-copy"), "named after the clone's domain: %s", seedDir)

	clone, err := readSeedISO(ctx, vp, cloneISO)
	require.NoError(t, err)
	meta := metaOf(t, clone[seedFileMetaData])
	assert.Equal(t, "virtrigaud-"+r26Target.UID, meta["instance-id"])
	assert.Equal(t, "web-copy", meta["local-hostname"])
	assert.Contains(t, clone[seedFileUserData], "hostname: \"web-copy\"\n")
	assert.Equal(t, src[seedFileNetworkConfig], clone[seedFileNetworkConfig], "network-config is carried")
}

// TestSingleHostClone_SeedFailureIsRetryable pins review L7: a clone whose
// own seed cannot be made defined nothing, so it is answered retryably
// (Unavailable), never as an unknown outcome, and leaves no seed behind.
func TestSingleHostClone_SeedFailureIsRetryable(t *testing.T) {
	c := newCreateHost(t)
	ctx := context.Background()
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "fail-genisoimage"), nil, 0o600))
	defines := strings.Count(c.log("virsh"), "\tdefine ")

	_, err = NewServer(c.p).Clone(ctx, &providerv1.CloneRequest{SourceVmId: "team-a.web", TargetName: r26Target.Name,
		TargetVm: &providerv1.ObjectIdentity{Uid: r26Target.UID, Namespace: r26Target.Namespace, Name: r26Target.Name}})
	assert.Equal(t, codes.Unavailable, status.Code(err), "got %v", err)
	assert.Contains(t, err.Error(), "nothing was defined")
	assert.Len(t, c.seedEntries(), 1, "only the source's seed remains")
	assert.Equal(t, defines, strings.Count(c.log("virsh"), "\tdefine "), "nothing was defined")

	// The retry, once the host is fixed, makes the clone.
	require.NoError(t, os.Remove(filepath.Join(c.root, "fail-genisoimage")))
	resp, err := c.p.Clone(ctx, r26CloneReq(""))
	require.NoError(t, err)
	assert.Equal(t, "team-a.web-copy", resp.TargetVmID)
}

// userDataOnlySeed packs a seed holding only user-data (another tool's seed,
// or a hand-made one) and returns its path.
func userDataOnlySeed(t *testing.T, c *createHost, userData string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, seedFileUserData), []byte(userData), 0o600))
	iso := filepath.Join(c.base, "ud-only.iso")
	writeTar(t, iso, dir, seedFileUserData)
	return iso
}

// TestPlanCloneSeed_NoSourceMetaDataIsBuiltByTheEncoder pins review L1: for a
// source seed without meta-data the provider's template is filled through the
// YAML encoder, so a customization domain carrying newlines and YAML syntax
// stays one value and adds no key.
func TestPlanCloneSeed_NoSourceMetaDataIsBuiltByTheEncoder(t *testing.T) {
	c := newCreateHost(t)
	iso := userDataOnlySeed(t, c, "#cloud-config\n")
	plan, err := planCloneSeed(context.Background(), c.vp, iso,
		r26CloneReq(`{"domain":"lab\nruncmd: [reboot]\ninstance-id: evil"}`), "team-a.web-copy")
	require.NoError(t, err)
	meta := metaOf(t, plan.config.MetaData)
	assert.Equal(t, "virtrigaud-"+r26Target.UID, meta["instance-id"])
	assert.Equal(t, "web-copy.lab\nruncmd: [reboot]\ninstance-id: evil", meta["local-hostname"])
	assert.NotContains(t, meta, "runcmd")
	assert.NotNil(t, meta["network"], "the provider's DHCP network config, as Create writes it")
	assert.NotContains(t, plan.config.MetaData, cloneMetaDataPlaceholder)
}

// TestPlanCloneSeed_InlineUserDataIsBounded pins review L9.
func TestPlanCloneSeed_InlineUserDataIsBounded(t *testing.T) {
	c := newCreateHost(t)
	iso := userDataOnlySeed(t, c, "#cloud-config\n")
	big := "#cloud-config\n# " + strings.Repeat("x", maxSeedFileBytes)
	_, err := planCloneSeed(context.Background(), c.vp, iso, r26CloneReq(`{"userData":{"cloudInit":{"inline":`+jsonString(big)+`}}}`),
		"team-a.web-copy")
	require.Error(t, err)
	assert.True(t, contracts.IsInvalidSpec(err), "InvalidArgument on the wire: %v", err)
	assert.NotContains(t, err.Error(), "xxxx", "the user-data is never echoed")
}

// TestPlanCloneSeed_WarnsAboutIdentityOverrides covers review L4: user-data
// keys that keep the source's identity are named in a warning — never their
// values.
func TestPlanCloneSeed_WarnsAboutIdentityOverrides(t *testing.T) {
	c := newCreateHost(t)
	iso := userDataOnlySeed(t, c, "#cloud-config\nssh_deletekeys: false\nssh_keys:\n  rsa_private: SECRETKEY\n")
	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	_, err := planCloneSeed(context.Background(), c.vp, iso, r26CloneReq(""), "team-a.web-copy")
	require.NoError(t, err)
	assert.Contains(t, logs.String(), "ssh_deletekeys, ssh_keys")
	assert.NotContains(t, logs.String(), "SECRETKEY")
}

// r26Secret is user-data content no error or log line may carry.
const r26Secret = "password: hunter2-r26-secret"

// TestReadSeedISO_PartialOutputNeverInErrorsOrLogs pins review M1: a read of
// the source's seed that fails after it printed part of the user-data (the
// SSH session dropped, or the context was cancelled, mid `isoinfo | head -c`)
// leaves that output in the VirshError's Stdout, which its Error() prints.
// Neither the Clone's error nor anything the provider logs may carry it.
func TestReadSeedISO_PartialOutputNeverInErrorsOrLogs(t *testing.T) {
	c := newCreateHost(t)
	ctx := context.Background()
	req := c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2"))
	req.UserData = &contracts.UserData{CloudInitData: "#cloud-config\n" + r26Secret + "\n"}
	_, err := c.p.Create(ctx, req)
	require.NoError(t, err)
	srcISO := filepath.Join(c.staging, c.seedOf("team-a.web"), cloudInitISOName)

	// head prints what it read, then fails: the failed command's stdout holds
	// the secret.
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "head"), []byte("#!/bin/sh\ncat\nexit 255\n"), 0o700)) //nolint:gosec // test fixture must be executable
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, direct := runHost(ctx, c.vp, "sh", "-c", seedFileReadScript, "sh", srcISO, "-R", "/user-data", "10")
	require.Error(t, direct)
	require.Contains(t, direct.Error(), "hunter2", "the fixture really leaves the secret in the raw error")

	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	_, err = NewServer(c.p).Clone(ctx, &providerv1.CloneRequest{SourceVmId: "team-a.web", TargetName: r26Target.Name,
		TargetVm: &providerv1.ObjectIdentity{Uid: r26Target.UID, Namespace: r26Target.Namespace, Name: r26Target.Name}})
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "retried: %v", err)
	assert.NotContains(t, err.Error(), "hunter2")
	assert.NotContains(t, logs.String(), "hunter2", "nothing logged carries the user-data")
	assert.Contains(t, logs.String(), "exit code 255", "the cause is logged as exit code and stderr")

	_, rerr := readSeedISO(ctx, c.vp, srcISO)
	require.Error(t, rerr)
	assert.NotContains(t, rerr.Error(), "hunter2")
	assert.ErrorIs(t, rerr, errSeedReadFailed)
}

func TestSeedReadFailure(t *testing.T) {
	leak := &VirshError{Command: "sh -c ...", ExitCode: -1, Stderr: "connection reset", Stdout: r26Secret}
	got := seedReadFailure(fmt.Errorf("wrapped: %w", leak))
	assert.NotContains(t, got.Error(), "hunter2")
	assert.Contains(t, got.Error(), "connection reset")
	assert.ErrorIs(t, seedReadFailure(context.Canceled), context.Canceled)
	assert.ErrorIs(t, seedReadFailure(context.DeadlineExceeded), context.DeadlineExceeded)
	assert.Equal(t, errSeedReadFailed, seedReadFailure(fmt.Errorf("anything %s", r26Secret)), "other errors are reduced to a constant")
}
