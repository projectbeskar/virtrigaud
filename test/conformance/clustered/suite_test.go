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

package clustered_test

import (
	"testing"

	"github.com/projectbeskar/virtrigaud/test/conformance/clustered"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/fake"
)

// TestSuiteAgainstReferenceFake runs the whole suite against the fake with no
// violation: the suite must pass, in both the asynchronous and the
// synchronous configuration.
func TestSuiteAgainstReferenceFake(t *testing.T) {
	t.Run("async", func(t *testing.T) {
		clustered.Run(t, func(clustered.T) clustered.Target { return fake.New() })
	})
	t.Run("sync", func(t *testing.T) {
		clustered.Run(t, func(clustered.T) clustered.Target { return fake.New(fake.Synchronous()) })
	})
}

// TestSuiteAgainstIdentityFake runs the suite against the fake in
// operation-identity mode, with a test-only signer. A fake that also accepts
// unsigned calls must pass the whole suite, asynchronous and synchronous. A
// fake that requires a grant on every mutating call refuses the unsigned
// calls of the other groups by design, so only the identity.* cases are run
// against it.
func TestSuiteAgainstIdentityFake(t *testing.T) {
	t.Run("supports-async", func(t *testing.T) {
		clustered.Run(t, identityFake(false))
	})
	t.Run("supports-sync", func(t *testing.T) {
		clustered.Run(t, identityFake(false, fake.Synchronous()))
	})
	t.Run("requires-async", func(t *testing.T) {
		clustered.Run(t, identityFake(true), clustered.OnlyCases("identity."))
	})
}
