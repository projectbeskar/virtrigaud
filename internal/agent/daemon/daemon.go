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
//     the store (inconsistent_store: a state file beside a store that
//     verifies is gone or damaged, like a missing store marker).
//  5. The server starts serving in the unattested state: reads, Hello and
//     GetHostInfo answer; every mutation is refused UNATTESTED. Nothing is
//     committed before step 6.
//  6. Continuity is established (with the local store, the checks of step
//     3). The store's active enrolment is brought to the one the host
//     certificate carries (syncEnrolment): a fresh store is enrolled, and a
//     store with another or no active enrolment is re-enrolled, which
//     freezes the previous enrolment's open operations first. A release
//     build does neither of the certificate-derived steps: it enrols a fresh
//     store only from a development enrolment (never set outside tests), and
//     otherwise leaves answers unsigned.
//  7. Recovery resolves every interrupted operation by the store's recovery
//     table. A development build starts a runner for every operation that
//     recovery left runnable, so that no operation stays running without
//     one.
//  8. Ready: systemd is notified (READY=1), and the trust state turns
//     READY. A development build then serves the operation path: it
//     verifies each mutating call's operation grant, has the operation store
//     admit it, runs it through the configured executor and answers
//     TaskStatus and Describe from the store (see package server). A release
//     build refuses every mutation (Unimplemented).
//
// A quarantined store also notifies READY=1, with the quarantine in STATUS=,
// so that systemd keeps the agent running to answer reads instead of
// restarting it.
//
// # Scrub
//
// While the agent runs, the operation store is scrubbed every
// --scrub-interval (default one hour; it cannot be switched off, because the
// scrub is what finds a damaged record that nothing reads). A scrub reads
// every record, compares their digest sum with the store's recorded live-state
// digest and verifies the chain's commitment to it. One goroutine runs it, so
// scrubs never overlap, and it stops with the agent's context. A scrub that
// finds damage quarantines the store (journal_corrupt): the loss is logged and
// the service status shows it, like any other quarantine.
//
// # Signals
//
// SIGTERM or SIGINT drains: the server stops accepting calls and waits for
// running calls up to the drain timeout, the clock floor is saved, the store
// is closed, and the process exits 0. A stop during recovery cancels it; the
// next start resumes it. SIGHUP reloads the key material and the trust
// configuration after checking them again; a reload that fails, or that
// names another host, or a new enrolment of this host that the store refuses
// (an enrolment id it has used before), keeps the running credentials. A
// certificate that names a new enrolment of this host re-enrols the store
// first, then replaces the credentials.
package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/config"
	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/runner"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/signer"
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

// ReasonSignerUnavailable is the start refusal reason for a signer process
// that cannot be reached, is not the signer's user, or does not serve a key
// of the host certificate. The agent cannot sign without it and starts
// nothing.
const ReasonSignerUnavailable = "SignerUnavailable"

// signerPollEvery is the pause between start-up attempts to reach the
// signer. The start waits for the signer without a limit, in a not-ready
// state that the service status names, because the signer is a separate unit
// that may be down for as long as it takes to restart or repair it, and an
// agent that exited instead would exhaust its start rate limit and stay
// failed. A variable so that this package's tests can shorten it.
var signerPollEvery = 2 * time.Second

// signerWaitLogEvery is the shortest interval between two log lines about a
// signer that is not there yet.
const signerWaitLogEvery = 30 * time.Second

// signerWaitStatus is the notification while the start waits for the
// signer: READY=1 with a status line. The wait can last as long as the signer
// is down, and an agent that sent nothing would hold up the boot
// (multi-user.target) and `systemctl start` for as long. Nothing is served
// while it waits: the agent has not opened its listener or its store, so
// "ready" here means the unit has started and is waiting, never that it
// answers; every call fails (connection refused) until the signer arrives,
// which is failing closed. READY=1 is sent again, with the trust state, when
// the start-up sequence ends.
const (
	// signerStartingStatus replaces the waiting status once the signer is
	// reached.
	signerStartingStatus = "STATUS=starting"
	signerWaitStatus     = systemd.NotifyReady + "\nSTATUS=waiting for the host key signer"
	signerWaitMessage    = "waiting for the host key signer"
)

// ErrRunPanicked is returned by Run after an operation run panicked: the
// agent drained and stopped, and its next start's recovery resolves the
// interrupted operation.
var ErrRunPanicked = errors.New("an operation run panicked")

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
// Probe, UID, SignerUID, Listener, TrustDir, TrustFileOwner, JournalFS, Floor, Hooks and
// Now are test overrides: they replace what the host itself provides, the
// agent binary never sets them, and a release build refuses to start when
// any of them is set.
type Options struct {
	// Config is the validated configuration. Required.
	Config *config.Config
	// Executor runs operations. Required.
	Executor config.Executor
	// DevEnrolment is the development enrolment stand-in: the start epoch
	// and the predecessors of a store's first enrolment. Nil: epoch 1, no
	// predecessors. The enrolment record itself always comes from the host
	// certificate.
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
	// SignerUID is the signer user's id (test override); nil resolves
	// Config.SignerUser.
	SignerUID *uint32
	// AllowSignerSameUser lets the signer's user be the agent's own (test
	// override): a signer that is the agent's user protects nothing, so the
	// agent refuses it otherwise. Root is always refused.
	AllowSignerSameUser bool
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
	// StepDeadlines bounds each operation step's run per kind (test
	// override); nil uses opstore.DefaultStepDeadline for every kind.
	StepDeadlines map[providerv1.OperationKind]time.Duration
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

// TrustState implements server.Agent. A store that is ready and recovered
// reports READY only while nothing keeps the host from running operations
// (UnattestedCause); otherwise it reports UNATTESTED. The signed answer
// carries no state reason for these causes: they reach the operator through
// the service status and the logs, and a refused call through its ErrorInfo
// metadata.
func (a *agentState) TrustState() (providerv1.AgentState, string) {
	st, lost := a.store.Journal().State()
	state, reason := answer.StoreState(st, lost, a.store.Recovered())
	if state == providerv1.AgentState_AGENT_STATE_READY && a.UnattestedCause() != "" {
		return providerv1.AgentState_AGENT_STATE_UNATTESTED, ""
	}
	return state, reason
}

// UnattestedCause implements server.Agent: why a ready store still refuses
// operations, or "".
func (a *agentState) UnattestedCause() string {
	if st, _ := a.store.Journal().State(); st == journal.StateLost {
		return "" // a quarantine says why
	}
	if a.store.Freezing() {
		return server.CauseFreezeIncomplete
	}
	switch a.ClockFloorState() {
	case clockfloor.StateOK:
		return ""
	case clockfloor.StateClockFloorAhead:
		return server.CauseClockFloorAhead
	}
	return server.CauseClockFloorUnavailable
}

// ClockFloorState implements server.Agent.
func (a *agentState) ClockFloorState() clockfloor.StateKind {
	f := a.floor.Load()
	if f == nil {
		return ""
	}
	return f.Status().State
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
		"Probe": o.Probe != nil, "UID": o.UID != nil, "SignerUID": o.SignerUID != nil, "AllowSignerSameUser": o.AllowSignerSameUser, "TrustDir": o.TrustDir != "", "JournalFS": o.JournalFS != nil,
		"TrustFileOwner": o.TrustFileOwner != nil, "Listener": o.Listener != nil, "Now": o.Now != nil,
		"Floor": o.Floor.Wall != nil || o.Floor.Boot != nil || o.Floor.BootID != nil || o.Floor.TimeNS != nil, "Hooks": o.Hooks.BeforeRecover != nil || o.Hooks.Started != nil,
		"StepDeadlines": o.StepDeadlines != nil,
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
	// signerUID is the signer process's user: the only peer the agent signs
	// through.
	signerUID uint32
	// trustOwner is the owner the trust configuration's files must have:
	// root.
	trustOwner uint32
	trustDir   string
	creds      *server.CredentialStore
	notifier   systemd.Notifier
	// started is set once the start-up sequence ended and READY was sent: a
	// scrub before it leaves the status to the start-up notification.
	started atomic.Bool
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
	if o.SignerUID != nil {
		d.signerUID = *o.SignerUID
	} else if d.signerUID, err = signer.ResolveUID(o.Config.SignerUser); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrStartRefused, ReasonSignerUnavailable, err)
	}
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
	creds, err := d.loadCredentials(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopped while waiting for the signer
		}
		reason := ReasonIdentityInvalid
		if f, ok := hostcheck.AsFailure(err); ok {
			reason = f.Reason
		} else if errors.Is(err, signer.ErrSigner) {
			reason = ReasonSignerUnavailable
		}
		d.log.Error(err, "start refused", "reason", reason)
		return fmt.Errorf("%w: %s: %v", ErrStartRefused, reason, err)
	}
	d.creds = server.NewCredentialStore(creds)
	d.log.Info("host checks passed", "host", creds.Identity.HostURI)

	// Steps 1 to 3: single writer, open, local continuity checks.
	hooks, rotate := devJournalHooks(d.log)
	st, err := opstore.Open(ctx, opstore.Options{
		Journal: journal.Options{Dir: d.o.Config.StateDir, FS: d.o.JournalFS, Events: d.journalEvent,
			Hooks: hooks, AnchorRotateBytes: rotate},
		Executor:      d.o.Executor,
		Audit:         d.auditEvent,
		StepDeadlines: d.o.StepDeadlines,
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
	// The runner pool lives until the drain; only a development build has an
	// operation path.
	poolCtx, stopPool := context.WithCancel(context.WithoutCancel(ctx))
	defer stopPool()
	// A run that panicked leaves its executor's state unknown: the agent
	// drains and exits, and the next start's recovery resolves the step by
	// its evidence (see package runner).
	fatal := make(chan any, 1)
	giveUp := opstore.DefaultStepDeadline
	for _, dl := range d.o.StepDeadlines {
		giveUp = max(giveUp, dl)
	}
	pool := runner.New(poolCtx, st, d.log.WithName("runner"), runner.Options{GiveUpAfter: giveUp, OnPanic: func(r any) {
		select {
		case fatal <- r:
		default:
		}
	}})
	var ops server.Operations
	if config.DevBuild {
		ops = &operations{store: st, exec: d.o.Executor, pool: pool}
	}
	srv, err := server.New(server.Options{Agent: agent, Credentials: d.creds, Version: d.o.Version,
		JournalSchemaVersion: schema, Log: d.log.WithName("server"), Operations: ops})
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

	// The scrub runs until the agent drains; the drain waits for it, so it
	// never reads a closed store.
	scrubCtx, stopScrub := context.WithCancel(ctx)
	scrubDone := make(chan struct{})
	go func() {
		defer close(scrubDone)
		d.scrubLoop(scrubCtx, agent)
	}()

	drain := func() {
		stopScrub()
		_ = d.notifier.Notify(systemd.NotifyStopping)
		dctx, cancel := context.WithTimeout(context.Background(), d.o.Config.DrainTimeout)
		defer cancel()
		select {
		case <-scrubDone:
		case <-dctx.Done():
			d.log.Error(dctx.Err(), "the store scrub did not stop within the drain timeout; abandoned")
		}
		srv.Shutdown(dctx)
		// Runs stop between steps; an interrupted step is left to the next
		// start's recovery.
		stopPool()
		if !pool.Drain(dctx) {
			d.log.Error(dctx.Err(), "operation runs did not stop within the drain timeout; abandoned to the next start's recovery")
		}
		if f := agent.floor.Load(); f != nil {
			if js, _ := st.Journal().State(); js == journal.StateLost {
				f.StopPersisting()
			}
			if err := f.Drain(dctx); err != nil {
				d.log.Error(err, "save the clock floor")
			}
		}
		closeStore()
		d.log.Info("stopped")
	}

	// Steps 6 to 8.
	for _, ref := range d.establish(ctx, st, agent.floor.Load()) {
		if ops != nil && !guard.Off(guard.RecoveredOpsRun) {
			_ = pool.Start(ref, nil)
		}
	}
	if ctx.Err() == nil {
		state, reason := agent.TrustState()
		if err := d.notifier.Notify(systemd.NotifyReady + "\nSTATUS=" + trustStatus(agent)); err != nil {
			d.log.Error(err, "notify systemd")
		}
		d.log.Info("started", "state", state.String(), "reason", reason)
		d.started.Store(true)
		if d.o.Hooks.Started != nil {
			d.o.Hooks.Started(state)
		}
	}

	// The cause that keeps a ready host from running operations (an incomplete
	// freeze, the clock floor's state) is logged when it appears or goes, and
	// shown in the service status.
	lastCause := agent.UnattestedCause()
	if lastCause != "" {
		d.log.Error(nil, "the host refuses operations", "cause", lastCause)
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
		case r := <-fatal:
			d.log.Error(fmt.Errorf("%v", r), "an operation run panicked; the agent stops so that recovery resolves it")
			drain()
			return fmt.Errorf("%w: %v", ErrRunPanicked, r)
		case <-reload:
			_ = d.notifier.Notify(systemd.NotifyReloading)
			if err := d.reload(ctx, st); err != nil {
				reason := reloadRefusalReason(err)
				d.log.Error(err, "reload refused; the running credentials stay in use", "reason", reason)
				_ = d.notifier.Notify(systemd.NotifyReady + "\nSTATUS=" + trustStatus(agent) + "; reload refused: " + reason)
			} else {
				d.log.Info("credentials reloaded")
				_ = d.notifier.Notify(systemd.NotifyReady + "\nSTATUS=" + trustStatus(agent) + "; credentials reloaded")
			}
		case <-ticker.C:
			if f := agent.floor.Load(); f != nil {
				if js, _ := st.Journal().State(); js == journal.StateLost {
					// The store can no longer commit; the floor goes on in memory.
					f.StopPersisting()
				}
				if err := f.Observe(ctx); err != nil {
					d.log.Error(err, "raise the clock floor")
				}
			}
			if c := agent.UnattestedCause(); c != lastCause {
				if c != "" {
					d.log.Error(nil, "the host refuses operations", "cause", c)
				} else {
					d.log.Info("the host accepts operations again", "was", lastCause)
				}
				lastCause = c
				if d.started.Load() {
					_ = d.notifier.Notify(systemd.NotifyReady + "\nSTATUS=" + trustStatus(agent))
				}
			}
		}
	}
}

// scrubInterval is the period of the store scrub.
func (d *daemon) scrubInterval() time.Duration {
	if d.o.Config.ScrubInterval > 0 {
		return d.o.Config.ScrubInterval
	}
	return config.DefaultScrubInterval
}

// scrubLoop scrubs the operation store every scrub interval until ctx ends.
// It is one goroutine, so a scrub never overlaps the next one: a tick that
// arrives during a scrub waits for it. A scrub that finds damage has
// quarantined the store; the loss is logged by the store's event and the
// service status reflects it like any other loss. A store that is already
// lost is not scrubbed again.
func (d *daemon) scrubLoop(ctx context.Context, agent *agentState) {
	log := d.log.WithName("scrub")
	ticker := time.NewTicker(d.scrubInterval())
	defer ticker.Stop()
	j := agent.store.Journal()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		before, _ := j.State()
		if before == journal.StateLost {
			continue
		}
		err := j.Scrub(ctx)
		switch {
		case err == nil:
			log.V(1).Info("journal scrub passed")
		case ctx.Err() != nil:
			return
		default:
			after, reason := j.State()
			log.Error(err, "journal scrub failed", "state", after.String(), "reason", string(reason))
			if after == journal.StateLost && d.started.Load() {
				_ = d.notifier.Notify(systemd.NotifyReady + "\nSTATUS=" + trustStatus(agent))
			}
		}
	}
}

// loadCredentials runs the start-up host checks and loads the trust
// configuration and the host identity.
func (d *daemon) loadCredentials(ctx context.Context) (*server.Credentials, error) {
	cfg := d.o.Config
	if err := hostcheck.Run(d.o.Probe, d.hostcheckConfig()); err != nil {
		return nil, err
	}
	// The host key is in the signer process; the agent only reaches it. The
	// start waits for a signer that is not there yet, without a limit.
	if d.signerUID == 0 || (d.signerUID == d.uid && !d.o.AllowSignerSameUser) {
		return nil, fmt.Errorf("%w: the signer's user (uid %d) must be neither root nor the agent's own user: %w", signer.ErrSigner, d.signerUID, signer.ErrPeer)
	}
	var lastLog time.Time
	sg, err := signer.ConnectWithRetry(ctx, d.signerOptions(ctx), signerPollEvery, func(err error) {
		_ = d.notifier.Notify(signerWaitStatus)
		if now := d.o.Now(); lastLog.IsZero() || now.Sub(lastLog) >= signerWaitLogEvery {
			lastLog = now
			d.log.Info(signerWaitMessage, "error", err.Error())
		}
	})
	if err != nil {
		return nil, fmt.Errorf("reach the signer: %w", err)
	}
	// The wait is over: do not leave the waiting status standing through the
	// rest of the start-up.
	_ = d.notifier.Notify(signerStartingStatus)
	tc, err := trust.LoadConfig(d.trustDir, trust.TrustFileReader(d.trustOwner))
	if err != nil {
		return nil, err
	}
	id, err := trust.LoadIdentity(cfg.PKIDir(), tc, d.o.Now(), trust.KeyFileReader(d.uid), sg)
	if err != nil {
		return nil, err
	}
	if err := hostcheck.CheckHostMarker(d.o.Probe, cfg.StateDir, id.HostURI, d.uid); err != nil {
		return nil, err
	}
	return &server.Credentials{Identity: id, Trust: tc}, nil
}

// hostcheckConfig is what the start-up host checks look at.
func (d *daemon) hostcheckConfig() hostcheck.Config {
	cfg := d.o.Config
	return hostcheck.Config{StateDir: cfg.StateDir, PKIDir: cfg.PKIDir(), TrustDir: d.trustDir, UID: d.uid,
		SignerKeyFile: cfg.SignerKeyFile, SignerSocketDir: filepath.Dir(cfg.SignerSocket), SignerUID: d.signerUID}
}

// signerOptions is how the agent reaches the signer: its socket, and only
// its user as the peer. The signing client lives as long as the run, so it
// takes the run's context, not that of the start-up wait.
func (d *daemon) signerOptions(run context.Context) signer.ClientOptions {
	return signer.ClientOptions{Socket: d.o.Config.SignerSocket, ServerUID: d.signerUID, Lifetime: run}
}

// reload checks and loads the key material and trust configuration again,
// and replaces the running credentials when they name the same enrolment.
func (d *daemon) reload(ctx context.Context, st *opstore.Store) error {
	cfg := d.o.Config
	hc := d.hostcheckConfig()
	if err := hostcheck.CheckPKI(d.o.Probe, cfg.PKIDir(), d.uid); err != nil {
		return err
	}
	if err := hostcheck.CheckHostKeyUnreadable(d.o.Probe, hc.PKIDir, hc.SignerKeyFile); err != nil {
		return err
	}
	// A reload fetches the signer's key again: a renewed certificate may name
	// a new key, which the signer loads on its own reload.
	sg, err := signer.Connect(ctx, d.signerOptions(ctx))
	if err != nil {
		return fmt.Errorf("reach the signer: %w", err)
	}
	if err := hostcheck.CheckTrustDir(d.o.Probe, d.trustDir); err != nil {
		return err
	}
	tc, err := trust.LoadConfig(d.trustDir, trust.TrustFileReader(d.trustOwner))
	if err != nil {
		return err
	}
	id, err := trust.LoadIdentity(cfg.PKIDir(), tc, d.o.Now(), trust.KeyFileReader(d.uid), sg)
	if err != nil {
		return err
	}
	if cur := d.creds.Load().Identity; !id.SameEnrolment(cur) {
		if id.HostURI != cur.HostURI || !config.DevBuild {
			return errEnrolmentChanged
		}
		// A new enrolment of this host (vrtg-agent enroll install --replace):
		// the store is re-enrolled first, then the credentials are swapped, so
		// an answer never names an enrolment the store has not activated.
		if err := d.syncEnrolment(ctx, st, id); err != nil {
			return fmt.Errorf("%w: %w", errReenrolmentRefused, err)
		}
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
		reason = a.UnattestedCause()
	}
	if reason == "" {
		return state.String()
	}
	return state.String() + " (" + reason + ")"
}

// ReasonEnrolmentChanged is the reload refusal reason for a certificate that
// names another host, or, in a release build, another enrolment.
const ReasonEnrolmentChanged = "EnrolmentChanged"

// ReasonReenrolmentRefused is the reload refusal reason for a certificate that
// names a new enrolment of this host which the store refused: an enrolment
// id it has used before, a store that cannot commit, or a freeze that could
// not finish. The running credentials stay, and a freeze that was cut short
// resumes at the next reload or start.
const ReasonReenrolmentRefused = "ReenrolmentRefused"

// errEnrolmentChanged refuses a reload that would change the host.
var errEnrolmentChanged = errors.New("the reloaded certificate names another host or enrolment")

// errReenrolmentRefused marks a reload whose re-enrolment the store refused.
var errReenrolmentRefused = errors.New("the store refused the new enrolment")

// reloadRefusalReason is the one-word reason of a refused reload, for the
// service status: never a path or a secret.
func reloadRefusalReason(err error) string {
	if f, ok := hostcheck.AsFailure(err); ok {
		return f.Reason
	}
	if errors.Is(err, errEnrolmentChanged) {
		return ReasonEnrolmentChanged
	}
	if errors.Is(err, errReenrolmentRefused) {
		return ReasonReenrolmentRefused
	}
	return ReasonIdentityInvalid
}

// openFloor opens the clock floor (step 4): it reads the floor record of the
// store and raises it in memory, and writes nothing, because the store does
// not commit before continuity is established (startFloor commits it). A floor
// record that is missing for an enrolled store, or cannot be read,
// quarantines the store; a clock source the floor cannot use refuses the
// start.
func (d *daemon) openFloor(ctx context.Context, agent *agentState) error {
	if left, err := d.o.JournalFS.Exists(filepath.Join(d.o.Config.StateDir, "clock.floor")); err == nil && left {
		d.log.Info("ignoring a clock.floor file in the state directory: the clock floor is kept in the operation store")
	}
	firstUse := false
	if v, err := agent.store.HostView(ctx, 0); err == nil {
		firstUse = v.LastStartEpoch == 0
	}
	f, err := clockfloor.Open(ctx, clockfloor.Config{
		Store: floorStore{st: agent.store}, FirstUse: firstUse, PersistInterval: FloorPersistInterval,
		NewBootSlack: FloorPersistInterval + d.o.ObserveEvery,
		Wall:         d.o.Floor.Wall, Boot: d.o.Floor.Boot, BootID: d.o.Floor.BootID, TimeNS: d.o.Floor.TimeNS,
	})
	switch {
	case err == nil:
		agent.floor.Store(f)
		return nil
	case errors.Is(err, clockfloor.ErrFloorMissing), errors.Is(err, clockfloor.ErrCorrupt), errors.Is(err, errFloorUnreadable):
		d.log.Error(err, "the clock floor cannot be read; the store is quarantined")
		// Not journal_corrupt: the journal verified. A state file beside it is
		// gone or damaged, the same shape as a missing store marker.
		agent.store.Journal().Quarantine(journal.LostInconsistentStore, "clock floor unreadable")
		return nil
	}
	d.log.Error(err, "start refused: the host's clocks cannot keep a floor", "reason", hostcheck.ReasonUnsupportedHost)
	return fmt.Errorf("%w: %s: %v", ErrStartRefused, hostcheck.ReasonUnsupportedHost, err)
}

// establish runs steps 6 and 7: continuity, the first commit of the clock
// floor, the development enrolment stand-in, and recovery. floor is nil when
// the floor could not be opened (the store is quarantined). It returns the
// operations recovery left runnable.
func (d *daemon) establish(ctx context.Context, st *opstore.Store, floor *clockfloor.Floor) []opstore.OpRef {
	if js, _ := st.Journal().State(); js != journal.StateUnattested {
		return nil
	}
	if err := st.EstablishContinuity(ctx); err != nil {
		d.log.Error(err, "continuity not established")
		return nil
	}
	if !d.startFloor(ctx, st, floor) {
		return nil
	}
	d.enrol(ctx, st)
	if d.o.Hooks.BeforeRecover != nil {
		d.o.Hooks.BeforeRecover(ctx)
	}
	if ctx.Err() != nil {
		d.log.Info("recovery cancelled by a stop; the next start resumes it")
		return nil
	}
	refs, err := st.Recover(ctx)
	if err != nil {
		if ctx.Err() != nil {
			d.log.Info("recovery cancelled by a stop; the next start resumes it")
			return nil
		}
		d.log.Error(err, "recovery failed")
		return nil
	}
	if len(refs) > 0 {
		d.log.Info("operations remain runnable after recovery", "count", len(refs))
	}
	return refs
}

// startFloor commits the clock floor, raised in memory since step 4, as the
// first write after continuity is established, before an enrolment or any
// recovery commit. It reports whether start-up may go on. A floor that cannot
// be committed quarantines the store (io_error): nothing is run on a floor
// the store does not hold.
func (d *daemon) startFloor(ctx context.Context, st *opstore.Store, floor *clockfloor.Floor) bool {
	if floor == nil {
		return true
	}
	if err := floor.StartPersisting(ctx); err != nil {
		if ctx.Err() != nil {
			return false
		}
		if errors.Is(err, clockfloor.ErrStoreLost) {
			d.log.Error(err, "the store cannot commit the clock floor; it is quarantined")
			return false
		}
		d.log.Error(err, "the clock floor cannot be written; the store is quarantined")
		st.Journal().Quarantine(journal.LostIOError, "clock floor not written")
		return false
	}
	return true
}

// enrol brings the store's active enrolment to the one the host certificate
// carries (syncEnrolment) and logs what it did. A refusal leaves answers
// unsigned: the store keeps what it has and nothing is made up.
func (d *daemon) enrol(ctx context.Context, st *opstore.Store) {
	id := d.creds.Load().Identity
	switch err := d.syncEnrolment(ctx, st, id); {
	case err == nil:
	case errors.Is(err, errNotEnrolled):
		d.log.Info("the store is not enrolled; answers stay unsigned")
	case errors.Is(err, errReenrolDevOnly):
		d.log.Error(err, "answers stay unsigned until the host is enrolled again")
	case errors.Is(err, errStoreOfOtherHost):
		d.log.Error(err, "enrolment refused; answers stay unsigned until the host is enrolled again", "reason", "StoreOfOtherHost")
	default:
		d.log.Error(err, "enrolment from the host certificate failed; answers stay unsigned until the host is enrolled again",
			"enrolment", hex.EncodeToString(id.Record.EnrolmentID))
	}
}

// errNotEnrolled is returned by a release build for a store that was never
// enrolled: it has no development enrolment, and a release build derives no
// enrolment from the certificate until the manager's re-enrolment flow lands.
var errNotEnrolled = errors.New("the store is not enrolled; a release build does not enrol from the certificate")

// errReenrolDevOnly is returned by a release build for a certificate that
// names an enrolment the store does not have active: re-enrolling is for
// development builds until the manager's re-enrolment flow lands.
var errReenrolDevOnly = errors.New("the store's active enrolment is not the one the host certificate names; a release build does not re-enrol")

// errStoreOfOtherHost refuses to enrol a store whose active enrolment is for
// another host.
var errStoreOfOtherHost = errors.New("the store belongs to another host")

// syncEnrolment derives the host's active enrolment from its certificate and
// makes the store's agree. The enrolment record (enrolment id, Provider uid,
// install id) is read from the URI subject alternative names the
// subordinate CA signed, so it is authentic without any further message.
//
//   - The store's active enrolment already is the certificate's: nothing to
//     do.
//   - The store was never enrolled: it is enrolled with the certificate's
//     record. The start epoch is 1, or the development enrolment file's (a
//     development-only stand-in, which also supplies predecessors that this
//     store never held) when one is given.
//   - Any other case (another enrolment is active, or none is because a
//     freeze was cut short or the host was re-provisioned): a re-enrolment
//     (opstore.Store.Reenrol) with the next start epoch. It freezes every
//     open operation of the previous enrolment in bounded chunks, then
//     activates the new one; an enrolment id the store has used before is
//     refused and the store keeps its state.
//
// Development builds only (config.DevBuild): a release build enrols a fresh
// store only from a development enrolment, never re-enrols, and leaves the
// answers unsigned otherwise, until the manager's re-enrolment flow lands.
//
// A store that belongs to another host is never touched: the active
// enrolment's host, or with none active the store marker's and the newest
// held enrolment record's, must be the certificate's.
func (d *daemon) syncEnrolment(ctx context.Context, st *opstore.Store, id *trust.Identity) error {
	v, err := st.HostView(ctx, 0)
	if err != nil {
		return fmt.Errorf("read the store's enrolment: %w", err)
	}
	e := opstore.Enrolment{
		ID: id.Record.EnrolmentID, ProviderUID: id.Record.ProviderUID, HostID: id.HostID(), InstallID: id.Record.InstallID,
	}
	// Whose store is it? The active enrolment names its host; with none
	// active, the store marker and the newest enrolment record still held
	// (the frozen predecessor) do.
	var latest string
	if v.Enrolment == nil {
		if latest, err = st.LatestEnrolmentHostID(ctx); err != nil {
			return fmt.Errorf("read the store's enrolments: %w", err)
		}
	}
	if storeOwnerMismatch(v.Enrolment, st.Journal().Marker().HostID, latest, id.HostID()) {
		return errStoreOfOtherHost
	}
	switch {
	case v.Enrolment != nil && bytes.Equal(v.Enrolment.ID, e.ID) && v.Enrolment.ProviderUID == e.ProviderUID &&
		bytes.Equal(v.Enrolment.InstallID, e.InstallID):
		return nil
	case v.Enrolment == nil && v.LastStartEpoch == 0:
		e.StartEpoch = 1
		if dev := d.o.DevEnrolment; dev != nil {
			e.StartEpoch, e.Predecessors = dev.StartEpoch, dev.Predecessors
		} else if !config.DevBuild {
			return errNotEnrolled
		}
		if err := st.Enrol(ctx, e); err != nil {
			return fmt.Errorf("enrol: %w", err)
		}
		d.log.Info("enrolled from the host certificate", "enrolment", hex.EncodeToString(e.ID), "startEpoch", e.StartEpoch)
		return nil
	}
	if !config.DevBuild {
		return errReenrolDevOnly
	}
	e.StartEpoch = v.LastStartEpoch + 1
	if err := st.Reenrol(ctx, e); err != nil {
		return fmt.Errorf("re-enrol: %w", err)
	}
	d.log.Info("re-enrolled from the host certificate; the previous enrolment's operations are frozen",
		"enrolment", hex.EncodeToString(e.ID), "startEpoch", e.StartEpoch)
	return nil
}

// storeOwnerMismatch reports whether a store belongs to a host other than
// host. With an active enrolment, only its host counts. With none, the store
// marker's host and the newest enrolment record's host (latestHost, "" when
// none is held) each count when set. A store that names no host at all (a
// marker with none and every record discarded) belongs to nobody and is
// adopted by whichever host asks: nothing in the store says otherwise.
func storeOwnerMismatch(active *opstore.ActiveEnrolment, markerHost, latestHost, host string) bool {
	owners := []string{markerHost, latestHost}
	if active != nil {
		owners = []string{active.HostID}
	}
	for _, o := range owners {
		if o != "" && o != host {
			return true
		}
	}
	return false
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
