//go:build linux

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

package clockfloor_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/agent/clockfloor"
)

// TestSystemBootClock_Linux: the real CLOCK_BOOTTIME is positive and does not
// go backwards, and the real boot_id parses.
func TestSystemBootClock_Linux(t *testing.T) {
	a, err := clockfloor.SystemBootClock{}.BootTime()
	require.NoError(t, err)
	b, err := clockfloor.SystemBootClock{}.BootTime()
	require.NoError(t, err)
	assert.Positive(t, int64(a))
	assert.GreaterOrEqual(t, int64(b), int64(a))

	id, err := clockfloor.ProcBootID{}.BootID()
	if err != nil {
		t.Skipf("no readable boot_id here: %v", err)
	}
	assert.NotEmpty(t, id)
}
