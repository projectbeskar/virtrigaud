//go:build virtrigaud_dev

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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/enrol"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/none"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck/hostchecktest"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal/journaltest"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
)

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
	waitAnswerEnrolment(t, r, 0x77)

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

// TestStoreOfAnotherHostIsNotReenrolled (end to end; each check alone is in
// TestStoreOwnerMismatch): a store that was cut short between
// the freeze and the activation of a re-enrolment has no active enrolment. A
// certificate of a different host must not be enrolled into it, whether the
// store marker or the frozen predecessor names the old host.
func TestStoreOfAnotherHostIsNotReenrolled(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	// Leave the store between freeze and activation, for this host.
	ctx := context.Background()
	crash := &journaltest.Crasher{Point: opstore.PointMidReenrolment, N: 1}
	st, err := opstore.Open(ctx, opstore.Options{
		Journal:  journal.Options{Dir: h.stateDir, Hooks: journal.Hooks{Crash: crash.At}},
		Executor: none.Executor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	hv, err := st.HostView(ctx, 0)
	if err != nil || hv.Enrolment == nil {
		t.Fatalf("host view: %+v %v", hv, err)
	}
	if latest, err := st.LatestEnrolmentHostID(ctx); err != nil || latest != trusttest.HostName {
		t.Fatalf("newest enrolment's host = %q, %v", latest, err)
	}
	next := opstore.Enrolment{ID: make([]byte, 16), ProviderUID: trusttest.ProviderUID, HostID: trusttest.HostName, StartEpoch: hv.LastStartEpoch + 1}
	next.ID[0] = 9
	if err := st.Reenrol(ctx, next); !errors.Is(err, journal.ErrCrashed) {
		t.Fatalf("reenrol: %v", err)
	}
	_ = st.Close()

	// The same disk is given another host's identity.
	otherURI, err := hostidentity.HostURI(trusttest.TrustDomain, trusttest.HostNS, "another-host")
	if err != nil {
		t.Fatal(err)
	}
	h.installCert(h.pki.IssueHost(t, h.pki.HostKey, hostidentity.RequestURIs(otherURI, h.pki.Record), hostidentitytest.IssueOptions{}))
	marker := filepath.Join(h.stateDir, hostcheck.HostMarkerFile)
	if err := os.WriteFile(marker, []byte(otherURI+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.probe.Edit(func(f *hostchecktest.Fake) { f.Content[marker] = []byte(otherURI + "\n") })
	r, _ = h.run()
	waitLog(t, h, "StoreOfOtherHost")
	if got := answerEnrolment(r); got != nil {
		t.Fatalf("an answer from a store of another host: %x", got)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	st, err = opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: none.Executor{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if hv, err := st.HostView(ctx, 0); err != nil || hv.Enrolment != nil {
		t.Fatalf("the store was enrolled for another host: %+v %v", hv, err)
	}
}

// TestReenrolmentAtExactlyTheCertificatesEpoch: the store re-enrols at the
// start epoch the certificate's record carries, and the daemon records it for
// enroll install. A certificate for a new enrolment whose epoch is not after
// the store's last start epoch is refused on reload; the store is not touched
// and the running credentials stay.
func TestReenrolmentAtExactlyTheCertificatesEpoch(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	h.installCert(certWithEnrolmentAt(h, 0x31, 7))
	r.signals <- syscall.SIGHUP
	waitLog(t, h, "re-enrolled from the host certificate")
	waitAnswerEnrolment(t, r, 0x31)
	readEpoch := func() uint64 {
		e, found, err := enrol.ReadStartEpoch(h.cfg.PKIDir(), os.ReadFile)
		if err != nil || !found {
			t.Fatalf("recorded start epoch: %v %v", found, err)
		}
		return e
	}
	for deadline := time.Now().Add(5 * time.Second); readEpoch() != 7; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("recorded start epoch %d, want 7", readEpoch())
		}
	}

	// A new enrolment at epoch 7 (not after 7) and at 5: refused, the store
	// keeps its enrolment.
	for i, epoch := range []uint64{7, 5} {
		h.installCert(certWithEnrolmentAt(h, byte(0x40+i), epoch))
		r.signals <- syscall.SIGHUP
		waitLogCount(t, h, "reload refused", i+1)
		if got := answerEnrolment(r); len(got) != hostidentity.IDLen || got[0] != 0x31 {
			t.Fatalf("answer after the refused reload names %x", got)
		}
	}
	if !strings.Contains(h.logs.text(), "start epoch is not after") {
		t.Fatalf("no epoch refusal in the log:\n%s", h.logs.text())
	}
	if readEpoch() != 7 {
		t.Fatal("a refused certificate moved the recorded start epoch")
	}
	// At epoch 8 it is accepted.
	h.installCert(certWithEnrolmentAt(h, 0x50, 8))
	r.signals <- syscall.SIGHUP
	waitLogCount(t, h, "re-enrolled from the host certificate", 2)
	waitAnswerEnrolment(t, r, 0x50)
}

// waitLogCount waits until sub appears at least n times in the logs.
func waitLogCount(t *testing.T, h *host, sub string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(h.logs.text(), sub) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%q fewer than %d times in the logs:\n%s", sub, n, h.logs.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitAnswerEnrolment waits, bounded, until the agent's signed answer names
// the enrolment whose id is b repeated. The daemon logs a re-enrolment only
// once the new credentials are in use, so this normally holds at once; the
// poll keeps the check independent of that ordering.
func waitAnswerEnrolment(t *testing.T, r *running, b byte) {
	t.Helper()
	var got []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got = answerEnrolment(r); len(got) == hostidentity.IDLen && got[0] == b {
			return
		}
	}
	t.Fatalf("answer names %x, want the enrolment %02x...", got, b)
}

// TestDevEnrolmentFileMustMatchTheRecordsEpoch: on a fresh store, a
// development enrolment file whose start epoch differs from the one the
// certificate's record carries is refused (the store stays unenrolled and
// answers unsigned); an equal one enrols at that epoch. A record without an
// epoch keeps the file's (TestReenrolmentFreezesTheOpenOperations and the
// harness use it).
func TestDevEnrolmentFileMustMatchTheRecordsEpoch(t *testing.T) {
	h := newHost(t)
	h.installCert(certWithEnrolmentAt(h, 0x21, 5)) // the file says 2
	r, _ := h.run()
	if got := answerEnrolment(r); got != nil {
		t.Fatalf("an answer under a refused enrolment: %x", got)
	}
	waitLog(t, h, "start epoch differs from the certificate's record")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	h.dev.StartEpoch = 5
	r, _ = h.run()
	waitAnswerEnrolment(t, r, 0x21)
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	st, err := opstore.Open(context.Background(), opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: none.Executor{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	v, err := st.HostView(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.LastStartEpoch != 5 {
		t.Fatalf("start epoch %d, want the record's 5", v.LastStartEpoch)
	}
}
