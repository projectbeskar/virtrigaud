//go:build virtrigaud_mutation

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

package opstore_test

import (
	"os"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/agent/internal/guard"
)

// In a mutation build, VIRTRIGAUD_MUTATION must name a known guard (or be
// empty for the baseline), so that a typo cannot pass for a killed mutant.
// hack/agent-mutation.sh runs the suite once per guard.
func TestMutationGuardIsKnown(t *testing.T) {
	name := os.Getenv(guard.EnvVar)
	if name == "" {
		return
	}
	for _, g := range guard.All() {
		if string(g) == name {
			return
		}
	}
	t.Fatalf("%s=%q names no guard", guard.EnvVar, name)
}
