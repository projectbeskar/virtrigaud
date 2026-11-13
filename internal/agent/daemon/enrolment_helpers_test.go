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
	"os"
	"path/filepath"

	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

// certWithEnrolment returns the host's certificate for a new enrolment id (b
// repeated), the same host, key and Provider.
func certWithEnrolment(h *host, b byte) []byte {
	h.t.Helper()
	rec := h.pki.Record
	rec.EnrolmentID = make([]byte, hostidentity.IDLen)
	for i := range rec.EnrolmentID {
		rec.EnrolmentID[i] = b
	}
	return h.pki.IssueHost(h.t, h.pki.HostKey, hostidentity.RequestURIs(h.pki.HostURI, rec), hostidentitytest.IssueOptions{})
}

func (h *host) installCert(cert []byte) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.cfg.PKIDir(), trust.FileHostCert), cert, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// answerEnrolment returns the enrolment id the host's signed answer names,
// or nil when the answer is unsigned.
func answerEnrolment(r *running) []byte {
	if a := r.hello(); a != nil {
		return a.GetContext().GetEnrolmentId()
	}
	return nil
}
