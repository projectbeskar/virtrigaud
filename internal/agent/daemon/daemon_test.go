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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/daemon"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/none"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck/hostchecktest"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestStartEmptyStore: a fresh host passes every check, enrols from its
// certificate, establishes continuity, recovers an empty store, notifies
// READY=1, answers Hello with a signed READY answer, refuses mutations
// (Unimplemented) and drains on SIGTERM.
func TestStartEmptyStore(t *testing.T) {
	h := newHost(t)
	r, state := h.run()
	if state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("state = %v\n%s", state, h.logs.text())
	}
	if !h.notes.has(systemd.NotifyReady) {
		t.Fatal("READY=1 not sent")
	}
	a := r.hello()
	if a == nil || a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_READY ||
		a.GetContext().GetHostId() != trusttest.HostName || a.GetClockFloor() == nil {
		t.Fatalf("answer = %v", a)
	}
	// A release build has no operation path; a development build verifies
	// the grant, and a call without one is refused before anything else.
	wantCode, wantReason := "Unimplemented", ""
	if config.DevBuild {
		wantCode, wantReason = "FailedPrecondition", "IDENTITY_REQUIRED"
	}
	if code, reason := r.create(); code != wantCode || reason != wantReason {
		t.Fatalf("Create = %s %s", code, reason)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if !h.notes.has(systemd.NotifyStopping) {
		t.Fatal("STOPPING=1 not sent")
	}
	// The store is closed and consistent: it reopens unattested.
	st, err := opstore.Open(context.Background(), opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: none.Executor{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if js, r := st.Journal().State(); js != journal.StateUnattested {
		t.Fatalf("reopened store = %v{%s}", js, r)
	}
	if _, found, err := st.LoadClockFloor(context.Background()); err != nil || !found {
		t.Fatalf("the clock floor record is not in the store: found %v, err %v", found, err)
	}
	if exists(t, filepath.Join(h.stateDir, "clock.floor")) {
		t.Fatal("a clock floor file was written beside the store")
	}
	// A second run starts READY again, without enrolling again.
	_ = st.Close()
	_, state = h.run()
	if state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("restart state = %v", state)
	}
}

// TestUnattestedBeforeReady: while recovery has not run, reads answer
// UNATTESTED and every mutation is refused UNATTESTED; READY=1 is not sent.
func TestUnattestedBeforeReady(t *testing.T) {
	h := newHost(t)
	gate, release := make(chan struct{}), make(chan struct{})
	h.hooks.BeforeRecover = func(context.Context) {
		close(gate)
		<-release
	}
	r := h.start()
	<-gate
	if a := r.hello(); a == nil || a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_UNATTESTED {
		t.Fatalf("answer before ready = %v", a)
	}
	if code, reason := r.create(); code != "Unavailable" || reason != opstore.ReasonUnattested {
		t.Fatalf("Create before ready = %s %s", code, reason)
	}
	if h.notes.has(systemd.NotifyReady) {
		t.Fatal("READY=1 sent before recovery")
	}
	close(release)
	if s := <-r.started; s != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("state = %v", s)
	}
	if a := r.hello(); a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("answer after ready = %v", a)
	}
}

// TestStopDuringRecovery: SIGTERM while recovery runs cancels it; the agent
// exits cleanly without READY=1, and the store is not quarantined.
func TestStopDuringRecovery(t *testing.T) {
	h := newHost(t)
	gate := make(chan struct{})
	h.hooks.BeforeRecover = func(ctx context.Context) {
		close(gate)
		<-ctx.Done()
	}
	r := h.start()
	<-gate
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if h.notes.has(systemd.NotifyReady) {
		t.Fatal("READY=1 sent")
	}
	if !strings.Contains(h.logs.text(), "recovery cancelled by a stop") {
		t.Fatalf("logs:\n%s", h.logs.text())
	}
	h.hooks.BeforeRecover = nil
	if _, state := h.run(); state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("restart state = %v", state)
	}
}

// TestSingleWriter: a second agent on the same state directory exits while
// the first runs.
func TestSingleWriter(t *testing.T) {
	h := newHost(t)
	h.run()
	r2 := h.start()
	select {
	case err := <-r2.done:
		if !errors.Is(err, journal.ErrLocked) {
			t.Fatalf("second agent: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second agent did not exit")
	}
}

// TestStoreNeedsNewerAgent: a store written by a newer agent that this one
// cannot read refuses the start and names the version it needs.
func TestStoreNeedsNewerAgent(t *testing.T) {
	h := newHost(t)
	r0, _ := h.run()
	if err := r0.stop(); err != nil {
		t.Fatal(err)
	}
	// A newer agent migrates the store to a schema that needs readers newer
	// than this binary.
	st, err := opstore.Open(context.Background(), opstore.Options{Executor: none.Executor{}, Journal: journal.Options{
		Dir: h.stateDir, SchemaVersion: journal.SchemaVersion + 1, Migrations: []journal.Migration{{ToVersion: journal.SchemaVersion + 1, MinReader: journal.ReaderMax + 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EstablishContinuity(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	r := h.start()
	select {
	case err := <-r.done:
		if !errors.Is(err, journal.ErrSchemaTooNew) || !strings.Contains(err.Error(), fmt.Sprint(journal.ReaderMax+1)) {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the agent did not refuse\n%s", h.logs.text())
	}
}

// TestStartRefusals: every start-up check refuses the start with its reason,
// and a refused start creates no store.
func TestStartRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		edit   func(h *host)
		reason string
	}{
		"runs as root": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) { f.UID = 0 })
		}, hostcheck.ReasonRunsAsRoot},
		"not in the initial PID namespace": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) { f.Namespaces["pid"] = 1 })
		}, hostcheck.ReasonUnsupportedHost},
		"no cgroup v2": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) { f.Mounts[1].FSType = "cgroup" })
		}, hostcheck.ReasonUnsupportedHost},
		"state directory owned by another user": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) {
				f.Files[h.stateDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o700, UID: 0}
			})
		}, hostcheck.ReasonStateDirInsecure},
		"state on tmpfs": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) {
				f.Mounts = append(f.Mounts, hostcheck.Mount{MountPoint: filepath.Dir(h.stateDir), FSType: "tmpfs", Source: "tmpfs"})
			})
		}, hostcheck.ReasonStateNotPersistent},
		"provisioning marker missing": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) { delete(f.Content, filepath.Join(h.stateDir, hostcheck.HostMarkerFile)) })
		}, hostcheck.ReasonStateNotPersistent},
		"host key copy in the agent's key directory": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) {
				f.Files[filepath.Join(h.cfg.PKIDir(), "host.key")] = hostcheck.FileInfo{Mode: 0o600, UID: testUID}
			})
		}, hostcheck.ReasonHostKeyReadable},
		"host key readable by the agent": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) { f.ReadablePaths[h.cfg.SignerKeyFile] = true })
		}, hostcheck.ReasonHostKeyReadable},
		"signer socket directory open to others": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) {
				f.Files[filepath.Dir(h.cfg.SignerSocket)] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o755, UID: testUID}
			})
		}, hostcheck.ReasonSignerSocketInsecure},
		"signer is the agent's own user": {func(h *host) { h.refuseSameUser = true }, daemon.ReasonSignerUnavailable},
		"signer is root": {func(h *host) {
			h.signerUID = 0
			h.probe.Edit(func(f *hostchecktest.Fake) {
				f.Files[filepath.Dir(h.cfg.SignerSocket)] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o750, UID: 0}
			})
		}, daemon.ReasonSignerUnavailable},
		"signer of another user": {func(h *host) {
			h.signerUID = testUID + 1
			h.probe.Edit(func(f *hostchecktest.Fake) {
				f.Files[filepath.Dir(h.cfg.SignerSocket)] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o750, UID: h.signerUID}
			})
		}, daemon.ReasonSignerUnavailable},
		"signer serves another key": {func(h *host) {
			h.signer.stop()
			h.signer = startSigner(h.t, h.signer.socket, hostidentitytest.NewKey(h.t))
		}, daemon.ReasonIdentityInvalid},
		"trust configuration writable": {func(h *host) {
			h.probe.Edit(func(f *hostchecktest.Fake) { f.WritablePaths[h.trustDir] = true })
		}, hostcheck.ReasonTrustConfigInsecure},
		"grant anchor is the host identity issuer": {func(h *host) {
			if err := os.WriteFile(filepath.Join(h.trustDir, trust.FileGrantAnchors), h.pki.HostCA.PEM, 0o600); err != nil {
				h.t.Fatal(err)
			}
		}, daemon.ReasonIdentityInvalid},
		"host certificate readable by group, seen on the open file": {func(h *host) {
			if err := os.Chmod(filepath.Join(h.cfg.PKIDir(), trust.FileHostCert), 0o640); err != nil {
				h.t.Fatal(err)
			}
		}, daemon.ReasonIdentityInvalid},
		"trust file replaced by a symbolic link": {func(h *host) {
			p := filepath.Join(h.trustDir, trust.FileHostCA)
			if err := os.Rename(p, p+".real"); err != nil {
				h.t.Fatal(err)
			}
			if err := os.Symlink(p+".real", p); err != nil {
				h.t.Fatal(err)
			}
		}, daemon.ReasonIdentityInvalid},
		"trust file writable by group, seen on the open file": {func(h *host) {
			if err := os.Chmod(filepath.Join(h.trustDir, trust.FileProviderSANs), 0o664); err != nil {
				h.t.Fatal(err)
			}
		}, daemon.ReasonIdentityInvalid},
		"host certificate from another issuer": {func(h *host) {
			csr := hostidentitytest.NewCSR(h.t, h.pki.HostKey, hostidentitytest.CSROptions{URIs: hostidentity.RequestURIs(h.pki.HostURI, h.pki.Record)})
			if err := os.WriteFile(filepath.Join(h.cfg.PKIDir(), trust.FileHostCert), hostidentitytest.NewCA(h.t).Issue(h.t, csr, hostidentitytest.IssueOptions{}), 0o600); err != nil {
				h.t.Fatal(err)
			}
		}, daemon.ReasonIdentityInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			tc.edit(h)
			r := h.start()
			select {
			case err := <-r.done:
				if !errors.Is(err, daemon.ErrStartRefused) || !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("Run = %v, want a refusal with %s", err, tc.reason)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the agent did not refuse")
			}
			if exists(t, filepath.Join(h.stateDir, journal.StoreFile)) {
				t.Fatal("a refused start created a store")
			}
		})
	}
}

// TestQuarantinedStoreServesReads: a store whose anchor log is gone is
// quarantined: the agent still runs and notifies READY=1 with the reason,
// answers reads with a signed QUARANTINED answer and refuses every mutation.
func TestQuarantinedStoreServesReads(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.stateDir, journal.AnchorFile)); err != nil {
		t.Fatal(err)
	}
	r, state := h.run()
	if state != providerv1.AgentState_AGENT_STATE_QUARANTINED || !h.notes.has(systemd.NotifyReady+"\nSTATUS=AGENT_STATE_QUARANTINED (anchor_missing)") {
		t.Fatalf("state = %v, notifications %v", state, h.notes.snapshot())
	}
	a := r.hello()
	if a == nil || a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_QUARANTINED ||
		a.GetContext().GetStateReason() != string(journal.LostAnchorMissing) {
		t.Fatalf("answer = %v", a)
	}
	if code, reason := r.create(); code != "FailedPrecondition" || reason != opstore.ReasonQuarantined {
		t.Fatalf("Create = %s %s", code, reason)
	}
}

// TestMissingClockFloorQuarantines: an enrolled store with no clock floor
// record is quarantined (inconsistent_store), never given a new floor.
func TestMissingClockFloorQuarantines(t *testing.T) {
	h := newHost(t)
	// A store that was enrolled without ever committing a floor.
	st, err := opstore.Open(context.Background(), opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: none.Executor{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	id := h.pki.Record
	if err := st.Enrol(ctx, opstore.Enrolment{ID: id.EnrolmentID, ProviderUID: id.ProviderUID, HostID: h.pki.HostURI,
		InstallID: id.InstallID, StartEpoch: 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	_, state := h.run()
	if !strings.Contains(h.logs.text(), "clock floor cannot be read") {
		t.Fatalf("the quarantine is not the floor's\n%s", h.logs.text())
	}
	if state != providerv1.AgentState_AGENT_STATE_QUARANTINED ||
		!h.notes.has(systemd.NotifyReady+"\nSTATUS=AGENT_STATE_QUARANTINED ("+string(journal.LostInconsistentStore)+")") {
		t.Fatalf("state = %v, notifications %v", state, h.notes.snapshot())
	}
}

// TestClockFloorFileIsIgnored: a clock.floor file left in the state directory
// is not read, and not written: the floor is the store's record.
func TestClockFloorFileIsIgnored(t *testing.T) {
	h := newHost(t)
	stale := filepath.Join(h.stateDir, "clock.floor")
	if err := os.WriteFile(stale, []byte("not a floor"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, state := h.run()
	if state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("state = %v\n%s", state, h.logs.text())
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(stale); err != nil || string(b) != "not a floor" {
		t.Fatalf("the stale file was touched: %q %v", b, err)
	}
}

// TestClockFloorRollbackIsStoreRollback: restoring an older copy of the store
// (the only place the floor lives) is detected by the store's anchor checks:
// the floor cannot be rolled back without rolling back the store.
func TestClockFloorRollbackIsStoreRollback(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(h.stateDir, journal.StoreFile)
	older, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Another run raises and commits the floor again.
	h.boot.start = h.boot.start.Add(-2 * time.Minute) // within the floor's ahead bound
	r, state := h.run()
	if state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("state = %v", state)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, older, 0o600); err != nil {
		t.Fatal(err)
	}
	r, state = h.run()
	if state != providerv1.AgentState_AGENT_STATE_QUARANTINED {
		t.Fatalf("state = %v\n%s", state, h.logs.text())
	}
	if a := r.hello(); a.GetContext().GetStateReason() != string(journal.LostRolledBack) {
		t.Fatalf("answer = %v", a)
	}
}

// TestClockFloorStoreNeverGoesDown: the store refuses to commit a floor below
// the stored one.
func TestClockFloorStoreNeverGoesDown(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: dir}, Executor: none.Executor{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.EstablishContinuity(ctx); err != nil {
		t.Fatal(err)
	}
	fs := daemon.NewFloorStore(st)
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	if err := fs.Save(ctx, clockfloor.State{Floor: t0, BootID: "b", BootTime: time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := fs.Save(ctx, clockfloor.State{Floor: t0.Add(-time.Second), BootID: "b", BootTime: 2 * time.Second}); err == nil {
		t.Fatal("a lower floor was committed")
	}
	if err := fs.Save(ctx, clockfloor.State{Floor: t0, BootID: "b", BootTime: 3 * time.Second}); err != nil {
		t.Fatalf("an equal floor: %v", err)
	}
	got, err := fs.Load(ctx)
	if err != nil || !got.Floor.Equal(t0) || got.BootTime != 3*time.Second {
		t.Fatalf("stored = %+v, %v", got, err)
	}
}

// TestNotEnrolledAnswersUnsigned: without the development enrolment a fresh
// store stays unenrolled; reads answer without a signed answer.
func TestNotEnrolledAnswersUnsigned(t *testing.T) {
	h := newHost(t)
	h.dev = nil
	r, state := h.run()
	if state != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("state = %v", state)
	}
	if a := r.hello(); a != nil {
		t.Fatalf("signed answer without an enrolment: %v", a)
	}
}

// TestReload: SIGHUP reloads the trust configuration and key material; a
// certificate for another enrolment is refused and the running one stays.
func TestReload(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	r.hello()
	// A certificate for another enrolment: refused, answers still name the
	// running enrolment.
	other := h.pki.Record
	other.EnrolmentID = make([]byte, 16)
	cert := h.pki.IssueHost(t, h.pki.HostKey, hostidentity.RequestURIs(h.pki.HostURI, other), hostidentitytest.IssueOptions{})
	if err := os.WriteFile(filepath.Join(h.cfg.PKIDir(), trust.FileHostCert), cert, 0o600); err != nil {
		t.Fatal(err)
	}
	r.signals <- syscall.SIGHUP
	waitLog(t, h, "reload refused")
	if !h.notes.has(systemd.NotifyReady + "\nSTATUS=AGENT_STATE_READY; reload refused: " + daemon.ReasonEnrolmentChanged) {
		t.Fatalf("no reload refusal status: %v", h.notes.snapshot())
	}
	if a := r.hello(); a == nil || string(a.GetContext().GetEnrolmentId()) != string(h.pki.Record.EnrolmentID) {
		t.Fatalf("answer after a refused reload = %v", a)
	}
	// An allow-list that drops the Provider takes effect.
	if err := os.WriteFile(filepath.Join(h.cfg.PKIDir(), trust.FileHostCert), h.pki.HostCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.trustDir, trust.FileProviderSANs), []byte("spiffe://example.test/virtrigaud/provider/infra/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.signals <- syscall.SIGHUP
	waitLog(t, h, "credentials reloaded")
	if !h.notes.has(systemd.NotifyReloading) {
		t.Fatal("RELOADING=1 not sent")
	}
	if code, _ := r.create(); code == "Unimplemented" {
		t.Fatal("a Provider removed from the allow-list still reaches the agent")
	}
}

func waitLog(t *testing.T, h *host, sub string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.logs.text(), sub) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in the logs:\n%s", sub, h.logs.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestLogsCarryNoKeyMaterial: nothing the agent logs contains the host key.
func TestLogsCarryNoKeyMaterial(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	r.hello()
	_ = r.stop()
	der, err := x509.MarshalPKCS8PrivateKey(h.pki.HostKey)
	if err != nil {
		t.Fatal(err)
	}
	body := base64.StdEncoding.EncodeToString(der)
	if strings.Contains(h.logs.text(), body[:32]) || strings.Contains(h.logs.text(), fmt.Sprintf("%x", rawKey(t, h))) {
		t.Fatal("the logs contain key material")
	}
}

// TestAgentHoldsNoKeyFile: the agent's key material directory holds the
// certificate only; the key is in the signer's hands.
func TestAgentHoldsNoKeyFile(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	r.hello()
	_ = r.stop()
	names, err := os.ReadDir(h.cfg.PKIDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n.Name() != trust.FileHostCert {
			t.Errorf("the agent's key material directory holds %s", n.Name())
		}
	}
}

// TestSignerRestartWhileRunning: the signer restarting under a running agent
// breaks signing only while it is down: answers are unsigned then (never
// signed by the agent), and signed again afterwards, with no agent restart.
func TestSignerRestartWhileRunning(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if r.hello() == nil {
		t.Fatal("no signed answer with the signer up")
	}
	socket := h.signer.socket
	h.signer.stop()
	if resp, err := agentv1.NewAgentServiceClient(r.conn()).Hello(callCtx(t),
		&agentv1.HelloRequest{AnswerNonce: bytes.Repeat([]byte{2}, 16), CallerApiVersion: 1}); err == nil && resp.GetHostAnswer() != nil {
		t.Fatal("a signed answer with the signer down")
	}
	h.signer = startSigner(t, socket, h.pki.HostKey)
	if r.hello() == nil {
		t.Fatal("no signed answer after the signer restarted")
	}
}

// TestReloadReachesTheSigner: a reload fetches the signer's key again, and a
// reload with the signer down is refused with the running credentials kept.
func TestReloadReachesTheSigner(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	r.hello()
	h.signer.stop()
	r.signals <- syscall.SIGHUP
	waitLog(t, h, "reload refused")
	if !strings.Contains(h.logs.text(), "reach the signer") {
		t.Fatalf("the refusal does not name the signer:\n%s", h.logs.text())
	}
}

// rawKey returns the host key's raw private scalar.
func rawKey(t *testing.T, h *host) []byte {
	t.Helper()
	b, err := h.pki.HostKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestReloadKeepsQuarantineInStatus: the status sent after a reload still
// names the quarantine of the store.
func TestReloadKeepsQuarantineInStatus(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.stateDir, journal.AnchorFile)); err != nil {
		t.Fatal(err)
	}
	r, _ = h.run()
	r.signals <- syscall.SIGHUP
	waitLog(t, h, "credentials reloaded")
	want := systemd.NotifyReady + "\nSTATUS=AGENT_STATE_QUARANTINED (anchor_missing); credentials reloaded"
	deadline := time.Now().Add(5 * time.Second)
	for !h.notes.has(want) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in %v", want, h.notes.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestScrubFindsDamageAndReflectsItInStatus: the agent scrubs its store
// periodically; damage to a record nothing reads is found, the store is
// quarantined, and the service status says so.
func TestScrubFindsDamageAndReflectsItInStatus(t *testing.T) {
	h := newHost(t)
	h.cfg.ScrubInterval = 20 * time.Millisecond
	r, state := h.run()
	if state == providerv1.AgentState_AGENT_STATE_QUARANTINED {
		t.Fatalf("quarantined at start\n%s", h.logs.text())
	}
	waitLog(t, h, "journal scrub passed")

	// Flip a byte of the recorded live-state digest in every page that holds
	// it (a bbolt leaf stores a value right after its key), in the store
	// file under bbolt's mapping.
	path := filepath.Join(h.stateDir, journal.StoreFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	needle := []byte("state_digest")
	hits := 0
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; ; {
		i := bytes.Index(raw[off:], needle)
		if i < 0 {
			break
		}
		hits++
		at := off + i + len(needle)
		if _, err := f.WriteAt([]byte{raw[at] ^ 1}, int64(at)); err != nil {
			t.Fatal(err)
		}
		off += i + len(needle)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if hits == 0 {
		t.Fatal("the live-state digest is not in the store file")
	}
	want := systemd.NotifyReady + "\nSTATUS=AGENT_STATE_QUARANTINED (journal_corrupt)"
	deadline := time.Now().Add(5 * time.Second)
	for !h.notes.has(want) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in %v\n%s", want, h.notes.snapshot(), h.logs.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitLog(t, h, "journal scrub failed")
	if a := r.hello(); a.GetContext().GetStateReason() != string(journal.LostJournalCorrupt) {
		t.Fatalf("answer = %v", a)
	}
}

// TestClockFloorAheadReportsAndRefusesMutations: the host clock is rolled back
// far beyond the floor's bound (and beyond a certificate's lifetime). The
// floor is then ahead: reads still answer (UNATTESTED, no state reason), the
// service status and the log name the cause, and mutating calls are refused
// UNATTESTED with the cause in the ErrorInfo metadata.
func TestClockFloorAheadReportsAndRefusesMutations(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if a := r.hello(); a == nil || a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("answer before the rollback = %v", a)
	}
	h.wallOff.Store(int64(-400 * 24 * time.Hour))
	a := r.hello()
	if a == nil {
		t.Fatalf("no answer while the floor is ahead\n%s", h.logs.text())
	}
	if a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_UNATTESTED || a.GetContext().GetStateReason() != "" {
		t.Fatalf("answer = %v %q", a.GetContext().GetAgentState(), a.GetContext().GetStateReason())
	}
	if code, reason := r.create(); code != "Unavailable" || reason != opstore.ReasonUnattested {
		t.Fatalf("Create = %s %s", code, reason)
	}
	waitNotify := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !h.notes.has(want) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q in %v", want, h.notes.snapshot())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitNotify(systemd.NotifyReady + "\nSTATUS=AGENT_STATE_UNATTESTED (clock_floor_ahead)")
	if !strings.Contains(h.logs.text(), "clock_floor_ahead") {
		t.Fatalf("the cause is not logged\n%s", h.logs.text())
	}
	// The clock is corrected: the host is READY again.
	h.wallOff.Store(0)
	if a := r.hello(); a == nil || a.GetContext().GetAgentState() != providerv1.AgentState_AGENT_STATE_READY {
		t.Fatalf("answer after the correction = %v", a)
	}
}

// TestDrainMarksTheFloorDrained: a clean stop commits the floor marked as
// drained (the clockfloor tests cover that the next run's first write clears
// the mark).
func TestDrainMarksTheFloorDrained(t *testing.T) {
	h := newHost(t)
	r, _ := h.run()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	load := func() clockfloor.State {
		t.Helper()
		st, err := opstore.Open(context.Background(), opstore.Options{Journal: journal.Options{Dir: h.stateDir}, Executor: none.Executor{}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		s, err := daemon.NewFloorStore(st).Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if !load().Drained {
		t.Fatal("the floor was not marked drained at a clean stop")
	}
}

// TestLostJournalWithoutADatabaseStillStarts: a store whose database is gone
// (journal_missing) or unopenable (journal_corrupt) starts QUARANTINED and
// answers reads, instead of refusing to start.
func TestLostJournalWithoutADatabaseStillStarts(t *testing.T) {
	for name, tc := range map[string]struct {
		damage func(t *testing.T, path string)
		want   journal.LostReason
	}{
		"missing": {func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, journal.LostJournalMissing},
		"garbage": {func(t *testing.T, path string) {
			if err := os.WriteFile(path, bytes.Repeat([]byte("garbage!"), 1024), 0o600); err != nil {
				t.Fatal(err)
			}
		}, journal.LostJournalCorrupt},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			r, _ := h.run()
			if err := r.stop(); err != nil {
				t.Fatal(err)
			}
			tc.damage(t, filepath.Join(h.stateDir, journal.StoreFile))
			r, state := h.run()
			if state != providerv1.AgentState_AGENT_STATE_QUARANTINED ||
				!h.notes.has(systemd.NotifyReady+"\nSTATUS=AGENT_STATE_QUARANTINED ("+string(tc.want)+")") {
				t.Fatalf("state = %v, notifications %v\n%s", state, h.notes.snapshot(), h.logs.text())
			}
			// Reads are served: Hello succeeds (unsigned, the store has no
			// enrolment to read) and a mutation is refused as quarantined.
			if _, err := agentv1.NewAgentServiceClient(r.conn()).Hello(callCtx(t), &agentv1.HelloRequest{AnswerNonce: bytes.Repeat([]byte{1}, 16), CallerApiVersion: 1}); err != nil {
				t.Fatalf("Hello = %v", err)
			}
			if code, reason := r.create(); code != "FailedPrecondition" || reason != opstore.ReasonQuarantined {
				t.Fatalf("Create = %s %s", code, reason)
			}
		})
	}
}

// TestStartWaitsForTheSigner: with the signer down at start-up the agent
// does not exit (it would exhaust its start rate limit and stay failed); it
// sends READY=1 with a status naming the wait (so the boot is not held up)
// but serves nothing, and starts when the signer comes up.
func TestStartWaitsForTheSigner(t *testing.T) {
	h := newHost(t)
	socket := h.signer.socket
	h.signer.stop()
	r := h.start()
	waitLog(t, h, "waiting for the host key signer")
	select {
	case s := <-r.started:
		t.Fatalf("the agent started (%v) without the signer", s)
	case err := <-r.done:
		t.Fatalf("the agent exited while waiting for the signer: %v", err)
	default:
	}
	// READY=1 with a status naming the wait, so that the boot and
	// systemctl start are not held up; but nothing is served: a TLS
	// handshake gets no answer.
	if !h.notes.has(systemd.NotifyReady + "\nSTATUS=waiting for the host key signer") {
		t.Fatalf("no wait status: %v", h.notes.snapshot())
	}
	d := &net.Dialer{Timeout: 300 * time.Millisecond}
	if c, err := tls.DialWithDialer(d, "tcp", r.addr, &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}); err == nil { // #nosec G402 -- a probe that must not complete
		_ = c.Close()
		t.Fatal("the agent served a handshake while waiting for the signer")
	}
	h.signer = startSigner(t, socket, h.pki.HostKey)
	select {
	case s := <-r.started:
		// The waiting status does not stand through the rest of the start-up.
		if !h.notes.has("STATUS=starting") {
			t.Fatalf("no STATUS=starting after the signer arrived: %v", h.notes.snapshot())
		}
		if s != providerv1.AgentState_AGENT_STATE_READY {
			t.Fatalf("state = %v", s)
		}
	case err := <-r.done:
		t.Fatalf("the agent exited: %v\n%s", err, h.logs.text())
	case <-time.After(10 * time.Second):
		t.Fatalf("the agent did not start once the signer was up\n%s", h.logs.text())
	}
	if r.hello() == nil {
		t.Fatal("no signed answer")
	}
	_ = r.stop()
}

// TestStopWhileWaitingForTheSigner: a stop during the wait ends the agent
// cleanly (exit status 0).
func TestStopWhileWaitingForTheSigner(t *testing.T) {
	h := newHost(t)
	h.signer.stop()
	r := h.start()
	waitLog(t, h, "waiting for the host key signer")
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}
