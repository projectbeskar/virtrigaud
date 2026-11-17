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
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	pb "github.com/projectbeskar/virtrigaud/internal/agent/journal/journalpb"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"google.golang.org/protobuf/proto"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// storeCheckOp is one acknowledged operation the store is asked about.
type storeCheckOp struct {
	Enrolment string `json:"enrolment"`
	Op        string `json:"op"`
	Terminal  string `json:"terminal"`
}

// storeCheckResult is the outcome of verify-store.
type storeCheckResult struct {
	Opened     bool     `json:"opened"`
	State      string   `json:"journal_state"`
	LostReason string   `json:"lost_reason,omitempty"`
	ScrubError string   `json:"scrub_error,omitempty"`
	Checked    int      `json:"checked"`
	Missing    []string `json:"missing,omitempty"`
	Differ     []string `json:"differ,omitempty"`
}

// nullExec is an executor that is never asked to do anything: the store is
// opened to be read.
type nullExec struct{}

func (nullExec) Plan(providerv1.OperationKind) ([]executor.Step, error) {
	return nil, executor.ErrUnsupportedKind
}
func (nullExec) Run(context.Context, executor.Operation, executor.Step) error { return nil }
func (nullExec) Inspect(context.Context, executor.Operation, executor.Step) (executor.Evidence, error) {
	return executor.EvidenceUnknown, nil
}
func (nullExec) Goal(context.Context, executor.Operation, executor.Step) (bool, error) {
	return false, nil
}

// outcomeName maps a store state to the TaskStatus outcome name the callers
// recorded.
func outcomeName(s opstore.OpState) string {
	switch s {
	case opstore.StateCompleted:
		return "TASK_OUTCOME_SUCCEEDED"
	case opstore.StateFailedClean:
		return "TASK_OUTCOME_FAILED_CLEAN"
	case opstore.StateFailedDirty:
		return "TASK_OUTCOME_FAILED_DIRTY"
	case opstore.StateOutcomeUnknown:
		return "TASK_OUTCOME_UNKNOWN"
	case opstore.StateAccepted, opstore.StateExecuting:
		return "TASK_OUTCOME_RUNNING"
	}
	return s.String()
}

// verifyStoreCommand opens the agent's operation store read-only in effect
// (it opens it, runs its local checks and a scrub, and commits nothing) and
// looks up every acknowledged operation. It must run as the agent's user.
func verifyStoreCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify-store", flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", "", "the agent's state directory")
	opsFile := fs.String("ops", "", "JSON file with the acknowledged operations")
	dumpAudit := fs.String("dump-audit", "", "write the store's audit events (seq, index, event, op) to this file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw, err := os.ReadFile(*opsFile) // #nosec G304 -- a path the lab chose
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "read ops:", err)
		return 2
	}
	var ops []storeCheckOp
	if err := json.Unmarshal(raw, &ops); err != nil {
		_, _ = fmt.Fprintln(stderr, "decode ops:", err)
		return 2
	}
	res := storeCheckResult{}
	ctx := context.Background()
	st, err := opstore.Open(ctx, opstore.Options{Journal: journal.Options{Dir: *state}, Executor: nullExec{}})
	if err != nil {
		res.State = "open_error: " + err.Error()
		return emit(stdout, res, 1)
	}
	defer func() { _ = st.Close() }()
	res.Opened = true
	js, lost := st.Journal().State()
	res.State, res.LostReason = fmt.Sprint(js), string(lost)
	if err := st.Journal().Scrub(ctx); err != nil {
		res.ScrubError = err.Error()
	}
	if *dumpAudit != "" {
		f, err := os.Create(*dumpAudit) // #nosec G304 -- a path the lab chose
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "dump audit:", err)
			return 2
		}
		w := bufio.NewWriter(f)
		err = st.Journal().View(ctx, func(tx *journal.Tx) error {
			return tx.ForEach("audit", nil, func(k, v []byte) error {
				rec := &pb.AuditEvent{}
				if err := proto.Unmarshal(v, rec); err != nil {
					return err
				}
				_, werr := fmt.Fprintf(w, "%d %d %s %x\n", binary.BigEndian.Uint64(k), binary.BigEndian.Uint32(k[8:]), rec.GetEvent(), rec.GetOpId())
				return werr
			})
		})
		_ = w.Flush()
		_ = f.Close()
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "dump audit:", err)
			return 1
		}
	}
	for _, o := range ops {
		enr, err1 := hex.DecodeString(o.Enrolment)
		id, err2 := hex.DecodeString(o.Op)
		if err1 != nil || err2 != nil {
			_, _ = fmt.Fprintln(stderr, "bad op id", o.Op)
			return 2
		}
		v, ok, err := st.Lookup(ctx, opstore.OpRef{EnrolmentID: enr, OpID: id})
		res.Checked++
		switch {
		case err != nil:
			res.Differ = append(res.Differ, fmt.Sprintf("%s: lookup: %v", o.Op, err))
		case !ok:
			res.Missing = append(res.Missing, o.Op)
		case o.Terminal != "" && outcomeName(v.State) != o.Terminal:
			res.Differ = append(res.Differ, fmt.Sprintf("%s: observed %s, store %s", o.Op, o.Terminal, outcomeName(v.State)))
		}
	}
	code := 0
	if len(res.Missing) > 0 || len(res.Differ) > 0 || res.ScrubError != "" || js == journal.StateLost {
		code = 1
	}
	return emit(stdout, res, code)
}

func emit(w io.Writer, v any, code int) int {
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintln(w, string(b))
	return code
}

// effectLine is one line of a step or effect log.
type effectLine struct{ op, step, vm string }

// readLines parses "<op> <step> <vm>" lines.
func readLines(path string) ([]effectLine, error) {
	f, err := os.Open(path) // #nosec G304 -- a path the lab chose
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []effectLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) == 3 {
			out = append(out, effectLine{p[0], p[1], p[2]})
		}
	}
	return out, sc.Err()
}

// Step classes of the fake executor, by name.
const (
	stepDefine  = "define"
	stepStart   = "start"
	stepDestroy = "destroy"
	stepPower   = "power"
)

// effectReport is the analysis of the effect (and step) logs.
type effectReport struct {
	Lines              int            `json:"effect_lines"`
	ByStep             map[string]int `json:"effects_by_step"`
	StepInvocations    int            `json:"step_invocation_lines,omitempty"`
	IdempotentReruns   int            `json:"idempotent_step_reruns"`
	IdempotentRerunOps int            `json:"idempotent_step_rerun_ops"`
	Intents            int            `json:"intent_lines"`
	IntentOnly         int            `json:"completed_ops_with_intent_line_but_no_effect_line"`
	IntentWithoutDone  int            `json:"intents_without_effect_line"`
	OrphanVMFiles      int            `json:"vm_files_of_unknown_vms"`
	VMChecked          int            `json:"vms_compared_with_outcomes"`
	VMSkipped          int            `json:"vms_skipped_unresolved_operations"`
}

// analyzeEffects checks the effect log (and, in wrapped mode, the step
// invocation log) against the operations the caller issued, and the fake VM
// files against the outcomes.
func (s *soak) analyzeEffects(stepLog string) (effectReport, error) {
	rep := effectReport{ByStep: map[string]int{}}
	eff, err := readLines(filepath.Join(s.lab.Dirs.effects(), "effects.log"))
	if err != nil {
		return rep, err
	}
	rep.Lines = len(eff)
	recs := s.led.snapshot()
	byID := map[string]opRec{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	type key struct{ op, step string }
	counts := map[key]int{}
	for _, e := range eff {
		rep.ByStep[e.step]++
		counts[key{e.op, e.step}]++
		r, ok := byID[e.op]
		switch {
		case ok && r.Refused:
			s.led.violate(vOrphanEffect, "effect %s %s for an operation the host refused as store full", e.op, e.step)
		case !ok:
			s.led.violate(vOrphanEffect, "effect %s %s on VM %s belongs to no operation the caller issued", e.op, e.step, e.vm)
		case r.VM != e.vm:
			s.led.violate(vOrphanEffect, "effect %s %s names VM %s, the operation is for %s", e.op, e.step, e.vm, r.VM)
		}
	}
	// The intents log: the fake executor appends one line before each effect
	// is performed, so every effect line has an intent line before it.
	intentsPath := filepath.Join(s.lab.Dirs.effects(), "intents.log")
	ints, err := readLines(intentsPath)
	if err != nil {
		return rep, err
	}
	if len(eff) > 0 && len(ints) == 0 {
		return rep, fmt.Errorf("the fake executor wrote %d effect lines and no intents log at %s", len(eff), intentsPath)
	}
	rep.Intents = len(ints)
	icounts := map[key]int{}
	for _, e := range ints {
		icounts[key{e.op, e.step}]++
		if _, ok := byID[e.op]; !ok {
			s.led.violate(vOrphanEffect, "intent %s %s on VM %s belongs to no operation the caller issued", e.op, e.step, e.vm)
		}
	}
	for k, n := range icounts {
		if n > counts[k] {
			rep.IntentWithoutDone += n - counts[k]
		}
		if n < counts[k] {
			s.led.violate(vOrphanEffect, "op %s step %s has %d effect lines and only %d intent lines", k.op, k.step, counts[k], n)
		}
		if n > 1 && k.step == stepDefine {
			s.led.violate(vDupOnce, "op %s logged %d intents for %s", k.op, n, k.step)
		}
		if n > 1 && k.step == stepDestroy {
			s.led.violate(vDupIrrev, "op %s logged %d intents for %s", k.op, n, k.step)
		}
	}
	reruns := map[string]bool{}
	for k, n := range counts {
		if n < 2 {
			continue
		}
		switch k.step {
		case stepDefine:
			s.led.violate(vDupOnce, "op %s performed %s %d times", k.op, k.step, n)
		case stepDestroy:
			s.led.violate(vDupIrrev, "op %s performed %s %d times", k.op, k.step, n)
		default:
			rep.IdempotentReruns += n - 1
			reruns[k.op] = true
		}
	}
	rep.IdempotentRerunOps = len(reruns)
	// Completed operations must have performed their effect.
	for _, r := range recs {
		if r.Terminal != "TASK_OUTCOME_SUCCEEDED" {
			continue
		}
		var need string
		switch r.Kind {
		case providerv1.OperationKind_OPERATION_KIND_CREATE:
			need = stepDefine
		case providerv1.OperationKind_OPERATION_KIND_DELETE:
			need = stepDestroy
		default:
			need = stepPower
		}
		if counts[key{r.ID, need}] > 0 {
			continue
		}
		if icounts[key{r.ID, need}] > 0 {
			// Intent logged, effect line not: a kill between the effect and
			// its line. Accounted for by the intent.
			rep.IntentOnly++
			continue
		}
		s.led.violate(vMissingEffect, "op %s (%s) completed with neither an effect line nor an intent line", r.ID, r.Kind)
	}
	// Step invocations (wrapped mode): a once or irreversible step is called
	// at most once.
	if stepLog != "" {
		inv, err := readLines(stepLog)
		if err != nil {
			return rep, err
		}
		rep.StepInvocations = len(inv)
		ic := map[key]int{}
		for _, e := range inv {
			ic[key{e.op, e.step}]++
		}
		for k, n := range ic {
			if n > 1 && (k.step == stepDefine || k.step == stepDestroy) {
				s.led.violate(vStepReinvoked, "op %s: step %s invoked %d times", k.op, k.step, n)
			}
		}
	}
	s.checkVMFiles(&rep, recs)
	return rep, nil
}

// vmFile is the fake executor's VM file.
type vmFile struct {
	Binding    string `json:"binding"`
	Power      string `json:"power"`
	Generation uint64 `json:"generation"`
}

// checkVMFiles compares each fake VM with what the succeeded operations on it
// imply. VMs with an operation that ended unknown or failed are skipped: their
// state is not implied.
func (s *soak) checkVMFiles(rep *effectReport, recs []opRec) {
	byVM := map[string][]opRec{}
	for _, r := range recs {
		byVM[r.VM] = append(byVM[r.VM], r)
	}
	vmDir := filepath.Join(s.lab.Dirs.effects(), "vms")
	entries, _ := os.ReadDir(vmDir)
	files := map[string]vmFile{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(vmDir, e.Name())) // #nosec G304 -- the lab's directory
		if err != nil {
			continue
		}
		var v vmFile
		if json.Unmarshal(b, &v) == nil {
			files[name] = v
		}
	}
	for name := range files {
		if _, ok := byVM[name]; !ok {
			rep.OrphanVMFiles++
			s.led.violate(vVMState, "VM file %s belongs to no VM the caller used", name)
		}
	}
	var uids []string
	for uid := range byVM {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		ops := byVM[uid]
		clean := true
		exists, power := false, ""
		for _, r := range ops { // issue order
			if r.Terminal == "" && r.Acked {
				clean = false
				break
			}
			if !r.Acked {
				continue
			}
			if r.Terminal != "TASK_OUTCOME_SUCCEEDED" {
				clean = false
				break
			}
			switch r.Kind {
			case providerv1.OperationKind_OPERATION_KIND_CREATE:
				exists, power = true, "on"
			case providerv1.OperationKind_OPERATION_KIND_DELETE:
				exists, power = false, ""
			case providerv1.OperationKind_OPERATION_KIND_POWER_ON, providerv1.OperationKind_OPERATION_KIND_REBOOT:
				power = "on"
			default:
				power = "off"
			}
		}
		if !clean {
			rep.VMSkipped++
			continue
		}
		rep.VMChecked++
		f, has := files[uid]
		switch {
		case has != exists:
			s.led.violate(vVMState, "VM %s: file present=%v, outcomes imply %v", uid, has, exists)
		case has && f.Power != power:
			s.led.violate(vVMState, "VM %s: power %s, outcomes imply %s", uid, f.Power, power)
		}
	}
}

func hexStr(b []byte) string { return hex.EncodeToString(b) }
