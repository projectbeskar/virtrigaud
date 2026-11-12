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

// Package config is the per-host agent's configuration: its flags, their
// defaults and validation, the executor it runs with and the development
// enrolment stand-in.
//
// A release build (no virtrigaud_dev build tag) has the no-op executor only
// and reads its trust configuration from the fixed signed-image location; it
// refuses to start when a development-only flag is set, rather than run
// without the refusal a release build promises. A development build can
// choose the fake executor, another trust directory and a development
// enrolment file.
//
// It imports no Kubernetes package.
package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
)

// Defaults.
const (
	// DefaultStateDir is the agent's state directory.
	DefaultStateDir = "/var/lib/virtrigaud-agent"
	// DefaultListenAddress is where the agent serves the Provider.
	DefaultListenAddress = ":9460"
	// DefaultDrainTimeout bounds how long a stop waits for running calls.
	DefaultDrainTimeout = 30 * time.Second
	// PKIDirName is the key material directory inside the state directory.
	PKIDirName = "pki"
	// FakeExecutorDirName is the fake executor's directory inside the state
	// directory (development builds).
	FakeExecutorDirName = "fake-executor"
)

// Flag names.
const (
	// FlagStateDir names the state directory flag.
	FlagStateDir = "state-dir"
	// FlagListenAddress names the listen address flag.
	FlagListenAddress = "listen-address"
	// FlagDrainTimeout names the drain timeout flag.
	FlagDrainTimeout = "drain-timeout"
	// FlagExecutor names the executor flag.
	FlagExecutor = "executor"
	// FlagDevTrustDir names the development-only trust directory flag.
	FlagDevTrustDir = "dev-trust-dir"
	// FlagDevEnrolmentFile names the development-only enrolment file flag.
	FlagDevEnrolmentFile = "dev-enrolment-file"
)

// ExecutorNone is the no-op executor's name.
const ExecutorNone = "none"

// ExecutorFake is the development-only fake executor's name.
const ExecutorFake = "fake"

// ErrDevOnly is returned when a release build is asked for a
// development-only setting.
var ErrDevOnly = errors.New("this setting needs a development build of the agent (virtrigaud_dev build tag)")

// Config is the agent's configuration.
type Config struct {
	// StateDir is the state directory.
	StateDir string
	// ListenAddress is the gRPC listen address.
	ListenAddress string
	// DrainTimeout bounds a graceful stop.
	DrainTimeout time.Duration
	// Executor names the executor: ExecutorNone, or ExecutorFake in a
	// development build.
	Executor string
	// DevTrustDir replaces the trust configuration directory (development
	// builds only; empty uses the signed-image location).
	DevTrustDir string
	// DevEnrolmentFile names the development enrolment file (development
	// builds only).
	DevEnrolmentFile string
}

// Register registers the agent's flags on fs with their defaults and returns
// the Config they fill.
func Register(fs *flag.FlagSet) *Config {
	c := &Config{}
	fs.StringVar(&c.StateDir, FlagStateDir, DefaultStateDir, "the agent's state directory (persistent, owned by the agent's user, mode 0700)")
	fs.StringVar(&c.ListenAddress, FlagListenAddress, DefaultListenAddress, "the address the agent serves the Provider on")
	fs.DurationVar(&c.DrainTimeout, FlagDrainTimeout, DefaultDrainTimeout, "how long a stop waits for running calls")
	fs.StringVar(&c.Executor, FlagExecutor, ExecutorNone, "the executor: none (every build) or fake (development builds only)")
	fs.StringVar(&c.DevTrustDir, FlagDevTrustDir, "", "development builds only: read the trust configuration from this directory")
	fs.StringVar(&c.DevEnrolmentFile, FlagDevEnrolmentFile, "", "development builds only: enrol a fresh store from this file and the host certificate")
	return c
}

// Validate checks the configuration and refuses a development-only setting in
// a release build.
func (c *Config) Validate() error {
	if c.StateDir == "" || !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) != c.StateDir {
		return fmt.Errorf("--%s must be a clean absolute path", FlagStateDir)
	}
	if c.ListenAddress == "" {
		return fmt.Errorf("--%s is empty", FlagListenAddress)
	}
	if c.DrainTimeout <= 0 {
		return fmt.Errorf("--%s must be positive", FlagDrainTimeout)
	}
	if _, err := c.TrustDir(); err != nil {
		return err
	}
	switch c.Executor {
	case ExecutorNone:
	case ExecutorFake:
		if !DevBuild {
			return fmt.Errorf("--%s=%s: %w", FlagExecutor, ExecutorFake, ErrDevOnly)
		}
	default:
		return fmt.Errorf("--%s=%q is not an executor", FlagExecutor, c.Executor)
	}
	if c.DevEnrolmentFile != "" && !DevBuild {
		return fmt.Errorf("--%s: %w", FlagDevEnrolmentFile, ErrDevOnly)
	}
	return nil
}

// PKIDir is the key material directory.
func (c *Config) PKIDir() string { return filepath.Join(c.StateDir, PKIDirName) }

// TrustDir is the trust configuration directory.
func (c *Config) TrustDir() (string, error) { return trustDir(c.DevTrustDir) }

// Executor is what the agent runs operations with and stops VMs with.
type Executor interface {
	executor.Executor
	executor.Stopper
}

// NewExecutor returns the configured executor.
func (c *Config) NewExecutor() (Executor, error) {
	return newExecutor(c.Executor, filepath.Join(c.StateDir, FakeExecutorDirName))
}

// DevEnrolment is the development enrolment stand-in: what a real enrolment
// will deliver beside the host certificate's enrolment record. A fresh store
// is enrolled with the certificate's record and these values.
type DevEnrolment struct {
	// StartEpoch is the epoch counter the enrolment starts at: the counter
	// of the manager's development enrolment file. At least 1.
	StartEpoch uint64
	// Predecessors are earlier enrolment ids of this host, never activated
	// in this store.
	Predecessors [][]byte
}

// devEnrolmentFile is the JSON form of a development enrolment file:
//
//	{"startEpoch": 2, "predecessors": ["<32 hex>"]}
type devEnrolmentFile struct {
	StartEpoch   uint64   `json:"startEpoch"`
	Predecessors []string `json:"predecessors"`
}

// ParseDevEnrolment parses a development enrolment file. Unknown fields and
// trailing data are refused; startEpoch must be at least 1 and every
// predecessor 32 hexadecimal characters.
func ParseDevEnrolment(data []byte) (*DevEnrolment, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f devEnrolmentFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse development enrolment file: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse development enrolment file: trailing data")
	}
	if f.StartEpoch == 0 {
		return nil, errors.New("parse development enrolment file: startEpoch must be at least 1")
	}
	out := &DevEnrolment{StartEpoch: f.StartEpoch}
	for i, p := range f.Predecessors {
		b, err := hex.DecodeString(p)
		if err != nil || len(b) != 16 {
			return nil, fmt.Errorf("parse development enrolment file: predecessors[%d] is not 32 hexadecimal characters", i)
		}
		out.Predecessors = append(out.Predecessors, b)
	}
	return out, nil
}

// LoadDevEnrolment reads the development enrolment file; nil when none is
// configured. A release build refuses a configured file.
func (c *Config) LoadDevEnrolment() (*DevEnrolment, error) {
	if c.DevEnrolmentFile == "" {
		return nil, nil
	}
	return loadDevEnrolment(c.DevEnrolmentFile)
}
