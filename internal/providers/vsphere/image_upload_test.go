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
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// failNFCUploads breaks every NFC upload with a connection reset.
type failNFCUploads struct{ rt http.RoundTripper }

func (f *failNFCUploads) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/nfc/") && (r.Method == http.MethodPut || r.Method == http.MethodPost) {
		return nil, &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
	}
	return f.rt.RoundTrip(r)
}

// vCenterDown makes vCenter's CurrentTime fail at the transport level.
type vCenterDown struct{ soap.RoundTripper }

func (v *vCenterDown) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if _, ok := req.(*methods.CurrentTimeBody); ok {
		return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	return v.RoundTripper.RoundTrip(ctx, req, res)
}

// TestUploadFailureClassification (review R5): once the import lease is
// ready, a network error during the NFC upload is the image transfer's
// problem, and counts toward the manager's circuit breaker only when vCenter
// itself does not answer a cheap CurrentTime call either.
func TestUploadFailureClassification(t *testing.T) {
	const iso = "ISO-CONTENT"
	ova := tarOVAMembers(t, [][2]string{
		{"descriptor.ovf", fmt.Sprintf(isoOVFTemplate, "seed.iso", len(iso))},
		{"seed.iso", iso},
	})
	breakUploads := func(p *Provider) {
		sc := p.client.Client.Client
		rt := sc.Transport
		if rt == nil {
			rt = http.DefaultTransport
		}
		sc.Transport = &failNFCUploads{rt: rt}
	}

	t.Run("vCenter answers: an image-source failure", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		breakUploads(p)
		_, err := p.ImagePrepare(context.Background(), identityReq(t, serveBody(t, http.StatusOK, ova), testImageUID, testDigestA))
		requireCode(t, err, codes.Unavailable)
		assert.Equal(t, []string{contracts.ImageSourceUnavailableReason}, errorReasons(err),
			"not counted toward the circuit breaker")
		assert.Contains(t, err.Error(), "upload")
		requireNoVMNamed(t, p, artifactNameFor(t, testImageUID, testDigestA))
	})

	t.Run("vCenter does not answer either: a vCenter failure", func(t *testing.T) {
		p, _, _ := newIdentitySim(t, "")
		breakUploads(p)
		p.client.RoundTripper = &vCenterDown{RoundTripper: p.client.RoundTripper}
		_, err := p.ImagePrepare(context.Background(), identityReq(t, serveBody(t, http.StatusOK, ova), testImageUID, testDigestA))
		requireCode(t, err, codes.Unavailable)
		assert.Empty(t, errorReasons(err), "counted toward the circuit breaker")
	})
}
