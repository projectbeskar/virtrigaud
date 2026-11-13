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
	"syscall"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
)

// TestReleaseBuildDoesNotReenrol: a release build keeps the running
// enrolment: a certificate of the same host with a new enrolment is refused on
// reload (EnrolmentChanged), and a restart with it leaves the store as it was,
// with unsigned answers.
func TestReleaseBuildDoesNotReenrol(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	first := answerEnrolment(r)
	if string(first) != string(h.pki.Record.EnrolmentID) {
		t.Fatalf("first answer names %x", first)
	}
	h.installCert(certWithEnrolment(h, 0x77))
	r.signals <- syscall.SIGHUP
	waitLog(t, h, "reload refused")
	status := systemd.NotifyReady + "\nSTATUS=AGENT_STATE_READY; reload refused: " + daemon.ReasonEnrolmentChanged
	for deadline := time.Now().Add(5 * time.Second); !h.notes.has(status); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no refusal status: %v", h.notes.snapshot())
		}
	}
	if got := answerEnrolment(r); string(got) != string(first) {
		t.Fatalf("answer after the refused reload names %x", got)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	r, _ = h.run()
	if got := answerEnrolment(r); got != nil {
		t.Fatalf("a release build answered under another enrolment: %x", got)
	}
	waitLog(t, h, "a release build does not re-enrol")
}
