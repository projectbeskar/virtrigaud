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

package imageartifact

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// wireImage returns the wire identity of team-a/ubuntu.
func wireImage() *providerv1.ObjectIdentity {
	return &providerv1.ObjectIdentity{Uid: testUID, Namespace: "team-a", Name: "ubuntu"}
}

// wireProvider returns the wire identity of the Provider team-a/libvirt.
func wireProvider() *providerv1.ObjectIdentity {
	return &providerv1.ObjectIdentity{Uid: "8c1e2f3a-4b5c-4d6e-8f7a-9b0c1d2e3f4a", Namespace: "team-a", Name: "libvirt"}
}

// TestParseRequestIdentity verifies an identity request is decoded with its
// image, digest and Provider, and that the Provider is optional.
func TestParseRequestIdentity(t *testing.T) {
	got, err := ParseRequest(&providerv1.ImagePrepareRequest{
		ImageJson: "{}", Image: wireImage(), SourceDigest: testDigest, Provider: wireProvider(),
	})
	require.NoError(t, err)
	assert.Equal(t, Request{
		Mode:         ModeIdentity,
		Image:        testImage("team-a", "ubuntu"),
		SourceDigest: testDigest,
		PreparedBy:   contracts.ObjectIdentity{UID: "8c1e2f3a-4b5c-4d6e-8f7a-9b0c1d2e3f4a", Namespace: "team-a", Name: "libvirt"},
	}, got)

	got, err = ParseRequest(&providerv1.ImagePrepareRequest{Image: wireImage(), SourceDigest: testDigest})
	require.NoError(t, err)
	assert.Equal(t, ModeIdentity, got.Mode)
	assert.True(t, got.PreparedBy.IsZero())
}

// TestParseRequestLegacy verifies a request without an image identity is a
// legacy request for its bare target name (ADR-0009 D7, Q3).
func TestParseRequestLegacy(t *testing.T) {
	got, err := ParseRequest(&providerv1.ImagePrepareRequest{ImageJson: "{}", TargetName: "ubuntu-22.04"})
	require.NoError(t, err)
	assert.Equal(t, Request{Mode: ModeLegacy, LegacyTargetName: "ubuntu-22.04"}, got)
}

// TestParseRequestNeverDowngradesToLegacy verifies a request that carries any
// identity field (source_digest, provider) but no image identity is refused,
// never served as legacy: an older manager sends neither field, so such a
// request is a broken identity request (e.g. a UID-less image dropped on the
// wire) and must not fall back to bare-name reuse.
func TestParseRequestNeverDowngradesToLegacy(t *testing.T) {
	for name, req := range map[string]*providerv1.ImagePrepareRequest{
		"digest and provider with a target_name": {TargetName: "ubuntu", SourceDigest: testDigest, Provider: wireProvider()},
		"digest with a target_name":              {TargetName: "ubuntu", SourceDigest: testDigest},
		"provider with a target_name":            {TargetName: "ubuntu", Provider: wireProvider()},
		"empty provider with a target_name":      {TargetName: "ubuntu", Provider: &providerv1.ObjectIdentity{}},
		"digest without a target_name":           {SourceDigest: testDigest},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRequest(req)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
}

// TestParseRequestRejectsMalformed verifies every malformed request is
// InvalidArgument, never served.
func TestParseRequestRejectsMalformed(t *testing.T) {
	withImage := func(mutate func(*providerv1.ImagePrepareRequest)) *providerv1.ImagePrepareRequest {
		r := &providerv1.ImagePrepareRequest{Image: wireImage(), SourceDigest: testDigest, Provider: wireProvider()}
		mutate(r)
		return r
	}
	for name, req := range map[string]*providerv1.ImagePrepareRequest{
		"neither identity nor target_name":                {ImageJson: "{}"},
		"legacy name with underscore (a new-scheme name)": {TargetName: "team-a.ubuntu_f06d2f97535bae75"},
		"legacy name uppercase":                           {TargetName: "Ubuntu"},
		"legacy name with slash":                          {TargetName: "a/b"},
		"identity with a target_name":                     withImage(func(r *providerv1.ImagePrepareRequest) { r.TargetName = "ubuntu" }),
		"identity without uid": withImage(func(r *providerv1.ImagePrepareRequest) {
			r.Image = &providerv1.ObjectIdentity{Namespace: "team-a", Name: "ubuntu"}
		}),
		"identity with empty message": withImage(func(r *providerv1.ImagePrepareRequest) { r.Image = &providerv1.ObjectIdentity{} }),
		"identity with invalid uid":   withImage(func(r *providerv1.ImagePrepareRequest) { r.Image.Uid = "x/y" }),
		"identity without namespace":  withImage(func(r *providerv1.ImagePrepareRequest) { r.Image.Namespace = "" }),
		"identity with invalid name":  withImage(func(r *providerv1.ImagePrepareRequest) { r.Image.Name = "Ubuntu_1" }),
		"identity without digest":     withImage(func(r *providerv1.ImagePrepareRequest) { r.SourceDigest = "" }),
		"identity with bad digest":    withImage(func(r *providerv1.ImagePrepareRequest) { r.SourceDigest = "sha256:XYZ" }),
		"provider without uid":        withImage(func(r *providerv1.ImagePrepareRequest) { r.Provider.Uid = "" }),
		"provider with invalid name":  withImage(func(r *providerv1.ImagePrepareRequest) { r.Provider.Name = "a b" }),
		"provider with invalid ns":    withImage(func(r *providerv1.ImagePrepareRequest) { r.Provider.Namespace = "A" }),
		"identity without name":       withImage(func(r *providerv1.ImagePrepareRequest) { r.Image.Name = "" }),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRequest(req)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), err.Error())
		})
	}
}

// TestConflictAndInProgressErrors pins the codes the manager maps to a
// Conflict (non-retryable) and a retryable error, and that the Conflict
// message names only the requester's own artifact.
func TestConflictAndInProgressErrors(t *testing.T) {
	err := ConflictError("team-a.ubuntu_f06d2f97535bae75")
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
	assert.Contains(t, err.Error(), "team-a.ubuntu_f06d2f97535bae75")
	assert.Contains(t, err.Error(), "refusing to use or replace it")

	err = InProgressError("team-a.ubuntu_f06d2f97535bae75")
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Contains(t, err.Error(), "team-a.ubuntu_f06d2f97535bae75")
	// The ErrorInfo tells the manager it is not an unreachable provider.
	st, ok := status.FromError(err)
	require.True(t, ok)
	var reasons []string
	for _, d := range st.Details() {
		if info, isInfo := d.(*errdetails.ErrorInfo); isInfo && info.GetDomain() == contracts.ErrorInfoDomain {
			reasons = append(reasons, info.GetReason())
		}
	}
	assert.Equal(t, []string{contracts.ImageArtifactInProgressReason}, reasons)
}

// legacyCounter returns virtrigaud_provider_image_prepare_legacy_requests_total
// for providerType (0 before the first increment).
func legacyCounter(t *testing.T, providerType string) float64 {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "virtrigaud_provider_image_prepare_legacy_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			if hasLabel(m, "provider_type", providerType) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// hasLabel reports whether m carries the label name=value.
func hasLabel(m *dto.Metric, name, value string) bool {
	for _, l := range m.GetLabel() {
		if l.GetName() == name && l.GetValue() == value {
			return true
		}
	}
	return false
}

// TestSignalLegacyRequest verifies the ADR-0009 D7 deprecation signal: a WARN
// log with the documented message and one counter increment per request.
func TestSignalLegacyRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	before := legacyCounter(t, "test-signal")

	SignalLegacyRequest(context.Background(), logger, "test-signal", "ubuntu")
	SignalLegacyRequest(context.Background(), nil, "test-signal", "ubuntu") // nil logger: slog.Default()

	assert.Equal(t, before+2, legacyCounter(t, "test-signal"))
	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "upgrade the manager")
	assert.Contains(t, out, "provider_type=test-signal")
	assert.Contains(t, out, "target_name=ubuntu")
	assert.Equal(t, 1, strings.Count(out, "level=WARN"), "the nil-logger call goes to slog.Default, not this logger")
}
