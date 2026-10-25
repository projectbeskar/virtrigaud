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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPerformanceConfigsLoad checks that every YAML file under test/performance
// is a configuration this tool accepts. A file in another format (for example
// a Kubernetes manifest wrapping a config the tool never read) fails the
// strict loader here instead of failing a run.
func TestPerformanceConfigsLoad(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "test", "performance", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "test/performance holds the load-test configurations")

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			cfg, loadErr := loadConfig(file)
			require.NoError(t, loadErr)
			assert.NotEmpty(t, cfg.Providers)
		})
	}
}
