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

package vsphere

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ---- parseVSphereImageSource ------------------------------------------------

// TestParseVSphereImageSource_RichOVAURL verifies the rich source.vsphere shape
// is parsed, including ovaURL and checksum fields (only this shape can express
// them).
func TestParseVSphereImageSource_RichOVAURL(t *testing.T) {
	imageJSON := `{
		"source": {
			"vsphere": {
				"ovaURL": "https://images.example.com/photon.ova",
				"checksum": "abc123",
				"checksumType": "sha512"
			}
		}
	}`

	src := parseVSphereImageSource(imageJSON)

	assert.Equal(t, "https://images.example.com/photon.ova", src.OVAURL)
	assert.Equal(t, "abc123", src.Checksum)
	assert.Equal(t, "sha512", src.ChecksumType)
	assert.Empty(t, src.TemplateName)
	assert.Nil(t, src.ContentLibrary)
}

// TestParseVSphereImageSource_RichTemplateName verifies the templateName branch
// of the rich shape (verify-only, no import).
func TestParseVSphereImageSource_RichTemplateName(t *testing.T) {
	src := parseVSphereImageSource(`{"source":{"vsphere":{"templateName":"ubuntu-2404-tmpl"}}}`)

	assert.Equal(t, "ubuntu-2404-tmpl", src.TemplateName)
	assert.Empty(t, src.OVAURL)
	assert.Nil(t, src.ContentLibrary)
}

// TestParseVSphereImageSource_RichContentLibrary verifies the contentLibrary
// branch parses library/item/version.
func TestParseVSphereImageSource_RichContentLibrary(t *testing.T) {
	imageJSON := `{
		"source":{"vsphere":{"contentLibrary":{"library":"prod-cl","item":"jammy","version":"3"}}}
	}`

	src := parseVSphereImageSource(imageJSON)

	require.NotNil(t, src.ContentLibrary)
	assert.Equal(t, "prod-cl", src.ContentLibrary.Library)
	assert.Equal(t, "jammy", src.ContentLibrary.Item)
	assert.Equal(t, "3", src.ContentLibrary.Version)
	assert.Empty(t, src.OVAURL)
	assert.Empty(t, src.TemplateName)
}

// TestParseVSphereImageSource_FlatTemplateName verifies the fallback flat
// contracts.VMImage shape (Go field name TemplateName) is parsed when no nested
// source.vsphere is present. This is the shape Create already round-trips.
func TestParseVSphereImageSource_FlatTemplateName(t *testing.T) {
	src := parseVSphereImageSource(`{"TemplateName":"legacy-template"}`)

	assert.Equal(t, "legacy-template", src.TemplateName)
	assert.Empty(t, src.OVAURL)
	assert.Nil(t, src.ContentLibrary)
}

// TestParseVSphereImageSource_RichWins verifies the rich source.vsphere shape
// wins over flat fields when both are present.
func TestParseVSphereImageSource_RichWins(t *testing.T) {
	imageJSON := `{
		"source":{"vsphere":{"ovaURL":"https://rich/x.ova"}},
		"TemplateName":"flat-template"
	}`

	src := parseVSphereImageSource(imageJSON)

	assert.Equal(t, "https://rich/x.ova", src.OVAURL)
	assert.Empty(t, src.TemplateName, "rich source.vsphere should win over the flat TemplateName field")
}

// TestParseVSphereImageSource_Empty verifies empty / source-less / unparseable
// inputs yield an empty source (the caller turns this into an InvalidSpec error).
func TestParseVSphereImageSource_Empty(t *testing.T) {
	for _, in := range []string{
		"", "   ", "{}", `{"source":{}}`, `{"source":{"vsphere":{}}}`, `{not json`,
	} {
		src := parseVSphereImageSource(in)
		assert.Empty(t, src.OVAURL, "input %q", in)
		assert.Empty(t, src.TemplateName, "input %q", in)
		assert.Nil(t, src.ContentLibrary, "input %q", in)
	}
}

// ---- vsphereChecksumHasher --------------------------------------------------

// TestVSphereChecksumHasher verifies algorithm selection, the sha256 default,
// and rejection of an unknown algorithm.
func TestVSphereChecksumHasher(t *testing.T) {
	tests := []struct {
		in     string
		wantOK bool
	}{
		{"", true},
		{"sha256", true},
		{"SHA256", true},
		{"md5", true},
		{"sha1", true},
		{"sha512", true},
		{"  sha512  ", true},
		{"crc32", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			h, ok := vsphereChecksumHasher(tt.in)
			assert.NotNil(t, h)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}

// ---- verifyFileChecksum -----------------------------------------------------

// TestVerifyFileChecksum covers the no-op (empty checksum), match, mismatch, and
// unknown-algorithm cases against a real temp file.
func TestVerifyFileChecksum(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "checksum-*.bin")
	require.NoError(t, err)
	content := []byte("virtrigaud-ova-bytes")
	_, err = f.Write(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])

	// Empty checksum disables verification.
	assert.NoError(t, verifyFileChecksum(f.Name(), "", ""))

	// Correct checksum (default sha256, and explicit, and uppercase tolerant).
	assert.NoError(t, verifyFileChecksum(f.Name(), hexSum, ""))
	assert.NoError(t, verifyFileChecksum(f.Name(), hexSum, "sha256"))
	assert.NoError(t, verifyFileChecksum(f.Name(), hexSum, "SHA256"))

	// Mismatch is an InvalidSpec error.
	err = verifyFileChecksum(f.Name(), "deadbeef", "sha256")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")

	// Unknown algorithm is rejected before hashing.
	err = verifyFileChecksum(f.Name(), hexSum, "crc32")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported checksum type")
}

// ---- ImagePrepare request validation (host-independent) ---------------------

// TestImagePrepare_NilClient verifies the guard fires before any vCenter
// interaction when the govmomi client is not initialized.
func TestImagePrepare_NilClient(t *testing.T) {
	p := &Provider{logger: slog.Default()}
	resp, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"vsphere":{"ovaURL":"https://x/y.ova"}}}`,
		TargetName: "tmpl",
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "not initialized")
}

// ---- vcsim integration: OVA import + idempotency ----------------------------

// minimalDisklessOVF is a self-contained OVF descriptor with a VirtualSystem but
// no disk references. CreateImportSpec therefore returns no FileItem, so the
// import completes the NFC lease without a disk upload — exercising the full
// import + MarkAsTemplate path against vcsim without needing a real VMDK.
const minimalDisklessOVF = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData"
          xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData">
  <References/>
  <VirtualSystem ovf:id="vm">
    <Info>A minimal virtual machine</Info>
    <Name>minimal-vm</Name>
    <OperatingSystemSection ovf:id="36">
      <Info>The kind of installed guest operating system</Info>
    </OperatingSystemSection>
    <VirtualHardwareSection>
      <Info>Virtual hardware requirements</Info>
      <System>
        <vssd:ElementName>Virtual Hardware Family</vssd:ElementName>
        <vssd:InstanceID>0</vssd:InstanceID>
        <vssd:VirtualSystemIdentifier>minimal-vm</vssd:VirtualSystemIdentifier>
        <vssd:VirtualSystemType>vmx-09</vssd:VirtualSystemType>
      </System>
      <Item>
        <rasd:AllocationUnits>hertz * 10^6</rasd:AllocationUnits>
        <rasd:Description>Number of Virtual CPUs</rasd:Description>
        <rasd:ElementName>1 virtual CPU(s)</rasd:ElementName>
        <rasd:InstanceID>1</rasd:InstanceID>
        <rasd:ResourceType>3</rasd:ResourceType>
        <rasd:VirtualQuantity>1</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits>
        <rasd:Description>Memory Size</rasd:Description>
        <rasd:ElementName>32MB of memory</rasd:ElementName>
        <rasd:InstanceID>2</rasd:InstanceID>
        <rasd:ResourceType>4</rasd:ResourceType>
        <rasd:VirtualQuantity>32</rasd:VirtualQuantity>
      </Item>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>`

// newOVATarServer serves a single-entry OVA tar (the descriptor only) over HTTP
// and returns its URL plus its sha256. The path ends in .ova so ImagePrepare
// uses a TapeArchive.
func newOVATarServer(t *testing.T) (ovaURL, sha256hex string, close func()) {
	t.Helper()

	// Build the OVA tar in memory: a single descriptor.ovf member.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name: "descriptor.ovf",
		Mode: 0o644,
		Size: int64(len(minimalDisklessOVF)),
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte(minimalDisklessOVF))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	ova := buf.Bytes()
	sum := sha256.Sum256(ova)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(ova)
	}))

	return srv.URL + "/image.ova", hex.EncodeToString(sum[:]), srv.Close
}

// newImageTestProvider builds a Provider backed by the in-memory vCenter
// simulator with the placement defaults vcsim's VPX model provides.
func newImageTestProvider(t *testing.T) (*Provider, func()) {
	t.Helper()
	cfg, cleanupSim := newSimConfig(t)
	// VPX model default inventory names.
	cfg.DefaultCluster = "DC0_C0"
	cfg.DefaultDatastore = "LocalDS_0"
	cfg.DefaultFolder = ""

	client, finder, err := createVSphereClient(cfg)
	require.NoError(t, err)

	// Pin the finder to the default datacenter so direct finder calls in tests (and
	// the idempotency gate) resolve; ImagePrepare also does this internally.
	dc, err := finder.DefaultDatacenter(context.Background())
	require.NoError(t, err)
	finder.SetDatacenter(dc)

	p := &Provider{
		client: client,
		finder: finder,
		config: cfg,
		logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	cleanup := func() {
		_ = client.Logout(context.Background())
		cleanupSim()
	}
	return p, cleanup
}

// TestImagePrepare_OVAImport_AndIdempotency imports a trivial OVA into vcsim,
// asserts a template named target_name results, and that a re-run is a no-op
// (does not re-import).
func TestImagePrepare_OVAImport_AndIdempotency(t *testing.T) {
	p, cleanup := newImageTestProvider(t)
	defer cleanup()

	ovaURL, ovaSum, closeSrv := newOVATarServer(t)
	defer closeSrv()

	const targetName = "virtrigaud-it-template"

	imageJSON := `{"source":{"vsphere":{"ovaURL":"` + ovaURL + `","checksum":"` + ovaSum + `","checksumType":"sha256"}}}`

	// First run: imports and marks as template.
	resp, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		ImageJson:  imageJSON,
		TargetName: targetName,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Nil(t, resp.GetTask(), "synchronous import returns an empty task")
	assert.Equal(t, targetName, resp.GetPreparedImageId(),
		"prepared_image_id is the imported template's name (#214)")
	assert.Empty(t, resp.GetPreparedImagePath(), "vSphere addresses templates by name, not path")

	// The imported object exists and is a template.
	vm, err := p.finder.VirtualMachine(context.Background(), targetName)
	require.NoError(t, err, "expected a VM/template named %q after import", targetName)
	require.NotNil(t, vm)
	isTemplate, err := vm.IsTemplate(context.Background())
	require.NoError(t, err)
	assert.True(t, isTemplate, "imported VM should be marked as a template")

	// Second run: idempotent no-op. Point at a URL that would fail if downloaded,
	// to prove the idempotency gate short-circuits BEFORE any import work.
	resp2, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"vsphere":{"ovaURL":"http://127.0.0.1:1/never.ova"}}}`,
		TargetName: targetName,
	})
	require.NoError(t, err, "re-run must be a no-op, not re-import")
	require.NotNil(t, resp2)
	assert.Equal(t, targetName, resp2.GetPreparedImageId(),
		"idempotent re-run still reports the prepared template id (#214)")
}

// TestImagePrepare_TemplateName_VerifyOnly_NotFound verifies the templateName
// branch returns NotFound when the named template does not exist (honest, no
// fabricated success).
func TestImagePrepare_TemplateName_VerifyOnly_NotFound(t *testing.T) {
	p, cleanup := newImageTestProvider(t)
	defer cleanup()

	resp, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"vsphere":{"templateName":"does-not-exist-tmpl"}}}`,
		TargetName: "prepared-name",
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "not found")
}

// TestImagePrepare_TemplateName_VerifyOnly_Exists verifies the templateName
// branch succeeds without importing when the named template already exists. It
// uses an existing vcsim VM (DC0_H0_VM0) as the "template".
func TestImagePrepare_TemplateName_VerifyOnly_Exists(t *testing.T) {
	p, cleanup := newImageTestProvider(t)
	defer cleanup()

	// Confirm the simulator VM exists, then ask ImagePrepare to verify it.
	const existing = "DC0_H0_VM0"
	_, err := p.finder.VirtualMachine(context.Background(), existing)
	require.NoError(t, err)

	resp, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"vsphere":{"templateName":"` + existing + `"}}}`,
		TargetName: "prepared-from-existing",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	// verify-only resolves the prepared image to the verified template name.
	assert.Equal(t, existing, resp.GetPreparedImageId(),
		"verify-only reports the existing template as the prepared id (#214)")
}

// TestImagePrepare_NoSource verifies an image spec with no usable vSphere source
// is rejected as InvalidSpec (no fabricated success).
func TestImagePrepare_NoSource(t *testing.T) {
	p, cleanup := newImageTestProvider(t)
	defer cleanup()

	resp, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"vsphere":{}}}`,
		TargetName: "tmpl",
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "templateName, contentLibrary, or ovaURL")
}

// TestImagePrepare_MissingTargetName verifies the target-name guard fires before
// any vCenter interaction.
func TestImagePrepare_MissingTargetName(t *testing.T) {
	p, cleanup := newImageTestProvider(t)
	defer cleanup()

	resp, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
		ImageJson:  `{"source":{"vsphere":{"ovaURL":"https://x/y.ova"}}}`,
		TargetName: "  ",
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "target name is required")
}

// TestFindOVADescriptorName_SkipsAppleDouble verifies the OVA descriptor resolver
// skips macOS AppleDouble sidecars (._*) and __MACOSX entries that precede the
// real descriptor — the failure mode hit by a macOS-packaged OVA during live
// validation (govmomi's "*.ovf" glob matched the binary "._foo.ovf" first).
func TestFindOVADescriptorName_SkipsAppleDouble(t *testing.T) {
	dir := t.TempDir()
	ovaPath := filepath.Join(dir, "test.ova")
	f, err := os.Create(ovaPath)
	require.NoError(t, err)
	tw := tar.NewWriter(f)
	// Order mirrors a real macOS-packaged OVA: AppleDouble sidecars first.
	entries := []struct {
		name string
		data string
	}{
		{"._ubuntu-24-04-ovf.ovf", "\x00\x05\x16\x07binary applesingle blob"},
		{"__MACOSX/._x.ovf", "\x00\x00binary"},
		{"ubuntu-24-04-ovf.ovf", "<?xml version='1.0'?><Envelope/>"},
		{"._ubuntu-24-04-ovf-1.vmdk", "\x00binary"},
		{"ubuntu-24-04-ovf-1.vmdk", "diskbytes"},
	}
	for _, e := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data))}))
		_, err := tw.Write([]byte(e.data))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, f.Close())

	name, err := findOVADescriptorName(ovaPath)
	require.NoError(t, err)
	assert.Equal(t, "ubuntu-24-04-ovf.ovf", name,
		"must skip ._*.ovf AppleDouble sidecars and pick the real descriptor")
}

// TestFindOVADescriptorName_NoDescriptor verifies an OVA with no real .ovf (only
// sidecars) is a clear error rather than a binary-parse failure downstream.
func TestFindOVADescriptorName_NoDescriptor(t *testing.T) {
	dir := t.TempDir()
	ovaPath := filepath.Join(dir, "nodesc.ova")
	f, err := os.Create(ovaPath)
	require.NoError(t, err)
	tw := tar.NewWriter(f)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "._only.ovf", Mode: 0o644, Size: 3}))
	_, _ = tw.Write([]byte("\x00ab"))
	require.NoError(t, tw.Close())
	require.NoError(t, f.Close())

	_, err = findOVADescriptorName(ovaPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no .ovf descriptor")
}
