//go:build !virtrigaud_dev

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

package daemon_test

import (
	"bytes"
	"encoding/pem"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestReleaseBuildServesNoRenewal: a release build never renews, even past
// two thirds of its certificate's lifetime: no request is made, no answer
// carries one, and InstallRenewedCertificate is Unimplemented.
func TestReleaseBuildServesNoRenewal(t *testing.T) {
	h := newHost(t)
	h.nowOff.Store(int64(21 * 24 * time.Hour))
	r, _ := h.run()
	cc := r.conn()
	time.Sleep(100 * time.Millisecond) // several renewal timer periods of a development build
	if exists(t, filepath.Join(h.cfg.PKIDir(), enrol.FileRenewalCSR)) {
		t.Fatal("a release build made a renewal request")
	}
	info, err := providerv1.NewProviderClient(cc).GetHostInfo(callCtx(t),
		&providerv1.GetHostInfoRequest{AnswerNonce: bytes.Repeat([]byte{5}, 16), RenewalVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if blob := info.GetHostAnswer(); blob != nil {
		a := &providerv1.HostAnswer{}
		if err := proto.Unmarshal(blob.GetBody(), a); err != nil {
			t.Fatal(err)
		}
		if a.GetPendingRenewal() != nil || len(a.GetStagedCertificateSha256()) != 0 {
			t.Fatal("a release build's answer carries a renewal")
		}
	}
	b, _ := pem.Decode(h.pki.HostCertPEM)
	_, err = agentv1.NewAgentServiceClient(cc).InstallRenewedCertificate(callCtx(t),
		&agentv1.InstallRenewedCertificateRequest{AnswerNonce: bytes.Repeat([]byte{6}, 16), Certificate: b.Bytes})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("InstallRenewedCertificate = %v, want Unimplemented", err)
	}
}
