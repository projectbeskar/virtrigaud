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

	t.Run("a Failed clone frees its target's slot", func(t *testing.T) {
		lg, _, clone := cloneOnce(t, "lg-settle-failed")
		lg.performCreate(ctx, "lab-provider", 0)
		require.True(t, (<-lg.results).Skipped)

		clone.Status.Phase = infrav1beta1.ClonePhaseFailed
		require.NoError(t, env.Client.Status().Update(ctx, clone))
		lg.settleClones(ctx)
		assert.Len(t, lg.vms, 1)
		lg.performCreate(ctx, "lab-provider", 0)
		res := <-lg.results
		assert.True(t, res.Success, "the failed clone made no VM: %s", res.Error)
	})

	t.Run("cleanup deletes a settled clone target once", func(t *testing.T) {
		lg, _, clone := cloneOnce(t, "lg-settle-cleanup")
		clone.Status.Phase = infrav1beta1.ClonePhaseReady
		require.NoError(t, env.Client.Status().Update(ctx, clone))
		require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: clone.Spec.Target.Name, Namespace: lg.namespace, Labels: clone.Spec.Target.Labels},
			Spec: infrav1beta1.VirtualMachineSpec{
				ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"}, ClassRef: infrav1beta1.ObjectRef{Name: "small"},
			},
		}))
		lg.settleClones(ctx)
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		assert.Empty(t, lg.cleanup(cctx))
		vms := &infrav1beta1.VirtualMachineList{}
		require.NoError(t, env.Client.List(ctx, vms, client.InNamespace(lg.namespace)))
		assert.Empty(t, vms.Items)
	})
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
	lg := newGenerator(t, c, "default", &options{})
	lg.config.VMCount = 1
	lg.pollInterval = time.Hour
	lg.deleting = []runObject{{name: "pending-delete"}}
	for i := 0; i < 5; i++ {
		lg.performCreate(context.Background(), "lab-provider", 0)
		res := <-lg.results
		assert.Equal(t, i > 0, res.Skipped, "the first create finds the deleted VM gone; the rest meet the cap")
	}
	assert.LessOrEqual(t, gets.Load(), int32(1), "one read of the VM being deleted, however many creates met the cap")
}
