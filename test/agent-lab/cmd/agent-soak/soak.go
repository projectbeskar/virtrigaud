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

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Reasons the agent puts in a refusal's ErrorInfo.
const (
	reasonUnattested     = "UNATTESTED"
	reasonVMBusy         = "VM_BUSY"
	reasonUnknownToAgent = "UNKNOWN_TO_AGENT"
)

// Violation classes: each one is a failure of the run.
const (
	vLostAcked        = "lost_acknowledged_operation"
	vTerminalChanged  = "terminal_state_changed"
	vStuck            = "operation_past_deadline"
	vUnacked          = "operation_never_acknowledged"
	vUnexpectedRefuse = "unexpected_refusal"
	vRedeliveryRef    = "redelivery_returned_other_task"
	vDupOnce          = "duplicate_effect_once_step"
	vDupIrrev         = "duplicate_effect_irreversible_step"
	vOrphanEffect     = "effect_without_operation"
	vMissingEffect    = "completed_operation_without_effect"
	vStepReinvoked    = "once_or_irreversible_step_invoked_twice"
	vRestartFailed    = "agent_did_not_restart"
	vStoreMismatch    = "store_differs_from_observed"
	vVMState          = "vm_state_differs_from_outcomes"
	vQuarantined      = "store_quarantined"
	vAgentExit        = "agent_not_stopped_cleanly"
	vStrayTemps       = "stray_temporary_files_remain"
)

// opRec is the caller's durable record of one operation: kept from before the
// first delivery, and never erased by an agent kill.
type opRec struct {
	Spec *opSpec
	ID   string
	VM   string
	Kind providerv1.OperationKind

	Sent      int
	Acked     bool
	AckGen    int
	AckUp     time.Duration
	Task      string
	Terminal  string // the outcome name once terminal
	Reason    string
	TermGen   int
	Redeliver int
	Refused   bool // refused as store full: never accepted
}

// ledger is the set of operations a run issued.
type ledger struct {
	mu   sync.Mutex
	ops  map[string]*opRec
	list []*opRec

	violations map[string]int
	details    []string
}

func newLedger() *ledger { return &ledger{ops: map[string]*opRec{}, violations: map[string]int{}} }

func (l *ledger) add(o *opSpec) *opRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := &opRec{Spec: o, ID: hex.EncodeToString(o.id.OpID), VM: o.id.VMUID, Kind: o.kind}
	l.ops[r.ID] = r
	l.list = append(l.list, r)
	return r
}

// violate records a failure of the run.
func (l *ledger) violate(class, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.violations[class]++
	if len(l.details) < 300 {
		l.details = append(l.details, class+": "+fmt.Sprintf(format, args...))
	}
}

// snapshot returns copies of every record.
func (l *ledger) snapshot() []opRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]opRec, len(l.list))
	for i, r := range l.list {
		out[i] = *r
	}
	return out
}

// reasonOf extracts the ErrorInfo reason of a refusal.
func reasonOf(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

// transient reports whether a failed call says nothing about the operation:
// the agent was killed, is starting, or refused as not yet attested.
func transient(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	return false
}

// soakConfig is the configuration of a kill run.
type soakConfig struct {
	Kills      int
	Workers    int
	Seed       int64
	OpDeadline time.Duration
	MaxRuntime time.Duration
	// ReadyWait bounds the wait for an agent to answer READY after a start.
	ReadyWait time.Duration
}

// soak is one kill run.
type soak struct {
	cfg  soakConfig
	lab  *lab
	sup  *supervisor
	call *caller
	led  *ledger
	rng  *rand.Rand

	attempt atomic.Uint64
	vmSeq   atomic.Uint64
	runTag  string

	ackEvents chan struct{}
	stopping  atomic.Bool

	latMu sync.Mutex
	lats  []time.Duration
	latT  []time.Time // when each acknowledgement was received

	// expectCap makes a store-full refusal (RESOURCE_EXHAUSTED) an expected,
	// counted answer instead of a failure; capMsg keeps its message.
	expectCap bool
	capMsg    atomic.Value

	counters sync.Map // name -> *atomic.Int64
	killLog  []killRecord

	plant      bool
	temps      *tempReport
	checkAudit bool
}

// killRecord is one kill.
type killRecord struct {
	N        int           `json:"n"`
	Mode     string        `json:"mode"`
	Gen      int           `json:"gen"`
	Delay    time.Duration `json:"delay_ns"`
	InFlight int           `json:"in_flight"`
}

func (s *soak) count(name string) {
	v, _ := s.counters.LoadOrStore(name, new(atomic.Int64))
	v.(*atomic.Int64).Add(1) //nolint:errcheck // the map holds only *atomic.Int64
}

func (s *soak) counts() map[string]int64 {
	out := map[string]int64{}
	s.counters.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load() //nolint:errcheck // the map holds only *atomic.Int64
		return true
	})
	return out
}

// vmState is a worker's view of the VM it drives.
type vmState struct {
	uid     string
	binding []byte
	powers  int // power operations left before the VM is deleted
}

// worker drives VM lifecycles one operation at a time, as a manager reconciling
// VirtualMachines would.
func (s *soak) worker(ctx context.Context, id int, wg *sync.WaitGroup) {
	defer wg.Done()
	rng := rand.New(rand.NewSource(s.cfg.Seed + int64(id)*7919)) // #nosec G404 -- test randomness
	var vm *vmState
	var reuse string
	for !s.stopping.Load() && ctx.Err() == nil {
		var kind providerv1.OperationKind
		switch {
		case vm == nil:
			uid := reuse
			reuse = ""
			if uid == "" {
				uid = fmt.Sprintf("vm-%s-%d", s.runTag, s.vmSeq.Add(1))
			}
			vm = &vmState{uid: uid, binding: randBytes(16), powers: 1 + rng.Intn(5)}
			kind = providerv1.OperationKind_OPERATION_KIND_CREATE
		case vm.powers > 0:
			vm.powers--
			kind = []providerv1.OperationKind{providerv1.OperationKind_OPERATION_KIND_POWER_ON, providerv1.OperationKind_OPERATION_KIND_POWER_OFF,
				providerv1.OperationKind_OPERATION_KIND_REBOOT, providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL}[rng.Intn(4)]
		default:
			kind = providerv1.OperationKind_OPERATION_KIND_DELETE
		}
		spec, err := s.call.newOp(kind, vm.uid, vm.binding, s.attempt.Add(1))
		if err != nil {
			s.led.violate(vUnexpectedRefuse, "build operation: %v", err)
			return
		}
		out := s.runOp(ctx, rng, spec)
		s.count("ops_" + out)
		switch {
		case out != "SUCCEEDED":
			// The VM is held by an unresolved or failed operation: leave it,
			// as a manager would leave it to an operator, and start another.
			s.count("vm_abandoned")
			vm = nil
		case kind == providerv1.OperationKind_OPERATION_KIND_DELETE:
			if rng.Intn(100) < 30 {
				reuse = vm.uid // the same VM again, with a new placement
			}
			vm = nil
		}
	}
}

// runOp delivers spec until it is acknowledged, then follows it to a terminal
// state, redelivering and re-reading as a caller would across agent restarts.
// It returns the outcome name, or a pseudo outcome for a failed run.
func (s *soak) runOp(ctx context.Context, rng *rand.Rand, spec *opSpec) string {
	rec := s.led.add(spec)
	busy := 0
	// Delivery.
	for {
		if ctx.Err() != nil {
			return "ABORTED"
		}
		ac, err := s.sup.wait(ctx)
		if err != nil {
			return "ABORTED"
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		t0 := time.Now()
		task, err := s.call.send(cctx, ac.cc, spec)
		lat := time.Since(t0)
		cancel()
		s.led.mu.Lock()
		rec.Sent++
		s.led.mu.Unlock()
		if err == nil {
			s.led.mu.Lock()
			rec.Acked, rec.Task, rec.AckGen, rec.AckUp = true, task, ac.gen, s.sup.uptime()
			s.led.mu.Unlock()
			s.latMu.Lock()
			s.lats = append(s.lats, lat)
			s.latT = append(s.latT, time.Now())
			s.latMu.Unlock()
			select {
			case s.ackEvents <- struct{}{}:
			default:
			}
			break
		}
		reason := reasonOf(err)
		switch {
		case s.expectCap && status.Code(err) == codes.ResourceExhausted:
			// A definite refusal: the store is full and nothing was recorded.
			s.led.mu.Lock()
			rec.Refused = true
			s.led.mu.Unlock()
			s.capMsg.Store(status.Convert(err).Message())
			s.count("refused_reason_" + reason)
			s.count("refused_domain_" + domainOf(err))
			s.count("refused_store_full")
			time.Sleep(200 * time.Millisecond)
			return "REFUSED_FULL"
		case transient(err) || reason == reasonUnattested:
			s.count("delivery_retries")
			time.Sleep(10 * time.Millisecond)
		case reason == reasonVMBusy && busy < 5:
			busy++
			s.count("vm_busy_retries")
			time.Sleep(50 * time.Millisecond)
		default:
			s.led.violate(vUnexpectedRefuse, "op %s %s: %v (reason %q)", rec.ID, rec.Kind, err, reason)
			return "REFUSED"
		}
	}
	// A duplicate delivery of the same operation, with a fresh grant: the agent
	// must answer with the same task and run nothing again.
	if rng.Intn(100) < 35 {
		s.redeliver(ctx, rec)
	}
	// Follow it to its end.
	out := s.await(ctx, rec)
	if out != "" && rng.Intn(100) < 25 {
		s.redeliver(ctx, rec)
	}
	if out == "" {
		return "UNFINISHED"
	}
	return out
}

// redeliver sends the operation again and checks that it names the same task.
func (s *soak) redeliver(ctx context.Context, rec *opRec) {
	ac, err := s.sup.wait(ctx)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	task, err := s.call.send(cctx, ac.cc, rec.Spec)
	s.led.mu.Lock()
	rec.Sent++
	rec.Redeliver++
	want := rec.Task
	s.led.mu.Unlock()
	switch {
	case err == nil && task != want:
		s.led.violate(vRedeliveryRef, "op %s: task %s, first answer %s", rec.ID, task, want)
	case err != nil && !transient(err) && reasonOf(err) != reasonUnattested:
		s.led.violate(vUnexpectedRefuse, "redelivery of op %s: %v (reason %q)", rec.ID, err, reasonOf(err))
	}
	s.count("redeliveries")
}

// await polls an acknowledged operation until it is terminal, enforcing its
// deadline over the time an agent was serving. It returns the outcome name, or
// "" when the operation did not finish.
func (s *soak) await(ctx context.Context, rec *opRec) string {
	flagged := false
	for {
		if ctx.Err() != nil {
			return ""
		}
		ac, err := s.sup.wait(ctx)
		if err != nil {
			return ""
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := taskStatus(cctx, ac.cc, rec.Task)
		cancel()
		switch {
		case err != nil && reasonOf(err) == reasonUnknownToAgent:
			s.led.violate(vLostAcked, "op %s (%s, acknowledged by run %d) is unknown to the agent after run %d", rec.ID, rec.Kind, rec.AckGen, ac.gen)
			return ""
		case err != nil && transient(err):
			time.Sleep(10 * time.Millisecond)
			continue
		case err != nil:
			s.led.violate(vUnexpectedRefuse, "TaskStatus of op %s: %v", rec.ID, err)
			return ""
		}
		if o := st.GetOutcome(); o != providerv1.TaskOutcome_TASK_OUTCOME_RUNNING {
			s.led.mu.Lock()
			rec.Terminal, rec.Reason, rec.TermGen = o.String(), st.GetOutcomeReason(), ac.gen
			s.led.mu.Unlock()
			return shortOutcome(o)
		}
		s.led.mu.Lock()
		over := s.sup.uptime()-rec.AckUp > s.cfg.OpDeadline
		s.led.mu.Unlock()
		if over && !flagged {
			flagged = true
			s.led.violate(vStuck, "op %s (%s) still running %s of agent uptime after its acknowledgement", rec.ID, rec.Kind, s.cfg.OpDeadline)
		}
		if over && s.sup.uptime()-rec.AckUp > 3*s.cfg.OpDeadline {
			return ""
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func shortOutcome(o providerv1.TaskOutcome) string {
	switch o {
	case providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED:
		return "SUCCEEDED"
	case providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN:
		return "FAILED_CLEAN"
	case providerv1.TaskOutcome_TASK_OUTCOME_FAILED_DIRTY:
		return "FAILED_DIRTY"
	case providerv1.TaskOutcome_TASK_OUTCOME_UNKNOWN:
		return "UNKNOWN"
	}
	return o.String()
}

// inFlight is the number of acknowledged operations not yet terminal.
func (s *soak) inFlight() int {
	s.led.mu.Lock()
	defer s.led.mu.Unlock()
	n := 0
	for _, r := range s.led.list {
		if r.Acked && r.Terminal == "" {
			n++
		}
	}
	return n
}

// effectsSize is the size of the fake executor's effect log.
func (s *soak) effectsSize() int64 {
	fi, err := os.Stat(filepath.Join(s.lab.Dirs.effects(), "effects.log"))
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (s *soak) holdPath() string { return filepath.Join(s.lab.Dirs.effects(), "hold") }

// Kill modes.
const (
	killDelay   = "random_delay"
	killAck     = "after_acknowledgement"
	killEffect  = "after_effect"
	killStartup = "during_startup"
	killHold    = "during_held_step"
)

// pickMode chooses a kill mode by weight.
func (s *soak) pickMode() string {
	switch r := s.rng.Intn(100); {
	case r < 30:
		return killDelay
	case r < 55:
		return killAck
	case r < 80:
		return killEffect
	case r < 90:
		return killStartup
	}
	return killHold
}

// chaos starts the agent, kills it at a mode-chosen point and starts it again
// until the configured number of kills is reached.
func (s *soak) chaos(ctx context.Context) error {
	if _, err := s.sup.start(ctx, s.cfg.ReadyWait); err != nil {
		return fmt.Errorf("first start: %w", err)
	}
	deadline := time.Now().Add(s.cfg.MaxRuntime)
	for n := 1; n <= s.cfg.Kills && time.Now().Before(deadline); n++ {
		mode := s.pickMode()
		var delay time.Duration
		gen := func() int {
			c, _ := s.sup.current()
			if c != nil {
				return c.gen
			}
			return -1
		}()
		t0 := time.Now()
		switch mode {
		case killDelay:
			delay = time.Duration(s.rng.Int63n(int64(1500 * time.Millisecond)))
			s.sleep(ctx, delay)
		case killAck:
			for len(s.ackEvents) > 0 {
				<-s.ackEvents
			}
			select {
			case <-s.ackEvents:
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
			}
		case killEffect:
			base := s.effectsSize()
			end := time.Now().Add(3 * time.Second)
			for s.effectsSize() == base && time.Now().Before(end) && ctx.Err() == nil {
				time.Sleep(100 * time.Microsecond)
			}
			s.sleep(ctx, time.Duration(s.rng.Int63n(int64(3*time.Millisecond))))
		case killHold:
			if err := os.WriteFile(s.holdPath(), nil, 0o644); err != nil { // #nosec G306 -- read by the agent's user
				return fmt.Errorf("set hold: %w", err)
			}
			for i := 0; i < 2; i++ {
				select {
				case <-s.ackEvents:
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
				}
			}
			s.sleep(ctx, time.Duration(s.rng.Int63n(int64(60*time.Millisecond))))
		case killStartup:
			// Handled below: the kill comes during the next start.
		}
		if mode == killStartup {
			// Restart first without waiting, and kill the new run at a random
			// point of its start-up (recovery included).
			if !s.sup.kill() {
				return fmt.Errorf("kill %d: no agent running", n)
			}
			s.recordKill(n, mode, gen, 0)
			if err := s.startupKill(ctx); err != nil {
				return err
			}
			continue
		}
		delay = time.Since(t0)
		flight := s.inFlight()
		if s.plant && s.rng.Intn(100) < 20 {
			s.temps.Planted += len(s.lab.plantTemps(false))
		}
		if !s.sup.kill() {
			return fmt.Errorf("kill %d: no agent running", n)
		}
		_ = os.Remove(s.holdPath())
		s.recordKill(n, mode, gen, flight)
		s.killLog[len(s.killLog)-1].Delay = delay
		if err := s.restart(ctx); err != nil {
			return err
		}
		if n%25 == 0 {
			s.verifyAcked(ctx, false)
		}
	}
	return nil
}

func (s *soak) recordKill(n int, mode string, gen, flight int) {
	s.killLog = append(s.killLog, killRecord{N: n, Mode: mode, Gen: gen, InFlight: flight})
	s.count("kills_" + mode)
	s.count("kills")
}

// startupKill spawns the agent and kills it a random time into its start,
// then starts it again for good.
func (s *soak) startupKill(ctx context.Context) error {
	// Spawn without waiting for READY: a short ready wait that is allowed to
	// run out, then kill. A start that completes within the window is simply
	// killed while serving.
	window := time.Duration(20+s.rng.Intn(380)) * time.Millisecond
	wctx, cancel := context.WithTimeout(ctx, window)
	_, err := s.sup.start(wctx, window)
	cancel()
	_ = err // timing out is the point
	s.sup.kill()
	s.count("startup_kills_second")
	return s.restart(ctx)
}

// restart starts the agent again, retrying a start that fails once.
func (s *soak) restart(ctx context.Context) error {
	var last error
	for try := 0; try < 3; try++ {
		if _, err := s.sup.start(ctx, s.cfg.ReadyWait); err != nil {
			last = err
			s.led.violate(vRestartFailed, "%v", err)
			s.sup.kill()
			continue
		}
		return nil
	}
	return fmt.Errorf("the agent did not restart: %w", last)
}

func (s *soak) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// verifyAcked reads back acknowledged operations through the agent and checks
// that each is present, and that no terminal state changed: every
// non-terminal one, and (all of them when full) the terminal ones.
func (s *soak) verifyAcked(ctx context.Context, full bool) {
	recs := s.led.snapshot()
	var pick []opRec
	var terminal []opRec
	for _, r := range recs {
		if !r.Acked {
			continue
		}
		if r.Terminal == "" {
			pick = append(pick, r)
		} else {
			terminal = append(terminal, r)
		}
	}
	if !full && len(terminal) > 150 {
		s.rng.Shuffle(len(terminal), func(i, j int) { terminal[i], terminal[j] = terminal[j], terminal[i] })
		terminal = terminal[:150]
	}
	pick = append(pick, terminal...)
	for _, r := range pick {
		ac, err := s.sup.wait(ctx)
		if err != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := taskStatus(cctx, ac.cc, r.Task)
		cancel()
		switch {
		case err != nil && reasonOf(err) == reasonUnknownToAgent:
			s.led.violate(vLostAcked, "op %s (%s) acknowledged by run %d is unknown to the agent (run %d)", r.ID, r.Kind, r.AckGen, ac.gen)
		case err != nil:
			// Transient: the next sweep reads it.
		case r.Terminal != "" && st.GetOutcome().String() != r.Terminal:
			s.led.violate(vTerminalChanged, "op %s was %s, now %s", r.ID, r.Terminal, st.GetOutcome())
		}
	}
	s.count("verify_sweeps")
}

// percentile returns the p-th percentile of sorted durations.
func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(float64(len(d)-1) * p)
	return d[i]
}

func (s *soak) latencySummary() map[string]any {
	s.latMu.Lock()
	d := append([]time.Duration(nil), s.lats...)
	s.latMu.Unlock()
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return map[string]any{"n": len(d), "p50_ms": ms(percentile(d, 0.5)), "p99_ms": ms(percentile(d, 0.99)), "max_ms": ms(percentile(d, 1))}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// domainOf extracts the ErrorInfo domain of a refusal.
func domainOf(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetDomain()
		}
	}
	return ""
}
