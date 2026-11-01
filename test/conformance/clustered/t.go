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

package clustered

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/stretchr/testify/require"
)

// T is the part of *testing.T the suite and the Targets use. *testing.T
// satisfies it; Recorder satisfies it too, so the suite can be run against a
// deliberately broken Provider without failing the surrounding test (the
// mutation meta-test).
type T interface {
	require.TestingT
	// Helper marks the caller as a test helper.
	Helper()
	// Logf logs a formatted line.
	Logf(format string, args ...any)
	// Skipf marks the case skipped and stops it.
	Skipf(format string, args ...any)
	// Cleanup registers fn to run, last in first out, when the case ends.
	Cleanup(fn func())
	// TempDir returns a directory removed when the case ends.
	TempDir() string
	// Setenv sets an environment variable for the rest of the case.
	Setenv(key, value string)
	// Name returns the case's name.
	Name() string
}

// Outcome is how a recorded case ended.
type Outcome int

const (
	// OutcomePassed means the case ran to the end without a failed assertion.
	OutcomePassed Outcome = iota
	// OutcomeFailed means at least one assertion failed.
	OutcomeFailed
	// OutcomeSkipped means the case skipped itself.
	OutcomeSkipped
)

// String returns the outcome's name.
func (o Outcome) String() string {
	switch o {
	case OutcomePassed:
		return "passed"
	case OutcomeFailed:
		return "failed"
	case OutcomeSkipped:
		return "skipped"
	}
	return "unknown"
}

// Recorder is a T that records what a case did instead of failing a test. It
// runs the case on its own goroutine, so a failed assertion (FailNow) or a
// skip stops only that case.
type Recorder struct {
	name string

	mu       sync.Mutex
	failures []string
	skipped  string
	logs     []string
	cleanups []func()
	restore  []func()
}

// NewRecorder returns a Recorder for the case called name.
func NewRecorder(name string) *Recorder { return &Recorder{name: name} }

// RunRecorded runs fn with r on a fresh goroutine and reports how it ended.
// Cleanups run before it returns.
func (r *Recorder) RunRecorded(fn func(t T)) Outcome {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer r.runCleanups()
		fn(r)
	}()
	<-done
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.skipped != "":
		return OutcomeSkipped
	case len(r.failures) > 0:
		return OutcomeFailed
	}
	return OutcomePassed
}

// Failures returns the recorded failure messages.
func (r *Recorder) Failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.failures...)
}

// Logs returns the lines the case logged.
func (r *Recorder) Logs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

// SkipReason returns why the case skipped, or "".
func (r *Recorder) SkipReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skipped
}

func (r *Recorder) runCleanups() {
	r.mu.Lock()
	fns := r.cleanups
	r.cleanups = nil
	restore := r.restore
	r.restore = nil
	r.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
	for i := len(restore) - 1; i >= 0; i-- {
		restore[i]()
	}
}

// Errorf records a failed assertion.
func (r *Recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// FailNow stops the case after a failed assertion.
func (r *Recorder) FailNow() {
	r.mu.Lock()
	if len(r.failures) == 0 {
		r.failures = append(r.failures, "FailNow")
	}
	r.mu.Unlock()
	runtime.Goexit()
}

// Helper is a no-op.
func (r *Recorder) Helper() {}

// Logf records a log line.
func (r *Recorder) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

// Skipf records the skip and stops the case.
func (r *Recorder) Skipf(format string, args ...any) {
	r.mu.Lock()
	r.skipped = fmt.Sprintf(format, args...)
	r.mu.Unlock()
	runtime.Goexit()
}

// Cleanup registers fn.
func (r *Recorder) Cleanup(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, fn)
}

// TempDir makes a directory removed at the end of the case.
func (r *Recorder) TempDir() string {
	dir, err := os.MkdirTemp("", "clustered-conformance-")
	if err != nil {
		r.Errorf("make temp dir: %v", err)
		r.FailNow()
	}
	r.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// Setenv sets key for the rest of the case and restores it afterwards.
func (r *Recorder) Setenv(key, value string) {
	old, had := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		r.Errorf("setenv %s: %v", key, err)
		r.FailNow()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restore = append(r.restore, func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// Name returns the case's name.
func (r *Recorder) Name() string { return r.name }

// String summarizes the recorder for a failure message.
func (r *Recorder) String() string {
	return fmt.Sprintf("%s: %s", r.name, strings.Join(r.Failures(), "; "))
}
