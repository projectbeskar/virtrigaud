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

package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestTimeRunningOut checks that a read failing because the deadline is too
// close (client-go's rate limiter refuses such a wait without wrapping
// context.DeadlineExceeded) is recognised, so it does not replace the last
// observation.
func TestTimeRunningOut(t *testing.T) {
	assert.False(t, timeRunningOut(context.Background(), time.Second), "no deadline")

	far, cancelFar := context.WithTimeout(context.Background(), time.Minute)
	defer cancelFar()
	assert.False(t, timeRunningOut(far, time.Second), "deadline well ahead")

	near, cancelNear := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelNear()
	assert.True(t, timeRunningOut(near, time.Second), "deadline closer than one interval")

	done, cancel := context.WithCancel(context.Background())
	cancel()
	assert.True(t, timeRunningOut(done, time.Second), "context done")
}
