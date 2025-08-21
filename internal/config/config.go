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

package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v2"
)

// Config holds all configuration for virtrigaud components
type Config struct {
	// Logging configuration
	Log LogConfig `yaml:"log"`

	// Tracing configuration
	Tracing TracingConfig `yaml:"tracing"`

	// RPC configuration
	RPC RPCConfig `yaml:"rpc"`

	// Retry configuration
	Retry RetryConfig `yaml:"retry"`

	// Circuit breaker configuration
	CircuitBreaker CircuitBreakerConfig `yaml:"circuitBreaker"`

	// Rate limiting configuration
	RateLimit RateLimitConfig `yaml:"rateLimit"`

	// Worker configuration
	Workers WorkerConfig `yaml:"workers"`

	// Feature gates
	FeatureGates []string `yaml:"featureGates"`

	// Performance and profiling
	Performance PerformanceConfig `yaml:"performance"`
}

// LogConfig holds logging configuration
type LogConfig struct {
	Level       string `yaml:"level"`
	Format      string `yaml:"format"`
	Sampling    bool   `yaml:"sampling"`
	Development bool   `yaml:"development"`
}

// TracingConfig holds tracing configuration
type TracingConfig struct {
	Enabled           bool    `yaml:"enabled"`
	Endpoint          string  `yaml:"endpoint"`
	SamplingRatio     float64 `yaml:"samplingRatio"`
	InsecureTransport bool    `yaml:"insecureTransport"`
}

// RPCConfig holds RPC timeout configuration
type RPCConfig struct {
	TimeoutDescribe   time.Duration `yaml:"timeoutDescribe"`
	TimeoutMutating   time.Duration `yaml:"timeoutMutating"`
	TimeoutValidate   time.Duration `yaml:"timeoutValidate"`
	TimeoutTaskStatus time.Duration `yaml:"timeoutTaskStatus"`
}

// RetryConfig holds retry configuration
type RetryConfig struct {
	MaxAttempts int           `yaml:"maxAttempts"`
	BaseDelay   time.Duration `yaml:"baseDelay"`
	MaxDelay    time.Duration `yaml:"maxDelay"`
	Multiplier  float64       `yaml:"multiplier"`
	Jitter      bool          `yaml:"jitter"`
}

// CircuitBreakerConfig holds circuit breaker configuration
type CircuitBreakerConfig struct {
	FailureThreshold int           `yaml:"failureThreshold"`
	ResetTimeout     time.Duration `yaml:"resetTimeout"`
	HalfOpenMaxCalls int           `yaml:"halfOpenMaxCalls"`
}

// RateLimitConfig holds rate limiting configuration
type RateLimitConfig struct {
	QPS   int `yaml:"qps"`
	Burst int `yaml:"burst"`
}

// WorkerConfig holds worker configuration
type WorkerConfig struct {
	PerKind          int `yaml:"perKind"`
	MaxInflightTasks int `yaml:"maxInflightTasks"`
}

// PerformanceConfig holds performance and profiling configuration
type PerformanceConfig struct {
	PProfEnabled bool   `yaml:"pprofEnabled"`
	PProfAddr    string `yaml:"pprofAddr"`
}

// DefaultConfig returns a default configuration
func DefaultConfig() *Config {
	return &Config{
		Log: LogConfig{
			Level:       getEnvWithDefault("LOG_LEVEL", "info"),
			Format:      getEnvWithDefault("LOG_FORMAT", "json"),
			Sampling:    getEnvBoolWithDefault("LOG_SAMPLING", true),
			Development: getEnvBoolWithDefault("LOG_DEVELOPMENT", false),
		},
		Tracing: TracingConfig{
			Enabled:           getEnvBoolWithDefault("VIRTRIGAUD_TRACING_ENABLED", false),
			Endpoint:          getEnvWithDefault("VIRTRIGAUD_TRACING_ENDPOINT", ""),
			SamplingRatio:     getEnvFloatWithDefault("VIRTRIGAUD_TRACING_SAMPLING_RATIO", 0.1),
			InsecureTransport: getEnvBoolWithDefault("VIRTRIGAUD_TRACING_INSECURE", true),
		},
		RPC: RPCConfig{
			TimeoutDescribe:   getEnvDurationWithDefault("RPC_TIMEOUT_DESCRIBE", 30*time.Second),
			TimeoutMutating:   getEnvDurationWithDefault("RPC_TIMEOUT_MUTATING", 4*time.Minute),
			TimeoutValidate:   getEnvDurationWithDefault("RPC_TIMEOUT_VALIDATE", 10*time.Second),
			TimeoutTaskStatus: getEnvDurationWithDefault("RPC_TIMEOUT_TASK_STATUS", 10*time.Second),
		},
		Retry: RetryConfig{
			MaxAttempts: getEnvIntWithDefault("RETRY_MAX_ATTEMPTS", 5),
			BaseDelay:   getEnvDurationWithDefault("RETRY_BASE_DELAY", 500*time.Millisecond),
			MaxDelay:    getEnvDurationWithDefault("RETRY_MAX_DELAY", 30*time.Second),
			Multiplier:  getEnvFloatWithDefault("RETRY_MULTIPLIER", 2.0),
			Jitter:      getEnvBoolWithDefault("RETRY_JITTER", true),
		},
		CircuitBreaker: CircuitBreakerConfig{
			FailureThreshold: getEnvIntWithDefault("CB_FAILURE_THRESHOLD", 10),
			ResetTimeout:     getEnvDurationWithDefault("CB_RESET_SECONDS", 60*time.Second),
			HalfOpenMaxCalls: getEnvIntWithDefault("CB_HALF_OPEN_MAX_CALLS", 3),
		},
		RateLimit: RateLimitConfig{
			QPS:   getEnvIntWithDefault("RATE_LIMIT_QPS", 10),
			Burst: getEnvIntWithDefault("RATE_LIMIT_BURST", 20),
		},
		Workers: WorkerConfig{
			PerKind:          getEnvIntWithDefault("WORKERS_PER_KIND", 2),
			MaxInflightTasks: getEnvIntWithDefault("MAX_INFLIGHT_TASKS", 100),
		},
		FeatureGates: getEnvSliceWithDefault("FEATURE_GATES", []string{}),
		Performance: PerformanceConfig{
			PProfEnabled: getEnvBoolWithDefault("VIRTRIGAUD_PPROF_ENABLED", false),
			PProfAddr:    getEnvWithDefault("VIRTRIGAUD_PPROF_ADDR", ":6060"),
		},
	}
}

// Manager manages configuration with hot-reload capability
type Manager struct {
	mu       sync.RWMutex
	config   *Config
	watchers []chan *Config
	watcher  *fsnotify.Watcher
	file     string
}

// NewManager creates a new configuration manager
func NewManager(configFile string) (*Manager, error) {
	config := DefaultConfig()

	// Load from file if provided
	if configFile != "" {
		if err := loadFromFile(configFile, config); err != nil {
			return nil, fmt.Errorf("failed to load config from file: %w", err)
		}
	}

	manager := &Manager{
		config:   config,
		watchers: make([]chan *Config, 0),
		file:     configFile,
	}

	// Set up file watcher if config file is provided
	if configFile != "" {
		if err := manager.setupFileWatcher(); err != nil {
			// Log but don't fail - configuration is still usable
			fmt.Printf("Warning: failed to setup config file watcher: %v\n", err)
		}
	}

	return manager, nil
}

// Get returns the current configuration
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// Watch returns a channel that receives configuration updates
func (m *Manager) Watch() <-chan *Config {
	m.mu.Lock()
	defer m.mu.Unlock()

	ch := make(chan *Config, 1)
	m.watchers = append(m.watchers, ch)

	// Send current config immediately
	ch <- m.config

	return ch
}

// Update updates the configuration and notifies watchers
func (m *Manager) Update(config *Config) {
	m.mu.Lock()
	m.config = config
	watchers := make([]chan *Config, len(m.watchers))
	copy(watchers, m.watchers)
	m.mu.Unlock()

	// Notify all watchers
	for _, watcher := range watchers {
		select {
		case watcher <- config:
		default:
			// Channel is full, skip this update
		}
	}
}

// Close closes the configuration manager and cleans up resources
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Close all watcher channels
	for _, watcher := range m.watchers {
		close(watcher)
	}
	m.watchers = nil

	// Close file watcher
	if m.watcher != nil {
		return m.watcher.Close()
	}

	return nil
}

// setupFileWatcher sets up file system notification for config changes
func (m *Manager) setupFileWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	m.watcher = watcher

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					m.reloadConfig()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Printf("Config file watcher error: %v\n", err)
			}
		}
	}()

	return watcher.Add(m.file)
}

// reloadConfig reloads configuration from file
func (m *Manager) reloadConfig() {
	config := DefaultConfig()
	if err := loadFromFile(m.file, config); err != nil {
		fmt.Printf("Error reloading config: %v\n", err)
		return
	}

	fmt.Println("Configuration reloaded from file")
	m.Update(config)
}

// loadFromFile loads configuration from a YAML file
func loadFromFile(filename string, config *Config) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	return yaml.Unmarshal(data, config)
}

// IsFeatureEnabled checks if a feature gate is enabled
func (c *Config) IsFeatureEnabled(feature string) bool {
	for _, gate := range c.FeatureGates {
		if gate == feature {
			return true
		}
	}
	return false
}

// GetRPCTimeout returns the appropriate timeout for an RPC operation
func (c *Config) GetRPCTimeout(operation string) time.Duration {
	switch operation {
	case "Describe":
		return c.RPC.TimeoutDescribe
	case "Validate":
		return c.RPC.TimeoutValidate
	case "TaskStatus":
		return c.RPC.TimeoutTaskStatus
	case "Create", "Delete", "Power", "Reconfigure":
		return c.RPC.TimeoutMutating
	default:
		return c.RPC.TimeoutMutating
	}
}

// Singleton configuration manager
var (
	globalManager *Manager
	globalOnce    sync.Once
)

// InitGlobal initializes the global configuration manager
func InitGlobal(configFile string) error {
	var err error
	globalOnce.Do(func() {
		globalManager, err = NewManager(configFile)
	})
	return err
}

// Global returns the global configuration
func Global() *Config {
	if globalManager == nil {
		// Return default config if not initialized
		return DefaultConfig()
	}
	return globalManager.Get()
}

// WatchGlobal returns a channel for global configuration updates
func WatchGlobal() <-chan *Config {
	if globalManager == nil {
		// Return a channel with default config
		ch := make(chan *Config, 1)
		ch <- DefaultConfig()
		return ch
	}
	return globalManager.Watch()
}

// Helper functions for environment variable parsing

func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBoolWithDefault(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvIntWithDefault(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvFloatWithDefault(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvDurationWithDefault(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvSliceWithDefault(key string, defaultValue []string) []string {
	if value := os.Getenv(key); value != "" {
		return strings.Split(value, ",")
	}
	return defaultValue
}
