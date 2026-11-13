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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func extByName(inv *providerv1.HostInventory) map[string]*providerv1.HostExtension {
	m := map[string]*providerv1.HostExtension{}
	for _, e := range inv.GetUnexpectedExtensions() {
		m[e.GetName()] = e
	}
	return m
}

func TestExtensionEntryClassification(t *testing.T) {
	tr := healthyTree(t)
	tr.mkdir("/var/lib/extensions")
	// Masked by a link to /dev/null: not merged, not listed.
	require.NoError(t, os.Symlink("/dev/null", tr.path("/var/lib/extensions/masked.raw")))
	// Dangling and non-regular targets: listed per entry, without a digest.
	require.NoError(t, os.Symlink("/nonexistent/x.raw", tr.path("/var/lib/extensions/dangling.raw")))
	tr.mkdir("/var/lib/dir-target")
	require.NoError(t, os.Symlink("/var/lib/dir-target", tr.path("/var/lib/extensions/dirlink.raw")))
	// A link to a real image is hashed.
	tr.write("/var/lib/real.bin", []byte("real image"))
	require.NoError(t, os.Symlink(tr.path("/var/lib/real.bin"), tr.path("/var/lib/extensions/linked.raw")))
	// A FIFO, a versioned directory and a plain file are listed, never opened.
	require.NoError(t, syscall.Mkfifo(tr.path("/var/lib/extensions/pipe.raw"), 0o600))
	tr.mkdir("/var/lib/extensions/versioned.raw.v")
	tr.write("/var/lib/extensions/readme", []byte("x"))
	// "x" and "x.raw" are one extension; the earlier directory wins.
	tr.write("/etc/extensions/dup.raw", []byte("etc"))
	tr.mkdir("/run/extensions/dup")
	inv := collectInventory(t, tr, nil)
	got := extByName(inv)
	assert.NotContains(t, got, "masked.raw")
	for _, name := range []string{"dangling.raw", "dirlink.raw", "pipe.raw", "versioned.raw.v", "readme"} {
		require.Contains(t, got, name)
		assert.Equal(t, providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY, got[name].GetKind(), name)
		assert.Empty(t, got[name].GetImageSha256(), name)
	}
	require.Contains(t, got, "linked.raw")
	assert.Equal(t, rawDigest([]byte("real image")), got["linked.raw"].GetImageSha256())
	assert.Contains(t, got, "dup.raw")
	assert.NotContains(t, got, "dup")
	assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_COMPLETE, inv.GetState())
}

func TestDeclaredExtensionMatchesWithoutSuffix(t *testing.T) {
	tr := sysextTree(t, "")
	tr.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", strings.Replace(
		manifestJSON("cloud-hypervisor", "1.5.0", `"artifact":{"name":"cloud-hypervisor"}`), `"package"`, `"sysext"`, 1))
	inv := collectInventory(t, tr, nil)
	c := component(t, inv, "cloud-hypervisor")
	assert.Equal(t, stReady, c.GetState())
	assert.Equal(t, rawDigest(imageBytes), c.GetArtifactSha256())
	assert.Empty(t, inv.GetUnexpectedExtensions())
}

func TestDisplayNameCollisionIsAnError(t *testing.T) {
	tr := healthyTree(t)
	name := "odd name$.raw"
	tr.write("/var/lib/extensions/"+name, []byte("odd"))
	hashed := invalidExtensionPrefix + fmt.Sprintf("%x", rawDigest([]byte(name))[:8])
	tr.write("/var/lib/extensions/"+hashed, []byte("literal"))
	inv := collectInventory(t, tr, nil)
	assert.Equal(t, providerv1.InventoryState_INVENTORY_STATE_UNREADABLE, inv.GetState())
	assert.Equal(t, ReasonProbeFailed, inv.GetStateReason())
}

func TestOverlayOnlyExcusedForTheDeclaringComponent(t *testing.T) {
	// A directory extension declared by the agent component that holds the
	// hypervisor's files is still an overlay of the hypervisor.
	tr := healthyTree(t)
	tr.write("/run/extensions/shared/usr/bin/cloud-hypervisor", []byte("x"))
	tr.manifest(ReadOnlyManifestDir, "agent.json", strings.Replace(manifestJSON("virtrigaud-agent", "0.1.0", `"artifact":{"name":"shared"}`), `"package"`, `"sysext"`, 1))
	inv := collectInventory(t, tr, nil)
	ch := component(t, inv, "cloud-hypervisor")
	assert.Equal(t, stUnhealthy, ch.GetState())
	assert.Equal(t, ReasonUnexpectedExtension, ch.GetStateReason())
	assert.Equal(t, stReady, component(t, inv, "virtrigaud-agent").GetState())
	// Declared by the hypervisor itself: not an overlay of it.
	tr2 := healthyTree(t)
	tr2.write("/run/extensions/mine/usr/bin/cloud-hypervisor", []byte("x"))
	tr2.manifest(ReadOnlyManifestDir, "cloud-hypervisor.json", strings.Replace(manifestJSON("cloud-hypervisor", "1.2.3", `"artifact":{"name":"mine"}`), `"package"`, `"sysext"`, 1))
	assert.Equal(t, stReady, component(t, collectInventory(t, tr2, nil), "cloud-hypervisor").GetState())
}

func TestOverlayLstatErrorCountsAsOverlay(t *testing.T) {
	tr := healthyTree(t)
	tr.mkdir("/run/extensions/odd")
	inv := collectInventory(t, tr, func(o *Options) {
		o.FS = lstatFailFS{FS: o.FS, prefix: "/run/extensions/odd", err: fs.ErrPermission}
	})
	assert.Equal(t, ReasonUnexpectedExtension, component(t, inv, "cloud-hypervisor").GetStateReason())
}

type lstatFailFS struct {
	FS
	prefix string
	err    error
}

func (l lstatFailFS) Lstat(p string) (fs.FileMode, int64, error) {
	if strings.HasPrefix(p, l.prefix+"/") {
		return 0, 0, l.err
	}
	return l.FS.Lstat(p)
}

func TestUnparseableVersionIsUnreadable(t *testing.T) {
	tr := healthyTree(t)
	tr.write("/var/lib/dpkg/status", []byte("Package: cloud-hypervisor\nStatus: install ok installed\nVersion: dev\n\n"))
	c := component(t, collectInventory(t, tr, nil), "cloud-hypervisor")
	assert.Equal(t, stIncompatible, c.GetState())
	assert.Equal(t, ReasonVersionUnreadable, c.GetStateReason())
	assert.Equal(t, ">=1.0.0", c.GetRequiredRange())
}

func TestBootProbes(t *testing.T) {
	feat := func(inv *providerv1.HostInventory, name string) *providerv1.HostFeature {
		for _, f := range inv.GetFeatures() {
			if f.GetName() == name {
				return f
			}
		}
		t.Fatalf("no feature %s", name)
		return nil
	}
	absentR, errR, presentR := providerv1.FeatureResult_FEATURE_RESULT_ABSENT, providerv1.FeatureResult_FEATURE_RESULT_ERROR, providerv1.FeatureResult_FEATURE_RESULT_PRESENT
	t.Run("no EFI firmware: absent", func(t *testing.T) {
		assert.Equal(t, absentR, feat(collectInventory(t, healthyTree(t), nil), FeatureSecureBoot).GetResult())
	})
	t.Run("EFI with efivarfs and no variable: absent", func(t *testing.T) {
		tr := healthyTree(t)
		tr.mkdir(efivarsDir)
		assert.Equal(t, absentR, feat(collectInventory(t, tr, nil), FeatureSecureBoot).GetResult())
	})
	t.Run("EFI without efivarfs mounted: cannot tell", func(t *testing.T) {
		tr := healthyTree(t)
		tr.mkdir(efivarsDir)
		h := healthyHost()
		h.magic[efivarsDir] = 0x1234
		f := feat(collectInventory(t, tr, func(o *Options) { o.Host = h }), FeatureSecureBoot)
		assert.Equal(t, errR, f.GetResult())
		assert.Equal(t, FeatureReasonProbeFailed, f.GetReason())
	})
	t.Run("secure boot variable on", func(t *testing.T) {
		tr := healthyTree(t)
		tr.write(secureBootVariable, []byte{6, 0, 0, 0, 1})
		assert.Equal(t, presentR, feat(collectInventory(t, tr, nil), FeatureSecureBoot).GetResult())
	})
	t.Run("measured boot needs securityfs", func(t *testing.T) {
		tr := healthyTree(t)
		tr.write(measuredBootLog, []byte("log"))
		h := healthyHost()
		h.magic[securityfsDir] = 0x1
		assert.Equal(t, errR, feat(collectInventory(t, tr, func(o *Options) { o.Host = h }), FeatureMeasuredBoot).GetResult())
		assert.Equal(t, presentR, feat(collectInventory(t, tr, nil), FeatureMeasuredBoot).GetResult())
		assert.Equal(t, absentR, feat(collectInventory(t, healthyTree(t), nil), FeatureMeasuredBoot).GetResult())
	})
	t.Run("kvm and tun", func(t *testing.T) {
		h := healthyHost()
		h.kvm, h.tunErr = false, fmt.Errorf("x: %w", fs.ErrPermission)
		inv := collectInventory(t, healthyTree(t), func(o *Options) { o.Host = h })
		assert.Equal(t, absentR, feat(inv, FeatureKVM).GetResult())
		assert.Equal(t, errR, feat(inv, FeatureTun).GetResult())
		assert.Equal(t, FeatureReasonPermissionDenied, feat(inv, FeatureTun).GetReason())
		h = healthyHost()
		assert.Equal(t, presentR, feat(collectInventory(t, healthyTree(t), func(o *Options) { o.Host = h }), FeatureKVM).GetResult())
	})
	t.Run("the tier follows the pidfs probes", func(t *testing.T) {
		h := healthyHost()
		h.handle, h.onPidfs = false, true
		assert.Equal(t, providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_INODE, collectInventory(t, healthyTree(t), func(o *Options) { o.Host = h }).GetProcessIdentityTier())
		h.onPidfs = false
		assert.Equal(t, providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_START_TIME, collectInventory(t, healthyTree(t), func(o *Options) { o.Host = h }).GetProcessIdentityTier())
	})
}

// TestSystemHostSmoke runs the real probes on the machine the test runs on:
// whatever they find, none may panic or report an unlisted reason.
func TestSystemHostSmoke(t *testing.T) {
	c, _ := newCollector(t, healthyTree(t), func(o *Options) { o.Host = SystemHost{} })
	c.CollectNow(context.Background())
	inv := c.Inventory(1)
	require.NoError(t, Validate(inv, 1))
	assert.Len(t, inv.GetFeatures(), len(valueSets[1].features))
}

func TestStrictManifestJSON(t *testing.T) {
	cat := map[string]*CatalogEntry{}
	for _, e := range DefaultCatalog() {
		cat[e.Name] = &e
	}
	ok := manifestJSON("cloud-hypervisor", "1.2.3", "")
	assert.Empty(t, parseManifest([]byte(ok), cat).reason)
	for name, doc := range map[string]string{
		"duplicate key":           `{"schemaVersion":1,"component":"cloud-hypervisor","version":"1","version":"2"}`,
		"duplicate nested key":    `{"schemaVersion":1,"component":"cloud-hypervisor","version":"1","artifact":{"name":"a","name":"b"}}`,
		"duplicate file key":      `{"schemaVersion":1,"component":"cloud-hypervisor","version":"1","files":{"cloud-hypervisor":"sha256:` + strings.Repeat("0", 64) + `","cloud-hypervisor":"sha256:` + strings.Repeat("1", 64) + `"}}`,
		"case-variant key":        `{"schemaversion":1,"component":"cloud-hypervisor","version":"1"}`,
		"upper-case key":          `{"schemaVersion":1,"Component":"cloud-hypervisor","version":"1"}`,
		"case-variant nested key": `{"schemaVersion":1,"component":"cloud-hypervisor","version":"1","artifact":{"Name":"a"}}`,
		"invalid UTF-8":           "{\"schemaVersion\":1,\"component\":\"cloud-hypervisor\",\"version\":\"1\",\"source\":\"\xff\"}",
		"too deep":                `{"schemaVersion":1,"component":"cloud-hypervisor","version":"1","files":{"a":{"b":{"c":{}}}}}`,
		"array at the top":        `[]`,
	} {
		assert.Equal(t, ReasonParseError, parseManifest([]byte(doc), cat).reason, name)
	}
}

// TestLogsCarryNoPathsOrErrorText captures the log sink: whatever fails, a log
// line names a class, never an error's text, a path or a name.
func TestLogsCarryNoPathsOrErrorText(t *testing.T) {
	var buf bytes.Buffer
	log := funcr.New(func(prefix, args string) { buf.WriteString(prefix + " " + args + "\n") }, funcr.Options{Verbosity: 2})
	secret := errors.New("open /etc/very/secret/path: SECRET-TOKEN-1234")
	tr := healthyTree(t)
	tr.mkdir(WritableManifestDir)
	c, _ := newCollector(t, tr, func(o *Options) {
		o.Log = log
		o.FS = failingFS{FS: o.FS, failOpen: map[string]error{ReadOnlyManifestDir + "/agent.json": fmt.Errorf("%w: %w", fs.ErrPermission, secret)}}
	})
	c.CollectNow(context.Background())
	out := buf.String()
	require.Contains(t, out, "not complete")
	assert.Contains(t, out, "permission")
	for _, needle := range []string{"SECRET", "/etc/", "agent.json", tr.root, "secret"} {
		assert.NotContains(t, out, needle)
	}
	assert.Equal(t, "none", ErrorClass(nil))
	assert.Equal(t, "other", ErrorClass(secret))
	assert.True(t, strings.HasPrefix(ErrorClass(syscall.EIO), "errno_"))
	raw, _ := proto.Marshal(c.Inventory(1))
	assert.NotContains(t, string(raw), "SECRET")
}

func TestHashBudgetCountsWhatWasRead(t *testing.T) {
	tr := newTree(t)
	tr.write("/f", bytes.Repeat([]byte{1}, 100))
	h := &hasher{fs: OSFS{Root: tr.root}, perFile: 60, remaining: 1000}
	_, res, err := h.sum(context.Background(), "/f", true)
	require.NoError(t, err)
	assert.Equal(t, hashLimit, res)
	assert.Equal(t, int64(1000), h.remaining, "a file over the limit is refused before it is read")
	h = &hasher{fs: OSFS{Root: tr.root}, perFile: 1000, remaining: 150}
	_, res, _ = h.sum(context.Background(), "/f", true)
	assert.Equal(t, hashOK, res)
	assert.Equal(t, int64(50), h.remaining)
}

func TestOpenNeverOpensANonRegularFile(t *testing.T) {
	tr := newTree(t)
	require.NoError(t, syscall.Mkfifo(tr.path("/pipe"), 0o600))
	_, err := OSFS{Root: tr.root}.Open("/pipe", true)
	assert.ErrorIs(t, err, ErrNotRegular)
	tr.mkdir("/d")
	_, err = OSFS{Root: tr.root}.Open("/d", false)
	assert.ErrorIs(t, err, ErrNotRegular)
}

func TestDefaultAgentExecutableIsTheRunningImage(t *testing.T) {
	assert.Equal(t, "/proc/self/exe", DefaultLayout().AgentExecutable)
	c, err := New(Options{FS: OSFS{}, Host: healthyHost(), Boot: &fakeBoot{}, AllowShortTimings: true})
	require.NoError(t, err)
	assert.Equal(t, "/proc/self/exe", c.o.Layout.AgentExecutable)
	h := &hasher{fs: OSFS{}, perFile: 1 << 30, remaining: 1 << 31}
	sum, res, err := h.sum(context.Background(), "/proc/self/exe", true)
	require.NoError(t, err)
	assert.Equal(t, hashOK, res)
	assert.Len(t, sum, 32)
}
