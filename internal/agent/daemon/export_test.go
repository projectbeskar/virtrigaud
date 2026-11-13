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

package daemon

import (
	"testing"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
)

// NewFloorStore returns the clock floor's Store over st.
func NewFloorStore(st *opstore.Store) clockfloor.Store { return floorStore{st: st} }

// AllowTestOverrides lets t use the test overrides of Options in a release
// build, until t ends.
func AllowTestOverrides(t testing.TB) {
	t.Helper()
	allowTestOverrides = true
	t.Cleanup(func() { allowTestOverrides = false })
}

// SetSignerPoll shortens the pause between attempts to reach the signer at
// start-up until t ends.
func SetSignerPoll(t testing.TB, d time.Duration) {
	t.Helper()
	old := signerPollEvery
	signerPollEvery = d
	t.Cleanup(func() { signerPollEvery = old })
}
