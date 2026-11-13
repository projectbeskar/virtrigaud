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

package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

var (
	imageBytes = []byte("system extension image")
	releaseOK  = "ID=_any\nSYSEXT_VERSION_ID=1.5.0\nVERSION_ID=9.9.9\n"
)

// sysextTree is a host whose hypervisor is delivered as a system extension.
func sysextTree(t *testing.T, declaredDigest string) *tree {
	t.Helper()
	tr := healthyTree(t)
	require.NoError(t, os.Remove(tr.path("/var/lib/dpkg/status")))
	tr.write("/var/lib/extensions/cloud-hypervisor.raw", imageBytes)
	tr.write(ExtensionReleaseDir+"/extension-release.cloud-hypervisor", []byte(releaseOK))
	extra := `"artifact":{"name":"cloud-hypervisor.raw"`
	if declaredDigest != "" {
		extra += `,"sha256":"` + declaredDigest + `"`
	}
	extra += "}"
	tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", strings.Replace(manifestJSON("cloud-hypervisor", "1.5.0", extra), `"package"`, `"sysext"`, 1))
	return tr
}

func TestSysextArtifact(t *testing.T) {
	t.Run("ready, digest computed by the agent", func(t *testing.T) {
		inv := collectInventory(t, sysextTree(t, digestOf(imageBytes)), nil)
		c := component(t, inv, "cloud-hypervisor")
		assert.Equal(t, stReady, c.GetState())
		assert.Equal(t, rawDigest(imageBytes), c.GetArtifactSha256())
		assert.Equal(t, "1.5.0", c.GetObservedVersion(), "SYSEXT_VERSION_ID of the extension-release file, before VERSION_ID")
		assert.Equal(t, "sysext", c.GetSource())
		assert.Empty(t, inv.GetUnexpectedExtensions(), "a declared extension is not unexpected")
	})
	t.Run("declared digest differs", func(t *testing.T) {
		inv := collectInventory(t, sysextTree(t, digestOf([]byte("other"))), nil)
		c := component(t, inv, "cloud-hypervisor")
		assert.Equal(t, stUnhealthy, c.GetState())
		assert.Equal(t, ReasonArtifactDigestMismatch, c.GetStateReason())
		assert.Equal(t, rawDigest(imageBytes), c.GetArtifactSha256(), "the observed digest is still reported")
	})
	t.Run("image missing", func(t *testing.T) {
		tr := sysextTree(t, "")
		require.NoError(t, os.Remove(tr.path("/var/lib/extensions/cloud-hypervisor.raw")))
		c := component(t, collectInventory(t, tr, nil), "cloud-hypervisor")
		assert.Equal(t, stMissing, c.GetState())
		assert.Equal(t, ReasonNotInstalled, c.GetStateReason())
		assert.Empty(t, c.GetArtifactSha256())
	})
	t.Run("invalid artifact name", func(t *testing.T) {
		tr := sysextTree(t, "")
		tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", manifestJSON("cloud-hypervisor", "1.5.0", `"artifact":{"name":"../etc/shadow"}`))
		c := component(t, collectInventory(t, tr, nil), "cloud-hypervisor")
		assert.Equal(t, stInvalid, c.GetState())
		assert.Equal(t, ReasonInvalidName, c.GetStateReason())
	})
	t.Run("directory artifact has no digest", func(t *testing.T) {
		tr := sysextTree(t, "")
		require.NoError(t, os.Remove(tr.path("/var/lib/extensions/cloud-hypervisor.raw")))
		tr.mkdir("/var/lib/extensions/cloud-hypervisor.raw")
		c := component(t, collectInventory(t, tr, nil), "cloud-hypervisor")
		assert.Equal(t, stReady, c.GetState())
		assert.Empty(t, c.GetArtifactSha256())
	})
}

func TestUnexpectedExtensions(t *testing.T) {
	tr := healthyTree(t)
	tr.mkdir("/run/extensions/rogue-dir")
	tr.write("/etc/extensions/other.raw", []byte("other image"))
	tr.write("/run/extensions/other.raw", []byte("shadowed by /etc"))
	tr.write("/var/lib/extensions/odd name$.raw", []byte("odd"))
	tr.write("/var/lib/extensions/.hidden.raw", []byte("hidden"))
	tr.write("/var/lib/extensions/notes.txt", []byte("not an extension"))
	tr.mkdir("/usr/lib/extensions")
	require.NoError(t, os.Symlink("/nonexistent", tr.path("/usr/lib/extensions/linked")))
	inv := collectInventory(t, tr, nil)
	byName := map[string]*providerv1.HostExtension{}
	for _, e := range inv.GetUnexpectedExtensions() {
		byName[e.GetName()] = e
	}
	require.Len(t, byName, 4, "%v", inv.GetUnexpectedExtensions())
	assert.Equal(t, providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY, byName["rogue-dir"].GetKind())
	assert.Empty(t, byName["rogue-dir"].GetImageSha256())
	assert.Equal(t, providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY, byName["linked"].GetKind(), "a symbolic link is listed, never followed to read")
	assert.Equal(t, providerv1.ExtensionKind_EXTENSION_KIND_UNDECLARED_IMAGE, byName["other.raw"].GetKind())
	assert.Equal(t, rawDigest([]byte("other image")), byName["other.raw"].GetImageSha256(), "the earlier directory wins")
	for name, e := range byName {
		if strings.HasPrefix(name, "invalid-") {
			assert.Equal(t, providerv1.ExtensionKind_EXTENSION_KIND_UNDECLARED_IMAGE, e.GetKind())
			assert.Len(t, name, len("invalid-")+16)
			assert.NotContains(t, name, "odd")
		}
	}
	names := make([]string, 0, 4)
	for _, e := range inv.GetUnexpectedExtensions() {
		names = append(names, e.GetName())
	}
	assert.IsIncreasing(t, names)
}

func TestHashLimits(t *testing.T) {
	t.Run("one file above the per-file limit", func(t *testing.T) {
		tr := healthyTree(t)
		inv := collectInventory(t, tr, func(o *Options) { o.hashFile = int64(len(chBinary)) - 1 })
		c := component(t, inv, "cloud-hypervisor")
		assert.Equal(t, stUnhealthy, c.GetState())
		assert.Equal(t, ReasonDigestLimitExceeded, c.GetStateReason())
		assert.Empty(t, c.GetFiles(), "a file that was not hashed is not listed")
	})
	t.Run("the collection total", func(t *testing.T) {
		tr := healthyTree(t)
		inv := collectInventory(t, tr, func(o *Options) { o.hashTotal = int64(len(chBinary)) })
		unhealthy := 0
		for _, c := range inv.GetComponents() {
			if c.GetStateReason() == ReasonDigestLimitExceeded {
				unhealthy++
			}
		}
		assert.Equal(t, 1, unhealthy, "the second file no longer fits")
	})
	t.Run("an undeclared image above the limit cannot be listed", func(t *testing.T) {
		tr := healthyTree(t)
		tr.write("/etc/extensions/huge.raw", bytes.Repeat([]byte{1}, 100))
		inv := collectInventory(t, tr, func(o *Options) { o.hashFile = 50 })
		assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, inv.GetState())
		assert.Equal(t, ReasonProbeFailed, inv.GetStateReason())
		assert.Empty(t, inv.GetComponents())
	})
}

func TestOverLimitNeverTruncates(t *testing.T) {
	overLimitReason := func(t *testing.T, inv *providerv1.HostInventory, want string) {
		t.Helper()
		assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT, inv.GetState())
		assert.Equal(t, want, inv.GetStateReason())
		assert.Empty(t, inv.GetComponents())
		assert.Empty(t, inv.GetFeatures())
		assert.Empty(t, inv.GetUnexpectedExtensions())
		assert.Equal(t, providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_UNSPECIFIED, inv.GetProcessIdentityTier())
	}
	t.Run("more than 64 manifest directory entries", func(t *testing.T) {
		tr := healthyTree(t)
		for i := range MaxManifestEntries {
			tr.manifest(WritableManifestDir, fmt.Sprintf("m%02d.json", i), "{}")
		}
		overLimitReason(t, collectInventory(t, tr, nil), ReasonTooManyComponents)
	})
	t.Run("exactly 64 entries are allowed", func(t *testing.T) {
		tr := healthyTree(t)
		for i := range MaxManifestEntries {
			tr.manifest(WritableManifestDir, fmt.Sprintf("m%02d.txt", i), "ignored")
		}
		assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_COMPLETE, collectInventory(t, tr, nil).GetState())
	})
	t.Run("more than 32 components", func(t *testing.T) {
		tr := healthyTree(t)
		for i := range MaxComponents - 1 { // 2 catalog components + 31 invalid = 33
			tr.manifest(WritableManifestDir, fmt.Sprintf("bad%02d.json", i), "{")
		}
		overLimitReason(t, collectInventory(t, tr, nil), ReasonTooManyComponents)
	})
	t.Run("exactly 32 components are allowed", func(t *testing.T) {
		tr := healthyTree(t)
		for i := range MaxComponents - 2 {
			tr.manifest(WritableManifestDir, fmt.Sprintf("bad%02d.json", i), "{")
		}
		inv := collectInventory(t, tr, nil)
		assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_COMPLETE, inv.GetState())
		assert.Len(t, inv.GetComponents(), MaxComponents)
	})
	t.Run("more than 8 files in a component", func(t *testing.T) {
		tr := newTree(t)
		cat := CatalogEntry{Name: "many", Range: ">=0.0.0"}
		for i := range MaxFilesPerComponent + 1 {
			name := fmt.Sprintf("f%d", i)
			cat.Files = append(cat.Files, CatalogFile{Name: name, Path: "/f/" + name})
			tr.write("/f/"+name, []byte(name))
		}
		tr.manifest(ReadOnlyManifestDir, "many.json", manifestJSON("many", "1", ""))
		inv := collectInventory(t, tr, func(o *Options) { o.Catalog = []CatalogEntry{cat} })
		overLimitReason(t, inv, ReasonTooManyFiles)
	})
	t.Run("more than 64 files across components", func(t *testing.T) {
		tr := newTree(t)
		var cat []CatalogEntry
		for c := range 9 {
			e := CatalogEntry{Name: fmt.Sprintf("c%d", c), Range: ">=0.0.0"}
			for i := range MaxFilesPerComponent {
				name := fmt.Sprintf("f%d", i)
				e.Files = append(e.Files, CatalogFile{Name: name, Path: fmt.Sprintf("/f/%d/%s", c, name)})
				tr.write(fmt.Sprintf("/f/%d/%s", c, name), []byte(name))
			}
			cat = append(cat, e)
		}
		inv := collectInventory(t, tr, func(o *Options) { o.Catalog = cat })
		overLimitReason(t, inv, ReasonTooManyFiles)
	})
	t.Run("more than 32 features", func(t *testing.T) {
		var ps []probe
		for i := range MaxFeatures + 1 {
			ps = append(ps, probe{name: fmt.Sprintf("p%02d", i), run: func(context.Context, *env) (providerv1.FeatureResult, string) { return present, "" }})
		}
		c, _ := newCollector(t, healthyTree(t), func(o *Options) { o.probes = ps })
		c.CollectNow(context.Background())
		overLimitReason(t, c.Inventory(1), ReasonTooManyFeatures)
	})
	t.Run("more than 16 unexpected extensions", func(t *testing.T) {
		tr := healthyTree(t)
		for i := range MaxExtensions + 1 {
			tr.mkdir(fmt.Sprintf("/run/extensions/ext%02d", i))
		}
		overLimitReason(t, collectInventory(t, tr, nil), ReasonTooManyExtensions)
	})
	t.Run("more than 256 entries in an extension directory", func(t *testing.T) {
		tr := healthyTree(t)
		for i := range maxExtensionEntries + 1 {
			tr.write(fmt.Sprintf("/run/extensions/f%03d.txt", i), nil)
		}
		overLimitReason(t, collectInventory(t, tr, nil), ReasonTooManyExtensions)
	})
	t.Run("encoded report above the size bound", func(t *testing.T) {
		inv := collectInventory(t, healthyTree(t), func(o *Options) { o.MaxInventoryBytes = 100 })
		overLimitReason(t, inv, ReasonInventoryTooLarge)
	})
}

// failingFS fails the paths it is told to.
type failingFS struct {
	FS
	failDir  map[string]error
	failOpen map[string]error
}

func (f failingFS) ReadDir(p string, limit int) ([]DirEntry, bool, error) {
	if err := f.failDir[p]; err != nil {
		return nil, false, err
	}
	return f.FS.ReadDir(p, limit)
}

func (f failingFS) Open(p string, follow bool) (File, error) {
	if err := f.failOpen[p]; err != nil {
		return nil, err
	}
	return f.FS.Open(p, follow)
}

func TestUnreadableStates(t *testing.T) {
	secret := errors.New("open /etc/very/secret/path: SECRET-TOKEN-1234")
	tests := []struct {
		name   string
		fs     func(FS) FS
		reason string
	}{
		{"manifest directory", func(f FS) FS {
			return failingFS{FS: f, failDir: map[string]error{WritableManifestDir: fmt.Errorf("%w: %w", fs.ErrPermission, secret)}}
		}, ReasonManifestDirUnreadable},
		{"manifest file", func(f FS) FS {
			return failingFS{FS: f, failOpen: map[string]error{ReadOnlyManifestDir + "/agent.json": secret}}
		}, ReasonIOError},
		{"component file", func(f FS) FS {
			return failingFS{FS: f, failOpen: map[string]error{"/usr/bin/cloud-hypervisor": fs.ErrPermission}}
		}, ReasonIOError},
		{"extension directory", func(f FS) FS {
			return failingFS{FS: f, failDir: map[string]error{"/run/extensions": secret}}
		}, ReasonIOError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := healthyTree(t)
			tr.mkdir(WritableManifestDir)
			inv := collectInventory(t, tr, func(o *Options) { o.FS = tt.fs(o.FS) })
			assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, inv.GetState())
			assert.Equal(t, tt.reason, inv.GetStateReason())
			assert.Empty(t, inv.GetComponents())
			raw, err := proto.Marshal(inv)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "SECRET-TOKEN", "an error text is never copied into the report")
		})
	}
}

func TestFeaturesAndTier(t *testing.T) {
	features := func(inv *providerv1.HostInventory) map[string]*providerv1.HostFeature {
		m := map[string]*providerv1.HostFeature{}
		for _, f := range inv.GetFeatures() {
			m[f.GetName()] = f
		}
		return m
	}
	t.Run("healthy modern host", func(t *testing.T) {
		tr := healthyTree(t)
		tr.write(secureBootVariable, []byte{7, 0, 0, 0, 1})
		tr.write(measuredBootLog, []byte("log"))
		inv := collectInventory(t, tr, nil)
		f := features(inv)
		require.Len(t, f, len(valueSets[1].features))
		for _, name := range []string{FeatureCgroupV2, FeatureCloneIntoCgroup, FeaturePidfd, FeaturePidfsHandle, FeaturePidfdInfoExit,
			FeatureInitialPIDNamespace, FeatureInitialCgroupNamespace, FeatureInitialTimeNamespace, FeatureSecureBoot, FeatureMeasuredBoot} {
			assert.Equal(t, providerv1.FeatureResult_FEATURE_RESULT_PRESENT, f[name].GetResult(), name)
		}
		for _, name := range []string{FeatureHypervisorPauseResume, FeatureExtensionImagePolicySigned} {
			assert.Equal(t, providerv1.FeatureResult_FEATURE_RESULT_ERROR, f[name].GetResult(), "%s cannot be decided as a data read", name)
			assert.Equal(t, FeatureReasonProbeFailed, f[name].GetReason(), name)
		}
		assert.Equal(t, providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE, inv.GetProcessIdentityTier())
	})
	t.Run("old host", func(t *testing.T) {
		h := healthyHost()
		h.kernel, h.cgroup2, h.handle, h.pidfdErr = "5.4.0-1", false, false, syscall.ENOSYS
		h.ns["pid"] = 4026532000
		delete(h.ns, "time")
		inv := collectInventory(t, healthyTree(t), func(o *Options) { o.Host = h })
		f := features(inv)
		for _, name := range []string{FeatureCgroupV2, FeatureCloneIntoCgroup, FeaturePidfd, FeaturePidfsHandle, FeaturePidfdInfoExit,
			FeatureInitialPIDNamespace, FeatureInitialTimeNamespace, FeatureSecureBoot, FeatureMeasuredBoot} {
			assert.Equal(t, providerv1.FeatureResult_FEATURE_RESULT_ABSENT, f[name].GetResult(), name)
			assert.Empty(t, f[name].GetReason(), name)
		}
		assert.Equal(t, providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME, inv.GetProcessIdentityTier())
	})
	t.Run("middle kernel gives the inode tier", func(t *testing.T) {
		h := healthyHost()
		h.kernel, h.handle = "6.10.1", false
		inv := collectInventory(t, healthyTree(t), func(o *Options) { o.Host = h })
		assert.Equal(t, providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_INODE, inv.GetProcessIdentityTier())
	})
	t.Run("probe errors map to closed reasons", func(t *testing.T) {
		h := healthyHost()
		h.pidfdErr = fmt.Errorf("SECRET: %w", fs.ErrPermission)
		h.handleErr = errors.New("SECRET-TOKEN something broke")
		h.kernel = "garbage"
		inv := collectInventory(t, healthyTree(t), func(o *Options) { o.Host = h })
		f := features(inv)
		assert.Equal(t, FeatureReasonPermissionDenied, f[FeaturePidfd].GetReason())
		assert.Equal(t, FeatureReasonProbeFailed, f[FeaturePidfsHandle].GetReason())
		assert.Equal(t, FeatureReasonProbeFailed, f[FeatureCloneIntoCgroup].GetReason())
		raw, err := proto.Marshal(inv)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "SECRET")
	})
	t.Run("secure boot variable shapes", func(t *testing.T) {
		for name, tt := range map[string]struct {
			content []byte
			want    providerv1.FeatureResult
		}{
			"disabled":  {[]byte{7, 0, 0, 0, 0}, providerv1.FeatureResult_FEATURE_RESULT_ABSENT},
			"truncated": {[]byte{7, 0}, providerv1.FeatureResult_FEATURE_RESULT_ERROR},
			"oversized": {bytes.Repeat([]byte{1}, 64), providerv1.FeatureResult_FEATURE_RESULT_ERROR},
		} {
			tr := healthyTree(t)
			tr.write(secureBootVariable, tt.content)
			assert.Equal(t, tt.want, features(collectInventory(t, tr, nil))[FeatureSecureBoot].GetResult(), name)
		}
	})
}

// buildELF returns a minimal 64-bit little-endian ELF file whose only
// interesting section is .note.package with a packaging metadata note.
func buildELF(desc string) []byte {
	name := []byte("FDO\x00")
	d := append([]byte(desc), 0)
	for len(d)%4 != 0 {
		d = append(d, 0)
	}
	var note bytes.Buffer
	for _, v := range []uint32{uint32(len(name)), uint32(len(desc) + 1), fdoPackageNote} {
		_ = binary.Write(&note, binary.LittleEndian, v)
	}
	note.Write(name)
	note.Write(d)
	shstr := []byte("\x00.note.package\x00.shstrtab\x00")
	const ehsize, shentsize = 64, 64
	noteOff := ehsize
	strOff := noteOff + note.Len()
	shOff := strOff + len(shstr)
	var b bytes.Buffer
	hdr := make([]byte, ehsize)
	copy(hdr, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	le := binary.LittleEndian
	le.PutUint16(hdr[16:], 2)  // ET_EXEC
	le.PutUint16(hdr[18:], 62) // x86-64
	le.PutUint32(hdr[20:], 1)
	le.PutUint64(hdr[40:], uint64(shOff))
	le.PutUint16(hdr[52:], ehsize)
	le.PutUint16(hdr[58:], shentsize)
	le.PutUint16(hdr[60:], 3)
	le.PutUint16(hdr[62:], 2)
	b.Write(hdr)
	b.Write(note.Bytes())
	b.Write(shstr)
	sh := func(name, typ uint32, off, size uint64) {
		e := make([]byte, shentsize)
		le.PutUint32(e[0:], name)
		le.PutUint32(e[4:], typ)
		le.PutUint64(e[24:], off)
		le.PutUint64(e[32:], size)
		b.Write(e)
	}
	sh(0, 0, 0, 0)
	sh(1, 7, uint64(noteOff), uint64(note.Len())) // SHT_NOTE
	sh(15, 3, uint64(strOff), uint64(len(shstr))) // SHT_STRTAB
	return b.Bytes()
}

func TestELFNoteVersion(t *testing.T) {
	tr := healthyTree(t)
	require.NoError(t, os.Remove(tr.path("/var/lib/dpkg/status")))
	tr.write("/usr/bin/cloud-hypervisor", buildELF(`{"type":"deb","name":"cloud-hypervisor","version":"1.7.0"}`))
	inv := collectInventory(t, tr, nil)
	c := component(t, inv, "cloud-hypervisor")
	assert.Equal(t, "1.7.0", c.GetObservedVersion(), "read from the ELF note as data")
	assert.Equal(t, stReady, c.GetState())

	for name, content := range map[string][]byte{
		"not elf":      []byte("#!/bin/sh\necho 9.9.9"),
		"bad note":     buildELF(`{"version":"has spaces and ; chars"}`),
		"not json":     buildELF(`version=1.0.0`),
		"truncated":    buildELF(`{"version":"1.0.0"}`)[:100],
		"empty binary": nil,
	} {
		tr := healthyTree(t)
		require.NoError(t, os.Remove(tr.path("/var/lib/dpkg/status")))
		tr.write("/usr/bin/cloud-hypervisor", content)
		c := component(t, collectInventory(t, tr, nil), "cloud-hypervisor")
		assert.Equal(t, stIncompatible, c.GetState(), name)
		assert.Equal(t, ReasonVersionUnreadable, c.GetStateReason(), name)
	}
}

func TestVersionSourceOrder(t *testing.T) {
	// extension-release beats the ELF note, which beats the package database.
	tr := sysextTree(t, "")
	tr.write("/usr/bin/cloud-hypervisor", buildELF(`{"version":"2.0.0"}`))
	tr.write("/var/lib/dpkg/status", []byte("Package: cloud-hypervisor\nStatus: install ok installed\nVersion: 3.0.0\n\n"))
	assert.Equal(t, "1.5.0", component(t, collectInventory(t, tr, nil), "cloud-hypervisor").GetObservedVersion())
	require.NoError(t, os.Remove(tr.path(ExtensionReleaseDir+"/extension-release.cloud-hypervisor")))
	assert.Equal(t, "2.0.0", component(t, collectInventory(t, tr, nil), "cloud-hypervisor").GetObservedVersion())
	tr.write("/usr/bin/cloud-hypervisor", chBinary)
	assert.Equal(t, "3.0.0", component(t, collectInventory(t, tr, nil), "cloud-hypervisor").GetObservedVersion())
}

func TestVersionRanges(t *testing.T) {
	tests := []struct {
		rng, version string
		want         bool
	}{
		{">=1.0.0", "1.0.0", true}, {">=1.0.0", "0.9.9", false}, {">=1.0.0", "v2.3", true},
		{">=1.0.0, <2.0.0", "1.9.9-3", true}, {">=1.0.0, <2.0.0", "2.0.0", false},
		{">1.0", "1.0.0", false}, {"<=1.0", "1.0.0", true}, {"=1.2", "1.2.0", true}, {">=1.0.0", "garbage", false},
		{">=1.0.0", "", false}, {">=1.0.0", "1.0.0+dfsg~rc1", true},
	}
	for _, tt := range tests {
		r, err := parseRange(tt.rng)
		require.NoError(t, err, tt.rng)
		assert.Equal(t, tt.want, r.contains(tt.version), "%s in %q", tt.version, tt.rng)
	}
	for _, bad := range []string{"", ">=", "1.0.0", ">=x", ",", ">=1.0.0,"} {
		_, err := parseRange(bad)
		assert.Error(t, err, "%q", bad)
	}
}

func TestPackageDBParsers(t *testing.T) {
	dpkg := "Package: a\nStatus: install ok installed\nVersion: 1.1\n\nPackage: tool\nStatus: install ok installed\nVersion: 4.5-2\n\n"
	v, ok := packageDBVersion(strings.NewReader(dpkg), "tool")
	assert.True(t, ok)
	assert.Equal(t, "4.5-2", v)
	apk := "P:musl\nV:1.2.5-r0\n\nP:tool\nV:7.8.9-r1\n\n"
	v, ok = packageDBVersion(strings.NewReader(apk), "tool")
	assert.True(t, ok)
	assert.Equal(t, "7.8.9-r1", v)
	_, ok = packageDBVersion(strings.NewReader(dpkg), "absent")
	assert.False(t, ok)
	_, ok = packageDBVersion(strings.NewReader("Package: tool\nVersion: "+strings.Repeat("9", 100)+"\n\n"), "tool")
	assert.False(t, ok, "a version above 64 bytes is refused")
	_, ok = packageDBVersion(strings.NewReader("Package: tool\nVersion: 1\n"+strings.Repeat("x", maxPackageDBLine+10)+"\n"), "tool")
	assert.False(t, ok, "a line above the bound stops the parser")
}

func TestExtensionReleaseParser(t *testing.T) {
	for in, want := range map[string]string{
		"SYSEXT_VERSION_ID=1.0\nVERSION_ID=2.0\n": "1.0",
		"VERSION_ID=\"2.0\"\n":                    "2.0",
		"# c\nVERSION_ID='3.1'":                   "3.1",
		"VERSION_ID=bad value\n":                  "",
		"":                                        "",
	} {
		v, ok := extensionReleaseVersion([]byte(in))
		assert.Equal(t, want != "", ok, in)
		assert.Equal(t, want, v, in)
	}
}

func TestRedactionCanary(t *testing.T) {
	const (
		secret = "AKIAIOSFODNN7EXAMPLE-secret-canary"
		vmUID  = "0b6f1c1e-6d2e-4b0a-9a53-5ce1a8f1c0de"
	)
	tr := healthyTree(t)
	// Planted in the manifest's free fields, a manifest's base name, a path and
	// an error.
	tr.manifest(WritableManifestDir, "vm-"+vmUID+".json", `{"schemaVersion":1,"component":"cloud-hypervisor","version":"1","note":"`+secret+`"}`)
	tr.manifest(WritableManifestDir, "x.json", `{"schemaVersion":1,"component":"guest-kernel","version":"`+secret+` with space","source":"`+secret+`"}`)
	tr.manifest(WritableManifestDir, "y.json", `{"schemaVersion":1,"component":"/etc/shadow","version":"1"}`)
	inv := collectInventory(t, tr, func(o *Options) {
		o.FS = failingFS{FS: o.FS, failOpen: map[string]error{"/nonexistent": errors.New(secret)}}
	})
	raw, err := proto.Marshal(inv)
	require.NoError(t, err)
	for _, needle := range []string{secret, vmUID, "/etc/shadow", tr.root, "vm-", WritableManifestDir, ReadOnlyManifestDir} {
		assert.NotContains(t, string(raw), needle)
	}
}

var _ = syscall.ENOENT
