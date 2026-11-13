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
	"syscall"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
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

// TestReloadReenrolsOnANewEnrolment: a certificate for a new enrolment of
// this host, loaded on SIGHUP, re-enrols the store and then replaces the
// credentials, so the next answer names the new enrolment. Going back to the
// old certificate is refused (its enrolment id was used) and the running
// credentials stay.
func TestReloadReenrolsOnANewEnrolment(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if got := answerEnrolment(r); string(got) != string(h.pki.Record.EnrolmentID) {
		t.Fatalf("first answer names %x", got)
	}
	next := certWithEnrolment(h, 0x77)
	h.installCert(next)
	r.signals <- syscall.SIGHUP
	waitLog(t, h, "credentials reloaded")
	waitLog(t, h, "re-enrolled from the host certificate")
	got := answerEnrolment(r)
	if len(got) != hostidentity.IDLen || got[0] != 0x77 {
		t.Fatalf("answer after the reload names %x, want the new enrolment", got)
	}

	// The old certificate: its enrolment id is retired in the store.
	h.installCert(h.pki.HostCertPEM)
	r.signals <- syscall.SIGHUP
	waitLog(t, h, "reload refused")
	status := systemd.NotifyReady + "\nSTATUS=AGENT_STATE_READY; reload refused: " + daemon.ReasonReenrolmentRefused
	for deadline := time.Now().Add(5 * time.Second); !h.notes.has(status); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no refusal status: %v", h.notes.snapshot())
		}
	}
	if got := answerEnrolment(r); len(got) != hostidentity.IDLen || got[0] != 0x77 {
		t.Fatalf("answer after the refused reload names %x", got)
	}
}

// TestRestartWithANewCertificateReenrols: a start with a certificate that
// names a new enrolment of the host re-enrols the store at step 6; a start
// with the old certificate again does not undo it (the id is used) and the
// answers stay unsigned.
func TestRestartWithANewCertificateReenrols(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	h.installCert(certWithEnrolment(h, 0x66))
	r, _ = h.run()
	if got := answerEnrolment(r); len(got) != hostidentity.IDLen || got[0] != 0x66 {
		t.Fatalf("answer after the restart names %x", got)
	}
	waitLog(t, h, "re-enrolled from the host certificate")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	h.installCert(h.pki.HostCertPEM)
	r, _ = h.run()
	if got := answerEnrolment(r); got != nil {
		t.Fatalf("an answer under a used enrolment: %x", got)
	}
	waitLog(t, h, "enrolment from the host certificate failed")
}
