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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// dripServer sends its headers at once, then one byte every interval until the
// client goes away (or, with interval 0, nothing at all).
func dripServer(t *testing.T, interval time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		if interval == 0 {
			<-r.Context().Done()
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte{'x'})
				if ok {
					flusher.Flush()
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/image.ova"
}

// TestDownloadOVA_ThroughputWatchdog (review R2): a source that sends its
// headers promptly and then drip-feeds (or withholds) its body is abandoned
// once a watchdog window moves too few bytes — an image-source failure that
// never counts toward the circuit breaker. A fast source is unaffected.
func TestDownloadOVA_ThroughputWatchdog(t *testing.T) {
	p := downloadProvider(true, 0)
	p.downloadStallWindow = 150 * time.Millisecond
	p.downloadMinBytesPerWindow = 64 << 10

	for name, u := range map[string]string{
		"drip-feeding source": dripServer(t, 20*time.Millisecond),
		"silent source":       dripServer(t, 0),
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			_, cleanup, err := p.downloadOVA(context.Background(), u)
			cleanup()
			requireCode(t, err, codes.Unavailable)
			assert.Contains(t, err.Error(), "too slow")
			assert.Equal(t, []string{contracts.ImageSourceUnavailableReason}, errorReasons(err))
			assert.Less(t, time.Since(start), 5*time.Second, "abandoned after about one window")
		})
	}

	t.Run("fast source", func(t *testing.T) {
		body := bytes.Repeat([]byte("a"), 1<<20)
		path, cleanup, err := p.downloadOVA(context.Background(), serveBody(t, http.StatusOK, body))
		require.NoError(t, err)
		defer cleanup()
		assert.NotEmpty(t, path)
	})
}

// TestImagePrepare_GivesUpBeforeTheManagersDeadline (review R2): the prepare
// runs on a deadline importDeadlineMargin before the request's, so a download
// or import that would outlast the manager's own timeout ends in an answer the
// manager can classify — an image-source failure, kept out of its circuit
// breaker — instead of the manager's DeadlineExceeded.
func TestImagePrepare_GivesUpBeforeTheManagersDeadline(t *testing.T) {
	p, _, _ := newIdentitySim(t, "")
	// Slow enough to never finish, fast enough to pass the watchdog.
	p.downloadStallWindow = time.Hour
	u := dripServer(t, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), importDeadlineMargin+300*time.Millisecond)
	defer cancel()
	_, err := p.ImagePrepare(ctx, identityReq(t, u, testImageUID, testDigestA))
	requireCode(t, err, codes.Unavailable)
	assert.NoError(t, ctx.Err(), "answered before the caller's deadline")
	assert.Contains(t, err.Error(), "before the manager's deadline")
	assert.Equal(t, []string{contracts.ImageSourceUnavailableReason}, errorReasons(err))
}

func TestIsDefinitiveAnswer(t *testing.T) {
	for _, err := range []error{
		descriptorTooLargeError(),
		imageartifact.ConflictError("x"),
		imageartifact.InProgressError("x"),
		status.Error(codes.FailedPrecondition, "folder"),
	} {
		assert.True(t, isDefinitiveAnswer(err), "%v", err)
	}
	for _, err := range []error{
		context.DeadlineExceeded,
		imageartifact.SourceUnavailableError("a cut-off download"),
		status.Error(codes.Unavailable, "vCenter"),
		status.Error(codes.Internal, "import"),
	} {
		assert.False(t, isDefinitiveAnswer(err), "%v", err)
	}
}
