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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests drive the WHOLE single-host create pipeline — storage, image
// confinement and copy, cloud-init seed, domain XML staging and define —
// against a fake hypervisor host (the fakeHost scratch tree of
// imagepath_test.go, with a virsh that records defined domains and a
// genisoimage that packs the seed), to prove that namespaced domain names keep
// two tenants' "web" VMs apart on one host, and that every staged file is
// unique to one create and cleaned up.

// createHostVirshScript extends fakeVirshScript with what a full create needs:
// `list --all` reports the defined domains, and `define <file>` records the
// domain (by name and by UUID) the way libvirt would, refusing a name that is
// already defined. A file named fail-define in the host directory makes define
// fail. With no -c URI (runRemoteVirshCommand on a non-system URI) the host is
// $FAKE_DEFAULT_HOST.
const createHostVirshScript = `#!/bin/sh
uri=""
if [ "$1" = "-c" ]; then uri="$2"; shift 2; fi
host="${uri##*/}"
if [ -z "$host" ]; then host="$FAKE_DEFAULT_HOST"; fi
d="$FAKE_HOST_DIR/$host"
printf '%s\t%s\n' "$host" "$*" >> "$FAKE_HOST_DIR/virsh.log"
case "$1" in
  list)
    if [ "$3" = "--uuid" ]; then cat "$d/uuids" 2>/dev/null; exit 0; fi
    printf ' Id   %-60s State\n' Name
    printf '%s\n' '----------------------------------------------------------------------------'
    if [ -f "$d/names" ]; then
      while read -r n; do printf ' -    %-60s shut off\n' "$n"; done < "$d/names"
    fi
    printf '\n' ;;
  dumpxml)
    if [ -f "$d/dom-$2.xml" ]; then exec cat "$d/dom-$2.xml"; fi
    echo "error: failed to get domain '$2'" >&2; exit 1 ;;
  define)
    if [ -f "$d/fail-define" ]; then echo "error: scripted define failure" >&2; exit 1; fi
    name=$(sed -n 's:.*<name>\(.*\)</name>.*:\1:p' "$2" | head -n 1)
    uuid=$(sed -n 's:.*<uuid>\(.*\)</uuid>.*:\1:p' "$2" | head -n 1)
    if [ -f "$d/dom-$name.xml" ]; then echo "error: domain '$name' already exists" >&2; exit 1; fi
    cp "$2" "$d/dom-$name.xml"
    cp "$2" "$d/dom-$uuid.xml"
    echo "$name" >> "$d/names"
    echo "$uuid" >> "$d/uuids"
    cp "$2" "$d/defined-from"
    echo "$2" > "$d/defined-path"
    if [ -f "$d/define-reply-lost" ]; then echo "error: Disconnected from the hypervisor due to end of file" >&2; exit 1; fi
    echo "Domain '$name' defined from $2" ;;
  domuuid)
    if [ -f "$d/fail-domuuid" ]; then echo "error: failed to connect to the hypervisor" >&2; exit 1; fi
    if [ -f "$d/dom-$2.xml" ]; then sed -n 's:.*<uuid>\(.*\)</uuid>.*:\1:p' "$d/dom-$2.xml" | head -n 1; exit 0; fi
    echo "error: failed to get domain '$2'" >&2; exit 1 ;;
  pool-list) printf ' Name      State    Autostart\n-------------------------------\n default   active   yes\n\n' ;;
  pool-info) printf 'Name:           default\nState:          running\n' ;;
  pool-dumpxml) printf "<pool type='dir'><name>default</name><target><path>%s</path></target></pool>\n<!-- /var/lib/libvirt/images -->\n" "$(cat "$d/pooldir")" ;;
  vol-path) exec cat "$d/vol-$3-$5" ;;
  *) exit 0 ;;
esac
`

// fakeGenisoimageScript packs the seed directory's user-data and meta-data into
// the -output file (so a test can see WHICH user-data went into which ISO), or
// fails when $FAKE_HOST_DIR/fail-genisoimage exists.
const fakeGenisoimageScript = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_HOST_DIR/genisoimage.log"
if [ -f "$FAKE_HOST_DIR/fail-genisoimage" ]; then echo "genisoimage: scripted failure" >&2; exit 1; fi
out=""; prev=""; last=""
for a in "$@"; do
  if [ "$prev" = "-output" ]; then out="$a"; fi
  prev="$a"; last="$a"
done
cat "$last/user-data" "$last/meta-data" > "$out"
`

// createHost is a fake hypervisor host that can run a full create.
type createHost struct {
	*fakeHost
	vp      *VirshProvider
	p       *Provider
	staging string
}

func newCreateHost(t *testing.T) *createHost {
	t.Helper()
	h := newFakeHost(t)
	bin := t.TempDir()
	for name, script := range map[string]string{"virsh": createHostVirshScript, "genisoimage": fakeGenisoimageScript} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700)) //nolint:gosec // test fixture must be executable
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DEFAULT_HOST", "h1")

	vp := h.host("h1")
	staging := t.TempDir()
	return &createHost{
		fakeHost: h,
		vp:       vp,
		p:        &Provider{virshProvider: vp, imageDirs: []string{h.images}, hostStagingDir: staging},
		staging:  staging,
	}
}

// domainXML returns the definition the fake recorded for name.
func (c *createHost) domainXML(name string) string {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.root, "h1", "dom-"+name+".xml")) //nolint:gosec // test reads its own fixture
	require.NoError(c.t, err, "domain %s must have been defined", name)
	return string(b)
}

// stagingEntries lists the names in the staging directory.
func (c *createHost) stagingEntries() []string {
	c.t.Helper()
	entries, err := os.ReadDir(c.staging)
	require.NoError(c.t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// createReq is a create for namespace/web from a base image, carrying user-data
// that names its tenant.
func (c *createHost) createReq(owner contracts.ObjectIdentity, image string) contracts.CreateRequest {
	return contracts.CreateRequest{
		Name:     owner.Name,
		Owner:    owner,
		Image:    contracts.VMImage{Path: image},
		UserData: &contracts.UserData{CloudInitData: "#cloud-config\n# secret-of-" + owner.Namespace + "\n"},
	}
}

func TestCreate_TwoNamespacesSameNameOnOneHost(t *testing.T) {
	c := newCreateHost(t)
	base := c.file(c.images, "ubuntu.qcow2")
	ctx := context.Background()

	respA, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(t, err)
	respB, err := c.p.Create(ctx, c.createReq(ownerTeamB, base))
	require.NoError(t, err, "team-b/web must not collide with team-a/web")

	assert.Equal(t, "team-a.web", respA.ID)
	assert.Equal(t, "team-b.web", respB.ID)

	for _, tc := range []struct {
		domain string
		owner  contracts.ObjectIdentity
		secret string
	}{
		{"team-a.web", ownerTeamA, "secret-of-team-a"},
		{"team-b.web", ownerTeamB, "secret-of-team-b"},
	} {
		d, err := parseDomainLibvirtxml(c.domainXML(tc.domain))
		require.NoError(t, err)
		assert.Equal(t, tc.domain, d.Name)

		owners, err := domainOwners(c.domainXML(tc.domain))
		require.NoError(t, err)
		assert.Equal(t, []contracts.ObjectIdentity{tc.owner}, owners, "each domain carries its own VM's stamp")

		refs, err := parseDomainPathRefs(c.domainXML(tc.domain))
		require.NoError(t, err)
		require.Len(t, refs.disks, 2, "primary disk + cloud-init CD-ROM")

		// Its own disk, named after the namespaced domain, copied from the base image.
		assert.Equal(t, filepath.Join(c.images, tc.domain+"-disk.qcow2"), refs.disks[0])
		assert.FileExists(t, refs.disks[0])

		// Its own cloud-init seed, in a per-create directory named after the domain.
		iso := refs.disks[1]
		seedDir := filepath.Dir(iso)
		assert.Equal(t, c.staging, filepath.Dir(seedDir))
		assert.True(t, strings.HasPrefix(filepath.Base(seedDir), cloudInitSeedDirPrefix+tc.domain+"."), "seed dir %s", seedDir)
		isoBytes, err := os.ReadFile(iso) //nolint:gosec // test reads the fake ISO
		require.NoError(t, err)
		assert.Contains(t, string(isoBytes), tc.secret, "the ISO carries this VM's user-data")
		assert.Contains(t, string(isoBytes), "instance-id: "+tc.domain)
		assert.Contains(t, string(isoBytes), "local-hostname: web", "the guest hostname stays the VM name")

		st, err := os.Stat(seedDir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o711), st.Mode().Perm(), "seed dir: traverse-only for the qemu user")
		ud, err := os.Stat(filepath.Join(seedDir, "user-data"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), ud.Mode().Perm(), "user-data stays private to the SSH user")
	}

	aISO, err := parseDomainPathRefs(c.domainXML("team-a.web"))
	require.NoError(t, err)
	bISO, err := parseDomainPathRefs(c.domainXML("team-b.web"))
	require.NoError(t, err)
	assert.NotEqual(t, aISO.disks[0], bISO.disks[0], "two disks")
	assert.NotEqual(t, filepath.Dir(aISO.disks[1]), filepath.Dir(bISO.disks[1]), "two seed directories")

	// No staged domain XML is left behind; only the two seed directories remain.
	entries := c.stagingEntries()
	assert.Len(t, entries, 2, "staging: %v", entries)
	for _, e := range entries {
		assert.True(t, strings.HasPrefix(e, cloudInitSeedDirPrefix), "unexpected staging leftover %q", e)
	}
	definedFrom, err := os.ReadFile(filepath.Join(c.root, "h1", "defined-path")) //nolint:gosec // test reads its fixture
	require.NoError(t, err)
	stagedXML := strings.TrimSpace(string(definedFrom))
	assert.True(t, strings.HasPrefix(filepath.Base(stagedXML), "team-b.web"+domainXMLStagingInfix), "staged as %s", stagedXML)
	assert.NoFileExists(t, stagedXML, "the staged domain XML is removed after define")

	// Owner-stamp idempotency: team-a's retried create (lost status write)
	// finds and binds its OWN namespaced domain; nothing is re-created.
	defines := strings.Count(c.log("virsh"), "\tdefine ")
	resp, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID)
	assert.Equal(t, defines, strings.Count(c.log("virsh"), "\tdefine "), "the retry defines nothing")
}

// TestCreate_StagedFilesAreUniquePerCreate: two creates of the same domain
// name never share a staging path (so a racing create cannot overwrite the
// other's user-data), and a failed create leaves no staging behind.
func TestCreate_StagingIsPerCreateAndCleanedOnFailure(t *testing.T) {
	c := newCreateHost(t)
	base := c.file(c.images, "ubuntu.qcow2")
	ctx := context.Background()

	// A define failure after the seed was prepared: both the staged XML and
	// the seed directory are removed.
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "fail-define"), nil, 0o600))
	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.Error(t, err)
	assert.Empty(t, c.stagingEntries(), "a failed create leaves no staging files")
	firstSeed := lastGenisoOutput(t, c)

	// The seed cannot be built: the directory is removed again.
	require.NoError(t, os.Remove(filepath.Join(c.root, "h1", "fail-define")))
	require.NoError(t, os.WriteFile(filepath.Join(c.root, "fail-genisoimage"), nil, 0o600))
	_, err = c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.Error(t, err)
	assert.Empty(t, c.stagingEntries())

	// The same VM retried: a fresh seed directory, never the earlier path.
	require.NoError(t, os.Remove(filepath.Join(c.root, "fail-genisoimage")))
	resp, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID)
	assert.NotEqual(t, filepath.Dir(firstSeed), filepath.Dir(lastGenisoOutput(t, c)), "every create gets its own seed directory")
}

// TestCreate_URLImageStagesPerDownload: a URL image is downloaded to a
// per-download mktemp file in the staging directory (never a predictable
// /tmp/<volume>-temp.img) and the file is removed after the convert.
func TestCreate_URLImageStagesPerDownload(t *testing.T) {
	c := newCreateHost(t)
	req := c.createReq(ownerTeamA, "")
	req.Image = contracts.VMImage{URL: "https://images.example/ubuntu.qcow2"}

	resp, err := c.p.Create(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID)

	dl := strings.Fields(strings.TrimSpace(c.log("wget")))
	require.GreaterOrEqual(t, len(dl), 2)
	staged := dl[1] // wget -O <staged> <url>
	assert.Equal(t, c.staging, filepath.Dir(staged))
	assert.True(t, strings.HasPrefix(filepath.Base(staged), "team-a.web-disk-temp.img."), "staged as %s", staged)
	assert.NoFileExists(t, staged, "the download is removed after the convert")
	assert.FileExists(t, filepath.Join(c.images, "team-a.web-disk.qcow2"))
	for _, e := range c.stagingEntries() {
		assert.True(t, strings.HasPrefix(e, cloudInitSeedDirPrefix), "unexpected staging leftover %q", e)
	}
}

// TestCreate_DefineReplyLost: `virsh define` succeeded on the host but its
// reply was lost. The create checks `virsh domuuid` against the UUID it
// generated and treats the define as done, so the seed ISO the new domain's
// CD-ROM references is kept (a retry then binds the owned domain). When the
// outcome cannot be established the seed is kept too; only a domain that is
// verifiably absent gets its seed removed.
func TestCreate_DefineReplyLost(t *testing.T) {
	ctx := context.Background()

	t.Run("define succeeded, reply lost: success, seed kept", func(t *testing.T) {
		c := newCreateHost(t)
		base := c.file(c.images, "ubuntu.qcow2")
		require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "define-reply-lost"), nil, 0o600))

		resp, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
		require.NoError(t, err)
		assert.Equal(t, "team-a.web", resp.ID)
		refs, err := parseDomainPathRefs(c.domainXML("team-a.web"))
		require.NoError(t, err)
		require.Len(t, refs.disks, 2)
		assert.FileExists(t, refs.disks[1], "the seed ISO the domain references is kept")
		assert.Contains(t, c.log("virsh"), "\tdomuuid team-a.web")

		// The retry binds the owned domain whose seed is still there.
		resp, err = c.p.Create(ctx, c.createReq(ownerTeamA, base))
		require.NoError(t, err)
		assert.Equal(t, "team-a.web", resp.ID)
		assert.FileExists(t, refs.disks[1])
	})

	t.Run("define failed and the check failed: error, seed kept", func(t *testing.T) {
		c := newCreateHost(t)
		base := c.file(c.images, "ubuntu.qcow2")
		require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "fail-define"), nil, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "fail-domuuid"), nil, 0o600))

		_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
		require.Error(t, err)
		assert.ErrorIs(t, err, errDefineOutcomeUnknown)
		entries := c.stagingEntries()
		require.Len(t, entries, 1, "only the seed directory is kept; the staged XML is removed")
		assert.True(t, strings.HasPrefix(entries[0], cloudInitSeedDirPrefix))
	})

	t.Run("define failed and the domain is absent: error, seed removed", func(t *testing.T) {
		c := newCreateHost(t)
		base := c.file(c.images, "ubuntu.qcow2")
		require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "fail-define"), nil, 0o600))

		_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
		require.Error(t, err)
		assert.NotErrorIs(t, err, errDefineOutcomeUnknown)
		assert.Empty(t, c.stagingEntries())
	})
}

func TestVerifyDefined(t *testing.T) {
	c := newCreateHost(t)
	hostDir := filepath.Join(c.root, "h1")
	mine := stampedDomainXML("team-a.web", ownerTeamA) // UUID 11111111-2222-4333-8444-555555555555

	assert.Equal(t, defineAbsent, verifyDefined(context.Background(), c.vp, "team-a.web", mine), "no such domain")

	require.NoError(t, os.WriteFile(filepath.Join(hostDir, "dom-team-a.web.xml"), []byte(mine), 0o600))
	assert.Equal(t, defineVerified, verifyDefined(context.Background(), c.vp, "team-a.web", mine))

	other := strings.Replace(mine, "11111111-2222-4333-8444-555555555555", "22222222-2222-4333-8444-555555555555", 1)
	assert.Equal(t, defineAbsent, verifyDefined(context.Background(), c.vp, "team-a.web", other),
		"a same-named domain with another UUID is not the one this create defined")

	require.NoError(t, os.WriteFile(filepath.Join(hostDir, "fail-domuuid"), nil, 0o600))
	assert.Equal(t, defineUnknown, verifyDefined(context.Background(), c.vp, "team-a.web", mine))
	assert.Equal(t, defineUnknown, verifyDefined(context.Background(), c.vp, "team-a.web", "<domain/>"), "no UUID to compare")
}

// lastGenisoOutput returns the -output path of the last genisoimage call.
func lastGenisoOutput(t *testing.T, c *createHost) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(c.log("genisoimage")), "\n")
	require.NotEmpty(t, lines)
	f := strings.Fields(lines[len(lines)-1])
	for i := range f {
		if f[i] == "-output" && i+1 < len(f) {
			return f[i+1]
		}
	}
	t.Fatalf("no -output in %q", lines[len(lines)-1])
	return ""
}

// TestCreate_NeverOverwritesAnotherDomainsDisk: a file at the VM's disk path
// that another domain uses is refused (Conflict, file untouched); one no
// domain uses — left by an earlier failed create of this same domain name — is
// overwritten.
func TestCreate_NeverOverwritesAnotherDomainsDisk(t *testing.T) {
	c := newCreateHost(t)
	base := c.file(c.images, "ubuntu.qcow2")
	ctx := context.Background()

	diskPath := filepath.Join(c.images, "team-a.web-disk.qcow2")
	require.NoError(t, os.WriteFile(diskPath, []byte("another VM's live disk"), 0o600))
	c.domain("h1", uuidA, diskDomainXML(diskPath)) // e.g. a legacy VM whose disk has this name

	_, err := c.p.Create(ctx, c.createReq(ownerTeamA, base))
	require.Error(t, err)
	assert.True(t, contracts.IsConflict(err), "got %v", err)
	assert.NotContains(t, err.Error(), uuidA, "the other domain is not disclosed")
	b, rerr := os.ReadFile(diskPath) //nolint:gosec // test reads its fixture
	require.NoError(t, rerr)
	assert.Equal(t, "another VM's live disk", string(b), "the other domain's disk is untouched")
	assert.NotContains(t, c.log("qemu-img"), "convert", "nothing was copied")
	assert.Empty(t, c.stagingEntries())

	// A leftover no domain uses is this domain name's own stale disk: replaced.
	c2 := newCreateHost(t)
	base2 := c2.file(c2.images, "ubuntu.qcow2")
	stale := filepath.Join(c2.images, "team-a.web-disk.qcow2")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	resp, err := c2.p.Create(ctx, c2.createReq(ownerTeamA, base2))
	require.NoError(t, err)
	assert.Equal(t, "team-a.web", resp.ID)
	b, rerr = os.ReadFile(stale) //nolint:gosec // test reads its fixture
	require.NoError(t, rerr)
	assert.Equal(t, "converted\n", string(b), "the stale file was replaced by the copy")
}

// TestCreate_LegacyManagerKeepsBareName: a request without an owner (a
// manager older than the provider) still creates the legacy bare-named domain
// and disk.
func TestCreate_LegacyManagerKeepsBareName(t *testing.T) {
	c := newCreateHost(t)
	base := c.file(c.images, "ubuntu.qcow2")

	req := c.createReq(ownerTeamA, base)
	req.Owner = contracts.ObjectIdentity{}
	resp, err := c.p.Create(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "web", resp.ID)
	assert.FileExists(t, filepath.Join(c.images, "web-disk.qcow2"))
	d, err := parseDomainLibvirtxml(c.domainXML("web"))
	require.NoError(t, err)
	assert.Equal(t, "web", d.Name)
}

// TestListVMs_ReportsOwnerStamp: ListVMs reports a created domain's owner UID
// (read from the dumpxml it already fetches) so adoption can skip domains a
// live VirtualMachine owns; an unstamped domain reports none.
func TestListVMs_ReportsOwnerStamp(t *testing.T) {
	c := newCreateHost(t)
	// Definitions as libvirt normalizes them (KiB units), as ListVMs reads them.
	hostDir := filepath.Join(c.root, "h1")
	for name, xml := range map[string]string{
		"team-a.web": stampedDomainXML("team-a.web", ownerTeamA),
		"web":        unstampedDomainXML("web"), // legacy / never created by VirtRigaud
	} {
		require.NoError(t, os.WriteFile(filepath.Join(hostDir, "dom-"+name+".xml"), []byte(xml), 0o600))
		f, err := os.OpenFile(filepath.Join(hostDir, "names"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = f.WriteString(name + "\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	before := strings.Count(c.log("virsh"), "\n")
	vms, err := c.p.ListVMs(context.Background())
	require.NoError(t, err)
	byID := map[string]contracts.VMInfo{}
	for _, v := range vms {
		byID[v.ID] = v
	}
	require.Contains(t, byID, "team-a.web")
	require.Contains(t, byID, "web")
	assert.Equal(t, ownerTeamA.UID, byID["team-a.web"].ProviderRaw[contracts.VMInfoOwnerUIDKey])
	assert.NotContains(t, byID["web"].ProviderRaw, contracts.VMInfoOwnerUIDKey)
	assert.Equal(t, 1+len(vms), strings.Count(c.log("virsh"), "\n")-before,
		"one list plus one dumpxml per domain, as before: no extra virsh call")
}

// TestClustered_Delete_PendingCreateFindsNamespacedDomain: the operator's
// finalizer cleans up a clustered create still in flight (no status.id) with an
// owner-checked Delete addressed by the BARE VM name. Create named the domain
// "<namespace>.<name>", so the provider also looks that name up — under the
// same owner check: its own domain is torn down (by UUID), another UID's is
// left untouched, and an id that is already a domain name is not re-derived.
func TestClustered_Delete_PendingCreateFindsNamespacedDomain(t *testing.T) {
	t.Run("own namespaced domain is deleted", func(t *testing.T) {
		fx := newOpsFixture(t, map[string]map[string]string{
			"host-a": {"team-a.web": routingDomainXML("team-a.web", ownerTeamA)}, "host-b": {},
		})
		p, _, _ := routedCluster(t)

		_, err := p.Delete(context.Background(), contracts.VMRef{ID: "web", HostID: "host-a", Owner: ownerTeamA})
		require.NoError(t, err)
		calls := fx.calls()
		assert.Contains(t, calls, "host-a dumpxml team-a.web")
		assert.Contains(t, calls, "host-a destroy "+routingDomainUUID, "torn down by the checked UUID")
		assert.Contains(t, calls, "host-a undefine "+routingDomainUUID)
	})

	t.Run("another UID's namespaced domain is untouched", func(t *testing.T) {
		fx := newOpsFixture(t, map[string]map[string]string{
			"host-a": {"team-a.web": routingDomainXML("team-a.web", staleTeamAWeb)}, "host-b": {},
		})
		p, _, _ := routedCluster(t)

		_, err := p.Delete(context.Background(), contracts.VMRef{ID: "web", HostID: "host-a", Owner: ownerTeamA})
		require.Error(t, err)
		assert.True(t, contracts.IsNotFound(err), "got %v", err)
		for _, c := range fx.calls() {
			assert.NotContains(t, c, "destroy")
			assert.NotContains(t, c, "undefine")
		}
	})

	t.Run("an id that is already the domain name is not re-derived", func(t *testing.T) {
		fx := newOpsFixture(t, map[string]map[string]string{"host-a": {}, "host-b": {}})
		p, _, _ := routedCluster(t)

		_, err := p.Delete(context.Background(), contracts.VMRef{ID: "team-a.web", HostID: "host-a", Owner: ownerTeamA})
		require.True(t, contracts.IsNotFound(err), "got %v", err)
		assert.Equal(t, []string{"host-a list --all"}, fx.calls(), "one lookup only")
	})
}

func TestPendingCreateDomainName(t *testing.T) {
	got, ok := pendingCreateDomainName("web", ownerTeamA)
	assert.True(t, ok)
	assert.Equal(t, "team-a.web", got)

	for _, tc := range []struct {
		id    string
		owner contracts.ObjectIdentity
	}{
		{"web", contracts.ObjectIdentity{}},         // no naming identity
		{"web", contracts.ObjectIdentity{UID: "u"}}, // uid only
		{"team-a.web", ownerTeamA},                  // already the domain name (status.id)
		{"db", ownerTeamA},                          // not this owner's name
	} {
		_, ok := pendingCreateDomainName(tc.id, tc.owner)
		assert.False(t, ok, "%q / %+v", tc.id, tc.owner)
	}
}
