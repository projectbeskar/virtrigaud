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

// Command agent-soak drives the per-host agent through its real serving path
// while killing it, and measures the growth of its operation store. It is a
// test tool, not part of any release; it runs as root inside a disposable
// host.
//
//	agent-soak soak   -state D -work D ... [-kills N] [-mode binary|wrapped]
//	agent-soak growth -state D -work D ... [-steps 0,10000,100000,1000000]
//	agent-soak verify-store -state D -ops FILE
//	agent-soak agent  <agent flags> --step-log FILE   (internal: wrapped agent)
//
// soak prepares a fresh host (PKI, trust configuration, signer, state and
// effect directories), starts the signer once and the agent repeatedly, and
// has several workers send a random mix of Create, Delete and Power
// operations with durable operation identities and mint their grants as the
// manager would. The agent is killed with SIGKILL at random points (a random
// time into a run, right after an acknowledgement, right after a step's
// effect, during start-up and recovery, and while steps are held); the
// workers redeliver and re-read as a caller does. It then checks that no
// effect was performed twice, no acknowledged operation was lost or changed
// its terminal state, and no operation stayed running past its deadline.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const usage = `usage: agent-soak <command> [flags]

commands:
  soak          kill the agent at random points under a create/delete/power load
  growth        measure the operation store at growing numbers of tombstones
  load          keep a create/delete/power load on the agent and record its compactions
  precursor     start the agent on a store with many audit events and no audit cursor
  verify-store  open a state directory and look up acknowledged operations
  agent         (internal) run the agent's daemon with a counting executor
`

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "soak":
		return soakCommand(args[1:], stdout, stderr)
	case "growth":
		return growthCommand(args[1:], stdout, stderr)
	case "growth-fill":
		return fillCommand(args[1:], stdout, stderr)
	case "growth-open":
		return openCommand(args[1:], stdout, stderr)
	case "precursor":
		return precursorCommand(args[1:], stdout, stderr)
	case "load":
		return loadCommand(args[1:], stdout, stderr)
	case "verify-store":
		return verifyStoreCommand(args[1:], stdout, stderr)
	case "agent":
		return runWrappedAgent(args[1:], stderr)
	}
	_, _ = fmt.Fprint(stderr, usage)
	return 2
}

// hostFlags are the flags that describe the lab host, shared by the commands.
type hostFlags struct {
	state, work, trust, signerKey, socket, devEnrol string
	agentUser, signerUser                           string
	agentBin, listen                                string
}

func (h *hostFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&h.state, "state", "/mnt/agent-state/state", "the agent's state directory (on the disk under test)")
	fs.StringVar(&h.work, "work", "/var/lib/agent-soak", "directory for logs, results and the effect log")
	fs.StringVar(&h.trust, "trust", "/etc/agent-soak/trust", "root-owned trust configuration directory")
	fs.StringVar(&h.signerKey, "signer-key-dir", "/var/lib/agent-soak-signer", "the signer's key directory")
	fs.StringVar(&h.socket, "socket-dir", "/run/agent-soak-signer", "the signer's socket directory")
	fs.StringVar(&h.devEnrol, "dev-enrolment", "/etc/agent-soak/dev-enrolment.json", "development enrolment file")
	fs.StringVar(&h.agentUser, "agent-user", "soakagent", "the agent's user")
	fs.StringVar(&h.signerUser, "signer-user", "soaksigner", "the signer's user")
	fs.StringVar(&h.agentBin, "agent-bin", "/opt/agent-soak/vrtg-agent", "the agent binary (development build)")
	fs.StringVar(&h.listen, "listen", "127.0.0.1:19460", "the agent's listen address")
}

func (h *hostFlags) dirs() labDirs {
	return labDirs{State: h.state, Work: h.work, Trust: h.trust, SignerKeyDir: h.signerKey, SocketDir: h.socket, DevEnrolment: h.devEnrol}
}

func (h *hostFlags) accounts() (account, account, error) {
	a, err := lookupAccount(h.agentUser)
	if err != nil {
		return account{}, account{}, err
	}
	s, err := lookupAccount(h.signerUser)
	return a, s, err
}

func soakCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("soak", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var h hostFlags
	h.register(fs)
	cfg := soakConfig{}
	mode := fs.String("mode", modeBinary, "agent launch mode: binary (vrtg-agent run) or wrapped (the daemon with a counting executor)")
	fs.IntVar(&cfg.Kills, "kills", 1000, "number of SIGKILLs")
	fs.IntVar(&cfg.Workers, "workers", 8, "concurrent VM lifecycles")
	fs.Int64Var(&cfg.Seed, "seed", time.Now().UnixNano()%1_000_000, "random seed")
	fs.DurationVar(&cfg.OpDeadline, "op-deadline", 30*time.Second, "an acknowledged operation must leave running within this much agent uptime")
	fs.DurationVar(&cfg.MaxRuntime, "max-runtime", 6*time.Hour, "stop killing after this long")
	fs.DurationVar(&cfg.ReadyWait, "ready-wait", 90*time.Second, "how long a started agent may take to answer READY")
	out := fs.String("out", "", "result file (default WORK/result.json)")
	agentArgs := fs.String("agent-args", "", "extra agent flags, space separated (for example --audit-retain-events 5000)")
	checkAudit := fs.Bool("check-audit", false, "compare the agent's audit lines with the store's audit events (needs the default retention, so that nothing is trimmed)")
	plant := fs.Bool("plant-temps", false, "leave stray temporary key files before signer and agent starts and check that they are swept")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	agentU, signerU, err := h.accounts()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	l, err := prepareLab(h.dirs(), agentU, signerU)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "prepare lab:", err)
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "locate self:", err)
		return 2
	}
	bin, extra := h.agentBin, strings.Fields(*agentArgs)
	stepLog := ""
	if *mode == modeWrapped {
		bin, stepLog = self, filepath.Join(h.work, "effects", "steps.log")
		extra = append([]string{"--step-log", stepLog}, extra...)
	}
	var temps tempReport
	if *plant {
		// A signer that is killed and started again sweeps the temporary
		// files an interrupted key write left beside its key.
		for i := 0; i < 10; i++ {
			temps.Planted += len(l.plantTemps(true))
			c, err := l.startSigner(h.agentBin)
			if err != nil {
				_, _ = fmt.Fprintln(stderr, err)
				return 2
			}
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
			temps.SignerRestarts++
			time.Sleep(50 * time.Millisecond)
		}
		temps.Planted += len(l.plantTemps(true))
	}
	signerCmd, err := l.startSigner(h.agentBin)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	defer func() { _ = signerCmd.Process.Kill(); _, _ = signerCmd.Process.Wait() }()
	if *plant {
		for _, p := range l.strayTemps() {
			if filepath.Dir(p) == l.Dirs.SignerKeyDir {
				temps.SignerLeft++
			}
		}
	}
	call, err := newCaller(l.PKI)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	s := &soak{cfg: cfg, lab: l, call: call, led: newLedger(), rng: rand.New(rand.NewSource(cfg.Seed)), // #nosec G404 -- test randomness
		runTag: fmt.Sprintf("%x", time.Now().Unix()%0xfffff), ackEvents: make(chan struct{}, 64)}
	s.sup = newSupervisor(l, *mode, bin, h.listen, extra)
	s.plant, s.temps, s.checkAudit = *plant, &temps, *checkAudit

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	started := time.Now()
	var wg sync.WaitGroup
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go s.worker(wctx, i, &wg)
	}
	chaosErr := s.chaos(ctx)
	if chaosErr != nil {
		_, _ = fmt.Fprintln(stderr, "chaos:", chaosErr)
	}
	// Let the workers finish what they started, against an agent that is no
	// longer killed. The step hold is off.
	s.stopping.Store(true)
	_ = os.Remove(s.holdPath())
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * cfg.OpDeadline):
		wcancel()
		<-done
	}
	res := s.finish(ctx, h, *mode, stepLog, chaosErr, time.Since(started))
	res["stray_temps"] = temps
	path := *out
	if path == "" {
		path = filepath.Join(h.work, "result.json")
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil { // #nosec G306 -- a result file
		_, _ = fmt.Fprintln(stderr, "write result:", err)
	}
	_, _ = fmt.Fprintln(stdout, string(b))
	if res["verdict"] != "PASS" {
		return 1
	}
	return 0
}

// finish runs the final checks and builds the result.
func (s *soak) finish(ctx context.Context, h hostFlags, mode, stepLog string, chaosErr error, took time.Duration) map[string]any {
	s.verifyAcked(ctx, true)
	// Anything still running after the workers are done is stuck.
	for _, r := range s.led.snapshot() {
		if r.Acked && r.Terminal == "" {
			s.led.violate(vStuck, "op %s (%s) never left running", r.ID, r.Kind)
		}
		if !r.Acked {
			s.led.violate(vUnacked, "op %s (%s) was sent %d times and never acknowledged", r.ID, r.Kind, r.Sent)
		}
	}
	graceful := s.sup.stop(30 * time.Second)
	if !graceful {
		s.led.violate(vAgentExit, "the agent did not stop within 30 s of SIGTERM")
	}
	if s.plant {
		s.temps.LeftAtEnd = s.lab.strayTemps()
		if len(s.temps.LeftAtEnd) > 0 {
			s.led.violate(vStrayTemps, "stray temporary files remain: %v", s.temps.LeftAtEnd)
		}
		if s.temps.SignerLeft > 0 {
			s.led.violate(vStrayTemps, "%d stray key temporaries remained after signer restarts", s.temps.SignerLeft)
		}
	}
	// The store, opened independently by the agent's user.
	var opsIn []storeCheckOp
	for _, r := range s.led.snapshot() {
		if r.Acked {
			opsIn = append(opsIn, storeCheckOp{Enrolment: hexStr(r.Spec.id.EnrolmentID), Op: r.ID, Terminal: r.Terminal})
		}
	}
	opsPath := filepath.Join(h.work, "acked-ops.json")
	b, _ := json.Marshal(opsIn)
	_ = os.WriteFile(opsPath, b, 0o644) // #nosec G306 -- a result file
	vargs := []string{"verify-store", "--state", h.state, "--ops", opsPath}
	auditDump := filepath.Join(h.work, "effects", "audit-dump.txt")
	if s.checkAudit {
		vargs = append(vargs, "--dump-audit", auditDump)
	}
	cmd := exec.Command(mustSelf(), vargs...) // #nosec G204 -- this tool
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: s.lab.Agent.credential()}
	storeOut, serr := cmd.CombinedOutput()
	var store storeCheckResult
	if err := json.Unmarshal(lastLine(storeOut), &store); err != nil || serr != nil {
		s.led.violate(vStoreMismatch, "verify-store failed: %v / %v: %s", serr, err, storeOut)
	}
	if len(store.Missing) > 0 {
		s.led.violate(vLostAcked, "%d acknowledged operations missing from the store at the end", len(store.Missing))
	}
	for _, d := range store.Differ {
		s.led.violate(vStoreMismatch, "%s", d)
	}
	if store.State == "lost" {
		s.led.violate(vQuarantined, "journal state lost (%s)", store.LostReason)
	}
	if store.ScrubError != "" {
		s.led.violate(vQuarantined, "scrub: %s", store.ScrubError)
	}
	var auditRep *auditReport
	if s.checkAudit {
		ar, err := s.analyzeAudit(auditDump, s.sup.currentGen())
		if err != nil {
			s.led.violate(vStoreMismatch, "analyse audit: %v", err)
		}
		auditRep = &ar
	}
	rep, err := s.analyzeEffects(stepLog)
	if err != nil {
		s.led.violate(vStoreMismatch, "analyse effects: %v", err)
	}
	// Outcome census.
	outcomes := map[string]int{}
	reasons := map[string]int{}
	byKindOutcome := map[string]int{}
	acked, redeliveries, sent := 0, 0, 0
	for _, r := range s.led.snapshot() {
		sent += r.Sent
		redeliveries += r.Redeliver
		if r.Acked {
			acked++
			outcomes[r.Terminal]++
			if r.Reason != "" {
				reasons[r.Kind.String()+"/"+r.Terminal+"/"+r.Reason]++
			}
			byKindOutcome[r.Kind.String()+"/"+r.Terminal]++
		}
	}
	st := s.sup.startTimes()
	sort.Slice(st, func(i, j int) bool { return st[i] < st[j] })
	s.led.mu.Lock()
	viol := s.led.violations
	details := s.led.details
	s.led.mu.Unlock()
	verdict := "PASS"
	if len(viol) > 0 || chaosErr != nil {
		verdict = "FAIL"
	}
	modes := map[string]int{}
	for _, k := range s.killLog {
		modes[k.Mode]++
	}
	return map[string]any{
		"verdict": verdict, "mode": mode, "seed": s.cfg.Seed, "workers": s.cfg.Workers, "kills_done": len(s.killLog),
		"kills_by_mode": modes, "agent_runs": len(st), "runtime_seconds": took.Seconds(),
		"operations_issued": len(s.led.list), "operations_acknowledged": acked, "deliveries_sent": sent, "redeliveries": redeliveries,
		"outcomes": outcomes, "outcome_reasons": reasons, "outcomes_by_kind": byKindOutcome, "violations": viol, "violation_details": details,
		"audit_check": auditRep, "effects": rep, "store_check": store, "counters": s.counts(), "ack_latency_ms": s.latencySummary(),
		"spawn_to_ready_ms": map[string]float64{"n": float64(len(st)), "p50": ms(percentile(st, 0.5)), "p99": ms(percentile(st, 0.99)), "max": ms(percentile(st, 1))},
		"kill_in_flight_ops_total": func() int {
			n := 0
			for _, k := range s.killLog {
				n += k.InFlight
			}
			return n
		}(),
		"kill_log": s.killLog,
	}
}

func mustSelf() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return p
}

func lastLine(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			return b[i+1:]
		}
	}
	return b
}
