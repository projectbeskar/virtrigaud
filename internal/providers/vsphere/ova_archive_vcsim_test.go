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

package vsphere

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"google.golang.org/grpc/codes"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// vcsim regression tests: an OVF can never make the provider open, and upload,
// a file on its own filesystem (the security review's PoC, turned around).

// isoOVFTemplate is a one-VM OVF whose CD-ROM is backed by the file reference
// %s of size %d: vCenter asks the importer to upload that file as an ISO.
const isoOVFTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData"
          xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData">
  <References>
    <File ovf:id="file1" ovf:href="%s" ovf:size="%d"/>
  </References>
  <VirtualSystem ovf:id="vm">
    <Info>iso</Info>
    <Name>iso-vm</Name>
    <OperatingSystemSection ovf:id="36"><Info>x</Info></OperatingSystemSection>
    <VirtualHardwareSection>
      <Info>hw</Info>
      <System>
        <vssd:ElementName>Virtual Hardware Family</vssd:ElementName>
        <vssd:InstanceID>0</vssd:InstanceID>
        <vssd:VirtualSystemIdentifier>iso-vm</vssd:VirtualSystemIdentifier>
        <vssd:VirtualSystemType>vmx-09</vssd:VirtualSystemType>
      </System>
      <Item>
        <rasd:ElementName>1 virtual CPU(s)</rasd:ElementName>
        <rasd:InstanceID>1</rasd:InstanceID>
        <rasd:ResourceType>3</rasd:ResourceType>
        <rasd:VirtualQuantity>1</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits>
        <rasd:ElementName>32MB</rasd:ElementName>
        <rasd:InstanceID>2</rasd:InstanceID>
        <rasd:ResourceType>4</rasd:ResourceType>
        <rasd:VirtualQuantity>32</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:Address>0</rasd:Address>
        <rasd:ElementName>IDE 0</rasd:ElementName>
        <rasd:InstanceID>3</rasd:InstanceID>
        <rasd:ResourceType>5</rasd:ResourceType>
      </Item>
      <Item ovf:required="false">
        <rasd:AddressOnParent>0</rasd:AddressOnParent>
        <rasd:AutomaticAllocation>true</rasd:AutomaticAllocation>
        <rasd:ElementName>CD/DVD drive 1</rasd:ElementName>
        <rasd:HostResource>ovf:/file/file1</rasd:HostResource>
        <rasd:InstanceID>4</rasd:InstanceID>
        <rasd:Parent>3</rasd:Parent>
        <rasd:ResourceType>15</rasd:ResourceType>
      </Item>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>`

// importCalls counts the OVF import calls that reach vCenter.
type importCalls struct {
	soap.RoundTripper
	createImportSpec atomic.Int32
	importVApp       atomic.Int32
}

func (r *importCalls) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	switch req.(type) {
	case *methods.CreateImportSpecBody:
		r.createImportSpec.Add(1)
	case *methods.ImportVAppBody:
		r.importVApp.Add(1)
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// nfcUploads records every body the importer PUTs or POSTs to an NFC lease
// URL (the disk and ISO uploads), below the SOAP layer.
type nfcUploads struct {
	rt     http.RoundTripper
	mu     sync.Mutex
	bodies [][]byte
}

func (u *nfcUploads) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/nfc/") && (r.Method == http.MethodPut || r.Method == http.MethodPost) && r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(b))
		u.mu.Lock()
		u.bodies = append(u.bodies, b)
		u.mu.Unlock()
	}
	return u.rt.RoundTrip(r)
}

func (u *nfcUploads) all() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

// instrument wraps p's vCenter client to count import calls and record NFC
// uploads.
func instrument(p *Provider) (*importCalls, *nfcUploads) {
	calls := &importCalls{RoundTripper: p.client.RoundTripper}
	p.client.RoundTripper = calls
	sc := p.client.Client.Client
	rt := sc.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	uploads := &nfcUploads{rt: rt}
	sc.Transport = uploads
	return calls, uploads
}

// countingServer serves body at a URL ending in suffix and counts requests.
func countingServer(t *testing.T, suffix string, body []byte) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/image" + suffix, &hits
}

// sentinelFile writes a secret-looking file the provider pod could read.
func sentinelFile(t *testing.T) (path string, content []byte) {
	t.Helper()
	content = []byte("SENTINEL-vcenter-password-canary\n")
	path = filepath.Join(t.TempDir(), "password")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path, content
}

func requireNoUploadOf(t *testing.T, uploads *nfcUploads, content []byte) {
	t.Helper()
	for _, b := range uploads.all() {
		require.False(t, bytes.Contains(b, content), "the provider pod's local file reached vCenter")
	}
}

func TestOVFFileReferenceAttacksAreRefused(t *testing.T) {
	secretPath, secret := sentinelFile(t)
	secretName := filepath.Base(secretPath)
	hrefs := map[string]string{
		"absolute path":          secretPath,
		"traversal":              "../../../../../../../../.." + secretPath,
		"relative traversal":     "../" + secretName,
		"URL":                    "http://127.0.0.1:1/" + secretName,
		"file URL":               "file://" + secretPath,
		"glob":                   "*",
		"glob matching a member": "*.ovf",
		"backslash":              `..\` + secretName,
	}
	for name, href := range hrefs {
		desc := fmt.Sprintf(isoOVFTemplate, href, len(secret))
		// The package even carries a member named like the secret: only the
		// reference's syntax decides.
		ova := tarOVAMembers(t, [][2]string{{"descriptor.ovf", desc}, {secretName, "not-the-secret"}})

		t.Run("identity: "+name, func(t *testing.T) {
			p, _, _ := newIdentitySim(t, "")
			calls, uploads := instrument(p)
			_, err := p.ImagePrepare(context.Background(),
				identityReq(t, serveBody(t, http.StatusOK, ova), testImageUID, testDigestA))
			requireCode(t, err, codes.InvalidArgument)
			assert.NotContains(t, err.Error(), secretPath)
			assert.Zero(t, calls.createImportSpec.Load(), "the descriptor never reaches vCenter")
			assert.Zero(t, calls.importVApp.Load())
			requireNoUploadOf(t, uploads, secret)
			requireNoVMNamed(t, p, artifactNameFor(t, testImageUID, testDigestA))
		})

		t.Run("legacy: "+name, func(t *testing.T) {
			p, _, _ := newIdentitySim(t, "")
			calls, uploads := instrument(p)
			_, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
				ImageJson:  ovaImageJSON(t, serveBody(t, http.StatusOK, ova), nil),
				TargetName: "legacy-img",
			})
			requireCode(t, err, codes.InvalidArgument)
			assert.Zero(t, calls.createImportSpec.Load())
			assert.Zero(t, calls.importVApp.Load())
			requireNoUploadOf(t, uploads, secret)
			requireNoVMNamed(t, p, "legacy-img")
		})
	}
}

func TestBareOVF(t *testing.T) {
	secretPath, secret := sentinelFile(t)

	t.Run("identity mode refuses a bare .ovf before downloading it", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		calls, _ := instrument(p)
		u, hits := countingServer(t, ".ovf", []byte(minimalDisklessOVF))
		_, err := p.ImagePrepare(context.Background(), identityReq(t, u, testImageUID, testDigestA))
		requireCode(t, err, codes.InvalidArgument)
		assert.Contains(t, err.Error(), "publish the image as an .ova")
		assert.Zero(t, hits.Load())
		assert.Zero(t, calls.createImportSpec.Load())

		// A query string does not hide the extension.
		_, err = p.ImagePrepare(context.Background(), identityReq(t, u+"?sig=abc", testImageUID, testDigestA))
		requireCode(t, err, codes.InvalidArgument)
		assert.Zero(t, hits.Load())
	})

	for name, href := range map[string]string{
		"a sibling file":   "disk1.vmdk",
		"an absolute path": secretPath,
		"a traversal":      "../../../../../../.." + secretPath,
	} {
		t.Run("legacy mode refuses a bare .ovf that references "+name, func(t *testing.T) {
			p, _, _ := newIdentitySim(t, "")
			calls, uploads := instrument(p)
			u, _ := countingServer(t, ".ovf", []byte(fmt.Sprintf(isoOVFTemplate, href, len(secret))))
			_, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
				ImageJson: ovaImageJSON(t, u, nil), TargetName: "legacy-img",
			})
			requireCode(t, err, codes.InvalidArgument)
			assert.Zero(t, calls.createImportSpec.Load())
			requireNoUploadOf(t, uploads, secret)
			requireNoVMNamed(t, p, "legacy-img")
		})
	}

	t.Run("legacy mode still imports a bare .ovf that references no file", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		u, _ := countingServer(t, ".ovf", []byte(minimalDisklessOVF))
		resp, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
			ImageJson: ovaImageJSON(t, u, nil), TargetName: "legacy-img",
		})
		require.NoError(t, err)
		assert.Equal(t, "legacy-img", resp.GetPreparedImageId())
		assert.Len(t, objectsNamed(t, p, defaultVMFolder(t, p), "legacy-img"), 1)
	})
}

// TestOversizedDescriptorIsRefusedBeforeItIsRead (review R1): a descriptor
// above maxOVFDescriptorBytes — the reviewer's was 128 MiB of whitespace in an
// XML comment, peaking near 2 GiB of heap — never reaches govmomi's ReadAll or
// vCenter, in either mode; a bare .ovf is not even downloaded past the cap.
func TestOversizedDescriptorIsRefusedBeforeItIsRead(t *testing.T) {
	huge := "<Envelope><!--" + strings.Repeat(" ", maxOVFDescriptorBytes) + "--></Envelope>"

	t.Run("OVA, identity mode", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		calls, _ := instrument(p)
		ova := tarOVAMembers(t, [][2]string{{"descriptor.ovf", huge}})
		_, err := p.ImagePrepare(context.Background(), identityReq(t, serveBody(t, http.StatusOK, ova), testImageUID, testDigestA))
		requireCode(t, err, codes.InvalidArgument)
		assert.Contains(t, err.Error(), "descriptor is larger than 16 MiB")
		assert.Zero(t, calls.createImportSpec.Load())
	})

	t.Run("bare .ovf, legacy mode", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		calls, _ := instrument(p)
		u, _ := countingServer(t, ".ovf", []byte(huge))
		_, err := p.ImagePrepare(context.Background(), &providerv1.ImagePrepareRequest{
			ImageJson: ovaImageJSON(t, u, nil), TargetName: "legacy-img",
		})
		requireCode(t, err, codes.InvalidArgument)
		assert.Contains(t, err.Error(), "descriptor is larger than 16 MiB")
		assert.Zero(t, calls.createImportSpec.Load())
	})
}

func TestOVAWithMemberFilesStillImports(t *testing.T) {
	const iso = "ISO-CONTENT-from-the-package"
	desc := fmt.Sprintf(isoOVFTemplate, "seed.iso", len(iso))

	for name, members := range map[string][][2]string{
		"root members":      {{"descriptor.ovf", desc}, {"seed.iso", iso}},
		"./-prefixed names": {{"./descriptor.ovf", desc}, {"./seed.iso", iso}},
		"macOS sidecars":    {{"._descriptor.ovf", "\x00\x05\x16\x07"}, {"descriptor.ovf", desc}, {"._seed.iso", "\x00x"}, {"seed.iso", iso}},
	} {
		t.Run(name, func(t *testing.T) {
			p, _, _ := newIdentitySim(t, "")
			_, uploads := instrument(p)
			resp, err := p.ImagePrepare(context.Background(),
				identityReq(t, serveBody(t, http.StatusOK, tarOVAMembers(t, members)), testImageUID, testDigestA))
			require.NoError(t, err)
			assert.False(t, resp.GetArtifact().GetReused())
			got := uploads.all()
			require.Len(t, got, 1, "the ISO member is uploaded")
			assert.Equal(t, iso, string(got[0]), "exactly the package member named by the reference")
		})
	}

	t.Run("a reference to a file the OVA does not contain is refused", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		calls, _ := instrument(p)
		ova := tarOVAMembers(t, [][2]string{{"descriptor.ovf", desc}, {"other.iso", iso}})
		_, err := p.ImagePrepare(context.Background(),
			identityReq(t, serveBody(t, http.StatusOK, ova), testImageUID, testDigestA))
		requireCode(t, err, codes.InvalidArgument)
		assert.Zero(t, calls.createImportSpec.Load())
	})
}

// tarOVAMembers packs members (name, content), in order, as an OVA.
func tarOVAMembers(t *testing.T, members [][2]string) []byte {
	t.Helper()
	b, err := os.ReadFile(writeTar(t, members))
	require.NoError(t, err)
	return b
}
