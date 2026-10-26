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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/cliclient"
	"github.com/projectbeskar/virtrigaud/cmd/internal/envtestutil"
)

// newFakeGenerator returns a generator on a fake client that counts the
// Create calls it receives, with room for vmCount VMs.
func newFakeGenerator(t *testing.T, vmCount int, create func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error) (*LoadGenerator, *atomic.Int32) {
	t.Helper()
	scheme, err := cliclient.NewScheme()
	require.NoError(t, err)
	var creates atomic.Int32
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).Build(), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			creates.Add(1)
			if create != nil {
				if err := create(ctx, c, obj, opts...); err != nil {
					return err
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	lg := newGenerator(t, c, "default", &options{})
	lg.config.VMCount = vmCount
	lg.results = make(chan Result, 1000)
	return lg, &creates
}

// TestWaitTickNeverStartsAnOperationAfterTheRunEnded checks the select race
// behind an operation issued minutes after the end of the run: a worker busy
// observing past the deadline finds the ticker's tick ready together with the
// end of the context, and select used to pick either at random.
func TestWaitTickNeverStartsAnOperationAfterTheRunEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 1000; i++ {
		tick := make(chan time.Time, 1)
		tick <- time.Now()
		require.False(t, waitTick(ctx, tick), "a tick ready together with the end of the run started an operation (attempt %d)", i)
	}

	live := context.Background()
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	assert.True(t, waitTick(live, tick), "a tick while the run is on starts an operation")
}

// TestPerformRandomOperationStartsNothingAfterTheRunEnded checks that the
// dispatcher itself refuses to start an operation once the run's context is
// done.
func TestPerformRandomOperationStartsNothingAfterTheRunEnded(t *testing.T) {
	lg, creates := newFakeGenerator(t, 10, nil)
	lg.config.Operations = OperationMix{Create: 1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lg.performRandomOperation(ctx, 0)
	assert.Zero(t, creates.Load())
	assert.Empty(t, lg.results)
}

// TestRunIssuesNothingAfterTheDeadline reproduces the lab finding: a worker
// whose --observe outlasts the run's duration issued new operations as soon as
// the observation returned. Now it finishes the observation and stops, and the
// run takes the duration plus at most that observation.
func TestRunIssuesNothingAfterTheDeadline(t *testing.T) {
	lg, creates := newFakeGenerator(t, 100, nil)
	lg.config.Concurrency = 1
	lg.config.Operations = OperationMix{Create: 1}
	lg.tickInterval = 10 * time.Millisecond
	lg.opts.observe = true
	lg.opts.observeTimeout = 1500 * time.Millisecond // the fake client never sets the status: the observation times out
	lg.pollInterval = 20 * time.Millisecond

	const duration = 400 * time.Millisecond
	start := time.Now()
	deadline := start.Add(duration)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	require.NoError(t, lg.run(ctx))
	elapsed := time.Since(start)
	close(lg.results)

	assert.EqualValues(t, 1, creates.Load(), "only the operation started before the deadline runs; the buffered ticks start nothing")
	var n int
	for res := range lg.results {
		n++
		assert.False(t, res.StartTime.After(deadline), "operation %s started %v after the deadline", res.Operation, res.StartTime.Sub(deadline))
		assert.NotEmpty(t, res.ObserveError, "the in-flight observation ran to its end")
	}
	assert.Equal(t, 1, n)
	assert.GreaterOrEqual(t, elapsed, 1200*time.Millisecond, "the in-flight observation finished")
	assert.Less(t, elapsed, 5*time.Second)
}

func TestVMCapSkipsCreatesAndClonesBeyondIt(t *testing.T) {
	ctx := context.Background()

	t.Run("a create beyond vmCount is skipped, not failed, and makes no API call", func(t *testing.T) {
		lg, creates := newFakeGenerator(t, 2, nil)
		for i := 0; i < 2; i++ {
			lg.performCreate(ctx, "lab-provider", 0)
			assert.True(t, (<-lg.results).Success)
		}
		lg.performCreate(ctx, "lab-provider", 0)
		res := <-lg.results
		assert.True(t, res.Skipped)
		assert.False(t, res.Success)
		assert.Empty(t, res.Error)
		assert.Equal(t, phaseSkipped, res.Phase)
		assert.Equal(t, opCreate, res.Operation)
		assert.EqualValues(t, 2, creates.Load())
		assert.Len(t, lg.vms, 2)
		assert.Zero(t, lg.reserved)
	})

	t.Run("a clone's target counts as a VM", func(t *testing.T) {
		lg, creates := newFakeGenerator(t, 3, nil)
		lg.performCreate(ctx, "lab-provider", 0)
		<-lg.results
		lg.performClone(ctx, "lab-provider", 0)
		res := <-lg.results
		require.True(t, res.Success, res.Error)
		lg.performClone(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Success)
		lg.performClone(ctx, "lab-provider", 0)
		res = <-lg.results
		assert.True(t, res.Skipped, "1 VM + 2 clone targets = vmCount")
		assert.Equal(t, opClone, res.Operation)
		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Skipped, "clone targets leave no room for a create either")
		assert.EqualValues(t, 3, creates.Load(), "1 VirtualMachine + 2 VMClones")
		assert.Len(t, lg.cloneTargets, 2)
		assert.Zero(t, lg.reserved)
	})

	t.Run("a failed create gives its slot back", func(t *testing.T) {
		var calls atomic.Int32
		lg, _ := newFakeGenerator(t, 1, func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			if calls.Add(1) == 1 {
				return apierrors.NewInternalError(errors.New("apiserver unavailable"))
			}
			return nil
		})
		lg.performCreate(ctx, "lab-provider", 0)
		res := <-lg.results
		assert.False(t, res.Success)
		assert.False(t, res.Skipped)
		assert.NotEmpty(t, res.Error)
		assert.Zero(t, lg.reserved)
		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Success, "the slot of the failed create is free again")
	})

	t.Run("a clone with no source VM gives its slot back", func(t *testing.T) {
		lg, _ := newFakeGenerator(t, 1, nil)
		lg.performClone(ctx, "lab-provider", 0)
		res := <-lg.results
		assert.False(t, res.Success)
		assert.Equal(t, errNoRunVM.Error(), res.Error)
		assert.Zero(t, lg.reserved)
	})

	t.Run("workers racing for the last slots never exceed vmCount", func(t *testing.T) {
		const maxVMs, workers = 5, 40
		lg, creates := newFakeGenerator(t, maxVMs, nil)
		lg.results = make(chan Result, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lg.performCreate(ctx, "lab-provider", i)
			}()
		}
		wg.Wait()
		close(lg.results)
		var ok, skipped int
		for res := range lg.results {
			switch {
			case res.Skipped:
				skipped++
			case res.Success:
				ok++
			}
		}
		assert.Equal(t, maxVMs, ok)
		assert.Equal(t, workers-maxVMs, skipped)
		assert.EqualValues(t, maxVMs, creates.Load())
	})

	t.Run("dry run applies the cap too", func(t *testing.T) {
		lg, _ := newFakeGenerator(t, 1, nil)
		lg.opts.dryRun = true
		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Success)
		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Skipped)
	})
}

// TestVMCapCountsVMsBeingDeleted checks, against a real apiserver, that a VM
// whose delete was accepted keeps its slot until it is gone.
func TestVMCapCountsVMsBeingDeleted(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()
	ns := createNamespace(t, env.Client, "lg-cap-delete")
	lg := newGenerator(t, env.Client, ns, &options{})
	lg.config.VMCount = 1

	created := createVM(t, lg)
	lg.performCreate(ctx, "lab-provider", 0)
	assert.True(t, (<-lg.results).Skipped, "one VM, vmCount 1")

	// A finalizer holds the VM after its delete, like a provider delete in progress.
	vm := getVM(t, env.Client, ns, created.VMName)
	vm.Finalizers = []string{"loadgen.virtrigaud.io/test"}
	require.NoError(t, env.Client.Update(ctx, vm))

	lg.performDelete(ctx, "lab-provider", 0)
	res := <-lg.results
	require.True(t, res.Success, res.Error)
	require.Empty(t, lg.vms)
	require.Len(t, lg.deleting, 1)
	assert.NotNil(t, getVM(t, env.Client, ns, created.VMName).DeletionTimestamp)

	lg.performCreate(ctx, "lab-provider", 0)
	assert.True(t, (<-lg.results).Skipped, "the VM is still being deleted, so its slot is not free")

	vm = getVM(t, env.Client, ns, created.VMName)
	vm.Finalizers = nil
	require.NoError(t, env.Client.Update(ctx, vm))

	lg.lastForget = time.Time{} // the reads of the VMs being deleted are paced; ask for a new one
	lg.performCreate(ctx, "lab-provider", 0)
	res = <-lg.results
	assert.True(t, res.Success, "the VM is gone: %s", res.Error)
	assert.False(t, res.Skipped)
	assert.Empty(t, lg.deleting)
}

// TestRunWithVMCountAgainstAPIServer runs the command with a small vmCount and
// checks that the namespace never holds more VMs than that, that the skipped
// creates are recorded as such, and that --cleanup still removes everything.
func TestRunWithVMCountAgainstAPIServer(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()
	ns := createNamespace(t, env.Client, "lg-cap-run")
	cfg := strings.Replace(createOnlyConfig, "providers: [lab-provider]\n", "providers: [lab-provider]\nvmCount: 2\n", 1)
	cfg = strings.Replace(cfg, "duration: 3s", "duration: 4s", 1)
	outDir := t.TempDir()

	out, err := runLoadgen(t, "run", "--kubeconfig", env.KubeconfigPath, "-n", ns, "--cleanup",
		"--config", writeConfig(t, cfg), "--output-dir", outDir)
	require.NoError(t, err, out)
	assert.Contains(t, out, "Max VMs alive (vmCount): 2")
	assert.Contains(t, out, "Cleanup complete")

	csvData, err := os.ReadFile(filepath.Join(outDir, "results.csv"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(csvData)), "\n")
	require.Greater(t, len(lines), 3)
	assert.True(t, strings.HasSuffix(lines[0], ",Skipped"))
	var created, skipped int
	for _, line := range lines[1:] {
		switch {
		case strings.HasSuffix(line, ",true"):
			skipped++
			assert.Contains(t, line, "create,lab-provider,")
			assert.Contains(t, line, ",false,,"+phaseSkipped+",", "a skipped row is not a failure: no error")
		case strings.Contains(line, ",true,,"):
			created++
		}
	}
	assert.Equal(t, 2, created, "vmCount 2 allows two creates in a create-only run")
	assert.Positive(t, skipped, "the other creates were skipped")

	summary, err := os.ReadFile(filepath.Join(outDir, "summary.md"))
	require.NoError(t, err)
	assert.Contains(t, string(summary), "- **Total Operations**: 2\n")
	assert.Contains(t, string(summary), "- **Failed**: 0 (0.0%)")
	assert.Contains(t, string(summary), "- **Skipped** (vmCount = 2 VMs alive reached; not counted above): ")

	vms := &infrav1beta1.VirtualMachineList{}
	require.NoError(t, env.Client.List(ctx, vms, client.InNamespace(ns)))
	assert.Empty(t, vms.Items, "--cleanup removed the VMs")
}

// TestInterruptedRunExitsNonZero checks that an interrupted run still cleans
// up and saves its results, and then fails like an interrupted vcts run.
func TestInterruptedRunExitsNonZero(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()
	ns := createNamespace(t, env.Client, "lg-interrupt")
	cfg := strings.Replace(createOnlyConfig, "duration: 3s", "duration: 10m", 1)
	outDir := t.TempDir()

	cmd := newRootCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "--kubeconfig", env.KubeconfigPath, "-n", ns, "--cleanup", "--config", writeConfig(t, cfg), "--output-dir", outDir})
	runCtx, interrupt := context.WithCancel(ctx)
	timer := time.AfterFunc(2500*time.Millisecond, interrupt)
	defer timer.Stop()
	defer interrupt()

	start := time.Now()
	err := cmd.ExecuteContext(runCtx)
	require.Error(t, err)
	assert.ErrorIs(t, err, errInterrupted)
	assert.Less(t, time.Since(start), time.Minute, "the interrupt ended the run")
	out := stdout.String()
	assert.Contains(t, out, "interrupted")
	assert.Contains(t, out, "Cleanup complete")
	assert.FileExists(t, filepath.Join(outDir, "results.csv"))

	vms := &infrav1beta1.VirtualMachineList{}
	require.NoError(t, env.Client.List(ctx, vms, client.InNamespace(ns)))
	assert.Empty(t, vms.Items, "the cleanup ran after the interrupt")
}

// TestInterruptedDryRunExitsNonZero is the interrupt test that needs no
// cluster.
func TestInterruptedDryRunExitsNonZero(t *testing.T) {
	cfg := strings.Replace(createOnlyConfig, "duration: 3s", "duration: 10m", 1)
	cmd := newRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "--dry-run", "--config", writeConfig(t, cfg), "--output-dir", t.TempDir()})
	ctx, interrupt := context.WithCancel(context.Background())
	timer := time.AfterFunc(300*time.Millisecond, interrupt)
	defer timer.Stop()
	defer interrupt()
	assert.ErrorIs(t, cmd.ExecuteContext(ctx), errInterrupted)

	// A run that reaches the end of its duration still succeeds.
	_, err := runLoadgen(t, "run", "--dry-run", "--config", writeConfig(t, strings.Replace(createOnlyConfig, "duration: 3s", "duration: 1s", 1)),
		"--output-dir", t.TempDir())
	assert.NoError(t, err)
}

func TestVMCountConfig(t *testing.T) {
	t.Run("the default allows a few VMs", func(t *testing.T) {
		cfg, err := loadConfig("")
		require.NoError(t, err)
		assert.Equal(t, 10, cfg.VMCount)
	})
	t.Run("a file may set it", func(t *testing.T) {
		cfg, err := loadConfig(writeConfig(t, "vmCount: 3\n"))
		require.NoError(t, err)
		assert.Equal(t, 3, cfg.VMCount)
	})
	for _, content := range []string{"vmCount: 0\n", "vmCount: -2\n"} {
		_, err := loadConfig(writeConfig(t, content))
		require.Error(t, err, content)
		assert.Contains(t, err.Error(), "vmCount must be at least 1")
	}
}

func TestUnusedSettings(t *testing.T) {
	cfg := getDefaultConfig()
	assert.Empty(t, cfg.unusedSettings())
	loaded, err := loadConfig(writeConfig(t, "rampUpTime: 30s\nsteadyState: 1m\ncloneInterval: 5s\nvmTemplate: {classRef: c, imageRef: i, annotations: {a: b}}\n"))
	require.NoError(t, err)
	assert.Equal(t, []string{"rampUpTime", "steadyState", "cloneInterval", "vmTemplate.annotations"}, loaded.unusedSettings())

	out, err := runLoadgen(t, "run", "--dry-run", "--config",
		writeConfig(t, "duration: 1s\nrampUpTime: 30s\n"), "--output-dir", t.TempDir())
	require.NoError(t, err, out)
	assert.Contains(t, out, "Warning: accepted but not used by this release: rampUpTime")
}

// TestSettleClones checks, against a real apiserver, how a clone's target
// counts against vmCount and when the run may delete the clone's source.
func TestSettleClones(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()

	// cloneOnce creates a VM and clones it; it returns the generator, the
	// source VM and the VMClone.
	cloneOnce := func(t *testing.T, name string) (*LoadGenerator, string, *infrav1beta1.VMClone) {
		t.Helper()
		ns := createNamespace(t, env.Client, name)
		lg := newGenerator(t, env.Client, ns, &options{})
		lg.config.VMCount = 2
		source := createVM(t, lg)
		lg.performClone(ctx, "lab-provider", 0)
		res := <-lg.results
		require.True(t, res.Success, res.Error)
		clone := &infrav1beta1.VMClone{}
		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: lg.clones[0].name}, clone))
		return lg, source.VMName, clone
	}

	t.Run("the source of a clone in flight is not deleted; a Ready clone's target becomes a VM of the run", func(t *testing.T) {
		lg, source, clone := cloneOnce(t, "lg-settle-ready")
		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Skipped, "VM + clone target = vmCount")

		lg.performDelete(ctx, "lab-provider", 0)
		res := <-lg.results
		assert.False(t, res.Success)
		assert.Equal(t, errNoRunVM.Error(), res.Error, "the only VM is the source of a clone that has not ended")
		getVM(t, env.Client, lg.namespace, source)

		clone.Status.Phase = infrav1beta1.ClonePhaseReady
		require.NoError(t, env.Client.Status().Update(ctx, clone))
		require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: clone.Spec.Target.Name, Namespace: lg.namespace, Labels: clone.Spec.Target.Labels},
			Spec: infrav1beta1.VirtualMachineSpec{
				ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"}, ClassRef: infrav1beta1.ObjectRef{Name: "small"},
				PowerState: infrav1beta1.PowerStateOff,
			},
		}))

		lg.settleClones(ctx)
		require.Len(t, lg.vms, 2, "the clone target is one of the run's VMs now")
		assert.Equal(t, clone.Spec.Target.Name, lg.vms[1].name)
		assert.Equal(t, infrav1beta1.PowerStateOff, lg.vms[1].power)
		lg.mu.Lock()
		alive := lg.aliveLocked()
		lg.mu.Unlock()
		assert.Equal(t, 2, alive, "settling a clone does not change how many VMs are alive")

		// Deleting now frees a slot, whichever VM is chosen.
		lg.performDelete(ctx, "lab-provider", 0)
		res = <-lg.results
		require.True(t, res.Success, res.Error)
		lg.lastForget = time.Time{} // the reads of the VMs being deleted are paced; ask for a new one
		lg.performCreate(ctx, "lab-provider", 0)
		res = <-lg.results
		assert.True(t, res.Success, "the delete freed a slot: %s", res.Error)
	})

	t.Run("a Failed clone's slot is held while its target VM exists, and freed once it is gone", func(t *testing.T) {
		lg, _, clone := cloneOnce(t, "lg-settle-failed")
		lg.performCreate(ctx, "lab-provider", 0)
		require.True(t, (<-lg.results).Skipped)

		// The operator removes the target of a failed clone only later.
		target := createTargetVM(t, env.Client, lg.namespace, clone, infrav1beta1.PowerStateOff)
		clone.Status.Phase = infrav1beta1.ClonePhaseFailed
		require.NoError(t, env.Client.Status().Update(ctx, clone))
		lg.settleClones(ctx)
		assert.Len(t, lg.vms, 1, "the target of a failed clone is not a VM the run acts on")
		require.Len(t, lg.deleting, 1)
		assert.Equal(t, target.Name, lg.deleting[0].name)
		assert.Equal(t, target.UID, lg.deleting[0].uid)

		lg.lastForget = time.Time{}
		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Skipped, "the failed clone's target still occupies a slot")

		require.NoError(t, env.Client.Delete(ctx, target))
		lg.lastForget = time.Time{}
		lg.performCreate(ctx, "lab-provider", 0)
		res := <-lg.results
		assert.True(t, res.Success, "the target is gone: %s", res.Error)
	})

	t.Run("a Failed clone with no target VM frees its slot at once", func(t *testing.T) {
		lg, _, clone := cloneOnce(t, "lg-settle-failed-gone")
		clone.Status.Phase = infrav1beta1.ClonePhaseFailed
		require.NoError(t, env.Client.Status().Update(ctx, clone))
		lg.settleClones(ctx)
		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Success)
	})

	t.Run("a Ready clone whose target is being deleted keeps its slot until it is gone", func(t *testing.T) {
		lg, _, clone := cloneOnce(t, "lg-settle-deleting")
		target := createTargetVM(t, env.Client, lg.namespace, clone, infrav1beta1.PowerStateOff)
		target.Finalizers = []string{"loadgen.virtrigaud.io/test"}
		require.NoError(t, env.Client.Update(ctx, target))
		require.NoError(t, env.Client.Delete(ctx, target))
		t.Cleanup(func() {
			held := &infrav1beta1.VirtualMachine{}
			if env.Client.Get(ctx, client.ObjectKeyFromObject(target), held) == nil {
				held.Finalizers = nil
				_ = env.Client.Update(ctx, held)
			}
		})
		clone.Status.Phase = infrav1beta1.ClonePhaseReady
		require.NoError(t, env.Client.Status().Update(ctx, clone))
		lg.settleClones(ctx)
		assert.Len(t, lg.vms, 1, "a VM being deleted is not taken over")
		assert.Len(t, lg.deleting, 1)

		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Skipped)
	})

	t.Run("a Ready clone whose target lacks the run label frees its slot", func(t *testing.T) {
		lg, _, clone := cloneOnce(t, "lg-settle-unlabelled")
		require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: clone.Spec.Target.Name, Namespace: lg.namespace},
			Spec: infrav1beta1.VirtualMachineSpec{
				ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"}, ClassRef: infrav1beta1.ObjectRef{Name: "small"},
			},
		}))
		clone.Status.Phase = infrav1beta1.ClonePhaseReady
		require.NoError(t, env.Client.Status().Update(ctx, clone))
		lg.settleClones(ctx)
		assert.Len(t, lg.vms, 1)
		assert.Empty(t, lg.deleting)
		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Success, "a VM the run did not label is not the run's")
	})

	failedWith := func(t *testing.T, clone *infrav1beta1.VMClone, mutate func(*infrav1beta1.VMClone)) {
		t.Helper()
		clone.Status.Phase = infrav1beta1.ClonePhaseFailed
		mutate(clone)
		require.NoError(t, env.Client.Status().Update(ctx, clone))
	}
	keptCases := []struct {
		name   string
		mutate func(*infrav1beta1.VMClone)
	}{
		{"the provider reported a cloned VM", func(c *infrav1beta1.VMClone) { c.Status.TargetVMID = "100" }},
		{"a Clone of unknown outcome is on record", func(c *infrav1beta1.VMClone) {
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type: conditionCloneRequested, Status: metav1.ConditionTrue, Reason: "Requested", LastTransitionTime: metav1.Now()})
		}},
		{"the operator recorded TargetKept", func(c *infrav1beta1.VMClone) {
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type: conditionTargetCleanup, Status: metav1.ConditionFalse, Reason: reasonTargetKept, LastTransitionTime: metav1.Now()})
		}},
	}
	for i, kc := range keptCases {
		t.Run("the target the operator keeps is counted, never deleted, and reported: "+kc.name, func(t *testing.T) {
			lg, _, clone := cloneOnce(t, "lg-kept-"+string(rune('a'+i)))
			target := createTargetVM(t, env.Client, lg.namespace, clone, infrav1beta1.PowerStateOff)
			failedWith(t, clone, kc.mutate)

			lg.settleClones(ctx)
			require.Len(t, lg.kept, 1)
			assert.Equal(t, target.Name, lg.kept[0].name)
			assert.Empty(t, lg.deleting, "the run does not delete it")
			lg.performCreate(ctx, "lab-provider", 0)
			assert.True(t, (<-lg.results).Skipped, "it still occupies a slot")

			// Without --cleanup it is listed, with a warning about the kubectl command.
			var out strings.Builder
			require.NoError(t, lg.finish(&out))
			assert.Contains(t, out.String(), keptMessage(target.Name))

			lg.opts.cleanup, lg.opts.cleanupTimeout = true, 20*time.Second
			out.Reset()
			err := lg.finish(&out)
			require.ErrorIs(t, err, errCleanupIncomplete)
			assert.Contains(t, out.String(), keptMessage(target.Name))
			getVM(t, env.Client, lg.namespace, target.Name) // still there
			vms := listVMs(t, env.Client, lg.namespace)
			assert.Len(t, vms, 1, "the source VM went, the kept target stayed")
			assert.Equal(t, target.Name, vms[0].Name)
			clones := &infrav1beta1.VMCloneList{}
			require.NoError(t, env.Client.List(ctx, clones, client.InNamespace(lg.namespace)))
			assert.Empty(t, clones.Items, "the VMClone itself is removed")
		})
	}

	t.Run("cleanup keeps the target of a failed clone it never settled", func(t *testing.T) {
		lg, _, clone := cloneOnce(t, "lg-kept-unsettled")
		target := createTargetVM(t, env.Client, lg.namespace, clone, infrav1beta1.PowerStateOff)
		failedWith(t, clone, func(c *infrav1beta1.VMClone) { c.Status.TargetVMID = "100" })
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		left := lg.cleanup(cctx)
		assert.Equal(t, []string{keptMessage(target.Name)}, left)
		getVM(t, env.Client, lg.namespace, target.Name)
	})

	t.Run("a kept target stops counting when it is gone", func(t *testing.T) {
		lg, _, clone := cloneOnce(t, "lg-kept-gone")
		target := createTargetVM(t, env.Client, lg.namespace, clone, infrav1beta1.PowerStateOff)
		failedWith(t, clone, func(c *infrav1beta1.VMClone) { c.Status.TargetVMID = "100" })
		lg.settleClones(ctx)
		require.Len(t, lg.kept, 1)
		require.NoError(t, env.Client.Delete(ctx, target))
		lg.lastForget = time.Time{}
		lg.performCreate(ctx, "lab-provider", 0)
		assert.True(t, (<-lg.results).Success)
		assert.Empty(t, lg.kept)
	})

	t.Run("cleanup deletes a settled clone target once", func(t *testing.T) {
		lg, _, clone := cloneOnce(t, "lg-settle-cleanup")
		clone.Status.Phase = infrav1beta1.ClonePhaseReady
		require.NoError(t, env.Client.Status().Update(ctx, clone))
		createTargetVM(t, env.Client, lg.namespace, clone, infrav1beta1.PowerStateOff)
		hc := &hookClient{Client: env.Client}
		lg.client = hc
		lg.settleClones(ctx)
		require.Len(t, lg.vms, 2)

		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		assert.Empty(t, lg.cleanup(cctx))
		vms := &infrav1beta1.VirtualMachineList{}
		require.NoError(t, env.Client.List(ctx, vms, client.InNamespace(lg.namespace)))
		assert.Empty(t, vms.Items)
		assert.Equal(t, 1, hc.deletes(clone.Spec.Target.Name), "the target is deleted once, not as a VM and again as a clone target")
	})
}

// createTargetVM creates the target VirtualMachine of clone, with the labels
// the operator gives it.
func createTargetVM(t *testing.T, c client.Client, ns string, clone *infrav1beta1.VMClone, power infrav1beta1.PowerState) *infrav1beta1.VirtualMachine {
	t.Helper()
	vm := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: clone.Spec.Target.Name, Namespace: ns, Labels: clone.Spec.Target.Labels},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"}, ClassRef: infrav1beta1.ObjectRef{Name: "small"},
			PowerState: power,
		},
	}
	require.NoError(t, c.Create(context.Background(), vm))
	return vm
}

// hookClient wraps a client to count Delete calls per object name, to delay
// every Get (a slow apiserver) and to make a VirtualMachine Create report an
// error after the object was stored (a lost answer).
type hookClient struct {
	client.Client
	getDelay      time.Duration
	failVMCreate  bool
	mu            sync.Mutex
	deletedByName map[string]int
	slowGets      atomic.Int32
	// onGet, when set, runs on every Get.
	onGet func()
	// afterVMList, when set, runs once after the first VirtualMachine list.
	afterVMList func()
}

func (h *hookClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := h.Client.List(ctx, list, opts...)
	if _, isVMs := list.(*infrav1beta1.VirtualMachineList); isVMs && h.afterVMList != nil {
		hook := h.afterVMList
		h.afterVMList = nil
		hook()
	}
	return err
}

func (h *hookClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if h.onGet != nil {
		h.onGet()
	}
	if h.getDelay > 0 {
		h.slowGets.Add(1)
		select {
		case <-time.After(h.getDelay):
		case <-ctx.Done():
		}
	}
	return h.Client.Get(ctx, key, obj, opts...)
}

func (h *hookClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	h.mu.Lock()
	if h.deletedByName == nil {
		h.deletedByName = map[string]int{}
	}
	h.deletedByName[obj.GetName()]++
	h.mu.Unlock()
	return h.Client.Delete(ctx, obj, opts...)
}

func (h *hookClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	err := h.Client.Create(ctx, obj, opts...)
	if _, isVM := obj.(*infrav1beta1.VirtualMachine); isVM && err == nil && h.failVMCreate {
		return apierrors.NewTimeoutError("answer lost", 1)
	}
	return err
}

// deletes returns how many Delete calls named name.
func (h *hookClient) deletes(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deletedByName[name]
}

// TestDeleteLatencyExcludesCloneSettling checks that the reads that look for
// ended clones are not part of a delete's recorded Duration.
func TestDeleteLatencyExcludesCloneSettling(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()
	ns := createNamespace(t, env.Client, "lg-delete-latency")
	hc := &hookClient{Client: env.Client}
	lg := newGenerator(t, hc, ns, &options{})
	createVM(t, lg)
	hc.getDelay = 300 * time.Millisecond
	// Three pending clones that are gone: each costs two slow reads to settle.
	for _, n := range []string{"a", "b", "c"} {
		lg.clones = append(lg.clones, runObject{name: "ghost-" + n, uid: "x", source: "elsewhere", target: "ghost-vm-" + n})
	}

	start := time.Now()
	lg.performDelete(ctx, "lab-provider", 0)
	total := time.Since(start)
	res := <-lg.results
	require.True(t, res.Success, res.Error)
	assert.GreaterOrEqual(t, total, 1500*time.Millisecond, "the clones were read")
	assert.Less(t, res.Duration, time.Second, "the delete's Duration is the delete call's, not the reads'")
	assert.Positive(t, hc.slowGets.Load())

	// A clone that stays pending (an existing VMClone with no phase and no
	// CloneRequested condition is settled, so make it in flight).
	createVM(t, lg)
	hc.getDelay = 0
	pending := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-clone", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "elsewhere"}},
			Target: infrav1beta1.VMCloneTarget{Name: "pending-target"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, pending))
	pending.Status.Phase = infrav1beta1.ClonePhaseCloning
	require.NoError(t, env.Client.Status().Update(ctx, pending))
	lg.clones = []runObject{{name: pending.Name, uid: pending.UID, source: "elsewhere", target: "pending-target"}}

	// Within the poll interval the clones are not read again; after it they are.
	var gets atomic.Int32
	lg.client = &hookClient{Client: env.Client, onGet: func() { gets.Add(1) }}
	lg.pollInterval = time.Hour
	lg.markSettled()
	lg.performDelete(ctx, "lab-provider", 0)
	<-lg.results
	assert.Zero(t, gets.Load(), "paced: no read")
	lg.lastSettle = time.Time{}
	createVM(t, lg)
	lg.performDelete(ctx, "lab-provider", 0)
	<-lg.results
	assert.Positive(t, gets.Load(), "due: the pending clone is read")
}

// TestCleanupDeletesObjectsItNeverRecorded checks that the cleanup also
// removes objects of the run whose create reported an error after the
// apiserver stored them, and leaves other runs' objects alone.
func TestCleanupDeletesObjectsItNeverRecorded(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()
	ns := createNamespace(t, env.Client, "lg-orphans")
	hc := &hookClient{Client: env.Client, failVMCreate: true}
	lg := newGenerator(t, hc, ns, &options{cleanup: true, cleanupTimeout: 20 * time.Second})

	lg.performCreate(ctx, "lab-provider", 0)
	res := <-lg.results
	require.False(t, res.Success, "the create reported a lost answer")
	require.Len(t, listVMs(t, env.Client, ns), 1, "the apiserver stored it")

	labels := map[string]string{loadgenRunLabelKey: lg.runID}
	require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-snap", Namespace: ns, Labels: labels},
		Spec:       infrav1beta1.VMSnapshotSpec{VMRef: infrav1beta1.LocalObjectReference{Name: "gone"}},
	}))
	require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-clone", Namespace: ns, Labels: labels},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "gone"}},
			Target: infrav1beta1.VMCloneTarget{Name: "late-target", Labels: labels},
		},
	}))
	// The clone's target appears after the cleanup listed the VMs, like the
	// target of a clone that was still running.
	hc.afterVMList = func() {
		require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "late-target", Namespace: ns, Labels: labels},
			Spec: infrav1beta1.VirtualMachineSpec{
				ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"}, ClassRef: infrav1beta1.ObjectRef{Name: "small"},
			},
		}))
	}
	other := map[string]string{loadgenRunLabelKey: "another-run", loadgenLabelKey: loadgenLabelValue}
	require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "other-run-vm", Namespace: ns, Labels: other},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"}, ClassRef: infrav1beta1.ObjectRef{Name: "small"},
		},
	}))

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	assert.Empty(t, lg.cleanup(cctx))

	vms := listVMs(t, env.Client, ns)
	require.Len(t, vms, 1)
	assert.Equal(t, "other-run-vm", vms[0].Name)
	snaps := &infrav1beta1.VMSnapshotList{}
	require.NoError(t, env.Client.List(ctx, snaps, client.InNamespace(ns)))
	assert.Empty(t, snaps.Items)
	clones := &infrav1beta1.VMCloneList{}
	require.NoError(t, env.Client.List(ctx, clones, client.InNamespace(ns)))
	assert.Empty(t, clones.Items)
}

// TestForgetDeletedIsPaced checks that workers meeting the cap read the VMs
// being deleted at most once per poll interval.
func TestForgetDeletedIsPaced(t *testing.T) {
	scheme, err := cliclient.NewScheme()
	require.NoError(t, err)
	var gets atomic.Int32
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
	})
	ctx := context.Background()
	pendingVM := &infrav1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "pending-delete", Namespace: "default"}}
	require.NoError(t, c.Create(ctx, pendingVM))
	lg := newGenerator(t, c, "default", &options{})
	lg.config.VMCount = 1
	lg.pollInterval = time.Hour
	lg.deleting = []runObject{{name: pendingVM.Name, uid: pendingVM.UID}} // still there: it keeps counting

	for i := 0; i < 5; i++ {
		assert.False(t, lg.reserveSlot(ctx), "the VM being deleted holds the only slot")
	}
	assert.EqualValues(t, 1, gets.Load(), "one read of the VM being deleted, however many workers met the cap")

	lg.lastForget = time.Time{}
	assert.False(t, lg.reserveSlot(ctx))
	assert.EqualValues(t, 2, gets.Load(), "a new round reads it again")
}
