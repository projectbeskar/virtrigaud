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

package grpc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/resilience"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestClient_PrepareImage_SourceUnavailableDoesNotTripBreaker verifies an
// ImagePrepare answered with IMAGE_SOURCE_UNAVAILABLE
// (imageartifact.SourceUnavailableError: the image's source or content failed,
// not the provider) is retryable for the manager but never counts toward the
// per-Provider circuit breaker, so one tenant's failing image cannot open the
// breaker for every tenant of the Provider. The reason is honoured on
// ImagePrepare only, and a plain Unavailable from ImagePrepare still counts.
func TestClient_PrepareImage_SourceUnavailableDoesNotTripBreaker(t *testing.T) {
	sourceDown := imageartifact.SourceUnavailableError(
		"ImagePrepare: the image source is unavailable (details in the provider log); will retry")
	prepare := providerv1.Provider_ImagePrepare_FullMethodName
	assert.False(t, countsTowardBreaker(prepare, sourceDown), "a failing image source says nothing about the provider's health")
	assert.True(t, countsTowardBreaker(prepare, status.Error(codes.Unavailable, "provider down")))
	assert.True(t, countsTowardBreaker(providerv1.Provider_Create_FullMethodName, sourceDown),
		"on any other RPC the reason is not a VirtRigaud answer: the Unavailable counts")

	mapped := (&Client{}).mapGRPCError("image prepare", sourceDown)
	assert.True(t, contracts.IsRetryable(mapped), "%v", mapped)
	assert.False(t, contracts.IsInvalidSpec(mapped))
	assert.False(t, contracts.IsInProgress(mapped))

	var calls atomic.Int32
	var answer atomic.Value
	answer.Store(sourceDown)
	dialer, cleanup := startBufconnServer(t, &imagePrepareFakeServer{
		fn: func(_ context.Context, _ *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
			calls.Add(1)
			return nil, answer.Load().(error)
		},
	})
	defer cleanup()
	cli, cb := newTestClientWithCB(t, dialer, "imgsrc", "imgsrc-provider", &resilience.Config{
		FailureThreshold: 2,
		ResetTimeout:     30 * time.Second,
		HalfOpenMaxCalls: 1,
	})
	req := contracts.ImagePrepareRequest{Image: contracts.ObjectIdentity{UID: "img-uid"}, SourceDigest: testImageDigest}
	for i := 0; i < 5; i++ {
		_, err := cli.PrepareImage(context.Background(), req)
		require.Error(t, err)
		assert.True(t, contracts.IsRetryable(err), "%v", err)
	}
	assert.Equal(t, int32(5), calls.Load(), "every call reached the provider")
	assert.Equal(t, resilience.StateClosed, cb.GetState(), "the breaker stays closed")

	// Control: the same RPC answered with a plain Unavailable trips it.
	answer.Store(status.Error(codes.Unavailable, "provider down"))
	for i := 0; i < 2; i++ {
		_, _ = cli.PrepareImage(context.Background(), req)
	}
	assert.Equal(t, resilience.StateOpen, cb.GetState())
}
