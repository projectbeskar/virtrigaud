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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// precursorRound is what one start of the agent on the pre-cursor store did.
type precursorRound struct {
	Round          int               `json:"round"`
	How            string            `json:"how"`
	SpawnToReady   float64           `json:"spawn_to_ready_seconds"`
	RSSKiBAtReady  int64             `json:"rss_kib_3s_after_ready"`
	ReplayedLines  int               `json:"replayed_event_lines"`
	CappedWarnings int               `json:"audit_replay_capped_lines"`
	CappedLine     string            `json:"audit_replay_capped_text,omitempty"`
	TrimmedLines   int               `json:"store_compacted_lines"`
	Scope          map[string]string `json:"scope,omitempty"`
	ProbeAck       latStats          `json:"gRPC_ack_during_trim,omitempty"`
	Stop           string            `json:"stop"`
	Notes          []string          `json:"notes,omitempty"`
}

// precursorCommand starts the agent on a store whose audit bucket holds many
// events and has no audit cursor (the state a build before the cursor leaves),
// in a scope with a memory limit, and records what each start replays.
func precursorCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("precursor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var h hostFlags
	h.register(fs)
	firstBin := fs.String("first-agent-bin", "", "the agent binary of the build that enrols the store (default: the agent binary)")
	events := fs.Int64("audit-events", 1_200_000, "audit events to write before the first start of the agent under test")
	memMax := fs.String("memory-max", "256M", "MemoryMax of the agent's scope")
	settle := fs.Duration("settle", 60*time.Second, "how long a graceful round runs after READY")
	out := fs.String("out", "", "result file (default WORK/precursor.json)")
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
	ctx := context.Background()
	fb := *firstBin
	if fb == "" {
		fb = h.agentBin
	}
	first := newSupervisor(l, modeBinary, fb, h.listen, nil)
	if _, err := first.start(ctx, 90*time.Second); err != nil {
		_, _ = fmt.Fprintln(stderr, "enrolling start:", err)
		return 1
	}
	first.stop(60 * time.Second)
	pj, _ := json.Marshal(growthParams{Enrolment: hex.EncodeToString(l.PKI.Record.EnrolmentID), ProviderUID: l.PKI.Record.ProviderUID, HostID: "host-a"})
	pp := filepath.Join(h.work, "growth-params.json")
	if err := os.WriteFile(pp, pj, 0o644); err != nil { // #nosec G306 -- a result file
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	t0 := time.Now()
	if _, _, err := h.runAs(agentU, "growth-fill", "--state", h.state, "--params", pp, "--audit-events", strconv.FormatInt(*events, 10)); err != nil {
		_, _ = fmt.Fprintln(stderr, "fill:", err)
		return 1
	}
	fillSeconds := time.Since(t0).Seconds()
	du := measureDisk(h.state)
	o, _, err := h.runAs(agentU, "growth-open", "--state", h.state, "--no-scrub")
	var pre openResult
	if err == nil {
		_ = json.Unmarshal(o, &pre)
	}

	sup := newSupervisor(l, modeBinary, h.agentBin, h.listen, nil)
	sup.memoryMax = *memMax
	var rounds []precursorRound
	plan := []struct{ how string }{{"killed 3 s after READY"}, {"graceful after settle, ops during trim"}, {"graceful after settle"}}
	for i, p := range plan {
		r := precursorRound{Round: i + 1, How: p.how}
		if i == 0 {
			_ = dropCaches()
		}
		t1 := time.Now()
		gen, err := sup.start(ctx, 10*time.Minute)
		if err != nil {
			r.Notes = append(r.Notes, "start: "+err.Error())
			r.Scope = sup.scopeStats(gen)
			sup.kill()
			rounds = append(rounds, r)
			break
		}
		r.SpawnToReady = time.Since(t1).Seconds()
		time.Sleep(3 * time.Second)
		r.RSSKiBAtReady = rssKiB(sup.pid())
		if i == 0 {
			r.Scope = sup.scopeStats(gen)
			sup.kill()
			r.Stop = "SIGKILL"
		} else {
			if i == 1 {
				cp := checkpoint{Tombstones: 0}
				h.probeAgent(ctx, sup, call, 100, &cp)
				r.ProbeAck = cp.GRPCAck
			}
			time.Sleep(*settle)
			r.Scope = sup.scopeStats(gen)
			if sup.stop(60 * time.Second) {
				r.Stop = "SIGTERM, graceful"
			} else {
				r.Stop = "SIGTERM, killed after 60 s"
			}
		}
		r.ReplayedLines = sup.logCount(gen, `"replayed":true`)
		r.CappedWarnings = sup.logCount(gen, "audit replay capped")
		r.TrimmedLines = sup.logCount(gen, "store compacted")
		if r.CappedWarnings > 0 {
			r.CappedLine = firstLineWith(l.Dirs.agentLog(gen), "audit replay capped")
		}
		rounds = append(rounds, r)
	}
	res := map[string]any{"audit_events_written": *events, "fill_seconds": fillSeconds, "disk_before_start": du, "store_before_start": pre,
		"memory_max": *memMax, "rounds": rounds}
	b, _ := json.MarshalIndent(res, "", "  ")
	p := *out
	if p == "" {
		p = filepath.Join(h.work, "precursor.json")
	}
	_ = os.WriteFile(p, b, 0o644) // #nosec G306 -- a result file
	_, _ = fmt.Fprintln(stdout, string(b))
	return 0
}

// firstLineWith returns the first line of path that contains substr.
func firstLineWith(path, substr string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- the lab's log
	if err != nil {
		return ""
	}
	for _, ln := range splitLines(string(b)) {
		if contains(ln, substr) {
			return ln
		}
	}
	return ""
}

func splitLines(s string) []string { return strings.Split(s, "\n") }
func contains(s, sub string) bool  { return strings.Contains(s, sub) }
