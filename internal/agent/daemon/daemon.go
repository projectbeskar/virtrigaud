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

// Package daemon runs the per-host agent: its start-up sequence, its gRPC
// server and its signal handling.
//
// # Start-up order
//
//  0. Host checks (package hostcheck): initial namespaces, cgroup v2, a
//     private state directory owned by the agent's user on persistent
//     storage, private key material, a trust configuration the agent cannot
//     write; then the trust configuration and the host identity are loaded
//     and the state directory's provisioning marker must name the host. Any
//     failure refuses the start; nothing on the host is changed.
//  1. The single-writer lock of the operation store: a second agent on the
//     same state directory exits.
//  2. The store opens with full fsync, a schema check (a store that needs a
//     newer agent refuses the start, naming the version) and
//  3. its local continuity checks (chain, anchor log, marker, quarantine
//     record). A store that fails them is quarantined: it answers reads and
//     refuses every mutation.
//  4. The clock floor is read and raised from the host's own clocks. A
//     floor that is missing for an enrolled store, or unreadable, quarantines
//     the store (journal_corrupt).
//  5. The server starts serving in the unattested state: reads, Hello and
//     GetHostInfo answer; every mutation is refused UNATTESTED. Nothing is
//     committed before step 6.
//  6. Continuity is established (with the local store, the checks of step
//     3). A development build enrols a fresh store here from the host
//     certificate's enrolment record and the development enrolment file.
//  7. Recovery resolves every interrupted operation by the store's recovery
//     table. This agent starts no runner for an operation left runnable.
//  8. Ready: systemd is notified (READY=1), and the trust state turns
//     READY. Mutations are still refused (Unimplemented), because this agent
//     executes no operations yet.
//
// A quarantined store also notifies READY=1, with the quarantine in STATUS=,
// so that systemd keeps the agent running to answer reads instead of
// restarting it.
//
// # Signals
//
// SIGTERM or SIGINT drains: the server stops accepting calls and waits for
// running calls up to the drain timeout, the clock floor is saved, the store
// is closed, and the process exits 0. A stop during recovery cancels it; the
// next start resumes it. SIGHUP reloads the key material and the trust
// configuration after checking them again; a reload that fails, or that
// would change the host's enrolment, keeps the running credentials.
package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/systemd"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// DefaultObserveEvery is the period at which the clock floor is raised.
const DefaultObserveEvery = 5 * time.Second

// ReasonIdentityInvalid is the start refusal reason for a trust
// configuration or host identity that does not load.
const ReasonIdentityInvalid = "IdentityInvalid"

// ErrStartRefused is wrapped by every refused start.
var ErrStartRefused = errors.New("the agent refused to start")

// FloorSources replace the clock floor's clock sources; nil fields use the
// host's own.
type FloorSources struct {
	// Wall is the wall clock.
	Wall grantverify.Clock
	// Boot reads CLOCK_BOOTTIME.
	Boot clockfloor.BootClock
	// BootID reads the boot_id.
	BootID clockfloor.BootIDSource
	// TimeNS checks the time namespace.
	TimeNS clockfloor.TimeNamespaceCheck
}

// Hooks are test hooks. Production code sets none.
type Hooks struct {
	// BeforeRecover runs just before recovery (step 7).
	BeforeRecover func(ctx context.Context)
	// Started receives the trust state once the start-up sequence ends.
	Started func(state providerv1.AgentState)
}

// Options configures Run.
//
// Probe, UID, Listener, TrustDir, TrustFileOwner, JournalFS, Floor, Hooks and
// Now are test overrides: they replace what the host itself provides, the
// agent binary never sets them, and a release build refuses to start when
// any of them is set.
type Options struct {
	// Config is the validated configuration. Required.
	Config *config.Config
	// Executor runs operations. Required.
	Executor config.Executor
	// DevEnrolment is the development enrolment stand-in; nil for none.
	DevEnrolment *config.DevEnrolment
	// Notifier receives systemd notifications; nil sends none.
	Notifier systemd.Notifier
	// Log is the agent's logger: one structured line per start-up step,
	// refusal, quarantine and operation event; never request bytes, keys or
	// secrets.
	Log logr.Logger
	// Version is the agent's build version.
	Version string
	// ObserveEvery is the clock floor's raise period; zero uses
	// DefaultObserveEvery.
	ObserveEvery time.Duration

	// Probe reads the host for the start-up checks (test override); nil
	// uses hostcheck.System.
	Probe hostcheck.Probe
	// UID is the agent user's id, the owner the state directory and the key
	// material must have (test override); nil uses the process's effective
	// uid.
	UID *uint32
	// Listener replaces listening on Config.ListenAddress (test override).
	Listener net.Listener
	// TrustDir replaces the configured trust directory, for example the
	// trust directory of a test PKI (test override).
	TrustDir string
	// TrustFileOwner replaces root as the owner the trust configuration's
	// files must have (test override).
	TrustFileOwner *uint32
	// JournalFS replaces the journal's file system (test override).
	JournalFS journal.FS
	// Floor replaces the clock floor's sources (test override).
	Floor FloorSources
	// Hooks are test hooks (test override).
	Hooks Hooks
	// Now is the clock for certificate validity at load and reload (test
	// override); nil uses time.Now.
	Now func() time.Time
}

// agentState is what the server reads about the agent.
type agentState struct {
	store *opstore.Store
	floor atomic.Pointer[clockfloor.Floor]
}

var _ server.Agent = (*agentState)(nil)

// HostView implements server.Agent.
func (a *agentState) HostView(ctx context.Context, continuitySeq uint64) (opstore.HostView, error) {
	return a.store.HostView(ctx, continuitySeq)
}

// TrustState implements server.Agent.
func (a *agentState) TrustState() (providerv1.AgentState, string) {
	st, lost := a.store.Journal().State()
	return answer.StoreState(st, lost, a.store.Recovered())
}

// ClockFloor implements server.Agent.
func (a *agentState) ClockFloor() time.Time {
	f := a.floor.Load()
	if f == nil {
		return time.Time{}
	}
	return f.Status().Floor
}

// allowTestOverrides lets this package's tests use the test overrides of
// Options in a release build; only export_test.go sets it.
var allowTestOverrides = false

// testOverrides names the Options fields that replace what the host itself
// provides. A release build refuses every one of them: the agent binary
// never sets them.
func (o Options) testOverrides() []string {
	var set []string
	for name, on := range map[string]bool{
		"Probe": o.Probe != nil, "UID": o.UID != nil, "TrustDir": o.TrustDir != "", "JournalFS": o.JournalFS != nil,
		"TrustFileOwner": o.TrustFileOwner != nil, "Listener": o.Listener != nil, "Now": o.Now != nil,
		"Floor": o.Floor.Wall != nil || o.Floor.Boot != nil || o.Floor.BootID != nil || o.Floor.TimeNS != nil, "Hooks": o.Hooks.BeforeRecover != nil || o.Hooks.Started != nil,
	} {
		if on {
			set = append(set, name)
		}
	}
	sort.Strings(set)
	return set
}

// daemon is one run of the agent.
type daemon struct {
	o   Options
	log logr.Logger
	uid uint32
	// trustOwner is the owner the trust configuration's files must have:
	// root.
	trustOwner uint32
	trustDir   string
	creds      *server.CredentialStore
	notifier   systemd.Notifier
}

// Run runs the agent until ctx ends or a stop signal arrives on signals, and
// returns nil after a graceful stop. It returns an error wrapping
// ErrStartRefused when a start-up check refuses, and an error when the store
// cannot be opened (another agent holds it, or it needs a newer agent).
func Run(ctx context.Context, o Options, signals <-chan os.Signal) error {
	if o.Config == nil || o.Executor == nil {
		return errors.New("run agent: configuration and executor are required")
	}
	d := &daemon{o: o, log: o.Log, notifier: o.Notifier}
	if d.o.Probe == nil {
		d.o.Probe = hostcheck.System{}
	}
	if d.notifier == nil {
		d.notifier = systemd.SocketNotifier{}
	}
	if d.o.Now == nil {
		d.o.Now = time.Now
	}
	if d.o.ObserveEvery == 0 {
		d.o.ObserveEvery = DefaultObserveEvery
	}
	if d.o.JournalFS == nil {
		d.o.JournalFS = journal.OSFS{}
	}
	if set := o.testOverrides(); len(set) > 0 && !config.DevBuild && !allowTestOverrides {
		return fmt.Errorf("%w: %v can be set only in a development build", ErrStartRefused, set)
	}
	d.uid = d.o.Probe.EUID()
	if o.UID != nil {
		d.uid = *o.UID
	}
	if o.TrustFileOwner != nil {
		d.trustOwner = *o.TrustFileOwner
	}
	var err error
	if d.trustDir, err = o.Config.TrustDir(); err != nil {
		return fmt.Errorf("%w: %v", ErrStartRefused, err)
	}
	if o.TrustDir != "" {
		d.trustDir = o.TrustDir
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	reload := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case sig, ok := <-signals:
				if !ok {
					return
				}
				if sig == syscall.SIGHUP {
					select {
					case reload <- struct{}{}:
					default:
					}
					continue
				}
				d.log.Info("stop requested", "signal", sig.String())
				stop()
			}
		}
	}()
	return d.run(runCtx, reload)
}

// run is the start-up sequence and the main loop.
func (d *daemon) run(ctx context.Context, reload <-chan struct{}) error {
	// Step 0: host checks, trust configuration, identity.
	creds, err := d.loadCredentials()
	if err != nil {
		reason := ReasonIdentityInvalid
		if f, ok := hostcheck.AsFailure(err); ok {
			reason = f.Reason
		}
		d.log.Error(err, "start refused", "reason", reason)
		return fmt.Errorf("%w: %s: %v", ErrStartRefused, reason, err)
	}
	d.creds = server.NewCredentialStore(creds)
	d.log.Info("host checks passed", "host", creds.Identity.HostURI)

	// Steps 1 to 3: single writer, open, local continuity checks.
	st, err := opstore.Open(ctx, opstore.Options{
		Journal:  journal.Options{Dir: d.o.Config.StateDir, FS: d.o.JournalFS, Events: d.journalEvent},
		Executor: d.o.Executor,
		Audit:    d.auditEvent,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case errors.Is(err, journal.ErrLocked):
			d.log.Error(err, "another agent holds the state directory")
		case errors.Is(err, journal.ErrSchemaTooNew):
			d.log.Error(err, "the operation store needs a newer agent")
		default:
			d.log.Error(err, "the operation store cannot be opened")
		}
		return fmt.Errorf("open the operation store: %w", err)
	}
	agent := &agentState{store: st}
	closeStore := func() {
		if err := st.Close(); err != nil {
			d.log.Error(err, "close the operation store")
		}
	}
	if js, lost := st.Journal().State(); js == journal.StateLost {
		d.log.Error(nil, "the operation store is quarantined; it answers reads and refuses every mutation", "reason", string(lost))
	}

	// Step 4: clock floor.
	if err := d.openFloor(ctx, agent); err != nil {
		closeStore()
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	// Step 5: serve, unattested.
	schema := uint32(0)
	if info, err := st.Journal().Schema(ctx); err == nil {
		schema = info.SchemaVersion
	}
	srv, err := server.New(server.Options{Agent: agent, Credentials: d.creds, Version: d.o.Version,
		JournalSchemaVersion: schema, Log: d.log.WithName("server")})
	if err != nil {
		closeStore()
		return err
	}
	lis := d.o.Listener
	if lis == nil {
		if lis, err = net.Listen("tcp", d.o.Config.ListenAddress); err != nil {
			closeStore()
			return fmt.Errorf("listen on %s: %w", d.o.Config.ListenAddress, err)
		}
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()
	d.log.Info("serving", "address", lis.Addr().String())

	drain := func() {
		_ = d.notifier.Notify(systemd.NotifyStopping)
		dctx, cancel := context.WithTimeout(context.Background(), d.o.Config.DrainTimeout)
		defer cancel()
		srv.Shutdown(dctx)
		if f := agent.floor.Load(); f != nil {
			if err := f.Flush(dctx); err != nil {
				d.log.Error(err, "save the clock floor")
			}
		}
		closeStore()
		d.log.Info("stopped")
	}

	// Steps 6 to 8.
	d.establish(ctx, st)
	if ctx.Err() == nil {
		state, reason := agent.TrustState()
		if err := d.notifier.Notify(systemd.NotifyReady + "\nSTATUS=" + trustStatus(agent)); err != nil {
			d.log.Error(err, "notify systemd")
		}
		d.log.Info("started", "state", state.String(), "reason", reason)
		if d.o.Hooks.Started != nil {
			d.o.Hooks.Started(state)
		}
	}

	ticker := time.NewTicker(d.o.ObserveEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			drain()
			return nil
		case err := <-serveErr:
			drain()
			return err
		case <-reload:
			_ = d.notifier.Notify(systemd.NotifyReloading)
			if err := d.reload(); err != nil {
				reason := reloadRefusalReason(err)
				d.log.Error(err, "reload refused; the running credentials stay in use", "reason", reason)
				_ = d.notifier.Notify(systemd.NotifyReady + "\nSTATUS=" + trustStatus(agent) + "; reload refused: " + reason)
			} else {
				d.log.Info("credentials reloaded")
				_ = d.notifier.Notify(systemd.NotifyReady + "\nSTATUS=" + trustStatus(agent) + "; credentials reloaded")
			}
		case <-ticker.C:
			if f := agent.floor.Load(); f != nil {
				if err := f.Observe(ctx); err != nil {
					d.log.Error(err, "raise the clock floor")
				}
			}
		}
	}
}

// loadCredentials runs the start-up host checks and loads the trust
// configuration and the host identity.
func (d *daemon) loadCredentials() (*server.Credentials, error) {
	cfg := d.o.Config
	if err := hostcheck.Run(d.o.Probe, hostcheck.Config{StateDir: cfg.StateDir, PKIDir: cfg.PKIDir(),
		TrustDir: d.trustDir, UID: d.uid}); err != nil {
		return nil, err
	}
	tc, err := trust.LoadConfig(d.trustDir, trust.TrustFileReader(d.trustOwner))
	if err != nil {
		return nil, err
	}
	id, err := trust.LoadIdentity(cfg.PKIDir(), tc, d.o.Now(), trust.KeyFileReader(d.uid))
	if err != nil {
		return nil, err
	}
	if err := hostcheck.CheckHostMarker(d.o.Probe, cfg.StateDir, id.HostURI, d.uid); err != nil {
		return nil, err
	}
	return &server.Credentials{Identity: id, Trust: tc}, nil
}

// reload checks and loads the key material and trust configuration again,
// and replaces the running credentials when they name the same enrolment.
func (d *daemon) reload() error {
	cfg := d.o.Config
	if err := hostcheck.CheckPKI(d.o.Probe, cfg.PKIDir(), d.uid); err != nil {
		return err
	}
	if err := hostcheck.CheckTrustDir(d.o.Probe, d.trustDir); err != nil {
		return err
	}
	tc, err := trust.LoadConfig(d.trustDir, trust.TrustFileReader(d.trustOwner))
	if err != nil {
		return err
	}
	id, err := trust.LoadIdentity(cfg.PKIDir(), tc, d.o.Now(), trust.KeyFileReader(d.uid))
	if err != nil {
		return err
	}
	if !id.SameEnrolment(d.creds.Load().Identity) {
		return errEnrolmentChanged
	}
	d.creds.Store(&server.Credentials{Identity: id, Trust: tc})
	return nil
}

// trustStatus renders the agent's trust state for the service status, for
// example "AGENT_STATE_QUARANTINED (rolled_back)", so that no status line
// hides a quarantine.
func trustStatus(a *agentState) string {
	state, reason := a.TrustState()
	if reason == "" {
		return state.String()
	}
	return state.String() + " (" + reason + ")"
}

// ReasonEnrolmentChanged is the reload refusal reason for a certificate that
// names another host or enrolment.
const ReasonEnrolmentChanged = "EnrolmentChanged"

// errEnrolmentChanged refuses a reload that would change the enrolment.
var errEnrolmentChanged = errors.New("the reloaded certificate names another host or enrolment; enrolling again is not a reload")

// reloadRefusalReason is the one-word reason of a refused reload, for the
// service status: never a path or a secret.
func reloadRefusalReason(err error) string {
	if f, ok := hostcheck.AsFailure(err); ok {
		return f.Reason
	}
	if errors.Is(err, errEnrolmentChanged) {
		return ReasonEnrolmentChanged
	}
	return ReasonIdentityInvalid
}

// openFloor opens the clock floor (step 4). A floor that cannot be read or
// written quarantines the store; a clock source the floor cannot use refuses
// the start.
func (d *daemon) openFloor(ctx context.Context, agent *agentState) error {
	firstUse := false
	if v, err := agent.store.HostView(ctx, 0); err == nil {
		firstUse = v.LastStartEpoch == 0
	}
	f, err := clockfloor.Open(ctx, clockfloor.Config{
		Store: floorFile{fsys: d.o.JournalFS, dir: d.o.Config.StateDir}, FirstUse: firstUse,
		Wall: d.o.Floor.Wall, Boot: d.o.Floor.Boot, BootID: d.o.Floor.BootID, TimeNS: d.o.Floor.TimeNS,
	})
	switch {
	case err == nil:
		agent.floor.Store(f)
		return nil
	case errors.Is(err, clockfloor.ErrFloorMissing), errors.Is(err, clockfloor.ErrCorrupt), errors.Is(err, errFloorUnreadable):
		d.log.Error(err, "the clock floor cannot be read; the store is quarantined")
		agent.store.Journal().Quarantine(journal.LostJournalCorrupt, "clock floor unreadable")
		return nil
	case errors.Is(err, errFloorWrite):
		d.log.Error(err, "the clock floor cannot be written; the store is quarantined")
		agent.store.Journal().Quarantine(journal.LostIOError, "clock floor not written")
		return nil
	}
	d.log.Error(err, "start refused: the host's clocks cannot keep a floor", "reason", hostcheck.ReasonUnsupportedHost)
	return fmt.Errorf("%w: %s: %v", ErrStartRefused, hostcheck.ReasonUnsupportedHost, err)
}

// establish runs steps 6 and 7: continuity, the development enrolment
// stand-in, and recovery.
func (d *daemon) establish(ctx context.Context, st *opstore.Store) {
	if js, _ := st.Journal().State(); js != journal.StateUnattested {
		return
	}
	if err := st.EstablishContinuity(ctx); err != nil {
		d.log.Error(err, "continuity not established")
		return
	}
	d.enrol(ctx, st)
	if d.o.Hooks.BeforeRecover != nil {
		d.o.Hooks.BeforeRecover(ctx)
	}
	if ctx.Err() != nil {
		d.log.Info("recovery cancelled by a stop; the next start resumes it")
		return
	}
	refs, err := st.Recover(ctx)
	if err != nil {
		if ctx.Err() != nil {
			d.log.Info("recovery cancelled by a stop; the next start resumes it")
			return
		}
		d.log.Error(err, "recovery failed")
		return
	}
	if len(refs) > 0 {
		d.log.Info("operations remain runnable; this agent starts no runner", "count", len(refs))
	}
}

// enrol is the development enrolment stand-in: a fresh store is enrolled with
// the host certificate's enrolment record and the development enrolment
// file's start epoch. A store with an enrolment is never changed; when its
// active enrolment is not the certificate's, answers stay unsigned.
func (d *daemon) enrol(ctx context.Context, st *opstore.Store) {
	id := d.creds.Load().Identity
	v, err := st.HostView(ctx, 0)
	if err != nil {
		d.log.Error(err, "read the store's enrolment")
		return
	}
	switch {
	case v.Enrolment != nil:
		if !bytes.Equal(v.Enrolment.ID, id.Record.EnrolmentID) ||
			v.Enrolment.ProviderUID != id.Record.ProviderUID || v.Enrolment.HostID != id.HostID() {
			d.log.Error(nil, "the store's active enrolment is not the one the host certificate names; answers stay unsigned until the host is enrolled again")
		}
	case v.LastStartEpoch != 0:
		d.log.Error(nil, "the store has no active enrolment; answers stay unsigned until the host is enrolled again")
	case d.o.DevEnrolment == nil:
		d.log.Info("the store is not enrolled; answers stay unsigned")
	default:
		err := st.Enrol(ctx, opstore.Enrolment{
			ID: id.Record.EnrolmentID, ProviderUID: id.Record.ProviderUID, HostID: id.HostID(),
			InstallID: id.Record.InstallID, StartEpoch: d.o.DevEnrolment.StartEpoch, Predecessors: d.o.DevEnrolment.Predecessors,
		})
		if err != nil {
			d.log.Error(err, "development enrolment failed")
			return
		}
		d.log.Info("enrolled from the host certificate (development stand-in)",
			"enrolment", hex.EncodeToString(id.Record.EnrolmentID), "startEpoch", d.o.DevEnrolment.StartEpoch)
	}
}

// journalEvent logs a journal event.
func (d *daemon) journalEvent(ev journal.Event) {
	d.log.Error(nil, "operation store event", "kind", ev.Kind, "reason", string(ev.Reason), "seq", ev.Seq, "detail", ev.Detail)
}

// auditEvent logs an operation store event. It holds identity fields only.
func (d *daemon) auditEvent(ev opstore.AuditEvent) {
	d.log.Info("operation event", "event", ev.Event, "op", hex.EncodeToString(ev.Ref.OpID),
		"enrolment", hex.EncodeToString(ev.Ref.EnrolmentID), "vm", ev.VMUID, "kind", ev.Kind.String(),
		"actor", ev.Actor, "reason", ev.Reason, "state", ev.State.String())
}
