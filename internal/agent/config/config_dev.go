//go:build virtrigaud_dev

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
	"path/filepath"

	"github.com/projectbeskar/virtrigaud/internal/agent/executor/fake"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor/none"
	"github.com/projectbeskar/virtrigaud/internal/agent/internal/safefile"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust"
)

// DevBuild reports whether this is a development build: true.
const DevBuild = true

// trustDir returns dir, or the signed-image location when dir is empty.
func trustDir(dir string) (string, error) {
	if dir == "" {
		return trust.DefaultDir, nil
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", fmt.Errorf("--%s must be a clean absolute path", FlagDevTrustDir)
	}
	return dir, nil
}

// newExecutor returns the named executor; the fake keeps its VMs in dir.
func newExecutor(name, dir string) (Executor, error) {
	switch name {
	case ExecutorNone:
		return none.Executor{}, nil
	case ExecutorFake:
		return fake.New(dir)
	}
	return nil, fmt.Errorf("--%s=%q is not an executor", FlagExecutor, name)
}

// maxDevEnrolmentBytes bounds the development enrolment file.
const maxDevEnrolmentBytes = 64 << 10

// loadDevEnrolment reads and parses the development enrolment file: a
// regular file (not through a symbolic link) that group and others cannot
// write, of at most 64 KiB.
func loadDevEnrolment(path string) (*DevEnrolment, error) {
	data, err := safefile.Read(path, safefile.Policy{DenyMode: 0o022, Max: maxDevEnrolmentBytes})
	if err != nil {
		return nil, fmt.Errorf("read development enrolment file: %w", err)
	}
	return ParseDevEnrolment(data)
}
