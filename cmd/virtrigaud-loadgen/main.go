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
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/cliclient"
	"github.com/projectbeskar/virtrigaud/cmd/internal/cliout"
	"github.com/projectbeskar/virtrigaud/internal/util/closer"
)

const (
	// loadgenLabelKey and loadgenLabelValue mark every object the tool
	// creates (VirtualMachines, VMSnapshots, VMClones and clone targets).
	loadgenLabelKey   = "generated-by"
	loadgenLabelValue = "loadgen"

	// loadgenRunLabelKey holds the run id: it tells one run's objects from
	// another's.
	loadgenRunLabelKey = "loadgen.virtrigaud.io/run"

	// operationTimeout bounds one create, delete or list call, so an
	// operation that is in flight when the run's duration ends can finish.
	operationTimeout = 30 * time.Second
)

// version is the release the binary was built from; the release workflow sets
// it with -ldflags "-X main.version=...".
var version = "dev"

// options holds the values of the tool's flags.
type options struct {
	kubeconfig string
	namespace  string
	outputDir  string
	configFile string
	dryRun     bool
	verbose    bool

	cleanup        bool
	cleanupTimeout time.Duration
	observe        bool
	observeTimeout time.Duration
}

// LoadGenConfig defines the load generation configuration
type LoadGenConfig struct {
	// Test configuration
	Duration     time.Duration `yaml:"duration"`
	Concurrency  int           `yaml:"concurrency"`
	RampUpTime   time.Duration `yaml:"rampUpTime"`
	SteadyState  time.Duration `yaml:"steadyState"`
	RampDownTime time.Duration `yaml:"rampDownTime"`

	// VM configuration
	VMTemplate VMTemplate `yaml:"vmTemplate"`
	Providers  []string   `yaml:"providers"`
	VMCount    int        `yaml:"vmCount"`

	// Operation mix
	Operations OperationMix `yaml:"operations"`

	// Intervals
	CreateInterval   time.Duration `yaml:"createInterval"`
	PowerInterval    time.Duration `yaml:"powerInterval"`
	ReconfigInterval time.Duration `yaml:"reconfigInterval"`
	SnapshotInterval time.Duration `yaml:"snapshotInterval"`
	CloneInterval    time.Duration `yaml:"cloneInterval"`
	DescribeInterval time.Duration `yaml:"describeInterval"`
}

// VMTemplate defines the VM template for load testing
type VMTemplate struct {
	ClassRef    string            `yaml:"classRef"`
	ImageRef    string            `yaml:"imageRef"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
	Resources   VMResources       `yaml:"resources"`
}

// VMResources defines resource configurations for load testing
type VMResources struct {
	CPURange    []int `yaml:"cpuRange"`    // [min, max]
	MemoryRange []int `yaml:"memoryRange"` // [min, max] in MiB
}

// OperationMix defines the mix of operations to perform
type OperationMix struct {
	Create      float64 `yaml:"create"`      // Percentage of create operations
	Delete      float64 `yaml:"delete"`      // Percentage of delete operations
	Power       float64 `yaml:"power"`       // Percentage of power operations
	Reconfigure float64 `yaml:"reconfigure"` // Percentage of reconfigure operations
	Snapshot    float64 `yaml:"snapshot"`    // Percentage of snapshot operations
	Clone       float64 `yaml:"clone"`       // Percentage of clone operations
	Describe    float64 `yaml:"describe"`    // Percentage of describe operations
}

// Result represents the result of a single operation
type Result struct {
	Operation string
	Provider  string
	VMName    string
	StartTime time.Time
	// Duration is the time the API call took: from sending the request until
	// the apiserver accepted (or refused) it.
	Duration time.Duration
	Success  bool
	Error    string
	Phase    string // what the operation asked for (power state, size, object name)
	// Observed is, with --observe, the time from sending the request until
	// the object's status reflected it; zero when not observed.
	Observed time.Duration
	// ObserveError says why the status was not seen to reflect the request
	// (--observe only).
	ObserveError string
}

// LoadGenerator generates load against virtrigaud
type LoadGenerator struct {
	client    client.Client
	namespace string
	config    LoadGenConfig
	results   chan Result
	opts      *options

	// runID makes VM names unique across runs in one namespace.
	runID string

	// observeCtx bounds --observe polling and the cleanup: the command's
	// context, which an interrupt cancels but the run's duration does not.
	observeCtx context.Context
	// pollInterval is how often --observe and the cleanup read an object.
	pollInterval time.Duration

	mu        sync.Mutex
	vmCounter int
	// vms holds the VMs this run created and has not deleted. Every
	// operation acts on one of these, never on another VM in the namespace.
	vms []*runVM
	// snapshots and clones hold the VMSnapshots and VMClones this run
	// created; cloneTargets the target VMs those clones were asked to create.
	snapshots    []runObject
	clones       []runObject
	cloneTargets []string
}

// Statistics holds performance statistics
type Statistics struct {
	TotalOperations    int
	SuccessfulOps      int
	FailedOps          int
	OperationsByType   map[string]int
	OperationsByResult map[string]int
	DurationsByOp      map[string][]time.Duration
	// ObservedByOp holds, per operation, the --observe times (request sent
	// until the status reflected it) of the operations that were observed.
	ObservedByOp map[string][]time.Duration
	// ObserveFailures counts, per operation, the accepted requests whose
	// effect --observe did not see.
	ObserveFailures map[string]int
	StartTime       time.Time
	EndTime            time.Time
}

func main() {
	// The first SIGINT/SIGTERM ends the run early (and lets --cleanup run);
	// a second one terminates the process at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// newRootCmd builds the virtrigaud-loadgen command tree. Every call returns an
// independent tree with its own flag values, so it can be run repeatedly in
// one process.
func newRootCmd() *cobra.Command {
	opts := &options{}

	rootCmd := &cobra.Command{
		Use:           "virtrigaud-loadgen",
		Short:         "Load generation tool for virtrigaud",
		Long:          "Generate synthetic load against virtrigaud to test performance and scalability",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVar(&opts.kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	rootCmd.PersistentFlags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	rootCmd.PersistentFlags().StringVarP(&opts.outputDir, "output-dir", "o", "./loadgen-results", "Output directory for results")
	rootCmd.PersistentFlags().BoolVar(&opts.dryRun, "dry-run", false, "Dry run mode: no API calls at all (no kubeconfig needed)")
	rootCmd.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false, "Verbose output")

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run load generation",
		Long: `Run load generation against virtrigaud.

Without --config the built-in defaults are used (5 minutes, 2 workers, VMs
created against provider "test-provider"). --config reads a YAML file that
overrides them; unknown keys are an error.

Every operation but create and describe acts on a VirtualMachine this run
created, never on another VM. The latency recorded per operation is the API
call's: from sending the request until the apiserver accepted it. --observe
also records the time until the object's status reflects the request.
Objects the run created are left in place unless --cleanup is given.`,
		Args: cobra.NoArgs,
		RunE: opts.runLoadGen,
	}

	runCmd.Flags().StringVarP(&opts.configFile, "config", "c", "", "Load generation config file (YAML)")
	runCmd.Flags().BoolVar(&opts.cleanup, "cleanup", false,
		"At the end, delete every VMSnapshot, VMClone, clone target and VirtualMachine the run created and wait until they are gone")
	runCmd.Flags().DurationVar(&opts.cleanupTimeout, "cleanup-timeout", defaultCleanupTimeout, "Time allowed for --cleanup")
	runCmd.Flags().BoolVar(&opts.observe, "observe", false,
		"After each accepted request, wait until the object's status reflects it and record that time too (a worker waits before its next operation)")
	runCmd.Flags().DurationVar(&opts.observeTimeout, "observe-timeout", defaultObserveTimeout, "How long --observe waits per operation")

	rootCmd.AddCommand(runCmd)

	return rootCmd
}

// loadConfig returns the built-in defaults overridden by the YAML file at
// path. Durations are written as Go duration strings ("30s", "5m"). A key the
// tool does not know is an error, so a misspelt or foreign config is not
// silently ignored.
//
// It returns an error when the file cannot be read or parsed, or when the
// resulting configuration is not usable.
func loadConfig(path string) (LoadGenConfig, error) {
	cfg := getDefaultConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the config path is the --config flag named by the operator
	if err != nil {
		return LoadGenConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return LoadGenConfig{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return LoadGenConfig{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// validate checks that the configuration can drive a run.
func (c *LoadGenConfig) validate() error {
	if c.Duration <= 0 {
		return errors.New("duration must be positive")
	}
	if c.Concurrency < 1 {
		return errors.New("concurrency must be at least 1")
	}
	if len(c.Providers) == 0 {
		return errors.New("providers must name at least one provider")
	}
	if c.VMTemplate.ClassRef == "" || c.VMTemplate.ImageRef == "" {
		return errors.New("vmTemplate.classRef and vmTemplate.imageRef are required")
	}
	if err := validRange("vmTemplate.resources.cpuRange", c.VMTemplate.Resources.CPURange, 1, 128); err != nil {
		return err
	}
	if err := validRange("vmTemplate.resources.memoryRange", c.VMTemplate.Resources.MemoryRange, 128, 1048576); err != nil {
		return err
	}
	weights := []float64{
		c.Operations.Create, c.Operations.Delete, c.Operations.Power, c.Operations.Reconfigure,
		c.Operations.Snapshot, c.Operations.Clone, c.Operations.Describe,
	}
	total := 0.0
	for _, w := range weights {
		if w < 0 {
			return errors.New("operations weights must not be negative")
		}
		total += w
	}
	if total <= 0 {
		return errors.New("operations weights must not all be zero")
	}
	return nil
}

// validRange checks an optional [min, max] range: empty, or two values with
// lo <= min <= max <= hi (the VirtualMachine spec.resources bounds).
func validRange(field string, r []int, lo, hi int) error {
	if len(r) == 0 {
		return nil
	}
	if len(r) != 2 {
		return fmt.Errorf("%s must be [min, max]", field)
	}
	if r[0] > r[1] || r[0] < lo || r[1] > hi {
		return fmt.Errorf("%s must be [min, max] with %d <= min <= max <= %d", field, lo, hi)
	}
	return nil
}

func (o *options) runLoadGen(cmd *cobra.Command, _ []string) error {
	// Load configuration
	lgConfig, err := loadConfig(o.configFile)
	if err != nil {
		return err
	}

	// Create Kubernetes client. A dry run makes no API call, so it needs
	// none.
	var k8sClient client.Client
	if !o.dryRun {
		cfg, err := cliclient.RESTConfig(o.kubeconfig)
		if err != nil {
			return fmt.Errorf("failed to get kubeconfig: %w", err)
		}
		// No client-side rate limit: the recorded latencies must be the
		// apiserver's, not time spent waiting for a local token bucket
		// (client-go's default allows 5 requests per second).
		cfg.QPS = -1

		k8sClient, err = cliclient.NewClient(cfg)
		if err != nil {
			return fmt.Errorf("failed to create Kubernetes client: %w", err)
		}
	}

	// Create output directory
	if err := os.MkdirAll(o.outputDir, 0o750); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}

	// Create load generator
	generator := &LoadGenerator{
		client:       k8sClient,
		namespace:    o.namespace,
		config:       lgConfig,
		results:      make(chan Result, 1000),
		opts:         o,
		runID:        strconv.FormatInt(time.Now().UnixNano(), 36),
		observeCtx:   parent,
		pollInterval: defaultPollInterval,
	}
	ctx, cancel := context.WithTimeout(parent, lgConfig.Duration)
	defer cancel()

	out := cmd.OutOrStdout()
	cliout.Printf(out, "Starting load generation...\n")
	cliout.Printf(out, "Duration: %v\n", lgConfig.Duration)
	cliout.Printf(out, "Concurrency: %d\n", lgConfig.Concurrency)
	cliout.Printf(out, "VM Count: %d\n", lgConfig.VMCount)
	cliout.Printf(out, "Providers: %v\n", lgConfig.Providers)
	cliout.Printf(out, "Run ID: %s (label %s=%s)\n", generator.runID, loadgenRunLabelKey, generator.runID)

	// Start results collector
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		generator.collectResults(out)
	}()

	// Run load generation
	err = generator.run(ctx)

	// Wait for results collection to complete
	close(generator.results)
	wg.Wait()

	if err != nil {
		return fmt.Errorf("load generation failed: %w", err)
	}

	cliout.Printf(out, "Load generation completed successfully\n")
	cliout.Printf(out, "Results saved to: %s\n", o.outputDir)

	return generator.finish(out)
}

func (lg *LoadGenerator) run(ctx context.Context) error {
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < lg.config.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			lg.worker(ctx, workerID)
		}(i)
	}

	// Wait for all workers to complete
	wg.Wait()

	return nil
}

func (lg *LoadGenerator) worker(ctx context.Context, workerID int) {
	ticker := time.NewTicker(time.Second) // Base interval
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lg.performRandomOperation(ctx, workerID)
		}
	}
}

func (lg *LoadGenerator) performRandomOperation(ctx context.Context, workerID int) {
	// Randomly select an operation based on the operation mix
	op := lg.selectRandomOperation()
	provider := lg.selectRandomProvider()

	switch op {
	case "create":
		lg.performCreate(ctx, provider, workerID)
	case "delete":
		lg.performDelete(ctx, provider, workerID)
	case "power":
		lg.performPower(ctx, provider, workerID)
	case "reconfigure":
		lg.performReconfigure(ctx, provider, workerID)
	case "snapshot":
		lg.performSnapshot(ctx, provider, workerID)
	case "clone":
		lg.performClone(ctx, provider, workerID)
	case "describe":
		lg.performDescribe(ctx, provider, workerID)
	}
}

func (lg *LoadGenerator) performCreate(ctx context.Context, provider string, workerID int) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	vmName := lg.generateVMName(workerID)

	vm := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmName,
			Namespace: lg.namespace,
			Labels:    lg.vmLabels(),
		},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: provider},
			ClassRef:    infrav1beta1.ObjectRef{Name: lg.config.VMTemplate.ClassRef},
			ImageRef:    &infrav1beta1.ObjectRef{Name: lg.config.VMTemplate.ImageRef},
			PowerState:  infrav1beta1.PowerStateOn,
		},
	}

	result := Result{
		Operation: "create",
		Provider:  provider,
		VMName:    vmName,
		StartTime: time.Now(),
	}

	if lg.opts.dryRun {
		result.Success = true
		result.Phase = phaseDryRun
		result.Duration = time.Since(result.StartTime)
		lg.recordCreated(&runVM{name: vmName, provider: provider, power: infrav1beta1.PowerStateOn})
		lg.results <- result
		return
	}

	err := lg.client.Create(ctx, vm)
	result.Duration = time.Since(result.StartTime)
	result.Success = err == nil
	if err != nil {
		result.Error = err.Error()
		lg.results <- result
		return
	}
	result.Phase = "Creating"
	// Created VMs are busy until observed, so no other operation acts on a
	// VM whose create is still being measured.
	created := &runVM{name: vmName, uid: vm.UID, provider: provider, power: infrav1beta1.PowerStateOn, busy: true}
	lg.recordCreated(created)
	defer lg.releaseVM(created)

	lg.observe(&result, func(ctx context.Context) (bool, error) {
		cur := &infrav1beta1.VirtualMachine{}
		if err := lg.client.Get(ctx, client.ObjectKey{Namespace: lg.namespace, Name: vmName}, cur); err != nil {
			return false, ignoreNotFound(err)
		}
		if cur.Status.Phase == infrav1beta1.VirtualMachinePhaseFailed {
			return false, fmt.Errorf("VM failed: %s", cur.Status.Message)
		}
		return cur.Status.ID != "" && cur.Status.PowerState == infrav1beta1.ObservedPowerStateOn, nil
	})
	lg.results <- result
}

func (lg *LoadGenerator) performDelete(ctx context.Context, provider string, _ int) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	result := Result{
		Operation: "delete",
		Provider:  provider,
		StartTime: time.Now(),
	}

	// Delete only a VM this run created, never another VM in the namespace.
	vm, ok := lg.takeCreated()
	if !ok {
		result.Duration = time.Since(result.StartTime)
		result.Error = errNoRunVM.Error()
		lg.results <- result
		return
	}
	result.VMName = vm.name
	result.Provider = vm.provider

	if lg.opts.dryRun {
		result.Success = true
		result.Phase = phaseDryRun
		result.Duration = time.Since(result.StartTime)
		lg.results <- result
		return
	}

	obj := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: vm.name, Namespace: lg.namespace},
	}
	// The UID precondition makes the delete reach only the VM this run
	// created, never another VM later created under the same name.
	err := lg.client.Delete(ctx, obj, client.Preconditions{UID: &vm.uid})
	result.Duration = time.Since(result.StartTime)
	result.Success = err == nil
	if err != nil {
		result.Error = err.Error()
		lg.results <- result
		return
	}
	lg.observe(&result, func(ctx context.Context) (bool, error) {
		cur := &infrav1beta1.VirtualMachine{}
		err := lg.client.Get(ctx, client.ObjectKey{Namespace: lg.namespace, Name: vm.name}, cur)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return err == nil && cur.UID != vm.uid, nil
	})
	lg.results <- result
}

func (lg *LoadGenerator) performDescribe(ctx context.Context, provider string, _ int) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	startTime := time.Now()

	result := Result{
		Operation: "describe",
		Provider:  provider,
		StartTime: startTime,
		Success:   true,
		Phase:     "Describing",
	}

	// List VMs as a describe operation; a dry run makes no API call.
	if lg.opts.dryRun {
		result.Phase = phaseDryRun
	} else {
		vmList := &infrav1beta1.VirtualMachineList{}
		err := lg.client.List(ctx, vmList, client.InNamespace(lg.namespace))
		result.Success = err == nil
		if err != nil {
			result.Error = err.Error()
		}
	}
	result.Duration = time.Since(startTime)

	lg.results <- result
}

func (lg *LoadGenerator) selectRandomOperation() string {
	// Weighted random selection based on operation mix
	operations := []string{"create", "delete", "power", "reconfigure", "snapshot", "clone", "describe"}
	weights := []float64{
		lg.config.Operations.Create,
		lg.config.Operations.Delete,
		lg.config.Operations.Power,
		lg.config.Operations.Reconfigure,
		lg.config.Operations.Snapshot,
		lg.config.Operations.Clone,
		lg.config.Operations.Describe,
	}

	// Simple weighted random selection
	totalWeight := 0.0
	for _, w := range weights {
		totalWeight += w
	}

	r := rand.Float64() * totalWeight // #nosec G404 -- picks the next simulated operation, not a secret
	cumulative := 0.0

	for i, weight := range weights {
		cumulative += weight
		if r <= cumulative {
			return operations[i]
		}
	}

	return "describe" // fallback
}

func (lg *LoadGenerator) selectRandomProvider() string {
	if len(lg.config.Providers) == 0 {
		return "default-provider"
	}
	return lg.config.Providers[rand.Intn(len(lg.config.Providers))] // #nosec G404 -- spreads load across providers, not a secret
}

func (lg *LoadGenerator) generateVMName(workerID int) string {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.vmCounter++
	return fmt.Sprintf("loadgen-vm-%s-%d-%d", lg.runID, workerID, lg.vmCounter)
}

// vmLabels returns the template labels plus the labels that mark an object
// as created by this tool and by this run.
func (lg *LoadGenerator) vmLabels() map[string]string {
	labels := make(map[string]string, len(lg.config.VMTemplate.Labels)+2)
	for k, v := range lg.config.VMTemplate.Labels {
		labels[k] = v
	}
	labels[loadgenLabelKey] = loadgenLabelValue
	labels[loadgenRunLabelKey] = lg.runID
	return labels
}

// recordCreated remembers a VM this run created.
func (lg *LoadGenerator) recordCreated(vm *runVM) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.vms = append(lg.vms, vm)
}

// takeCreated removes and returns a random VM this run created that no other
// operation is using. It reports false when there is none.
func (lg *LoadGenerator) takeCreated() (*runVM, bool) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	idle := lg.idleVMs()
	if len(idle) == 0 {
		return nil, false
	}
	i := idle[rand.Intn(len(idle))] // #nosec G404 -- picks which created VM to delete next, not a secret
	vm := lg.vms[i]
	lg.vms = append(lg.vms[:i], lg.vms[i+1:]...)
	return vm, true
}

// operationContext returns a context for one API call that outlives the run's
// duration by up to operationTimeout, so an operation in flight when the run
// ends is not reported as failed.
func operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
}

func (lg *LoadGenerator) collectResults(out io.Writer) {
	stats := &Statistics{
		OperationsByType:   make(map[string]int),
		OperationsByResult: make(map[string]int),
		DurationsByOp:      make(map[string][]time.Duration),
		ObservedByOp:       make(map[string][]time.Duration),
		ObserveFailures:    make(map[string]int),
		StartTime:          time.Now(),
	}

	var results []Result

	for result := range lg.results {
		results = append(results, result)
		lg.updateStatistics(stats, result)

		if lg.opts.verbose {
			status := "✅"
			if !result.Success {
				status = "❌"
			}
			cliout.Printf(out, "%s %s/%s: %v\n", status, result.Operation, result.Provider, result.Duration)
		}
	}

	stats.EndTime = time.Now()

	// Save results
	lg.saveResults(out, results, stats)
}

func (lg *LoadGenerator) updateStatistics(stats *Statistics, result Result) {
	stats.TotalOperations++

	if result.Success {
		stats.SuccessfulOps++
		stats.OperationsByResult["success"]++
	} else {
		stats.FailedOps++
		stats.OperationsByResult["failed"]++
	}

	stats.OperationsByType[result.Operation]++

	if stats.DurationsByOp[result.Operation] == nil {
		stats.DurationsByOp[result.Operation] = []time.Duration{}
	}
	stats.DurationsByOp[result.Operation] = append(stats.DurationsByOp[result.Operation], result.Duration)

	if result.Observed > 0 {
		stats.ObservedByOp[result.Operation] = append(stats.ObservedByOp[result.Operation], result.Observed)
	}
	if result.ObserveError != "" {
		stats.ObserveFailures[result.Operation]++
	}
}

func (lg *LoadGenerator) saveResults(out io.Writer, results []Result, stats *Statistics) {
	// Save CSV results
	csvFile := filepath.Join(lg.opts.outputDir, "results.csv")
	lg.saveCSV(csvFile, results)

	// Save summary
	summaryFile := filepath.Join(lg.opts.outputDir, "summary.md")
	lg.saveSummary(summaryFile, stats)

	cliout.Printf(out, "Results saved:\n")
	cliout.Printf(out, "  CSV: %s\n", csvFile)
	cliout.Printf(out, "  Summary: %s\n", summaryFile)
}

func (lg *LoadGenerator) saveCSV(filename string, results []Result) {
	file, err := os.Create(filename) // #nosec G304 -- the file lives in the --output-dir named by the operator
	if err != nil {
		log.Printf("Failed to create CSV file: %v", err)
		return
	}
	defer closer.CloseQuietlyWithoutLogger(file)

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Write header
	if err := writer.Write([]string{"Operation", "Provider", "VMName", "StartTime", "Duration", "Success", "Error", "Phase",
		"ObservedDuration", "ObserveError"}); err != nil {
		log.Printf("Failed to write CSV header: %v", err)
		return
	}

	// Write results
	for _, result := range results {
		if err := writer.Write([]string{
			result.Operation,
			result.Provider,
			result.VMName,
			result.StartTime.Format(time.RFC3339),
			result.Duration.String(),
			strconv.FormatBool(result.Success),
			result.Error,
			result.Phase,
			observedString(result.Observed),
			result.ObserveError,
		}); err != nil {
			log.Printf("Failed to write CSV row: %v", err)
			return
		}
	}
}

func (lg *LoadGenerator) saveSummary(filename string, stats *Statistics) {
	file, err := os.Create(filename) // #nosec G304 -- the file lives in the --output-dir named by the operator
	if err != nil {
		log.Printf("Failed to create summary file: %v", err)
		return
	}
	defer closer.CloseQuietlyWithoutLogger(file)

	// Write summary with error handling
	writeOrLog := func(format string, args ...interface{}) {
		if _, err := fmt.Fprintf(file, format, args...); err != nil {
			log.Printf("Failed to write to summary file: %v", err)
		}
	}

	writeOrLog("# Load Generation Summary\n\n")
	writeOrLog("## Overview\n\n")
	writeOrLog("- **Duration**: %v\n", stats.EndTime.Sub(stats.StartTime))
	writeOrLog("- **Total Operations**: %d\n", stats.TotalOperations)
	writeOrLog("- **Successful**: %d (%.1f%%)\n", stats.SuccessfulOps, percent(stats.SuccessfulOps, stats.TotalOperations))
	writeOrLog("- **Failed**: %d (%.1f%%)\n", stats.FailedOps, percent(stats.FailedOps, stats.TotalOperations))

	writeOrLog("\n## Operations by Type\n\n")
	writeOrLog("| Operation | Count | Percentage |\n")
	writeOrLog("|-----------|-------|------------|\n")
	for op, count := range stats.OperationsByType {
		writeOrLog("| %s | %d | %.1f%% |\n", op, count, percent(count, stats.TotalOperations))
	}

	writeOrLog("\n## Performance Metrics\n\n")
	writeOrLog("API call latency: from sending the request until the apiserver answered (accepted or refused).\n\n")
	writePercentiles(writeOrLog, stats.DurationsByOp)

	if lg.opts.observe {
		writeOrLog("\n## Time Until Observed (--observe)\n\n")
		writeOrLog("From sending the request until the object's status reflected it. Not observed within --observe-timeout:")
		if len(stats.ObserveFailures) == 0 {
			writeOrLog(" none.\n\n")
		} else {
			writeOrLog("\n\n")
			for op, n := range stats.ObserveFailures {
				writeOrLog("- %s: %d\n", op, n)
			}
			writeOrLog("\n")
		}
		writePercentiles(writeOrLog, stats.ObservedByOp)
	}
}

// writePercentiles writes a table of p50/p95/p99/max per operation.
func writePercentiles(writeOrLog func(string, ...interface{}), byOp map[string][]time.Duration) {
	writeOrLog("| Operation | Count | P50 | P95 | P99 | Max |\n")
	writeOrLog("|-----------|-------|-----|-----|-----|-----|\n")
	ops := make([]string, 0, len(byOp))
	for op := range byOp {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		durations := byOp[op]
		if len(durations) == 0 {
			continue
		}

		sort.Slice(durations, func(i, j int) bool {
			return durations[i] < durations[j]
		})

		p50 := durations[len(durations)*50/100]
		p95 := durations[len(durations)*95/100]
		p99 := durations[len(durations)*99/100]
		max := durations[len(durations)-1]

		writeOrLog("| %s | %d | %v | %v | %v | %v |\n",
			op, len(durations), p50, p95, p99, max)
	}
}

// observedString renders an --observe duration for the CSV ("" when not
// observed).
func observedString(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

// percent returns part as a percentage of total, or 0 when total is 0.
func percent(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func getDefaultConfig() LoadGenConfig {
	return LoadGenConfig{
		Duration:    5 * time.Minute,
		Concurrency: 2,
		VMCount:     10,
		Providers:   []string{"test-provider"},
		VMTemplate: VMTemplate{
			ClassRef: "test-class",
			ImageRef: "test-image",
			Labels: map[string]string{
				loadgenLabelKey: loadgenLabelValue,
			},
		},
		Operations: OperationMix{
			Create:      20.0,
			Delete:      10.0,
			Power:       15.0,
			Reconfigure: 10.0,
			Snapshot:    5.0,
			Clone:       5.0,
			Describe:    35.0,
		},
	}
}
