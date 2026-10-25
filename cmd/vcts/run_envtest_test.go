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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/envtestutil"
	"github.com/projectbeskar/virtrigaud/internal/conformance"
)

// envNamespace is the namespace the runner tests work in.
const envNamespace = "default"

// stuckMarker in a VirtualMachine's name makes fakeProvider ignore it, so a
// wait on it times out; in a VMClone's name it makes fakeProvider hold the
// clone in phase Cloning (a provider clone in flight).
const stuckMarker = "stuck"

// pinnedMarker in a VirtualMachine's name makes fakeProvider give it a
// finalizer, so deleting it never completes.
const pinnedMarker = "pinned"

// pinFinalizer is the finalizer fakeProvider puts on pinned VMs.
const pinFinalizer = "test.virtrigaud.io/pin"

// fakeProvider stands in for the manager and a Provider (envtest runs no
// controller): it writes the status a healthy provider would produce for
// every VirtualMachine, VMSnapshot and VMClone in the namespace, and creates
// a clone's target VirtualMachine.
type fakeProvider struct {
	c  client.Client
	ns string

	mu   sync.Mutex
	seen map[string]bool // VM names observed
}

// run updates statuses until ctx ends.
func (f *fakeProvider) run(ctx context.Context) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.tick(ctx)
		}
	}
}

// sawVM reports whether a VirtualMachine named name was observed.
func (f *fakeProvider) sawVM(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[name]
}

func readyCondition(gen int64) metav1.Condition {
	return metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "ReconcileSuccess", Message: "ready",
		ObservedGeneration: gen, LastTransitionTime: metav1.Now(),
	}
}

func (f *fakeProvider) tick(ctx context.Context) {
	vms := &infrav1beta1.VirtualMachineList{}
	if err := f.c.List(ctx, vms, client.InNamespace(f.ns)); err == nil {
		for i := range vms.Items {
			f.reconcileVM(ctx, &vms.Items[i])
		}
	}
	snaps := &infrav1beta1.VMSnapshotList{}
	if err := f.c.List(ctx, snaps, client.InNamespace(f.ns)); err == nil {
		for i := range snaps.Items {
			s := &snaps.Items[i]
			if s.Status.Phase == infrav1beta1.SnapshotPhaseReady {
				continue
			}
			s.Status.Phase = infrav1beta1.SnapshotPhaseReady
			s.Status.SnapshotID = "snap-" + s.Name
			meta.SetStatusCondition(&s.Status.Conditions, readyCondition(s.Generation))
			_ = f.c.Status().Update(ctx, s)
		}
	}
	clones := &infrav1beta1.VMCloneList{}
	if err := f.c.List(ctx, clones, client.InNamespace(f.ns)); err == nil {
		for i := range clones.Items {
			f.reconcileClone(ctx, &clones.Items[i])
		}
	}
}

// effectiveResources is the VM's class size with spec.resources applied.
func (f *fakeProvider) effectiveResources(ctx context.Context, vm *infrav1beta1.VirtualMachine) (int32, int64) {
	cpu, mem := int32(1), int64(1024)
	class := &infrav1beta1.VMClass{}
	if err := f.c.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: vm.Spec.ClassRef.Name}, class); err == nil {
		cpu = class.Spec.CPU
		mem = class.Spec.Memory.Value() / (1024 * 1024)
	}
	if r := vm.Spec.Resources; r != nil {
		if r.CPU != nil {
			cpu = *r.CPU
		}
		if r.MemoryMiB != nil {
			mem = *r.MemoryMiB
		}
	}
	return cpu, mem
}

func (f *fakeProvider) reconcileVM(ctx context.Context, vm *infrav1beta1.VirtualMachine) {
	f.mu.Lock()
	f.seen[vm.Name] = true
	f.mu.Unlock()
	if strings.Contains(vm.Name, stuckMarker) {
		return
	}
	if strings.Contains(vm.Name, pinnedMarker) && !controllerutil.ContainsFinalizer(vm, pinFinalizer) {
		controllerutil.AddFinalizer(vm, pinFinalizer)
		_ = f.c.Update(ctx, vm)
		return
	}
	power := infrav1beta1.ObservedPowerStateOn
	phase := infrav1beta1.VirtualMachinePhaseRunning
	if vm.Spec.PowerState == infrav1beta1.PowerStateOff || vm.Spec.PowerState == infrav1beta1.PowerStateOffGraceful {
		power = infrav1beta1.ObservedPowerStateOff
		phase = infrav1beta1.VirtualMachinePhaseStopped
	}
	cpu, mem := f.effectiveResources(ctx, vm)
	ready := meta.FindStatusCondition(vm.Status.Conditions, "Ready")
	if vm.Status.ID != "" && vm.Status.PowerState == power && ready != nil && ready.ObservedGeneration == vm.Generation &&
		vm.Status.CurrentResources != nil && *vm.Status.CurrentResources.CPU == cpu && *vm.Status.CurrentResources.MemoryMiB == mem {
		return
	}
	vm.Status.ID = "fake-" + vm.Name
	vm.Status.PowerState = power
	vm.Status.Phase = phase
	vm.Status.CurrentResources = &infrav1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
	vm.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: f.ns, Name: vm.Spec.ProviderRef.Name}
	meta.SetStatusCondition(&vm.Status.Conditions, readyCondition(vm.Generation))
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type: "Reconfiguring", Status: metav1.ConditionFalse, Reason: "ReconcileSuccess", Message: "done",
		ObservedGeneration: vm.Generation, LastTransitionTime: metav1.Now(),
	})
	_ = f.c.Status().Update(ctx, vm)
}

func (f *fakeProvider) reconcileClone(ctx context.Context, clone *infrav1beta1.VMClone) {
	if clone.Status.Phase == infrav1beta1.ClonePhaseReady || clone.Spec.Source.VMRef == nil {
		return
	}
	if strings.Contains(clone.Name, stuckMarker) {
		if clone.Status.Phase != infrav1beta1.ClonePhaseCloning {
			clone.Status.Phase = infrav1beta1.ClonePhaseCloning
			_ = f.c.Status().Update(ctx, clone)
		}
		return
	}
	src := &infrav1beta1.VirtualMachine{}
	if err := f.c.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: clone.Spec.Source.VMRef.Name}, src); err != nil {
		return
	}
	target := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: clone.Spec.Target.Name, Namespace: f.ns, Labels: clone.Spec.Target.Labels,
			Annotations: map[string]string{"virtrigaud.io/cloned-from": src.Name, "virtrigaud.io/clone": clone.Name},
		},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: src.Spec.ProviderRef, ClassRef: src.Spec.ClassRef, PowerState: infrav1beta1.PowerStateOff,
		},
	}
	if err := f.c.Create(ctx, target); err != nil && !apierrors.IsAlreadyExists(err) {
		return
	}
	clone.Status.Phase = infrav1beta1.ClonePhaseReady
	clone.Status.TargetRef = &infrav1beta1.LocalObjectReference{Name: target.Name}
	meta.SetStatusCondition(&clone.Status.Conditions, readyCondition(clone.Generation))
	_ = f.c.Status().Update(ctx, clone)
}

// specDirWith writes spec as the only specification file of a new directory.
func specDirWith(t *testing.T, spec string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o600))
	return dir
}

// runnerEnv is an apiserver with a Provider and a fakeProvider.
type runnerEnv struct {
	env  *envtestutil.Env
	fake *fakeProvider
}

func startRunnerEnv(t *testing.T) *runnerEnv {
	t.Helper()
	env := envtestutil.Start(t)
	ctx, cancel := context.WithCancel(context.Background())

	require.NoError(t, env.Client.Create(ctx, &infrav1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "conf-provider", Namespace: envNamespace},
		Spec: infrav1beta1.ProviderSpec{
			Type:                infrav1beta1.ProviderTypeLibvirt,
			Endpoint:            "qemu+tcp://libvirt.example.com/system",
			CredentialSecretRef: infrav1beta1.ObjectRef{Name: "libvirt-creds"},
			Runtime: &infrav1beta1.ProviderRuntimeSpec{
				Mode:  infrav1beta1.RuntimeModeRemote,
				Image: "example.invalid/provider-libvirt:test",
			},
		},
	}))

	fake := &fakeProvider{c: env.Client, ns: envNamespace, seen: map[string]bool{}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		fake.run(ctx)
	}()
	// Cleanups run last-registered first: stop the fake, then wait for it.
	t.Cleanup(wg.Wait)
	t.Cleanup(cancel)
	return &runnerEnv{env: env, fake: fake}
}

// setReportedCapabilities sets the Provider's status.reportedCapabilities.
func (e *runnerEnv) setReportedCapabilities(t *testing.T, caps *infrav1beta1.ReportedCapabilities) {
	t.Helper()
	ctx := context.Background()
	p := &infrav1beta1.Provider{}
	require.NoError(t, e.env.Client.Get(ctx, types.NamespacedName{Namespace: envNamespace, Name: "conf-provider"}, p))
	p.Status.ReportedCapabilities = caps
	p.Status.Healthy = true
	require.NoError(t, e.env.Client.Status().Update(ctx, p))
}

// newRunner returns a runner for the specifications in specDir.
func (e *runnerEnv) newRunner(t *testing.T, specDir string, mutate func(*conformance.Config)) *conformance.Runner {
	t.Helper()
	cfg := conformance.Config{
		KubeClient:     e.env.Client,
		Namespace:      envNamespace,
		Provider:       "conf-provider",
		OutputDir:      t.TempDir(),
		SpecDir:        specDir,
		Vars:           map[string]string{"IMAGE": "test-image"},
		PollInterval:   100 * time.Millisecond,
		CleanupTimeout: 30 * time.Second,
		Parallel:       1,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return conformance.NewRunner(cfg)
}

// runObjects returns "Kind/name" of every VirtualMachine, VMClass, VMSnapshot
// and VMClone carrying the run label runID.
func (e *runnerEnv) runObjects(t *testing.T, runID string) []string {
	t.Helper()
	ctx := context.Background()
	sel := client.MatchingLabels{conformance.LabelRun: runID}
	var out []string
	vms := &infrav1beta1.VirtualMachineList{}
	require.NoError(t, e.env.Client.List(ctx, vms, client.InNamespace(envNamespace), sel))
	for _, o := range vms.Items {
		out = append(out, "VirtualMachine/"+o.Name)
	}
	classes := &infrav1beta1.VMClassList{}
	require.NoError(t, e.env.Client.List(ctx, classes, client.InNamespace(envNamespace), sel))
	for _, o := range classes.Items {
		out = append(out, "VMClass/"+o.Name)
	}
	snaps := &infrav1beta1.VMSnapshotList{}
	require.NoError(t, e.env.Client.List(ctx, snaps, client.InNamespace(envNamespace), sel))
	for _, o := range snaps.Items {
		out = append(out, "VMSnapshot/"+o.Name)
	}
	clones := &infrav1beta1.VMCloneList{}
	require.NoError(t, e.env.Client.List(ctx, clones, client.InNamespace(envNamespace), sel))
	for _, o := range clones.Items {
		out = append(out, "VMClone/"+o.Name)
	}
	return out
}

// getRunVM reads the VirtualMachine named name.
func getRunVM(t *testing.T, e *runnerEnv, name string) *infrav1beta1.VirtualMachine {
	t.Helper()
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, e.env.Client.Get(context.Background(), types.NamespacedName{Namespace: envNamespace, Name: name}, vm))
	return vm
}

// testResult returns the result of the test named name.
func testResult(t *testing.T, res *conformance.Results, name string) conformance.TestResult {
	t.Helper()
	for _, tr := range res.Tests {
		if tr.Name == name {
			return tr
		}
	}
	t.Fatalf("no result for test %s", name)
	return conformance.TestResult{}
}

const classStep = `    - name: create-class
      type: create
      resource:
        apiVersion: infra.virtrigaud.io/v1beta1
        kind: VMClass
        metadata:
          name: vcts-${RUN}-class
        spec:
          cpu: 1
          memory: 1Gi
`

// vmStep creates a VirtualMachine named vcts-${RUN}-<suffix>.
func vmStep(suffix string) string {
	return `    - name: create-vm
      type: create
      resource:
        apiVersion: infra.virtrigaud.io/v1beta1
        kind: VirtualMachine
        metadata:
          name: vcts-${RUN}-` + suffix + `
        spec:
          providerRef:
            name: ${PROVIDER}
          classRef:
            name: vcts-${RUN}-class
          imageRef:
            name: ${IMAGE}
          powerState: "On"
`
}

// vmRef is the resource block naming vcts-${RUN}-<suffix>.
func vmRef(suffix string) string {
	return `      resource:
        apiVersion: infra.virtrigaud.io/v1beta1
        kind: VirtualMachine
        metadata:
          name: vcts-${RUN}-` + suffix + `
`
}

func TestRunnerAgainstAPIServer(t *testing.T) {
	e := startRunnerEnv(t)
	ctx := context.Background()

	t.Run("create, wait, validate and delete pass; the class is cleaned up", func(t *testing.T) {
		spec := "- name: lifecycle\n  description: d\n  steps:\n" + classStep + vmStep("vm") +
			"    - name: wait-ready\n      type: wait\n" + vmRef("vm") + "      waitFor:\n        condition: Ready\n" +
			"      validate:\n        - path: .status.powerState\n          value: \"On\"\n" +
			"    - name: validate\n      type: validate\n" + vmRef("vm") +
			"      validate:\n        - path: .status.id\n          operator: exists\n" +
			"        - path: .status.currentResources.memoryMiB\n          value: 1024\n" +
			"        - path: .metadata.labels.vcts\\.virtrigaud\\.io/test\n          value: ${TEST}\n" +
			"    - name: power-off\n      type: update\n" + vmRef("vm") + "        spec:\n          powerState: \"Off\"\n" +
			"    - name: wait-off\n      type: wait\n" + vmRef("vm") + "      waitFor:\n        condition: Stopped\n" +
			"    - name: delete\n      type: delete\n" + vmRef("vm") +
			"    - name: wait-deleted\n      type: wait\n" + vmRef("vm") + "      waitFor:\n        condition: Deleted\n"
		r := e.newRunner(t, specDirWith(t, spec), nil)
		res, err := r.Run(ctx)
		require.NoError(t, err)
		tr := testResult(t, res, "lifecycle")
		require.Equal(t, conformance.StatusPassed, tr.Status, "%+v", tr)
		assert.Len(t, tr.Steps, 8)
		for _, s := range tr.Steps {
			assert.Equal(t, conformance.StatusPassed, s.Status, s.Name)
		}
		assert.Equal(t, 1, res.Passed)
		assert.Empty(t, e.runObjects(t, res.RunID), "cleanup left objects behind")
	})

	t.Run("a failing assertion fails the test, skips the rest and still cleans up", func(t *testing.T) {
		spec := "- name: bad-assertion\n  description: d\n  steps:\n" + classStep + vmStep("vm") +
			"    - name: wait-ready\n      type: wait\n" + vmRef("vm") + "      waitFor:\n        condition: Ready\n" +
			"    - name: validate\n      type: validate\n" + vmRef("vm") +
			"      validate:\n        - path: .status.powerState\n          value: \"Off\"\n" +
			"    - name: never-run\n      type: delete\n" + vmRef("vm")
		outDir := t.TempDir()
		r := e.newRunner(t, specDirWith(t, spec), func(c *conformance.Config) { c.OutputDir = outDir })
		res, err := r.Run(ctx)
		require.NoError(t, err)
		tr := testResult(t, res, "bad-assertion")
		require.Equal(t, conformance.StatusFailed, tr.Status)
		assert.Contains(t, tr.Error, `step validate: VirtualMachine/vcts-`+res.RunID+`-vm: .status.powerState: got "On", want eq "Off"`)
		assert.Equal(t, conformance.StatusSkipped, tr.Steps[4].Status)
		assert.Contains(t, tr.Steps[4].Error, "not run: step validate failed")
		assert.Equal(t, 1, res.Failed)
		assert.Empty(t, e.runObjects(t, res.RunID), "cleanup after a failure left objects behind")
		for _, f := range []string{"results.json", "junit.xml", "report.md"} {
			assert.FileExists(t, filepath.Join(outDir, f))
		}
		junit, err := os.ReadFile(filepath.Join(outDir, "junit.xml"))
		require.NoError(t, err)
		assert.Contains(t, string(junit), `failures="1"`)
	})

	t.Run("a wait that never completes times out with the last observed state", func(t *testing.T) {
		spec := "- name: times-out\n  description: d\n  steps:\n" + classStep + vmStep(stuckMarker) +
			"    - name: wait-ready\n      type: wait\n" + vmRef(stuckMarker) + "      waitFor:\n        condition: Ready\n        timeout: 1s\n"
		r := e.newRunner(t, specDirWith(t, spec), nil)
		start := time.Now()
		res, err := r.Run(ctx)
		require.NoError(t, err)
		assert.Less(t, time.Since(start), 20*time.Second)
		tr := testResult(t, res, "times-out")
		require.Equal(t, conformance.StatusFailed, tr.Status)
		assert.Contains(t, tr.Error, "timed out after")
		assert.Contains(t, tr.Error, "waiting for Ready on VirtualMachine/vcts-"+res.RunID+"-stuck")
		assert.Contains(t, tr.Error, "no Ready condition")
		assert.Empty(t, e.runObjects(t, res.RunID))
	})

	t.Run("a test needing a capability the Provider does not offer is skipped", func(t *testing.T) {
		spec := "- name: needs-snapshots\n  description: d\n  requiredCapabilities: [vm-snapshot]\n  steps:\n" + classStep +
			"- name: core-only\n  description: d\n  requiredCapabilities: [vm-create]\n  steps:\n" + classStep
		e.setReportedCapabilities(t, nil)
		res, err := e.newRunner(t, specDirWith(t, spec), nil).Run(ctx)
		require.NoError(t, err)
		skipped := testResult(t, res, "needs-snapshots")
		assert.Equal(t, conformance.StatusSkipped, skipped.Status)
		assert.Equal(t, "Provider conf-provider does not offer vm-snapshot", skipped.SkipReason)
		assert.Empty(t, skipped.Steps)
		assert.Equal(t, conformance.StatusPassed, testResult(t, res, "core-only").Status)
		assert.Equal(t, 2, res.Total)
		assert.Equal(t, 1, res.Skipped)
		assert.Equal(t, 0, res.Failed)

		e.setReportedCapabilities(t, &infrav1beta1.ReportedCapabilities{SupportsSnapshots: true})
		res, err = e.newRunner(t, specDirWith(t, spec), nil).Run(ctx)
		require.NoError(t, err)
		assert.Equal(t, conformance.StatusPassed, testResult(t, res, "needs-snapshots").Status)
		e.setReportedCapabilities(t, nil)
	})

	t.Run("an interrupted run still cleans up, and the tests it did not start fail", func(t *testing.T) {
		spec := "- name: interrupted\n  description: d\n  steps:\n" + classStep + vmStep(stuckMarker) +
			"    - name: wait-ready\n      type: wait\n" + vmRef(stuckMarker) + "      waitFor:\n        condition: Ready\n        timeout: 2m\n" +
			"- name: never-started\n  description: d\n  steps:\n" + classStep
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		r := e.newRunner(t, specDirWith(t, spec), nil)
		runID, err := r.RunID()
		require.NoError(t, err)
		go func() {
			// Interrupt once the VM exists.
			for runCtx.Err() == nil && !e.fake.sawVM("vcts-"+runID+"-"+stuckMarker) {
				select {
				case <-runCtx.Done():
				case <-time.After(20 * time.Millisecond):
				}
			}
			cancel()
		}()
		res, err := r.Run(runCtx)
		require.NoError(t, err)
		tr := testResult(t, res, "interrupted")
		assert.Equal(t, conformance.StatusFailed, tr.Status)
		assert.Contains(t, tr.Error, "interrupted after")
		assert.NotEmpty(t, tr.Cleanup)
		notStarted := testResult(t, res, "never-started")
		assert.Equal(t, conformance.StatusFailed, notStarted.Status, "a test the run never started must not pass or be skipped")
		assert.Contains(t, notStarted.Error, "not run")
		assert.Equal(t, 2, res.Failed)
		assert.Empty(t, e.runObjects(t, runID), "an interrupted run left objects behind")
	})

	t.Run("an object stuck in deletion does not keep the cleanup from deleting the others", func(t *testing.T) {
		spec := "- name: pinned\n  description: d\n  steps:\n" + classStep + vmStep(pinnedMarker) +
			"    - name: wait-pinned\n      type: wait\n" + vmRef(pinnedMarker) +
			"      validate:\n        - path: .metadata.finalizers\n          operator: contains\n          value: " + pinFinalizer + "\n"
		r := e.newRunner(t, specDirWith(t, spec), func(c *conformance.Config) { c.CleanupTimeout = time.Second })
		res, err := r.Run(ctx)
		require.NoError(t, err)
		tr := testResult(t, res, "pinned")
		assert.Equal(t, conformance.StatusFailed, tr.Status)
		assert.Contains(t, tr.Error, "cleanup delete VirtualMachine/vcts-"+res.RunID+"-pinned")
		// The class, deleted after the VM, still got its delete call.
		assert.Equal(t, []string{"VirtualMachine/vcts-" + res.RunID + "-pinned"}, e.runObjects(t, res.RunID))

		vm := getRunVM(t, e, "vcts-"+res.RunID+"-pinned")
		controllerutil.RemoveFinalizer(vm, pinFinalizer)
		require.NoError(t, e.env.Client.Update(ctx, vm))
	})

	t.Run("a clone still in flight is left in place with its source and target", func(t *testing.T) {
		spec := "- name: clone-in-flight\n  description: d\n  steps:\n" + classStep + vmStep("src") +
			`    - name: create-clone
      type: create
      resource:
        apiVersion: infra.virtrigaud.io/v1beta1
        kind: VMClone
        metadata:
          name: vcts-${RUN}-` + stuckMarker + `-clone
        spec:
          source:
            vmRef:
              name: vcts-${RUN}-src
          target:
            name: vcts-${RUN}-dst
    - name: wait-cloning
      type: wait
      resource:
        apiVersion: infra.virtrigaud.io/v1beta1
        kind: VMClone
        metadata:
          name: vcts-${RUN}-` + stuckMarker + `-clone
      validate:
        - path: .status.phase
          value: Cloning
`
		r := e.newRunner(t, specDirWith(t, spec), func(c *conformance.Config) { c.CleanupTimeout = time.Second })
		res, err := r.Run(ctx)
		require.NoError(t, err)
		tr := testResult(t, res, "clone-in-flight")
		assert.Equal(t, conformance.StatusFailed, tr.Status)
		assert.Contains(t, tr.Error, "still has a clone in flight")
		assert.ElementsMatch(t, []string{
			"VMClone/vcts-" + res.RunID + "-" + stuckMarker + "-clone",
			"VirtualMachine/vcts-" + res.RunID + "-src",
		}, e.runObjects(t, res.RunID), "the clone and its source must stay; the class is deleted")
		require.NoError(t, e.env.Client.DeleteAllOf(ctx, &infrav1beta1.VMClone{}, client.InNamespace(envNamespace),
			client.MatchingLabels{conformance.LabelRun: res.RunID}))
		require.NoError(t, e.env.Client.DeleteAllOf(ctx, &infrav1beta1.VirtualMachine{}, client.InNamespace(envNamespace),
			client.MatchingLabels{conformance.LabelRun: res.RunID}))
	})

	t.Run("an update cannot change the run labels", func(t *testing.T) {
		spec := "- name: relabel\n  description: d\n  steps:\n" + classStep +
			"    - name: relabel\n      type: update\n      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n" +
			"        kind: VMClass\n        metadata:\n          name: vcts-${RUN}-class\n          labels:\n" +
			"            vcts.virtrigaud.io/run: other\n"
		res, err := e.newRunner(t, specDirWith(t, spec), nil).Run(ctx)
		require.NoError(t, err)
		tr := testResult(t, res, "relabel")
		assert.Equal(t, conformance.StatusFailed, tr.Status)
		assert.Contains(t, tr.Error, "an update cannot change the label "+conformance.LabelRun)
		assert.Empty(t, e.runObjects(t, res.RunID))
	})

	t.Run("objects created on the test's behalf are cleaned up by label", func(t *testing.T) {
		spec := "- name: clone\n  description: d\n  steps:\n" + classStep + vmStep("src") +
			`    - name: create-clone
      type: create
      resource:
        apiVersion: infra.virtrigaud.io/v1beta1
        kind: VMClone
        metadata:
          name: vcts-${RUN}-clone
        spec:
          source:
            vmRef:
              name: vcts-${RUN}-src
          target:
            name: vcts-${RUN}-dst
            labels:
              vcts.virtrigaud.io/run: ${RUN}
              vcts.virtrigaud.io/test: ${TEST}
    - name: wait-clone
      type: wait
      resource:
        apiVersion: infra.virtrigaud.io/v1beta1
        kind: VMClone
        metadata:
          name: vcts-${RUN}-clone
      waitFor:
        condition: Ready
    - name: wait-target
      type: wait
` + vmRef("dst") + "      waitFor:\n        condition: Provisioned\n"
		res, err := e.newRunner(t, specDirWith(t, spec), nil).Run(ctx)
		require.NoError(t, err)
		tr := testResult(t, res, "clone")
		require.Equal(t, conformance.StatusPassed, tr.Status, "%+v", tr)
		var names []string
		for _, c := range tr.Cleanup {
			names = append(names, c.Name)
		}
		assert.Contains(t, names, "delete VirtualMachine/vcts-"+res.RunID+"-dst (labelled with this run)")
		assert.Empty(t, e.runObjects(t, res.RunID))
	})

	t.Run("an object the run did not create is never changed or deleted", func(t *testing.T) {
		bystander := &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "bystander", Namespace: envNamespace},
			Spec: infrav1beta1.VirtualMachineSpec{
				ProviderRef: infrav1beta1.ObjectRef{Name: "conf-provider"},
				ClassRef:    infrav1beta1.ObjectRef{Name: "small"},
				PowerState:  infrav1beta1.PowerStateOn,
			},
		}
		require.NoError(t, e.env.Client.Create(ctx, bystander))
		ref := "      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n        kind: VirtualMachine\n        metadata:\n          name: bystander\n"
		spec := "- name: touch-update\n  description: d\n  steps:\n    - name: update\n      type: update\n" + ref +
			"        spec:\n          powerState: \"Off\"\n" +
			"- name: touch-delete\n  description: d\n  steps:\n    - name: delete\n      type: delete\n" + ref
		res, err := e.newRunner(t, specDirWith(t, spec), nil).Run(ctx)
		require.NoError(t, err)
		for _, name := range []string{"touch-update", "touch-delete"} {
			tr := testResult(t, res, name)
			assert.Equal(t, conformance.StatusFailed, tr.Status)
			assert.Contains(t, tr.Error, "does not carry the label "+conformance.LabelRun)
		}
		got := &infrav1beta1.VirtualMachine{}
		require.NoError(t, e.env.Client.Get(ctx, client.ObjectKeyFromObject(bystander), got))
		assert.Equal(t, infrav1beta1.PowerStateOn, got.Spec.PowerState)
		assert.Nil(t, got.DeletionTimestamp)
		require.NoError(t, e.env.Client.Delete(ctx, got))
	})

	t.Run("--keep leaves the objects in place", func(t *testing.T) {
		spec := "- name: kept\n  description: d\n  steps:\n" + classStep + vmStep("vm")
		res, err := e.newRunner(t, specDirWith(t, spec), func(c *conformance.Config) { c.Keep = true }).Run(ctx)
		require.NoError(t, err)
		tr := testResult(t, res, "kept")
		assert.Equal(t, conformance.StatusPassed, tr.Status)
		assert.Empty(t, tr.Cleanup)
		assert.ElementsMatch(t, []string{"VMClass/vcts-" + res.RunID + "-class", "VirtualMachine/vcts-" + res.RunID + "-vm"}, tr.Kept)
		assert.ElementsMatch(t, tr.Kept, e.runObjects(t, res.RunID))
		require.NoError(t, e.env.Client.DeleteAllOf(ctx, &infrav1beta1.VirtualMachine{}, client.InNamespace(envNamespace),
			client.MatchingLabels{conformance.LabelRun: res.RunID}))
		require.NoError(t, e.env.Client.DeleteAllOf(ctx, &infrav1beta1.VMClass{}, client.InNamespace(envNamespace),
			client.MatchingLabels{conformance.LabelRun: res.RunID}))
	})

	t.Run("a variable that is not set stops the run before anything is created", func(t *testing.T) {
		spec := "- name: needs-image\n  description: d\n  steps:\n" + classStep + vmStep("vm")
		r := e.newRunner(t, specDirWith(t, spec), func(c *conformance.Config) { c.Vars = nil })
		runID, err := r.RunID()
		require.NoError(t, err)
		_, err = r.Run(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "${IMAGE} (used by needs-image; pass --set IMAGE=<value>)")
		assert.Empty(t, e.runObjects(t, runID))
	})

	t.Run("the shipped specifications pass against a well-behaved provider", func(t *testing.T) {
		e.setReportedCapabilities(t, &infrav1beta1.ReportedCapabilities{SupportsSnapshots: true})
		defer e.setReportedCapabilities(t, nil)
		r := e.newRunner(t, filepath.Join("..", "..", conformance.DefaultSpecDir), func(c *conformance.Config) { c.Parallel = 3 })
		res, err := r.Run(ctx)
		require.NoError(t, err)
		for _, tr := range res.Tests {
			assert.Equal(t, conformance.StatusPassed, tr.Status, "%s: %s", tr.Name, tr.Error)
		}
		assert.Equal(t, res.Total, res.Passed)
		assert.Empty(t, e.runObjects(t, res.RunID))
	})

	t.Run("vcts run exits non-zero when a test fails and zero when all pass", func(t *testing.T) {
		pass := "- name: ok\n  description: d\n  steps:\n" + classStep
		fail := "- name: not-ok\n  description: d\n  steps:\n" + classStep +
			"    - name: validate\n      type: validate\n      resource:\n        apiVersion: infra.virtrigaud.io/v1beta1\n" +
			"        kind: VMClass\n        metadata:\n          name: vcts-${RUN}-class\n" +
			"      validate:\n        - path: .spec.cpu\n          value: 4\n"
		out, err := runVcts(t, "run", "--kubeconfig", e.env.KubeconfigPath, "-p", "conf-provider",
			"--spec-dir", specDirWith(t, pass), "-o", t.TempDir())
		require.NoError(t, err, out)
		assert.Contains(t, out, "Passed: 1")
		assert.Contains(t, out, "Run ID: ")

		out, err = runVcts(t, "run", "--kubeconfig", e.env.KubeconfigPath, "-p", "conf-provider",
			"--spec-dir", specDirWith(t, fail), "-o", t.TempDir())
		require.ErrorIs(t, err, errTestsFailed)
		assert.Contains(t, out, "Failed: 1")
		assert.Contains(t, out, `.spec.cpu: got 1, want eq 4`)
	})
}
