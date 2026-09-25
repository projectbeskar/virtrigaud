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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestImagePrepare_SourceFailuresAreSourceUnavailable proves a retryable
// download failure the image source caused goes on the wire as
// IMAGE_SOURCE_UNAVAILABLE (retried, kept out of the manager's circuit
// breaker), with the historical text; a failure of the libvirt host or the SSH
// transport still counts, and a permanent one is still InvalidSpec.
func TestImagePrepare_SourceFailuresAreSourceUnavailable(t *testing.T) {
	// The historical wire text: fmt.Errorf("failed to prepare image: %w") over
	// the retryable ProviderError.
	wantMessage := "failed to prepare image: " + contracts.NewRetryableError(downloadRetryMessage, nil).Error()
	type outcome int
	const (
		sourceUnavailable outcome = iota
		counted
		permanent
	)
	for name, tc := range map[string]struct {
		setup func(t *testing.T, h *prepareHost)
		hook  func(args []string) *hookAnswer
		want  outcome
	}{
		"HTTP 503":              {setup: failDownload("503 22"), want: sourceUnavailable},
		"HTTP 500":              {setup: failDownload("500 22"), want: sourceUnavailable},
		"HTTP 408":              {setup: failDownload("408 22"), want: sourceUnavailable},
		"HTTP 425":              {setup: failDownload("425 22"), want: sourceUnavailable},
		"HTTP 429":              {setup: failDownload("429 22"), want: sourceUnavailable},
		"name does not resolve": {setup: failDownload("000 6"), want: sourceUnavailable},
		"connection refused":    {setup: failDownload("000 7"), want: sourceUnavailable},
		"timed out":             {setup: failDownload("000 28"), want: sourceUnavailable},
		"TLS handshake failed":  {setup: failDownload("000 35"), want: sourceUnavailable},
		"truncated body":        {setup: failDownload("200 18"), want: sourceUnavailable},
		"empty reply":           {setup: failDownload("000 52"), want: sourceUnavailable},
		"receive failed":        {setup: failDownload("200 56"), want: sourceUnavailable},

		"write error in the pool directory": {setup: failDownload("200 23"), want: counted},
		"curl missing on the host":          {setup: failDownload("000 127"), want: counted},
		"SSH transport fails":               {hook: failWhen(func(a []string) bool { return len(a) > 1 && a[1] == "curl" }), want: counted},

		"HTTP 404":            {setup: failDownload("404 22"), want: permanent},
		"TLS cert unverified": {setup: failDownload("000 60"), want: permanent},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPrepareHost(t)
			if tc.setup != nil {
				tc.setup(t, h)
			}
			js := urlImageJSON(testImageURL)
			req, err := imageartifact.ParseRequest(identityProtoReq(testImageUID, testDigest, js))
			require.NoError(t, err)
			_, err = h.preparer(&hookedHost{VirshProvider: h.vp, before: tc.hook}).prepare(context.Background(), req, js, "", h.policy)
			require.Error(t, err)

			wire := rpcErr(err)
			st, _ := status.FromError(wire)
			switch tc.want {
			case sourceUnavailable:
				assert.True(t, contracts.IsRetryable(err), "still a retryable provider error inside: %v", err)
				assert.Equal(t, codes.Unavailable, st.Code())
				assert.Equal(t, contracts.ImageSourceUnavailableReason, errorInfoReason(st))
				assert.Equal(t, wantMessage, st.Message(), "the historical text, nothing more")
			case counted:
				assert.NotEqual(t, codes.InvalidArgument, st.Code(), "retryable: %v", wire)
				assert.Empty(t, errorInfoReason(st), "counts toward the circuit breaker: %v", wire)
			case permanent:
				assert.Equal(t, codes.InvalidArgument, st.Code(), "permanent: %v", wire)
				assert.Empty(t, errorInfoReason(st))
			}
			assertNoStagingFiles(t, h.images)
		})
	}
}

// TestImagePrepare_SourceUnavailableOnTheWire drives the RPC itself: the
// tagged answer reaches the caller unchanged, with no status, exit code or URL.
func TestImagePrepare_SourceUnavailableOnTheWire(t *testing.T) {
	h := newPrepareHost(t)
	failDownload("503 22")(t, h)
	_, err := h.s.ImagePrepare(context.Background(), identityProtoReq(testImageUID, testDigest, urlImageJSON(testImageURL)))
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.Equal(t, contracts.ImageSourceUnavailableReason, errorInfoReason(st))
	for _, f := range []string{"503", "22", "curl", "images.example.com", "supersecret"} {
		assert.NotContains(t, err.Error(), f)
	}
}

func TestIsSourceFailure(t *testing.T) {
	for _, tc := range []struct {
		exit, http int
		want       bool
	}{
		{curlExitHTTPError, 502, true},
		{curlExitHTTPError, 429, true},
		{curlExitHTTPError, 403, false}, // permanent, classified before
		{curlExitHTTPError, 0, false},
		{7, 0, true},
		{18, 200, true},
		{23, 200, false}, // write error: the host's
		{-1, 0, false},   // the transport's
		{127, 0, false},  // curl missing: the host's
	} {
		assert.Equal(t, tc.want, isSourceFailure(tc.exit, tc.http), "exit %d http %d", tc.exit, tc.http)
	}
}
