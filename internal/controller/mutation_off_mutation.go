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

package controller

import (
	"os"
	"testing"
)

// mutationEnvVar names the environment variable that selects the guard a
// mutation test build switches off.
const mutationEnvVar = "VIRTRIGAUD_MUTATION"

// mutationOff reports whether guard g is the one VIRTRIGAUD_MUTATION names.
// Outside a test binary it is always false, even in a mutation build.
func mutationOff(g mutationGuard) bool {
	return testing.Testing() && os.Getenv(mutationEnvVar) == string(g)
}

// testingMutationBuild reports whether this is a mutation test build.
const testingMutationBuild = true
