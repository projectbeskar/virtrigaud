/*
Copyright 2025.

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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

// allOpsConfig weighs every operation.
const allOpsConfig = `duration: 4s
concurrency: 3
providers: [lab-provider]
vmTemplate:
  classRef: small
  imageRef: ubuntu
  resources:
    cpuRange: [1, 4]
    memoryRange: [512, 2048]
operations:
  create: 30
  delete: 5
  power: 15
  reconfigure: 15
  snapshot: 15
  clone: 15
  describe: 5
`

// setTickInterval sets how often the workers of the runs the test starts
// through the command begin an operation, for the rest of the test.
func setTickInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := defaultTickInterval
	defaultTickInterval = d
	t.Cleanup(func() { defaultTickInterval = prev })
}

// runSeq numbers the run ids of the generators the tests build.
var runSeq atomic.Int32

// newGenerator returns a LoadGenerator on c in namespace ns.
func newGenerator(t *testing.T, c client.Client, ns string, opts *options) *LoadGenerator {
	t.Helper()
	if opts.outputDir == "" {
		opts.outputDir = t.TempDir()
	}
	cfg := getDefaultConfig()
	cfg.Providers = []string{"lab-provider"}
	cfg.VMTemplate.Resources = VMResources{CPURange: []int{1, 4}, MemoryRange: []int{512, 2048}}
	return &LoadGenerator{
		client:       c,
		namespace:    ns,
		config:       cfg,
		results:      make(chan Result, 100),
		opts:         opts,
		runID:        fmt.Sprintf("t%d", runSeq.Add(1)),
		observeCtx:   context.Background(),
		pollInterval: 50 * time.Millisecond,
	}
}

// createVM runs one create and returns its result.
func createVM(t *testing.T, lg *LoadGenerator) Result {
	t.Helper()
	lg.performCreate(context.Background(), "lab-provider", 1)
	res := <-lg.results
	require.True(t, res.Success, res.Error)
	return res
}

// getVM reads a VirtualMachine.
func getVM(t *testing.T, c client.Client, ns, name string) *infrav1beta1.VirtualMachine {
	t.Helper()
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, vm))
	return vm
}

// statusFaker writes, for every object in a namespace, the status a provider
// would produce once it acted on the spec (envtest runs no controller).
func statusFaker(ctx context.Context, c client.Client, ns string) {
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		vms := &infrav1beta1.VirtualMachineList{}
		if c.List(ctx, vms, client.InNamespace(ns)) == nil {
			for i := range vms.Items {
				vm := &vms.Items[i]
				vm.Status.ID = "fake-" + vm.Name
				vm.Status.PowerState = infrav1beta1.ObservedPowerStateOn
				if vm.Spec.PowerState == infrav1beta1.PowerStateOff {
					vm.Status.PowerState = infrav1beta1.ObservedPowerStateOff
				}
				if r := vm.Spec.Resources; r != nil {
					vm.Status.CurrentResources = r.DeepCopy()
				}
				_ = c.Status().Update(ctx, vm)
			}
		}
		snaps := &infrav1beta1.VMSnapshotList{}
		if c.List(ctx, snaps, client.InNamespace(ns)) == nil {
			for i := range snaps.Items {
				snaps.Items[i].Status.Phase = infrav1beta1.SnapshotPhaseReady
				_ = c.Status().Update(ctx, &snaps.Items[i])
			}
		}
		clones := &infrav1beta1.VMCloneList{}
		if c.List(ctx, clones, client.InNamespace(ns)) == nil {
			for i := range clones.Items {
				clones.Items[i].Status.Phase = infrav1beta1.ClonePhaseReady
				meta.SetStatusCondition(&clones.Items[i].Status.Conditions, metav1.Condition{
					Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", Message: "ok", LastTransitionTime: metav1.Now(),
				})
				_ = c.Status().Update(ctx, &clones.Items[i])
			}
		}
	}
}

// startFaker runs statusFaker until the test ends.
func startFaker(t *testing.T, c client.Client, ns string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		statusFaker(ctx, c, ns)
	}()
	t.Cleanup(wg.Wait)
	t.Cleanup(cancel)
}

// createNamespace creates a namespace for one test.
func createNamespace(t *testing.T, c client.Client, name string) string {
	t.Helper()
	require.NoError(t, c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	return name
}

func TestOperationsAgainstAPIServer(t *testing.T) {
	env := envtestutil.Start(t)
	ctx := context.Background()

	t.Run("power toggles spec.powerState of a VM the run created", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-power")
		lg := newGenerator(t, env.Client, ns, &options{})
		created := createVM(t, lg)

		lg.performPower(ctx, "other-provider", 0)
		res := <-lg.results
		require.True(t, res.Success, res.Error)
		assert.Equal(t, "power", res.Operation)
		assert.Equal(t, created.VMName, res.VMName)
		assert.Equal(t, "lab-provider", res.Provider, "attributed to the VM's provider")
		assert.Equal(t, "Off", res.Phase)
		assert.Positive(t, res.Duration)
		assert.Equal(t, infrav1beta1.PowerStateOff, getVM(t, env.Client, ns, created.VMName).Spec.PowerState)

		lg.performPower(ctx, "lab-provider", 0)
		res = <-lg.results
		require.True(t, res.Success, res.Error)
		assert.Equal(t, "On", res.Phase)
		assert.Equal(t, infrav1beta1.PowerStateOn, getVM(t, env.Client, ns, created.VMName).Spec.PowerState)
	})

	t.Run("an operation with no VM of the run fails without touching anything", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-none")
		lg := newGenerator(t, env.Client, ns, &options{})
		for _, op := range []func(context.Context, string, int){lg.performPower, lg.performReconfigure, lg.performSnapshot, lg.performClone, lg.performDelete} {
			op(ctx, "lab-provider", 0)
			res := <-lg.results
			assert.False(t, res.Success)
			assert.Equal(t, errNoRunVM.Error(), res.Error)
		}
	})

	t.Run("reconfigure sets spec.resources within the configured ranges", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-reconf")
		lg := newGenerator(t, env.Client, ns, &options{})
		created := createVM(t, lg)
		var lastCPU int32
		for i := 0; i < 3; i++ {
			lg.performReconfigure(ctx, "lab-provider", 0)
			res := <-lg.results
			require.True(t, res.Success, res.Error)
			vm := getVM(t, env.Client, ns, created.VMName)
			require.NotNil(t, vm.Spec.Resources)
			require.NotNil(t, vm.Spec.Resources.CPU)
			require.NotNil(t, vm.Spec.Resources.MemoryMiB)
			cpu, mem := *vm.Spec.Resources.CPU, *vm.Spec.Resources.MemoryMiB
			assert.GreaterOrEqual(t, cpu, int32(1))
			assert.LessOrEqual(t, cpu, int32(4))
			assert.GreaterOrEqual(t, mem, int64(512))
			assert.LessOrEqual(t, mem, int64(2048))
			assert.NotEqual(t, lastCPU, cpu, "each reconfigure changes the size")
			assert.Contains(t, res.Phase, "cpu=")
			lastCPU = cpu
		}
	})

	t.Run("snapshot creates a VMSnapshot of a run VM that is deleted with it", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-snap")
		lg := newGenerator(t, env.Client, ns, &options{})
		created := createVM(t, lg)
		lg.performSnapshot(ctx, "lab-provider", 0)
		res := <-lg.results
		require.True(t, res.Success, res.Error)
		assert.Equal(t, created.VMName, res.VMName)

		snaps := &infrav1beta1.VMSnapshotList{}
		require.NoError(t, env.Client.List(ctx, snaps, client.InNamespace(ns)))
		require.Len(t, snaps.Items, 1)
		s := snaps.Items[0]
		assert.Equal(t, res.Phase, s.Name)
		assert.Equal(t, created.VMName, s.Spec.VMRef.Name)
		require.NotNil(t, s.Spec.RetentionPolicy)
		assert.True(t, s.Spec.RetentionPolicy.DeleteOnVMDelete)
		assert.Equal(t, lg.runID, s.Labels[loadgenRunLabelKey])
		assert.Equal(t, loadgenLabelValue, s.Labels[loadgenLabelKey])
	})

	t.Run("clone creates a VMClone of a run VM whose target carries the run labels", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-clone")
		lg := newGenerator(t, env.Client, ns, &options{})
		created := createVM(t, lg)
		lg.performClone(ctx, "lab-provider", 0)
		res := <-lg.results
		require.True(t, res.Success, res.Error)

		clones := &infrav1beta1.VMCloneList{}
		require.NoError(t, env.Client.List(ctx, clones, client.InNamespace(ns)))
		require.Len(t, clones.Items, 1)
		c := clones.Items[0]
		require.NotNil(t, c.Spec.Source.VMRef)
		assert.Equal(t, created.VMName, c.Spec.Source.VMRef.Name)
		assert.Equal(t, res.Phase, c.Spec.Target.Name)
		assert.True(t, strings.HasPrefix(c.Spec.Target.Name, "loadgen-vm-"+lg.runID+"-clone-"))
		assert.Equal(t, lg.runID, c.Spec.Target.Labels[loadgenRunLabelKey])
		assert.Equal(t, lg.runID, c.Labels[loadgenRunLabelKey])
		assert.Equal(t, []string{c.Spec.Target.Name}, lg.cloneTargets)
	})

	t.Run("--observe records the time until the status reflects each request", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-observe")
		startFaker(t, env.Client, ns)
		lg := newGenerator(t, env.Client, ns, &options{observe: true, observeTimeout: 20 * time.Second})

		created := createVM(t, lg)
		assert.Positive(t, created.Observed, created.ObserveError)
		assert.GreaterOrEqual(t, created.Observed, created.Duration)
		for _, op := range []func(context.Context, string, int){lg.performPower, lg.performReconfigure, lg.performSnapshot, lg.performClone, lg.performDelete} {
			op(ctx, "lab-provider", 0)
			res := <-lg.results
			require.True(t, res.Success, res.Error)
			assert.Positive(t, res.Observed, "%s: %s", res.Operation, res.ObserveError)
			assert.Empty(t, res.ObserveError, res.Operation)
		}
	})

	t.Run("--observe reports a status that never reflects the request", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-observe-timeout")
		lg := newGenerator(t, env.Client, ns, &options{observe: true, observeTimeout: 300 * time.Millisecond})
		res := func() Result {
			lg.performCreate(ctx, "lab-provider", 1)
			return <-lg.results
		}()
		require.True(t, res.Success, res.Error)
		assert.Zero(t, res.Observed)
		assert.Contains(t, res.ObserveError, "did not reflect the request within 300ms")
	})

	t.Run("a full run never touches VMs it did not create, and --cleanup removes what it created", func(t *testing.T) {
		// At the default one operation per second per worker, the run drew
		// about ten operations, and none was a create about 3% of the time
		// (0.7^10), which left nothing to check. At 100ms it draws about
		// a hundred.
		setTickInterval(t, 100*time.Millisecond)
		ns := createNamespace(t, env.Client, "lg-mix")
		bystander := &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "bystander", Namespace: ns, Labels: map[string]string{loadgenLabelKey: loadgenLabelValue}},
			Spec: infrav1beta1.VirtualMachineSpec{
				ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"},
				ClassRef:    infrav1beta1.ObjectRef{Name: "small"},
				PowerState:  infrav1beta1.PowerStateOn,
			},
		}
		require.NoError(t, env.Client.Create(ctx, bystander))
		before := getVM(t, env.Client, ns, "bystander")

		outDir := t.TempDir()
		out, err := runLoadgen(t, "run", "--kubeconfig", env.KubeconfigPath, "-n", ns, "--cleanup", "--cleanup-timeout", "30s",
			"--config", writeConfig(t, allOpsConfig), "--output-dir", outDir)
		require.NoError(t, err, out)
		assert.Contains(t, out, "Cleanup complete")

		after := getVM(t, env.Client, ns, "bystander")
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "the bystander VM was changed")

		csvData, err := os.ReadFile(filepath.Join(outDir, "results.csv"))
		require.NoError(t, err)
		csv := string(csvData)
		assert.Contains(t, csv, ",ObservedDuration,ObserveError")
		assert.Contains(t, csv, "\ncreate,lab-provider,loadgen-vm-")
		// Every operation that acted on a VM acted on one of the run's.
		for _, line := range strings.Split(strings.TrimSpace(csv), "\n")[1:] {
			vmName := strings.Split(line, ",")[2]
			assert.True(t, vmName == "" || strings.HasPrefix(vmName, "loadgen-vm-"), "row acts on %q: %s", vmName, line)
		}

		snaps := &infrav1beta1.VMSnapshotList{}
		require.NoError(t, env.Client.List(ctx, snaps, client.InNamespace(ns)))
		assert.Empty(t, snaps.Items, "cleanup left VMSnapshots")
		clones := &infrav1beta1.VMCloneList{}
		require.NoError(t, env.Client.List(ctx, clones, client.InNamespace(ns)))
		assert.Empty(t, clones.Items, "cleanup left VMClones")
		vms := &infrav1beta1.VirtualMachineList{}
		require.NoError(t, env.Client.List(ctx, vms, client.InNamespace(ns)))
		require.Len(t, vms.Items, 1, "cleanup left VMs, or deleted the bystander")
		assert.Equal(t, "bystander", vms.Items[0].Name)
	})

	t.Run("without --cleanup the run's objects stay and the command says how to remove them", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-keep")
		out, err := runLoadgen(t, "run", "--kubeconfig", env.KubeconfigPath, "-n", ns,
			"--config", writeConfig(t, createOnlyConfig), "--output-dir", t.TempDir())
		require.NoError(t, err, out)
		assert.Contains(t, out, "Left in place:")
		assert.Contains(t, out, "kubectl delete vmsnapshots,vmclones,virtualmachines -n "+ns+" -l "+loadgenRunLabelKey+"=")
		vms := &infrav1beta1.VirtualMachineList{}
		require.NoError(t, env.Client.List(ctx, vms, client.InNamespace(ns)))
		assert.NotEmpty(t, vms.Items)
	})

	t.Run("cleanup deletes a clone target only when it carries the run label", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-targets")
		lg := newGenerator(t, env.Client, ns, &options{cleanup: true, cleanupTimeout: 20 * time.Second})
		lg.cloneTargets = []string{"ours", "theirs", "absent"}
		for name, labels := range map[string]map[string]string{
			"ours":   {loadgenRunLabelKey: lg.runID},
			"theirs": {loadgenRunLabelKey: "another-run"},
		} {
			require.NoError(t, env.Client.Create(ctx, &infrav1beta1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
				Spec: infrav1beta1.VirtualMachineSpec{
					ProviderRef: infrav1beta1.ObjectRef{Name: "lab-provider"}, ClassRef: infrav1beta1.ObjectRef{Name: "small"},
				},
			}))
		}
		assert.Empty(t, lg.cleanup(ctx))
		vms := &infrav1beta1.VirtualMachineList{}
		require.NoError(t, env.Client.List(ctx, vms, client.InNamespace(ns)))
		require.Len(t, vms.Items, 1)
		assert.Equal(t, "theirs", vms.Items[0].Name)
	})

	t.Run("cleanup leaves a clone in flight, its source and its target, and reports them", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-inflight")
		lg := newGenerator(t, env.Client, ns, &options{cleanup: true})
		created := createVM(t, lg)
		createVM(t, lg)
		// Make the clone pick the first VM: only it is powered off.
		lg.vms[0].power = infrav1beta1.PowerStateOff
		lg.performClone(ctx, "lab-provider", 0)
		res := <-lg.results
		require.True(t, res.Success, res.Error)
		assert.Equal(t, created.VMName, res.VMName, "clone prefers a powered-off source")

		clone := &infrav1beta1.VMClone{}
		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: lg.clones[0].name}, clone))
		clone.Status.Phase = infrav1beta1.ClonePhaseCloning
		require.NoError(t, env.Client.Status().Update(ctx, clone))

		cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		left := lg.cleanup(cctx)
		assert.Contains(t, left, "VMClone/"+clone.Name+" (clone in flight)")
		assert.Contains(t, left, "VirtualMachine/"+created.VMName+" (source of a clone in flight)")
		assert.Contains(t, left, "VirtualMachine/"+clone.Spec.Target.Name+" (target of a clone in flight)")
		// The clone and its source are still there; the other VM is gone.
		require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(clone), &infrav1beta1.VMClone{}))
		getVM(t, env.Client, ns, created.VMName)
		vms := &infrav1beta1.VirtualMachineList{}
		require.NoError(t, env.Client.List(ctx, vms, client.InNamespace(ns)))
		assert.Len(t, vms.Items, 1)
	})

	t.Run("--observe counts a resize applied to the next boot as reflected", func(t *testing.T) {
		ns := createNamespace(t, env.Client, "lg-restart")
		lg := newGenerator(t, env.Client, ns, &options{})
		created := createVM(t, lg)
		vm := getVM(t, env.Client, ns, created.VMName)
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type: conditionReconfiguring, Status: metav1.ConditionTrue, Reason: reasonRestartRequired, Message: "next boot",
			ObservedGeneration: 1000, LastTransitionTime: metav1.Now(),
		})
		require.NoError(t, env.Client.Status().Update(ctx, vm))

		lg.opts.observe, lg.opts.observeTimeout = true, 5*time.Second
		lg.performReconfigure(ctx, "lab-provider", 0)
		res := <-lg.results
		require.True(t, res.Success, res.Error)
		assert.Positive(t, res.Observed, res.ObserveError)
	})
}

// TestFailuresKeepTrackOfRunObjects checks that a failed delete keeps the VM
// for --cleanup and that a clone target that cannot be read is reported.
func TestFailuresKeepTrackOfRunObjects(t *testing.T) {
	scheme, err := cliclient.NewScheme()
	require.NoError(t, err)
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).Build(), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return apierrors.NewInternalError(errors.New("apiserver unavailable"))
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "unreadable" {
				return apierrors.NewInternalError(errors.New("apiserver unavailable"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	lg := newGenerator(t, c, "default", &options{})
	createVM(t, lg)
	lg.performDelete(context.Background(), "lab-provider", 0)
	res := <-lg.results
	assert.False(t, res.Success)
	assert.Len(t, lg.vms, 1, "a VM whose delete failed must stay tracked")
	assert.Empty(t, lg.deleting)

	lg.cloneTargets = []string{"unreadable"}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	left := lg.cleanup(ctx)
	assert.Contains(t, strings.Join(left, "\n"), "VirtualMachine/unreadable (could not be read")
	assert.Contains(t, left, "VirtualMachine/"+lg.vms[0].name)
}

// TestDryRunMakesNoAPICalls runs every operation, the describe included, in
// dry-run mode against a client that counts calls.
func TestDryRunMakesNoAPICalls(t *testing.T) {
	scheme, err := cliclient.NewScheme()
	require.NoError(t, err)
	var calls atomic.Int32
	count := func() { calls.Add(1) }
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			count()
			return nil
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			count()
			return nil
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			count()
			return nil
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			count()
			return nil
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			count()
			return nil
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			count()
			return nil
		},
	})

	lg := newGenerator(t, c, "default", &options{dryRun: true, observe: true, cleanup: true})
	lg.performCreate(context.Background(), "lab-provider", 1)
	<-lg.results
	for _, op := range []func(context.Context, string, int){lg.performPower, lg.performReconfigure, lg.performSnapshot,
		lg.performClone, lg.performDescribe, lg.performDelete} {
		op(context.Background(), "lab-provider", 0)
		res := <-lg.results
		assert.True(t, res.Success, "%s: %s", res.Operation, res.Error)
		assert.Equal(t, phaseDryRun, res.Phase, res.Operation)
	}
	require.NoError(t, lg.finish(&strings.Builder{}))
	assert.Zero(t, calls.Load(), "a dry run made API calls")

	t.Run("the command needs no kubeconfig", func(t *testing.T) {
		out, err := runLoadgen(t, "run", "--dry-run", "--kubeconfig", filepath.Join(t.TempDir(), "absent"),
			"--config", writeConfig(t, strings.Replace(allOpsConfig, "duration: 4s", "duration: 2s", 1)), "--output-dir", t.TempDir())
		require.NoError(t, err, out)
	})
}
