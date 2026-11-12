//go:build !virtrigaud_dev

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

package config

import (
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor/none"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
)

// DevBuild reports whether this is a development build: false, a release
// build.
const DevBuild = false

// trustDir returns the signed-image location; a release build refuses any
// other.
func trustDir(dir string) (string, error) {
	if dir != "" {
		return "", fmt.Errorf("--%s: %w", FlagDevTrustDir, ErrDevOnly)
	}
	return trust.DefaultDir, nil
}

// newExecutor returns the no-op executor: a release build has no executor
// that performs effects.
func newExecutor(name, _ string) (Executor, error) {
	if name != ExecutorNone {
		return nil, fmt.Errorf("--%s=%q: %w", FlagExecutor, name, ErrDevOnly)
	}
	return none.Executor{}, nil
}

// loadDevEnrolment refuses: a release build has no development enrolment.
func loadDevEnrolment(string) (*DevEnrolment, error) {
	return nil, fmt.Errorf("--%s: %w", FlagDevEnrolmentFile, ErrDevOnly)
}
