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
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/test/spike1b/workload"
)

// Lifecycle stages a tracked VM passes, in order.
const (
	stCreated    = "created"    // the Create call returned
	stReconciled = "reconciled" // the manager's finalizer is on it (first reconcile)
	stPending    = "pending"    // status.placement.pendingHost (or host) recorded
	stBound      = "bound"      // status.id recorded
	stReady      = "ready"      // Ready=True
	stDeleteReq  = "deletereq"  // the Delete call returned
	stGone       = "gone"       // the object is gone (finalizer released)
	readyCond    = "Ready"
	placedCond   = "Placed"
)

// stages are the transitions reported, in order.
var stages = []string{stCreated, stReconciled, stPending, stBound, stReady, stDeleteReq, stGone}

// vmTrack is one VM's observed timeline (times relative to its submit).
type vmTrack struct {
	key       string
	submitted time.Time
	at        map[string]time.Time
	placed    string // last Placed reason seen
	failure   string // create error, if any
	deleting  bool   // a delete has been dispatched
}

// tracker follows the VMs of one run through an informer.
type tracker struct {
	mu     sync.Mutex
	vms    map[string]*vmTrack
	counts map[string]int
}

// newTracker starts an informer on VMs labelled run and returns once synced.
func newTracker(ctx context.Context, e *env, run string) (*tracker, error) {
	t := &tracker{vms: map[string]*vmTrack{}, counts: map[string]int{}}
	sel := labels.SelectorFromSet(labels.Set{labelRun: run})
	c, err := cache.New(e.cfg, cache.Options{Scheme: e.scheme, ByObject: map[client.Object]cache.ByObject{
		&v1beta1.VirtualMachine{}: {Label: sel},
	}})
	if err != nil {
		return nil, err
	}
	inf, err := c.GetInformer(ctx, &v1beta1.VirtualMachine{})
	if err != nil {
		return nil, err
	}
	_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o interface{}) { t.observe(o, false) },
		UpdateFunc: func(_, o interface{}) { t.observe(o, false) },
		DeleteFunc: func(o interface{}) { t.observe(o, true) },
	})
	if err != nil {
		return nil, err
	}
	go func() { _ = c.Start(ctx) }()
	if !c.WaitForCacheSync(ctx) {
		return nil, fmt.Errorf("tracker cache did not sync")
	}
	return t, nil
}

// submit registers a VM before its Create is sent.
func (t *tracker) submit(key string) *vmTrack {
	t.mu.Lock()
	defer t.mu.Unlock()
	vt := &vmTrack{key: key, submitted: time.Now(), at: map[string]time.Time{}}
	t.vms[key] = vt
	return vt
}

// mark records stage for key once.
func (t *tracker) mark(key, stage string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.markLocked(key, stage, now)
}

// markLocked is mark with t.mu held.
func (t *tracker) markLocked(key, stage string, now time.Time) {
	vt := t.vms[key]
	if vt == nil {
		return
	}
	if _, done := vt.at[stage]; !done {
		vt.at[stage] = now
		t.counts[stage]++
	}
}

// observe records what an informer event shows.
func (t *tracker) observe(o interface{}, deleted bool) {
	if d, ok := o.(toolscache.DeletedFinalStateUnknown); ok {
		o = d.Obj
	}
	vm, ok := o.(*v1beta1.VirtualMachine)
	if !ok {
		return
	}
	now := time.Now()
	key := vm.Namespace + "/" + vm.Name
	t.mu.Lock()
	defer t.mu.Unlock()
	if deleted {
		t.markLocked(key, stGone, now)
		return
	}
	if slices.Contains(vm.Finalizers, v1beta1.VirtualMachineFinalizer) {
		t.markLocked(key, stReconciled, now)
	}
	if pl := vm.Status.Placement; pl != nil && (pl.PendingHost != "" || pl.Host != "") {
		t.markLocked(key, stPending, now)
	}
	if vm.Status.ID != "" {
		t.markLocked(key, stBound, now)
	}
	if meta.IsStatusConditionTrue(vm.Status.Conditions, readyCond) {
		t.markLocked(key, stReady, now)
	}
	if vt := t.vms[key]; vt != nil {
		if c := meta.FindStatusCondition(vm.Status.Conditions, placedCond); c != nil {
			vt.placed = c.Reason
		}
	}
}

// count returns how many VMs reached stage.
func (t *tracker) count(stage string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[stage]
}

// newRunVM returns VM i of a run: medium class, round-robin over tenant
// namespaces and the layout's Providers.
func newRunVM(l *layout, run string, i int, class string) *v1beta1.VirtualMachine {
	pl := l.Providers[i%len(l.Providers)]
	return &v1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%06d", run, i),
			Namespace: workload.Namespace(i % l.Namespaces),
			Labels:    map[string]string{labelRun: run},
		},
		Spec: v1beta1.VirtualMachineSpec{
			ProviderRef: v1beta1.ObjectRef{Name: pl.Name, Namespace: infraNamespace},
			ClassRef:    v1beta1.ObjectRef{Name: class},
			ImageRef:    &v1beta1.ObjectRef{Name: imageName},
			PowerState:  v1beta1.PowerStateOn,
		},
	}
}

// createTracked submits and creates vm.
func (t *tracker) createTracked(ctx context.Context, e *env, vm *v1beta1.VirtualMachine) {
	key := vm.Namespace + "/" + vm.Name
	vt := t.submit(key)
	if err := e.c.Create(ctx, vm); err != nil {
		t.mu.Lock()
		vt.failure = err.Error()
		t.mu.Unlock()
		return
	}
	t.mark(key, stCreated, time.Now())
}

// deleteTracked deletes the VM key and marks the request.
func (t *tracker) deleteTracked(ctx context.Context, e *env, key string) {
	ns, name, _ := cutKey(key)
	vm := &v1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := e.c.Delete(ctx, vm); err != nil && !apierrors.IsNotFound(err) {
		return
	}
	t.mark(key, stDeleteReq, time.Now())
}

// cutKey splits namespace/name.
func cutKey(key string) (string, string, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return "", key, false
}

// summary is a run's lifecycle summary.
type summary struct {
	Run           string                        `json:"run"`
	Submitted     int                           `json:"submitted"`
	Reached       map[string]int                `json:"reached"`
	LatencyMS     map[string]map[string]float64 `json:"latencyMs"`
	Throughput    map[string]float64            `json:"throughputPerSec"`
	PendingReason map[string]int                `json:"stuckByPlacedReason"`
	CreateErrors  int                           `json:"createErrors"`
	WallSeconds   float64                       `json:"wallSeconds"`
}

// summarize computes percentiles of each stage relative to submit, and
// throughput of each stage (VMs that reached it / span from first submit to
// last arrival).
func (t *tracker) summarize(run string, start time.Time) summary {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := summary{Run: run, Submitted: len(t.vms), Reached: map[string]int{}, LatencyMS: map[string]map[string]float64{},
		Throughput: map[string]float64{}, PendingReason: map[string]int{}, WallSeconds: time.Since(start).Seconds()}
	for _, st := range stages {
		var ds []float64
		var last time.Time
		for _, vt := range t.vms {
			at, ok := vt.at[st]
			if !ok {
				continue
			}
			ref := vt.submitted
			if st == stGone {
				if req, ok := vt.at[stDeleteReq]; ok {
					ref = req
				}
			}
			ds = append(ds, float64(at.Sub(ref).Milliseconds()))
			if at.After(last) {
				last = at
			}
		}
		s.Reached[st] = len(ds)
		if len(ds) == 0 {
			continue
		}
		slices.Sort(ds)
		pct := func(q float64) float64 { return ds[min(len(ds)-1, int(q*float64(len(ds))))] }
		s.LatencyMS[st] = map[string]float64{"p50": pct(0.5), "p90": pct(0.9), "p99": pct(0.99), "max": ds[len(ds)-1]}
		if span := last.Sub(start).Seconds(); span > 0 {
			s.Throughput[st] = float64(len(ds)) / span
		}
	}
	for _, vt := range t.vms {
		if vt.failure != "" {
			s.CreateErrors++
			continue
		}
		if _, ok := vt.at[stReady]; !ok {
			if _, del := vt.at[stDeleteReq]; !del {
				s.PendingReason[vt.placed]++
			}
		}
	}
	return s
}

// writeResults writes the per-VM CSV and the summary JSON into dir.
func (t *tracker) writeResults(dir, run string, s summary) error {
	if err := os.MkdirAll(dir, 0o750); err != nil { // #nosec G703 -- the path is the operator's own --out flag of this local benchmark CLI
		return err
	}
	f, err := os.Create(filepath.Join(dir, run+"-vms.csv")) // #nosec G703 -- the path is the operator's own --out flag of this local benchmark CLI
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	_ = w.Write(append([]string{"vm", "placed_reason", "error"}, stages...))
	t.mu.Lock()
	for _, vt := range t.vms {
		row := []string{vt.key, vt.placed, vt.failure}
		for _, st := range stages {
			if at, ok := vt.at[st]; ok {
				row = append(row, strconv.FormatInt(at.Sub(vt.submitted).Milliseconds(), 10))
			} else {
				row = append(row, "")
			}
		}
		_ = w.Write(row)
	}
	t.mu.Unlock()
	w.Flush()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, run+"-summary.json"), data, 0o600) // #nosec G703 -- the path is the operator's own --out flag of this local benchmark CLI
}

// burstCmd creates --count VMs at once and follows them to Ready, then
// optionally deletes them and follows them until they are gone.
func burstCmd(fs *flag.FlagSet) func(context.Context, *env) error {
	count := fs.Int("count", 100, "VMs to create")
	run := fs.String("run", "", "run name (VM name prefix and label); required")
	class := fs.String("class", "medium", "VMClass")
	creators := fs.Int("creators", 50, "concurrent Create calls")
	timeout := fs.Duration("timeout", 20*time.Minute, "give up waiting for Ready after this")
	del := fs.Bool("delete", true, "delete the VMs afterwards and time the deletion")
	deleteAfter := fs.Duration("delete-after", 0, "delete every VM this long after its create, Ready or not (delete while creating)")
	out := fs.String("out", ".", "results directory")
	return func(ctx context.Context, e *env) error {
		if *run == "" {
			return fmt.Errorf("--run is required")
		}
		l, err := loadLayout(ctx, e)
		if err != nil {
			return err
		}
		t, err := newTracker(ctx, e, *run)
		if err != nil {
			return err
		}
		start := time.Now()
		_ = parallel(ctx, *count, *creators, func(i int) error {
			t.createTracked(ctx, e, newRunVM(l, *run, i, *class))
			return nil
		})
		fmt.Printf("%s: %d creates submitted in %s\n", *run, *count, time.Since(start).Round(time.Millisecond))
		if *deleteAfter > 0 {
			time.Sleep(time.Until(start.Add(*deleteAfter)))
			_ = parallel(ctx, *count, *creators, func(i int) error {
				vm := newRunVM(l, *run, i, *class)
				t.deleteTracked(ctx, e, vm.Namespace+"/"+vm.Name)
				return nil
			})
			waitStage(ctx, t, stGone, *count, *timeout, start)
			s := t.summarize(*run, start)
			printSummary(s)
			return t.writeResults(*out, *run, s)
		}
		waitStage(ctx, t, stReady, *count, *timeout, start)
		s := t.summarize(*run, start)
		if *del {
			delStart := time.Now()
			_ = parallel(ctx, *count, *creators, func(i int) error {
				vm := newRunVM(l, *run, i, *class)
				t.deleteTracked(ctx, e, vm.Namespace+"/"+vm.Name)
				return nil
			})
			waitStage(ctx, t, stGone, *count, *timeout, delStart)
			s = t.summarize(*run, start)
		}
		printSummary(s)
		return t.writeResults(*out, *run, s)
	}
}

// waitStage polls until want VMs reached stage, the timeout passes or ctx ends.
func waitStage(ctx context.Context, t *tracker, stage string, want int, timeout time.Duration, start time.Time) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for t.count(stage) < want && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			fmt.Printf("  t=%4.0fs created=%d reconciled=%d pending=%d bound=%d ready=%d gone=%d\n",
				time.Since(start).Seconds(), t.count(stCreated), t.count(stReconciled), t.count(stPending),
				t.count(stBound), t.count(stReady), t.count(stGone))
		}
	}
}

// printSummary prints the headline numbers.
func printSummary(s summary) {
	data, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(data))
}

// churnCmd creates VMs at --rate per second for --duration, deletes each as
// soon as it is Ready (after --hold), then stops creating and follows the
// backlog for --recovery.
func churnCmd(fs *flag.FlagSet) func(context.Context, *env) error {
	run := fs.String("run", "", "run name; required")
	rate := fs.Float64("rate", 1, "creates per second requested")
	duration := fs.Duration("duration", 5*time.Minute, "load duration")
	recovery := fs.Duration("recovery", 10*time.Minute, "how long to follow the backlog after load stops")
	hold := fs.Duration("hold", 0, "how long a Ready VM lives before its delete")
	out := fs.String("out", ".", "results directory")
	return func(ctx context.Context, e *env) error {
		if *run == "" {
			return fmt.Errorf("--run is required")
		}
		l, err := loadLayout(ctx, e)
		if err != nil {
			return err
		}
		t, err := newTracker(ctx, e, *run)
		if err != nil {
			return err
		}
		start := time.Now()
		var submitted atomic.Int64
		series := [][]string{{"t_s", "submitted", "created", "ready", "delete_requested", "gone", "create_backlog", "delete_backlog"}}
		sample := func() {
			sub := int(submitted.Load())
			c, r, d, g := t.count(stCreated), t.count(stReady), t.count(stDeleteReq), t.count(stGone)
			series = append(series, []string{fmt.Sprintf("%.0f", time.Since(start).Seconds()), strconv.Itoa(sub), strconv.Itoa(c),
				strconv.Itoa(r), strconv.Itoa(d), strconv.Itoa(g), strconv.Itoa(c - r), strconv.Itoa(d - g)})
			fmt.Printf("  t=%4.0fs submitted=%d created=%d ready=%d deleteReq=%d gone=%d\n", time.Since(start).Seconds(), sub, c, r, d, g)
		}
		// Deleter: delete each Ready VM after hold.
		delCtx, stopDel := context.WithCancel(ctx)
		defer stopDel()
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-delCtx.Done():
					return
				case <-tick.C:
				}
				var due []string
				t.mu.Lock()
				for k, vt := range t.vms {
					if r, ok := vt.at[stReady]; ok && !vt.deleting && time.Since(r) >= *hold {
						vt.deleting = true
						due = append(due, k)
					}
				}
				t.mu.Unlock()
				for _, k := range due {
					go t.deleteTracked(delCtx, e, k)
				}
			}
		}()
		interval := time.Duration(float64(time.Second) / *rate)
		tick := time.NewTicker(interval)
		sampleTick := time.NewTicker(5 * time.Second)
		end := time.After(*duration)
		i := 0
	load:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-end:
				break load
			case <-sampleTick.C:
				sample()
			case <-tick.C:
				vm := newRunVM(l, *run, i, "small")
				i++
				submitted.Add(1)
				go t.createTracked(ctx, e, vm)
			}
		}
		tick.Stop()
		fmt.Printf("%s: load stopped after %d submits; following the backlog\n", *run, i)
		recEnd := time.Now().Add(*recovery)
		for time.Now().Before(recEnd) {
			if t.count(stGone) >= i {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-sampleTick.C:
				sample()
			}
		}
		sample()
		s := t.summarize(*run, start)
		printSummary(s)
		if err := t.writeResults(*out, *run, s); err != nil {
			return err
		}
		f, err := os.Create(filepath.Join(*out, *run+"-series.csv")) // #nosec G703 -- the path is the operator's own --out flag of this local benchmark CLI
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		w := csv.NewWriter(f)
		_ = w.WriteAll(series)
		return nil
	}
}
