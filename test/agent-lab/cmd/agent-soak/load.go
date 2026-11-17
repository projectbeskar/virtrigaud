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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// loadSample is one reading of a sustained load run.
type loadSample struct {
	Seconds     float64 `json:"seconds"`
	StateBytes  int64   `json:"database_file_bytes"`
	Operations  int     `json:"operations_issued"`
	RSSKiB      int64   `json:"agent_rss_kib"`
	Compactions int     `json:"store_compacted_lines"`
}

// loadCommand keeps a create/delete/power load on the agent, without kills,
// for a long time, optionally on a store filled with tombstones first, and
// records the agent's compaction log lines, the database file size and the
// agent's memory.
func loadCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var h hostFlags
	h.register(fs)
	duration := fs.Duration("duration", 70*time.Minute, "how long to keep the load")
	workers := fs.Int("workers", 1, "concurrent VM lifecycles")
	prefill := fs.Int64("prefill", 0, "tombstones to write before the load (a multiple of 5)")
	memMax := fs.String("memory-max", "", "run the agent in a systemd scope with this MemoryMax")
	agentArgs := fs.String("agent-args", "", "extra agent flags, space separated")
	expectCap := fs.Bool("expect-cap", false, "a store-full refusal is an expected answer; check that it is definite and loses nothing")
	sample := fs.Duration("sample", 30*time.Second, "sampling period")
	out := fs.String("out", "", "result file (default WORK/load.json)")
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
	sc, err := l.startSigner(h.agentBin)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	defer func() { _ = sc.Process.Kill(); _, _ = sc.Process.Wait() }()
	call, err := newCaller(l.PKI)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	sup := newSupervisor(l, modeBinary, h.agentBin, h.listen, strings.Fields(*agentArgs))
	ctx := context.Background()
	if _, err := sup.start(ctx, 90*time.Second); err != nil { // enrols the empty store
		_, _ = fmt.Fprintln(stderr, "first start:", err)
		return 1
	}
	sup.stop(60 * time.Second)
	if *prefill > 0 {
		pj, _ := json.Marshal(growthParams{Enrolment: fmt.Sprintf("%x", l.PKI.Record.EnrolmentID), ProviderUID: l.PKI.Record.ProviderUID, HostID: "host-a"})
		pp := filepath.Join(h.work, "growth-params.json")
		if err := os.WriteFile(pp, pj, 0o644); err != nil { // #nosec G306 -- a result file
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		if _, _, err := h.runAs(agentU, "growth-fill", "--bulk", "--state", h.state, "--params", pp, "--from", "0", "--target", strconv.FormatInt(*prefill, 10)); err != nil {
			_, _ = fmt.Fprintln(stderr, "prefill:", err)
			return 1
		}
	}
	sup.memoryMax = *memMax
	t0 := time.Now()
	gen, err := sup.start(ctx, 10*time.Minute)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "start under load:", err)
		return 1
	}
	startSeconds := time.Since(t0).Seconds()
	s := &soak{cfg: soakConfig{Workers: *workers, Seed: time.Now().UnixNano() % 1_000_000, OpDeadline: 60 * time.Second}, lab: l, call: call, led: newLedger(),
		rng: rand.New(rand.NewSource(1)), runTag: fmt.Sprintf("%x", time.Now().Unix()%0xfffff), ackEvents: make(chan struct{}, 64)} // #nosec G404 -- test randomness
	s.sup = sup
	s.expectCap = *expectCap
	wctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go s.worker(wctx, i, &wg)
	}
	var samples []loadSample
	start := time.Now()
	for time.Since(start) < *duration {
		time.Sleep(*sample)
		du := measureDisk(h.state)
		var db int64
		for _, n := range du.ByFile {
			db += n
		}
		s.led.mu.Lock()
		n := len(s.led.list)
		s.led.mu.Unlock()
		samples = append(samples, loadSample{Seconds: time.Since(start).Seconds(), StateBytes: db, Operations: n, RSSKiB: rssKiB(sup.pid()),
			Compactions: sup.logCount(gen, "store compacted")})
	}
	s.stopping.Store(true)
	cancel()
	wg.Wait()
	rs := sup.runStats(gen)
	rs.Scope = sup.scopeStats(gen)
	graceful := sup.stop(60 * time.Second)
	var lines []string
	var compactAt []time.Time
	if b, err := os.ReadFile(l.Dirs.agentLog(gen)); err == nil { // #nosec G304 -- the lab's log
		for _, ln := range strings.Split(string(b), "\n") {
			if strings.Contains(ln, "store compacted") || strings.Contains(ln, "stays near its size threshold") || strings.Contains(ln, "compaction") {
				lines = append(lines, ln)
			}
			if strings.Contains(ln, "store compacted") {
				var j struct {
					Time time.Time `json:"time"`
				}
				if json.Unmarshal([]byte(ln), &j) == nil {
					compactAt = append(compactAt, j.Time)
				}
			}
		}
	}
	latency := s.latencyAroundCompactions(compactAt)

	// The store after the load: opened independently, then the agent started
	// again, every acknowledged operation read back, a sample redelivered.
	var opsIn []storeCheckOp
	for _, r := range s.led.snapshot() {
		if r.Acked {
			opsIn = append(opsIn, storeCheckOp{Enrolment: hexStr(r.Spec.id.EnrolmentID), Op: r.ID, Terminal: r.Terminal})
		}
	}
	opsPath := filepath.Join(h.work, "acked-ops.json")
	b0, _ := json.Marshal(opsIn)
	_ = os.WriteFile(opsPath, b0, 0o644)                                                   // #nosec G306 -- a result file
	vcmd := exec.Command(mustSelf(), "verify-store", "--state", h.state, "--ops", opsPath) // #nosec G204 -- this tool
	vcmd.SysProcAttr = &syscall.SysProcAttr{Credential: l.Agent.credential()}
	vout, verr := vcmd.CombinedOutput()
	var store storeCheckResult
	if err := json.Unmarshal(lastLine(vout), &store); err != nil || verr != nil {
		s.led.violate(vStoreMismatch, "verify-store failed: %v / %v: %s", verr, err, vout)
	}
	if len(store.Missing) > 0 {
		s.led.violate(vLostAcked, "%d acknowledged operations missing after the load", len(store.Missing))
	}
	for _, d := range store.Differ {
		s.led.violate(vStoreMismatch, "%s", d)
	}
	t1 := time.Now()
	gen2, rerr := sup.start(ctx, 10*time.Minute)
	restartSeconds := time.Since(t1).Seconds()
	redelivered := 0
	if rerr != nil {
		s.led.violate(vRestartFailed, "%v", rerr)
	} else {
		s.verifyAcked(ctx, true)
		recs := s.led.snapshot()
		for i := range recs {
			if !recs[i].Acked || redelivered >= 1000 {
				continue
			}
			if i%7 == 0 {
				s.led.mu.Lock()
				rec := s.led.list[i]
				s.led.mu.Unlock()
				s.redeliver(ctx, rec)
				redelivered++
			}
		}
		time.Sleep(2 * time.Second)
		rs2 := sup.runStats(gen2)
		_ = rs2
		sup.stop(60 * time.Second)
	}
	rep, aerr := s.analyzeEffects("")
	if aerr != nil {
		s.led.violate(vStoreMismatch, "analyse effects: %v", aerr)
	}
	capMsg, _ := s.capMsg.Load().(string)
	s.led.mu.Lock()
	viol, det, nops := s.led.violations, s.led.details, len(s.led.list)
	s.led.mu.Unlock()
	res := map[string]any{"duration_seconds": time.Since(start).Seconds(), "start_to_ready_seconds": startSeconds, "prefill_tombstones": *prefill,
		"workers": *workers, "operations": nops, "outcomes": s.counts(), "violations": viol, "violation_details": det, "run": rs,
		"agent_stopped_gracefully": graceful, "store_full_message": capMsg, "latency_around_compactions": latency,
		"store_check_after_load": store, "restart_after_load_seconds": restartSeconds, "redelivered_after_restart": redelivered,
		"effects": rep, "compaction_log_lines": lines, "samples": samples}
	b, _ := json.MarshalIndent(res, "", "  ")
	p := *out
	if p == "" {
		p = filepath.Join(h.work, "load.json")
	}
	_ = os.WriteFile(p, b, 0o644) // #nosec G306 -- a result file
	_, _ = fmt.Fprintln(stdout, string(b))
	return 0
}

// latencyAroundCompactions splits the acknowledgement latencies into those of
// acknowledgements received within 3 s before a compaction's log line (during
// it) and the rest, and reports the largest gap between two acknowledgements.
func (s *soak) latencyAroundCompactions(at []time.Time) map[string]any {
	s.latMu.Lock()
	lats := append([]time.Duration(nil), s.lats...)
	ts := append([]time.Time(nil), s.latT...)
	s.latMu.Unlock()
	var in, out []time.Duration
	var maxGap, maxGapIn time.Duration
	for i := range lats {
		during := false
		for _, c := range at {
			if d := c.Sub(ts[i]); d >= -500*time.Millisecond && d <= 3*time.Second {
				during = true
				break
			}
		}
		if during {
			in = append(in, lats[i])
		} else {
			out = append(out, lats[i])
		}
		if i > 0 {
			g := ts[i].Sub(ts[i-1])
			maxGap = max(maxGap, g)
			if during {
				maxGapIn = max(maxGapIn, g)
			}
		}
	}
	return map[string]any{"compactions": len(at), "during": stats(in), "outside": stats(out), "all": stats(lats),
		"max_gap_between_acks_ms": ms(maxGap), "max_gap_during_ms": ms(maxGapIn)}
}
