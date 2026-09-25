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

package proxmox

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// ADR-0009 Slice 5 (D10): the Proxmox guard. A URL image import fails closed
// with InvalidSpec before any PVE call, with or without an image identity; a
// legacy (identity-less) request is additionally signalled as deprecated;
// reference-style template sources are verified exactly as before; every
// request is validated with imageartifact.ParseRequest; and the provider
// advertises supports_image_artifact_identity.

const (
	// guardImageUID is a well-formed VMImage UID.
	guardImageUID = "5f0c6a7e-2d0b-4a8e-9a53-0f1e2d3c4b5a"
	// guardProviderUID is a well-formed Provider UID.
	guardProviderUID = "8c1e2f3a-4b5c-4d6e-8f7a-9b0c1d2e3f4a"
	// guardDigest is a well-formed ADR-0009 D2 source digest.
	guardDigest = "sha256:9b1e0c3f5a7d2e4b6c8a0f1e3d5b7a9c2e4f6a8b0d1c3e5f7a9b2d4e6f8a0c1e"
	// guardLegacyTotal is the ADR-0009 D7 deprecation counter.
	guardLegacyTotal = "virtrigaud_provider_image_prepare_legacy_requests_total"
	// guardSecretURL is an import URL whose query string carries a token: it
	// must never appear in an error or a log line.
	guardSecretURL = "https://images.example.com/jammy.img?X-Amz-Signature=s3cr3t-t0ken"
	// guardSeededTemplate is the template the fake PVE server seeds (VMID 9000).
	guardSeededTemplate = "ubuntu-22-template"
)

// guardLogWriter serializes writes to the log buffer.
type guardLogWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *guardLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *guardLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// newGuardProvider returns a provider backed by a fresh fake PVE server and
// logging to the returned writer.
func newGuardProvider(t *testing.T) (*Provider, *pvefake.Server, *guardLogWriter) {
	t.Helper()
	server, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	p := createTestProvider(endpoint)
	logs := &guardLogWriter{}
	p.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return p, server, logs
}

// identityPrepareRequest returns the identity request a new manager sends for
// VMImage team-a/<name> with imageJSON as its spec: an empty target_name.
func identityPrepareRequest(name, imageJSON string) *providerv1.ImagePrepareRequest {
	return &providerv1.ImagePrepareRequest{
		ImageJson:    imageJSON,
		Image:        &providerv1.ObjectIdentity{Uid: guardImageUID, Namespace: "team-a", Name: name},
		SourceDigest: guardDigest,
		Provider:     &providerv1.ObjectIdentity{Uid: guardProviderUID, Namespace: "team-a", Name: "proxmox"},
	}
}

// legacyPrepareRequest returns the request a manager older than ADR-0009
// sends: no identity, the bare VMImage name as target_name.
func legacyPrepareRequest(targetName, imageJSON string) *providerv1.ImagePrepareRequest {
	return &providerv1.ImagePrepareRequest{ImageJson: imageJSON, TargetName: targetName}
}

// proxmoxLegacyCount returns the legacy counter's value for provider_type
// "proxmox" (0 before its first increment).
func proxmoxLegacyCount(t *testing.T) float64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != guardLegacyTotal {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "provider_type" && l.GetValue() == proxmoxProviderType {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// requireURLImportRefused asserts err is the D10 InvalidSpec, carried as a
// gRPC InvalidArgument status with the fixed, non-secret message.
func requireURLImportRefused(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "expected a gRPC status error, got %T: %v", err, err)
	assert.Equal(t, codes.InvalidArgument, st.Code())
	assert.Equal(t, urlImportUnsupportedMessage, st.Message())
	assert.Contains(t, st.Message(), "ADR-0009 Slice 6", "the refusal points to the follow-up")
	assert.Contains(t, st.Message(), "source.proxmox.templateID", "the refusal says what to do instead")
	assert.NotContains(t, st.Message(), "images.example.com", "the refusal never echoes the URL")
}

// urlImportSources are image specs that request a URL import.
var urlImportSources = map[string]string{
	"source.http.url":                       `{"source":{"http":{"url":"` + guardSecretURL + `"}}}`,
	"source.http with proxmox storage/node": `{"source":{"proxmox":{"storage":"local-lvm","node":"pve","format":"qcow2"},"http":{"url":"` + guardSecretURL + `"}}}`,
	"flat contracts.VMImage URL":            `{"URL":"` + guardSecretURL + `","Format":"qcow2"}`,
}

// TestImagePrepare_URLImportRefusedWithIdentity verifies an identity request
// for a URL import is refused with InvalidSpec before any PVE call: no
// download, no VM, no lookup (ADR-0009 D10). It is not a legacy request, so
// the deprecation counter does not move.
func TestImagePrepare_URLImportRefusedWithIdentity(t *testing.T) {
	for name, imageJSON := range urlImportSources {
		t.Run(name, func(t *testing.T) {
			p, server, logs := newGuardProvider(t)
			before := proxmoxLegacyCount(t)

			resp, err := p.ImagePrepare(context.Background(), identityPrepareRequest("ubuntu", imageJSON))

			assert.Nil(t, resp)
			requireURLImportRefused(t, err)
			assert.Empty(t, server.Requests(), "the guard runs before any PVE call")
			assert.Nil(t, server.LastDownloadRequest(), "no download-url")
			assert.Equal(t, before, proxmoxLegacyCount(t), "an identity request is not legacy")
			assert.NotContains(t, logs.String(), imageartifact.LegacyRequestWarning)
			assert.NotContains(t, logs.String(), "s3cr3t-t0ken", "the URL is never logged")
			assert.Contains(t, logs.String(), "level=WARN", "the refusal is logged")
		})
	}
}

// TestImagePrepare_URLImportRefusedLegacy verifies an identity-less request
// from an older manager gets the same InvalidSpec (Proxmox has no bare-name
// path to keep) and is signalled as legacy: one WARN line and one counter
// increment per request (ADR-0009 D7, D10, Q3).
func TestImagePrepare_URLImportRefusedLegacy(t *testing.T) {
	for name, imageJSON := range urlImportSources {
		t.Run(name, func(t *testing.T) {
			p, server, logs := newGuardProvider(t)
			before := proxmoxLegacyCount(t)

			resp, err := p.ImagePrepare(context.Background(), legacyPrepareRequest("jammy-base", imageJSON))

			assert.Nil(t, resp)
			requireURLImportRefused(t, err)
			assert.Empty(t, server.Requests(), "the guard runs before any PVE call")
			assert.Equal(t, before+1, proxmoxLegacyCount(t), "the legacy request is counted")
			out := logs.String()
			assert.Equal(t, 1, strings.Count(out, imageartifact.LegacyRequestWarning))
			assert.Contains(t, out, "level=WARN msg=\""+imageartifact.LegacyRequestWarning+"\"")
			assert.Contains(t, out, "provider_type=proxmox")
			assert.NotContains(t, out, "s3cr3t-t0ken", "the URL is never logged")
		})
	}
}

// TestImagePrepare_URLImportNeverReusesByName verifies the pre-ADR bare-name
// gate is gone: a URL import whose target (legacy) or image name (identity)
// equals an existing template's name is refused, never reported as
// "prepared", and the provider does not even look the name up.
func TestImagePrepare_URLImportNeverReusesByName(t *testing.T) {
	const imageJSON = `{"source":{"http":{"url":"https://images.example.com/base.img"}}}`
	for name, req := range map[string]*providerv1.ImagePrepareRequest{
		"legacy":   legacyPrepareRequest(guardSeededTemplate, imageJSON),
		"identity": identityPrepareRequest(guardSeededTemplate, imageJSON),
	} {
		t.Run(name, func(t *testing.T) {
			p, server, _ := newGuardProvider(t)

			resp, err := p.ImagePrepare(context.Background(), req)

			assert.Nil(t, resp, "a same-named template is never adopted")
			requireURLImportRefused(t, err)
			assert.Empty(t, server.Requests(), "no template lookup by name")
		})
	}
}

// TestImagePrepare_URLImportRefusedWithoutClient verifies the refusal does not
// depend on the PVE connection: a URL import is InvalidSpec (permanent), never
// Unavailable (retried), even when the PVE client is not configured.
func TestImagePrepare_URLImportRefusedWithoutClient(t *testing.T) {
	p, _, _ := newGuardProvider(t)
	p.client = nil

	_, err := p.ImagePrepare(context.Background(),
		identityPrepareRequest("ubuntu", urlImportSources["source.http.url"]))
	requireURLImportRefused(t, err)
}

// TestImagePrepare_ReferenceStyleUnchanged verifies a reference-style source
// (source.proxmox.templateID / templateName) is verified exactly as before
// ADR-0009, from a legacy or an identity request: the template's VMID or name
// as prepared_image_id, no task, no artifact echo (nothing was prepared or
// stamped), no PVE write. A legacy request is still signalled.
func TestImagePrepare_ReferenceStyleUnchanged(t *testing.T) {
	cases := map[string]struct {
		imageJSON string
		wantID    string
	}{
		"templateName":         {`{"source":{"proxmox":{"templateName":"` + guardSeededTemplate + `"}}}`, guardSeededTemplate},
		"templateID":           {`{"source":{"proxmox":{"templateID":9000}}}`, "9000"},
		"templateID with node": {`{"source":{"proxmox":{"templateID":9000,"node":"pve"}}}`, "9000"},
		"flat TemplateName":    {`{"TemplateName":"` + guardSeededTemplate + `"}`, guardSeededTemplate},
		// A template reference wins over an import URL (verify-only beats
		// import), as before: the URL is ignored, not refused.
		"templateName wins over source.http": {
			`{"source":{"proxmox":{"templateName":"` + guardSeededTemplate + `"},"http":{"url":"https://images.example.com/x.img"}}}`,
			guardSeededTemplate,
		},
	}
	for name, tc := range cases {
		for mode, req := range map[string]*providerv1.ImagePrepareRequest{
			"legacy":   legacyPrepareRequest("ubuntu", tc.imageJSON),
			"identity": identityPrepareRequest("ubuntu", tc.imageJSON),
		} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				p, server, _ := newGuardProvider(t)
				before := proxmoxLegacyCount(t)

				resp, err := p.ImagePrepare(context.Background(), req)

				require.NoError(t, err)
				assert.Equal(t, tc.wantID, resp.GetPreparedImageId())
				assert.Empty(t, resp.GetPreparedImagePath())
				assert.Nil(t, resp.GetTask(), "verification is synchronous")
				assert.Nil(t, resp.GetArtifact(), "a referenced template is not a prepared artifact")
				assert.Empty(t, server.MutatingRequests(), "verification writes nothing")
				assert.Nil(t, server.LastDownloadRequest())
				wantCount := before
				if mode == "legacy" {
					wantCount++
				}
				assert.Equal(t, wantCount, proxmoxLegacyCount(t))
			})
		}
	}
}

// TestImagePrepare_ReferenceStyleErrorsUnchanged verifies the reference-style
// failures are the same in both modes: a missing template is NotFound, and a
// VMID that is not a template is InvalidSpec.
func TestImagePrepare_ReferenceStyleErrorsUnchanged(t *testing.T) {
	cases := map[string]struct {
		imageJSON string
		want      codes.Code
	}{
		"missing templateName": {`{"source":{"proxmox":{"templateName":"does-not-exist"}}}`, codes.NotFound},
		"missing templateID":   {`{"source":{"proxmox":{"templateID":424242}}}`, codes.NotFound},
		"VMID not a template":  {`{"source":{"proxmox":{"templateID":100}}}`, codes.InvalidArgument},
	}
	for name, tc := range cases {
		for mode, req := range map[string]*providerv1.ImagePrepareRequest{
			"legacy":   legacyPrepareRequest("ubuntu", tc.imageJSON),
			"identity": identityPrepareRequest("ubuntu", tc.imageJSON),
		} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				p, server, _ := newGuardProvider(t)
				_, err := p.ImagePrepare(context.Background(), req)
				assert.Equal(t, tc.want, imagePrepareGRPCCode(t, err))
				assert.Empty(t, server.MutatingRequests())
			})
		}
	}
}

// TestImagePrepare_MalformedRequestsRejected verifies every request is
// validated by imageartifact.ParseRequest before anything else: a malformed
// request is InvalidArgument with no PVE call and no legacy signal, even with
// a reference-style source that would otherwise verify.
func TestImagePrepare_MalformedRequestsRejected(t *testing.T) {
	const refJSON = `{"source":{"proxmox":{"templateID":9000}}}`
	valid := func() *providerv1.ImagePrepareRequest { return identityPrepareRequest("ubuntu", refJSON) }
	cases := map[string]*providerv1.ImagePrepareRequest{
		"neither identity nor target_name": {ImageJson: refJSON},
		"neither identity nor target_name, URL source": {
			ImageJson: urlImportSources["source.http.url"],
		},
		"source_digest without image identity": {ImageJson: refJSON, TargetName: "ubuntu", SourceDigest: guardDigest},
		"provider identity without image identity": {
			ImageJson: refJSON, TargetName: "ubuntu",
			Provider: &providerv1.ObjectIdentity{Uid: guardProviderUID, Namespace: "team-a", Name: "proxmox"},
		},
		"legacy target_name not a DNS name": {ImageJson: refJSON, TargetName: "Ubuntu_22"},
	}
	withTargetName := valid()
	withTargetName.TargetName = "ubuntu"
	cases["identity with a target_name"] = withTargetName
	noUID := valid()
	noUID.Image.Uid = ""
	cases["identity without uid"] = noUID
	badUID := valid()
	badUID.Image.Uid = "uid/with/slashes"
	cases["identity with an invalid uid"] = badUID
	badNamespace := valid()
	badNamespace.Image.Namespace = "Team_A"
	cases["identity with an invalid namespace"] = badNamespace
	badName := valid()
	badName.Image.Name = ""
	cases["identity with an empty name"] = badName
	noDigest := valid()
	noDigest.SourceDigest = ""
	cases["identity without source_digest"] = noDigest
	shortDigest := valid()
	shortDigest.SourceDigest = guardDigest[:len(guardDigest)-1]
	cases["identity with a truncated source_digest"] = shortDigest
	upperDigest := valid()
	upperDigest.SourceDigest = strings.ToUpper(guardDigest)
	cases["identity with a non-canonical source_digest"] = upperDigest
	badProvider := valid()
	badProvider.Provider.Uid = ""
	cases["identity with an invalid provider identity"] = badProvider

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			p, server, logs := newGuardProvider(t)
			before := proxmoxLegacyCount(t)

			resp, err := p.ImagePrepare(context.Background(), req)

			assert.Nil(t, resp)
			require.Error(t, err)
			st, ok := status.FromError(err)
			require.True(t, ok)
			assert.Equal(t, codes.InvalidArgument, st.Code())
			assert.True(t, strings.HasPrefix(st.Message(), "invalid image prepare request: "), st.Message())
			assert.Empty(t, server.Requests(), "a malformed request makes no PVE call")
			assert.Equal(t, before, proxmoxLegacyCount(t), "a malformed request is not a legacy request")
			assert.NotContains(t, logs.String(), imageartifact.LegacyRequestWarning)
		})
	}
}

// TestImagePrepare_UnservableSourceIsInvalidSpec verifies an identity request
// whose source is neither a template reference nor a URL (a registry or
// DataVolume source, or none) is InvalidSpec with no PVE call.
func TestImagePrepare_UnservableSourceIsInvalidSpec(t *testing.T) {
	for name, imageJSON := range map[string]string{
		"no source":         `{}`,
		"registry source":   `{"source":{"registry":{"image":"foo:bar"}}}`,
		"proxmox, no ref":   `{"source":{"proxmox":{"storage":"local-lvm"}}}`,
		"malformed JSON":    `{"source":`,
		"empty image JSON":  ``,
		"dataVolume source": `{"source":{"dataVolume":{"name":"dv"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			p, server, _ := newGuardProvider(t)
			_, err := p.ImagePrepare(context.Background(), identityPrepareRequest("ubuntu", imageJSON))
			require.Error(t, err)
			st, ok := status.FromError(err)
			require.True(t, ok)
			assert.Equal(t, codes.InvalidArgument, st.Code())
			assert.Equal(t, emptySourceMessage, st.Message())
			assert.Empty(t, server.Requests())
		})
	}
}

// TestGetCapabilities_AdvertisesImageArtifactIdentity verifies the provider
// advertises supports_image_artifact_identity together with
// supports_image_import (ADR-0009 D10): the manager then sends identity
// requests, whose URL imports get the honest InvalidSpec, instead of holding
// the VMImage with a false "upgrade the provider image" reason.
func TestGetCapabilities_AdvertisesImageArtifactIdentity(t *testing.T) {
	p, _, _ := newGuardProvider(t)
	resp, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, resp.GetSupportsImageArtifactIdentity())
	assert.True(t, resp.GetSupportsImageImport(), "identity is only meaningful with image import")
}
