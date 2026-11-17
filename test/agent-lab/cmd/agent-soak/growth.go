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
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// growthParams are what the fill and open children need to know about the
// enrolment the agent made of the store.
type growthParams struct {
	Enrolment   string `json:"enrolment"`
	ProviderUID string `json:"provider_uid"`
	HostID      string `json:"host_id"`
}

// fastExec is an executor whose steps do nothing and cost nothing: the growth
// measurement is of the store, not of an executor. Its plans are the fake
// executor's.
type fastExec struct{}

// Plan implements executor.Executor.
func (fastExec) Plan(k providerv1.OperationKind) ([]executor.Step, error) {
	switch k {
	case providerv1.OperationKind_OPERATION_KIND_CREATE:
		return []executor.Step{{Name: stepDefine, Once: true}, {Name: stepStart}}, nil
	case providerv1.OperationKind_OPERATION_KIND_DELETE:
		return []executor.Step{{Name: stepDestroy, Irreversible: true}}, nil
	case providerv1.OperationKind_OPERATION_KIND_POWER_ON, providerv1.OperationKind_OPERATION_KIND_POWER_OFF,
		providerv1.OperationKind_OPERATION_KIND_REBOOT, providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL:
		return []executor.Step{{Name: stepPower}}, nil
	}
	return nil, executor.ErrUnsupportedKind
}

// Run implements executor.Executor; it performs nothing.
func (fastExec) Run(context.Context, executor.Operation, executor.Step) error { return nil }

// Inspect implements executor.Executor.
func (fastExec) Inspect(context.Context, executor.Operation, executor.Step) (executor.Evidence, error) {
	return executor.EvidencePresent, nil
}

// Goal implements executor.Executor.
func (fastExec) Goal(context.Context, executor.Operation, executor.Step) (bool, error) {
	return true, nil
}

// lifecycleKinds is the operation mix of one VM lifecycle: a Create, three
// power operations and a Delete, so 20% Create, 20% Delete, 60% power.
var lifecycleKinds = []providerv1.OperationKind{
	providerv1.OperationKind_OPERATION_KIND_CREATE,
	providerv1.OperationKind_OPERATION_KIND_POWER_ON,
	providerv1.OperationKind_OPERATION_KIND_POWER_OFF,
	providerv1.OperationKind_OPERATION_KIND_REBOOT,
	providerv1.OperationKind_OPERATION_KIND_DELETE,
}

// fillResult is what the fill child reports.
type fillResult struct {
	From          int64     `json:"from"`
	Reached       int64     `json:"reached"`
	Seconds       float64   `json:"seconds"`
	OpsPerSecond  float64   `json:"ops_per_second"`
	AckWindow     latStats  `json:"ack_window"`
	AckAll        latStats  `json:"ack_all"`
	LiveSize      int64     `json:"live_size_bytes"`
	Size          int64     `json:"size_bytes"`
	Samples       []sample  `json:"samples"`
	CloseSeconds  float64   `json:"close_seconds"`
	StartedAt     time.Time `json:"started_at"`
	WindowSize    int       `json:"window_size"`
	StoreLostNote string    `json:"store_lost,omitempty"`
}

// sample is the store's size at a count of finished operations.
type sample struct {
	Ops      int64 `json:"ops"`
	LiveSize int64 `json:"live_size_bytes"`
	Size     int64 `json:"size_bytes"`
}

// latStats summarises latencies in milliseconds.
type latStats struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

func stats(d []time.Duration) latStats {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return latStats{N: len(s), P50: ms(percentile(s, 0.5)), P99: ms(percentile(s, 0.99)), Max: ms(percentile(s, 1))}
}

// openStore opens the enrolled store the way the agent does: open, continuity,
// recovery. It returns the time each took.
func openStore(ctx context.Context, state string, maxBytes int64) (*opstore.Store, [3]time.Duration, error) {
	var t [3]time.Duration
	t0 := time.Now()
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: state}, Executor: fastExec{}, MaxStoreBytes: maxBytes})
	t[0] = time.Since(t0)
	if err != nil {
		return nil, t, fmt.Errorf("open store: %w", err)
	}
	t0 = time.Now()
	if err := st.EstablishContinuity(ctx); err != nil {
		_ = st.Close()
		return nil, t, fmt.Errorf("establish continuity: %w", err)
	}
	t[1] = time.Since(t0)
	t0 = time.Now()
	if _, err := st.Recover(ctx); err != nil {
		_ = st.Close()
		return nil, t, fmt.Errorf("recover: %w", err)
	}
	t[2] = time.Since(t0)
	return st, t, nil
}

func loadParams(path string) (growthParams, []byte, error) {
	var p growthParams
	b, err := os.ReadFile(path) // #nosec G304 -- a path the lab chose
	if err != nil {
		return p, nil, fmt.Errorf("read params: %w", err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, nil, fmt.Errorf("decode params: %w", err)
	}
	enr, err := hex.DecodeString(p.Enrolment)
	return p, enr, err
}

// fillCommand adds finished operations to the store through Admit and Execute,
// the store's own path for creating tombstones, until it holds target. It
// runs as the agent's user.
func fillCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("growth-fill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", "", "state directory")
	params := fs.String("params", "", "enrolment parameters file")
	from := fs.Int64("from", 0, "finished operations already in the store (a multiple of 5)")
	target := fs.Int64("target", 0, "finished operations to reach (a multiple of 5)")
	window := fs.Int("window", 1000, "acknowledgements the window statistics cover (the last ones)")
	every := fs.Int64("sample-every", 10000, "sample the store size every this many operations")
	auditEvents := fs.Int64("audit-events", 0, "write this many audit events (as a build without an audit cursor leaves them), 1000 per commit, before anything else")
	bulk := fs.Bool("bulk", false, "write tombstone-only records, 1000 per commit, and then 200 operations through Admit and Execute (a store whose finished operations were compacted to tombstones)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p, enr, err := loadParams(*params)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	ctx := context.Background()
	// The size threshold is lifted so the measurement can pass it; the
	// samples show where the default one is crossed.
	st, _, err := openStore(ctx, *state, 1<<50)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	res := fillResult{From: *from, WindowSize: *window, StartedAt: time.Now()}
	var acks []time.Duration
	done := *from
	binding := make([]byte, 16)
	holder := make([]byte, 16)
	_, _ = rand.Read(holder)
	t0 := time.Now()
	if *auditEvents > 0 {
		if err := bulkAudit(ctx, st, enr, *auditEvents); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			_ = st.Close()
			return 1
		}
	}
	tailOps := int64(0)
	if *bulk && *target > *from {
		if err := bulkTombstones(ctx, st, enr, p.ProviderUID, holder, *from, *target, *every, &res, t0); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			_ = st.Close()
			return 1
		}
		tailOps = 200
		done = *from
		*target = *from + tailOps
		res.From = *from
	}
	for done < *target {
		vm := fmt.Sprintf("vm-g%d-%d", done/5, *target)
		_, _ = rand.Read(binding)
		b := append([]byte(nil), binding...)
		for _, kind := range lifecycleKinds {
			op := make([]byte, 16)
			_, _ = rand.Read(op)
			d := sha256.Sum256(op)
			req := opstore.Request{ProviderUID: p.ProviderUID, EnrolmentID: enr, OpID: op, VMUID: vm, BindingID: b, Kind: kind, Attempt: 1,
				IntentDigest: d[:], MintEpoch: opstore.Epoch{Counter: 2, Holder: holder}, Actor: "growth"}
			a0 := time.Now()
			dec, err := st.Admit(ctx, req)
			lat := time.Since(a0)
			if err != nil || dec.Outcome != opstore.OutcomeAccepted {
				_, _ = fmt.Fprintf(stderr, "admit %s %s: %+v %v\n", vm, kind, dec, err)
				_ = st.Close()
				return 1
			}
			acks = append(acks, lat)
			if v, err := st.Execute(ctx, opstore.OpRef{EnrolmentID: enr, OpID: op}, nil); err != nil || v.State != opstore.StateCompleted {
				_, _ = fmt.Fprintf(stderr, "execute %s %s: %+v %v\n", vm, kind, v, err)
				_ = st.Close()
				return 1
			}
			done++
			if done%*every == 0 {
				res.Samples = append(res.Samples, sample{Ops: done, LiveSize: st.Journal().LiveSize(), Size: st.Journal().Size()})
				_, _ = fmt.Fprintf(stderr, "%s ops=%d live=%d size=%d rate=%.0f/s\n", time.Now().Format(time.RFC3339), done, st.Journal().LiveSize(),
					st.Journal().Size(), float64(done-*from)/time.Since(t0).Seconds())
			}
		}
	}
	res.Reached, res.Seconds = done, time.Since(t0).Seconds()
	if *bulk {
		res.Reached = *target - tailOps + 0
		res.Reached = res.Reached - 0
	}
	if res.Seconds > 0 {
		res.OpsPerSecond = float64(done-*from) / res.Seconds
	}
	res.AckAll = stats(acks)
	w := acks
	if len(w) > *window {
		w = w[len(w)-*window:]
	}
	res.AckWindow = stats(w)
	res.LiveSize, res.Size = st.Journal().LiveSize(), st.Journal().Size()
	if js, lost := st.Journal().State(); js == journal.StateLost {
		res.StoreLostNote = string(lost)
	}
	c0 := time.Now()
	_ = st.Close()
	res.CloseSeconds = time.Since(c0).Seconds()
	return emit(stdout, res, 0)
}

// openResult is what the open child reports.
type openResult struct {
	OpenSeconds       float64 `json:"open_seconds"`
	ContinuitySeconds float64 `json:"continuity_seconds"`
	RecoverSeconds    float64 `json:"recover_seconds"`
	TotalSeconds      float64 `json:"total_seconds"`
	ScrubSeconds      float64 `json:"scrub_seconds"`
	ScrubError        string  `json:"scrub_error,omitempty"`
	LiveSize          int64   `json:"live_size_bytes"`
	Size              int64   `json:"size_bytes"`
	State             string  `json:"journal_state"`
}

// openCommand times the store's open, continuity checks and recovery, and a
// scrub. It runs as the agent's user.
func openCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("growth-open", flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", "", "state directory")
	noScrub := fs.Bool("no-scrub", false, "skip the scrub")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx := context.Background()
	st, t, err := openStore(ctx, *state, 0)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = st.Close() }()
	r := openResult{OpenSeconds: t[0].Seconds(), ContinuitySeconds: t[1].Seconds(), RecoverSeconds: t[2].Seconds(),
		TotalSeconds: (t[0] + t[1] + t[2]).Seconds(), LiveSize: st.Journal().LiveSize(), Size: st.Journal().Size()}
	js, _ := st.Journal().State()
	r.State = js.String()
	if !*noScrub {
		s0 := time.Now()
		if err := st.Journal().Scrub(ctx); err != nil {
			r.ScrubError = err.Error()
		}
		r.ScrubSeconds = time.Since(s0).Seconds()
	}
	return emit(stdout, r, 0)
}

// diskUse is the state directory's footprint.
type diskUse struct {
	ApparentBytes  int64            `json:"apparent_bytes"`
	AllocatedBytes int64            `json:"allocated_bytes"`
	Files          int              `json:"files"`
	WitnessFiles   int              `json:"witness_files"`
	ByFile         map[string]int64 `json:"large_files_apparent_bytes"`
}

// measureDisk walks dir. Witness files are counted separately.
func measureDisk(dir string) diskUse {
	u := diskUse{ByFile: map[string]int64{}}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		u.Files++
		u.ApparentBytes += fi.Size()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			u.AllocatedBytes += st.Blocks * 512
		}
		rel, _ := filepath.Rel(dir, path)
		if strings.Contains(rel, "witness") {
			u.WitnessFiles++
		} else if fi.Size() > 64<<10 {
			u.ByFile[rel] = fi.Size()
		}
		return nil
	})
	return u
}

func dropCaches() error {
	syscall.Sync()
	return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o200)
}

// runAs runs this tool's subcommand as the agent's user and returns its
// stdout and the wall time.
func (h *hostFlags) runAs(a account, args ...string) ([]byte, time.Duration, error) {
	cmd := exec.Command(mustSelf(), args...) // #nosec G204 -- this tool
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: a.credential()}
	var stderr strings.Builder
	cmd.Stderr = &stderrTee{w: os.Stderr, b: &stderr}
	t0 := time.Now()
	out, err := cmd.Output()
	if err != nil {
		return out, time.Since(t0), fmt.Errorf("%v: %w: %s", args, err, tail(stderr.String(), 400))
	}
	return lastLine(out), time.Since(t0), nil
}

type stderrTee struct {
	w io.Writer
	b *strings.Builder
}

// Write implements io.Writer, copying p to both writers.
func (t *stderrTee) Write(p []byte) (int, error) { _, _ = t.w.Write(p); return t.b.Write(p) }

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// checkpoint is everything measured at one tombstone count.
type checkpoint struct {
	Tombstones       int64            `json:"finished_operations"`
	Disk             diskUse          `json:"state_on_disk"`
	Fill             *fillResult      `json:"fill,omitempty"`
	OpenCold         *openResult      `json:"open_cold,omitempty"`
	OpenColdWall     float64          `json:"open_cold_wall_seconds"`
	OpenWarm         *openResult      `json:"open_warm,omitempty"`
	OpenWarmWall     float64          `json:"open_warm_wall_seconds"`
	AgentReadyCold   float64          `json:"agent_spawn_to_ready_cold_seconds"`
	AgentReadyWarm   float64          `json:"agent_spawn_to_ready_warm_seconds"`
	AgentStopSeconds float64          `json:"agent_stop_seconds"`
	AgentRSSKiB      int64            `json:"agent_rss_kib"`
	GRPCAck          latStats         `json:"grpc_ack_create"`
	GRPCStatus       latStats         `json:"grpc_task_status"`
	GRPCAckRefused   map[string]int   `json:"grpc_ack_refusals,omitempty"`
	ColdRun          *runStats        `json:"agent_run_cold,omitempty"`
	WarmRun          *runStats        `json:"agent_run_warm,omitempty"`
	Notes            []string         `json:"notes,omitempty"`
	Extra            map[string]int64 `json:"extra,omitempty"`
}

// runStats is what one agent run reported about itself.
type runStats struct {
	Gen             int               `json:"gen"`
	Compacted       int               `json:"store_compacted_lines"`
	NearCap         int               `json:"stays_near_threshold_lines"`
	Scope           map[string]string `json:"scope,omitempty"`
	ExitedBeforeRdy bool              `json:"exited_before_ready,omitempty"`
}

func (s *supervisor) runStats(gen int) *runStats {
	return &runStats{Gen: gen, Compacted: s.logCount(gen, "store compacted"), NearCap: s.logCount(gen, "stays near its size threshold"),
		Scope: map[string]string(nil)}
}

// growthCommand measures the operation store at growing numbers of finished
// operations (each leaves a tombstone): size on disk, open time cold and warm,
// the agent's time to READY, and acknowledgement latency, both of the store's
// Admit and over the agent's gRPC path.
func growthCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("growth", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var h hostFlags
	h.register(fs)
	steps := fs.String("steps", "0,10000,100000,1000000", "finished-operation counts to measure at (multiples of 5)")
	bulk := fs.Bool("bulk", false, "fill with tombstone-only records (see growth-fill --bulk)")
	memMax := fs.String("memory-max", "", "run the agent in a systemd scope with this MemoryMax (for example 256M)")
	probe := fs.Int("probe", 200, "Create operations sent over gRPC at each count to measure acknowledgement latency")
	out := fs.String("out", "", "result file (default WORK/growth.json)")
	fs.Parse(args) //nolint:errcheck // ContinueOnError with an exit below
	agentU, signerU, err := h.accounts()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	var counts []int64
	for _, f := range strings.Split(*steps, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
		if err != nil || n%5 != 0 {
			_, _ = fmt.Fprintf(stderr, "bad step %q\n", f)
			return 2
		}
		counts = append(counts, n)
	}
	l, err := prepareLab(h.dirs(), agentU, signerU)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "prepare lab:", err)
		return 2
	}
	signerCmd, err := l.startSigner(h.agentBin)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	defer func() { _ = signerCmd.Process.Kill(); _, _ = signerCmd.Process.Wait() }()
	call, err := newCaller(l.PKI)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	pj, _ := json.Marshal(growthParams{Enrolment: hex.EncodeToString(l.PKI.Record.EnrolmentID), ProviderUID: l.PKI.Record.ProviderUID, HostID: trusttest.HostName})
	paramsPath := filepath.Join(h.work, "growth-params.json")
	if err := os.WriteFile(paramsPath, pj, 0o644); err != nil { // #nosec G306 -- a result file
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	sup := newSupervisor(l, modeBinary, h.agentBin, h.listen, nil)
	ctx := context.Background()
	sup.memoryMax = ""
	// The agent enrols the empty store itself, then stops: the fill then
	// works on a store enrolled the way a real one is.
	if _, err := sup.start(ctx, 90*time.Second); err != nil {
		_, _ = fmt.Fprintln(stderr, "first start:", err)
		return 1
	}
	sup.memoryMax = *memMax
	var all []checkpoint
	var cur int64
	flush := func() {
		b, _ := json.MarshalIndent(map[string]any{"checkpoints": all}, "", "  ")
		p := *out
		if p == "" {
			p = filepath.Join(h.work, "growth.json")
		}
		_ = os.WriteFile(p, b, 0o644) // #nosec G306 -- a result file
	}
	for i, target := range counts {
		cp := checkpoint{Tombstones: target, Notes: nil}
		if i == 0 || target > cur {
			// Fill from the agent's stopped state.
			if sup.pid() != 0 {
				// Measure the empty (or current) store over gRPC first, with the
				// agent that is running, then stop it.
				h.probeAgent(ctx, sup, call, *probe, &cp)
				t0 := time.Now()
				sup.stop(60 * time.Second)
				cp.AgentStopSeconds = time.Since(t0).Seconds()
			}
			if target > cur {
				fillArgs := []string{"growth-fill", "--state", h.state, "--params", paramsPath,
					"--from", strconv.FormatInt(cur, 10), "--target", strconv.FormatInt(target, 10)}
				if *bulk {
					fillArgs = append(fillArgs, "--bulk")
				}
				out, _, err := h.runAs(agentU, fillArgs...)
				if err != nil {
					cp.Notes = append(cp.Notes, "fill failed: "+err.Error())
					all = append(all, cp)
					flush()
					return 1
				}
				var fr fillResult
				if err := json.Unmarshal(out, &fr); err == nil {
					cp.Fill = &fr
				}
				cur = target
			}
		}
		cp.Disk = measureDisk(h.state)
		// Cold open: caches dropped inside this host.
		if err := dropCaches(); err != nil {
			cp.Notes = append(cp.Notes, "drop_caches: "+err.Error())
		}
		o, wall, err := h.runAs(agentU, "growth-open", "--state", h.state)
		if err == nil {
			var r openResult
			if json.Unmarshal(o, &r) == nil {
				cp.OpenCold, cp.OpenColdWall = &r, wall.Seconds()
			}
		} else {
			cp.Notes = append(cp.Notes, "cold open: "+err.Error())
		}
		o, wall, err = h.runAs(agentU, "growth-open", "--state", h.state)
		if err == nil {
			var r openResult
			if json.Unmarshal(o, &r) == nil {
				cp.OpenWarm, cp.OpenWarmWall = &r, wall.Seconds()
			}
		}
		// The real agent's time to READY, cold then warm.
		for _, cold := range []bool{true, false} {
			if cold {
				_ = dropCaches()
			}
			t0 := time.Now()
			gen, err := sup.start(ctx, 10*time.Minute)
			if err != nil {
				cp.Notes = append(cp.Notes, "agent start: "+err.Error())
				rs := sup.runStats(gen)
				rs.ExitedBeforeRdy = true
				if cold {
					cp.ColdRun = rs
				} else {
					cp.WarmRun = rs
				}
				sup.kill()
				continue
			}
			ready := time.Since(t0).Seconds()
			if cold {
				cp.AgentReadyCold = ready
				cp.AgentRSSKiB = rssKiB(sup.pid())
				time.Sleep(5 * time.Second) // the first compaction runs once the agent is ready
				rs := sup.runStats(gen)
				rs.Scope = sup.scopeStats(gen)
				cp.ColdRun = rs
				sup.stop(60 * time.Second)
			} else {
				cp.AgentReadyWarm = ready
				// The warm agent is kept for the gRPC probe below.
			}
		}
		if sup.pid() != 0 {
			cp.AgentRSSKiB = max(cp.AgentRSSKiB, rssKiB(sup.pid()))
			h.probeAgent(ctx, sup, call, *probe, &cp)
			time.Sleep(3 * time.Second)
			if gen := sup.currentGen(); gen > 0 {
				rs := sup.runStats(gen)
				rs.Scope = sup.scopeStats(gen)
				cp.WarmRun = rs
			}
			t0 := time.Now()
			sup.stop(60 * time.Second)
			cp.AgentStopSeconds = time.Since(t0).Seconds()
		}
		cp.Disk = measureDisk(h.state)
		all = append(all, cp)
		flush()
	}
	flush()
	b, _ := json.MarshalIndent(map[string]any{"checkpoints": all}, "", "  ")
	_, _ = fmt.Fprintln(stdout, string(b))
	return 0
}

// rssKiB reads a process's resident set size.
func rssKiB(pid int) int64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				n, _ := strconv.ParseInt(f[0], 10, 64)
				return n
			}
		}
	}
	return 0
}

// probeAgent sends n Create operations to the running agent, one at a time,
// and records the acknowledgement and TaskStatus latencies.
func (h *hostFlags) probeAgent(ctx context.Context, sup *supervisor, call *caller, n int, cp *checkpoint) {
	if n <= 0 || sup.pid() == 0 {
		return
	}
	ac, err := sup.wait(ctx)
	if err != nil {
		return
	}
	var acks, sts []time.Duration
	refused := map[string]int{}
	var tag [4]byte
	_, _ = rand.Read(tag[:])
	for i := 0; i < n; i++ {
		spec, err := call.newOp(providerv1.OperationKind_OPERATION_KIND_CREATE, fmt.Sprintf("vm-p%x-%d-%d", tag, cp.Tombstones, i), randBytes(16), 1)
		if err != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		t0 := time.Now()
		task, err := call.send(cctx, ac.cc, spec)
		lat := time.Since(t0)
		cancel()
		if err != nil {
			refused[fmt.Sprintf("%v/%s", errCode(err), reasonOf(err))]++
			continue
		}
		acks = append(acks, lat)
		cctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		s0 := time.Now()
		_, _ = taskStatus(cctx, ac.cc, task)
		sts = append(sts, time.Since(s0))
		cancel()
	}
	cp.GRPCAck, cp.GRPCStatus = stats(acks), stats(sts)
	if len(refused) > 0 {
		cp.GRPCAckRefused = refused
	}
}

func errCode(err error) codes.Code { return status.Code(err) }

// bulkTombstones writes finished-operation tombstones in commits of 1000, as
// the store's own terminal write does (key: enrolment and op_id; value: the
// Tombstone record), without the operation records.
func bulkTombstones(ctx context.Context, st *opstore.Store, enr []byte, providerUID string, holder []byte, from, to, every int64, res *fillResult, t0 time.Time) error {
	for done := from; done < to; {
		if _, err := st.Journal().Update(ctx, func(tx *journal.Tx) error {
			for k := 0; k < 1000 && done < to; k++ {
				op := make([]byte, 16)
				binary.BigEndian.PutUint64(op, uint64(done)+1<<40) // #nosec G115 -- a counter
				_, _ = rand.Read(op[8:])
				id := &pb.OpIdentity{ProviderUid: providerUID, EnrolmentId: enr, OpId: op, VmUid: fmt.Sprintf("vm-b%d", done/5),
					BindingId: enr, Kind: int32(providerv1.OperationKind_OPERATION_KIND_POWER_ON), Attempt: uint64(done) + 1, IntentDigest: make([]byte, 32), // #nosec G115 -- a counter
					MintEpoch: &pb.Epoch{Counter: 2, Holder: holder}}
				b, err := proto.MarshalOptions{Deterministic: true}.Marshal(&pb.Tombstone{Identity: id, State: opstore.StateCompleted, Irrev: false})
				if err != nil {
					return fmt.Errorf("encode tombstone: %w", err)
				}
				key := append(append([]byte(nil), enr...), op...)
				if err := tx.Put("tombstones", key, b); err != nil {
					return fmt.Errorf("put tombstone: %w", err)
				}
				done++
			}
			return nil
		}); err != nil {
			return fmt.Errorf("commit tombstones: %w", err)
		}
		if done%every < 1000 {
			res.Samples = append(res.Samples, sample{Ops: done, LiveSize: st.Journal().LiveSize(), Size: st.Journal().Size()})
			_, _ = fmt.Fprintf(os.Stderr, "%s bulk ops=%d live=%d rate=%.0f/s\n", time.Now().Format(time.RFC3339), done, st.Journal().LiveSize(), float64(done-from)/time.Since(t0).Seconds())
		}
	}
	return nil
}

// bulkAudit writes n audit events into the store's audit bucket, 1000 per
// commit, with the store's own key (be64(seq) || be32(index)) and record
// type, and no audit cursor: what a build that kept every event and had no
// cursor left.
func bulkAudit(ctx context.Context, st *opstore.Store, enr []byte, n int64) error {
	for done := int64(0); done < n; {
		if _, err := st.Journal().Update(ctx, func(tx *journal.Tx) error {
			for i := 0; i < 1000 && done < n; i++ {
				op := make([]byte, 16)
				binary.BigEndian.PutUint64(op, uint64(done)) // #nosec G115 -- a counter
				rec := &pb.AuditEvent{Event: "terminal", EnrolmentId: enr, OpId: op, VmUid: fmt.Sprintf("vm-a%d", done/5),
					Kind: int32(providerv1.OperationKind_OPERATION_KIND_POWER_ON), Actor: "audit-bulk", State: opstore.StateCompleted}
				b, err := proto.MarshalOptions{Deterministic: true}.Marshal(rec)
				if err != nil {
					return fmt.Errorf("encode audit event: %w", err)
				}
				k := make([]byte, 12)
				binary.BigEndian.PutUint64(k, tx.Seq())
				binary.BigEndian.PutUint32(k[8:], uint32(i)) // #nosec G115 -- at most 1000
				if err := tx.Put("audit", k, b); err != nil {
					return fmt.Errorf("put audit event: %w", err)
				}
				done++
			}
			return nil
		}); err != nil {
			return fmt.Errorf("commit audit events: %w", err)
		}
	}
	return nil
}
