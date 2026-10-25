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
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/cmd/internal/cliclient"
	"github.com/projectbeskar/virtrigaud/internal/util/closer"
)

const (
	// loadgenLabelKey and loadgenLabelValue mark every VM the tool creates.
	loadgenLabelKey   = "generated-by"
	loadgenLabelValue = "loadgen"

	// operationTimeout bounds one create, delete or list call, so an
	// operation that is in flight when the run's duration ends can finish.
	operationTimeout = 30 * time.Second
)

// options holds the values of the tool's flags.
type options struct {
	kubeconfig string
	namespace  string
	outputDir  string
	configFile string
	dryRun     bool
	verbose    bool
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
	Duration  time.Duration
	Success   bool
	Error     string
	Phase     string // VM phase if applicable
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

	mu        sync.Mutex
	vmCounter int
	// created holds the names of the VMs this run created and has not
	// deleted. Only these are ever deleted, never other VMs in the namespace.
	created []string
}

// Statistics holds performance statistics
type Statistics struct {
	TotalOperations    int
	SuccessfulOps      int
	FailedOps          int
	OperationsByType   map[string]int
	OperationsByResult map[string]int
	DurationsByOp      map[string][]time.Duration
	StartTime          time.Time
	EndTime            time.Time
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
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
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	rootCmd.PersistentFlags().StringVar(&opts.kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	rootCmd.PersistentFlags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	rootCmd.PersistentFlags().StringVarP(&opts.outputDir, "output-dir", "o", "./loadgen-results", "Output directory for results")
	rootCmd.PersistentFlags().BoolVar(&opts.dryRun, "dry-run", false, "Dry run mode (don't create resources)")
	rootCmd.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false, "Verbose output")

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run load generation",
		Long: `Run load generation against virtrigaud.

Without --config the built-in defaults are used (5 minutes, 2 workers, VMs
created against provider "test-provider"). --config reads a YAML file that
overrides them; unknown keys are an error.`,
		Args: cobra.NoArgs,
		RunE: opts.runLoadGen,
	}

	runCmd.Flags().StringVarP(&opts.configFile, "config", "c", "", "Load generation config file (YAML)")

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
	data, err := os.ReadFile(path)
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

func (o *options) runLoadGen(cmd *cobra.Command, _ []string) error {
	// Load configuration
	lgConfig, err := loadConfig(o.configFile)
	if err != nil {
		return err
	}

	// Create Kubernetes client
	cfg, err := cliclient.RESTConfig(o.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	k8sClient, err := cliclient.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Create output directory
	if err := os.MkdirAll(o.outputDir, 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Create load generator
	generator := &LoadGenerator{
		client:    k8sClient,
		namespace: o.namespace,
		config:    lgConfig,
		results:   make(chan Result, 1000),
		opts:      o,
		runID:     strconv.FormatInt(time.Now().UnixNano(), 36),
	}

	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, lgConfig.Duration)
	defer cancel()

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Starting load generation...\n")
	fmt.Fprintf(out, "Duration: %v\n", lgConfig.Duration)
	fmt.Fprintf(out, "Concurrency: %d\n", lgConfig.Concurrency)
	fmt.Fprintf(out, "VM Count: %d\n", lgConfig.VMCount)
	fmt.Fprintf(out, "Providers: %v\n", lgConfig.Providers)

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

	fmt.Fprintf(out, "Load generation completed successfully\n")
	fmt.Fprintf(out, "Results saved to: %s\n", o.outputDir)

	return nil
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

	startTime := time.Now()
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
			PowerState:  "On",
		},
	}

	var err error
	var success bool
	var phase string

	if !lg.opts.dryRun {
		err = lg.client.Create(ctx, vm)
		success = err == nil
		if success {
			phase = "Creating"
		}
	} else {
		success = true
		phase = "DryRun"
	}
	if success {
		lg.recordCreated(vmName)
	}

	result := Result{
		Operation: "create",
		Provider:  provider,
		VMName:    vmName,
		StartTime: startTime,
		Duration:  time.Since(startTime),
		Success:   success,
		Phase:     phase,
	}

	if err != nil {
		result.Error = err.Error()
	}

	lg.results <- result
}

func (lg *LoadGenerator) performDelete(ctx context.Context, provider string, _ int) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	startTime := time.Now()

	result := Result{
		Operation: "delete",
		Provider:  provider,
		StartTime: startTime,
		Success:   false,
	}

	// Delete only a VM this run created, never another VM in the namespace.
	vmName, ok := lg.takeCreated()
	if !ok {
		result.Duration = time.Since(startTime)
		result.Error = "No VMs created by this run to delete"
		lg.results <- result
		return
	}
	result.VMName = vmName

	if !lg.opts.dryRun {
		vm := &infrav1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: lg.namespace},
		}
		err := lg.client.Delete(ctx, vm)
		result.Success = err == nil
		if err != nil {
			result.Error = err.Error()
		}
	} else {
		result.Success = true
		result.Phase = "DryRun"
	}

	result.Duration = time.Since(startTime)
	lg.results <- result
}

func (lg *LoadGenerator) performPower(ctx context.Context, provider string, workerID int) {
	// Implementation similar to delete but updates power state
	result := Result{
		Operation: "power",
		Provider:  provider,
		StartTime: time.Now(),
		Success:   true, // Simplified for demo
		Phase:     "PowerToggle",
	}
	result.Duration = time.Since(result.StartTime)
	lg.results <- result
}

func (lg *LoadGenerator) performReconfigure(ctx context.Context, provider string, workerID int) {
	result := Result{
		Operation: "reconfigure",
		Provider:  provider,
		StartTime: time.Now(),
		Success:   true, // Simplified for demo
		Phase:     "Reconfiguring",
	}
	result.Duration = time.Since(result.StartTime)
	lg.results <- result
}

func (lg *LoadGenerator) performSnapshot(ctx context.Context, provider string, workerID int) {
	result := Result{
		Operation: "snapshot",
		Provider:  provider,
		StartTime: time.Now(),
		Success:   true, // Simplified for demo
		Phase:     "Snapshotting",
	}
	result.Duration = time.Since(result.StartTime)
	lg.results <- result
}

func (lg *LoadGenerator) performClone(ctx context.Context, provider string, workerID int) {
	result := Result{
		Operation: "clone",
		Provider:  provider,
		StartTime: time.Now(),
		Success:   true, // Simplified for demo
		Phase:     "Cloning",
	}
	result.Duration = time.Since(result.StartTime)
	lg.results <- result
}

func (lg *LoadGenerator) performDescribe(ctx context.Context, provider string, _ int) {
	ctx, cancel := operationContext(ctx)
	defer cancel()

	startTime := time.Now()

	// List VMs as a describe operation
	vmList := &infrav1beta1.VirtualMachineList{}
	err := lg.client.List(ctx, vmList, client.InNamespace(lg.namespace))

	result := Result{
		Operation: "describe",
		Provider:  provider,
		StartTime: startTime,
		Duration:  time.Since(startTime),
		Success:   err == nil,
		Phase:     "Describing",
	}

	if err != nil {
		result.Error = err.Error()
	}

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

	r := rand.Float64() * totalWeight
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
	return lg.config.Providers[rand.Intn(len(lg.config.Providers))]
}

func (lg *LoadGenerator) generateVMName(workerID int) string {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.vmCounter++
	return fmt.Sprintf("loadgen-vm-%s-%d-%d", lg.runID, workerID, lg.vmCounter)
}

// vmLabels returns the template labels plus the label that marks a VM as
// created by this tool.
func (lg *LoadGenerator) vmLabels() map[string]string {
	labels := make(map[string]string, len(lg.config.VMTemplate.Labels)+1)
	for k, v := range lg.config.VMTemplate.Labels {
		labels[k] = v
	}
	labels[loadgenLabelKey] = loadgenLabelValue
	return labels
}

// recordCreated remembers a VM this run created.
func (lg *LoadGenerator) recordCreated(name string) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.created = append(lg.created, name)
}

// takeCreated removes and returns a random VM this run created. It reports
// false when there is none.
func (lg *LoadGenerator) takeCreated() (string, bool) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if len(lg.created) == 0 {
		return "", false
	}
	i := rand.Intn(len(lg.created))
	name := lg.created[i]
	lg.created = append(lg.created[:i], lg.created[i+1:]...)
	return name, true
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
			fmt.Fprintf(out, "%s %s/%s: %v\n", status, result.Operation, result.Provider, result.Duration)
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
}

func (lg *LoadGenerator) saveResults(out io.Writer, results []Result, stats *Statistics) {
	// Save CSV results
	csvFile := filepath.Join(lg.opts.outputDir, "results.csv")
	lg.saveCSV(csvFile, results)

	// Save summary
	summaryFile := filepath.Join(lg.opts.outputDir, "summary.md")
	lg.saveSummary(summaryFile, stats)

	fmt.Fprintf(out, "Results saved:\n")
	fmt.Fprintf(out, "  CSV: %s\n", csvFile)
	fmt.Fprintf(out, "  Summary: %s\n", summaryFile)
}

func (lg *LoadGenerator) saveCSV(filename string, results []Result) {
	file, err := os.Create(filename)
	if err != nil {
		log.Printf("Failed to create CSV file: %v", err)
		return
	}
	defer closer.CloseQuietlyWithoutLogger(file)

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Write header
	if err := writer.Write([]string{"Operation", "Provider", "VMName", "StartTime", "Duration", "Success", "Error", "Phase"}); err != nil {
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
		}); err != nil {
			log.Printf("Failed to write CSV row: %v", err)
			return
		}
	}
}

func (lg *LoadGenerator) saveSummary(filename string, stats *Statistics) {
	file, err := os.Create(filename)
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
	writeOrLog("| Operation | Count | P50 | P95 | P99 | Max |\n")
	writeOrLog("|-----------|-------|-----|-----|-----|-----|\n")
	for op, durations := range stats.DurationsByOp {
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
